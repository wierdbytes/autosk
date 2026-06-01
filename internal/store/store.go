// Package store defines the storage abstraction autosk's commands sit on top
// of. Two implementations live alongside: doltlite (default, MVP) and
// doltserver (build-tagged stub).
package store

import (
	"context"
	"time"
)

// Status is the lifecycle state of a task. See docs/plans/20260517-Workflows-Plan.md §5.1.
//
// Transitions are unconstrained at the SQL layer (subject only to the
// CHECK that ties `work` to a non-null `current_step_id`). The CLI
// and the daemon are responsible for the rest of the lifecycle.
//
// Every value is ≤ 7 characters so the TUI status column and `--status`
// flags can render the strings without ellipsis truncation; see
// docs/plans/20260521-Short-Statuses.md for the rename rationale.
//
// "blocked" is NOT a stored status. A task is blocked iff it has at least
// one blocker edge whose blocker has not reached a terminal status
// (done|cancel); this is computed by Ready and by GetTask consumers.
type Status string

const (
	StatusNew    Status = "new"
	StatusWork   Status = "work"
	StatusHuman  Status = "human"
	StatusDone   Status = "done"
	StatusCancel Status = "cancel"
)

// Valid reports whether s is one of the five allowed values.
func (s Status) Valid() bool {
	switch s {
	case StatusNew, StatusWork, StatusHuman, StatusDone, StatusCancel:
		return true
	}
	return false
}

// AllStatuses returns the enum in canonical order.
func AllStatuses() []Status {
	return []Status{StatusNew, StatusWork, StatusHuman, StatusDone, StatusCancel}
}

// OpenStatuses returns the statuses that count as "open work" — the
// default filter for `autosk list`.
func OpenStatuses() []Status {
	return []Status{StatusNew, StatusWork, StatusHuman}
}

// MinPriority and MaxPriority bound the priority range (0 = highest).
const (
	MinPriority     = 0
	MaxPriority     = 3
	DefaultPriority = 2
)

// Task is the core domain object.
//
// AuthorID, WorkflowID, CurrentStepID are nullable FKs: empty string
// means "unset / NULL". The SQL CHECK enforces the invariant
// (status='work' ⇔ current_step_id IS NOT NULL).
//
// Metadata is a free-form JSON object (tasks.metadata TEXT column).
// Nil here means "NULL on disk"; an empty map round-trips back to NULL
// so empty/absent metadata is indistinguishable to consumers. The
// engine reserves the top-level key `step_visits` for per-step visit
// counters — see internal/meta.
type Task struct {
	ID            string
	Title         string
	Description   string
	Status        Status
	Priority      int
	AuthorID      string
	WorkflowID    string
	CurrentStepID string
	Metadata      map[string]any
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// TaskPatch is a partial update. Nil fields are left unchanged. The
// workflow / step pointers can be cleared by passing a non-nil pointer to
// an empty string.
//
// Metadata uses replace-wholesale semantics: nil = unchanged; non-nil
// (including a pointer to an empty / nil map) = REPLACE the column.
// Callers that need a partial edit must load → mutate → patch; the
// doltlite backend exposes UpdateMetadata for the read-modify-write
// pattern in a single transaction.
type TaskPatch struct {
	Title         *string
	Description   *string
	Status        *Status
	Priority      *int
	WorkflowID    *string
	CurrentStepID *string
	Metadata      *map[string]any
}

// IsEmpty reports whether the patch would change nothing.
func (p TaskPatch) IsEmpty() bool {
	return p.Title == nil && p.Description == nil && p.Status == nil &&
		p.Priority == nil && p.WorkflowID == nil && p.CurrentStepID == nil &&
		p.Metadata == nil
}

// ListFilter narrows ListTasks results.
//
// Semantics:
//   - Statuses == nil  → backend default ({new, work, human} — open work).
//   - Statuses == []   → no filter (all statuses).
//   - Priority == nil  → no priority filter.
//   - Limit  ==  0     → backend default (typically unlimited or a sane cap).
type ListFilter struct {
	Statuses []Status
	Priority *int
	Limit    int
}

// Store is the storage abstraction. Every backend implements this interface.
//
// All methods are safe to call after a successful Open; behavior before Open
// or after Close is implementation-defined (usually returns an error).
type Store interface {
	// Lifecycle.
	Open(ctx context.Context, dbPath string) error
	Close() error
	Migrate(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)

	// Task CRUD.
	CreateTask(ctx context.Context, t Task) (Task, error)
	GetTask(ctx context.Context, id string) (Task, error)
	UpdateTask(ctx context.Context, id string, p TaskPatch) (Task, error)
	ListTasks(ctx context.Context, f ListFilter) ([]Task, error)

	// DeleteTask removes a task row. Used by the worktree-isolation
	// rollback path in `autosk create` when the freshly-created row
	// must be unwound because the worktree could not be allocated.
	//
	// FK CASCADE handles every dependent row declared in the schema:
	// task_deps (both blocker_id and blocked_id), comments,
	// daemon_runs and — transitively through daemon_runs —
	// step_signals. A future caller deleting a task with run history
	// attached therefore does NOT need to reap those rows first.
	//
	// Returns ErrNotFound when no row matched.
	DeleteTask(ctx context.Context, id string) error

	// UpdateMetadata is the read-modify-write helper used by engine and
	// CLI callers that need to mutate one or two keys in tasks.metadata
	// without clobbering concurrent writers.
	//
	// Contract for backends:
	//   - fn ALWAYS receives a non-nil map (NULL columns are presented
	//     as an empty map[string]any{}); fn may mutate it in place.
	//   - Returning a non-nil error from fn aborts the transaction.
	//   - When the post-fn marshalled metadata equals the pre-fn state
	//     (i.e. fn was a no-op for storage purposes), the SQL UPDATE is
	//     skipped and the returned `changed` flag is false. updated_at
	//     is NOT bumped in that case; CLI callers use this to skip the
	//     accompanying dolt-commit audit row.
	//   - An empty post-fn map is written back as SQL NULL so the
	//     column round-trips cleanly.
	UpdateMetadata(ctx context.Context, id string, fn func(m map[string]any) error) (m map[string]any, changed bool, err error)

	// UpdateMetadataAndPatch is the atomic engine-side helper that
	// performs a metadata read-modify-write AND a TaskPatch in a single
	// transaction. workflow.EnterStep uses it so the step_visits counter
	// bump and the task pointer move (workflow_id / current_step_id /
	// status) land or fail together.
	//
	// p.Metadata MUST be nil; route metadata mutations through fn.
	// Returns the resulting Task as read after commit.
	UpdateMetadataAndPatch(ctx context.Context, id string, fn func(m map[string]any) error, p TaskPatch) (Task, error)

	// Edges. Variadic; runs in a single transaction; rejects self-block and
	// cycles. Block is idempotent (re-adding an existing edge is a no-op).
	Block(ctx context.Context, id string, blockers ...string) error
	Unblock(ctx context.Context, id string, blockers ...string) error
	UnblockAll(ctx context.Context, id string) (removed int, err error)

	// Deps returns the ids of tasks that block id (incoming) and the ids
	// of tasks that id blocks (outgoing).
	Deps(ctx context.Context, id string) (incoming, outgoing []string, err error)

	// IsBlocked is the derived `blocked` flag: true iff id has at least one
	// incoming blocker edge whose blocker is not terminal (done|cancel).
	IsBlocked(ctx context.Context, id string) (bool, error)

	// Ready returns tasks where status='new' AND every incoming blocker is
	// terminal (done|cancel). Sorted priority ASC, created_at ASC.
	Ready(ctx context.Context, limit int) ([]Task, error)

	// Raw passthrough for `autosk sql`. Implementations may refuse writes
	// when readOnly is true; "writes" is interpreted dialect-specifically.
	QueryRaw(ctx context.Context, q string, args ...any) (Rows, error)
	ExecRaw(ctx context.Context, q string, args ...any) (Result, error)
}

// Rows is a minimal subset of database/sql.Rows so the interface stays portable.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Columns() ([]string, error)
	Err() error
	Close() error
}

// Result mirrors database/sql.Result.
type Result interface {
	LastInsertId() (int64, error)
	RowsAffected() (int64, error)
}
