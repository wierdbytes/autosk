package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
)

// fakeDS is a minimal Datasource stub used to drive refreshAll's
// fetch/apply path without spinning up a real DB. Only the verbs
// fetchRefresh calls need real implementations; the rest return
// ErrDaemonRequired or zero values.
type refreshFakeDS struct {
	tasks        []datasource.Task
	tasksErr     error
	jobs         []datasource.Job
	jobsErr      error
	workflows    []datasource.Workflow
	workflowsErr error
	agents       []datasource.Agent
	agentsErr    error
	health       datasource.Health
	comments     []datasource.Comment
	commentsErr  error
	signals      []datasource.Signal
	signalsErr   error
}

func (f *refreshFakeDS) Tasks(_ context.Context, _ datasource.TaskFilter) ([]datasource.Task, error) {
	return f.tasks, f.tasksErr
}
func (f *refreshFakeDS) GetTask(_ context.Context, _ string) (datasource.Task, error) {
	return datasource.Task{}, nil
}
func (f *refreshFakeDS) Jobs(_ context.Context, _ datasource.JobFilter) ([]datasource.Job, error) {
	return f.jobs, f.jobsErr
}
func (f *refreshFakeDS) GetJob(_ context.Context, _ string) (datasource.Job, error) {
	return datasource.Job{}, nil
}
func (f *refreshFakeDS) Workflows(_ context.Context, _ bool) ([]datasource.Workflow, error) {
	return f.workflows, f.workflowsErr
}
func (f *refreshFakeDS) Agents(_ context.Context) ([]datasource.Agent, error) {
	return f.agents, f.agentsErr
}
func (f *refreshFakeDS) Comments(_ context.Context, _ string) ([]datasource.Comment, error) {
	return f.comments, f.commentsErr
}
func (f *refreshFakeDS) Signals(_ context.Context, _ string) ([]datasource.Signal, error) {
	return nil, nil
}
func (f *refreshFakeDS) SignalsForTask(_ context.Context, _ string) ([]datasource.Signal, error) {
	return f.signals, f.signalsErr
}
func (f *refreshFakeDS) Messages(_ context.Context, _ string, _ bool, _ int) ([]datasource.MessageEvent, error) {
	return nil, nil
}
func (f *refreshFakeDS) Healthz(_ context.Context) (datasource.Health, error) {
	return f.health, nil
}
func (f *refreshFakeDS) Reconnect(_ context.Context) error { return nil }
func (f *refreshFakeDS) CreateTask(_ context.Context, _, _ string, _ int) (string, error) {
	return "", datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) UpdateStatus(_ context.Context, _ string, _ store.Status) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) UpdatePriority(_ context.Context, _ string, _ int) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) UpdateTitleDescription(_ context.Context, _, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) Enroll(_ context.Context, _, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) Resume(_ context.Context, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) Block(_ context.Context, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) Unblock(_ context.Context, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) AddComment(_ context.Context, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) SetMetadata(_ context.Context, _ string, _ map[string]any) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) CreateWorkflow(_ context.Context, _ string) (string, error) {
	return "", datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) DeleteWorkflow(_ context.Context, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) UpdateWorkflowIsolation(_ context.Context, _, _ string, _ bool) (datasource.UpdateIsolationReport, error) {
	return datasource.UpdateIsolationReport{}, datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) InstallAgent(_ context.Context, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) UninstallAgent(_ context.Context, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) CancelJob(_ context.Context, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) SendInput(_ context.Context, _, _, _ string) (string, error) {
	return "", datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) AbortJob(_ context.Context, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) StreamLive(_ context.Context, _ string) (*datasource.LiveHandle, error) {
	return nil, datasource.ErrDaemonRequired
}

// TestRefreshApply_CommentsErrorPreservesCache pins the regression
// the previous review flagged: when ds.Comments fails for the
// currently-selected task, refreshAll must NOT overwrite the existing
// cache entry — stale-but-shown is better than empty-and-flickering.
//
// We drive fetchRefresh + applyRefreshLocked directly (no gocui
// needed) and exercise both branches: success populates the cache,
// error preserves the prior cache entry.
func (*refreshFakeDS) SyncWorkflows(_ context.Context, _, _ bool) (datasource.SyncReport, error) { return datasource.SyncReport{}, nil }
func TestRefreshApply_CommentsErrorPreservesCache(t *testing.T) {
	gu := &Gui{st: newState()}
	// Seed the task list so selectedTask() returns something.
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa", Title: "x"}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks
	// Pre-seed the cache with a "stale" value.
	gu.st.comments["ask-aaaaaa"] = []datasource.Comment{{Text: "stale-cached"}}

	// First refresh: success path replaces the stale value.
	fakeOK := &refreshFakeDS{
		tasks:    []datasource.Task{{ID: "ask-aaaaaa", Title: "x"}},
		comments: []datasource.Comment{{Text: "fresh"}},
	}
	gu.ds = fakeOK
	r := gu.fetchRefresh(context.Background())
	if r.hasCommentsErr {
		t.Fatalf("ok path produced hasCommentsErr")
	}
	gu.applyRefreshLocked(r)
	if got := gu.st.comments["ask-aaaaaa"]; len(got) != 1 || got[0].Text != "fresh" {
		t.Fatalf("ok path didn't update cache: %+v", got)
	}

	// Second refresh: Comments() errors. The cache MUST stay at "fresh"
	// (not get cleared, not be overwritten with nil).
	wantErr := errors.New("db lock")
	fakeErr := &refreshFakeDS{
		tasks:       []datasource.Task{{ID: "ask-aaaaaa", Title: "x"}},
		commentsErr: wantErr,
	}
	gu.ds = fakeErr

	// Also pin dlog firing on error.
	var dlogBuf strings.Builder
	SetDebugLogger(func(format string, args ...any) {
		dlogBuf.WriteString(format)
	})
	defer SetDebugLogger(nil)

	r2 := gu.fetchRefresh(context.Background())
	if !r2.hasCommentsErr {
		t.Fatalf("err path didn't set hasCommentsErr (err=%v)", wantErr)
	}
	if r2.selectedComments != nil {
		t.Fatalf("err path leaked stale comments into result: %+v", r2.selectedComments)
	}
	gu.applyRefreshLocked(r2)
	if got := gu.st.comments["ask-aaaaaa"]; len(got) != 1 || got[0].Text != "fresh" {
		t.Fatalf("err path overwrote cache: %+v (want %q)", got, "fresh")
	}
	if !strings.Contains(dlogBuf.String(), "ds.Comments") {
		t.Fatalf("dlog did not fire on comments error; buf=%q", dlogBuf.String())
	}
}

// TestRefreshApply_SignalsErrorPreservesCache mirrors the comments
// test for the signals cache.
func TestRefreshApply_SignalsErrorPreservesCache(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.tasks = []datasource.Task{{ID: "ask-bbbbbb", Title: "y"}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks
	gu.st.signals["ask-bbbbbb"] = []datasource.Signal{{StepName: "stale"}}

	fakeErr := &refreshFakeDS{
		tasks:      []datasource.Task{{ID: "ask-bbbbbb", Title: "y"}},
		signalsErr: errors.New("boom"),
	}
	gu.ds = fakeErr
	r := gu.fetchRefresh(context.Background())
	if !r.hasSignalsErr {
		t.Fatalf("expected hasSignalsErr")
	}
	gu.applyRefreshLocked(r)
	if got := gu.st.signals["ask-bbbbbb"]; len(got) != 1 || got[0].StepName != "stale" {
		t.Fatalf("err path overwrote signals cache: %+v", got)
	}
}

// TestRefreshApply_FallbacksCounterAdvances pins the fallback chip
// machinery: a Datasource that implements Fallbacks() must drive the
// state.fallbacksNow / fallbacksLast pair so the status bar can
// detect delta > 0 between ticks.
func TestRefreshApply_FallbacksCounterAdvances(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &fallbackFakeDS{
		refreshFakeDS: refreshFakeDS{},
		count:         5,
	}
	r := gu.fetchRefresh(context.Background())
	if r.fallbacksNow != 5 {
		t.Fatalf("fallbacksNow=%d want 5", r.fallbacksNow)
	}
	gu.applyRefreshLocked(r)
	if gu.st.fallbacksNow != 5 {
		t.Fatalf("applied fallbacksNow=%d want 5", gu.st.fallbacksNow)
	}
	// Second tick at 8 → delta = 3.
	gu.ds.(*fallbackFakeDS).count = 8
	r2 := gu.fetchRefresh(context.Background())
	gu.applyRefreshLocked(r2)
	if delta := gu.st.fallbacksNow - gu.st.fallbacksLast; delta != 3 {
		t.Fatalf("delta=%d want 3 (now=%d last=%d)", delta, gu.st.fallbacksNow, gu.st.fallbacksLast)
	}
}

// fallbackFakeDS wraps refreshFakeDS with a Fallbacks() method so
// fetchRefresh's interface assertion picks it up.
type fallbackFakeDS struct {
	refreshFakeDS
	count uint64
}

func (f *fallbackFakeDS) Fallbacks() uint64 { return f.count }

// TestRefreshApply_RunningToTerminalInvalidatesArchive pins the
// hand-wired transition path in applyRefreshLocked: when the
// currently-selected job's status flips from running to terminal
// between refresh snapshots, the existing transcript cache entry
// must have its loadedAt zeroed so the next selection-driven
// hydration (or the queued OnWorker dispatch) re-fetches the
// archive.
func (*fallbackFakeDS) SyncWorkflows(_ context.Context, _, _ bool) (datasource.SyncReport, error) { return datasource.SyncReport{}, nil }
func TestRefreshApply_RunningToTerminalInvalidatesArchive(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}
	const jobID = "job-trans-XYZ"

	// Seed: jobs slice has one running job, cursor on it, cache
	// entry exists with a non-zero loadedAt.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: jobID, Status: "running", Streaming: true},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
		te := gu.ensureTranscriptEntryLocked(jobID)
		te.loadedAt = time.Now().Add(-time.Second) // already loaded
	})

	// Drive applyRefreshLocked with a refreshResult that flips the
	// status to terminal.
	r := refreshResult{
		jobs: []datasource.Job{{
			JobResponse: api.JobResponse{JobID: jobID, Status: "done"},
		}},
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	var loadedAt time.Time
	gu.st.withRLock(func() {
		te := gu.st.jobTranscript[jobID]
		if te != nil {
			loadedAt = te.loadedAt
		}
	})
	if !loadedAt.IsZero() {
		t.Errorf("running→terminal transition did not zero loadedAt; got %v", loadedAt)
	}
	// jobArchiveStale must report the entry as stale now.
	if !gu.jobArchiveStale(jobID, false) {
		t.Errorf("entry should report stale after loadedAt zeroing")
	}
}

// TestRefreshApply_RunningToTerminal_RevertsFocusFromJobInput
// pins the second-half of the running→terminal transition: if the
// operator was typing in winJobInput when the job finished
// (state.focused == panelJobInput), the layout pass that follows
// will delete the input view because showJobInput now reads false.
// applyRefreshLocked must therefore also revert focused to
// panelJobs so SetCurrentView lands the caret on a view that
// still exists, instead of leaving it in phantom-focus on a
// deleted view.
func TestRefreshApply_RunningToTerminal_RevertsFocusFromJobInput(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}
	const jobID = "job-typing"

	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: jobID, Status: "running", Streaming: true},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobInput // operator is typing
	})

	r := refreshResult{
		jobs: []datasource.Job{{
			JobResponse: api.JobResponse{JobID: jobID, Status: "done"},
		}},
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	var focused panelID
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelJobs {
		t.Errorf("focused after running→terminal = %v, want panelJobs (input view goes away; revert)", focused)
	}
}

// TestRefreshApply_RunningToTerminal_PreservesNonInputFocus: the
// focus-revert is gated on focused == panelJobInput specifically.
// If the operator wasn't in the input (e.g. focused == panelJobs
// or panelTasks) the running→terminal transition must NOT spuriously
// alter focus — only the actual phantom-focus case needs the
// revert.
func TestRefreshApply_RunningToTerminal_PreservesNonInputFocus(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}
	const jobID = "job-typing"

	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: jobID, Status: "running", Streaming: true},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelTasks // not in the input
	})

	r := refreshResult{
		jobs: []datasource.Job{{
			JobResponse: api.JobResponse{JobID: jobID, Status: "done"},
		}},
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	var focused panelID
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelTasks {
		t.Errorf("focused = %v after running→terminal, want panelTasks (unrelated; should not change)", focused)
	}
}

// TestRefreshApply_SelectedJobVanishes_RevertsFocusFromJobInput
// pins the phantom-focus guard: when the currently-selected running
// job disappears from the jobs slice (filter / scope removed it)
// and the remaining job at the cursor is terminal, the caret must
// revert to panelJobs so the next layout doesn't leave it on a
// deleted view.
func TestRefreshApply_SelectedJobVanishes_RevertsFocusFromJobInput(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}

	// Seed: cursor on job-A (running), focus in the input.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobInput
	})

	// Refresh removes job-A entirely; only a terminal job-Z remains.
	// ID-based restore won't find job-A, so the cursor falls back to
	// index 0 (job-Z, terminal). The winJobInput view must go away.
	r := refreshResult{
		jobs: []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-Z", Status: "done"}},
		},
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	var focused panelID
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelJobs {
		t.Errorf("focused after selected-job-vanishes = %v, want panelJobs (input view goes away; revert)", focused)
	}
}

// TestRefreshApply_ReshuffleKeepsCursorOnSameJob pins the ID-based
// cursor restore: a refresh-driven reshuffle that re-orders the jobs
// slice must keep the cursor anchored to the previously-selected
// job, not to its old positional index.
func TestRefreshApply_ReshuffleKeepsCursorOnSameJob(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}

	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true}},
		}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobInput
	})

	// Reshuffle: job-Z lands at index 0, job-A moves to index 1.
	r := refreshResult{
		jobs: []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-Z", Status: "running", Streaming: true}},
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true}},
		},
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	var cursor int
	var focused panelID
	gu.st.withRLock(func() {
		cursor = gu.st.jobCursor
		focused = gu.st.focused
	})
	if cursor != 1 {
		t.Errorf("jobCursor after reshuffle = %d, want 1 (job-A)", cursor)
	}
	if focused != panelJobInput {
		t.Errorf("focused after reshuffle = %v, want panelJobInput (job-A still live)", focused)
	}
}

// TestRefreshApply_JobsEmptied_RevertsFocusFromJobInput covers the
// edge case where a filter removes EVERY job between snapshots
// while the operator was in the input. The selected job becomes
// absent (selectedJob returns ok=false), but the focused-input
// state still needs the revert so the next layout doesn't leave
// the caret on a deleted view.
func TestRefreshApply_JobsEmptied_RevertsFocusFromJobInput(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}

	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobInput
	})

	r := refreshResult{
		jobs:       []datasource.Job{}, // every job filtered out
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	var focused panelID
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelJobs {
		t.Errorf("focused after jobs-emptied = %v, want panelJobs", focused)
	}
}

// TestRefreshApply_NoTransitionPreservesArchive is the inverse:
// when the selected job's status DOESN'T change (still running
// across two snapshots, or still terminal), the cache entry's
// loadedAt must NOT be zeroed.
func TestRefreshApply_NoTransitionPreservesArchive(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}
	const jobID = "job-stable"
	loadedAt := time.Now().Add(-time.Second)

	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: jobID, Status: "running", Streaming: true},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
		te := gu.ensureTranscriptEntryLocked(jobID)
		te.loadedAt = loadedAt
	})

	r := refreshResult{
		jobs: []datasource.Job{{
			JobResponse: api.JobResponse{JobID: jobID, Status: "running", Streaming: true},
		}},
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	var got time.Time
	gu.st.withRLock(func() {
		te := gu.st.jobTranscript[jobID]
		if te != nil {
			got = te.loadedAt
		}
	})
	if !got.Equal(loadedAt) {
		t.Errorf("unchanged-status refresh clobbered loadedAt; got=%v want=%v", got, loadedAt)
	}
}

// TestRefreshApply_TasksCursorByID pins ID-based cursor restore for
// the Tasks panel: a re-sort or re-filter that changes positional
// indices must keep the selection anchored to the same task.
func TestRefreshApply_TasksCursorByID(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}

	gu.st.withLock(func() {
		gu.st.tasks = []datasource.Task{
			{ID: "ask-aaaaaa", Title: "first", UpdatedAt: time.Now().Add(-time.Hour)},
			{ID: "ask-bbbbbb", Title: "second", UpdatedAt: time.Now()},
		}
		gu.st.taskCursor = 0
	})

	// Refresh re-orders: ask-bbbbbb (newer) lands at index 0.
	r := refreshResult{
		tasks: []datasource.Task{
			{ID: "ask-bbbbbb", Title: "second", UpdatedAt: time.Now()},
			{ID: "ask-aaaaaa", Title: "first", UpdatedAt: time.Now().Add(-time.Hour)},
		},
	}
	gu.applyRefreshLocked(r)

	var cursor int
	var id string
	gu.st.withRLock(func() {
		cursor = gu.st.taskCursor
		if t, ok := gu.st.selectedTask(); ok {
			id = t.ID
		}
	})
	if id != "ask-aaaaaa" {
		t.Errorf("selected task after reshuffle = %q, want ask-aaaaaa", id)
	}
	if cursor != 1 {
		t.Errorf("taskCursor after reshuffle = %d, want 1", cursor)
	}
}

// TestRefreshApply_WorkflowsCursorByID mirrors the tasks test for
// the Workflows panel.
func TestRefreshApply_WorkflowsCursorByID(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}

	gu.st.withLock(func() {
		gu.st.workflows = []datasource.Workflow{
			{ID: "wf-1", Name: "alpha"},
			{ID: "wf-2", Name: "beta"},
		}
		gu.st.workflowCursor = 0
	})

	// Refresh re-orders: beta lands at index 0.
	r := refreshResult{
		visibleWorkflows: []datasource.Workflow{
			{ID: "wf-2", Name: "beta"},
			{ID: "wf-1", Name: "alpha"},
		},
	}
	gu.applyRefreshLocked(r)

	var cursor int
	var id string
	gu.st.withRLock(func() {
		cursor = gu.st.workflowCursor
		if w, ok := gu.st.selectedWorkflow(); ok {
			id = w.ID
		}
	})
	if id != "wf-1" {
		t.Errorf("selected workflow after reshuffle = %q, want wf-1", id)
	}
	if cursor != 1 {
		t.Errorf("workflowCursor after reshuffle = %d, want 1", cursor)
	}
}

// TestRefreshApply_AgentsCursorByKey mirrors the tasks test for
// the Agents panel (keyed by Name).
func TestRefreshApply_AgentsCursorByKey(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}

	gu.st.withLock(func() {
		gu.st.agents = []datasource.Agent{
			{Name: "alice"},
			{Name: "bob"},
		}
		gu.st.agentCursor = 0
	})

	// Refresh re-orders: bob lands at index 0.
	r := refreshResult{
		agents: []datasource.Agent{
			{Name: "bob"},
			{Name: "alice"},
		},
	}
	gu.applyRefreshLocked(r)

	var cursor int
	var name string
	gu.st.withRLock(func() {
		cursor = gu.st.agentCursor
		if a, ok := gu.st.selectedAgent(); ok {
			name = a.Name
		}
	})
	if name != "alice" {
		t.Errorf("selected agent after reshuffle = %q, want alice", name)
	}
	if cursor != 1 {
		t.Errorf("agentCursor after reshuffle = %d, want 1", cursor)
	}
}
