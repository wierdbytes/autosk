package doltlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosk/internal/sqlretry"
	"autosk/internal/store"
)

// ListTasks returns tasks matching the filter. See ListFilter docs for
// nil/empty semantics.
func (s *Store) ListTasks(ctx context.Context, f store.ListFilter) ([]store.Task, error) {
	if s.db == nil {
		return nil, store.ErrNotOpen
	}

	var (
		where []string
		args  []any
	)

	statuses := defaultStatuses(f.Statuses)
	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, st := range statuses {
			placeholders[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "status IN ("+strings.Join(placeholders, ",")+")")
	}

	if f.Priority != nil {
		where = append(where, "priority = ?")
		args = append(args, *f.Priority)
	}

	q := `SELECT id, title, description, status, priority,
		           author_id, workflow_id, current_step_id,
		           metadata,
		           created_at, updated_at
		    FROM tasks`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY priority ASC, created_at ASC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	return s.scanTasks(ctx, q, args...)
}

// Ready returns the ready set: status='new' AND every incoming blocker is
// already in a terminal state (done|cancel). Tasks already work
// or human are owned by the daemon / a human and are not surfaced
// as "ready to pick up".
func (s *Store) Ready(ctx context.Context, limit int) ([]store.Task, error) {
	if s.db == nil {
		return nil, store.ErrNotOpen
	}
	q := `SELECT t.id, t.title, t.description, t.status, t.priority,
		           t.author_id, t.workflow_id, t.current_step_id,
		           t.metadata,
		           t.created_at, t.updated_at
		    FROM tasks t
		   WHERE t.status = 'new'
		     AND NOT EXISTS (
		         SELECT 1 FROM task_deps d
		           JOIN tasks b ON b.id = d.blocker_id
		          WHERE d.blocked_id = t.id
		            AND b.status NOT IN ('done','cancel'))
		ORDER BY t.priority ASC, t.created_at ASC`
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	return s.scanTasks(ctx, q, args...)
}

// UpdateTask applies a partial update and returns the resulting row.
func (s *Store) UpdateTask(ctx context.Context, idStr string, p store.TaskPatch) (store.Task, error) {
	if s.db == nil {
		return store.Task{}, store.ErrNotOpen
	}
	if p.IsEmpty() {
		return s.GetTask(ctx, idStr)
	}
	if err := validatePatch(p); err != nil {
		return store.Task{}, err
	}

	sets, args, err := patchSetsAndArgs(p)
	if err != nil {
		return store.Task{}, err
	}
	if p.Metadata != nil {
		metaArg, merr := marshalMetadata(*p.Metadata)
		if merr != nil {
			return store.Task{}, merr
		}
		sets = append(sets, "metadata = ?")
		args = append(args, metaArg)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC().Unix())
	args = append(args, idStr)

	q := "UPDATE tasks SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	var res sql.Result
	err = sqlretry.OnBusy(ctx, func() error {
		var e error
		res, e = s.db.ExecContext(ctx, q, args...)
		return e
	})
	if err != nil {
		return store.Task{}, fmt.Errorf("update task: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either id doesn't exist, or update had no effect (unlikely given we
		// always bump updated_at). Distinguish via GetTask.
		if _, err := s.GetTask(ctx, idStr); err != nil {
			return store.Task{}, err
		}
		return store.Task{}, errors.New("update affected 0 rows")
	}
	return s.GetTask(ctx, idStr)
}

// ---- helpers --------------------------------------------------------------

func (s *Store) scanTasks(ctx context.Context, q string, args ...any) ([]store.Task, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()
	var out []store.Task
	for rows.Next() {
		t, err := scanOneTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// defaultStatuses applies the "nil = backend default" rule.
// nil  → OpenStatuses (new + work + human).
// []   → no filter (all statuses).
// list → as given.
func defaultStatuses(in []store.Status) []store.Status {
	if in == nil {
		return store.OpenStatuses()
	}
	return in
}

// patchSetsAndArgs translates the non-metadata fields of p into
// matching SQL SET-clause fragments and the parameter list. The
// metadata column and the trailing updated_at + WHERE id are appended
// by the caller (UpdateTask vs UpdateMetadataAndPatch handle metadata
// differently). Returns an error for inputs that fail per-field
// validation (today: empty title).
func patchSetsAndArgs(p store.TaskPatch) (sets []string, args []any, err error) {
	if p.Title != nil {
		title := strings.TrimSpace(*p.Title)
		if title == "" {
			return nil, nil, store.ErrEmptyTitle
		}
		sets = append(sets, "title = ?")
		args = append(args, title)
	}
	if p.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *p.Description)
	}
	if p.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, string(*p.Status))
	}
	if p.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *p.Priority)
	}
	if p.WorkflowID != nil {
		sets = append(sets, "workflow_id = ?")
		args = append(args, nullText(*p.WorkflowID))
	}
	if p.CurrentStepID != nil {
		sets = append(sets, "current_step_id = ?")
		args = append(args, nullText(*p.CurrentStepID))
	}
	return sets, args, nil
}

func validatePatch(p store.TaskPatch) error {
	if p.Status != nil && !p.Status.Valid() {
		return store.ErrInvalidStatus
	}
	if p.Priority != nil && (*p.Priority < store.MinPriority || *p.Priority > store.MaxPriority) {
		return store.ErrInvalidPriority
	}
	return nil
}

// _ silences unused-import warning if sql is unreferenced (it is referenced
// implicitly via *sql.DB usage in other files).
var _ = sql.ErrNoRows
