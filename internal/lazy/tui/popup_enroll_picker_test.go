package tui

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
)

// enrollFakeDS captures the Enroll / Resume calls so the picker
// tests can assert that the Enter-on-step path dispatches the right
// (taskID, workflow, step) triple. Embeds refreshFakeDS for full
// Datasource surface coverage.
type enrollFakeDS struct {
	refreshFakeDS
	mu sync.Mutex

	enrollCalled bool
	enrollID     string
	enrollWF     string
	enrollStep   string
	enrollErr    error

	resumeCalled bool
	resumeID     string
	resumeStep   string
	resumeErr    error
}

func (f *enrollFakeDS) Enroll(_ context.Context, id, wf, step string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrollCalled = true
	f.enrollID = id
	f.enrollWF = wf
	f.enrollStep = step
	return f.enrollErr
}

func (f *enrollFakeDS) Resume(_ context.Context, id, step string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalled = true
	f.resumeID = id
	f.resumeStep = step
	return f.resumeErr
}

// Sanity check: the type satisfies datasource.Datasource via the
// embedded refreshFakeDS.
var _ datasource.Datasource = (*enrollFakeDS)(nil)

// newPickerTestGui builds a headless Gui with sensible defaults for
// the popup-flow tests: synchronous dispatcher (so OnPick paths land
// without a real MainLoop) and a fresh state with one selected task.
func (*enrollFakeDS) SyncWorkflows(_ context.Context, _, _ bool) (datasource.SyncReport, error) { return datasource.SyncReport{}, nil }
func newPickerTestGui(t *testing.T) (*Gui, *enrollFakeDS) {
	t.Helper()
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      120,
		Height:     40,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	fake := &enrollFakeDS{}
	gu := &Gui{g: g, st: newState(), ctx: context.Background()}
	gu.ds = fake
	gu.dispatch = func(f func()) { f() } // synchronous worker
	return gu, fake
}

// seedPickerWorkflows installs a two-workflow + one-synthetic
// workflow slice on the gui's state. Returns the slice the tests
// can index by name (and the synthetic one — never expected to
// appear in the picker but useful for the exclusion assertion).
func seedPickerWorkflows(gu *Gui) (a, b, synthetic datasource.Workflow) {
	a = datasource.Workflow{
		ID: "wf-a", Name: "feature-dev-generic",
		Steps: []datasource.WorkflowStep{
			{ID: "st-a-dev", Name: "dev"},
			{ID: "st-a-rev", Name: "review"},
		},
	}
	b = datasource.Workflow{
		ID: "wf-b", Name: "release",
		Steps: []datasource.WorkflowStep{
			{ID: "st-b-cut", Name: "cut"},
			{ID: "st-b-ship", Name: "ship"},
		},
	}
	synthetic = datasource.Workflow{
		ID: "wf-syn", Name: "single:@autosk/dev",
		IsSynthetic: true,
		Steps: []datasource.WorkflowStep{
			{ID: "st-syn-do", Name: "do"},
		},
	}
	gu.st.workflows = []datasource.Workflow{a, synthetic, b}
	return a, b, synthetic
}

// TestTaskEnroll_OpensPickerExcludingSynthetic pins the AC:
// pressing `e` on the Tasks panel opens popupEnroll with the
// workflow list excluding synthetic single:<agent> rows.
func TestTaskEnroll_OpensPickerExcludingSynthetic(t *testing.T) {
	gu, _ := newPickerTestGui(t)
	a, b, syn := seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa"}}
	gu.st.taskCursor = 0

	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupEnroll {
		t.Fatalf("popup kind=%v want popupEnroll", k)
	}
	if len(gu.st.popup.Workflows) != 2 {
		t.Fatalf("picker workflows = %d, want 2 (synthetic must be filtered out): %+v", len(gu.st.popup.Workflows), gu.st.popup.Workflows)
	}
	got := []string{gu.st.popup.Workflows[0].Name, gu.st.popup.Workflows[1].Name}
	for _, name := range got {
		if name == syn.Name {
			t.Fatalf("synthetic workflow %q leaked into picker: %v", syn.Name, got)
		}
	}
	// Real workflows preserved in input order (the picker doesn't
	// re-sort).
	if got[0] != a.Name || got[1] != b.Name {
		t.Errorf("workflow order = %v, want %v", got, []string{a.Name, b.Name})
	}
	if gu.st.popup.WorkflowLocked {
		t.Errorf("enroll picker must NOT be locked (resume locks; enroll doesn't)")
	}
	if gu.st.popup.ActivePane != pickerPaneWorkflow {
		t.Errorf("enroll picker initial pane = %v, want pickerPaneWorkflow", gu.st.popup.ActivePane)
	}
}

// TestTaskEnroll_PreselectsCurrentWorkflow pins the cursor seeding:
// when the task's WorkflowID matches an entry in the cached
// workflows slice, the picker's workflow cursor lands on that row.
func TestTaskEnroll_PreselectsCurrentWorkflow(t *testing.T) {
	gu, _ := newPickerTestGui(t)
	_, b, _ := seedPickerWorkflows(gu)
	// Task is already enrolled in "release" (the second post-filter
	// workflow row).
	gu.st.tasks = []datasource.Task{{ID: "ask-bbbbbb", WorkflowID: b.ID, CurrentStepID: "st-b-ship"}}
	gu.st.taskCursor = 0

	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	// After filtering: index 0 = feature-dev-generic, index 1 = release.
	if gu.st.popup.WorkflowCursor != 1 {
		t.Errorf("WorkflowCursor=%d want 1 (task's current workflow=release)", gu.st.popup.WorkflowCursor)
	}
	// And the step cursor seeds from the task's current step.
	if gu.st.popup.StepCursor != 1 {
		t.Errorf("StepCursor=%d want 1 (task's current step=ship)", gu.st.popup.StepCursor)
	}
}

// TestTaskEnroll_FallsBackToRowZero pins the fallback: when the
// task has no current workflow (or it's not in the cached slice),
// the cursor lands on row 0.
func TestTaskEnroll_FallsBackToRowZero(t *testing.T) {
	gu, _ := newPickerTestGui(t)
	seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-cccccc"}} // no WorkflowID
	gu.st.taskCursor = 0

	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	if gu.st.popup.WorkflowCursor != 0 {
		t.Errorf("WorkflowCursor=%d want 0 (no current workflow)", gu.st.popup.WorkflowCursor)
	}
	if gu.st.popup.StepCursor != 0 {
		t.Errorf("StepCursor=%d want 0 (no current step)", gu.st.popup.StepCursor)
	}
}

// TestEnrollPickerCursor_RefreshesStepPane drives the j/k path: a
// cursor move on the workflow pane resets the step cursor to 0 (so
// the right pane re-renders with a sensible selection); no
// Datasource call is dispatched.
func TestEnrollPickerCursor_RefreshesStepPane(t *testing.T) {
	gu, fake := newPickerTestGui(t)
	seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa"}}
	gu.st.taskCursor = 0
	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	// Seed a non-zero step cursor so we can observe the reset.
	gu.st.withLock(func() { gu.st.popup.StepCursor = 1 })

	move := gu.enrollPickerCursor(pickerPaneWorkflow, +1)
	if err := move(nil, nil); err != nil {
		t.Fatalf("cursor move: %v", err)
	}
	if gu.st.popup.WorkflowCursor != 1 {
		t.Errorf("WorkflowCursor=%d want 1 (after +1 move)", gu.st.popup.WorkflowCursor)
	}
	if gu.st.popup.StepCursor != 0 {
		t.Errorf("StepCursor=%d want 0 (workflow cursor move must reset step cursor)", gu.st.popup.StepCursor)
	}
	if fake.enrollCalled {
		t.Errorf("Datasource.Enroll dispatched on cursor move; want only on Enter")
	}
}

// TestEnrollPickerWorkflowAccept_MovesFocusToStepPane pins the
// Enter-on-workflow path: focus moves to the step pane, the step
// cursor resets to 0, and the Datasource is NOT called yet.
func TestEnrollPickerWorkflowAccept_MovesFocusToStepPane(t *testing.T) {
	gu, fake := newPickerTestGui(t)
	seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa"}}
	gu.st.taskCursor = 0
	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	// Pretend the operator wandered the step cursor to row 1 (shouldn't
	// happen via real input on this pane, but the reset is best
	// verified against a non-zero seed).
	gu.st.withLock(func() { gu.st.popup.StepCursor = 1 })

	if err := gu.enrollPickerWorkflowAccept(nil, nil); err != nil {
		t.Fatalf("workflow accept: %v", err)
	}
	if gu.st.popup.ActivePane != pickerPaneStep {
		t.Errorf("ActivePane=%v want pickerPaneStep (after Enter on workflow)", gu.st.popup.ActivePane)
	}
	if gu.st.popup.StepCursor != 0 {
		t.Errorf("StepCursor=%d want 0 (Enter on workflow must reset to first step)", gu.st.popup.StepCursor)
	}
	if fake.enrollCalled {
		t.Errorf("Datasource.Enroll dispatched on workflow Enter; want only on step Enter")
	}
}

// TestEnrollPickerStepAccept_DispatchesEnroll pins the happy path:
// Enter on the step pane fires Datasource.Enroll(taskID, wfName,
// stepName) once with the highlighted workflow's and step's names.
func TestEnrollPickerStepAccept_DispatchesEnroll(t *testing.T) {
	gu, fake := newPickerTestGui(t)
	a, _, _ := seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa"}}
	gu.st.taskCursor = 0
	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	// Operator picks the second step of the first workflow.
	gu.st.withLock(func() {
		gu.st.popup.ActivePane = pickerPaneStep
		gu.st.popup.WorkflowCursor = 0
		gu.st.popup.StepCursor = 1
	})

	if err := gu.enrollPickerStepAccept(nil, nil); err != nil {
		t.Fatalf("step accept: %v", err)
	}
	if !fake.enrollCalled {
		t.Fatalf("Datasource.Enroll was not dispatched")
	}
	if fake.enrollID != "ask-aaaaaa" {
		t.Errorf("enroll id=%q want ask-aaaaaa", fake.enrollID)
	}
	// Smoke check that seedPickerWorkflows still names the canonical
	// workflow row "feature-dev-generic" (review R10): if a future
	// edit to the seed changes that, this assertion fires loudly
	// instead of silently breaking the per-name dispatch check below.
	if a.Name != "feature-dev-generic" {
		t.Fatalf("seedPickerWorkflows changed the canonical workflow name to %q; update the test", a.Name)
	}
	if fake.enrollWF != a.Name {
		t.Errorf("enroll workflow=%q want %q", fake.enrollWF, a.Name)
	}
	if fake.enrollStep != "review" {
		t.Errorf("enroll step=%q want review", fake.enrollStep)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("popup kind=%v want popupNone after dispatch", k)
	}
}

// TestEnrollPickerStepEscape_ReturnsToWorkflowPane pins the
// back-step behaviour on the enroll flow: Esc on the step pane
// moves focus back to the workflow pane WITHOUT clearing the
// workflow cursor.
func TestEnrollPickerStepEscape_ReturnsToWorkflowPane(t *testing.T) {
	gu, fake := newPickerTestGui(t)
	seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa"}}
	gu.st.taskCursor = 0
	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	// Simulate the user already on the step pane after Enter on
	// row 1 of the workflow pane.
	gu.st.withLock(func() {
		gu.st.popup.WorkflowCursor = 1
		gu.st.popup.ActivePane = pickerPaneStep
		gu.st.popup.StepCursor = 1
	})

	if err := gu.enrollPickerStepEscape(nil, nil); err != nil {
		t.Fatalf("step escape: %v", err)
	}
	if gu.st.popup.ActivePane != pickerPaneWorkflow {
		t.Errorf("ActivePane=%v want pickerPaneWorkflow (after Esc on step pane)", gu.st.popup.ActivePane)
	}
	if gu.st.popup.WorkflowCursor != 1 {
		t.Errorf("WorkflowCursor=%d want 1 (Esc must preserve the workflow cursor)", gu.st.popup.WorkflowCursor)
	}
	if gu.st.popup.Kind != popupEnroll {
		t.Errorf("popup closed on Esc; want it to stay open on the workflow pane")
	}
	if fake.enrollCalled {
		t.Errorf("Esc must NOT dispatch Enroll")
	}
}

// TestEnrollPickerWorkflowEscape_ClosesPopup pins that Esc on the
// workflow pane (via popupClose) closes the picker entirely. The
// production binding maps winEnrollWorkflowList/Esc directly to
// popupClose so the test calls that path.
func TestEnrollPickerWorkflowEscape_ClosesPopup(t *testing.T) {
	gu, fake := newPickerTestGui(t)
	seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa"}}
	gu.st.taskCursor = 0
	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	if err := gu.popupClose(nil, nil); err != nil {
		t.Fatalf("popupClose: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("popup kind=%v want popupNone after Esc on workflow pane", k)
	}
	if fake.enrollCalled {
		t.Errorf("Esc on workflow pane must NOT dispatch Enroll")
	}
}

// TestTaskEnroll_NoWorkflowsFlashes pins the empty-set branch:
// pressing `e` with zero non-synthetic workflows flashes
// "no workflows defined" and does NOT open the popup.
func TestTaskEnroll_NoWorkflowsFlashes(t *testing.T) {
	gu, fake := newPickerTestGui(t)
	// Only a synthetic row in the cache.
	gu.st.workflows = []datasource.Workflow{
		{ID: "wf-syn", Name: "single:@autosk/dev", IsSynthetic: true},
	}
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa"}}
	gu.st.taskCursor = 0

	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("popup must NOT open when no workflows are defined; got kind=%v", k)
	}
	if gu.st.flash.Text == "" || !strings.Contains(gu.st.flash.Text, "no workflows defined") {
		t.Errorf("flash text = %q, want a 'no workflows defined' hint", gu.st.flash.Text)
	}
	if fake.enrollCalled {
		t.Errorf("Datasource.Enroll must not fire on the empty-set branch")
	}
}

// TestTaskResume_OpensPickerLocked pins the resume flow: pressing
// `r` opens the picker with WorkflowLocked=true (single row =
// task's current workflow) and focus already on the step pane.
func TestTaskResume_OpensPickerLocked(t *testing.T) {
	gu, _ := newPickerTestGui(t)
	_, b, _ := seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-rrrrrr", WorkflowID: b.ID, CurrentStepID: "st-b-ship"}}
	gu.st.taskCursor = 0

	if err := gu.taskResume(nil, nil); err != nil {
		t.Fatalf("taskResume: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupEnroll {
		t.Fatalf("popup kind=%v want popupEnroll", k)
	}
	if !gu.st.popup.WorkflowLocked {
		t.Errorf("resume picker must have WorkflowLocked=true")
	}
	if len(gu.st.popup.Workflows) != 1 {
		t.Fatalf("resume picker workflows = %d, want exactly 1 (task's current workflow)", len(gu.st.popup.Workflows))
	}
	if gu.st.popup.Workflows[0].ID != b.ID {
		t.Errorf("resume picker workflow = %q, want task's current workflow %q", gu.st.popup.Workflows[0].ID, b.ID)
	}
	if gu.st.popup.ActivePane != pickerPaneStep {
		t.Errorf("resume picker initial pane = %v, want pickerPaneStep", gu.st.popup.ActivePane)
	}
	if gu.st.popup.StepCursor != 1 {
		t.Errorf("resume step cursor = %d, want 1 (task's current step = ship)", gu.st.popup.StepCursor)
	}
}

// TestResumePickerStepAccept_DispatchesResume pins the Enter-on-step
// path for the resume flow: it dispatches Datasource.Resume(taskID,
// stepName) with the highlighted step's name.
func TestResumePickerStepAccept_DispatchesResume(t *testing.T) {
	gu, fake := newPickerTestGui(t)
	_, b, _ := seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-rrrrrr", WorkflowID: b.ID, CurrentStepID: "st-b-ship"}}
	gu.st.taskCursor = 0
	if err := gu.taskResume(nil, nil); err != nil {
		t.Fatalf("taskResume: %v", err)
	}
	// Move to first step ("cut") and confirm.
	gu.st.withLock(func() { gu.st.popup.StepCursor = 0 })
	if err := gu.enrollPickerStepAccept(nil, nil); err != nil {
		t.Fatalf("step accept: %v", err)
	}
	if !fake.resumeCalled {
		t.Fatalf("Datasource.Resume was not dispatched")
	}
	if fake.resumeID != "ask-rrrrrr" {
		t.Errorf("resume id=%q want ask-rrrrrr", fake.resumeID)
	}
	if fake.resumeStep != "cut" {
		t.Errorf("resume step=%q want cut", fake.resumeStep)
	}
	if fake.enrollCalled {
		t.Errorf("Resume path must NOT dispatch Enroll")
	}
}

// TestResumePickerStepAccept_OnCurrentStepUsesNoBumpPath pins
// review R7 / CLI parity: Enter on the pre-selected current step
// dispatches Resume(taskID, "") — the status-only flip that does
// NOT bump step_visits or fire max_visits. Picking a DIFFERENT step
// still dispatches Resume(taskID, stepName) (covered by
// TestResumePickerStepAccept_DispatchesResume above).
func TestResumePickerStepAccept_OnCurrentStepUsesNoBumpPath(t *testing.T) {
	gu, fake := newPickerTestGui(t)
	_, b, _ := seedPickerWorkflows(gu)
	// Task parked on step "ship" (st-b-ship). The picker pre-selects
	// it; Enter without moving the cursor must collapse to the
	// no-bump branch.
	gu.st.tasks = []datasource.Task{{ID: "ask-rrrrrr", WorkflowID: b.ID, CurrentStepID: "st-b-ship"}}
	gu.st.taskCursor = 0
	if err := gu.taskResume(nil, nil); err != nil {
		t.Fatalf("taskResume: %v", err)
	}
	if gu.st.popup.StepCursor != 1 {
		t.Fatalf("setup: StepCursor=%d want 1 (current step = ship at index 1)", gu.st.popup.StepCursor)
	}
	if err := gu.enrollPickerStepAccept(nil, nil); err != nil {
		t.Fatalf("step accept: %v", err)
	}
	if !fake.resumeCalled {
		t.Fatalf("Datasource.Resume was not dispatched")
	}
	if fake.resumeID != "ask-rrrrrr" {
		t.Errorf("resume id=%q want ask-rrrrrr", fake.resumeID)
	}
	if fake.resumeStep != "" {
		t.Errorf("resume step=%q want \"\" (no-bump path: Enter on the current step must NOT pass --to STEP, otherwise step_visits[ship]++ and max_visits could fire on a task that hit human via the cap)", fake.resumeStep)
	}
}

// TestResumeCurrentStepName pins the small helper used by taskResume
// to detect the no-bump branch (review R7).
func TestResumeCurrentStepName(t *testing.T) {
	wf := datasource.Workflow{
		ID: "wf",
		Steps: []datasource.WorkflowStep{
			{ID: "s1", Name: "dev"},
			{ID: "s2", Name: "review"},
		},
	}
	if got := resumeCurrentStepName(wf, "s2"); got != "review" {
		t.Errorf("match: got %q want review", got)
	}
	if got := resumeCurrentStepName(wf, ""); got != "" {
		t.Errorf("empty: got %q want empty", got)
	}
	if got := resumeCurrentStepName(wf, "missing"); got != "" {
		t.Errorf("stale id: got %q want empty (no false positive)", got)
	}
}

// TestResumePickerStepEscape_ClosesPopup pins the back-step
// behaviour on the resume flow: WorkflowLocked=true means Esc on
// the step pane closes the picker (there is no workflow pane to
// return to).
func TestResumePickerStepEscape_ClosesPopup(t *testing.T) {
	gu, fake := newPickerTestGui(t)
	_, b, _ := seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-rrrrrr", WorkflowID: b.ID, CurrentStepID: "st-b-ship"}}
	gu.st.taskCursor = 0
	if err := gu.taskResume(nil, nil); err != nil {
		t.Fatalf("taskResume: %v", err)
	}
	if err := gu.enrollPickerStepEscape(nil, nil); err != nil {
		t.Fatalf("step escape: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("resume + Esc-on-step must close the popup; got kind=%v", k)
	}
	if fake.resumeCalled {
		t.Errorf("Esc must NOT dispatch Resume")
	}
}

// TestTaskResume_NoWorkflowFlashes pins the stale-cache / no-workflow
// branch: pressing `r` on a task whose WorkflowID is empty (or not
// in the cached workflows slice) flashes a hint and does NOT open
// the popup.
func TestTaskResume_NoWorkflowFlashes(t *testing.T) {
	gu, fake := newPickerTestGui(t)
	seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-orphaned"}} // no WorkflowID
	gu.st.taskCursor = 0

	if err := gu.taskResume(nil, nil); err != nil {
		t.Fatalf("taskResume: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("resume must NOT open the picker when the task has no workflow; got kind=%v", k)
	}
	if gu.st.flash.Text == "" {
		t.Errorf("expected a flash message")
	}
	if fake.resumeCalled {
		t.Errorf("Resume must NOT be dispatched on the no-workflow branch")
	}
}

// TestTaskResume_WorkflowMissingFromCacheFlashes pins the
// stale-cache branch: the task carries a WorkflowID that isn't in
// the cached workflows slice (perhaps a recent CreateWorkflow that
// the refresh tick hasn't picked up yet, or a hand-edit). The
// resume picker must not mount with an empty workflow list; it
// flashes and bails.
func TestTaskResume_WorkflowMissingFromCacheFlashes(t *testing.T) {
	gu, fake := newPickerTestGui(t)
	seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-stale", WorkflowID: "wf-unknown"}}
	gu.st.taskCursor = 0

	if err := gu.taskResume(nil, nil); err != nil {
		t.Fatalf("taskResume: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("resume must NOT open when the cached slice doesn't carry the task's workflow; got kind=%v", k)
	}
	if gu.st.flash.Text == "" || !strings.Contains(gu.st.flash.Text, "not loaded") {
		t.Errorf("flash text = %q, want a 'not loaded' hint", gu.st.flash.Text)
	}
	if fake.resumeCalled {
		t.Errorf("Resume must NOT be dispatched on the stale-cache branch")
	}
}

// TestArrangementHasEnrollPicker pins that both winEnrollWorkflowList
// and winEnrollStepList are registered in allPopupWindows so
// layoutPopup's garbage-collect pass can clean them up when the
// popup closes. Mirrors TestArrangementHasSingleCompose's role.
func TestArrangementHasEnrollPicker(t *testing.T) {
	for _, name := range []string{winEnrollWorkflowList, winEnrollStepList} {
		found := false
		for _, w := range allPopupWindows {
			if w == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s missing from allPopupWindows: %v", name, allPopupWindows)
		}
	}
}

// TestLayoutEnrollPickerIsolation pins the popup-isolation invariant:
// while popupEnroll is active, no other popup view (menu, confirm,
// prompt, task compose, single compose) is left on screen.
func TestLayoutEnrollPickerIsolation(t *testing.T) {
	gu, _ := newPickerTestGui(t)
	seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa"}}
	gu.st.taskCursor = 0

	// Open the menu first so a menu view exists, then switch to the
	// enroll picker: the menu view must be cleaned up by the layout
	// pass.
	gu.openMenu("pick", []string{"a", "b"}, func(int) error { return nil })
	gu.layoutPopup(gu.g, 120, 40)
	if _, err := gu.g.View(winPopupMenu); err != nil {
		t.Fatalf("menu view missing after first layout: %v", err)
	}

	gu.st.withLock(func() { gu.st.popup = popupState{} })
	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	gu.layoutPopup(gu.g, 120, 40)

	for _, name := range []string{
		winPopupMenu, winPopupConfirm, winPopupPrompt,
		winTaskComposeSummary, winTaskComposeDescription,
		winSingleCompose,
	} {
		if _, err := gu.g.View(name); err == nil {
			t.Errorf("popup view %q must be deleted while popupEnroll is active", name)
		}
	}
	for _, name := range []string{winEnrollWorkflowList, winEnrollStepList} {
		if _, err := gu.g.View(name); err != nil {
			t.Errorf("enroll-picker view %q should exist while popupEnroll is active: %v", name, err)
		}
	}
}

// TestLayoutEnrollPickerTitlePlacement pins review R9: on the
// enroll flow the action title rides the workflow pane (the
// operator is acting on it); on the resume flow it rides the step
// pane and the workflow pane shows the inert "Workflow (locked)"
// label. Without this routing the resume flow would land the
// "Resume — pick step" title on the workflow pane, which is
// non-navigable, and the step pane would carry only the generic
// "Step" header.
func TestLayoutEnrollPickerTitlePlacement(t *testing.T) {
	t.Run("enroll", func(t *testing.T) {
		gu, _ := newPickerTestGui(t)
		seedPickerWorkflows(gu)
		gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa"}}
		gu.st.taskCursor = 0
		if err := gu.taskEnroll(nil, nil); err != nil {
			t.Fatalf("taskEnroll: %v", err)
		}
		gu.layoutEnrollPicker(gu.g, 120, 40)
		wfV, err := gu.g.View(winEnrollWorkflowList)
		if err != nil {
			t.Fatalf("workflow view: %v", err)
		}
		stV, err := gu.g.View(winEnrollStepList)
		if err != nil {
			t.Fatalf("step view: %v", err)
		}
		if !strings.Contains(wfV.Title, "pick workflow") {
			t.Errorf("enroll workflow title = %q, want it to carry the action hint", wfV.Title)
		}
		if strings.Contains(wfV.Title, "locked") {
			t.Errorf("enroll workflow title = %q, must NOT include `locked` (enroll allows navigation)", wfV.Title)
		}
		if stV.Title != "Step" {
			t.Errorf("enroll step title = %q, want generic \"Step\" (action sits on the workflow pane)", stV.Title)
		}
	})
	t.Run("resume", func(t *testing.T) {
		gu, _ := newPickerTestGui(t)
		_, b, _ := seedPickerWorkflows(gu)
		gu.st.tasks = []datasource.Task{{ID: "ask-rrrrrr", WorkflowID: b.ID, CurrentStepID: "st-b-ship"}}
		gu.st.taskCursor = 0
		if err := gu.taskResume(nil, nil); err != nil {
			t.Fatalf("taskResume: %v", err)
		}
		gu.layoutEnrollPicker(gu.g, 120, 40)
		wfV, err := gu.g.View(winEnrollWorkflowList)
		if err != nil {
			t.Fatalf("workflow view: %v", err)
		}
		stV, err := gu.g.View(winEnrollStepList)
		if err != nil {
			t.Fatalf("step view: %v", err)
		}
		if wfV.Title != "Workflow (locked)" {
			t.Errorf("resume workflow title = %q, want \"Workflow (locked)\" (action sits on the step pane)", wfV.Title)
		}
		if !strings.Contains(stV.Title, "pick step") {
			t.Errorf("resume step title = %q, want it to carry the action hint", stV.Title)
		}
	})
}

// TestEnrollPickerCursor_BoundsDefensive pins review R6: even when
// popup state is mutated past the helpers (an OnPick that re-opens
// the popup with a stale cursor; a future programmatic test path;
// etc.) the cursor handlers must not panic on an out-of-range
// WorkflowCursor. Symmetric with the StepCursor guard the dispatch
// already carries.
func TestEnrollPickerCursor_BoundsDefensive(t *testing.T) {
	gu, fake := newPickerTestGui(t)
	seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa"}}
	gu.st.taskCursor = 0
	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	// Force an out-of-range WorkflowCursor and a focus on the step
	// pane (so the step-pane arm of enrollPickerCursor runs).
	gu.st.withLock(func() {
		gu.st.popup.WorkflowCursor = 99
		gu.st.popup.ActivePane = pickerPaneStep
	})

	// Cursor move on the step pane: must not panic, must not move.
	move := gu.enrollPickerCursor(pickerPaneStep, +1)
	if err := move(nil, nil); err != nil {
		t.Fatalf("cursor move: %v", err)
	}
	if gu.st.popup.StepCursor != 0 {
		t.Errorf("StepCursor=%d want 0 (out-of-range workflow cursor must short-circuit)", gu.st.popup.StepCursor)
	}

	// Dispatch path: must not panic, must not call Enroll.
	if err := gu.enrollPickerStepAccept(nil, nil); err != nil {
		t.Fatalf("step accept: %v", err)
	}
	if fake.enrollCalled {
		t.Errorf("Enroll dispatched with out-of-range WorkflowCursor")
	}
}

// TestLayoutEnrollPickerFocusRoutesByPane verifies that the layout
// switches g.currentView between the two views based on
// popupState.ActivePane on every pass — so enrollPickerWorkflowAccept
// (which sets ActivePane = pickerPaneStep) actually changes which
// pane the next keystroke reaches.
func TestLayoutEnrollPickerFocusRoutesByPane(t *testing.T) {
	gu, _ := newPickerTestGui(t)
	seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa"}}
	gu.st.taskCursor = 0
	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	gu.layoutPopup(gu.g, 120, 40)
	if cv := gu.g.CurrentView(); cv == nil || cv.Name() != winEnrollWorkflowList {
		t.Fatalf("initial focus must be workflow pane, got %v", currentViewName(gu.g))
	}

	// Simulate Enter on the workflow pane → focus moves to step pane.
	if err := gu.enrollPickerWorkflowAccept(nil, nil); err != nil {
		t.Fatalf("workflow accept: %v", err)
	}
	gu.layoutPopup(gu.g, 120, 40)
	if cv := gu.g.CurrentView(); cv == nil || cv.Name() != winEnrollStepList {
		t.Fatalf("after Enter on workflow, focus must be step pane, got %v", currentViewName(gu.g))
	}

	// Simulate Esc on the step pane → focus returns to workflow pane.
	if err := gu.enrollPickerStepEscape(nil, nil); err != nil {
		t.Fatalf("step escape: %v", err)
	}
	gu.layoutPopup(gu.g, 120, 40)
	if cv := gu.g.CurrentView(); cv == nil || cv.Name() != winEnrollWorkflowList {
		t.Fatalf("after Esc on step pane, focus must be workflow pane again, got %v", currentViewName(gu.g))
	}
}

// TestEnrollPickerKeysViewScoped pins risk-mitigation #6 for the new
// popup: the per-pane chords (Enter, Esc, j/k, arrow keys, mouse
// wheel) are bound on the right views, and the keys that are NOT
// meant to be reachable globally (Enter, j, k) really aren't — a
// future global Tab-cycler-style handler that grabs Enter would
// silently steal the picker chord otherwise. DeleteKeybinding
// returns nil iff the binding exists at the given (view, key)
// tuple, so we use it to assert both presence on the right views
// and absence on the global slot (review R4).
func TestEnrollPickerKeysViewScoped(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      120,
		Height:     40,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()
	gu := &Gui{g: g, st: newState()}
	if err := gu.bindKeys(); err != nil {
		t.Fatalf("bindKeys: %v", err)
	}

	// Per-pane bindings exist on the right views.
	type binding struct {
		view string
		key  any
	}
	wantBound := []binding{
		{winEnrollWorkflowList, 'j'},
		{winEnrollWorkflowList, 'k'},
		{winEnrollWorkflowList, gocui.KeyArrowDown},
		{winEnrollWorkflowList, gocui.KeyArrowUp},
		{winEnrollWorkflowList, gocui.KeyEnter},
		{winEnrollWorkflowList, gocui.KeyEsc},
		{winEnrollWorkflowList, gocui.MouseWheelDown},
		{winEnrollWorkflowList, gocui.MouseWheelUp},
		{winEnrollStepList, 'j'},
		{winEnrollStepList, 'k'},
		{winEnrollStepList, gocui.KeyArrowDown},
		{winEnrollStepList, gocui.KeyArrowUp},
		{winEnrollStepList, gocui.KeyEnter},
		{winEnrollStepList, gocui.KeyEsc},
		{winEnrollStepList, gocui.MouseWheelDown},
		{winEnrollStepList, gocui.MouseWheelUp},
	}
	for _, b := range wantBound {
		if err := g.DeleteKeybinding(b.view, b.key, gocui.ModNone); err != nil {
			t.Errorf("expected binding %v on %s; DeleteKeybinding returned %v", b.key, b.view, err)
		}
	}

	// Complement (review R4): the picker-only chords (Enter, j, k)
	// must NOT be bound globally. Esc IS globally bound (handleEsc)
	// and gocui's per-view binding shadows it, so we deliberately
	// exclude it from this assertion. Re-bind the chords first so the
	// previous DeleteKeybinding loop hasn't already cleared the
	// per-view bindings we want to verify aren't globally bound —
	// DeleteKeybinding("", key) checks the global slot independent of
	// any per-view binding.
	if err := gu.bindKeys(); err != nil {
		t.Fatalf("re-bindKeys: %v", err)
	}
	for _, key := range []any{gocui.KeyEnter, 'j', 'k'} {
		if err := g.DeleteKeybinding("", key, gocui.ModNone); err == nil {
			t.Errorf("%v must NOT be globally bound (would steal the picker chord)", key)
		}
	}
}

// TestFilterPickerWorkflowsDropsSynthetic pins the pure helper used
// by both taskEnroll and the picker test setup: synthetic rows are
// excluded and order is preserved.
func TestFilterPickerWorkflowsDropsSynthetic(t *testing.T) {
	in := []datasource.Workflow{
		{ID: "a", Name: "alpha"},
		{ID: "syn", Name: "single:@autosk/dev", IsSynthetic: true},
		{ID: "b", Name: "beta"},
	}
	got := filterPickerWorkflows(in)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2: %+v", len(got), got)
	}
	if got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Errorf("order not preserved: %+v", got)
	}
}

// TestPickerInitialWorkflowCursor pins the pre-selection helper:
// returns the index when the id matches, 0 otherwise.
func TestPickerInitialWorkflowCursor(t *testing.T) {
	wfs := []datasource.Workflow{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}
	if got := pickerInitialWorkflowCursor(wfs, "b"); got != 1 {
		t.Errorf("match: got %d want 1", got)
	}
	if got := pickerInitialWorkflowCursor(wfs, ""); got != 0 {
		t.Errorf("empty id: got %d want 0", got)
	}
	if got := pickerInitialWorkflowCursor(wfs, "missing"); got != 0 {
		t.Errorf("no match: got %d want 0", got)
	}
	if got := pickerInitialWorkflowCursor(nil, "anything"); got != 0 {
		t.Errorf("nil slice: got %d want 0", got)
	}
}

// TestPickerInitialStepCursor pins the step-cursor seed helper.
func TestPickerInitialStepCursor(t *testing.T) {
	wfs := []datasource.Workflow{{
		ID: "wf",
		Steps: []datasource.WorkflowStep{
			{ID: "s1"}, {ID: "s2"}, {ID: "s3"},
		},
	}}
	if got := pickerInitialStepCursor(wfs, 0, "s2"); got != 1 {
		t.Errorf("match: got %d want 1", got)
	}
	if got := pickerInitialStepCursor(wfs, 0, ""); got != 0 {
		t.Errorf("empty step id: got %d want 0", got)
	}
	if got := pickerInitialStepCursor(wfs, 0, "missing"); got != 0 {
		t.Errorf("no match: got %d want 0", got)
	}
	if got := pickerInitialStepCursor(wfs, -1, "s1"); got != 0 {
		t.Errorf("negative wf cursor: got %d want 0", got)
	}
	if got := pickerInitialStepCursor(wfs, 5, "s1"); got != 0 {
		t.Errorf("out-of-range wf cursor: got %d want 0", got)
	}
}

// TestEnrollPickerWorkflowLocked_CursorMovesNoOp pins the locked
// workflow pane: j/k on the workflow pane must NOT move the cursor
// when WorkflowLocked=true (the resume flow's single-row pane has
// nothing to navigate to).
func TestEnrollPickerWorkflowLocked_CursorMovesNoOp(t *testing.T) {
	gu, _ := newPickerTestGui(t)
	_, b, _ := seedPickerWorkflows(gu)
	gu.st.tasks = []datasource.Task{{ID: "ask-rrrrrr", WorkflowID: b.ID}}
	gu.st.taskCursor = 0
	if err := gu.taskResume(nil, nil); err != nil {
		t.Fatalf("taskResume: %v", err)
	}

	before := gu.st.popup.WorkflowCursor
	move := gu.enrollPickerCursor(pickerPaneWorkflow, +1)
	if err := move(nil, nil); err != nil {
		t.Fatalf("cursor move: %v", err)
	}
	if gu.st.popup.WorkflowCursor != before {
		t.Errorf("WorkflowLocked cursor moved: was %d now %d", before, gu.st.popup.WorkflowCursor)
	}
}

// TestEnrollPickerHeight pins the panel-height math: at least the
// max of (workflow row count, step row count) plus frame, floor
// independent of input. The screen-aware cap (3/4 of the terminal)
// and the floor (>= 9) are applied externally in layoutEnrollPicker
// (review R1), so this helper carries no termH parameter.
func TestEnrollPickerHeight(t *testing.T) {
	wfs := []datasource.Workflow{
		{ID: "a", Steps: []datasource.WorkflowStep{{ID: "s1"}, {ID: "s2"}, {ID: "s3"}}},
		{ID: "b", Steps: []datasource.WorkflowStep{{ID: "s1"}}},
	}
	// 2 workflow rows, 3 step rows on the focused workflow → max=3 → +2 frame = 5.
	if got := enrollPickerHeight(wfs, 0); got != 5 {
		t.Errorf("got %d want 5 (cursor on a; max=3)", got)
	}
	// Cursor on b: 2 workflow rows vs 1 step row → max=2 → +2 frame = 4.
	if got := enrollPickerHeight(wfs, 1); got != 4 {
		t.Errorf("got %d want 4 (cursor on b; max=2)", got)
	}
	// Empty workflow slice: both columns get at least 1 → 1 + 2 = 3.
	if got := enrollPickerHeight(nil, 0); got != 3 {
		t.Errorf("got %d want 3 on empty input", got)
	}
}

// TestLayoutEnrollPickerNarrowTerminal pins the screen-aware
// sizing invariants on small terminals (review R2): the picker
// must never paint outside the screen, even when the floors
// (panelHeight>=9) would otherwise overflow the cap (termH*3/4),
// and the workflow / step sub-panes must stay inside the
// composePanelWidth ceiling on narrow widths.
func TestLayoutEnrollPickerNarrowTerminal(t *testing.T) {
	cases := []struct {
		name         string
		termW, termH int
	}{
		// Very short terminal: floor would push past the cap.
		{"shortHeight", 120, 10},
		// Very narrow terminal: composePanelWidth gives termW-2.
		{"narrowWidth", 30, 40},
		// Tiny terminal: both axes constrained.
		{"tinyBoth", 22, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := gocui.NewGui(gocui.NewGuiOpts{
				OutputMode: gocui.OutputNormal,
				Headless:   true,
				Width:      tc.termW,
				Height:     tc.termH,
			})
			if err != nil {
				t.Fatalf("gocui new: %v", err)
			}
			defer g.Close()
			gu := &Gui{g: g, st: newState(), ctx: context.Background()}
			gu.ds = &enrollFakeDS{}
			gu.dispatch = func(f func()) { f() }
			seedPickerWorkflows(gu)
			gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa"}}
			gu.st.taskCursor = 0
			if err := gu.taskEnroll(nil, nil); err != nil {
				t.Fatalf("taskEnroll: %v", err)
			}
			gu.layoutEnrollPicker(g, tc.termW, tc.termH)

			for _, name := range []string{winEnrollWorkflowList, winEnrollStepList} {
				v, err := g.View(name)
				if err != nil {
					t.Fatalf("view %s missing: %v", name, err)
				}
				x0, y0, x1, y1 := v.Dimensions()
				// gocui's SetView accepts negative coords on tiny
				// terminals; the invariant we care about is that
				// the bottom-right corner stays INSIDE the screen
				// so nothing paints past the visible area.
				if x1 >= tc.termW {
					t.Errorf("%s x1=%d >= termW=%d (popup overflows on the right)", name, x1, tc.termW)
				}
				if y1 >= tc.termH {
					t.Errorf("%s y1=%d >= termH=%d (popup overflows at the bottom)", name, y1, tc.termH)
				}
				if x0 < 0 {
					t.Errorf("%s x0=%d < 0 (popup starts off-screen on the left)", name, x0)
				}
				if y0 < 0 {
					t.Errorf("%s y0=%d < 0 (popup starts off-screen at the top)", name, y0)
				}
			}
		})
	}
}
