package poller_test

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/agent"
	"autosk/internal/daemon/poller"
	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/scheduler"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

type pollFixture struct {
	ts    *doltlite.Store
	runs  *runstore.Store
	wfs   *workflow.Store
	wf    workflow.Workflow
	close func()
}

func newPollFixture(t *testing.T) *pollFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		_ = ts.Close()
		t.Fatalf("Migrate: %v", err)
	}
	ag := agent.New(ts.DB())
	for _, name := range []string{"developer", "code-reviewer", "task-validator"} {
		if _, err := ag.Create(ctx, name, false); err != nil {
			_ = ts.Close()
			t.Fatalf("agent: %v", err)
		}
	}
	wfs := workflow.New(ts.DB(), ag)
	def, err := workflow.ParseFile("../../../docs/examples/workflows/workflow-example.json")
	if err != nil {
		_ = ts.Close()
		t.Fatalf("ParseFile: %v", err)
	}
	wf, err := wfs.Create(ctx, def, false)
	if err != nil {
		_ = ts.Close()
		t.Fatalf("workflow Create: %v", err)
	}
	return &pollFixture{
		ts:    ts,
		runs:  runstore.New(ts.DB()),
		wfs:   wfs,
		wf:    wf,
		close: func() { _ = ts.Close() },
	}
}

func (fx *pollFixture) makeTask(t *testing.T, title, stepName string) string {
	t.Helper()
	var stepID string
	for _, s := range fx.wf.Steps {
		if s.Name == stepName {
			stepID = s.ID
			break
		}
	}
	if stepID == "" {
		t.Fatalf("step %q missing", stepName)
	}
	tk, err := fx.ts.CreateTask(context.Background(), store.Task{
		Title:         title,
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    fx.wf.ID,
		CurrentStepID: stepID,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return tk.ID
}

// TestPoller_EnqueuesNonHumanTask creates one work task whose
// current step's agent is the (non-human) developer. After Start, a
// daemon_runs row appears within ~3s.
func TestPoller_EnqueuesNonHumanTask(t *testing.T) {
	fx := newPollFixture(t)
	defer fx.close()
	ctx := context.Background()
	taskID := fx.makeTask(t, "Auto-picked", "dev")

	enqueued := make(chan string, 4)
	var seen atomic.Int32
	exec := scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error {
		seen.Add(1)
		enqueued <- job.ID
		if job.Project != "proj-test" {
			t.Errorf("expected project key 'proj-test', got %q", job.Project)
		}
		// Stay running long enough that a second tick observes the row as
		// active (so dedupe is exercised in TestPoller_Dedupes).
		time.Sleep(2 * time.Second)
		_, _ = fx.runs.MarkDone(ctx, job.ID, 0, nil)
		return nil
	})
	sched := scheduler.New(exec, scheduler.Config{Workers: 1})
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gctx, gc := context.WithTimeout(context.Background(), 5*time.Second)
		defer gc()
		_ = sched.Stop(gctx)
	})

	p := poller.New(fx.ts.DB(), fx.runs, sched, poller.Config{Interval: 150 * time.Millisecond, ProjectKey: "proj-test"})
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		gctx, gc := context.WithTimeout(context.Background(), 2*time.Second)
		defer gc()
		_ = p.Stop(gctx)
	}()

	select {
	case <-enqueued:
		// good
	case <-time.After(3 * time.Second):
		t.Fatal("poller did not enqueue the task within 3s")
	}

	// The daemon_runs row should reference our taskID.
	rs, err := fx.runs.ListRuns(ctx, runstore.RunFilter{TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("expected 1 run row, got %d", len(rs))
	}
}

// TestPoller_Dedupes verifies the WHERE NOT EXISTS clause: a second scan
// doesn't enqueue a second row while the first is still queued/running.
func TestPoller_Dedupes(t *testing.T) {
	fx := newPollFixture(t)
	defer fx.close()
	ctx := context.Background()
	taskID := fx.makeTask(t, "No double-tap", "dev")

	// Long-running executor so the row stays in 'running' across ticks.
	exec := scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error {
		_, _ = fx.runs.MarkRunning(ctx, job.ID, 0)
		<-ctx.Done()
		// terminal so cleanup is graceful
		_, _ = fx.runs.MarkCancelled(context.Background(), job.ID, nil)
		return ctx.Err()
	})
	sched := scheduler.New(exec, scheduler.Config{Workers: 1})
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gctx, gc := context.WithTimeout(context.Background(), 5*time.Second)
		defer gc()
		_ = sched.Stop(gctx)
	})

	p := poller.New(fx.ts.DB(), fx.runs, sched, poller.Config{Interval: 60 * time.Millisecond, ProjectKey: "proj-test"})
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		gctx, gc := context.WithTimeout(context.Background(), 2*time.Second)
		defer gc()
		_ = p.Stop(gctx)
	}()

	// Wait long enough for several ticks.
	time.Sleep(500 * time.Millisecond)
	rs, err := fx.runs.ListRuns(ctx, runstore.RunFilter{TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("expected exactly 1 row (dedupe), got %d", len(rs))
	}
}

// TestPoller_SkipsBlockedWorkTaskUntilBlockerTerminal verifies that a task
// already enrolled into work is not enqueued until all blockers are terminal.
func TestPoller_SkipsBlockedWorkTaskUntilBlockerTerminal(t *testing.T) {
	cases := []struct {
		name        string
		status      store.Status
		wantBlocked bool
		wantPicked  bool
	}{
		{name: "new", status: store.StatusNew, wantBlocked: true},
		{name: "work", status: store.StatusWork, wantBlocked: true},
		{name: "human", status: store.StatusHuman, wantBlocked: true},
		{name: "done", status: store.StatusDone, wantPicked: true},
		{name: "cancel", status: store.StatusCancel, wantPicked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newPollFixture(t)
			defer fx.close()
			ctx := context.Background()

			targetID := fx.makeTask(t, "Enrolled but blocked", "dev")
			var blockerID string
			if tc.status == store.StatusWork {
				blockerID = fx.makeTask(t, "Work blocker", "dev")
			} else {
				blocker, err := fx.ts.CreateTask(ctx, store.Task{
					Title:    "Blocker " + tc.name,
					Status:   tc.status,
					Priority: 2,
				})
				if err != nil {
					t.Fatalf("CreateTask blocker: %v", err)
				}
				blockerID = blocker.ID
			}
			if err := fx.ts.Block(ctx, targetID, blockerID); err != nil {
				t.Fatalf("Block: %v", err)
			}

			blocked, err := fx.ts.IsBlocked(ctx, targetID)
			if err != nil {
				t.Fatalf("IsBlocked: %v", err)
			}
			if blocked != tc.wantBlocked {
				t.Fatalf("IsBlocked = %v, want %v", blocked, tc.wantBlocked)
			}

			p := poller.New(fx.ts.DB(), fx.runs, nil, poller.Config{ProjectKey: "proj-test"})
			cands, err := p.Scan(ctx)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			picked := false
			for _, c := range cands {
				if c.TaskID == targetID {
					picked = true
				}
			}
			if picked != tc.wantPicked {
				t.Fatalf("target picked = %v, want %v (candidates=%v)", picked, tc.wantPicked, cands)
			}
		})
	}
}

// TestPoller_SkipsHumanAgent: a task whose current step's agent is human
// must NOT be enqueued.
func TestPoller_SkipsHumanAgent(t *testing.T) {
	fx := newPollFixture(t)
	defer fx.close()
	ctx := context.Background()

	// Build a synthetic single:human workflow and point a task at it.
	syn, err := fx.wfs.EnsureSingle(ctx, "human")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := fx.ts.CreateTask(ctx, store.Task{
		Title:         "Mine",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    syn.ID,
		CurrentStepID: syn.Steps[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	exec := scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error {
		t.Errorf("scheduler should not have received job for human task; got %s", job.ID)
		return nil
	})
	sched := scheduler.New(exec, scheduler.Config{Workers: 1})
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gctx, gc := context.WithTimeout(context.Background(), 2*time.Second)
		defer gc()
		_ = sched.Stop(gctx)
	})

	p := poller.New(fx.ts.DB(), fx.runs, sched, poller.Config{Interval: 60 * time.Millisecond, ProjectKey: "proj-test"})
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		gctx, gc := context.WithTimeout(context.Background(), 2*time.Second)
		defer gc()
		_ = p.Stop(gctx)
	}()

	time.Sleep(400 * time.Millisecond)
	rs, err := fx.runs.ListRuns(ctx, runstore.RunFilter{TaskID: tk.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 0 {
		t.Fatalf("expected no runs for human-owned task, got %d", len(rs))
	}
}
