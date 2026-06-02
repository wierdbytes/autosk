package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
)

// fakeMetaDS records SetMetadata calls so the tests can assert the
// parsed map round-tripped through the JSON decoder.
type fakeMetaDS struct {
	refreshFakeDS
	gotID  string
	gotMap map[string]any
	calls  int
	err    error
}

func (f *fakeMetaDS) SetMetadata(_ context.Context, id string, m map[string]any) error {
	f.calls++
	f.gotID = id
	f.gotMap = m
	return f.err
}

// TestTaskMetadataEditEmptyShowsCurlies pins the "empty metadata"
// branch: when t.Metadata is nil or empty the popup is seeded with
// "{}" so the user can type into a non-blank but unambiguous JSON
// object scaffold.
func (*fakeMetaDS) SyncWorkflows(_ context.Context, _, _ bool) (datasource.SyncReport, error) { return datasource.SyncReport{}, nil }
func TestTaskMetadataEditEmptyShowsCurlies(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.tasks = []datasource.Task{{ID: "ask-eeeeee"}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks

	if err := gu.taskMetadataEdit(nil, nil); err != nil {
		t.Fatalf("taskMetadataEdit: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupSingleCompose {
		t.Fatalf("popup kind = %v, want popupSingleCompose", k)
	}
	if gu.st.popup.Input != "{}" {
		t.Errorf("popup seed = %q, want {}", gu.st.popup.Input)
	}
}

// TestTaskMetadataEditPrettyPrints pins the pre-fill format: a
// non-empty metadata map is rendered through json.MarshalIndent
// with 2-space indent. Map keys are sorted alphabetically by Go's
// encoder, so the assertion is deterministic.
func TestTaskMetadataEditPrettyPrints(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.tasks = []datasource.Task{{
		ID: "ask-ffffff",
		Metadata: map[string]any{
			"alpha": "one",
			"beta":  float64(2),
		},
	}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks

	_ = gu.taskMetadataEdit(nil, nil)
	want := "{\n  \"alpha\": \"one\",\n  \"beta\": 2\n}"
	if gu.st.popup.Input != want {
		t.Errorf("popup seed mismatch:\n got %q\nwant %q", gu.st.popup.Input, want)
	}
}

// TestTaskMetadataEditNoOpWithoutSelection asserts the no-selection
// path does NOT open the popup.
func TestTaskMetadataEditNoOpWithoutSelection(t *testing.T) {
	gu := &Gui{st: newState()}
	if err := gu.taskMetadataEdit(nil, nil); err != nil {
		t.Fatalf("taskMetadataEdit: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("no-selection path opened popup: kind=%v", k)
	}
}

// TestTaskMetadataInvalidJSONReopensPopup pins the validation
// branch: invalid JSON MUST NOT call SetMetadata and MUST leave the
// popup re-opened. The flash level must be "err" so the operator
// sees the toast. The re-open MUST also preserve the typed text so
// the user can fix the JSON without re-typing from scratch.
func TestTaskMetadataInvalidJSONReopensPopup(t *testing.T) {
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

	ds := &fakeMetaDS{}
	gu := &Gui{g: g, st: newState(), ds: ds, ctx: context.Background()}
	gu.openSingleComposeForMetadata("ask-aaaaaa", "{not-valid")
	gu.layoutPopup(g, 120, 40)

	v, _ := g.View(winSingleCompose)
	v.TextArea.Clear()
	v.TextArea.TypeString("{this is not valid json")

	if err := gu.singleComposeConfirm(nil, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if ds.calls != 0 {
		t.Errorf("SetMetadata called %d times on invalid JSON, want 0", ds.calls)
	}
	if k := gu.st.popup.Kind; k != popupSingleCompose {
		t.Errorf("popup must be re-opened on invalid JSON; kind=%v", k)
	}
	if !strings.Contains(gu.st.popup.Input, "{this is not valid json") {
		t.Errorf("typed text lost on re-open: input=%q", gu.st.popup.Input)
	}
	if !strings.Contains(gu.st.flash.Text, "invalid JSON") {
		t.Errorf("flash should mention 'invalid JSON': %+v", gu.st.flash)
	}
	if gu.st.flash.Level != "err" {
		t.Errorf("flash level = %q, want err", gu.st.flash.Level)
	}
}

// TestTaskMetadataNonObjectJSONReopensPopup pins acceptance
// criteria #9: valid JSON that isn't an object (array, string,
// number, bool, or `null`) MUST behave the same as invalid JSON.
// json.Unmarshal of an array / string / number / bool into a
// map[string]any returns an error so that path goes through the
// invalid-JSON branch. The `null` literal is the tricky case:
// json.Unmarshal succeeds and leaves the map at nil, so the code
// adds an explicit nil-map guard. Pin both shapes.
func TestTaskMetadataNonObjectJSONReopensPopup(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"array", `["array", "not", "object"]`},
		{"string", `"just a string"`},
		{"number", `42`},
		{"bool", `true`},
		// `null` is the dangerous corner case — without the
		// nil-map guard json.Unmarshal returns no error and
		// SetMetadata(ctx, id, nil) silently wipes metadata.
		{"null", `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

			ds := &fakeMetaDS{}
			gu := &Gui{g: g, st: newState(), ds: ds, ctx: context.Background()}
			gu.openSingleComposeForMetadata("ask-aaaaaa", "{}")
			gu.layoutPopup(g, 120, 40)

			v, _ := g.View(winSingleCompose)
			v.TextArea.Clear()
			v.TextArea.TypeString(tc.body)

			if err := gu.singleComposeConfirm(nil, nil); err != nil {
				t.Fatalf("confirm: %v", err)
			}
			if ds.calls != 0 {
				t.Errorf("SetMetadata called %d times on non-object JSON (%q), want 0", ds.calls, tc.body)
			}
			if k := gu.st.popup.Kind; k != popupSingleCompose {
				t.Errorf("popup must be re-opened on non-object JSON (%q); kind=%v", tc.body, k)
			}
			// Text preservation: the user's typed body must round-
			// trip into popupState.Input on re-open so they can
			// fix the value without losing their work.
			if !strings.Contains(gu.st.popup.Input, tc.body) {
				t.Errorf("typed text lost on re-open: input=%q want contains %q", gu.st.popup.Input, tc.body)
			}
			if !strings.Contains(gu.st.flash.Text, "invalid JSON") {
				t.Errorf("flash should mention 'invalid JSON' for %q: %+v", tc.body, gu.st.flash)
			}
			if gu.st.flash.Level != "err" {
				t.Errorf("flash level = %q for %q, want err", gu.st.flash.Level, tc.body)
			}
		})
	}
}

// TestTaskMetadataEmptyObjectClearsMap pins acceptance criteria #7
// for the special case: submitting `{}` is equivalent to clearing
// metadata, and SetMetadata is called with an empty map. The
// synchronous dispatcher in the test fixture lets us observe the
// worker-body's side-effect inline.
func TestTaskMetadataEmptyObjectClearsMap(t *testing.T) {
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

	ds := &fakeMetaDS{}
	gu := &Gui{g: g, st: newState(), ds: ds, ctx: context.Background()}
	gu.dispatch = func(f func()) { f() }
	gu.openSingleComposeForMetadata("ask-aaaaaa", "{}")
	gu.layoutPopup(g, 120, 40)

	v, _ := g.View(winSingleCompose)
	v.TextArea.Clear()
	v.TextArea.TypeString("{}")

	if err := gu.singleComposeConfirm(nil, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("popup not cleared on valid `{}`: kind=%v", k)
	}
	if gu.st.flash.Level == "err" {
		t.Errorf("flash level = err on valid `{}`: %+v", gu.st.flash)
	}
	// The synchronous dispatcher has already run SetMetadata. Verify
	// the integration: the empty-object branch routes the parsed map
	// (zero-length, non-nil) through to the datasource.
	if ds.calls != 1 {
		t.Fatalf("SetMetadata called %d times, want 1", ds.calls)
	}
	if ds.gotID != "ask-aaaaaa" {
		t.Errorf("id = %q, want ask-aaaaaa", ds.gotID)
	}
	if len(ds.gotMap) != 0 {
		t.Errorf("map = %+v, want empty", ds.gotMap)
	}
}

// TestTaskMetadataValidJSONClosesPopup pins acceptance criteria #7
// for the happy path: a valid JSON object closes the popup, sets a
// non-error flash, and the parsed map round-trips through the
// datasource (via the synchronous dispatcher).
func TestTaskMetadataValidJSONClosesPopup(t *testing.T) {
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

	ds := &fakeMetaDS{}
	gu := &Gui{g: g, st: newState(), ds: ds, ctx: context.Background()}
	gu.dispatch = func(f func()) { f() }
	gu.openSingleComposeForMetadata("ask-aaaaaa", "{}")
	gu.layoutPopup(g, 120, 40)

	v, _ := g.View(winSingleCompose)
	v.TextArea.Clear()
	v.TextArea.TypeString(`{"foo":"bar","n":42}`)

	if err := gu.singleComposeConfirm(nil, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("popup not cleared on valid JSON: kind=%v", k)
	}
	if gu.st.flash.Level == "err" {
		t.Errorf("error flash on valid JSON: %+v", gu.st.flash)
	}
	if ds.calls != 1 {
		t.Fatalf("SetMetadata called %d times, want 1", ds.calls)
	}
	if ds.gotID != "ask-aaaaaa" {
		t.Errorf("id = %q, want ask-aaaaaa", ds.gotID)
	}
	if got := ds.gotMap["foo"]; got != "bar" {
		t.Errorf("map[foo] = %v, want bar", got)
	}
	// JSON numbers decode into float64 in map[string]any.
	if got := ds.gotMap["n"]; got != float64(42) {
		t.Errorf("map[n] = %v, want 42 (float64)", got)
	}
	if gu.st.flash.Level != "info" {
		t.Errorf("flash level = %q, want info; flash=%+v", gu.st.flash.Level, gu.st.flash)
	}
}
