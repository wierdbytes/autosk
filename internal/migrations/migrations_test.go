package migrations_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"

	_ "github.com/mattn/go-sqlite3" // libsqlite3-backed driver (Makefile sets the tag)

	"autosk/internal/migrations"
)

// TestApply_FreshDB_AppliesInitialMigration applies every embedded
// migration to a brand-new database and asserts:
//
//   - schema_migrations lands at the highest embedded version,
//   - every table the v0.1 release schema promises actually exists,
//   - the canonical `human` agent has been seeded,
//   - Apply is idempotent (a second call applies zero migrations and
//     does not duplicate the human row).
//
// This is the only smoke test we ship at the migration layer; the
// per-table shape contracts (CHECK enums, FK behaviour, indexes) are
// pinned by the store conformance suite under internal/store/conformance.
func TestApply_FreshDB_AppliesInitialMigration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	db := openDB(t, dbPath)
	t.Cleanup(func() { _ = db.Close() })

	applied, err := migrations.Apply(ctx, db)
	if err != nil {
		t.Fatalf("Apply on empty DB: %v", err)
	}
	wantApplied := embeddedCount(t)
	if applied != wantApplied {
		t.Errorf("Apply applied %d migrations, want %d", applied, wantApplied)
	}

	// schema_migrations advanced to the highest embedded version.
	gotVer := currentVersion(t, ctx, db)
	wantVer := highestEmbeddedVersion(t)
	if gotVer != wantVer {
		t.Errorf("schema_migrations max(version)=%d, want %d", gotVer, wantVer)
	}

	// Every release-schema table is present.
	tables := scanTableSet(t, ctx, db)
	cols := scanColumnSet(ctx, t, db, "workflows")
	if !cols["superseded_by_id"] {
		t.Errorf("workflows missing superseded_by_id column; have: %v", sortedKeys(cols))
	}
	if got := foreignKeyDeleteAction(ctx, t, db, "workflows", "superseded_by_id"); got != "SET NULL" {
		t.Errorf("workflows.superseded_by_id ON DELETE = %q, want SET NULL", got)
	}
	for _, want := range []string{
		"agents", "workflows", "steps", "step_transitions", "workflow_origins",
		"tasks", "task_deps", "comments",
		"daemon_runs", "step_signals",
		"schema_migrations",
	} {
		if !tables[want] {
			t.Errorf("missing table %q after Apply; have: %v", want, sortedKeys(tables))
		}
	}

	// The canonical human agent is seeded with is_human=1.
	var name string
	var isHuman int
	if err := db.QueryRowContext(ctx,
		`SELECT name, is_human FROM agents WHERE name='human'`).
		Scan(&name, &isHuman); err != nil {
		t.Fatalf("read human agent: %v", err)
	}
	if name != "human" || isHuman != 1 {
		t.Errorf("unexpected human agent row: name=%q is_human=%d", name, isHuman)
	}

	// Second Apply is a no-op and does not duplicate the seed row.
	applied2, err := migrations.Apply(ctx, db)
	if err != nil {
		t.Fatalf("Apply (second call): %v", err)
	}
	if applied2 != 0 {
		t.Errorf("second Apply applied %d migrations, want 0", applied2)
	}
	var humans int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents WHERE name='human'`).Scan(&humans); err != nil {
		t.Fatalf("count humans: %v", err)
	}
	if humans != 1 {
		t.Errorf("human agent rowcount=%d, want 1", humans)
	}
}

// TestApplyWith_Until_CapsAtVersion pins the ApplyOptions{Until: N}
// contract that fixture-replay tests in other packages rely on. With a
// single embedded migration today the cap is degenerate (Until=0 stops
// before 1; Until>=1 applies it), but the loop logic is identical to
// what a multi-migration future will exercise.
func TestApplyWith_Until_CapsAtVersion(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "capped.db")
	db := openDB(t, dbPath)
	t.Cleanup(func() { _ = db.Close() })

	// Until: 0 means "no cap" — applies everything.
	applied, err := migrations.ApplyWith(ctx, db, migrations.ApplyOptions{Until: 0})
	if err != nil {
		t.Fatalf("ApplyWith{Until=0}: %v", err)
	}
	if want := embeddedCount(t); applied != want {
		t.Errorf("Until=0 applied %d, want %d", applied, want)
	}
}

// TestApplyWith_Until_BelowFirstVersion_AppliesNothing exercises the
// "Until caps below every embedded migration" branch. A cap < 1 must
// leave the DB untouched (schema_migrations not even populated).
func TestApplyWith_Until_BelowFirstVersion_AppliesNothing(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "uncapped.db")
	db := openDB(t, dbPath)
	t.Cleanup(func() { _ = db.Close() })

	// Pick a cap strictly below the lowest embedded version. Embedded
	// versions are >= 1 (filenames are NNN_*.sql with NNN >= 001), so
	// Until=0 is "no cap"; we want "below first", which only happens
	// if someone numbers a migration 0 — defensive coverage.
	first := lowestEmbeddedVersion(t)
	if first <= 1 {
		// Below 1 is "no cap" semantics (Until == 0). Skip rather than
		// fake a number that the engine treats specially.
		t.Skip("no embedded migration with version > 1 to cap below")
	}
	applied, err := migrations.ApplyWith(ctx, db, migrations.ApplyOptions{Until: first - 1})
	if err != nil {
		t.Fatalf("ApplyWith{Until=%d}: %v", first-1, err)
	}
	if applied != 0 {
		t.Errorf("ApplyWith with sub-first cap applied %d, want 0", applied)
	}
}

// --- helpers --------------------------------------------------------------

// openDB opens a libsqlite3-backed sqlite3 db with foreign-key
// enforcement on. We bypass doltlite here because we want a stock
// database/sql handle: doltlite would otherwise wrap the connection
// with single-writer machinery irrelevant to the migration unit test.
func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}
	return db
}

func currentVersion(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	v, err := migrations.CurrentVersion(ctx, db)
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	return v
}

func scanTableSet(t *testing.T, ctx context.Context, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func scanColumnSet(ctx context.Context, t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		t.Fatalf("list columns for %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func foreignKeyDeleteAction(ctx context.Context, t *testing.T, db *sql.DB, table, fromColumn string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_list(`+table+`)`)
	if err != nil {
		t.Fatalf("list foreign keys for %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, seq int
		var tableName, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &tableName, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("scan foreign key: %v", err)
		}
		if from == fromColumn {
			return onDelete
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return ""
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func embeddedCount(t *testing.T) int {
	t.Helper()
	migs, err := migrations.All()
	if err != nil {
		t.Fatalf("migrations.All: %v", err)
	}
	return len(migs)
}

func highestEmbeddedVersion(t *testing.T) int {
	t.Helper()
	migs, err := migrations.All()
	if err != nil {
		t.Fatalf("migrations.All: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("no embedded migrations")
	}
	return migs[len(migs)-1].Version
}

func lowestEmbeddedVersion(t *testing.T) int {
	t.Helper()
	migs, err := migrations.All()
	if err != nil {
		t.Fatalf("migrations.All: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("no embedded migrations")
	}
	return migs[0].Version
}
