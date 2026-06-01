package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosk/internal/agent"
	"autosk/internal/id"
	"autosk/internal/worktree"
)

// ID prefixes for workflow/step rows.
const (
	WorkflowIDPrefix = "wf"
	StepIDPrefix     = "st"
)

// Sentinel errors.
var (
	ErrNotOpen      = errors.New("workflow store: not open")
	ErrNotFound     = errors.New("workflow not found")
	ErrAlreadyExist = errors.New("workflow already exists")
	ErrInUse        = errors.New("workflow has tasks pointing at it; refuse delete")

	// ErrNonTerminalTasks is returned by UpdateIsolation when the
	// workflow has at least one task in a non-terminal state
	// referencing it AND the caller did not pass Force=true. The
	// returned UpdateIsolationReport carries the offending task ids in
	// NonTerminalTasks so callers can surface them.
	ErrNonTerminalTasks = errors.New("workflow has non-terminal tasks; pass --force to update")

	// ErrEnsureFailed wraps a worktree.Manager.Ensure failure that
	// happened mid-rollout during a `none → worktree --force` flip.
	// Every prior Ensure for the same run is rolled back via
	// OnTerminal before this error is returned; the workflows row is
	// left unchanged.
	ErrEnsureFailed = errors.New("worktree allocation failed; isolation column not updated")

	// ErrSyntheticImmutable is returned by UpdateIsolation when the
	// targeted workflow is a synthetic single:<agent> row. Synthetic
	// workflows are pinned to isolation='none' by construction; the
	// guard fires regardless of Force.
	ErrSyntheticImmutable = errors.New("cannot update synthetic workflow")
)

// Workflow is the materialised view of one `workflows` row.
type Workflow struct {
	ID          string
	Name        string
	Description string
	FirstStepID string
	IsSynthetic bool
	// Isolation is the workflow-level execution-isolation mode. See
	// docs/plans/20260521-Worktree-Isolation.md. Empty in the on-disk
	// row collapses to IsolationNone on scan so callers can compare
	// against IsolationNone / IsolationWorktree directly.
	Isolation IsolationMode
	CreatedAt time.Time
	Steps     []Step // populated by GetByName/Show, not by List
}

// Step mirrors one `steps` row plus its outgoing transitions.
type Step struct {
	ID          string
	WorkflowID  string
	Name        string
	AgentID     string
	AgentName   string       // joined-in for convenience
	AgentParams *AgentParams // nil = use the package's defaults verbatim
	MaxVisits   int          // 0 = unlimited; see docs/workflows.md
	Transitions []Transition // outgoing, in source order
}

// Transition mirrors one `step_transitions` row.
type Transition struct {
	ID         int64
	StepID     string
	NextStepID string // empty when TaskStatus is set
	TaskStatus string // empty when NextStepID is set
	PromptRule string
	// NextStepName populated as a convenience when reading via the store.
	NextStepName string
}

// IsTaskStatus reports whether this transition terminates / parks the
// workflow rather than advancing to a sibling step.
func (t Transition) IsTaskStatus() bool { return t.TaskStatus != "" }

// Store backs workflows / steps / step_transitions on the shared *sql.DB.
type Store struct {
	db    *sql.DB
	agent *agent.Store // resolves agent names → ids during Create
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// New constructs a Store. agent_store must be non-nil (we need it for
// agent name → id resolution).
func New(db *sql.DB, ag *agent.Store) *Store {
	return &Store{db: db, agent: ag}
}

// Agents exposes the underlying agent.Store. Useful for CLI helpers that
// want to resolve agent ids without re-constructing a store off the same
// *sql.DB.
func (s *Store) Agents() *agent.Store { return s.agent }

// Create persists a parsed Definition transactionally. Returns the
// materialised Workflow with all nested steps + transitions.
//
// On UNIQUE(name) collision returns ErrAlreadyExist.
func (s *Store) Create(ctx context.Context, def Definition, isSynthetic bool) (Workflow, error) {
	created, err := s.create(ctx, def, isSynthetic, nil)
	if err != nil {
		return Workflow{}, err
	}
	return s.GetByName(ctx, created.workflowName)
}

// CreateWithOrigin persists a parsed Definition and its provenance in one
// database transaction. If writing either the workflow graph or the origin row
// fails, neither is left behind.
func (s *Store) CreateWithOrigin(ctx context.Context, def Definition, isSynthetic bool, origin Origin) (Workflow, error) {
	created, err := s.create(ctx, def, isSynthetic, &origin)
	if err != nil {
		return Workflow{}, err
	}
	return s.GetByName(ctx, created.workflowName)
}

// CheckReplaceAllowed verifies that replacing name with def would not violate
// non-mutating store preconditions such as name matching and in-use protection.
// It intentionally does not validate agent availability or write anything.
func (s *Store) CheckReplaceAllowed(ctx context.Context, name string, def Definition) error {
	if s.db == nil {
		return ErrNotOpen
	}
	if def.Name != name {
		return fmt.Errorf("replacement workflow name %q does not match existing workflow %q", def.Name, name)
	}
	old, err := s.getWorkflowRowByName(ctx, name)
	if err != nil {
		return err
	}
	refs, err := s.countTaskRefs(ctx, old.ID)
	if err != nil {
		return err
	}
	if refs > 0 {
		return fmt.Errorf("%w: %d task(s) reference %q", ErrInUse, refs, name)
	}
	return nil
}

// ReplaceWithOrigin atomically replaces an existing workflow with def and a
// matching origin row. Validation and agent resolution happen before the old
// workflow is deleted; any later failure rolls back the delete+insert together.
func (s *Store) ReplaceWithOrigin(ctx context.Context, name string, def Definition, origin Origin) (Workflow, error) {
	if def.Name != name {
		return Workflow{}, fmt.Errorf("replacement workflow name %q does not match existing workflow %q", def.Name, name)
	}
	prep, err := s.prepareCreate(ctx, def, false)
	if err != nil {
		return Workflow{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workflow{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx,
		`SELECT id, name, description, first_step_id, is_synthetic, isolation, created_at
		   FROM workflows WHERE name = ?`, name)
	old, err := scanWorkflowRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ErrNotFound
	}
	if err != nil {
		return Workflow{}, err
	}
	refs, err := countTaskRefs(ctx, tx, old.ID)
	if err != nil {
		return Workflow{}, err
	}
	if refs > 0 {
		return Workflow{}, fmt.Errorf("%w: %d task(s) reference %q", ErrInUse, refs, name)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workflows WHERE id = ?`, old.ID); err != nil {
		return Workflow{}, fmt.Errorf("delete workflow: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `REINDEX workflows`); err != nil {
		return Workflow{}, fmt.Errorf("reindex workflows after delete: %w", err)
	}
	if err := s.insertDefinitionTx(ctx, tx, def, false, prep); err != nil {
		return Workflow{}, err
	}
	origin.WorkflowID = prep.workflowID
	if err := s.upsertOriginTx(ctx, tx, origin); err != nil {
		return Workflow{}, err
	}
	if err := tx.Commit(); err != nil {
		return Workflow{}, fmt.Errorf("commit: %w", err)
	}
	return s.GetByName(ctx, def.Name)
}

type preparedWorkflowCreate struct {
	workflowID   string
	workflowName string
	stepIDs      map[string]string
	stepAgents   map[string]string
}

func (s *Store) getWorkflowRowByName(ctx context.Context, name string) (Workflow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, description, first_step_id, is_synthetic, isolation, created_at
		   FROM workflows WHERE name = ?`, name)
	w, err := scanWorkflowRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ErrNotFound
	}
	return w, err
}

func (s *Store) countTaskRefs(ctx context.Context, workflowID string) (int, error) {
	return countTaskRefs(ctx, s.db, workflowID)
}

func countTaskRefs(ctx context.Context, q queryRower, workflowID string) (int, error) {
	var refs int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE workflow_id = ?`, workflowID).Scan(&refs); err != nil {
		return 0, fmt.Errorf("count tasks: %w", err)
	}
	return refs, nil
}

func (s *Store) create(ctx context.Context, def Definition, isSynthetic bool, origin *Origin) (preparedWorkflowCreate, error) {
	prep, err := s.prepareCreate(ctx, def, isSynthetic)
	if err != nil {
		return preparedWorkflowCreate{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return preparedWorkflowCreate{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.insertDefinitionTx(ctx, tx, def, isSynthetic, prep); err != nil {
		return preparedWorkflowCreate{}, err
	}
	if origin != nil {
		o := *origin
		o.WorkflowID = prep.workflowID
		if err := s.upsertOriginTx(ctx, tx, o); err != nil {
			return preparedWorkflowCreate{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return preparedWorkflowCreate{}, fmt.Errorf("commit: %w", err)
	}
	return prep, nil
}

func (s *Store) prepareCreate(ctx context.Context, def Definition, isSynthetic bool) (preparedWorkflowCreate, error) {
	if s.db == nil {
		return preparedWorkflowCreate{}, ErrNotOpen
	}
	if err := Validate(ctx, def, s.agent, ValidateOpts{AllowSyntheticName: isSynthetic}); err != nil {
		return preparedWorkflowCreate{}, err
	}
	// Resolve agent names to ids up-front so we fail fast.
	stepAgents := make(map[string]string, len(def.Steps))
	for stepName, sd := range def.Steps {
		a, err := s.agent.GetByName(ctx, sd.AgentName)
		if err != nil {
			return preparedWorkflowCreate{}, fmt.Errorf("resolve agent %q for step %q: %w", sd.AgentName, stepName, err)
		}
		stepAgents[stepName] = a.ID
	}

	// Mint ids BEFORE BeginTx: doltlite is single-writer (one conn), so
	// queries inside a tx must go through *Tx, not *DB. id.NewUnique uses
	// the *DB-level helpers, so we run it first.
	wfID, err := id.NewUnique(WorkflowIDPrefix, func(candidate string) (bool, error) {
		return s.workflowIDExists(ctx, candidate)
	})
	if err != nil {
		return preparedWorkflowCreate{}, fmt.Errorf("generate workflow id: %w", err)
	}
	stepIDs := make(map[string]string, len(def.Steps))
	for _, name := range orderedStepNames(def) {
		stepID, err := id.NewUnique(StepIDPrefix, func(candidate string) (bool, error) {
			return s.stepIDExists(ctx, candidate)
		})
		if err != nil {
			return preparedWorkflowCreate{}, fmt.Errorf("generate step id: %w", err)
		}
		stepIDs[name] = stepID
	}
	return preparedWorkflowCreate{
		workflowID:   wfID,
		workflowName: def.Name,
		stepIDs:      stepIDs,
		stepAgents:   stepAgents,
	}, nil
}

func (s *Store) insertDefinitionTx(ctx context.Context, tx *sql.Tx, def Definition, isSynthetic bool, prep preparedWorkflowCreate) error {
	now := time.Now().Unix()

	firstStepID, ok := prep.stepIDs[def.FirstStep]
	if !ok {
		return fmt.Errorf("internal: first_step %q has no id", def.FirstStep)
	}

	synthetic := 0
	if isSynthetic {
		synthetic = 1
	}
	isolation := def.Isolation.Normalize()
	if isSynthetic && isolation != IsolationNone {
		// Defensive: synthetic workflows must always pin isolation='none'.
		// Validate() also catches user-driven attempts; this guard makes
		// sure EnsureSingle's invariant stays true even if a programmer
		// tries to flip the field on a synthetic def.
		return fmt.Errorf("synthetic workflow %q cannot use isolation=%q", def.Name, isolation)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, isolation, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, prep.workflowID, def.Name, def.Description, firstStepID, synthetic, string(isolation), now); err != nil {
		if isUniqueErr(err, "workflows.name") {
			return fmt.Errorf("%w: %s", ErrAlreadyExist, def.Name)
		}
		return fmt.Errorf("insert workflow: %w", err)
	}

	// Two passes: insert ALL steps first (so forward-referencing
	// transitions like dev→review don't hit FK), then insert transitions.
	names := orderedStepNames(def)
	for seq, stepName := range names {
		sd := def.Steps[stepName]
		paramsJSON, perr := marshalAgentParams(sd.AgentParams)
		if perr != nil {
			return fmt.Errorf("marshal agent_params for step %q: %w", stepName, perr)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO steps(id, workflow_id, name, agent_id, seq, agent_params, max_visits) VALUES (?, ?, ?, ?, ?, ?, ?)
		`, prep.stepIDs[stepName], prep.workflowID, stepName, prep.stepAgents[stepName], seq, paramsJSON, sd.MaxVisits); err != nil {
			return fmt.Errorf("insert step %q: %w", stepName, err)
		}
	}
	for _, stepName := range names {
		sd := def.Steps[stepName]
		for i, tr := range sd.NextSteps {
			var nextID, status any
			if tr.IsTaskStatus() {
				nextID = nil
				status = tr.TaskStatus
			} else {
				nid, ok := prep.stepIDs[tr.Step]
				if !ok {
					return fmt.Errorf("internal: transition %d in step %q targets unknown step %q", i, stepName, tr.Step)
				}
				nextID = nid
				status = nil
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO step_transitions(step_id, next_step_id, task_status, prompt_rule)
				VALUES (?, ?, ?, ?)
			`, prep.stepIDs[stepName], nextID, status, tr.PromptRule); err != nil {
				return fmt.Errorf("insert transition %d for step %q: %w", i, stepName, err)
			}
		}
	}
	return nil
}

// GetByName returns a fully-loaded workflow (with steps + transitions),
// or ErrNotFound.
func (s *Store) GetByName(ctx context.Context, name string) (Workflow, error) {
	if s.db == nil {
		return Workflow{}, ErrNotOpen
	}
	w, err := s.getWorkflowRowByName(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ErrNotFound
	}
	if err != nil {
		return Workflow{}, err
	}
	steps, err := s.loadSteps(ctx, w.ID)
	if err != nil {
		return Workflow{}, fmt.Errorf("load steps: %w", err)
	}
	w.Steps = steps
	return w, nil
}

// List returns workflows ordered by name. is_synthetic=1 rows are hidden
// unless includeSynthetic is true.
func (s *Store) List(ctx context.Context, includeSynthetic bool) ([]Workflow, error) {
	if s.db == nil {
		return nil, ErrNotOpen
	}
	q := `SELECT id, name, description, first_step_id, is_synthetic, isolation, created_at
	        FROM workflows`
	if !includeSynthetic {
		q += ` WHERE is_synthetic = 0`
	}
	q += ` ORDER BY name ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer rows.Close()
	var out []Workflow
	for rows.Next() {
		w, err := scanWorkflowRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Delete removes a workflow by name. Refuses with ErrInUse when at least
// one task currently references it.
func (s *Store) Delete(ctx context.Context, name string) error {
	if s.db == nil {
		return ErrNotOpen
	}
	w, err := s.GetByName(ctx, name)
	if err != nil {
		return err
	}
	refs, err := s.countTaskRefs(ctx, w.ID)
	if err != nil {
		return err
	}
	if refs > 0 {
		return fmt.Errorf("%w: %d task(s) reference %q", ErrInUse, refs, name)
	}
	// CASCADE handles steps + step_transitions; rows in daemon_runs use
	// ON DELETE RESTRICT on step_id, so if any run row exists deletion
	// fails noisily. That's the safe default for v0.2.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM workflows WHERE id = ?`, w.ID); err != nil {
		return fmt.Errorf("delete workflow: %w", err)
	}
	// doltlite v0.10.8 workaround: a DELETE against a row with a UNIQUE
	// constraint (here `workflows.name`) does not always evict the
	// matching entry from the implicit unique index. A subsequent
	// INSERT with the same name then trips a phantom UNIQUE violation
	// even though SELECT-by-name reports the row as gone. REINDEX on
	// the affected table rebuilds the auto-index from the live row
	// set and clears the phantom. The workaround is cheap (small
	// table) and idempotent.
	//
	// Tracked alongside the doltlite version pin in the Makefile;
	// remove once upstream ships a fix and we bump DOLTLITE_VERSION.
	if _, err := s.db.ExecContext(ctx, `REINDEX workflows`); err != nil {
		return fmt.Errorf("reindex workflows after delete: %w", err)
	}
	return nil
}

// UpdateIsolationOpts controls UpdateIsolation. ProjectRoot and
// Worktrees are only consulted when the update actually mutates the
// column AND the transition is `none → worktree` with Force=true
// (the only mode that performs per-task Ensure calls). DryRun
// short-circuits both the writes and the side-effects.
type UpdateIsolationOpts struct {
	Force       bool
	DryRun      bool
	ProjectRoot string           // required when Force && transitioning none→worktree
	Worktrees   worktree.Manager // required when Force && transitioning none→worktree
}

// EnsureRecord describes one worktree allocation produced by a
// `none → worktree --force` flip. Existing reports whether the
// directory was already on disk (no-op Ensure) at the time of the
// run.
type EnsureRecord struct {
	TaskID   string `json:"task_id"`
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Existing bool   `json:"existing"`
}

// LeftoverWorktree describes one (taskID, path) pair surfaced by a
// `worktree → none --force` flip. The store does NOT remove the
// directory — doing so would discard uncommitted state. Callers
// render the list so the human can `autosk worktree rm` once
// they've salvaged anything they need.
type LeftoverWorktree struct {
	TaskID string `json:"task_id"`
	Path   string `json:"path"`
}

// UpdateIsolationReport carries the structured outcome of
// UpdateIsolation. Populated up to the point of failure, so callers
// can surface partial rollbacks even on the error path.
type UpdateIsolationReport struct {
	Workflow          string             `json:"workflow"`
	From              IsolationMode      `json:"from"`
	To                IsolationMode      `json:"to"`
	Noop              bool               `json:"noop"`
	DryRun            bool               `json:"dry_run"`
	NonTerminalTasks  []string           `json:"non_terminal_tasks,omitempty"`
	EnsuredTasks      []EnsureRecord     `json:"ensured_tasks,omitempty"`
	LeftoverWorktrees []LeftoverWorktree `json:"leftover_worktrees,omitempty"`
	// RolledBackEnsures lists the EnsureRecord entries whose worktrees
	// were rolled back via OnTerminal because a later Ensure failed in
	// the same run. Populated only on ErrEnsureFailed paths. Useful
	// for the CLI's error rendering ("rolled back: X, Y, Z").
	RolledBackEnsures []EnsureRecord `json:"rolled_back_ensures,omitempty"`
	// FailedTask, when non-empty, names the task whose Ensure call
	// failed during a `none → worktree --force` rollout. Populated
	// only on ErrEnsureFailed paths.
	FailedTask string `json:"failed_task,omitempty"`
}

// UpdateIsolation flips the workflows.isolation column on the named
// workflow. The method is the single chokepoint for `workflow update
// --isolation` (CLI) and for lazy's `i` keybinding — both reduce to
// this call.
//
// Behavioural matrix (mirrors plan §4.2):
//
//	current     target      no-non-terminal-tasks   non-terminal (!force)   non-terminal (force)
//	none        none        no-op                   no-op                   no-op
//	worktree    worktree    no-op                   no-op                   no-op
//	none        worktree    flip column             refuse                  Ensure each; flip
//	worktree    none        flip column             refuse                  flip + leftover list
//
// The doltlite commit (the
// dl.DoltCommit message) is the caller's responsibility; this method
// only mutates the DB rows.
//
// Returns a populated UpdateIsolationReport even on error so callers
// can render partial-rollback / refusal output.
func (s *Store) UpdateIsolation(
	ctx context.Context,
	name string,
	target IsolationMode,
	opts UpdateIsolationOpts,
) (UpdateIsolationReport, error) {
	if s.db == nil {
		return UpdateIsolationReport{Workflow: name}, ErrNotOpen
	}
	target = target.Normalize()
	if !target.Valid() {
		return UpdateIsolationReport{Workflow: name}, fmt.Errorf("invalid isolation mode %q (want none|worktree)", target)
	}
	w, err := s.GetByName(ctx, name)
	if err != nil {
		return UpdateIsolationReport{Workflow: name}, err
	}
	report := UpdateIsolationReport{
		Workflow: w.Name,
		From:     w.Isolation.Normalize(),
		To:       target,
		DryRun:   opts.DryRun,
	}
	if w.IsSynthetic {
		return report, fmt.Errorf("%w: %s", ErrSyntheticImmutable, name)
	}
	if report.From == target {
		report.Noop = true
		return report, nil
	}

	nonTerminal, err := s.listNonTerminalTaskIDs(ctx, w.ID)
	if err != nil {
		return report, fmt.Errorf("list non-terminal tasks: %w", err)
	}
	report.NonTerminalTasks = nonTerminal
	if len(nonTerminal) > 0 && !opts.Force {
		return report, fmt.Errorf("%w (%d task(s))", ErrNonTerminalTasks, len(nonTerminal))
	}

	// Per-task side effects only matter when there is at least one
	// non-terminal task AND the transition actually moves between
	// modes (the noop guard above already short-circuited
	// no-op flips). Hoist precondition checks here so the dry-run
	// path (which renders the plan to the operator) errors
	// loudly instead of shipping an empty Path that the operator
	// cannot act on. Mirrors the predecessor plan's contract that
	// a worktree.PathFor failure is an environment problem (HOME
	// unset, project root not resolvable) and should never be
	// silently masked.
	if len(nonTerminal) > 0 && strings.TrimSpace(opts.ProjectRoot) == "" {
		return report, fmt.Errorf("isolation flip requires a non-empty ProjectRoot when %d non-terminal task(s) reference the workflow", len(nonTerminal))
	}

	// Plan per-task side effects.
	var plannedEnsures []EnsureRecord
	var plannedLeftovers []LeftoverWorktree
	switch {
	case report.From == IsolationNone && target == IsolationWorktree && len(nonTerminal) > 0:
		// Force is guaranteed by the guard above.
		for _, tid := range nonTerminal {
			path, perr := worktree.PathFor(opts.ProjectRoot, tid)
			if perr != nil {
				return report, fmt.Errorf("plan worktree path for task %s: %w", tid, perr)
			}
			plannedEnsures = append(plannedEnsures, EnsureRecord{
				TaskID: tid,
				Path:   path,
				Branch: worktree.BranchFor(tid),
			})
		}
	case report.From == IsolationWorktree && target == IsolationNone && len(nonTerminal) > 0:
		for _, tid := range nonTerminal {
			path, perr := worktree.PathFor(opts.ProjectRoot, tid)
			if perr != nil {
				return report, fmt.Errorf("plan worktree path for task %s: %w", tid, perr)
			}
			plannedLeftovers = append(plannedLeftovers, LeftoverWorktree{
				TaskID: tid,
				Path:   path,
			})
		}
	}
	report.EnsuredTasks = plannedEnsures
	report.LeftoverWorktrees = plannedLeftovers

	if opts.DryRun {
		return report, nil
	}

	// Execute per-task side effects BEFORE the column flip so a mid-run
	// Ensure failure leaves the column unchanged. (The doltlite store is
	// single-writer; we don't open a tx that spans the Ensure shell-outs
	// because git operations on a per-task path are independent of the
	// DB row and a transaction held across N shell-outs would block
	// every other writer for the duration of the flip.)
	if report.From == IsolationNone && target == IsolationWorktree && len(plannedEnsures) > 0 {
		if opts.Worktrees == nil {
			return report, fmt.Errorf("%w: nil worktree.Manager", ErrEnsureFailed)
		}
		// opts.ProjectRoot is guaranteed non-empty by the precondition
		// hoist above; this defensive guard preserves the explicit
		// ErrEnsureFailed surface if the precondition is ever moved.
		if strings.TrimSpace(opts.ProjectRoot) == "" {
			return report, fmt.Errorf("%w: empty project root", ErrEnsureFailed)
		}
		var applied []EnsureRecord
		for _, rec := range plannedEnsures {
			res, eerr := opts.Worktrees.Ensure(ctx, opts.ProjectRoot, rec.TaskID, "")
			if eerr != nil {
				// Roll back every prior Ensure: best-effort OnTerminal.
				for _, prev := range applied {
					_, _ = opts.Worktrees.OnTerminal(ctx, opts.ProjectRoot, prev.TaskID)
				}
				report.FailedTask = rec.TaskID
				report.RolledBackEnsures = applied
				report.EnsuredTasks = nil
				return report, fmt.Errorf("%w: task %s: %v", ErrEnsureFailed, rec.TaskID, eerr)
			}
			applied = append(applied, EnsureRecord{
				TaskID:   rec.TaskID,
				Path:     res.Path,
				Branch:   res.Branch,
				Existing: res.Existing,
			})
		}
		report.EnsuredTasks = applied
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE workflows SET isolation = ? WHERE id = ?`,
		string(target), w.ID); err != nil {
		// Roll back per-task Ensures we made for this run, otherwise we'd
		// leak worktrees with no DB row claiming them.
		if report.From == IsolationNone && target == IsolationWorktree {
			for _, prev := range report.EnsuredTasks {
				_, _ = opts.Worktrees.OnTerminal(ctx, opts.ProjectRoot, prev.TaskID)
			}
			report.RolledBackEnsures = report.EnsuredTasks
			report.EnsuredTasks = nil
		}
		return report, fmt.Errorf("update workflows.isolation: %w", err)
	}
	return report, nil
}

// listNonTerminalTaskIDs returns the ids of tasks that reference the
// workflow in a non-terminal state, ordered by id ASC for
// deterministic rendering.
//
// The match set is exactly the SQL: every row where
// workflow_id = <wf-id> AND status IN ('new','work','human').
// workflowID is the row's PK and is never NULL or empty (callers
// pass the value pulled from the workflows row), so the WHERE
// clause implicitly excludes unenrolled rows.
//
// `done` / `cancel` rows are intentionally excluded — they cannot
// touch the worktree at the next step run because there is no
// next step run.
func (s *Store) listNonTerminalTaskIDs(ctx context.Context, workflowID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM tasks
		 WHERE workflow_id = ?
		   AND status IN ('new','work','human')
		 ORDER BY id ASC`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FindStepByID returns the step row for the given step id, including its
// outgoing transitions. Used by the CLI to surface derived current_step
// / current_agent on a Task.
func (s *Store) FindStepByID(ctx context.Context, stepID string) (Step, error) {
	if s.db == nil {
		return Step{}, ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.workflow_id, s.name, s.agent_id, a.name, s.agent_params, s.max_visits
		  FROM steps s JOIN agents a ON s.agent_id = a.id
		 WHERE s.id = ?`, stepID)
	st, err := scanStep(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Step{}, ErrNotFound
	}
	if err != nil {
		return Step{}, err
	}
	trs, err := s.loadTransitions(ctx, st.ID)
	if err != nil {
		return Step{}, err
	}
	st.Transitions = trs
	return st, nil
}

// GetByID returns a fully-loaded workflow by id, mirror of GetByName.
func (s *Store) GetByID(ctx context.Context, workflowID string) (Workflow, error) {
	if s.db == nil {
		return Workflow{}, ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, description, first_step_id, is_synthetic, isolation, created_at
		   FROM workflows WHERE id = ?`, workflowID)
	w, err := scanWorkflowRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ErrNotFound
	}
	if err != nil {
		return Workflow{}, err
	}
	steps, err := s.loadSteps(ctx, w.ID)
	if err != nil {
		return Workflow{}, err
	}
	w.Steps = steps
	return w, nil
}

// FindStepByName returns the (single) step row matching (workflow, step name).
// Used by EnsureSingle and the CLI to translate step names into ids.
func (s *Store) FindStepByName(ctx context.Context, workflowID, stepName string) (Step, error) {
	if s.db == nil {
		return Step{}, ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.workflow_id, s.name, s.agent_id, a.name, s.agent_params, s.max_visits
		  FROM steps s JOIN agents a ON s.agent_id = a.id
		 WHERE s.workflow_id = ? AND s.name = ?`,
		workflowID, stepName)
	st, err := scanStep(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Step{}, ErrNotFound
	}
	if err != nil {
		return Step{}, err
	}
	trs, err := s.loadTransitions(ctx, st.ID)
	if err != nil {
		return Step{}, err
	}
	st.Transitions = trs
	return st, nil
}

// ---- helpers --------------------------------------------------------------

func (s *Store) workflowIDExists(ctx context.Context, wfID string) (bool, error) {
	var x int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM workflows WHERE id = ?`, wfID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) stepIDExists(ctx context.Context, stID string) (bool, error) {
	var x int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM steps WHERE id = ?`, stID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) loadSteps(ctx context.Context, workflowID string) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.workflow_id, s.name, s.agent_id, a.name, s.agent_params, s.max_visits
		  FROM steps s JOIN agents a ON s.agent_id = a.id
		 WHERE s.workflow_id = ?
		 ORDER BY s.seq ASC`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		trs, err := s.loadTransitions(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Transitions = trs
	}
	return out, nil
}

func (s *Store) loadTransitions(ctx context.Context, stepID string) ([]Transition, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.step_id, t.next_step_id, t.task_status, t.prompt_rule,
		       (SELECT name FROM steps WHERE id = t.next_step_id) AS next_name
		  FROM step_transitions t
		 WHERE t.step_id = ?
		 ORDER BY t.id ASC`, stepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transition
	for rows.Next() {
		var (
			tr       Transition
			nextID   sql.NullString
			status   sql.NullString
			nextName sql.NullString
		)
		if err := rows.Scan(&tr.ID, &tr.StepID, &nextID, &status, &tr.PromptRule, &nextName); err != nil {
			return nil, err
		}
		tr.NextStepID = nextID.String
		tr.TaskStatus = status.String
		tr.NextStepName = nextName.String
		out = append(out, tr)
	}
	return out, rows.Err()
}

// scanStep scans one steps-table row (joined against agents.name) into
// a Step value, including any non-null agent_params JSON blob and the
// max_visits cap.
func scanStep(sc interface{ Scan(...any) error }) (Step, error) {
	var (
		st        Step
		paramsRaw sql.NullString
	)
	if err := sc.Scan(&st.ID, &st.WorkflowID, &st.Name, &st.AgentID, &st.AgentName, &paramsRaw, &st.MaxVisits); err != nil {
		return Step{}, err
	}
	if paramsRaw.Valid && strings.TrimSpace(paramsRaw.String) != "" {
		var p AgentParams
		if err := json.Unmarshal([]byte(paramsRaw.String), &p); err != nil {
			return Step{}, fmt.Errorf("unmarshal agent_params for step %s: %w", st.ID, err)
		}
		if !p.IsZero() {
			st.AgentParams = &p
		}
	}
	return st, nil
}

// marshalAgentParams returns the SQL-friendly NullString representation
// of an AgentParams pointer. nil/zero collapses to SQL NULL.
func marshalAgentParams(p *AgentParams) (any, error) {
	if p.IsZero() {
		return nil, nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func scanWorkflowRow(sc interface{ Scan(...any) error }) (Workflow, error) {
	var (
		w         Workflow
		synth     int
		isolation sql.NullString
		created   int64
	)
	if err := sc.Scan(&w.ID, &w.Name, &w.Description, &w.FirstStepID, &synth, &isolation, &created); err != nil {
		return Workflow{}, err
	}
	w.IsSynthetic = synth != 0
	if isolation.Valid {
		w.Isolation = IsolationMode(strings.TrimSpace(isolation.String)).Normalize()
	} else {
		w.Isolation = IsolationNone
	}
	w.CreatedAt = time.Unix(created, 0).UTC()
	return w, nil
}

func isUniqueErr(err error, indexKey string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed: "+indexKey)
}

// orderedStepNames returns the steps' source-file order (if known) or a
// stable alphabetical order otherwise. Centralised here so Create and
// future renderers agree on ordering.
func orderedStepNames(def Definition) []string {
	if len(def.StepNames) > 0 {
		return def.StepNames
	}
	out := make([]string, 0, len(def.Steps))
	for k := range def.Steps {
		out = append(out, k)
	}
	// Stable sort for determinism (alpha asc).
	sortStringsInPlace(out)
	return out
}

// sortStringsInPlace is a tiny inline sort to avoid adding sort import
// solely for the fallback path.
func sortStringsInPlace(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
