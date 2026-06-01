package datasource

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/comments"
	"autosk/internal/daemon/api"
	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/transcript"
	"autosk/internal/globalworkflow"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/tasksvc"
	"autosk/internal/workflow"
	"autosk/internal/worktree"
)

// Offline is a Datasource backed entirely by the project's .autosk/db
// + the on-disk session.jsonl transcripts. No daemon traffic.
//
// Writes go through the same store APIs the CLI uses (so commits land
// in doltlite, comments insert via comments.Store, etc.) which keeps
// the lazy TUI semantically identical to the CLI when the daemon is
// down. Verbs that fundamentally need a daemon (CancelJob, SendInput,
// AbortJob, StreamLive) return ErrDaemonRequired.
type Offline struct {
	s           *doltlite.Store
	cwd         string
	projectRoot string // resolved <root>/, i.e. parent of .autosk/; used by tasksvc for worktree cleanup
	registry    *pkgregistry.Registry
}

// NewOffline wires an Offline datasource on top of an already-open
// store. The caller retains ownership of s; closing the store while
// the datasource is in use is undefined behaviour.
//
// registry is optional; when nil agent metadata is filled in best
// effort from the DB row alone.
//
// The project root (parent of .autosk/) is derived from the store's
// resolved db path so tasksvc-driven worktree cleanup on done/cancel
// uses the same root the CLI does, regardless of cwd. Falls back to
// empty (= skip worktree cleanup) when the store path isn't a
// well-formed .autosk/db location (e.g. :memory: in tests).
func NewOffline(s store.Store, cwd string, registry *pkgregistry.Registry) (*Offline, error) {
	dl, ok := s.(*doltlite.Store)
	if !ok {
		return nil, fmt.Errorf("offline datasource: store is not doltlite (%T)", s)
	}
	return &Offline{s: dl, cwd: cwd, projectRoot: projectRootFromDBPath(dl.Path()), registry: registry}, nil
}

// projectRootFromDBPath maps `<root>/.autosk/db` → `<root>`. Returns
// "" for paths that don't match the expected layout (":memory:" in
// tests, bare files outside an .autosk/ dir) — callers must treat ""
// as "skip worktree cleanup".
func projectRootFromDBPath(dbPath string) string {
	if dbPath == "" || dbPath == ":memory:" {
		return ""
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return ""
	}
	parent := filepath.Dir(abs)
	if filepath.Base(parent) != ".autosk" {
		return ""
	}
	return filepath.Dir(parent)
}

// DB returns the underlying *sql.DB. Exposed for tests / palette ops
// that need a raw query; ordinary callers should not reach in.
func (o *Offline) DB() *sql.DB { return o.s.DB() }

// Tasks lists matching tasks with all derived fields resolved.
func (o *Offline) Tasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	statuses := f.Statuses
	if statuses == nil {
		statuses = store.OpenStatuses()
	}
	raw, err := o.s.ListTasks(ctx, store.ListFilter{Statuses: statuses, Priority: f.Priority, Limit: 0})
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	out := make([]Task, 0, len(raw))
	for _, r := range raw {
		t, err := o.projectTask(ctx, r)
		if err != nil {
			return nil, err
		}
		if f.WorkflowID != "" && t.WorkflowID != f.WorkflowID {
			continue
		}
		if f.AgentName != "" {
			// Broad match: author OR current step agent.
			if !strings.EqualFold(t.AuthorName, f.AgentName) && !strings.EqualFold(t.AgentName, f.AgentName) {
				continue
			}
		}
		if f.AuthorName != "" {
			if !strings.EqualFold(t.AuthorName, f.AuthorName) {
				continue
			}
		}
		if f.StepAgentName != "" {
			if !strings.EqualFold(t.AgentName, f.StepAgentName) {
				continue
			}
		}
		if f.Search != "" {
			needle := strings.ToLower(f.Search)
			if !strings.Contains(strings.ToLower(t.ID), needle) &&
				!strings.Contains(strings.ToLower(t.Title), needle) {
				continue
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// GetTask returns one task with all derived fields resolved.
func (o *Offline) GetTask(ctx context.Context, id string) (Task, error) {
	raw, err := o.s.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	return o.projectTask(ctx, raw)
}

func (o *Offline) projectTask(ctx context.Context, raw store.Task) (Task, error) {
	t := Task{
		ID:            raw.ID,
		Title:         raw.Title,
		Description:   raw.Description,
		Status:        raw.Status,
		Priority:      raw.Priority,
		AuthorID:      raw.AuthorID,
		WorkflowID:    raw.WorkflowID,
		CurrentStepID: raw.CurrentStepID,
		Metadata:      raw.Metadata,
		CreatedAt:     raw.CreatedAt,
		UpdatedAt:     raw.UpdatedAt,
	}
	if raw.AuthorID != "" {
		ag := agent.New(o.s.DB())
		a, err := ag.GetByID(ctx, raw.AuthorID)
		if err == nil {
			t.AuthorName = a.Name
		}
	}
	if raw.WorkflowID != "" {
		wf, err := workflow.New(o.s.DB(), agent.New(o.s.DB())).GetByID(ctx, raw.WorkflowID)
		if err == nil {
			t.WorkflowName = wf.Name
		}
	}
	if raw.CurrentStepID != "" {
		st, err := workflow.New(o.s.DB(), agent.New(o.s.DB())).FindStepByID(ctx, raw.CurrentStepID)
		if err == nil {
			t.StepName = st.Name
			t.AgentName = st.AgentName
		}
	}
	if blocked, err := o.s.IsBlocked(ctx, raw.ID); err == nil {
		t.Blocked = blocked
	}
	if in, out, err := o.s.Deps(ctx, raw.ID); err == nil {
		t.BlockedBy = o.resolveTaskRefs(ctx, in)
		t.Blocks = o.resolveTaskRefs(ctx, out)
	}
	cs := comments.New(o.s.DB())
	if list, err := cs.ListByTask(ctx, raw.ID); err == nil {
		t.CommentCount = len(list)
	}
	return t, nil
}

// resolveTaskRefs enriches a list of task ids with each task's current
// status so the detail pane can paint closed blockers in gray without
// re-querying the store at render time. Missing ids (a stale Deps row
// pointing at a deleted task, say) carry an empty Status and the
// renderer treats them like an active row — we'd rather flag a stale
// blocker than hide it.
//
// O(N) sql calls because Deps lists are typically tiny (single-digit
// blockers per task). A bulk "WHERE id IN (...)" lookup would be a
// pure win if blocker counts ever scale up; out of scope for now.
func (o *Offline) resolveTaskRefs(ctx context.Context, ids []string) []TaskRef {
	if len(ids) == 0 {
		return nil
	}
	refs := make([]TaskRef, 0, len(ids))
	for _, id := range ids {
		ref := TaskRef{ID: id}
		if raw, err := o.s.GetTask(ctx, id); err == nil {
			ref.Status = raw.Status
		}
		refs = append(refs, ref)
	}
	return refs
}

// Jobs reads daemon_runs and decorates each row with workflow / step
// / agent names for display.
func (o *Offline) Jobs(ctx context.Context, f JobFilter) ([]Job, error) {
	statuses := make([]runstore.RunStatus, 0, len(f.Statuses))
	for _, s := range f.Statuses {
		statuses = append(statuses, runstore.RunStatus(s))
	}
	rs := runstore.New(o.s.DB())
	raw, err := rs.ListRuns(ctx, runstore.RunFilter{Statuses: statuses, TaskID: f.TaskID, Limit: f.Limit})
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	// Batch-decorate: one SQL pulls workflow:step:agent labels for
	// every step_id we just saw. Avoids the N+1 per-run lookup that
	// the previous implementation incurred (and that hit the project
	// DB ~3x per Job on every 2s refresh tick).
	stepIDs := make([]string, 0, len(raw))
	for _, r := range raw {
		if r.StepID != "" {
			stepIDs = append(stepIDs, r.StepID)
		}
	}
	decor := o.lookupStepLabels(ctx, stepIDs)
	out := make([]Job, 0, len(raw))
	for _, r := range raw {
		j := Job{JobResponse: api.FromRun(r)}
		if r.StepID != "" {
			if d, ok := decor[r.StepID]; ok {
				j.StepName = d.StepName
				j.AgentName = d.AgentName
				j.WorkflowName = d.WorkflowName
				if f.WorkflowID != "" && d.WorkflowID != f.WorkflowID {
					continue
				}
			} else if f.WorkflowID != "" {
				continue
			}
		} else if f.WorkflowID != "" {
			continue
		}
		out = append(out, j)
	}
	return out, nil
}

// stepLabel is one row of the batch-decorate query.
type stepLabel struct {
	WorkflowID   string
	WorkflowName string
	StepName     string
	AgentName    string
}

// lookupStepLabels resolves wf:step:agent names for many step_ids in
// one SQL round-trip. Used by Jobs() and GetJob() so a Jobs-panel
// refresh against a project with N in-flight jobs costs ~1 query, not
// 3N.
func (o *Offline) lookupStepLabels(ctx context.Context, stepIDs []string) map[string]stepLabel {
	if len(stepIDs) == 0 {
		return nil
	}
	placeholders := make([]string, len(stepIDs))
	args := make([]any, len(stepIDs))
	for i, id := range stepIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT s.id, s.name, COALESCE(a.name, ''), s.workflow_id, COALESCE(w.name, '')
	            FROM steps s
	            LEFT JOIN agents a ON a.id = s.agent_id
	            LEFT JOIN workflows w ON w.id = s.workflow_id
	           WHERE s.id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := o.s.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[string]stepLabel, len(stepIDs))
	for rows.Next() {
		var id string
		var l stepLabel
		if err := rows.Scan(&id, &l.StepName, &l.AgentName, &l.WorkflowID, &l.WorkflowName); err != nil {
			return out
		}
		out[id] = l
	}
	return out
}

// GetJob returns one job (DB-backed; Streaming/AttachCount stay 0).
func (o *Offline) GetJob(ctx context.Context, id string) (Job, error) {
	rs := runstore.New(o.s.DB())
	r, err := rs.GetRun(ctx, id)
	if err != nil {
		return Job{}, err
	}
	j := Job{JobResponse: api.FromRun(r)}
	if r.StepID != "" {
		if d, ok := o.lookupStepLabels(ctx, []string{r.StepID})[r.StepID]; ok {
			j.StepName = d.StepName
			j.AgentName = d.AgentName
			j.WorkflowName = d.WorkflowName
		}
	}
	return j, nil
}

func wfName(ctx context.Context, db *sql.DB, wfID string) string {
	if wfID == "" {
		return ""
	}
	var name string
	_ = db.QueryRowContext(ctx,
		`SELECT name FROM workflows WHERE id = ?`, wfID).Scan(&name)
	return name
}

// Workflows lists workflows + their steps + their per-step task counts.
// Global-managed workflows carry SourceType/Source/DefinitionHash/Revision
// and IsStale is computed against the on-disk global registry.
func (o *Offline) Workflows(ctx context.Context, includeSynthetic bool) ([]Workflow, error) {
	ws := workflow.New(o.s.DB(), agent.New(o.s.DB()))
	list, err := ws.List(ctx, includeSynthetic)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}

	// Load the global registry once so we can check staleness for
	// every managed workflow without re-opening registry.json N times.
	var globalMap map[string]globalworkflow.Entry
	if reg, rerr := globalworkflow.Default(); rerr == nil {
		if entries, lerr := reg.List(true); lerr == nil {
			globalMap = make(map[string]globalworkflow.Entry, len(entries))
			for _, e := range entries {
				globalMap[e.Name] = e
			}
		}
	}

	out := make([]Workflow, 0, len(list))
	for _, w := range list {
		full, err := ws.GetByID(ctx, w.ID)
		if err != nil {
			continue
		}
		origin, _ := ws.GetOrigin(ctx, w.ID)
		out = append(out, projectWorkflow(ctx, o.s.DB(), full, origin, globalMap))
	}
	return out, nil
}

func projectWorkflow(ctx context.Context, db *sql.DB, w workflow.Workflow, origin workflow.Origin, globalMap map[string]globalworkflow.Entry) Workflow {
	iso := string(w.Isolation.Normalize())
	out := Workflow{
		ID: w.ID, Name: w.Name, Description: w.Description, IsSynthetic: w.IsSynthetic,
		Isolation: iso,
	}
	firstStep := ""
	stepNames := make(map[string]string, len(w.Steps))
	for _, s := range w.Steps {
		stepNames[s.ID] = s.Name
		if s.ID == w.FirstStepID {
			firstStep = s.Name
		}
	}
	out.FirstStep = firstStep
	for _, s := range w.Steps {
		ws := WorkflowStep{ID: s.ID, Name: s.Name, AgentName: s.AgentName}
		for _, tr := range s.Transitions {
			if tr.IsTaskStatus() {
				ws.NextStatus = append(ws.NextStatus, tr.TaskStatus)
			} else if tr.NextStepName != "" {
				ws.NextSteps = append(ws.NextSteps, tr.NextStepName)
			}
		}
		var n int
		_ = db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tasks WHERE current_step_id = ?`, s.ID).Scan(&n)
		ws.TaskCount = n
		out.Steps = append(out.Steps, ws)
		out.TaskCount += n
	}
	// Non-terminal task list + count: one query yields the per-task
	// rows lazy's isolation confirm popup needs (plan §6.3: the body
	// enumerates every affected task) AND the total count surfaced in
	// the inspector. Replaces the prior COUNT(*) round-trip so the
	// Workflows panel doesn't pay two queries per workflow per
	// refresh tick. Mirrors workflow.Store.UpdateIsolation's guard so
	// the lazy popup and the CLI agree on what "non-terminal" means.
	loadNonTerminalSample(ctx, db, w.ID, stepNames, &out)

	// Origin + staleness.
	out.SourceType = origin.SourceType
	out.Source = origin.Source
	out.DefinitionHash = origin.DefinitionHash
	out.Revision = origin.Revision
	if origin.SourceType == "global" && globalMap != nil {
		if entry, ok := globalMap[origin.Source]; ok {
			out.IsStale = origin.DefinitionHash != entry.DefinitionHash
		} else if origin.Source != "" {
			// Global origin but no longer in registry -
			// treat as stale so the operator notices.
			out.IsStale = true
		}
	}
	return out
}

// loadNonTerminalSample populates out.NonTerminalTaskCount (total) and
// out.NonTerminalTasks (capped sample of NonTerminalTaskSampleSize
// entries, ordered by id ASC). Failures (closed conn, sql syntax
// from a bad migration) are intentionally swallowed so a single
// flaky query doesn't blank the Workflows panel — the inspector
// just shows 0 non-terminal tasks. Same defensive shape the
// per-step COUNT loop above uses.
func loadNonTerminalSample(ctx context.Context, db *sql.DB, workflowID string, stepNames map[string]string, out *Workflow) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, status, COALESCE(current_step_id, '')
		   FROM tasks
		  WHERE workflow_id = ? AND status IN ('new','work','human')
		  ORDER BY id ASC`, workflowID)
	if err != nil {
		return
	}
	defer rows.Close()
	total := 0
	for rows.Next() {
		var id, status, stepID string
		if err := rows.Scan(&id, &status, &stepID); err != nil {
			return
		}
		total++
		if len(out.NonTerminalTasks) < NonTerminalTaskSampleSize {
			out.NonTerminalTasks = append(out.NonTerminalTasks, NonTerminalTaskRef{
				ID:       id,
				Status:   store.Status(status),
				StepName: stepNames[stepID],
			})
		}
	}
	out.NonTerminalTaskCount = total
}

// Agents lists DB agents + registry / package metadata.
func (o *Offline) Agents(ctx context.Context) ([]Agent, error) {
	as := agent.New(o.s.DB())
	list, err := as.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	out := make([]Agent, 0, len(list))
	for _, a := range list {
		out = append(out, o.projectAgent(a))
	}
	// Tasks owned: author OR current_step agent.
	for i := range out {
		var n int
		_ = o.s.DB().QueryRowContext(ctx, `
			SELECT COUNT(*) FROM tasks t
			LEFT JOIN steps s ON s.id = t.current_step_id
			WHERE t.author_id = ? OR s.agent_id = ?
		`, out[i].ID, out[i].ID).Scan(&n)
		out[i].TasksOwned = n
	}
	return out, nil
}

func (o *Offline) projectAgent(a agent.Agent) Agent {
	out := Agent{
		ID: a.ID, Name: a.Name, IsHuman: a.IsHuman,
	}
	if a.IsHuman {
		out.Source = "builtin"
		return out
	}
	out.Source = "db_only"
	if o.registry == nil {
		return out
	}
	if !o.registry.Has(a.Name) {
		return out
	}
	cfg, err := o.registry.Resolve(a.Name)
	if err != nil {
		return out
	}
	out.Source = "installed"
	out.Version = cfg.Version
	out.Model = cfg.Model
	out.Thinking = cfg.Thinking
	out.ExtraArgs = cfg.ExtraArgs
	out.PiExt = cfg.PiExtensions
	out.PiSkills = cfg.PiSkills
	return out
}

// Comments returns the task's comment thread, oldest first.
func (o *Offline) Comments(ctx context.Context, taskID string) ([]Comment, error) {
	cs := comments.New(o.s.DB())
	raw, err := cs.ListByTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(raw))
	for _, c := range raw {
		out = append(out, Comment{
			ID: c.ID, TaskID: c.TaskID, AuthorID: c.AuthorID, AuthorName: c.AuthorName,
			Text: c.Text, CreatedAt: c.CreatedAt,
		})
	}
	return out, nil
}

// signalsBaseQuery is the shared projection for the two Signals
// verbs. step_signals has no synthetic id column (PK = run_id), so
// Signal carries TransitionID + JobID instead.
//
// The workflow id/name come from st.workflow_id → workflows; an
// INNER JOIN is safe because every step row carries a non-NULL
// workflow_id (schema invariant). The TUI uses these to render the
// source side of each signal row as `workflow:step` (matching the
// Jobs panel) instead of the bare step name.
const signalsBaseQuery = `
	SELECT ss.transition_id, ss.task_id, ss.run_id, ss.created_at,
	       dr.step_id, st.name,
	       st.workflow_id, w.name,
	       COALESCE(t.next_step_id, ''), COALESCE(t.task_status, ''),
	       COALESCE(ns.name, ''),
	       st.agent_id, a.name
	  FROM step_signals ss
	  JOIN daemon_runs dr      ON dr.job_id = ss.run_id
	  JOIN steps st            ON st.id = dr.step_id
	  JOIN workflows w         ON w.id = st.workflow_id
	  JOIN agents a            ON a.id = st.agent_id
	  LEFT JOIN step_transitions t  ON t.id = ss.transition_id
	  LEFT JOIN steps ns       ON ns.id = t.next_step_id`

// Signals returns step_signals rows attached to a single run
// (jobID), newest first. Design plan §5.5: the Inspector "Signals"
// tab is scoped to ONE run, so the operator can tell rows emitted by
// the current run apart from rows emitted by earlier runs of the
// same task (kickback loops can leave many).
//
// For task-scoped lookups (the dashboard's Tasks-detail widgets) use
// SignalsForTask instead. The prior implementation overloaded one
// verb based on a `strings.HasPrefix(id, "as-")` sniff; that's
// brittle (silently breaks if id prefixes ever change) and dead
// (the task-scoped branch had no callers). Splitting them gives each
// call site a statically chosen semantic.
//
// Tie-break order: (created_at, transition_id). Within one run
// run_id is constant, so ordering by it doesn't disambiguate
// anything; transition_id is monotonic per (step_id, target) and is
// what step_signals's effective unique tuple is keyed by.
func (o *Offline) Signals(ctx context.Context, jobID string) ([]Signal, error) {
	query := signalsBaseQuery + ` WHERE ss.run_id = ? ORDER BY ss.created_at DESC, ss.transition_id DESC`
	return o.scanSignals(ctx, query, jobID)
}

// SignalsForTask returns every step_signals row attached to a task,
// across all of its runs, newest first. Used by the dashboard's
// Tasks-detail widgets (a kickback loop is one task with many runs;
// the dashboard cares about all of them).
func (o *Offline) SignalsForTask(ctx context.Context, taskID string) ([]Signal, error) {
	query := signalsBaseQuery + ` WHERE ss.task_id = ? ORDER BY ss.created_at DESC, ss.transition_id DESC`
	return o.scanSignals(ctx, query, taskID)
}

func (o *Offline) scanSignals(ctx context.Context, query, arg string) ([]Signal, error) {
	rows, err := o.s.DB().QueryContext(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("list signals: %w", err)
	}
	defer rows.Close()
	var out []Signal
	for rows.Next() {
		var (
			s        Signal
			created  int64
			nextID   string
			status   string
			nextName string
		)
		if err := rows.Scan(&s.TransitionID, &s.TaskID, &s.JobID, &created,
			&s.StepID, &s.StepName,
			&s.WorkflowID, &s.WorkflowName,
			&nextID, &status, &nextName,
			&s.AgentID, &s.AgentName); err != nil {
			return nil, err
		}
		s.CreatedAt = time.Unix(created, 0).UTC()
		switch {
		case status != "":
			s.Target = status
		case nextName != "":
			s.Target = nextName
		default:
			s.Target = "(unknown)"
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Messages reads a job's session.jsonl from disk and projects to
// MessageEvents. Offline always has access to the file; live tabs use
// this when SSE isn't available.
func (o *Offline) Messages(ctx context.Context, jobID string, full bool, limit int) ([]MessageEvent, error) {
	rs := runstore.New(o.s.DB())
	r, err := rs.GetRun(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if r.SessionPath == "" {
		return nil, nil
	}
	if _, err := os.Stat(r.SessionPath); err != nil {
		return nil, nil
	}
	events, err := transcript.Read(r.SessionPath)
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	if !full && limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	out := make([]MessageEvent, 0, len(events))
	for _, e := range events {
		out = append(out, MessageEvent{
			Kind:    string(e.Kind),
			TS:      e.TS,
			Text:    e.Text,
			Name:    e.Name,
			Input:   asAny(e.Input),
			IsError: e.IsError,
			Raw:     asAny(e.Raw),
		})
	}
	return out, nil
}

func asAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// Healthz always reports daemon=down in offline mode.
func (o *Offline) Healthz(ctx context.Context) (Health, error) {
	return Health{Daemon: "down", UpdatedAt: time.Now().UTC()}, nil
}

// Reconnect forces the underlying doltlite store to retire its pooled
// *sqlite3.SQLiteConn so the next read opens a fresh connection at the
// current path. See doltlite.Store.Reconnect for the gory details; the
// short version: this is how lazy recovers from a cross-process
// `dolt_gc()` that rewrote `.autosk/db` under our fd.
func (o *Offline) Reconnect(ctx context.Context) error {
	return o.s.Reconnect(ctx)
}

// ---- writes -------------------------------------------------------------

// CreateTask inserts a task and returns its id.
func (o *Offline) CreateTask(ctx context.Context, title, description string, priority int) (string, error) {
	if title = strings.TrimSpace(title); title == "" {
		return "", fmt.Errorf("title is required")
	}
	if priority < store.MinPriority || priority > store.MaxPriority {
		priority = store.DefaultPriority
	}
	t, err := o.s.CreateTask(ctx, store.Task{Title: title, Description: description, Priority: priority, Status: store.StatusNew})
	if err != nil {
		return "", err
	}
	_ = o.s.DoltCommit(ctx, "lazy: create task "+t.ID)
	return t.ID, nil
}

// UpdateStatus is the lazy-side single entry point for human-driven
// status flips. It routes through internal/tasksvc so the TUI's `d`
// (done), `x` (cancel) and `o` (reopen) hotkeys share the CLI's
// done|cancel|reopen behaviour exactly:
//
//   - terminal targets (done|cancel) clear current_step_id so a
//     task paused in human with a non-null step can actually
//     be closed (SQL CHECK invariant: status='work' ⇔
//     current_step_id IS NOT NULL);
//   - terminal targets also do best-effort worktree cleanup when the
//     task ran under an isolated workflow;
//   - StatusNew on a done|cancel task delegates to tasksvc.Reopen
//     and inherits its precondition (rejects new / work /
//     human sources);
//   - work targets are rejected (workflow lifecycle is owned
//     by the workflow engine).
func (o *Offline) UpdateStatus(ctx context.Context, id string, status store.Status) error {
	opts := tasksvc.Options{ProjectRoot: o.projectRoot}
	var err error
	switch status {
	case store.StatusDone:
		_, err = tasksvc.Done(ctx, o.s, id, opts)
	case store.StatusCancel:
		_, err = tasksvc.Cancel(ctx, o.s, id, opts)
	case store.StatusNew:
		// Mirror the CLI: only valid coming from done|cancel.
		// tasksvc.Reopen returns a clear error otherwise.
		_, err = tasksvc.Reopen(ctx, o.s, id)
	default:
		_, err = tasksvc.SetStatus(ctx, o.s, id, status, opts)
	}
	if err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, "lazy: status "+id+"="+string(status))
	return nil
}

// UpdateTitleDescription rewrites tasks.title and tasks.description
// in one transaction and commits the change to dolt.
//
// Title is trimmed before the store write; an empty title after
// trimming is rejected so the UI can render a flash and keep the
// compose popup open. Description is passed through verbatim so the
// caller can blank it out by submitting an empty string.
func (o *Offline) UpdateTitleDescription(ctx context.Context, id, title, description string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("title required")
	}
	if _, err := o.s.UpdateTask(ctx, id, store.TaskPatch{Title: &title, Description: &description}); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, "lazy: edit "+id)
	return nil
}

// UpdatePriority rewrites tasks.priority.
func (o *Offline) UpdatePriority(ctx context.Context, id string, p int) error {
	if p < store.MinPriority || p > store.MaxPriority {
		return fmt.Errorf("priority must be in [%d,%d]", store.MinPriority, store.MaxPriority)
	}
	if _, err := o.s.UpdateTask(ctx, id, store.TaskPatch{Priority: &p}); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, fmt.Sprintf("lazy: priority %s=%d", id, p))
	return nil
}

// Enroll (re-)attaches an existing task to a workflow's entry step.
//
// When stepName is empty the task lands on the workflow's first_step
// (matching the CLI default for `autosk enroll`); when stepName is
// non-empty the task lands on the named step inside the workflow
// (matching `autosk enroll --step NAME`). Unknown step names are
// rejected with the same shape error the CLI surfaces so the flash
// hint is actionable.
//
// Accepted source statuses: new, human, done, cancel. The only refusal
// is status='work' — the task is currently owned by the engine and
// re-stamping its workflow_id / current_step_id underneath would race
// the daemon; the operator has to cancel then enroll instead.
//
// Matches the status matrix of `autosk enroll`, but unlike the CLI
// verb this code path does NOT allocate a per-task worktree for
// isolation=worktree workflows (a known limitation — the lazy picker
// has no --base-ref / --agent shorthand). For isolated workflows the
// operator should still run `autosk enroll` on the CLI to allocate
// the worktree.
//
// Routes through workflow.EnterStep so the step_visits counter on the
// entry step is bumped and any max_visits cap is enforced. A cap hit
// on first enroll is exotic but legitimate (e.g. someone bumped the
// counter via `metadata set`); we surface it as a clear flash message
// instead of silently succeeding with a stale counter.
func (o *Offline) Enroll(ctx context.Context, id, wfName, stepName string) error {
	t, err := o.s.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if t.Status == store.StatusWork {
		return fmt.Errorf("enroll: task is 'work' (owned by engine); cancel then enroll to switch workflows (or reopen first to inspect in 'new')")
	}
	ws := workflow.New(o.s.DB(), agent.New(o.s.DB()))
	wf, err := ws.GetByName(ctx, wfName)
	if err != nil {
		return fmt.Errorf("workflow %q: %w", wfName, err)
	}
	stepID := wf.FirstStepID
	if stepName != "" {
		resolved, found := "", false
		for _, s := range wf.Steps {
			if s.Name == stepName {
				resolved = s.ID
				found = true
				break
			}
		}
		if !found {
			// Mirror cmd/autosk's stepByName error shape (review R3):
			// include the available step names so the lazy flash hint
			// is actionable on stale cache / hand-edited rows / future
			// programmatic callers.
			names := make([]string, 0, len(wf.Steps))
			for _, s := range wf.Steps {
				names = append(names, s.Name)
			}
			return fmt.Errorf("step %q not found in workflow %s (available: %s)", stepName, wfName, strings.Join(names, ", "))
		}
		stepID = resolved
	}
	if err := workflow.EnterStep(ctx, o.s, ws, workflow.EnterStepInput{
		TaskID:     id,
		StepID:     stepID,
		WorkflowID: wf.ID,
	}); err != nil {
		return workflow.MapEnterStepError(id, err)
	}
	commit := fmt.Sprintf("lazy: enroll %s -> %s", id, wfName)
	if stepName != "" {
		commit = fmt.Sprintf("lazy: enroll %s -> %s --step %s", id, wfName, stepName)
	}
	_ = o.s.DoltCommit(ctx, commit)
	return nil
}

// Resume flips a task from human back to work,
// optionally relocating its current step.
//
// Visit-counter semantics (docs/plans/20260520-Step-Visit-Limits.md):
//
//   - Resume(id, "") does NOT count as a transition; the task stays
//     on the step it was parked on and step_visits is untouched.
//   - Resume(id, "STEP") IS a deliberate transition into STEP and is
//     routed through workflow.EnterStep so step_visits[STEP] bumps and
//     step.max_visits is enforced.
func (o *Offline) Resume(ctx context.Context, id, toStep string) error {
	t, err := o.s.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != store.StatusHuman {
		return fmt.Errorf("resume: task is not 'human' (status=%s)", t.Status)
	}
	if toStep == "" {
		// No transition — just flip the status. Do NOT touch
		// step_visits or current_step_id.
		//
		// Refuse if the parked task has no current_step_id (e.g. someone
		// hand-edited via `autosk sql --write`): without --to we have
		// nowhere to land, and the SQL CHECK invariant
		// (status='work' ⇔ current_step_id IS NOT NULL) would reject
		// the work flip with a cryptic constraint error in the flash
		// bar. Mirror the CLI guard in cmd/autosk/resume.go so the
		// operator sees the actionable hint.
		if t.CurrentStepID == "" {
			return errors.New("task has no current_step_id; pass --to STEP")
		}
		newStatus := store.StatusWork
		if _, err := o.s.UpdateTask(ctx, id, store.TaskPatch{Status: &newStatus}); err != nil {
			return err
		}
		_ = o.s.DoltCommit(ctx, "lazy: resume "+id)
		return nil
	}
	// --to STEP: deliberate transition. Resolve and route through
	// EnterStep so the counter bumps and the cap fires loudly.
	ws := workflow.New(o.s.DB(), agent.New(o.s.DB()))
	st, err := ws.FindStepByName(ctx, t.WorkflowID, toStep)
	if err != nil {
		return fmt.Errorf("resume target step %q: %w", toStep, err)
	}
	if err := workflow.EnterStep(ctx, o.s, ws, workflow.EnterStepInput{
		TaskID: id,
		StepID: st.ID,
	}); err != nil {
		return workflow.MapEnterStepError(id, err)
	}
	_ = o.s.DoltCommit(ctx, "lazy: resume "+id+" --to "+toStep)
	return nil
}

// Block adds a blocker edge id ← blocker.
func (o *Offline) Block(ctx context.Context, id, blocker string) error {
	if err := o.s.Block(ctx, id, blocker); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, "lazy: block "+id+"<-"+blocker)
	return nil
}

// Unblock removes a blocker edge.
func (o *Offline) Unblock(ctx context.Context, id, blocker string) error {
	if err := o.s.Unblock(ctx, id, blocker); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, "lazy: unblock "+id+"<-"+blocker)
	return nil
}

// SetMetadata replaces tasks.metadata with m wholesale. The store's
// UpdateMetadata takes a mutate-in-place callback, so we express a
// full replace as clear+copy. UpdateMetadata returns a `changed`
// flag (false when the resulting JSON encodes identically to the
// previous value); we gate the DoltCommit on it so submitting the
// same JSON twice doesn't churn a no-op revision — matches the
// CLI's `metadata set` path in cmd/autosk/metadata.go.
func (o *Offline) SetMetadata(ctx context.Context, id string, m map[string]any) error {
	_, changed, err := o.s.UpdateMetadata(ctx, id, func(cur map[string]any) error {
		for k := range cur {
			delete(cur, k)
		}
		for k, v := range m {
			cur[k] = v
		}
		return nil
	})
	if err != nil {
		return err
	}
	if changed {
		_ = o.s.DoltCommit(ctx, "lazy: metadata "+id)
	}
	return nil
}

// AddComment inserts a comment authored by the current user.
func (o *Offline) AddComment(ctx context.Context, taskID, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("comment text is empty")
	}
	as := agent.New(o.s.DB())
	authorName := os.Getenv("AUTOSK_AGENT")
	if authorName == "" {
		authorName = agent.HumanAgentName
	}
	a, err := as.EnsureByName(ctx, authorName)
	if err != nil {
		return fmt.Errorf("ensure author %q: %w", authorName, err)
	}
	cs := comments.New(o.s.DB())
	if _, err := cs.Add(ctx, taskID, a.ID, text); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, "lazy: comment "+taskID)
	return nil
}

// CreateWorkflow loads a JSON file from disk and persists it.
func (o *Offline) CreateWorkflow(ctx context.Context, path string) (string, error) {
	def, err := workflow.ParseFile(path)
	if err != nil {
		return "", fmt.Errorf("parse workflow %q: %w", path, err)
	}
	ws := workflow.New(o.s.DB(), agent.New(o.s.DB()))
	wf, err := ws.Create(ctx, def, false)
	if err != nil {
		return "", err
	}
	_ = o.s.DoltCommit(ctx, "lazy: create workflow "+wf.Name)
	return wf.Name, nil
}

// DeleteWorkflow removes a workflow by name. Refuses on referenced wfs.
func (o *Offline) DeleteWorkflow(ctx context.Context, name string) error {
	ws := workflow.New(o.s.DB(), agent.New(o.s.DB()))
	if err := ws.Delete(ctx, name); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, "lazy: delete workflow "+name)
	return nil
}

// UpdateWorkflowIsolation routes through workflow.Store.UpdateIsolation
// so the CLI and the TUI share one code path (plan §2: "Lazy ↔ CLI
// parity"). On success the doltlite commit message mirrors the CLI's
// for consistency in `git log`.
func (o *Offline) UpdateWorkflowIsolation(ctx context.Context, name, mode string, force bool) (UpdateIsolationReport, error) {
	ws := workflow.New(o.s.DB(), agent.New(o.s.DB()))
	wtMgr := worktree.NewManager()
	rep, err := ws.UpdateIsolation(ctx, name, workflow.IsolationMode(mode), workflow.UpdateIsolationOpts{
		Force:       force,
		ProjectRoot: o.projectRoot,
		Worktrees:   wtMgr,
	})
	out := toDatasourceUpdateIsolationReport(rep)
	if err != nil {
		return out, err
	}
	if !rep.Noop {
		_ = o.s.DoltCommit(ctx, fmt.Sprintf("lazy: workflow update %s isolation=%s→%s", name, rep.From, rep.To))
	}
	return out, nil
}

// SyncWorkflows materializes enabled global workflows into the project
// DB using the same path as `autosk workflow sync`. Conflicts and
// per-workflow outcomes are returned in the report; the caller (lazy
// TUI) surfaces them via flash + command log.
func (o *Offline) SyncWorkflows(ctx context.Context, dryRun, force bool) (SyncReport, error) {
	reg, err := globalworkflow.Default()
	if err != nil {
		return SyncReport{}, err
	}
	ws := workflow.New(o.s.DB(), agent.New(o.s.DB()))
	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, reg, ws, globalworkflow.SyncOptions{
		DryRun: dryRun,
		Force:  force,
		InstallAgents: func(ctx context.Context, def workflow.Definition) ([]pkgregistry.Entry, error) {
			return autoInstallMissingAgents(ctx, def, ws.Agents(), o.s, o.registry)
		},
	})
	if !dryRun && rep.Mutated() {
		_ = o.s.DoltCommit(ctx, "lazy: workflow sync global")
	}
	return toDatasourceSyncReport(rep), err
}

// autoInstallMissingAgents mirrors the CLI's auto-install logic so
// the TUI sync path behaves identically. The pkgregistry.Registry
// argument is optional (may be nil in tests); when nil scoped agents
// are skipped (they'll produce validation errors later).
func autoInstallMissingAgents(ctx context.Context, def workflow.Definition, ag *agent.Store, dl *doltlite.Store, reg *pkgregistry.Registry) ([]pkgregistry.Entry, error) {
	if reg == nil {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(def.Steps))
	for _, s := range def.Steps {
		name := strings.TrimSpace(s.AgentName)
		if name == "" || name == agent.HumanAgentName || !looksLikeScopedNpmName(name) {
			continue
		}
		seen[name] = struct{}{}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	var todo []string
	for name := range seen {
		if _, err := ag.GetByName(ctx, name); err == nil {
			continue
		} else if !errors.Is(err, agent.ErrNotFound) {
			return nil, fmt.Errorf("check agent %s: %w", name, err)
		}
		todo = append(todo, name)
	}
	if len(todo) == 0 {
		return nil, nil
	}
	if err := reg.EnsurePrefix(); err != nil {
		return nil, fmt.Errorf("ensure packages prefix: %w", err)
	}
	agWithResolver := agent.New(dl.DB()).WithResolver(reg)
	installed := make([]pkgregistry.Entry, 0, len(todo))
	for _, name := range todo {
		entry, ierr := reg.Install(ctx, name, "")
		if ierr != nil {
			return installed, fmt.Errorf("auto-install %s failed: %w (install manually with `autosk agent install %s`)", name, ierr, name)
		}
		if cfg, rerr := reg.Resolve(entry.Name); rerr == nil && cfg.Runner != "" {
			if err := reg.EnsureRuntime(ctx, ""); err != nil {
				return installed, fmt.Errorf("install runtime for custom-runner agent %s: %w", entry.Name, err)
			}
		}
		if _, eerr := agWithResolver.EnsureByName(ctx, entry.Name); eerr != nil {
			return installed, fmt.Errorf("register %s in agents table: %w", entry.Name, eerr)
		}
		installed = append(installed, entry)
	}
	return installed, nil
}

func looksLikeScopedNpmName(s string) bool {
	if len(s) < 3 || s[0] != '@' {
		return false
	}
	slash := strings.IndexByte(s, '/')
	return slash > 1 && slash < len(s)-1
}

// toDatasourceSyncReport maps the globalworkflow package report to
// the datasource-package mirror.
func toDatasourceSyncReport(r globalworkflow.SyncReport) SyncReport {
	out := SyncReport{
		Prefix: r.Prefix,
		DryRun: r.DryRun,
		Force:  r.Force,
	}
	for _, item := range r.Workflows {
		wj := SyncWorkflowReport{
			Name:           item.Name,
			Status:         string(item.Status),
			WorkflowID:     item.WorkflowID,
			DefinitionHash: item.DefinitionHash,
			PreviousHash:   item.PreviousHash,
			Revision:       item.Revision,
			Reason:         item.Reason,
			Error:          item.Error,
			Mutated:        item.Mutated,
		}
		for _, e := range item.AutoInstalledAgents {
			wj.AutoInstalledAgents = append(wj.AutoInstalledAgents, e.Name+"@"+e.Version)
		}
		out.Workflows = append(out.Workflows, wj)
	}
	return out
}

// toDatasourceUpdateIsolationReport maps the workflow-package report
// to the datasource-package mirror. Keeping the mirror lets the TUI
// stay clear of internal/workflow imports.
func toDatasourceUpdateIsolationReport(r workflow.UpdateIsolationReport) UpdateIsolationReport {
	out := UpdateIsolationReport{
		Workflow:         r.Workflow,
		From:             string(r.From),
		To:               string(r.To),
		Noop:             r.Noop,
		NonTerminalTasks: r.NonTerminalTasks,
		FailedTask:       r.FailedTask,
	}
	for _, e := range r.EnsuredTasks {
		out.EnsuredTasks = append(out.EnsuredTasks, EnsureRecord{
			TaskID: e.TaskID, Path: e.Path, Branch: e.Branch, Existing: e.Existing,
		})
	}
	for _, l := range r.LeftoverWorktrees {
		out.LeftoverWorktrees = append(out.LeftoverWorktrees, LeftoverWorktree{
			TaskID: l.TaskID, Path: l.Path,
		})
	}
	for _, e := range r.RolledBackEnsures {
		out.RolledBackEnsures = append(out.RolledBackEnsures, EnsureRecord{
			TaskID: e.TaskID, Path: e.Path, Branch: e.Branch, Existing: e.Existing,
		})
	}
	return out
}

// InstallAgent is the offline-mode shim that only adds the row to the
// agents table; running 'npm install' from inside the TUI is left to
// the live datasource which dispatches via the daemon. Offline returns
// ErrDaemonRequired so the popup can surface a clean error.
func (o *Offline) InstallAgent(ctx context.Context, name, version string) error {
	return ErrDaemonRequired
}

// UninstallAgent mirrors InstallAgent — needs the registry.
func (o *Offline) UninstallAgent(ctx context.Context, name string) error {
	return ErrDaemonRequired
}

// CancelJob requires a live daemon.
func (o *Offline) CancelJob(ctx context.Context, jobID string) error { return ErrDaemonRequired }

// SendInput requires a live daemon.
func (o *Offline) SendInput(ctx context.Context, jobID, message, behavior string) (string, error) {
	return "", ErrDaemonRequired
}

// AbortJob requires a live daemon.
func (o *Offline) AbortJob(ctx context.Context, jobID string) error { return ErrDaemonRequired }

// StreamLive requires a live daemon.
func (o *Offline) StreamLive(ctx context.Context, jobID string) (*LiveHandle, error) {
	return nil, ErrDaemonRequired
}
