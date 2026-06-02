package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/lazy/datasource"
)

// jobDetailLayoutFixture wires a headless gui with a refreshFakeDS
// so the layout pass has somewhere to read from. A background
// context is attached so handlers that schedule refreshes can
// derive timeouts without panicking on a nil parent.
func jobDetailLayoutFixture(t *testing.T) *Gui {
	t.Helper()
	gu := newHeadlessGui(t, 120, 40)
	gu.ds = &refreshFakeDS{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gu.ctx = ctx
	return gu
}

// seedRunningJob seeds one running job in the model and focuses the
// Jobs panel.
func seedRunningJob(gu *Gui, jobID string) {
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: jobID, Status: "running", Streaming: true},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
	})
}

// seedTerminalJob seeds one done job in the model.
func seedTerminalJob(gu *Gui, jobID string) {
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: jobID, Status: "done"},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
	})
}

// TestJobInput_DoesNotLeakAcrossJobs pins the regression review
// flagged: drafting text against job-A and then moving the cursor
// to job-B must NOT leave job-A's text in winJobInput. The
// jobInputOwner field is the contract — clearJobInputIfStale
// drops the cached buffer when the owner doesn't match the new
// cursor row.
func TestJobInput_DoesNotLeakAcrossJobs(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	// Seed two running jobs.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true}},
			{JobResponse: api.JobResponse{JobID: "job-B", Status: "running", Streaming: true}},
		}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
		// Pretend the user has typed "hello to A" into job-A's input.
		gu.st.jobInput = "hello to A"
		gu.st.jobInputOwner = "job-A"
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (cursor on job-A): %v", err)
	}
	v, err := gu.g.View(winJobInput)
	if err != nil {
		t.Fatalf("winJobInput missing: %v", err)
	}
	if !strings.Contains(v.Buffer(), "hello to A") {
		t.Fatalf("setup: job-A draft missing from view buffer: %q", v.Buffer())
	}

	// Flip the cursor to job-B and call afterCursorMove (what cursorDown
	// would do). The draft must be discarded before the next layout.
	gu.st.withLock(func() { gu.st.jobCursor = 1 })
	gu.afterCursorMove(panelJobs)

	var input, owner string
	gu.st.withRLock(func() {
		input = gu.st.jobInput
		owner = gu.st.jobInputOwner
	})
	if input != "" {
		t.Errorf("state.jobInput not cleared on cursor move; got %q", input)
	}
	if owner != "" {
		t.Errorf("state.jobInputOwner not cleared; got %q", owner)
	}

	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (cursor on job-B): %v", err)
	}
	v2, _ := gu.g.View(winJobInput)
	if v2 == nil {
		t.Fatalf("winJobInput missing for job-B")
	}
	if strings.Contains(v2.Buffer(), "hello to A") {
		t.Errorf("job-A draft leaked into job-B's input view: %q", v2.Buffer())
	}
}

// TestJobInput_SurvivesRefreshDrivenReshuffle pins the regression
// the latest review flagged: a refresh tick that REORDERS the
// jobs slice without an operator-authored cursor move (e.g. a
// newly-created job inserts at index 0 and pushes the previously-
// cursored row to index 1) must NOT silently wipe the operator's
// in-flight draft.
//
// The fix is to drop the clearJobInputIfStale call from
// applyRefreshLocked: only afterCursorMove (which fires on explicit
// j/k keypresses) clears the draft on jobID-mismatch. Refresh-
// driven reshuffles preserve it.
func TestJobInput_SurvivesRefreshDrivenReshuffle(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	// Seed: cursor on job-A at index 0, draft typed against job-A.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true}},
		}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
		gu.st.jobInput = "plan: refactor X"
		gu.st.jobInputOwner = "job-A"
	})

	// Refresh-driven reshuffle: a brand-new job-Z lands at index 0,
	// pushing job-A to index 1. jobCursor stays pinned at 0, so the
	// selected job is now job-Z. The operator did NOT press j/k.
	r := refreshResult{
		jobs: []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-Z", Status: "running", Streaming: true}},
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true}},
		},
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	var (
		input string
		owner string
	)
	gu.st.withRLock(func() {
		input = gu.st.jobInput
		owner = gu.st.jobInputOwner
	})
	if input != "plan: refactor X" {
		t.Errorf("refresh-driven reshuffle wiped the in-flight draft: jobInput=%q (want %q)", input, "plan: refactor X")
	}
	if owner != "job-A" {
		t.Errorf("jobInputOwner clobbered on refresh: %q (want job-A)", owner)
	}

	// Sanity: a subsequent EXPLICIT cursor move to job-A (now at
	// index 1) still wins — the owner matches so the draft survives.
	gu.st.withLock(func() { gu.st.jobCursor = 1 })
	gu.afterCursorMove(panelJobs)
	gu.st.withRLock(func() {
		input = gu.st.jobInput
		owner = gu.st.jobInputOwner
	})
	if input != "plan: refactor X" {
		t.Errorf("draft lost after re-cursoring back to job-A: jobInput=%q", input)
	}
	if owner != "job-A" {
		t.Errorf("owner lost after re-cursoring back to job-A: %q", owner)
	}
}

// TestLayout_JobInputAppears_WhenSelectedJobLive pins acceptance
// criterion 5: a live selected job allocates winJobInput, AND the
// input is overlaid INSIDE winDetail (not below it). winDetail's
// outer height stays the same as the no-input case — the input
// sits on top of detail's bottom rows; detail's visible content
// area is shrunk via detailEffectiveInnerH() in the scroll /
// sticky-tail handlers, not via boxlayout.
func TestLayout_JobInputAppears_WhenSelectedJobLive(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	seedRunningJob(gu, "job-run")
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	input, err := gu.g.View(winJobInput)
	if err != nil {
		t.Fatalf("winJobInput missing for live selected job: %v", err)
	}
	detail, err := gu.g.View(winDetail)
	if err != nil {
		t.Fatalf("winDetail missing: %v", err)
	}
	// Overlay containment: input's outer box must sit STRICTLY
	// inside detail's frame coordinates.
	dx0, dy0, dx1, dy1 := detail.Dimensions()
	ix0, iy0, ix1, iy1 := input.Dimensions()
	if ix0 <= dx0 || iy0 <= dy0 || ix1 >= dx1 || iy1 >= dy1 {
		t.Errorf("input box not contained inside detail's frame: detail=(%d,%d,%d,%d) input=(%d,%d,%d,%d)",
			dx0, dy0, dx1, dy1, ix0, iy0, ix1, iy1)
	}
	// Detail's outer height must NOT shrink due to the overlay —
	// the overlay sits inside detail's box, doesn't push it up.
	if dh := detail.Height(); dh < 8 {
		t.Errorf("winDetail unexpectedly thin (overlay should not shrink outer height): h=%d", dh)
	}
}

// TestLayout_JobInput_OnlyVisibleForNonTerminalJobs pins the
// post-feedback visibility contract: the input view is allocated
// for any non-terminal job (queued or running, with or without
// the Streaming flag) and absent for terminal jobs (the
// "archive view" state the user's feedback called out).
//
// The predicate matches the daemon's buildInputCommand contract
// — input is accepted whenever the job is non-terminal (dispatched
// as `prompt` when pi is idle, `steer` / `follow_up` when pi is
// streaming). Tying view lifetime to non-terminal status (rather
// than the narrower Streaming flag) eliminates the view-recreation
// churn that would otherwise destroy in-flight drafts on every
// agent_end→agent_start cycle.
func TestLayout_JobInput_OnlyVisibleForNonTerminalJobs(t *testing.T) {
	t.Run("running_and_streaming_shows_input", func(t *testing.T) {
		gu := jobDetailLayoutFixture(t)
		gu.st.withLock(func() {
			gu.st.jobs = []datasource.Job{{
				JobResponse: api.JobResponse{JobID: "job-x", Status: "running", Streaming: true},
			}}
			gu.st.jobCursor = 0
			gu.st.focused = panelJobs
		})
		if err := gu.layout(gu.g); err != nil {
			t.Fatalf("layout: %v", err)
		}
		if _, err := gu.g.View(winJobInput); err != nil {
			t.Errorf("winJobInput missing for running+streaming job: %v", err)
		}
	})
	t.Run("running_but_idle_still_shows_input", func(t *testing.T) {
		// Job is still running on the daemon but pi is between
		// turns (Streaming==false). Daemon's buildInputCommand
		// will dispatch as a plain `prompt`, so the input must
		// stay visible — the operator can still send messages.
		gu := jobDetailLayoutFixture(t)
		gu.st.withLock(func() {
			gu.st.jobs = []datasource.Job{{
				JobResponse: api.JobResponse{JobID: "job-x", Status: "running", Streaming: false},
			}}
			gu.st.jobCursor = 0
			gu.st.focused = panelJobs
		})
		if err := gu.layout(gu.g); err != nil {
			t.Fatalf("layout: %v", err)
		}
		if _, err := gu.g.View(winJobInput); err != nil {
			t.Errorf("winJobInput missing for running-but-idle job: %v", err)
		}
	})
	t.Run("queued_shows_input", func(t *testing.T) {
		gu := jobDetailLayoutFixture(t)
		gu.st.withLock(func() {
			gu.st.jobs = []datasource.Job{{
				JobResponse: api.JobResponse{JobID: "job-q", Status: "queued"},
			}}
			gu.st.jobCursor = 0
			gu.st.focused = panelJobs
		})
		if err := gu.layout(gu.g); err != nil {
			t.Fatalf("layout: %v", err)
		}
		if _, err := gu.g.View(winJobInput); err != nil {
			t.Errorf("winJobInput missing for queued job: %v", err)
		}
	})
	t.Run("terminal_hides_input", func(t *testing.T) {
		gu := jobDetailLayoutFixture(t)
		gu.st.withLock(func() {
			gu.st.jobs = []datasource.Job{{
				JobResponse: api.JobResponse{JobID: "job-x", Status: "done"},
			}}
			gu.st.jobCursor = 0
			gu.st.focused = panelJobs
		})
		if err := gu.layout(gu.g); err != nil {
			t.Fatalf("layout: %v", err)
		}
		if _, err := gu.g.View(winJobInput); err == nil {
			t.Errorf("winJobInput unexpectedly visible for terminal job")
		}
	})
}

// TestLayout_JobInputDisappears_WhenSelectedJobTerminal pins the
// running→terminal teardown: a terminal selected job means
// winJobInput is deleted. Also exercises the
// focused=panelJobInput case where the input view goes away
// while it still owned current view — layout must not leave the
// caret in phantom-focus on a deleted view.
func TestLayout_JobInputDisappears_WhenSelectedJobTerminal(t *testing.T) {
	t.Run("focus_on_jobs", func(t *testing.T) {
		gu := jobDetailLayoutFixture(t)
		seedRunningJob(gu, "job-run")
		if err := gu.layout(gu.g); err != nil {
			t.Fatalf("layout (running): %v", err)
		}
		if _, err := gu.g.View(winJobInput); err != nil {
			t.Fatalf("setup: winJobInput should exist for running job: %v", err)
		}
		// Flip to terminal. The input view is gated on isJobLive
		// (non-terminal status); the Status flip alone is sufficient.
		gu.st.withLock(func() {
			gu.st.jobs[0].Status = "done"
			gu.st.jobs[0].Streaming = false
		})
		if err := gu.layout(gu.g); err != nil {
			t.Fatalf("layout (terminal): %v", err)
		}
		if _, err := gu.g.View(winJobInput); err == nil {
			t.Errorf("winJobInput should be deleted for terminal job")
		}
	})
	t.Run("focus_on_jobInput", func(t *testing.T) {
		// The operator was typing in winJobInput (focused ==
		// panelJobInput). The job finishes mid-typing. The next
		// layout pass deletes winJobInput; without the
		// applyRefreshLocked focus-revert, layout's
		// SetCurrentView(focused.window() == winJobInput) would
		// fail with ErrUnknownView and leave the caret in
		// phantom-focus. After the running→terminal transition
		// is detected in applyRefreshLocked, focused must read
		// panelJobs so the next layout lands the caret somewhere
		// real.
		gu := jobDetailLayoutFixture(t)
		seedRunningJob(gu, "job-run")
		gu.st.withLock(func() { gu.st.focused = panelJobInput })
		if err := gu.layout(gu.g); err != nil {
			t.Fatalf("layout (running): %v", err)
		}
		// Drive applyRefreshLocked with a status-flip refreshResult
		// (that's where the focus revert lives).
		r := refreshResult{
			jobs: []datasource.Job{{
				JobResponse: api.JobResponse{JobID: "job-run", Status: "done"},
			}},
			taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
		}
		gu.applyRefreshLocked(r)
		var focused panelID
		gu.st.withRLock(func() { focused = gu.st.focused })
		if focused != panelJobs {
			t.Errorf("focused after running→terminal = %v, want panelJobs (input view goes away; revert focus)", focused)
		}
		if err := gu.layout(gu.g); err != nil {
			t.Fatalf("layout (terminal): %v", err)
		}
		if _, err := gu.g.View(winJobInput); err == nil {
			t.Errorf("winJobInput should be deleted for terminal job")
		}
		if cv := gu.g.CurrentView(); cv != nil && cv.Name() == winJobInput {
			t.Errorf("current view = winJobInput after running→terminal; should be on a real view")
		}
	})
}

// TestFocusPanel_DetailStashesPreviousFocus pins the focusPanel
// write side that TestRenderDetail_PanelDetailFocus_KeepsJobBody
// only exercises on the read side: pressing '0' from one of the
// side panels must record that panel into state.detailFocus so
// renderDetail can keep emitting the matching entity body.
//
// Without this stash, the panelDetail switch arm in renderDetail
// would fall through to "(nothing selected)" — the original
// blocking regression from the first review pass.
func TestFocusPanel_DetailStashesPreviousFocus(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	// Set focused = panelWorkflows so the outgoing focus is
	// distinct from the default panelTasks seed.
	gu.st.withLock(func() { gu.st.focused = panelWorkflows })
	if err := gu.focusPanel(panelDetail)(nil, nil); err != nil {
		t.Fatalf("focusPanel(panelDetail): %v", err)
	}
	var focused, detailFocus panelID
	gu.st.withRLock(func() {
		focused = gu.st.focused
		detailFocus = gu.st.detailFocus
	})
	if focused != panelDetail {
		t.Errorf("focused = %v, want panelDetail", focused)
	}
	if detailFocus != panelWorkflows {
		t.Errorf("detailFocus = %v, want panelWorkflows (the outgoing focus)", detailFocus)
	}
}

// TestFocusPanel_DetailRepeatedPressPreservesStash pins the guard
// in focusPanel: pressing '0' twice in a row must NOT re-stash
// `panelDetail` into detailFocus (which would later make
// renderDetail render itself recursively and fall through to
// "(nothing selected)"). The first press records the source
// panel; the second press finds focused == panelDetail already
// and skips the stash.
func TestFocusPanel_DetailRepeatedPressPreservesStash(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	gu.st.withLock(func() { gu.st.focused = panelJobs })
	// First press: stashes panelJobs.
	if err := gu.focusPanel(panelDetail)(nil, nil); err != nil {
		t.Fatalf("first focusPanel(panelDetail): %v", err)
	}
	// Second press: focused is already panelDetail; guard must
	// prevent overwriting detailFocus to panelDetail.
	if err := gu.focusPanel(panelDetail)(nil, nil); err != nil {
		t.Fatalf("second focusPanel(panelDetail): %v", err)
	}
	var detailFocus panelID
	gu.st.withRLock(func() { detailFocus = gu.st.detailFocus })
	if detailFocus != panelJobs {
		t.Errorf("detailFocus = %v after two presses, want panelJobs (original source preserved)", detailFocus)
	}
}

// TestFocusPanel_DetailFromJobInputNormalises pins the
// panelJobInput → panelJobs normalisation in focusPanel's stash
// path. If the operator is in the input textarea and presses '0',
// detailFocus must hold panelJobs (not panelJobInput) so the next
// renderDetail call can find its switch arm. The renderer has its
// own normalisation as a defence-in-depth, but storing the model
// field already normalised keeps the persisted state sensible.
func TestFocusPanel_DetailFromJobInputNormalises(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	gu.st.withLock(func() { gu.st.focused = panelJobInput })
	if err := gu.focusPanel(panelDetail)(nil, nil); err != nil {
		t.Fatalf("focusPanel(panelDetail): %v", err)
	}
	var detailFocus panelID
	gu.st.withRLock(func() { detailFocus = gu.st.detailFocus })
	if detailFocus != panelJobs {
		t.Errorf("detailFocus = %v, want panelJobs (panelJobInput must normalise)", detailFocus)
	}
}

// TestJobInput_FocusPersistsAcrossLayout pins the regression
// the third-round review flagged: after jobsEnter on a running
// job sets the current view to winJobInput, the next layout
// pass MUST NOT yank focus back to winJobs (the previous
// focused panel).
//
// The fix is the synthetic panelJobInput focus identity:
// jobsEnter sets state.focused = panelJobInput, and layout's
// `g.SetCurrentView(focused.window())` then re-asserts the
// input view as current on every redraw — even at the 100ms
// spinner-tick cadence — instead of fighting it back.
func TestJobInput_FocusPersistsAcrossLayout(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	seedRunningJob(gu, "job-run")
	// Layout #1: create the views (current view defaults to
	// winJobs because state.focused == panelJobs from the seed).
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout #1: %v", err)
	}
	if err := gu.jobsEnter(nil, nil); err != nil {
		t.Fatalf("jobsEnter: %v", err)
	}
	if cv := gu.g.CurrentView(); cv == nil || cv.Name() != winJobInput {
		name := "<nil>"
		if cv != nil {
			name = cv.Name()
		}
		t.Fatalf("after jobsEnter, current view = %q, want %q", name, winJobInput)
	}
	// Also assert the model field: state.focused must reflect
	// the synthetic input panel, not panelJobs (otherwise the
	// next layout would override SetCurrentView back to winJobs).
	var focused panelID
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelJobInput {
		t.Fatalf("state.focused = %v after jobsEnter, want panelJobInput", focused)
	}

	// Layout #2: simulates the next spinner tick / refresh tick
	// redrawing the dashboard. SetCurrentView is called inside
	// layout unconditionally; the input view MUST still be
	// current afterwards.
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout #2: %v", err)
	}
	if cv := gu.g.CurrentView(); cv == nil || cv.Name() != winJobInput {
		name := "<nil>"
		if cv != nil {
			name = cv.Name()
		}
		t.Errorf("after layout #2, current view = %q, want %q (focus did not persist across redraw)", name, winJobInput)
	}

	// One more layout pass for good measure.
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout #3: %v", err)
	}
	if cv := gu.g.CurrentView(); cv == nil || cv.Name() != winJobInput {
		name := "<nil>"
		if cv != nil {
			name = cv.Name()
		}
		t.Errorf("after layout #3, current view = %q, want %q", name, winJobInput)
	}
}

// TestJobInput_EscRestoresJobsFocus pins the Esc handler contract:
// jobInputEscape (bound to Esc on winJobInput) returns state.focused
// to panelJobs so the next layout pass moves the caret back to
// the Jobs panel — mirroring the operator's mental model of
// "close the textarea, back to navigating jobs".
func TestJobInput_EscRestoresJobsFocus(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	seedRunningJob(gu, "job-run")
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	// Enter the input.
	if err := gu.jobsEnter(nil, nil); err != nil {
		t.Fatalf("jobsEnter: %v", err)
	}
	var focused panelID
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelJobInput {
		t.Fatalf("setup: state.focused = %v, want panelJobInput", focused)
	}
	// Esc.
	if err := gu.jobInputEscape(nil, nil); err != nil {
		t.Fatalf("jobInputEscape: %v", err)
	}
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelJobs {
		t.Errorf("state.focused after Esc = %v, want panelJobs", focused)
	}
}

// dispatchFakeDS captures SendInput / AbortJob calls so the
// TestLiveDispatch_* / TestLiveAbort_* tests can assert the
// target jobID.
type dispatchFakeDS struct {
	refreshFakeDS
	calls        atomic.Int32
	lastJob      atomic.Value // string
	lastMsg      atomic.Value // string
	abortCalls   atomic.Int32
	abortLastJob atomic.Value // string
}

func (f *dispatchFakeDS) SendInput(_ context.Context, jobID, msg, _ string) (string, error) {
	f.calls.Add(1)
	f.lastJob.Store(jobID)
	f.lastMsg.Store(msg)
	return "ok", nil
}

func (f *dispatchFakeDS) AbortJob(_ context.Context, jobID string) error {
	f.abortCalls.Add(1)
	f.abortLastJob.Store(jobID)
	return nil
}

// TestLiveDispatch_AfterReshuffle_DispatchesToOwner pins the
// regression the third-round review flagged: a refresh-driven
// reshuffle of the jobs slice preserves the draft text (per the
// previous review's option-(a)), but the dispatch must still
// target the AUTHORED job — not whatever job ended up under
// the cursor after the reshuffle.
//
// Scenario:
//
//  1. Operator drafts "plan: refactor X" against job-A.
//  2. Refresh tick reshuffles the jobs slice so job-Z is at
//     index 0 and job-A at index 1; the cursor is restored to
//     job-A by ID, so the selected job stays job-A.
//  3. Operator presses Ctrl-D (liveSend / liveDispatch).
//  4. SendInput must be called with jobID == "job-A" (the
//     authored target, recorded in jobInputOwner).
func (*dispatchFakeDS) SyncWorkflows(_ context.Context, _, _ bool) (datasource.SyncReport, error) { return datasource.SyncReport{}, nil }
func TestLiveDispatch_AfterReshuffle_DispatchesToOwner(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	ds := &dispatchFakeDS{}
	gu.ds = ds

	// Seed: cursor on job-A, draft typed against job-A.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true}},
		}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
		gu.st.jobInput = "plan: refactor X"
		gu.st.jobInputOwner = "job-A"
	})

	// Refresh-driven reshuffle: job-Z lands at index 0, job-A at index 1.
	// ID-based restore keeps the cursor on job-A (index 1).
	r := refreshResult{
		jobs: []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-Z", Status: "running", Streaming: true}},
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true}},
		},
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	// Layout so winJobInput exists with the draft text.
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	v, err := gu.g.View(winJobInput)
	if err != nil {
		t.Fatalf("winJobInput missing: %v", err)
	}
	if !strings.Contains(v.Buffer(), "plan: refactor X") {
		t.Fatalf("setup: draft text not in view buffer: %q", v.Buffer())
	}

	// Press Ctrl-D → liveSend → liveDispatch. We invoke directly to
	// avoid the keybinding plumbing.
	if err := gu.liveSend(gu.g, v); err != nil {
		t.Fatalf("liveSend: %v", err)
	}

	// liveDispatch dispatches inside g.OnWorker; the headless gui
	// doesn't drain workers automatically. Poll for up to a second.
	deadline := time.Now().Add(1 * time.Second)
	for ds.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ds.calls.Load(); got != 1 {
		t.Fatalf("SendInput calls = %d, want 1", got)
	}
	if got, _ := ds.lastJob.Load().(string); got != "job-A" {
		t.Errorf("SendInput target jobID = %q, want %q (the authored target, NOT the post-reshuffle cursor)", got, "job-A")
	}
	if got, _ := ds.lastMsg.Load().(string); got != "plan: refactor X" {
		t.Errorf("SendInput msg = %q, want %q", got, "plan: refactor X")
	}
}

// TestLiveDispatch_AfterReshuffleAndKeystroke_DispatchesToOwner
// pins the regression review flagged: even if the operator types
// one more character AFTER a refresh-driven reshuffle of the
// jobs slice, the dispatch target must remain the authored job
// (jobInputOwner), NOT whatever job happens to be under the
// cursor.
//
// The previous round's TestLiveDispatch_AfterReshuffle_
// DispatchesToOwner passed because the cursor was stuck at index 0
// (the old positional behaviour).  With ID-based restore the
// cursor now tracks the selected job, but the liveEditor must still
// NOT re-stamp jobInputOwner on a keystroke when a draft already
// exists, otherwise a later scope/filter change that removes the
// authored job could flip ownership to the new cursor job.
// The fix: liveEditor only stamps jobInputOwner when it is
// currently empty (i.e. on the first keystroke of a new draft).
func TestLiveDispatch_AfterReshuffleAndKeystroke_DispatchesToOwner(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	ds := &dispatchFakeDS{}
	gu.ds = ds

	// Seed: cursor on job-A, draft typed against job-A.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true}},
		}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
		gu.st.jobInput = "hello"
		gu.st.jobInputOwner = "job-A"
	})

	// Refresh-driven reshuffle: job-Z lands at index 0, job-A at index 1.
	// ID-based restore keeps the cursor on job-A (index 1).
	r := refreshResult{
		jobs: []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-Z", Status: "running", Streaming: true}},
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true}},
		},
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	// Layout so winJobInput exists with the draft text.
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	v, err := gu.g.View(winJobInput)
	if err != nil {
		t.Fatalf("winJobInput missing: %v", err)
	}

	// One more keystroke AFTER the reshuffle. The operator appends
	// '!' to their existing draft. With currentJobID() now resolving
	// to job-Z, the broken liveEditor would re-stamp jobInputOwner
	// to job-Z and silently redirect the dispatch.
	gu.liveEditor(v, 0, '!', 0)

	// Verify the stamp survived the keystroke.
	var owner string
	gu.st.withRLock(func() { owner = gu.st.jobInputOwner })
	if owner != "job-A" {
		t.Errorf("jobInputOwner re-attributed by liveEditor after keystroke: %q (want job-A)", owner)
	}

	// Dispatch.
	if err := gu.liveSend(gu.g, v); err != nil {
		t.Fatalf("liveSend: %v", err)
	}
	deadline := time.Now().Add(1 * time.Second)
	for ds.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ds.calls.Load(); got != 1 {
		t.Fatalf("SendInput calls = %d, want 1", got)
	}
	if got, _ := ds.lastJob.Load().(string); got != "job-A" {
		t.Errorf("SendInput target jobID = %q, want %q (the authored target, NOT the post-keystroke cursor)", got, "job-A")
	}
}

// TestLiveAbort_AfterReshuffle_TargetsOwner mirrors
// TestLiveDispatch_AfterReshuffle_DispatchesToOwner for the
// Ctrl-A handler: a refresh-driven reshuffle that shifts the
// cursor's underlying jobID must NOT silently redirect a Ctrl-A
// to the wrong job. The handler must use jobInputOwner (the
// operator's authored target), with currentJobID() only as the
// fallback when no draft has been authored yet.
func TestLiveAbort_AfterReshuffle_TargetsOwner(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	ds := &dispatchFakeDS{}
	gu.ds = ds

	// Seed: cursor on job-A with an authored draft.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true}},
		}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
		gu.st.jobInput = "about to escalate"
		gu.st.jobInputOwner = "job-A"
	})

	// Refresh reshuffle: job-Z at index 0, job-A at index 1.
	r := refreshResult{
		jobs: []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-Z", Status: "running", Streaming: true}},
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true}},
		},
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	if err := gu.liveAbort(gu.g, nil); err != nil {
		t.Fatalf("liveAbort: %v", err)
	}
	deadline := time.Now().Add(1 * time.Second)
	for ds.abortCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ds.abortCalls.Load(); got != 1 {
		t.Fatalf("AbortJob calls = %d, want 1", got)
	}
	if got, _ := ds.abortLastJob.Load().(string); got != "job-A" {
		t.Errorf("AbortJob target = %q, want %q (the authored target)", got, "job-A")
	}

	// Fallback path: no authored draft → abort targets the current
	// cursor.  With ID-based cursor restore the selection stays on
	// job-A (index 1) even after the reshuffle, so the fallback
	// target is job-A.
	gu.st.withLock(func() {
		gu.st.jobInput = ""
		gu.st.jobInputOwner = ""
	})
	ds.abortCalls.Store(0)
	ds.abortLastJob.Store("")
	if err := gu.liveAbort(gu.g, nil); err != nil {
		t.Fatalf("liveAbort fallback: %v", err)
	}
	deadline = time.Now().Add(1 * time.Second)
	for ds.abortCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got, _ := ds.abortLastJob.Load().(string); got != "job-A" {
		t.Errorf("AbortJob fallback target = %q, want %q (current cursor when no draft)", got, "job-A")
	}
}

// TestCyclePanel_FromJobInput_StartsFromJobs pins the
// source-side normalisation in cyclePanel: Tab from
// panelJobInput must NOT skip the Jobs column (the visual
// anchor of the input). The expected behaviour is to first
// restore the cycle source to panelJobs and then step from
// there, so Tab→panelWorkflows and Shift-Tab→panelTasks.
func TestCyclePanel_FromJobInput_StartsFromJobs(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	seedRunningJob(gu, "job-run")
	gu.st.withLock(func() { gu.st.focused = panelJobInput })

	if err := gu.cyclePanel(+1)(nil, nil); err != nil {
		t.Fatalf("cyclePanel(+1): %v", err)
	}
	var focused panelID
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelWorkflows {
		t.Errorf("Tab from panelJobInput landed on %v, want panelWorkflows", focused)
	}

	// Reset and verify Shift-Tab (step=-1) symmetrically lands on
	// panelTasks (the panel BEFORE panelJobs in the cycle).
	gu.st.withLock(func() { gu.st.focused = panelJobInput })
	if err := gu.cyclePanel(-1)(nil, nil); err != nil {
		t.Fatalf("cyclePanel(-1): %v", err)
	}
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelTasks {
		t.Errorf("Shift-Tab from panelJobInput landed on %v, want panelTasks", focused)
	}
}

// TestLayout_EnterFocusesJobInput pins acceptance criterion 6:
// jobsEnter on a running job moves current view to winJobInput;
// on a terminal job it moves logical focus to panelDetail.
func TestLayout_EnterFocusesJobInput(t *testing.T) {
	t.Run("running_focuses_input", func(t *testing.T) {
		gu := jobDetailLayoutFixture(t)
		seedRunningJob(gu, "job-run")
		// First layout to create the views.
		if err := gu.layout(gu.g); err != nil {
			t.Fatalf("layout: %v", err)
		}
		// Move the current view somewhere else first so we can
		// observe jobsEnter's SetCurrentView actually firing
		// (rather than passing trivially because winJobInput was
		// already current after the layout pass).
		if _, err := gu.g.SetCurrentView(winJobs); err != nil {
			t.Fatalf("setup: SetCurrentView winJobs: %v", err)
		}

		if err := gu.jobsEnter(nil, nil); err != nil {
			t.Fatalf("jobsEnter: %v", err)
		}
		// jobsEnter applies SetCurrentView synchronously now — the
		// previous g.Update wrap was vestigial. The current view
		// must already be winJobInput by the time jobsEnter returns.
		cv := gu.g.CurrentView()
		if cv == nil || cv.Name() != winJobInput {
			name := "<nil>"
			if cv != nil {
				name = cv.Name()
			}
			t.Errorf("current view after jobsEnter = %q, want %q", name, winJobInput)
		}
	})
	t.Run("terminal_focuses_detail", func(t *testing.T) {
		gu := jobDetailLayoutFixture(t)
		seedTerminalJob(gu, "job-done")
		if err := gu.layout(gu.g); err != nil {
			t.Fatalf("layout: %v", err)
		}
		if err := gu.jobsEnter(nil, nil); err != nil {
			t.Fatalf("jobsEnter: %v", err)
		}
		var focused panelID
		gu.st.withRLock(func() { focused = gu.st.focused })
		if focused != panelDetail {
			t.Errorf("focused = %v, want panelDetail", focused)
		}
	})
}

// TestJobInput_TextAreaClearedOnCursorMove pins the BLOCKING
// regression the latest review flagged: gocui's editable views
// maintain TWO buffers — v.lines (what Buffer()/Write touch) and
// v.TextArea (where SimpleEditor's TypeCharacter writes; the next
// keystroke calls RenderTextArea which re-rasterises TextArea
// back into v.lines). The old clear path (`v.Clear() + v.SetCursor`)
// only scrubs v.lines, so the very next keystroke on the new job
// re-materialised the previous job's draft into the visible buffer.
//
// Fix: clearJobInputIfStale must use v.ClearTextArea() which scrubs
// both buffers. This test drives the production flow end-to-end
// (real liveEditor → real cursor move → real liveEditor) and
// asserts no leak survives.
func TestJobInput_TextAreaClearedOnCursorMove(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running", Streaming: true}},
			{JobResponse: api.JobResponse{JobID: "job-B", Status: "running", Streaming: true}},
		}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (cursor on job-A): %v", err)
	}
	v, err := gu.g.View(winJobInput)
	if err != nil {
		t.Fatalf("winJobInput missing: %v", err)
	}
	// Type "hello" against job-A through the REAL editor path.
	for _, ch := range "hello" {
		gu.liveEditor(v, 0, ch, 0)
	}
	if got := v.Buffer(); !strings.Contains(got, "hello") {
		t.Fatalf("setup: typed 'hello' but view buffer is %q", got)
	}
	var input, owner string
	gu.st.withRLock(func() {
		input = gu.st.jobInput
		owner = gu.st.jobInputOwner
	})
	if !strings.Contains(input, "hello") || owner != "job-A" {
		t.Fatalf("setup: state.jobInput=%q owner=%q (want hello/job-A)", input, owner)
	}

	// Explicit cursor move to job-B. afterCursorMove(panelJobs)
	// triggers clearJobInputIfStale because jobInputOwner="job-A"
	// no longer matches the selected job (job-B).
	gu.st.withLock(func() { gu.st.jobCursor = 1 })
	gu.afterCursorMove(panelJobs)

	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (cursor on job-B): %v", err)
	}
	v2, err := gu.g.View(winJobInput)
	if err != nil {
		t.Fatalf("winJobInput missing after cursor move: %v", err)
	}
	if got := v2.Buffer(); strings.Contains(got, "hello") {
		t.Errorf("job-A's draft leaked into v.lines on job-B (BEFORE keystroke): %q", got)
	}

	// One more keystroke on job-B. If v.ClearTextArea was NOT
	// used, this fires RenderTextArea on the still-populated
	// TextArea and "hello" reappears prepended to 'x'.
	gu.liveEditor(v2, 0, 'x', 0)

	if got := v2.Buffer(); strings.Contains(got, "hello") {
		t.Errorf("LEAK: job-A's 'hello' re-materialised into job-B's input on first keystroke: %q", got)
	}
	gu.st.withRLock(func() {
		input = gu.st.jobInput
		owner = gu.st.jobInputOwner
	})
	if strings.Contains(input, "hello") {
		t.Errorf("LEAK: state.jobInput contains job-A text after typing on job-B: %q", input)
	}
	if owner != "job-B" {
		t.Errorf("jobInputOwner = %q, want job-B (the new authored target)", owner)
	}
}

// TestJobInput_TextAreaClearedOnDispatch is the Ctrl-D variant of
// TestJobInput_TextAreaClearedOnCursorMove. After a successful
// dispatch the textarea must be fully empty — both v.lines and
// v.TextArea — so the next keystroke starts a fresh draft instead
// of re-materialising the just-sent message.
func TestJobInput_TextAreaClearedOnDispatch(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	ds := &dispatchFakeDS{}
	gu.ds = ds
	seedRunningJob(gu, "job-A")
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	v, err := gu.g.View(winJobInput)
	if err != nil {
		t.Fatalf("winJobInput missing: %v", err)
	}
	for _, ch := range "first message" {
		gu.liveEditor(v, 0, ch, 0)
	}
	if err := gu.liveSend(gu.g, v); err != nil {
		t.Fatalf("liveSend: %v", err)
	}
	// liveDispatch routes the actual SendInput call onto a
	// worker, but the buffer + state clearing happen inline
	// before that. The buffer must already be empty.
	if got := strings.TrimSpace(v.Buffer()); got != "" {
		t.Errorf("v.Buffer() after dispatch = %q, want empty", got)
	}

	// Next keystroke. With the old v.Clear() path the TextArea
	// still holds "first message" and RenderTextArea would
	// re-emit it prepended to 'h'.
	gu.liveEditor(v, 0, 'h', 0)
	if got := v.Buffer(); strings.Contains(got, "first message") {
		t.Errorf("LEAK: dispatched message re-materialised on next keystroke: %q", got)
	}
	if got := strings.TrimSpace(v.Buffer()); got != "h" {
		t.Errorf("v.Buffer() after fresh keystroke = %q, want %q", got, "h")
	}
}

// TestJobInput_TextAreaClearedOnEscape is the Esc variant. After
// jobInputEscape the textarea must be empty enough that returning
// to the same running job and pressing a key starts a fresh draft
// instead of re-materialising the abandoned one.
func TestJobInput_TextAreaClearedOnEscape(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	seedRunningJob(gu, "job-A")
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	v, err := gu.g.View(winJobInput)
	if err != nil {
		t.Fatalf("winJobInput missing: %v", err)
	}
	for _, ch := range "draft" {
		gu.liveEditor(v, 0, ch, 0)
	}
	if err := gu.jobInputEscape(nil, nil); err != nil {
		t.Fatalf("jobInputEscape: %v", err)
	}
	if got := strings.TrimSpace(v.Buffer()); got != "" {
		t.Errorf("v.Buffer() after Esc = %q, want empty", got)
	}

	// Operator returns: jobsEnter on the same running job moves
	// focus back to panelJobInput. Layout pass keeps the same view
	// (we never DeleteView while the running job stays selected).
	if err := gu.jobsEnter(nil, nil); err != nil {
		t.Fatalf("jobsEnter (return): %v", err)
	}
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (return): %v", err)
	}
	v2, err := gu.g.View(winJobInput)
	if err != nil {
		t.Fatalf("winJobInput missing on return: %v", err)
	}
	// First keystroke after returning. With the old v.Clear() path
	// the TextArea still holds "draft" and 'y' would produce "drafty".
	gu.liveEditor(v2, 0, 'y', 0)
	if got := v2.Buffer(); strings.Contains(got, "draft") {
		t.Errorf("LEAK: previous draft re-materialised after Esc + return + keystroke: %q", got)
	}
	if got := strings.TrimSpace(v2.Buffer()); got != "y" {
		t.Errorf("v.Buffer() after fresh keystroke = %q, want %q", got, "y")
	}
}

// archiveFakeDS records ds.Messages() calls (the archive-load
// entrypoint scheduleJobArchive eventually dispatches via
// gu.g.OnWorker). Lets the focus-change regression test below poll
// for the worker-dispatched fetch to fire.
type archiveFakeDS struct {
	refreshFakeDS
	msgCalls atomic.Int32
	lastJob  atomic.Value // string
}

func (f *archiveFakeDS) Messages(_ context.Context, jobID string, _ bool, _ int) ([]datasource.MessageEvent, error) {
	f.msgCalls.Add(1)
	f.lastJob.Store(jobID)
	return nil, nil
}

// TestSettleOnPanelJobs_FocusChangeHydratesDetail pins the
// regression a manual-test report flagged: switching focus TO the
// Jobs panel without moving the cursor left the Detail pane stuck
// on "(loading...)" forever — the transcript only appeared after the
// operator pressed j/k. Root cause: scheduleJobArchive +
// scheduleJobLive were wired into afterCursorMove(panelJobs) only,
// so all three focus-change paths (focusPanel '2', cyclePanel Tab,
// tasksEnter on a task) bypassed the hydration entirely.
//
// The fix routes the same per-job side-effect block through
// settleOnPanelJobs and invokes it from every focus-change path
// that LANDS on panelJobs. This test asserts the contract
// observationally on two signals settleOnPanelJobs sets up:
//
//   - state.jobLiveTimer becomes non-nil synchronously (scheduleJobLive
//     arms a time.AfterFunc on every call). Before the fix the timer
//     stayed nil on the focus-change paths.
//   - ds.Messages() is invoked exactly once for the selected jobID
//     (scheduleJobArchive dispatches loadJobArchive on a worker; we
//     poll because OnWorker is async). Before the fix the call
//     never happened until a cursor move.
func (*archiveFakeDS) SyncWorkflows(_ context.Context, _, _ bool) (datasource.SyncReport, error) { return datasource.SyncReport{}, nil }
func TestSettleOnPanelJobs_FocusChangeHydratesDetail(t *testing.T) {
	cases := []struct {
		name string
		do   func(gu *Gui) error
	}{
		{"focusPanel('2')", func(gu *Gui) error { return gu.focusPanel(panelJobs)(gu.g, nil) }},
		{"cyclePanel(+1)_from_Tasks_lands_on_Jobs", func(gu *Gui) error { return gu.cyclePanel(1)(gu.g, nil) }},
		{"tasksEnter", func(gu *Gui) error { return gu.tasksEnter(nil, nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gu := jobDetailLayoutFixture(t)
			ds := &archiveFakeDS{}
			gu.ds = ds
			// Seed: one running job; focus starts on Tasks (so the
			// upcoming focus change crosses the panelTasks→panelJobs
			// boundary we're testing).
			gu.st.withLock(func() {
				gu.st.jobs = []datasource.Job{{
					JobResponse: api.JobResponse{JobID: "job-X", Status: "running", Streaming: true},
				}}
				gu.st.jobCursor = 0
				gu.st.focused = panelTasks
			})

			// Setup sanity: live timer not armed, transcript cache empty.
			gu.st.withRLock(func() {
				if gu.st.jobLiveTimer != nil {
					t.Fatalf("setup: jobLiveTimer should be nil before focus change")
				}
				if _, ok := gu.st.jobTranscript["job-X"]; ok {
					t.Fatalf("setup: jobTranscript should be empty before focus change")
				}
			})

			if err := tc.do(gu); err != nil {
				t.Fatalf("focus handler: %v", err)
			}
			t.Cleanup(func() { gu.stopJobLive() })

			// Signal 1 (synchronous): scheduleJobLive armed the
			// debounce timer. Smoking-gun assertion for the bug —
			// BEFORE the fix this stayed nil on focus changes.
			gu.st.withRLock(func() {
				if gu.st.jobLiveTimer == nil {
					t.Errorf("jobLiveTimer not armed after focus change \u2014 settleOnPanelJobs was not invoked ('(loading...)' bug regression)")
				}
			})

			// Signal 2 (async): scheduleJobArchive dispatched
			// loadJobArchive on a worker, which called ds.Messages.
			// Poll briefly because gocui's OnWorker runs on its own
			// goroutine.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) && ds.msgCalls.Load() == 0 {
				time.Sleep(10 * time.Millisecond)
			}
			if got := ds.msgCalls.Load(); got != 1 {
				t.Errorf("ds.Messages calls = %d, want 1 (archive hydration not dispatched)", got)
			}
			if got, _ := ds.lastJob.Load().(string); got != "job-X" {
				t.Errorf("ds.Messages last jobID = %q, want job-X", got)
			}
		})
	}
}
