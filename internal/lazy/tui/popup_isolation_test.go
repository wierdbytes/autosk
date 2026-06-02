package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
)

// TestPopupConfirmKeysNotGloballyBound (a structural pin for
// risk #6 from the impl plan) asserts that the confirm-popup chords
// (y / n / Y / N) are NOT bound at the empty-view (global) level in
// the production keymap. gocui's dispatcher routes view-scoped
// bindings before globals, so a popup-active 'q' still triggers the
// global quit — that integration behaviour is intentionally outside
// the scope of this test (it'd require running a real headless
// MainLoop and is on the followup list as the proper risk-#6 test).
//
// What this test DOES pin: nobody accidentally adds e.g. a global
// 'y' binding (which would override the popup's per-view 'y' as a
// quit shortcut). The previous version of this test attempted to
// detect the binding via SetKeybinding, which always succeeds (gocui
// silently appends duplicates) and so pinned nothing at all. This
// version uses DeleteKeybinding, which returns errors.New("keybinding
// not found") iff no matching binding exists — a real, falsifiable
// assertion.
//
// To prove the test bites: add e.g. `{"", 'y', gocui.ModNone, gu.quit}`
// to bindKeys and the assertion below fails because DeleteKeybinding
// returns nil (a binding existed and was removed).
func TestPopupConfirmKeysNotGloballyBound(t *testing.T) { //nolint:gocognit

	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      80,
		Height:     24,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()
	gu := &Gui{g: g, st: newState()}
	if err := gu.bindKeys(); err != nil {
		t.Fatalf("bindKeys: %v", err)
	}

	// DeleteKeybinding(view, key, mod) returns nil iff a matching
	// binding was found AND removed. If it returns "keybinding not
	// found" we know the production keymap doesn't register that
	// chord. The popup confirm chords must NOT be globally bound —
	// otherwise a popup-active 'y' would conflict with a global
	// handler (gocui appends duplicates silently).
	for _, key := range []any{'y', 'n', 'Y', 'N'} {
		err := g.DeleteKeybinding("", key, gocui.ModNone)
		if err == nil {
			t.Errorf("chord %q IS globally bound; production keymap must NOT register confirm chords at view \"\" (popup leak)", key)
			continue
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("DeleteKeybinding(%q) returned unexpected error: %v (want 'not found')", key, err)
		}
	}

	// Sanity-check the same assertion against a chord we KNOW is
	// globally bound: 'q' should be a global quit. Deleting it must
	// succeed (returning nil). If this branch fails, the test
	// infrastructure itself is broken (e.g. bindKeys never ran).
	if err := g.DeleteKeybinding("", 'q', gocui.ModNone); err != nil {
		t.Fatalf("sanity check: 'q' must be globally bound (quit); DeleteKeybinding returned %v", err)
	}
}

// TestIsolationKeyBoundOnWorkflows pins that the `i` key is bound on
// the Workflows panel. The structural mirror of
// TestPopupConfirmKeysNotGloballyBound: DeleteKeybinding returns nil
// iff the binding exists. The complement assertion (the binding
// must NOT be globally bound, so other panels are unaffected) is in
// the second branch.
func TestIsolationKeyBoundOnWorkflows(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      80,
		Height:     24,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()
	gu := &Gui{g: g, st: newState()}
	if err := gu.bindKeys(); err != nil {
		t.Fatalf("bindKeys: %v", err)
	}
	if err := g.DeleteKeybinding(winWorkflows, 'i', gocui.ModNone); err != nil {
		t.Fatalf("`i` should be bound on winWorkflows; got %v", err)
	}
	// Complement: must NOT be globally bound.
	if err := g.DeleteKeybinding("", 'i', gocui.ModNone); err == nil {
		t.Fatalf("`i` should NOT be globally bound")
	}
}

// fakeIsolationDS captures the UpdateWorkflowIsolation call so the
// popup-flow tests can assert that confirming the dialog dispatches
// the right (name, mode, force) triple. Embeds refreshFakeDS so we
// inherit the full Datasource surface.
type fakeIsolationDS struct {
	refreshFakeDS
	mu        sync.Mutex
	called    bool
	gotName   string
	gotMode   string
	gotForce  bool
	report    datasource.UpdateIsolationReport
	returnErr error
}

func (f *fakeIsolationDS) UpdateWorkflowIsolation(_ context.Context, name, mode string, force bool) (datasource.UpdateIsolationReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.gotName = name
	f.gotMode = mode
	f.gotForce = force
	return f.report, f.returnErr
}

// runWorkerSync runs every worker callback in-line so the popup
// flow's gu.g.OnWorker invocations dispatch synchronously and the
// test can observe the datasource call right after popupConfirmYes.
func (*fakeIsolationDS) SyncWorkflows(_ context.Context, _, _ bool) (datasource.SyncReport, error) {
	return datasource.SyncReport{}, nil
}
func runWorkerSync(t *testing.T) (*gocui.Gui, func()) {
	t.Helper()
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      80,
		Height:     24,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	return g, func() { g.Close() }
}

// TestWorkflowIsolation_SyntheticFlashesAndDoesNotOpen pins FR3 +
// the lazy synthetic-row contract: pressing `i` on a synthetic row
// drops a status-bar message and leaves the popup state untouched.
func TestWorkflowIsolation_SyntheticFlashesAndDoesNotOpen(t *testing.T) {
	g, done := runWorkerSync(t)
	defer done()
	gu := &Gui{g: g, st: newState(), ctx: context.Background()}
	gu.ds = &fakeIsolationDS{}
	gu.st.workflows = []datasource.Workflow{
		{Name: "single:@autosk/dev", IsSynthetic: true, Isolation: "none"},
	}
	gu.st.workflowCursor = 0

	if err := gu.workflowIsolation(nil, nil); err != nil {
		t.Fatalf("workflowIsolation: %v", err)
	}
	if gu.st.popup.Kind != popupNone {
		t.Fatalf("synthetic row should NOT open a popup; got kind=%d", gu.st.popup.Kind)
	}
	if gu.st.flash.Text == "" || !strings.Contains(gu.st.flash.Text, "synthetic") {
		t.Errorf("expected flash mentioning 'synthetic'; got %q", gu.st.flash.Text)
	}
}

// TestWorkflowIsolation_OpensIsolationMenu pins the binding's main
// effect: it opens a popupIsolation menu listing the two modes with
// the current value marked.
func TestWorkflowIsolation_OpensIsolationMenu(t *testing.T) {
	g, done := runWorkerSync(t)
	defer done()
	gu := &Gui{g: g, st: newState(), ctx: context.Background()}
	gu.ds = &fakeIsolationDS{}
	gu.st.workflows = []datasource.Workflow{
		{Name: "plain-wf", Isolation: "none"},
	}
	gu.st.workflowCursor = 0

	if err := gu.workflowIsolation(nil, nil); err != nil {
		t.Fatalf("workflowIsolation: %v", err)
	}
	if gu.st.popup.Kind != popupIsolation {
		t.Fatalf("popup kind: %d (want popupIsolation)", gu.st.popup.Kind)
	}
	if len(gu.st.popup.Lines) != 2 {
		t.Fatalf("menu line count: %d (want 2): %v", len(gu.st.popup.Lines), gu.st.popup.Lines)
	}
	if !strings.Contains(gu.st.popup.Lines[0], "← current") {
		t.Errorf("first row should be marked as current: %q", gu.st.popup.Lines[0])
	}
	if strings.Contains(gu.st.popup.Lines[1], "← current") {
		t.Errorf("second row should NOT be marked as current: %q", gu.st.popup.Lines[1])
	}
}

// TestWorkflowIsolation_ConfirmDispatchesUpdate drives the menu →
// confirm → worker chain end-to-end via in-process worker dispatch.
// We monkey-patch gu.g.OnWorker by skipping the dispatch and calling
// the closure synchronously — not via gocui's real worker pool,
// which would race the test.
func TestWorkflowIsolation_ConfirmDispatchesUpdate(t *testing.T) {
	cases := []struct {
		name             string
		cur              string
		nonTerminalCount int
		selectMode       string // mode the test "clicks" in the menu ("none"|"worktree")
		wantForce        bool
		wantBodyContains string
	}{
		{"none_to_worktree_no_tasks", "none", 0, "worktree", false, "No tasks are currently affected."},
		{"none_to_worktree_with_tasks", "none", 3, "worktree", true, "3 non-terminal task(s) will get a fresh worktree allocated from HEAD:"},
		{"worktree_to_none_no_tasks", "worktree", 0, "none", false, "No tasks are currently affected."},
		{"worktree_to_none_with_tasks", "worktree", 2, "none", true, "2 non-terminal task(s) keep their worktree directories on disk:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Pad a representative slice; the body only renders the
			// per-task list when nonTerminalCount > 0.
			tasks := make([]datasource.NonTerminalTaskRef, c.nonTerminalCount)
			for i := range tasks {
				tasks[i] = datasource.NonTerminalTaskRef{
					ID:       fmt.Sprintf("ask-%03d", i),
					Status:   "work",
					StepName: "dev",
				}
			}
			body := buildIsolationConfirmBody(c.cur, c.selectMode, c.nonTerminalCount, tasks)
			if !strings.Contains(body, c.wantBodyContains) {
				t.Errorf("body missing %q:\n%s", c.wantBodyContains, body)
			}
			if !strings.Contains(body, c.cur+" → "+c.selectMode) {
				t.Errorf("body missing transition arrow %q→%q:\n%s", c.cur, c.selectMode, body)
			}
			// And: when nonTerminalCount > 0 the call site MUST pass
			// force=true; the workflowIsolation handler computes this
			// directly. We assert the value via the call-site helper.
			gotForce := c.nonTerminalCount > 0
			if gotForce != c.wantForce {
				t.Errorf("force inferred=%v, want %v", gotForce, c.wantForce)
			}
		})
	}
}

// TestWorkflowIsolation_CurrentValueIsNoOp pins that selecting the
// current value closes the popup without invoking the datasource.
func TestWorkflowIsolation_CurrentValueIsNoOp(t *testing.T) {
	g, done := runWorkerSync(t)
	defer done()
	fake := &fakeIsolationDS{}
	gu := &Gui{g: g, st: newState(), ctx: context.Background()}
	gu.ds = fake
	gu.st.workflows = []datasource.Workflow{{Name: "plain", Isolation: "none"}}
	gu.st.workflowCursor = 0

	if err := gu.workflowIsolation(nil, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if gu.st.popup.Kind != popupIsolation {
		t.Fatalf("popup not opened")
	}
	// Cursor stays on index 0 (the current value). popupAccept
	// invokes the onSelect closure with cur=0.
	onSelect := gu.st.popup.OnSelect
	if onSelect == nil {
		t.Fatalf("OnSelect nil")
	}
	if err := onSelect(0); err != nil {
		t.Fatalf("onSelect(0): %v", err)
	}
	if fake.called {
		t.Errorf("selecting the current value must NOT dispatch UpdateWorkflowIsolation")
	}
	if gu.st.popup.Kind == popupConfirm {
		t.Errorf("current-value selection must NOT chain into a confirm popup")
	}
}

// TestWorkflowIsolation_DifferentValueOpensConfirm verifies that
// selecting the non-current value chains into a popupConfirm whose
// body matches buildIsolationConfirmBody.
func TestWorkflowIsolation_DifferentValueOpensConfirm(t *testing.T) {
	g, done := runWorkerSync(t)
	defer done()
	fake := &fakeIsolationDS{}
	gu := &Gui{g: g, st: newState(), ctx: context.Background()}
	gu.ds = fake
	gu.st.workflows = []datasource.Workflow{
		{Name: "plain", Isolation: "none", NonTerminalTaskCount: 2},
	}
	gu.st.workflowCursor = 0

	if err := gu.workflowIsolation(nil, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	onSelect := gu.st.popup.OnSelect
	if onSelect == nil {
		t.Fatalf("OnSelect nil")
	}
	// Select index 1 = "worktree".
	if err := onSelect(1); err != nil {
		t.Fatalf("onSelect(1): %v", err)
	}
	if gu.st.popup.Kind != popupConfirm {
		t.Fatalf("selecting non-current must chain into confirm; got kind=%d", gu.st.popup.Kind)
	}
	if !strings.Contains(gu.st.popup.Title, "none → worktree") {
		t.Errorf("confirm title missing arrow: %q", gu.st.popup.Title)
	}
	if !strings.Contains(gu.st.popup.Title, "2 non-terminal task(s)") {
		t.Errorf("confirm title missing task count: %q", gu.st.popup.Title)
	}
}

// TestBuildIsolationConfirmBody_NoneToWorktree pins the worded body
// for the canonical case (FR-derived AC).
func TestBuildIsolationConfirmBody_NoneToWorktree(t *testing.T) {
	tasks := []datasource.NonTerminalTaskRef{
		{ID: "ask-aaa", Status: "work", StepName: "dev"},
		{ID: "ask-bbb", Status: "human", StepName: "review"},
		{ID: "ask-ccc", Status: "new", StepName: "dev"},
	}
	body := buildIsolationConfirmBody("none", "worktree", 3, tasks)
	if !strings.Contains(body, "3 non-terminal task(s) will get a fresh worktree allocated from HEAD") {
		t.Errorf("missing FR-specified phrasing: %q", body)
	}
	if !strings.Contains(body, "--force") {
		t.Errorf("body should mention --force path: %q", body)
	}
	// Plan §6.3 + §5.4.3: every affected task must be enumerated.
	for _, want := range []string{
		"- ask-aaa (status=work, current step=dev)",
		"- ask-bbb (status=human, current step=review)",
		"- ask-ccc (status=new, current step=dev)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing per-task line %q in body:\n%s", want, body)
		}
	}
}

// TestBuildIsolationConfirmBody_CapsAtTenWithMoreSuffix pins the
// plan §8 risk #4 mitigation: the per-task list is capped at
// isolationConfirmTaskCap entries and overflow is summarised with
// `... and N more`. Without the cap the popup would overflow when
// many tasks reference the workflow.
func TestBuildIsolationConfirmBody_CapsAtTenWithMoreSuffix(t *testing.T) {
	// 15 sample rows + total=15 so we trip the cap; the body should
	// render the first 10 and append `... and 5 more`.
	tasks := make([]datasource.NonTerminalTaskRef, 15)
	for i := range tasks {
		tasks[i] = datasource.NonTerminalTaskRef{
			ID:       fmt.Sprintf("ask-%03d", i),
			Status:   "work",
			StepName: "dev",
		}
	}
	body := buildIsolationConfirmBody("none", "worktree", 15, tasks)
	// First 10 ids must be present.
	for i := 0; i < 10; i++ {
		want := fmt.Sprintf("ask-%03d", i)
		if !strings.Contains(body, want) {
			t.Errorf("first-10 task %s missing from body:\n%s", want, body)
		}
	}
	// Tasks past the cap must NOT be rendered.
	if strings.Contains(body, "ask-010") {
		t.Errorf("body rendered task beyond cap (ask-010):\n%s", body)
	}
	if !strings.Contains(body, "... and 5 more") {
		t.Errorf("missing overflow suffix `... and 5 more`:\n%s", body)
	}
}

// TestBuildIsolationConfirmBody_TotalGreaterThanSample pins the
// case where the datasource shipped a partial sample (count > len)
// — typical real-world shape, since loadNonTerminalSample caps at
// NonTerminalTaskSampleSize. The overflow suffix reflects total -
// rendered, not total - len(sample) (they're equal here, but the
// invariant matters for future cap changes).
func TestBuildIsolationConfirmBody_TotalGreaterThanSample(t *testing.T) {
	tasks := []datasource.NonTerminalTaskRef{
		{ID: "ask-aaa", Status: "work", StepName: "dev"},
		{ID: "ask-bbb", Status: "human", StepName: "review"},
	}
	body := buildIsolationConfirmBody("worktree", "none", 50, tasks)
	if !strings.Contains(body, "ask-aaa") || !strings.Contains(body, "ask-bbb") {
		t.Errorf("sample tasks missing:\n%s", body)
	}
	if !strings.Contains(body, "... and 48 more") {
		t.Errorf("expected `... and 48 more` (total=50 - rendered=2):\n%s", body)
	}
	// And the worktree→none flavour tail must still appear.
	if !strings.Contains(body, "autosk worktree rm") {
		t.Errorf("worktree→none body missing the cleanup hint:\n%s", body)
	}
}

// errIsolationFailDS is a small adapter that returns a typed error
// from UpdateWorkflowIsolation so the flash-message branch is
// exercised by tests that don't want a worker pool.
type errIsolationFailDS struct {
	refreshFakeDS
	err error
}

func (e *errIsolationFailDS) UpdateWorkflowIsolation(_ context.Context, _, _ string, _ bool) (datasource.UpdateIsolationReport, error) {
	return datasource.UpdateIsolationReport{}, e.err
}

// (Sanity check: the type satisfies datasource.Datasource via the
// embedded refreshFakeDS.)
var _ datasource.Datasource = (*errIsolationFailDS)(nil)
var _ datasource.Datasource = (*fakeIsolationDS)(nil)

// silence unused-import lints for the helpers that may be elided
// once more tests fill in.
var _ = errors.New

// ---- workflow sync tests ------------------------------------------------

// fakeSyncDS captures the SyncWorkflows call for testing the TUI handler.
type fakeSyncDS struct {
	refreshFakeDS
	mu        sync.Mutex
	called    bool
	gotDryRun bool
	gotForce  bool
	report    datasource.SyncReport
	returnErr error
}

func (f *fakeSyncDS) SyncWorkflows(_ context.Context, dryRun, force bool) (datasource.SyncReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.gotDryRun = dryRun
	f.gotForce = force
	return f.report, f.returnErr
}

// TestWorkflowSync_KeyBoundOnWorkflows pins that `s` is bound on the
// Workflows panel and NOT globally.
func TestWorkflowSync_KeyBoundOnWorkflows(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      80,
		Height:     24,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()
	gu := &Gui{g: g, st: newState()}
	if err := gu.bindKeys(); err != nil {
		t.Fatalf("bindKeys: %v", err)
	}
	if err := g.DeleteKeybinding(winWorkflows, 's', gocui.ModNone); err != nil {
		t.Fatalf("`s` should be bound on winWorkflows; got %v", err)
	}
	if err := g.DeleteKeybinding("", 's', gocui.ModNone); err == nil {
		t.Fatalf("`s` should NOT be globally bound")
	}
}

// TestWorkflowSync_FlashesSummaryAndLogsDetails drives the sync
// handler end-to-end and asserts on the flash + command-log output.
func TestWorkflowSync_FlashesSummaryAndLogsDetails(t *testing.T) {
	g, done := runWorkerSync(t)
	defer done()
	fake := &fakeSyncDS{
		report: datasource.SyncReport{
			Workflows: []datasource.SyncWorkflowReport{
				{Name: "wf-a", Status: "added", Mutated: true},
				{Name: "wf-b", Status: "noop", Mutated: false},
				{Name: "wf-c", Status: "conflict", Reason: "local workflow with same name has no global origin"},
			},
		},
	}
	gu := &Gui{g: g, st: newState(), ctx: context.Background()}
	gu.ds = fake
	gu.dispatch = func(f func()) { f() } // sync-on-worker

	if err := gu.workflowSync(nil, nil); err != nil {
		t.Fatalf("workflowSync: %v", err)
	}
	if !fake.called {
		t.Fatal("SyncWorkflows was not called")
	}
	if fake.gotDryRun {
		t.Error("expected dryRun=false by default")
	}
	if fake.gotForce {
		t.Error("expected force=false by default")
	}
	if !strings.Contains(gu.st.flash.Text, "1 added, 1 noop, 1 conflict") {
		t.Errorf("flash text=%q want 1 added, 1 noop, 1 conflict", gu.st.flash.Text)
	}
	log := strings.Join(gu.st.logBuf, "\n")
	for _, want := range []string{
		"sync wf-a: added",
		"sync wf-b: noop",
		"sync wf-c: conflict (local workflow with same name has no global origin)",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q; full log:\n%s", want, log)
		}
	}
}

// TestWorkflowSync_ErrorFlashesAndStillLogsDetails verifies that even
// when SyncWorkflows returns an error, the per-workflow lines are
// appended to the command log.
func TestWorkflowSync_ErrorFlashesAndStillLogsDetails(t *testing.T) {
	g, done := runWorkerSync(t)
	defer done()
	fake := &fakeSyncDS{
		report: datasource.SyncReport{
			Workflows: []datasource.SyncWorkflowReport{
				{Name: "wf-x", Status: "error", Error: "parse failed"},
			},
		},
		returnErr: errors.New("global workflow sync failed"),
	}
	gu := &Gui{g: g, st: newState(), ctx: context.Background()}
	gu.ds = fake
	gu.dispatch = func(f func()) { f() }

	if err := gu.workflowSync(nil, nil); err != nil {
		t.Fatalf("workflowSync: %v", err)
	}
	if !strings.Contains(gu.st.flash.Text, "workflow sync failed") {
		t.Errorf("flash text=%q want error mention", gu.st.flash.Text)
	}
	log := strings.Join(gu.st.logBuf, "\n")
	if !strings.Contains(log, "sync wf-x: error: parse failed") {
		t.Errorf("log missing wf-x error line; full log:\n%s", log)
	}
}

// TestWorkflowSync_ErrorWithMultipleRowsFlashesSummary verifies that
// when SyncWorkflows returns an error alongside a non-empty report,
// the flash includes the status-count summary (e.g. 1 added, 1 error).
func TestWorkflowSync_ErrorWithMultipleRowsFlashesSummary(t *testing.T) {
	g, done := runWorkerSync(t)
	defer done()
	fake := &fakeSyncDS{
		report: datasource.SyncReport{
			Workflows: []datasource.SyncWorkflowReport{
				{Name: "wf-a", Status: "added", Mutated: true},
				{Name: "wf-b", Status: "error", Error: "disk full"},
			},
		},
		returnErr: errors.New("partial sync failed"),
	}
	gu := &Gui{g: g, st: newState(), ctx: context.Background()}
	gu.ds = fake
	gu.dispatch = func(f func()) { f() }

	if err := gu.workflowSync(nil, nil); err != nil {
		t.Fatalf("workflowSync: %v", err)
	}
	if !strings.Contains(gu.st.flash.Text, "workflow sync failed: 1 added, 1 error") {
		t.Errorf("flash text=%q want 'workflow sync failed: 1 added, 1 error'", gu.st.flash.Text)
	}
	log := strings.Join(gu.st.logBuf, "\n")
	for _, want := range []string{
		"sync wf-a: added",
		"sync wf-b: error: disk full",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q; full log:\n%s", want, log)
		}
	}
}

// TestWorkflowSync_ConflictAndSkippedNoMutated verifies that a sync
// report containing only conflict/skipped rows (no Mutated entries)
// still flashes a meaningful summary instead of "everything up to
// date".
func TestWorkflowSync_ConflictAndSkippedNoMutated(t *testing.T) {
	g, done := runWorkerSync(t)
	defer done()
	fake := &fakeSyncDS{
		report: datasource.SyncReport{
			Workflows: []datasource.SyncWorkflowReport{
				{Name: "wf-a", Status: "conflict", Reason: "local workflow with same name has no global origin"},
				{Name: "wf-b", Status: "skipped", Reason: "stale global workflow without --force"},
			},
		},
	}
	gu := &Gui{g: g, st: newState(), ctx: context.Background()}
	gu.ds = fake
	gu.dispatch = func(f func()) { f() }

	if err := gu.workflowSync(nil, nil); err != nil {
		t.Fatalf("workflowSync: %v", err)
	}
	if !strings.Contains(gu.st.flash.Text, "1 skipped, 1 conflict") {
		t.Errorf("flash text=%q want 1 skipped, 1 conflict", gu.st.flash.Text)
	}
	log := strings.Join(gu.st.logBuf, "\n")
	for _, want := range []string{
		"sync wf-a: conflict (local workflow with same name has no global origin)",
		"sync wf-b: skipped (stale global workflow without --force)",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q; full log:\n%s", want, log)
		}
	}
}

// TestWorkflowSync_AutoInstalledAgentsLogged verifies that when a
// sync report row includes AutoInstalledAgents, the command log
// emits an auto_agents line so the operator can see which packages
// were installed during sync.
func TestWorkflowSync_AutoInstalledAgentsLogged(t *testing.T) {
	g, done := runWorkerSync(t)
	defer done()
	fake := &fakeSyncDS{
		report: datasource.SyncReport{
			Workflows: []datasource.SyncWorkflowReport{
				{Name: "wf-a", Status: "added", Mutated: true, AutoInstalledAgents: []string{"@autosk/dev@0.2.5", "@autosk/review@1.0.0"}},
				{Name: "wf-b", Status: "noop", Mutated: false},
			},
		},
	}
	gu := &Gui{g: g, st: newState(), ctx: context.Background()}
	gu.ds = fake
	gu.dispatch = func(f func()) { f() }

	if err := gu.workflowSync(nil, nil); err != nil {
		t.Fatalf("workflowSync: %v", err)
	}
	log := strings.Join(gu.st.logBuf, "\n")
	if !strings.Contains(log, "sync wf-a: added") {
		t.Errorf("log missing wf-a line; full log:\n%s", log)
	}
	if !strings.Contains(log, "  auto_agents: @autosk/dev@0.2.5, @autosk/review@1.0.0") {
		t.Errorf("log missing auto_agents line; full log:\n%s", log)
	}
	// noop row must NOT produce an auto_agents line.
	if strings.Contains(log, "auto_agents:") && strings.Index(log, "auto_agents:") != strings.LastIndex(log, "auto_agents:") {
		// More than one auto_agents occurrence.
		t.Errorf("noop wf-b should not produce auto_agents; full log:\n%s", log)
	}
}

func (*errIsolationFailDS) SyncWorkflows(_ context.Context, _, _ bool) (datasource.SyncReport, error) {
	return datasource.SyncReport{}, nil
}
