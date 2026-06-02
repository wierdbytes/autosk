package tui

import (
	"context"
	"testing"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
)

// fakeEditDS is a refreshFakeDS extension that records every
// UpdateTitleDescription call so the test can assert the args.
type fakeEditDS struct {
	refreshFakeDS
	gotID, gotTitle, gotDesc string
	calls                    int
	err                      error
}

func (f *fakeEditDS) UpdateTitleDescription(_ context.Context, id, title, desc string) error {
	f.calls++
	f.gotID, f.gotTitle, f.gotDesc = id, title, desc
	return f.err
}

// TestTaskEditOpensComposePrefilled exercises the `c` happy path:
// taskEdit opens the two-pane compose popup with the selected task's
// title and description already in popup state. The first layout
// pass after that point seeds the TextArea from those initial
// values — covered by the popup_compose_test.go layout test, so
// here we pin the state shape instead.
func (*fakeEditDS) SyncWorkflows(_ context.Context, _, _ bool) (datasource.SyncReport, error) { return datasource.SyncReport{}, nil }
func TestTaskEditOpensComposePrefilled(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.tasks = []datasource.Task{{
		ID: "ask-aaaaaa", Title: "old title", Description: "old body",
	}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks

	if err := gu.taskEdit(nil, nil); err != nil {
		t.Fatalf("taskEdit: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupTaskCompose {
		t.Fatalf("popup kind = %v, want popupTaskCompose", k)
	}
	if gu.st.popup.Summary != "old title" {
		t.Errorf("summary seed = %q, want %q", gu.st.popup.Summary, "old title")
	}
	if gu.st.popup.Description != "old body" {
		t.Errorf("description seed = %q, want %q", gu.st.popup.Description, "old body")
	}
	if gu.st.popup.OnComposeAccept == nil {
		t.Fatalf("OnComposeAccept not recorded")
	}
	if title := gu.st.popup.Title; title != "Edit ask-aaaaaa" {
		t.Errorf("title = %q, want %q", title, "Edit ask-aaaaaa")
	}
}

// TestTaskEditNoOpWithoutSelection: with no selected task, taskEdit
// must NOT open the compose popup.
func TestTaskEditNoOpWithoutSelection(t *testing.T) {
	gu := &Gui{st: newState()}
	if err := gu.taskEdit(nil, nil); err != nil {
		t.Fatalf("taskEdit: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("no-selection path opened popup: kind=%v", k)
	}
}

// TestTaskEditHappyPathCallsDatasource drives the full edit path:
// open compose via `c`, type into both panes, Ctrl+S submits. The
// submit-path's worker body is routed through gu.runDispatch (so a
// test can substitute a synchronous dispatcher) — that's the only
// reason this test can assert UpdateTitleDescription was called
// without driving a real gocui MainLoop.
func TestTaskEditHappyPathCallsDatasource(t *testing.T) {
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

	ds := &fakeEditDS{}
	gu := &Gui{g: g, st: newState(), ds: ds, ctx: context.Background()}
	// Synchronous dispatcher: lets us observe the worker side-effect
	// inside the test without a MainLoop. Production keeps the
	// default OnWorker path.
	gu.dispatch = func(f func()) { f() }
	gu.st.tasks = []datasource.Task{{ID: "ask-bbbbbb", Title: "x", Description: "y"}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks

	_ = gu.taskEdit(nil, nil)
	gu.layoutPopup(g, 120, 40)

	summV, _ := g.View(winTaskComposeSummary)
	descV, _ := g.View(winTaskComposeDescription)
	// Replace the seed content wholesale.
	summV.TextArea.Clear()
	descV.TextArea.Clear()
	summV.TextArea.TypeString("new title")
	descV.TextArea.TypeString("new\nbody")

	if err := gu.taskComposeConfirm(nil, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("popup not cleared on Ctrl+S: kind=%v", k)
	}

	// With the synchronous dispatcher in place the accept callback
	// has already run UpdateTitleDescription by the time Confirm
	// returns. Assert on the observable side-effect of the real
	// integration path.
	if ds.calls != 1 {
		t.Fatalf("UpdateTitleDescription called %d times, want 1", ds.calls)
	}
	if ds.gotID != "ask-bbbbbb" || ds.gotTitle != "new title" || ds.gotDesc != "new\nbody" {
		t.Errorf("got (%q, %q, %q), want (ask-bbbbbb, new title, new\\nbody)",
			ds.gotID, ds.gotTitle, ds.gotDesc)
	}
	if gu.st.flash.Level != "info" {
		t.Errorf("flash level = %q, want info; flash=%+v", gu.st.flash.Level, gu.st.flash)
	}
}

// TestTaskEditEmptyTitleReopensPopup pins the validation branch of
// openTaskComposeForEdit's accept callback: an empty (whitespace-
// only) title MUST NOT call UpdateTitleDescription and MUST leave
// the popup re-opened so the user can fix the title without losing
// their work. The re-open also has to preserve the typed
// description (and the whitespace-only summary) so we explicitly
// assert popup.Summary / popup.Description survived.
func TestTaskEditEmptyTitleReopensPopup(t *testing.T) {
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

	ds := &fakeEditDS{}
	gu := &Gui{g: g, st: newState(), ds: ds, ctx: context.Background()}
	gu.dispatch = func(f func()) { f() }
	gu.st.tasks = []datasource.Task{{ID: "ask-cccccc", Title: "x", Description: "y"}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks

	// Drive the accept callback directly: emulate Ctrl+S with an
	// empty summary. The popup must be re-opened.
	gu.openTaskComposeForEdit("ask-cccccc", "x", "y")
	gu.layoutPopup(g, 120, 40)

	summV, _ := g.View(winTaskComposeSummary)
	descV, _ := g.View(winTaskComposeDescription)
	summV.TextArea.Clear()
	descV.TextArea.Clear()
	// Whitespace-only summary, retained description.
	summV.TextArea.TypeString("   ")
	descV.TextArea.TypeString("kept body")

	if err := gu.taskComposeConfirm(nil, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	if k := gu.st.popup.Kind; k != popupTaskCompose {
		t.Errorf("popup must be re-opened after empty-title submit; kind=%v", k)
	}
	// Text preservation is the whole point of the re-open path: if a
	// future refactor drops the (s, d) round-trip and re-seeds with
	// empty values, the user silently loses their typed body. Pin
	// the contract here.
	if gu.st.popup.Summary != "   " {
		t.Errorf("summary lost on re-open: got %q, want %q", gu.st.popup.Summary, "   ")
	}
	if gu.st.popup.Description != "kept body" {
		t.Errorf("description lost on re-open: got %q, want %q", gu.st.popup.Description, "kept body")
	}
	if ds.calls != 0 {
		t.Errorf("UpdateTitleDescription called %d times on empty-title submit, want 0", ds.calls)
	}
	if gu.st.flash.Text == "" || gu.st.flash.Level != "err" {
		t.Errorf("expected error flash, got %+v", gu.st.flash)
	}
}
