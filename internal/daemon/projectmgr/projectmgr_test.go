package projectmgr_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/agent/pkgregistry"
	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/projectmgr"
	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/scheduler"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// fakeNpm is a stub for pkgregistry — projectmgr never uses it directly,
// but Manager.Deps.Packages must be non-nil for executor.Resolve to behave.
type fakeNpm struct{}

func (fakeNpm) Install(_ context.Context, prefix, spec string) error   { return nil }
func (fakeNpm) Uninstall(_ context.Context, prefix, name string) error { return nil }

func newManagerWithSched(t *testing.T) (*projectmgr.Manager, *scheduler.Scheduler, func()) {
	t.Helper()
	dir := t.TempDir()
	reg, err := pkgregistry.Open(filepath.Join(dir, "packages"), pkgregistry.WithNpm(fakeNpm{}))
	if err != nil {
		t.Fatalf("pkgregistry.Open: %v", err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	exec := scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error { return nil })
	sched := scheduler.New(exec, scheduler.Config{Workers: 1})
	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("scheduler start: %v", err)
	}
	mgr := projectmgr.New(projectmgr.Deps{
		Sched:        sched,
		Packages:     reg,
		PollInterval: time.Hour, // we don't want the poller actually scanning here
		ExecCfg: executor.Config{
			Grace:       100 * time.Millisecond,
			IdleTimeout: 5 * time.Second,
		},
	})
	cleanup := func() {
		gctx, gc := context.WithTimeout(context.Background(), 2*time.Second)
		defer gc()
		_ = mgr.CloseAll(gctx)
		_ = sched.Stop(gctx)
	}
	return mgr, sched, cleanup
}

// initProject creates a tmp dir, puts a real .autosk/db inside it (so
// projectdb.Resolve walks up and finds it), and returns the project root.
func initProject(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ProjectRoot")
	dir := filepath.Join(root, ".autosk")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(dir, "db")
	ts := doltlite.New()
	if err := ts.Open(context.Background(), dbPath); err != nil {
		t.Fatalf("doltlite open: %v", err)
	}
	if err := ts.Migrate(context.Background()); err != nil {
		_ = ts.Close()
		t.Fatalf("migrate: %v", err)
	}
	if err := ts.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return root
}

func TestResolve_OpensProjectLazily(t *testing.T) {
	mgr, _, cleanup := newManagerWithSched(t)
	defer cleanup()
	root := initProject(t)

	proj, err := mgr.Resolve(context.Background(), root, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if proj.Root != root {
		// On macOS the temp dir lives under /var, which is a symlink to
		// /private/var. EvalSymlinks resolves it. Both spellings must
		// reference the same project.
		canon, _ := filepath.EvalSymlinks(root)
		if proj.Root != canon {
			t.Fatalf("Project.Root = %q, want %q (or canonical %q)", proj.Root, root, canon)
		}
	}
	if proj.DBPath == "" {
		t.Fatal("Project.DBPath is empty")
	}
	if _, err := os.Stat(proj.DBPath); err != nil {
		t.Fatalf("DBPath stat: %v", err)
	}
	if loaded := mgr.Loaded(); len(loaded) != 1 {
		t.Fatalf("Loaded() = %d, want 1", len(loaded))
	}
}

func TestResolve_IsIdempotentUnderRace(t *testing.T) {
	mgr, _, cleanup := newManagerWithSched(t)
	defer cleanup()
	root := initProject(t)

	const n = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []*projectmgr.Project
	)
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			p, err := mgr.Resolve(context.Background(), root, "")
			if err != nil {
				t.Errorf("Resolve: %v", err)
				return
			}
			mu.Lock()
			results = append(results, p)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	if len(results) != n {
		t.Fatalf("got %d results, want %d", len(results), n)
	}
	first := results[0]
	for i, p := range results {
		if p != first {
			t.Fatalf("resolve #%d returned a different *Project (%p vs %p)", i, p, first)
		}
	}
}

func TestResolve_InvalidCwd(t *testing.T) {
	mgr, _, cleanup := newManagerWithSched(t)
	defer cleanup()
	_, err := mgr.Resolve(context.Background(), "relative/path", "")
	if !errors.Is(err, projectmgr.ErrInvalidCwd) {
		t.Fatalf("expected ErrInvalidCwd, got %v", err)
	}
	_, err = mgr.Resolve(context.Background(), "", "")
	if !errors.Is(err, projectmgr.ErrInvalidCwd) {
		t.Fatalf("expected ErrInvalidCwd for empty cwd, got %v", err)
	}
}

func TestResolve_ProjectNotFound(t *testing.T) {
	mgr, _, cleanup := newManagerWithSched(t)
	defer cleanup()
	// AUTOSK_NO_AUTOINIT is irrelevant: projectmgr never auto-inits.
	dir := t.TempDir() // no .autosk inside
	_, err := mgr.Resolve(context.Background(), dir, "")
	if !errors.Is(err, projectmgr.ErrProjectNotFound) {
		t.Fatalf("expected ErrProjectNotFound, got %v", err)
	}
}

func TestResolve_CanonicalisesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks not portable on Windows test envs")
	}
	mgr, _, cleanup := newManagerWithSched(t)
	defer cleanup()
	real := initProject(t)
	// Create a sibling symlink pointing at the real project root.
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	p1, err := mgr.Resolve(context.Background(), real, "")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := mgr.Resolve(context.Background(), link, "")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("expected same *Project, got %p and %p (roots %q vs %q)", p1, p2, p1.Root, p2.Root)
	}
}

func TestResolve_DedupesCaseInsensitiveAliases(t *testing.T) {
	mgr, _, cleanup := newManagerWithSched(t)
	defer cleanup()
	root := initProject(t)
	alias := caseAlias(t, root)

	p1, err := mgr.Resolve(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := mgr.Resolve(context.Background(), alias, "")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("expected case aliases to resolve same *Project, got %p and %p (roots %q vs %q)", p1, p2, p1.Root, p2.Root)
	}
	if loaded := mgr.Loaded(); len(loaded) != 1 {
		t.Fatalf("Loaded() = %d, want 1", len(loaded))
	}
}

func caseAlias(t *testing.T, path string) string {
	t.Helper()
	base := filepath.Base(path)
	aliasBase := toggleCase(base)
	if aliasBase == base {
		t.Fatalf("test setup needs alphabetic project root basename, got %q", base)
	}
	alias := filepath.Join(filepath.Dir(path), aliasBase)
	origInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat original path: %v", err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(origInfo, aliasInfo) {
		t.Skipf("filesystem is case-sensitive for %q", path)
	}
	return alias
}

func toggleCase(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case 'a' <= r && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case 'A' <= r && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestResolve_RestartRecoverySweepsOnFirstOpen(t *testing.T) {
	mgr, _, cleanup := newManagerWithSched(t)
	defer cleanup()
	root := initProject(t)

	// Seed a 'running' daemon_runs row by hand so the sweep has
	// something to rewrite.
	ts := doltlite.New()
	if err := ts.Open(context.Background(), filepath.Join(root, ".autosk", "db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ts.Close()
	if err := ts.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Need a task + workflow + step for the FK.
	tk, err := ts.CreateTask(context.Background(), store.Task{Title: "x", Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	db := ts.DB()
	var humanID string
	if err := db.QueryRowContext(context.Background(), `SELECT id FROM agents WHERE name='human'`).Scan(&humanID); err != nil {
		t.Fatalf("select human: %v", err)
	}
	now := time.Now().Unix()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at)
		 VALUES ('wf-r','wf-r','','st-r',0,?)`, now); err != nil {
		t.Fatalf("insert wf: %v", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES ('st-r','wf-r','do',?,0)`, humanID); err != nil {
		t.Fatalf("insert step: %v", err)
	}
	rs := runstore.New(db)
	run, err := rs.CreateRun(context.Background(), runstore.NewRun{TaskID: tk.ID, StepID: "st-r"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := rs.MarkRunning(context.Background(), run.JobID, 42); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := ts.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// First Resolve must run the sweep.
	proj, err := mgr.Resolve(context.Background(), root, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := proj.Runs.GetRun(context.Background(), run.JobID)
	if err != nil {
		t.Fatalf("GetRun after sweep: %v", err)
	}
	if got.Status != runstore.StatusFailed || got.Error != "daemon_restart" {
		t.Fatalf("expected sweep-to-failed/daemon_restart, got %+v", got)
	}

	// Plant another running row and call Resolve a second time. The
	// sweep ran on the FIRST open and only on the first — so this row
	// stays as-is.
	run2, err := proj.Runs.CreateRun(context.Background(), runstore.NewRun{TaskID: tk.ID, StepID: "st-r"})
	if err != nil {
		t.Fatalf("CreateRun #2: %v", err)
	}
	if _, err := proj.Runs.MarkRunning(context.Background(), run2.JobID, 43); err != nil {
		t.Fatalf("MarkRunning #2: %v", err)
	}
	proj2, err := mgr.Resolve(context.Background(), root, "")
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if proj2 != proj {
		t.Fatalf("expected idempotent Resolve to return the same *Project")
	}
	got2, _ := proj.Runs.GetRun(context.Background(), run2.JobID)
	if got2.Status != runstore.StatusRunning {
		t.Fatalf("expected second running row untouched, got %+v", got2)
	}
}

// TestResolve_DBOverrideMissingFileReturnsNotFound asserts that when
// X-Autosk-DB points at a path that does not exist on disk, Resolve
// returns ErrProjectNotFound rather than silently auto-creating a
// fresh empty database. Per the daemon-must-not-create-on-open
// contract (docs/plans/20260518-Daemon-UDS-Plan.md §4.1).
func TestResolve_DBOverrideMissingFileReturnsNotFound(t *testing.T) {
	mgr, _, cleanup := newManagerWithSched(t)
	defer cleanup()

	// cwd just needs to be an absolute existing dir; the override is
	// what matters here.
	cwd := t.TempDir()
	// Point dbOverride at a deliberately-missing path under a fresh
	// directory. If the bug is back, doltlite will create this file
	// on open.
	missingRoot := t.TempDir()
	missingDB := filepath.Join(missingRoot, ".autosk", "db")
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("precondition: %s should not exist (got err=%v)", missingDB, err)
	}

	_, err := mgr.Resolve(context.Background(), cwd, missingDB)
	if !errors.Is(err, projectmgr.ErrProjectNotFound) {
		t.Fatalf("expected ErrProjectNotFound, got %v", err)
	}

	// Crucially: no file must have been created as a side effect.
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Resolve created %s as a side effect (db must not auto-create)", missingDB)
	}
}

// TestResolve_FirstPollAfterOpenIsDispatched is the regression test for
// the open-vs-poll race: when a project has a work task waiting
// at first Resolve and the poll interval is short, the scheduler closure
// must observe the project as loaded (i.e. mgr.Get must return non-nil
// for the job's project key) so the job is dispatched rather than
// orphaned in 'queued'.
//
// We instrument the scheduler executor closure to record which jobs it
// sees. If the race re-appears the closure either won't be called at
// all (job dropped before dispatch) or will see Get() return false.
func TestResolve_FirstPollAfterOpenIsDispatched(t *testing.T) {
	t.Helper()
	regDir := t.TempDir()
	reg, err := pkgregistry.Open(filepath.Join(regDir, "packages"), pkgregistry.WithNpm(fakeNpm{}))
	if err != nil {
		t.Fatalf("pkgregistry.Open: %v", err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}

	// Pre-create a project with a work task pointed at a
	// non-human step so the poller has something to enqueue
	// immediately.
	root := initProject(t)
	ts := doltlite.New()
	if err := ts.Open(context.Background(), filepath.Join(root, ".autosk", "db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ts.Migrate(context.Background()); err != nil {
		_ = ts.Close()
		t.Fatalf("migrate: %v", err)
	}
	db := ts.DB()
	// Use the built-in human agent's bookkeeping as a starting point,
	// then create a fake non-human agent so a_is_human=0.
	now := time.Now().Unix()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO agents(id, name, is_human, created_at) VALUES ('ag-race','race',0,?)`, now); err != nil {
		_ = ts.Close()
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at)
		 VALUES ('wf-race','wf-race','','st-race',0,?)`, now); err != nil {
		_ = ts.Close()
		t.Fatalf("insert wf: %v", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES ('st-race','wf-race','run','ag-race',0)`); err != nil {
		_ = ts.Close()
		t.Fatalf("insert step: %v", err)
	}
	tk, err := ts.CreateTask(context.Background(), store.Task{
		Title:    "race-me",
		Status:   store.StatusNew,
		Priority: 2,
	})
	if err != nil {
		_ = ts.Close()
		t.Fatalf("create task: %v", err)
	}
	// Attach the workflow + step and flip the status to work.
	if _, err := db.ExecContext(context.Background(),
		`UPDATE tasks SET workflow_id='wf-race', current_step_id='st-race', status='work' WHERE id=?`, tk.ID); err != nil {
		_ = ts.Close()
		t.Fatalf("attach workflow: %v", err)
	}
	if err := ts.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Spy executor: records every dispatched (project, jobID) tuple
	// and verifies mgr.Get returns a usable project.
	var (
		dispatched atomic.Int32
		mgrPtr     atomic.Pointer[projectmgr.Manager]
	)
	exec := scheduler.ExecutorFunc(func(_ context.Context, job scheduler.Job) error {
		m := mgrPtr.Load()
		if m == nil {
			t.Errorf("scheduler closure fired before manager was wired")
			return nil
		}
		proj, ok := m.Get(projectmgr.Key(job.Project))
		if !ok || proj == nil {
			t.Errorf("scheduler closure: mgr.Get(%q) returned ok=%v, proj=%v (open-vs-poll race regressed)",
				job.Project, ok, proj)
			return nil
		}
		dispatched.Add(1)
		// Mark the run done so the poller's NOT EXISTS clause stops
		// surfacing the task on subsequent ticks.
		_, _ = proj.Runs.MarkDone(context.Background(), job.ID, 0, nil)
		return nil
	})
	sched := scheduler.New(exec, scheduler.Config{Workers: 1})
	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("sched start: %v", err)
	}
	defer func() {
		gctx, gc := context.WithTimeout(context.Background(), 2*time.Second)
		defer gc()
		_ = sched.Stop(gctx)
	}()

	mgr := projectmgr.New(projectmgr.Deps{
		Sched:        sched,
		Packages:     reg,
		PollInterval: 50 * time.Millisecond, // short so the first scan fires fast.
		ExecCfg: executor.Config{
			Grace:       100 * time.Millisecond,
			IdleTimeout: 5 * time.Second,
		},
	})
	mgrPtr.Store(mgr)
	defer func() {
		gctx, gc := context.WithTimeout(context.Background(), 2*time.Second)
		defer gc()
		_ = mgr.CloseAll(gctx)
	}()

	if _, err := mgr.Resolve(context.Background(), root, ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Wait up to 3s for the closure to fire. Without the fix the
	// closure either runs and finds mgr.Get returns false (the t.Errorf
	// above fires) or never runs because the job was dropped pre-
	// dispatch (dispatched stays 0).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && dispatched.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if dispatched.Load() == 0 {
		t.Fatalf("scheduler closure was never invoked; first-scan job appears orphaned")
	}
}

// TestManager_StopPollersThenCloseDBsAreSeparable: the daemon's shutdown
// order relies on being able to stop pollers (so they stop inserting
// new daemon_runs rows) without yet closing the underlying *sql.DB.
// Workers in flight must remain able to call MarkCancelled / MarkFailed
// against an open DB. This test exercises that contract.
func TestManager_StopPollersThenCloseDBsAreSeparable(t *testing.T) {
	mgr, _, cleanup := newManagerWithSched(t)
	defer cleanup() // safe no-op if we already closed.

	root := initProject(t)
	proj, err := mgr.Resolve(context.Background(), root, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	gctx, gc := context.WithTimeout(context.Background(), 2*time.Second)
	defer gc()
	if err := mgr.StopPollers(gctx); err != nil {
		t.Fatalf("StopPollers: %v", err)
	}
	// At this point the DB must still be usable: writes/reads succeed.
	// We simulate the in-flight-worker case by issuing a write through
	// the per-project runstore. If StopPollers had closed the DB, this
	// would fail with "sql: database is closed".
	if _, err := proj.Tasks.DB().ExecContext(context.Background(),
		`SELECT 1`); err != nil {
		t.Fatalf("DB unusable after StopPollers (expected open): %v", err)
	}
	// Snapshot the DB handle before close (Tasks.DB() returns nil
	// after the store is closed in some impls).
	dbHandle := proj.Tasks.DB()
	if err := mgr.CloseDBs(gctx); err != nil {
		t.Fatalf("CloseDBs: %v", err)
	}
	// After CloseDBs queries via the snapshotted handle must fail with
	// "sql: database is closed".
	if _, err := dbHandle.ExecContext(context.Background(), `SELECT 1`); err == nil {
		t.Fatalf("DB still usable after CloseDBs (expected closed)")
	}
	// CloseDBs must be idempotent.
	if err := mgr.CloseDBs(gctx); err != nil {
		t.Fatalf("CloseDBs (second call): %v", err)
	}
	// StopPollers must be idempotent.
	if err := mgr.StopPollers(gctx); err != nil {
		t.Fatalf("StopPollers (second call): %v", err)
	}
}
