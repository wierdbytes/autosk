// Package datasource is the only seam between the lazy TUI and the
// rest of autosk. Contexts, controllers, and helpers never reach into
// internal/store, internal/daemon/client, or internal/workflow
// directly — they go through a Datasource.
//
// Three implementations live alongside:
//
//   - offline: reads .autosk/db directly via internal/store +
//     internal/agent + internal/workflow + internal/comments +
//     internal/daemon/runstore + internal/daemon/transcript. Writes
//     mutate the DB in-process (the same code paths the CLI commands
//     in cmd/autosk/*.go use). Live (SSE input/abort) verbs are no-ops
//     that return ErrDaemonRequired.
//
//   - live: same read surface, but Jobs and the transcript stream come
//     from the daemon's HTTP/UDS API (internal/daemon/client). Used
//     when the daemon is reachable so the TUI sees the in-flight pi
//     state (Streaming / AttachCount) rather than the stale DB row.
//
//   - compose: 2s health probe over the UDS that flips between live
//     and offline. Exposes a Health channel the status-bar reads.
//
// All verbs are context-cancellable and MUST return within a few
// hundred milliseconds. Long-running streams (the Live tab's SSE) are
// expressed as a dedicated StreamLive() method that returns a handle
// the caller can close.
package datasource

import (
	"context"
	"errors"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/store"
)

// ErrDaemonRequired is returned by offline-mode verbs that can't be
// answered without a live daemon (Live tab SSE, /input, /abort,
// CancelJob).
var ErrDaemonRequired = errors.New("daemon required")

// TaskRef is a lightweight reference to a related task carrying just
// enough metadata for the detail pane to render the id with the right
// status hue without re-querying the store. Used in Task.BlockedBy
// and Task.Blocks.
type TaskRef struct {
	ID     string
	Status store.Status
}

// Task is the lazy TUI's projection of a task row. Derived fields
// (WorkflowName, StepName, AgentName, Blocked, CommentCount) are
// resolved by the datasource so the TUI never joins by hand.
type Task struct {
	ID            string
	Title         string
	Description   string
	Status        store.Status
	Priority      int
	AuthorID      string
	AuthorName    string
	WorkflowID    string
	WorkflowName  string
	CurrentStepID string
	StepName      string
	AgentName     string // current step's agent
	Blocked       bool
	BlockedBy     []TaskRef // every blocker (open and closed), in store-order
	Blocks        []TaskRef // every task this task blocks (open and closed)
	CommentCount  int
	// Metadata is the raw tasks.metadata JSON object. The Tasks-panel
	// `M` hotkey reads it (pretty-prints with json.MarshalIndent) and
	// writes it back wholesale via Datasource.SetMetadata. nil when
	// the column is SQL NULL.
	Metadata  map[string]any
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Job mirrors api.JobResponse with a few cross-referenced helper
// fields so the Jobs list can render the workflow:step label without
// a second lookup.
type Job struct {
	api.JobResponse
	WorkflowName string
	StepName     string
	AgentName    string
}

// Workflow is the projection of a workflows row + steps.
type Workflow struct {
	ID          string
	Name        string
	Description string
	IsSynthetic bool
	FirstStep   string
	Steps       []WorkflowStep
	TaskCount   int // tasks pointing at this workflow
	// Isolation mirrors workflows.isolation ("none" | "worktree"). Empty
	// in the on-disk row collapses to "none" at scan time. Surfaced on
	// the Workflows panel ([wt] marker) and the workflow inspector.
	Isolation string
	// NonTerminalTaskCount is the TOTAL number of tasks currently in a
	// non-terminal state ({new, work, human}) pointing at this
	// workflow. The workflow inspector uses it to render the
	// "(N non-terminal task(s) currently use this)" hint. Lazy's
	// isolation popup uses it as the denominator when rendering the
	// per-task list with a cap-at-NonTerminalTaskSampleSize suffix.
	NonTerminalTaskCount int
	// NonTerminalTasks is a bounded sample (first
	// NonTerminalTaskSampleSize by id ASC) of the workflow's
	// non-terminal tasks. Lazy's isolation confirm popup
	// enumerates this list (plan §6.3 + §8 risk #4: cap at 10 with
	// `... and N more` suffix). The full list is always available
	// via `autosk list --workflow <name>`.
	NonTerminalTasks []NonTerminalTaskRef
	// Origin fields surface provenance for managed workflows.
	// SourceType is "" for unmanaged (operator-created) workflows.
	SourceType     string
	Source         string
	DefinitionHash string
	Revision       string
	// IsStale is true when the workflow is globally managed and the
	// local definition hash no longer matches the global registry.
	IsStale bool
}

// NonTerminalTaskSampleSize bounds Workflow.NonTerminalTasks (the
// sample lazy renders in the isolation confirm popup body). Sized to
// fit comfortably in the popup before the `... and N more` suffix
// kicks in; pinned in popup_isolation_test.go.
const NonTerminalTaskSampleSize = 10

// NonTerminalTaskRef is one row of Workflow.NonTerminalTasks — the
// information the isolation confirm popup needs to render one
// "  - <id> (status=..., current step=...)" line per affected task.
// Separate from TaskRef (which is used for blocker references and
// intentionally carries only id+status) so the slimmer blocker
// renderer doesn't grow a StepName field it doesn't use.
type NonTerminalTaskRef struct {
	ID       string
	Status   store.Status
	StepName string
}

// WorkflowStep is one row of a workflow's step graph.
type WorkflowStep struct {
	ID         string
	Name       string
	AgentName  string
	NextSteps  []string // resolved names
	NextStatus []string // task_status transitions (terminal)
	TaskCount  int      // tasks whose current_step_id == this step
}

// Agent is the projection of an agents row + (optional) package
// metadata pulled from the pkgregistry.
type Agent struct {
	ID         string
	Name       string
	IsHuman    bool
	Source     string // "builtin" | "installed" | "db_only"
	Version    string // empty unless Source=="installed"
	Model      string
	Thinking   string
	ExtraArgs  []string
	PiSkills   []string
	PiExt      []string
	TasksOwned int // tasks where author_id == id OR current_step.agent_id == id
}

// Comment mirrors comments.Comment for rendering convenience.
type Comment struct {
	ID         int64
	TaskID     string
	AuthorID   string
	AuthorName string
	Text       string
	CreatedAt  time.Time
}

// Signal is one step_signals row (the rows agents insert via
// `autosk step next`). Decoded inline because the workflow store
// doesn't expose them.
//
// step_signals exposes a (run_id, transition_id) tuple but no
// synthetic id; we project TransitionID alongside JobID so the
// inspector can render "step_next via transition #N" if anyone needs
// it. The transition_id is also decoded into Target via the
// step_transitions row's NextStepName / TaskStatus.
type Signal struct {
	TransitionID int64
	TaskID       string
	JobID        string
	StepID       string
	StepName     string
	WorkflowID   string
	WorkflowName string
	Target       string // sibling step name OR done|cancel|human
	AgentID      string
	AgentName    string
	CreatedAt    time.Time
}

// Health describes the datasource's view of the daemon.
type Health struct {
	Daemon    string // "ok" | "down" | "stale"
	Workers   int
	Queued    int
	Running   int
	UpdatedAt time.Time
}

// IsOK reports whether daemon is reachable and healthy.
func (h Health) IsOK() bool { return h.Daemon == "ok" }

// MessageEvent is the transcript event shape rendered by Archive /
// Live tabs. Same wire shape as api.MessageEvent but importable
// without pulling daemon/api into TUI code.
type MessageEvent = api.MessageEvent

// LiveEvent is one SSE frame from StreamLive.
type LiveEvent struct {
	// Kind is "message" | "status" | "done" | "error".
	Kind    string
	Message MessageEvent    // populated when Kind == "message"
	Status  api.JobResponse // populated when Kind == "status" or "done"
	Err     error           // populated when Kind == "error"
	EventID int             // SSE id, 0 when absent
}

// LiveHandle is the active SSE subscription returned by StreamLive.
type LiveHandle struct {
	Events <-chan LiveEvent
	close  func() error
}

// Close terminates the SSE stream. Idempotent.
func (h *LiveHandle) Close() error {
	if h == nil || h.close == nil {
		return nil
	}
	return h.close()
}

// NewLiveHandle is the constructor live.go uses; exported for the
// composer.
func NewLiveHandle(ch <-chan LiveEvent, closeFn func() error) *LiveHandle {
	return &LiveHandle{Events: ch, close: closeFn}
}

// UpdateIsolationReport is the lazy-side mirror of
// workflow.UpdateIsolationReport. We keep a parallel struct so the
// TUI never imports internal/workflow directly (the datasource
// package is the only seam between the TUI and the rest of the
// project).
type UpdateIsolationReport struct {
	Workflow          string
	From              string
	To                string
	Noop              bool
	NonTerminalTasks  []string
	EnsuredTasks      []EnsureRecord
	LeftoverWorktrees []LeftoverWorktree
	RolledBackEnsures []EnsureRecord
	FailedTask        string
}

// EnsureRecord and LeftoverWorktree mirror the workflow-package types
// of the same names.
type EnsureRecord struct {
	TaskID   string
	Path     string
	Branch   string
	Existing bool
}

type LeftoverWorktree struct {
	TaskID string
	Path   string
}

// SyncReport is the lazy-side mirror of globalworkflow.SyncReport.
type SyncReport struct {
	Prefix    string
	DryRun    bool
	Force     bool
	Workflows []SyncWorkflowReport
}

// Mutated reports whether this non-dry-run sync changed the project DB.
func (r SyncReport) Mutated() bool {
	if r.DryRun {
		return false
	}
	for _, wf := range r.Workflows {
		if wf.Mutated {
			return true
		}
	}
	return false
}

// SyncWorkflowReport is the lazy-side mirror of
// globalworkflow.SyncWorkflowReport.
type SyncWorkflowReport struct {
	Name                string
	Status              string
	WorkflowID          string
	DefinitionHash      string
	PreviousHash        string
	Revision            string
	Reason              string
	Error               string
	AutoInstalledAgents []string // "name@version" strings for display
	Mutated             bool
}

// Datasource is the read+write contract the TUI talks through.
type Datasource interface {
	// ---- reads (return promptly; safe to call from g.OnWorker) ----

	Tasks(ctx context.Context, f TaskFilter) ([]Task, error)
	GetTask(ctx context.Context, id string) (Task, error)
	Jobs(ctx context.Context, f JobFilter) ([]Job, error)
	GetJob(ctx context.Context, id string) (Job, error)
	Workflows(ctx context.Context, includeSynthetic bool) ([]Workflow, error)
	Agents(ctx context.Context) ([]Agent, error)
	Comments(ctx context.Context, taskID string) ([]Comment, error)
	// Signals returns step_signals rows attached to a single run
	// (jobID), newest first. Design plan §5.5: the Inspector "Signals"
	// tab is scoped to ONE run.
	Signals(ctx context.Context, jobID string) ([]Signal, error)
	// SignalsForTask returns every step_signals row attached to a task
	// across all of its runs. Used by the dashboard's Tasks-detail
	// widgets where the operator wants the full kickback history.
	SignalsForTask(ctx context.Context, taskID string) ([]Signal, error)
	Messages(ctx context.Context, jobID string, full bool, limit int) ([]MessageEvent, error)
	Healthz(ctx context.Context) (Health, error)

	// ---- writes (task lifecycle) ----

	CreateTask(ctx context.Context, title, description string, priority int) (string, error)
	UpdateStatus(ctx context.Context, id string, status store.Status) error
	UpdatePriority(ctx context.Context, id string, p int) error
	// UpdateTitleDescription rewrites tasks.title and tasks.description
	// in one call. Either field may be unchanged from the current task —
	// callers are expected to pre-load the current values (or accept
	// the wipe) and submit a full pair. Returns an error when title
	// is empty after trimming so the UI can render a flash.
	UpdateTitleDescription(ctx context.Context, id, title, description string) error
	// Enroll (re-)attaches a task to a workflow. When stepName is empty
	// the task lands on the workflow's first step (CLI default);
	// otherwise it lands on the named step (CLI parity with
	// `autosk enroll --step NAME`).
	Enroll(ctx context.Context, id, workflow, stepName string) error
	Resume(ctx context.Context, id, toStep string) error
	Block(ctx context.Context, id, blocker string) error
	Unblock(ctx context.Context, id, blocker string) error
	AddComment(ctx context.Context, taskID, text string) error
	// SetMetadata replaces tasks.metadata wholesale with m. A nil or
	// empty map clears the metadata column (renders as "{}" on read).
	SetMetadata(ctx context.Context, id string, m map[string]any) error

	// ---- writes (workflow / agent) ----

	CreateWorkflow(ctx context.Context, jsonOrPath string) (string, error)
	DeleteWorkflow(ctx context.Context, name string) error
	// UpdateWorkflowIsolation flips the workflows.isolation column.
	// The mode string is the same shape as `autosk workflow update
	// --isolation`: "none" or "worktree". force=true bypasses the
	// non-terminal-tasks guard with mode-specific side-effects (per
	// task Ensure for none→worktree; leftover worktree paths surfaced
	// in Report.LeftoverWorktrees for worktree→none). DryRun is not
	// exposed at the datasource layer: the TUI's confirmation popup
	// is the preview; the call always commits.
	UpdateWorkflowIsolation(ctx context.Context, name, mode string, force bool) (UpdateIsolationReport, error)
	// SyncWorkflows materializes enabled global workflows into the
	// project DB. It uses the same code path as `autosk workflow sync`
	// (globalworkflow.SyncGlobalWorkflows) so the TUI and CLI agree
	// on every outcome. force=true replaces changed global-managed
	// workflows; dryRun previews without writing.
	SyncWorkflows(ctx context.Context, dryRun, force bool) (SyncReport, error)
	InstallAgent(ctx context.Context, name, version string) error
	UninstallAgent(ctx context.Context, name string) error

	// ---- jobs (live only) ----

	CancelJob(ctx context.Context, jobID string) error
	SendInput(ctx context.Context, jobID, message, behavior string) (string, error)
	AbortJob(ctx context.Context, jobID string) error

	// Reconnect drops any pooled connection inside the underlying
	// store and forces the next query to acquire a fresh
	// *sqlite3.SQLiteConn. The lever lazy uses to recover from a
	// cross-process `dolt_gc()` that atomic-rewrote `.autosk/db` out
	// from under our fd. Safe to call concurrently with in-flight
	// reads; idempotent. Offline + Compose forward to the doltlite
	// store; daemon-only datasources may treat this as a no-op.
	Reconnect(ctx context.Context) error

	// StreamLive opens an SSE subscription to a job. Caller must call
	// LiveHandle.Close. Offline datasources return ErrDaemonRequired.
	StreamLive(ctx context.Context, jobID string) (*LiveHandle, error)
}

// TaskFilter narrows Tasks results. Empty struct == "default open work".
//
// Agent scoping has three flavours because the design plan §3.4
// explicitly distinguishes them:
//   - AgentName       – broad: match either AuthorName OR StepAgentName.
//     Used by the agent: facet for backwards
//     compatibility with the help text.
//   - AuthorName      – narrow: filter on tasks.author_id only.
//     Used when the Agents-panel popup chooses
//     "by author".
//   - StepAgentName   – narrow: filter on current_step.agent_id only.
//     Used when the Agents-panel popup chooses
//     "by current step".
type TaskFilter struct {
	Statuses      []store.Status
	Priority      *int
	WorkflowID    string
	AgentName     string // broad match (author OR current step's agent)
	AuthorName    string // narrow: author_id only
	StepAgentName string // narrow: current_step.agent_id only
	Search        string // substring match on id / title (case-insensitive)
}

// DefaultTaskFilter returns the filter that mirrors `autosk list`
// default behaviour: open work (new/work/human).
func DefaultTaskFilter() TaskFilter {
	return TaskFilter{Statuses: store.OpenStatuses()}
}

// JobFilter narrows Jobs results.
type JobFilter struct {
	TaskID     string
	WorkflowID string
	Statuses   []string
	Limit      int
}
