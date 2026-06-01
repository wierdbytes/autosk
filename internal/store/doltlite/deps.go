package doltlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"autosk/internal/sqlretry"
	"autosk/internal/store"
)

// Block adds edges so that each blocker → id. Variadic, transactional.
//
// Validation:
//   - Each blocker must exist; otherwise ErrBlockerNotFound.
//   - id must exist; otherwise ErrNotFound.
//   - blocker == id is ErrSelfBlock.
//   - Adding an edge that would create a cycle in the blocker graph is ErrCycle.
//
// Re-adding an existing edge is a no-op (idempotent).
func (s *Store) Block(ctx context.Context, id string, blockers ...string) error {
	if s.db == nil {
		return store.ErrNotOpen
	}
	if len(blockers) == 0 {
		return nil
	}
	return sqlretry.OnBusy(ctx, func() error { return s.blockOnce(ctx, id, blockers) })
}

func (s *Store) blockOnce(ctx context.Context, id string, blockers []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := assertTaskExistsTx(ctx, tx, id, store.ErrNotFound); err != nil {
		return err
	}
	for _, b := range blockers {
		if b == id {
			return store.ErrSelfBlock
		}
		if err := assertTaskExistsTx(ctx, tx, b, store.ErrBlockerNotFound); err != nil {
			return err
		}
		// Cycle check: from b, can we reach id via outgoing edges?
		// (If b is already transitively blocking id, that's expected — adding
		// b→id is exactly the edge we want, which doesn't create a cycle.
		// The cycle case is: id already transitively blocks b. Then b→id closes it.)
		cyc, err := reachableTx(ctx, tx, id /*from*/, b /*to*/)
		if err != nil {
			return err
		}
		if cyc {
			return store.ErrCycle
		}
		// INSERT OR IGNORE makes re-adding existing edges a no-op.
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO task_deps(blocker_id, blocked_id, kind) VALUES (?, ?, 'blocks')`,
			b, id); err != nil {
			return fmt.Errorf("insert edge %s→%s: %w", b, id, err)
		}
	}
	return tx.Commit()
}

// Unblock removes specific edges. Missing edges are silently ignored.
func (s *Store) Unblock(ctx context.Context, id string, blockers ...string) error {
	if s.db == nil {
		return store.ErrNotOpen
	}
	if len(blockers) == 0 {
		return nil
	}
	return sqlretry.OnBusy(ctx, func() error { return s.unblockOnce(ctx, id, blockers) })
}

func (s *Store) unblockOnce(ctx context.Context, id string, blockers []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, b := range blockers {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM task_deps WHERE blocker_id = ? AND blocked_id = ? AND kind = 'blocks'`,
			b, id); err != nil {
			return fmt.Errorf("delete edge %s→%s: %w", b, id, err)
		}
	}
	return tx.Commit()
}

// UnblockAll removes every incoming blocker edge for id. Returns the number
// of rows deleted.
func (s *Store) UnblockAll(ctx context.Context, id string) (int, error) {
	if s.db == nil {
		return 0, store.ErrNotOpen
	}
	var n int64
	err := sqlretry.OnBusy(ctx, func() error {
		res, e := s.db.ExecContext(ctx,
			`DELETE FROM task_deps WHERE blocked_id = ? AND kind = 'blocks'`, id)
		if e != nil {
			return e
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("unblock-all: %w", err)
	}
	return int(n), nil
}

// Deps returns the ids of tasks that block id (incoming) and the ids of tasks
// that id blocks (outgoing). Both lists are sorted ascending for determinism.
func (s *Store) Deps(ctx context.Context, id string) (incoming, outgoing []string, err error) {
	if s.db == nil {
		return nil, nil, store.ErrNotOpen
	}
	incoming, err = s.queryIDs(ctx,
		`SELECT blocker_id FROM task_deps WHERE blocked_id = ? AND kind='blocks' ORDER BY blocker_id`, id)
	if err != nil {
		return nil, nil, err
	}
	outgoing, err = s.queryIDs(ctx,
		`SELECT blocked_id FROM task_deps WHERE blocker_id = ? AND kind='blocks' ORDER BY blocked_id`, id)
	if err != nil {
		return nil, nil, err
	}
	return incoming, outgoing, nil
}

// IsBlocked reports whether id has any non-terminal blocker. Only
// done/cancel blockers release their dependents; every other blocker status
// continues to block.
func (s *Store) IsBlocked(ctx context.Context, id string) (bool, error) {
	if s.db == nil {
		return false, store.ErrNotOpen
	}
	var x int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1
		  FROM task_deps d
		  JOIN tasks b ON b.id = d.blocker_id
		 WHERE d.blocked_id = ?
		   AND d.kind = 'blocks'
		   AND b.status NOT IN ('done','cancel')
		 LIMIT 1`, id).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) queryIDs(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// assertTaskExistsTx returns onMissing if id is not in tasks.
func assertTaskExistsTx(ctx context.Context, tx *sql.Tx, id string, onMissing error) error {
	var x int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, id).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return onMissing
	}
	return err
}

// reachableTx reports whether `to` is reachable from `from` by following
// outgoing blocker edges (from → x → ... → to). Iterative BFS, capped at
// the number of nodes in the graph.
//
// Used by Block: if `to` (a proposed blocker) is reachable from `from` (the
// task being blocked), adding from→to as an edge... wait, that's not what we
// want. The cycle case for adding edge b→id is: id already transitively
// blocks b (id→...→b). Then b→id closes the cycle.
//
// So caller passes from=id, to=b. Returns true if id→...→b exists.
func reachableTx(ctx context.Context, tx *sql.Tx, from, to string) (bool, error) {
	if from == to {
		return true, nil
	}
	visited := map[string]bool{from: true}
	frontier := []string{from}
	for len(frontier) > 0 {
		next := frontier
		frontier = nil
		// Build placeholder list for IN clause.
		ph := make([]string, len(next))
		args := make([]any, len(next))
		for i, n := range next {
			ph[i] = "?"
			args[i] = n
		}
		q := `SELECT blocker_id, blocked_id FROM task_deps
		       WHERE kind='blocks' AND blocker_id IN (` + joinComma(ph) + `)`
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return false, fmt.Errorf("cycle query: %w", err)
		}
		var newlyDiscovered []string
		for rows.Next() {
			var br, bd string
			if err := rows.Scan(&br, &bd); err != nil {
				_ = rows.Close()
				return false, err
			}
			if bd == to {
				_ = rows.Close()
				return true, nil
			}
			if !visited[bd] {
				visited[bd] = true
				newlyDiscovered = append(newlyDiscovered, bd)
			}
			_ = br // unused; kept for SQL symmetry
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return false, err
		}
		_ = rows.Close()
		frontier = newlyDiscovered
	}
	return false, nil
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
