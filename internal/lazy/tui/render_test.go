package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"autosk/internal/lazy/ansiutil"
	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
	"autosk/internal/timeformat"
)

// lipglossWidth is the on-screen cell width of s (ANSI-aware).
// Tiny alias around lipgloss.Width so the box-dimensions test reads
// the way the prose narrative talks about "visible width".
func lipglossWidth(s string) int { return lipgloss.Width(s) }

// forceTrueColor pins the lipgloss color profile to TrueColor for
// tests that need to verify styled output. Under `go test` the
// default renderer auto-detects a non-TTY stdout and degrades to
// the Ascii profile (no SGR escapes), which would make every
// "is this string styled?" assertion silently pass against an
// unstyled output. Call from TestMain or via t.Cleanup to scope
// the side effect.
func forceTrueColor(t *testing.T) {
	t.Helper()
	prev := lipgloss.DefaultRenderer().ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	// Styles cache the renderer's profile at creation time, so swap
	// the profile FIRST and then rebuild so the cached styles in
	// this package pick the new profile up.
	RebuildStyles()
	t.Cleanup(func() {
		lipgloss.SetColorProfile(prev)
		RebuildStyles()
	})
}

// stripANSI is defined in refresh_test.go (a thin alias over
// ansiutil.Strip) and shared across render-pane tests so substring
// assertions can ignore SGR escapes layered over the visible text.

// TestTruncate_RuneSafe verifies that truncate doesn't slice inside a
// multibyte rune (which would crash for some boundary positions and
// produce mojibake for others). The previous byte-slicing version
// would panic on len("Рефакторинг") > 30 == false but on a longer
// Cyrillic title (e.g. 22 runes = 44 bytes) the byte slice would fall
// mid-codepoint.
func TestTruncate_RuneSafe(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
	}{
		{"ascii_short", "abc", 10},
		{"ascii_long", strings.Repeat("a", 80), 30},
		{"cyrillic_long", strings.Repeat("Рефакторинг ", 4), 30},
		{"emoji_long", strings.Repeat("🚀 ", 20), 10},
		{"cjk_long", strings.Repeat("作業中", 10), 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("truncate panicked: %v", r)
				}
			}()
			got := truncate(tc.in, tc.n)
			if !utf8.ValidString(got) {
				t.Errorf("invalid UTF-8 output: %q", got)
			}
			// utf8.RuneCountInString(input) <= n → unchanged.
			if utf8.RuneCountInString(tc.in) <= tc.n {
				if got != tc.in {
					t.Errorf("unchanged input was mutated: %q→%q", tc.in, got)
				}
				return
			}
			if utf8.RuneCountInString(got) != tc.n {
				t.Errorf("rune count: got %d want %d (output %q)",
					utf8.RuneCountInString(got), tc.n, got)
			}
		})
	}
}

// TestTruncate_NEdge: n < 1 → empty.
func TestTruncate_NEdge(t *testing.T) {
	if got := truncate("hello", 0); got != "" {
		t.Errorf("n=0: got %q want empty", got)
	}
	if got := truncate("hello", -3); got != "" {
		t.Errorf("n<0: got %q want empty", got)
	}
}

// fixedTime gives every fixture row a deterministic creation stamp
// so timeformat output stays stable across test runs and CI clocks.
var fixedTime = time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

// TestRenderTaskDetail_Markdown verifies that markdown in
// task.Description is rendered through glamour and not surfaced as
// raw characters. The literal "# Title" marker disappears (heading
// styled, hash stripped) and the body picks up ANSI escapes.
func TestRenderTaskDetail_Markdown(t *testing.T) {
	task := datasource.Task{
		ID:          "ask-000001",
		Title:       "fixture",
		Status:      store.StatusNew,
		Priority:    2,
		CreatedAt:   fixedTime,
		Description: "# Title\n\n- one\n- two\n",
	}
	out := renderTaskDetail(task, nil, nil, 80)
	if out == "" {
		t.Fatal("renderTaskDetail returned empty")
	}
	visible := stripANSI(out)
	if !strings.Contains(visible, "Title") {
		t.Errorf("heading text missing from rendered output: %q", out)
	}
	// glamour styles the heading with its prefix ("# ") and ANSI;
	// the raw markdown "# Title" with no escape codes between the
	// hash and the title shouldn't survive verbatim.
	if strings.Contains(out, "# Title\n") {
		t.Errorf("raw markdown heading leaked unstyled into output: %q", out)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("expected ANSI escapes in rendered output: %q", out)
	}
	// Bullet glyph (StyleConfig.Item.BlockPrefix = "• ") proves the
	// list went through markdown.Render, not raw passthrough.
	if !strings.Contains(visible, "•") {
		t.Errorf("bullet glyph missing from rendered list: %q", out)
	}
}

// TestRenderTaskDetail_EmptyDescription pins the "(no description)"
// placeholder (acceptance criterion #4). An empty description must
// NOT be routed through glamour — the placeholder is the contract.
func TestRenderTaskDetail_EmptyDescription(t *testing.T) {
	task := datasource.Task{
		ID:          "ask-000002",
		Title:       "fixture",
		Status:      store.StatusNew,
		CreatedAt:   fixedTime,
		Description: "",
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := stripANSI(out)
	if !strings.Contains(visible, "(no description)") {
		t.Errorf("missing (no description) placeholder: %q", out)
	}
}

// TestRenderTaskDetail_CommentsMultiline pins the comment-rendering
// contract from acceptance criterion #3: comments are not clipped
// to 70 chars, multi-line text renders in full, and EVERY comment
// in the thread is rendered — there is no implicit cap.
func TestRenderTaskDetail_CommentsMultiline(t *testing.T) {
	long := strings.Repeat("word ", 30) // >70 chars on a single line
	multi := "first line\nsecond line\nthird line"
	comments := []datasource.Comment{
		{ID: 1, AuthorName: "alice", Text: "c1", CreatedAt: fixedTime},
		{ID: 2, AuthorName: "bob", Text: "c2", CreatedAt: fixedTime},
		{ID: 3, AuthorName: "carol", Text: long, CreatedAt: fixedTime},
		{ID: 4, AuthorName: "dave", Text: multi, CreatedAt: fixedTime},
		{ID: 5, AuthorName: "eve", Text: "c5", CreatedAt: fixedTime},
		{ID: 6, AuthorName: "frank", Text: "c6", CreatedAt: fixedTime},
	}
	task := datasource.Task{
		ID: "ask-000003", Title: "f", Status: store.StatusNew, CreatedAt: fixedTime,
	}
	out := renderTaskDetail(task, comments, nil, 80)
	visible := stripANSI(out)

	// No cap: every author in the seed (including the oldest,
	// alice) must show up in the rendered output.
	for _, name := range []string{"alice", "bob", "carol", "dave", "eve", "frank"} {
		if !strings.Contains(visible, name) {
			t.Errorf("expected author %q in rendered comments, got: %q", name, out)
		}
	}

	// Multi-line comment renders in full — every line of dave's text
	// makes it through markdown.Render (glamour collapses adjacent
	// lines into one paragraph, so the words have to be present even
	// if the line layout changes).
	for _, word := range []string{"first", "second", "third"} {
		if !strings.Contains(visible, word) {
			t.Errorf("multi-line word %q missing from rendered comment: %q", word, out)
		}
	}

	// Long comment is NOT clipped at 70 chars + ellipsis: every
	// repetition of "word" survives the render (glamour may wrap
	// but won't drop content).
	wordCount := strings.Count(visible, "word")
	if wordCount < 25 {
		t.Errorf("long comment looks clipped: only %d 'word' occurrences in %q",
			wordCount, out)
	}
	if strings.Contains(out, "…") && strings.Contains(out, "word…") {
		t.Errorf("comment appears clipped with ellipsis: %q", out)
	}
}

// TestRenderTaskDetail_AllCommentsRendered pins the no-cap contract:
// seeding a long comment thread (>>5) must render every comment
// in the Detail pane. Guards against any future regression where
// the display layer reintroduces a per-task trim.
func TestRenderTaskDetail_AllCommentsRendered(t *testing.T) {
	const n = 25
	comments := make([]datasource.Comment, n)
	authors := make([]string, n)
	for i := 0; i < n; i++ {
		author := fmt.Sprintf("author%02d", i)
		authors[i] = author
		comments[i] = datasource.Comment{
			ID:         int64(i + 1),
			AuthorName: author,
			Text:       fmt.Sprintf("body %02d", i),
			CreatedAt:  fixedTime,
		}
	}
	task := datasource.Task{
		ID: "ask-allcmt", Title: "f", Status: store.StatusNew,
		CreatedAt: fixedTime, CommentCount: n,
	}
	out := renderTaskDetail(task, comments, nil, 80)
	visible := stripANSI(out)

	for _, name := range authors {
		if !strings.Contains(visible, name) {
			t.Errorf("author %q missing from rendered detail pane (expected all %d comments to render)", name, n)
		}
	}
}

// TestRenderTaskDetail_EmptyCommentSkipped pins the new
// box-rendering contract for empty comments: a comment with an
// empty body must NOT produce a box at all (header included).
// The previous design (REMARK 3 from the as-a261 review) emitted
// a header line and skipped the body, leaving a wasted indented
// line behind; with each comment now wrapped in its own labeled
// box, the right thing to do is drop the entry entirely — a
// labeled box with no content conveys less than no entry at all.
// autosk_comment rejects empty bodies upstream so this is
// defensive, but rendering still has to behave sanely.
func TestRenderTaskDetail_EmptyCommentSkipped(t *testing.T) {
	comments := []datasource.Comment{
		{ID: 1, AuthorName: "alice", Text: "first body", CreatedAt: fixedTime},
		{ID: 2, AuthorName: "bob", Text: "", CreatedAt: fixedTime},
		{ID: 3, AuthorName: "carol", Text: "third body", CreatedAt: fixedTime},
	}
	task := datasource.Task{
		ID: "ask-e22907", Title: "f", Status: store.StatusNew, CreatedAt: fixedTime,
	}
	out := renderTaskDetail(task, comments, nil, 80)
	visible := ansiutil.Strip(out)

	// alice and carol have non-empty bodies — their author header
	// shows up on the box's top border.
	for _, name := range []string{"alice", "carol"} {
		if !strings.Contains(visible, name) {
			t.Errorf("author %q header missing from rendered output: %q", name, out)
		}
	}
	// bob's empty-body comment must be skipped entirely — no
	// labeled box with just a header and no content.
	if strings.Contains(visible, "bob") {
		t.Errorf("empty-body comment author %q leaked into rendered output: %q", "bob", out)
	}
}

// TestRenderWorkflowDetail_Markdown verifies that workflow.Description
// is rendered through the same markdown pipeline as task descriptions.
func TestRenderWorkflowDetail_Markdown(t *testing.T) {
	wf := datasource.Workflow{
		Name:        "wf-test",
		Description: "# Workflow Heading\n\nSome **bold** text.",
		FirstStep:   "dev",
		Steps: []datasource.WorkflowStep{
			{Name: "dev", AgentName: "a1"},
		},
	}
	out := renderWorkflowDetail(wf, 80)
	visible := stripANSI(out)
	if !strings.Contains(visible, "Workflow Heading") {
		t.Errorf("heading text missing: %q", out)
	}
	if strings.Contains(out, "# Workflow Heading\n") {
		t.Errorf("raw markdown heading leaked unstyled: %q", out)
	}
	if strings.Contains(out, "**bold**") {
		t.Errorf("raw bold marker leaked: %q", out)
	}
	if !strings.Contains(visible, "bold") {
		t.Errorf("bold text payload missing: %q", out)
	}
}

// TestRenderTaskDetail_LongDescription_Scrollable pins REMARK 6 from
// the as-a261 review: with markdown rendering live, a multi-paragraph
// description must produce enough rendered lines for the detail
// pane's scroll machinery (gocui's View.oy, driven by detailScroll
// in inspector.go) to have something to scroll over. The previous
// truncate-to-70-chars rendering capped comments at one line each
// and descriptions at whatever fit; a future refactor that
// accidentally re-truncates would regress scrolling silently.
//
// This is a one-shot "the pane has scrollable content" assertion;
// the actual scroll-position-survives-clear contract belongs to
// gocui (View.Clear nulls line buffers but preserves oy) and is
// exercised by the lazy TUI's own integration runs, not by a unit
// test against renderTaskDetail.
func TestRenderTaskDetail_LongDescription_Scrollable(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("# Long description\n\n")
	for i := 0; i < 100; i++ {
		sb.WriteString("- bullet line that is long enough to take a row in the pane\n")
	}
	task := datasource.Task{
		ID: "ask-100043", Title: "f", Status: store.StatusNew, CreatedAt: fixedTime,
		Description: sb.String(),
	}
	out := renderTaskDetail(task, nil, nil, 80)
	// Header + metadata lines are <10. A markdown render of 100
	// bullets should produce well over 50 newline-terminated lines;
	// pick a conservative floor so the test isn't a fragile
	// glamour-version pin while still catching a re-truncate.
	newlines := strings.Count(out, "\n")
	if newlines < 50 {
		t.Errorf("rendered detail too short for scrolling (%d lines); a re-truncate may have crept back in. out=%q",
			newlines, out)
	}
	// And no ellipsis chevron sneaking in from a truncate path.
	visible := ansiutil.Strip(out)
	if strings.Contains(visible, "bullet line that is long enough to take a row in the pane\u2026") {
		t.Errorf("bullet line was truncated with ellipsis; got %q", visible)
	}
}

// TestRenderTaskDetail_ZeroWidth pins the fail-open behaviour for
// the first-frame layout pass: when innerWidth returns 0 (the view
// hasn't been sized yet), markdown.Render emits the raw text and
// renderTaskDetail still produces a usable pane.
func TestRenderTaskDetail_ZeroWidth(t *testing.T) {
	task := datasource.Task{
		ID: "ask-000004", Title: "f", Status: store.StatusNew,
		CreatedAt:   fixedTime,
		Description: "# Title\n\nbody",
	}
	out := renderTaskDetail(task, nil, nil, 0)
	visible := stripANSI(out)
	if !strings.Contains(visible, "# Title") {
		t.Errorf("width=0 should pass markdown through verbatim, missing source: %q", out)
	}
	if !strings.Contains(visible, "body") {
		t.Errorf("body text missing at width=0: %q", out)
	}
}

// TestRenderTaskDetail_HeaderRow pins the first-row contract from
// the as-a261 spec: P{prio} id status wf:step agent, no "task"
// label and no per-token labels ("wf=", "step=", "agent=").
// Priority is muted-gray so the row reads the same way the Tasks
// panel does.
func TestRenderTaskDetail_HeaderRow(t *testing.T) {
	task := datasource.Task{
		ID: "ask-00a261", Title: "—", Status: store.StatusHuman, Priority: 2,
		CreatedAt:    fixedTime,
		WorkflowName: "feature-dev-generic",
		StepName:     "validator",
		AgentName:    "@autogent/generic",
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := ansiutil.Strip(out)
	firstLine := strings.SplitN(visible, "\n", 2)[0]
	want := "P2 ask-00a261 human feature-dev-generic:validator @autogent/generic"
	if firstLine != want {
		t.Errorf("first row mismatch:\n got: %q\nwant: %q", firstLine, want)
	}
	if strings.Contains(firstLine, "task ") {
		t.Errorf("unexpected \"task\" label literal on first row: %q", firstLine)
	}
	if strings.Contains(firstLine, "wf=") || strings.Contains(firstLine, "agent=") {
		t.Errorf("unexpected key=value tokens on first row: %q", firstLine)
	}
}

// TestRenderTaskDetail_BlockedRow pins the conditional "blocked by"
// row: it shows up only when the task is actually blocked and we
// have ids; the word "blocked" wears the err (red) hue.
func TestRenderTaskDetail_BlockedRow(t *testing.T) {
	task := datasource.Task{
		ID: "ask-00a261", Status: store.StatusWork, Priority: 1,
		CreatedAt: fixedTime,
		Blocked:   true,
		BlockedBy: []datasource.TaskRef{
			{ID: "ask-000011", Status: store.StatusWork},
			{ID: "ask-000022", Status: store.StatusNew},
		},
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "blocked by: ask-000011, ask-000022") {
		t.Errorf("missing blocked-by row with both ids: %q", visible)
	}
	// The word "blocked" must be styled in styleErr (red). We look for
	// the styled prefix the renderer emits — styleErr.Render("blocked")
	// wraps the word in an SGR escape ending with the visible token.
	if !strings.Contains(out, styleErr.Render("blocked")) {
		t.Errorf("\"blocked\" label not painted with styleErr (red): %q", out)
	}

	// And the row must NOT appear when Blocked=false, even if
	// BlockedBy still carries closed-blocker ids from a stale snapshot.
	task.Blocked = false
	out = renderTaskDetail(task, nil, nil, 80)
	visible = ansiutil.Strip(out)
	if strings.Contains(visible, "blocked by:") {
		t.Errorf("blocked-by row leaked when Blocked=false: %q", visible)
	}
}

// TestRenderTaskDetail_BlockedRow_Partitioned pins the active-first
// then closed-last ordering and the gray colour for done/cancel
// blockers. The two contracts overlap on the same row: a blocker
// that just went done should slide to the tail AND drop to
// styleMuted, while the active rows stay at the head in the
// regular task-id hue.
func TestRenderTaskDetail_BlockedRow_Partitioned(t *testing.T) {
	forceTrueColor(t)
	task := datasource.Task{
		ID: "ask-30a1c5", Status: store.StatusWork, CreatedAt: fixedTime,
		Blocked: true,
		BlockedBy: []datasource.TaskRef{
			{ID: "ask-d0ne01", Status: store.StatusDone},
			{ID: "ask-ac7501", Status: store.StatusWork},
			{ID: "ask-ca0c01", Status: store.StatusCancel},
			{ID: "ask-ac7502", Status: store.StatusNew},
			{ID: "ask-ac7503", Status: store.StatusHuman},
		},
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := ansiutil.Strip(out)

	// Active first (input order), then closed (input order).
	wantOrder := "blocked by: ask-ac7501, ask-ac7502, ask-ac7503, ask-d0ne01, ask-ca0c01"
	if !strings.Contains(visible, wantOrder) {
		t.Errorf("blocker order wrong; want substring %q in %q", wantOrder, visible)
	}

	// Active rows wear the task-id hue (blue).
	for _, id := range []string{"ask-ac7501", "ask-ac7502", "ask-ac7503"} {
		if !strings.Contains(out, renderTaskID(id)) {
			t.Errorf("active blocker %q not styled with task-id hue: %q", id, out)
		}
	}
	// Closed rows drop to muted gray.
	for _, id := range []string{"ask-d0ne01", "ask-ca0c01"} {
		if !strings.Contains(out, styleMuted.Render(id)) {
			t.Errorf("closed blocker %q not styled with muted hue: %q", id, out)
		}
	}
}

// TestRenderTaskDetail_StatsRow_Coloring pins the muted/white/green
// split on the stats row:
//   - the whole row is gray by default
//   - when CommentCount > 0, the "comments: " label drops to default
//     foreground ("white") and the count is OK-green
//   - when CommentCount == 0, the comments half stays all-gray
func TestRenderTaskDetail_StatsRow_Coloring(t *testing.T) {
	forceTrueColor(t)

	// Zero-comment branch: the comments half stays muted.
	task := datasource.Task{ID: "ask-2e7000", Status: store.StatusNew, CreatedAt: fixedTime, CommentCount: 0}
	out := renderTaskDetail(task, nil, nil, 80)
	if !strings.Contains(out, styleMuted.Render("comments: 0")) {
		t.Errorf("zero-comment label not all-muted: %q", out)
	}

	// Comment-bearing branch: count is OK-green, label is default fg
	// (no SGR span around "comments:").
	task.CommentCount = 7
	out = renderTaskDetail(task, nil, nil, 80)
	if !strings.Contains(out, styleOK.Render("7")) {
		t.Errorf("comment count not styled with OK hue (green): %q", out)
	}
	// The label should NOT be wrapped in styleMuted when there are
	// comments — i.e. "comments: " appears in the output without a
	// leading SGR escape immediately before it.
	if strings.Contains(out, styleMuted.Render("comments:")) {
		t.Errorf("comments label wears muted hue when CommentCount>0: %q", out)
	}

	// And the created half is always muted, regardless of branch.
	wantCreated := styleMuted.Render("created: " + timeformat.FormatDateTimeSmart(fixedTime) + ",")
	if !strings.Contains(out, wantCreated) {
		t.Errorf("created half not all-muted: want %q in %q", wantCreated, out)
	}
}

// TestRenderTaskDetail_StatsRow pins the created+comments row: time
// without date for today (FormatDateTimeSmart contract), full
// datetime otherwise, and comma-separated formatting.
func TestRenderTaskDetail_StatsRow(t *testing.T) {
	// Reference "now" matching the test's CreatedAt local date so
	// FormatDateTimeSmart drops the date — we exercise the today
	// branch by passing a fixed CreatedAt and letting time.Now() act
	// as the smart-format reference. To keep the assertion stable
	// across CI clocks we use the formatted time string and check the
	// row contains it (instead of pinning a literal HH:MM:SS).
	// Use a CreatedAt anchored at noon local-today so the
	// FormatDateTimeSmart "today" branch fires deterministically
	// regardless of when the test is run (a now-30m offset breaks
	// across local midnight).
	now := time.Now().In(time.Local)
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	task := datasource.Task{
		ID: "ask-00a261", Status: store.StatusNew,
		CreatedAt:    today,
		CommentCount: 12,
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := ansiutil.Strip(out)
	wantStamp := timeformat.FormatDateTimeSmart(task.CreatedAt)
	wantRow := "created: " + wantStamp + ", comments: 12"
	if !strings.Contains(visible, wantRow) {
		t.Errorf("stats row missing or mis-formatted; want substring %q in %q", wantRow, visible)
	}
	// Today branch: the rendered stamp has no date dashes.
	if strings.Contains(wantStamp, "-") {
		t.Fatalf("test fixture broken: smart stamp for local noon today should be time-only, got %q", wantStamp)
	}
}

// TestRenderTaskDetail_TitleBox pins the Title box contract: the
// task title sits inside a rounded box labelled "Title".
func TestRenderTaskDetail_TitleBox(t *testing.T) {
	task := datasource.Task{
		ID: "ask-b07001", Status: store.StatusNew, CreatedAt: fixedTime,
		Title: "feat: markdown rendering of tasks description",
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "╭─ Title ") {
		t.Errorf("missing Title box top border: %q", visible)
	}
	if !strings.Contains(visible, task.Title) {
		t.Errorf("task title body missing from Title box: %q", visible)
	}
	if !strings.Contains(visible, "╰─") {
		t.Errorf("missing rounded-bottom-left corner anywhere in output: %q", visible)
	}
}

// TestRenderTaskDetail_DescriptionBox pins the Description box: the
// rendered markdown sits inside a box labelled "Description".
func TestRenderTaskDetail_DescriptionBox(t *testing.T) {
	task := datasource.Task{
		ID: "ask-b07002", Status: store.StatusNew, CreatedAt: fixedTime,
		Description: "# Heading\n\nbody paragraph.\n",
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "╭─ Description ") {
		t.Errorf("missing Description box top border: %q", visible)
	}
	if !strings.Contains(visible, "Heading") {
		t.Errorf("description heading missing from box: %q", visible)
	}
}

// TestRenderTaskDetail_SignalsBoxAll pins the new contract that the
// Recent signals box now shows ALL signals, not just the last 3.
// Box label includes the count.
func TestRenderTaskDetail_SignalsBoxAll(t *testing.T) {
	task := datasource.Task{
		ID: "ask-516001", Status: store.StatusWork, CreatedAt: fixedTime,
	}
	signals := []datasource.Signal{
		{WorkflowName: "wf", StepName: "step-a", Target: "step-b", CreatedAt: fixedTime},
		{WorkflowName: "wf", StepName: "step-b", Target: "step-c", CreatedAt: fixedTime},
		{WorkflowName: "wf", StepName: "step-c", Target: "step-d", CreatedAt: fixedTime},
		{WorkflowName: "wf", StepName: "step-d", Target: "step-e", CreatedAt: fixedTime},
		{WorkflowName: "wf", StepName: "step-e", Target: "step-f", CreatedAt: fixedTime},
		{WorkflowName: "wf", StepName: "step-f", Target: "done", CreatedAt: fixedTime},
	}
	out := renderTaskDetail(task, nil, signals, 80)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "╭─ Recent signals (6) ") {
		t.Errorf("missing \"Recent signals (6)\" box header: %q", visible)
	}
	// Every signal row must render — no last-3 truncation.
	for _, want := range []string{"step-a", "step-b", "step-c", "step-d", "step-e", "step-f"} {
		if !strings.Contains(visible, want) {
			t.Errorf("signal step %q missing from box; the box must show ALL signals, not just the last 3: %q", want, visible)
		}
	}
}

// TestRenderTaskDetail_SignalsTargetColored pins the contract that
// the right-hand side of each signal arrow wears its canonical
// entity colour: a sibling step name gets the step-name hue, while
// a lifecycle terminal (done/cancel/human) gets its task-status hue.
//
// Forces TrueColor so lipgloss actually emits SGR escapes under
// `go test` (where stdout is a pipe and the renderer would
// otherwise degrade to Ascii). The previous incarnation of this
// test was a false-positive on that exact issue — the assertion
// hit a substring that happened to be in the output regardless of
// styling because lipgloss wasn't emitting any escapes at all.
func TestRenderTaskDetail_SignalsTargetColored(t *testing.T) {
	forceTrueColor(t)
	task := datasource.Task{
		ID: "ask-516002", Status: store.StatusWork, CreatedAt: fixedTime,
	}
	signals := []datasource.Signal{
		// Each signal pairs a source unique to the row with a target
		// of the same name appearing only on the target side, so a
		// substring search for "\u001b...<target>\u001b" finds the
		// target's styling and nothing else.
		{WorkflowName: "wf", StepName: "src1", Target: "validator", CreatedAt: fixedTime},
		{WorkflowName: "wf", StepName: "src2", Target: "done", CreatedAt: fixedTime},
		{WorkflowName: "wf", StepName: "src3", Target: "cancel", CreatedAt: fixedTime},
		{WorkflowName: "wf", StepName: "src4", Target: "human", CreatedAt: fixedTime},
	}
	out := renderTaskDetail(task, nil, signals, 80)

	// Step-name target wears renderStepName's hue.
	wantStep := renderStepName("validator")
	if !strings.Contains(out, "\x1b") {
		t.Fatalf("output carries no ANSI escapes at all — forceTrueColor did not take effect: %q", out)
	}
	if !strings.Contains(out, wantStep) {
		t.Errorf("step-name target \"validator\" not styled with step-name hue\nwant substring: %q\n         in: %q", wantStep, out)
	}
	// Lifecycle-terminal targets wear their task-status hue (same
	// styling renderWorkflowDetail uses on the step graph).
	for _, st := range []store.Status{store.StatusDone, store.StatusCancel, store.StatusHuman} {
		want := styleForTaskStatus(st).Render(string(st))
		if !strings.Contains(out, want) {
			t.Errorf("lifecycle terminal %q not styled with task-status hue\nwant substring: %q\n         in: %q", st, want, out)
		}
	}
}

// TestRenderTaskDetail_SignalsSourceWorkflowStep pins the contract
// that the source side of every signal row carries the same
// `workflow:step` formatting as the Jobs panel: yellow workflow
// name (styleWorkflowName), default-colour colon, purple step name
// (styleStepName). The canonical composition is produced by
// renderWorkflowStep, so the assertion checks the rendered substring
// directly — a regression to bare renderStepName on the source side
// would drop the styleWorkflowName escapes and fail the substring
// check.
func TestRenderTaskDetail_SignalsSourceWorkflowStep(t *testing.T) {
	forceTrueColor(t)
	task := datasource.Task{
		ID: "ask-516010", Status: store.StatusWork, CreatedAt: fixedTime,
	}
	signals := []datasource.Signal{
		{WorkflowName: "feature-dev", StepName: "dev", Target: "review", CreatedAt: fixedTime},
		{WorkflowName: "feature-dev", StepName: "review", Target: "done", CreatedAt: fixedTime},
	}
	out := renderTaskDetail(task, nil, signals, 80)
	if !strings.Contains(out, "\x1b") {
		t.Fatalf("output carries no ANSI escapes at all — forceTrueColor did not take effect: %q", out)
	}
	for _, s := range signals {
		want := renderWorkflowStep(s.WorkflowName, s.StepName)
		if !strings.Contains(out, want) {
			t.Errorf("signal source not rendered via renderWorkflowStep\nwant substring: %q\n         in: %q", want, out)
		}
		// Defence-in-depth: both halves must carry their canonical
		// entity hues (yellow workflow, purple step), with a
		// default-colour colon in between. A renderStepName-only
		// regression would drop the workflow escape.
		if !strings.Contains(out, styleWorkflowName.Render(s.WorkflowName)) {
			t.Errorf("workflow name %q missing styleWorkflowName hue in: %q", s.WorkflowName, out)
		}
		if !strings.Contains(out, styleStepName.Render(s.StepName)) {
			t.Errorf("step name %q missing styleStepName hue in: %q", s.StepName, out)
		}
	}
}

// TestRenderTaskDetail_SignalsSourceAlignment pins that the arrow
// stays aligned at the same x even when one row has a much longer
// workflow name than the others — i.e. the source column is padded
// to the widest `workflowStepPlainWidth(wf, step)` in the visible
// row set, not to the widest step name alone.
func TestRenderTaskDetail_SignalsSourceAlignment(t *testing.T) {
	old := time.Now().Add(-72 * time.Hour)
	signals := []datasource.Signal{
		// `long-feature-workflow:a` is much wider than `wf:b`, so
		// padding the source to `len(step)` only would land the
		// arrow at different x positions on each row.
		{WorkflowName: "long-feature-workflow", StepName: "a", Target: "b", CreatedAt: old.Add(3 * time.Hour)},
		{WorkflowName: "wf", StepName: "b", Target: "c", CreatedAt: old.Add(2 * time.Hour)},
		// Orphaned/legacy row (WorkflowName empty) renders via
		// renderWorkflowStep's `(no-wf)` muted fallback. Plain width
		// of that fallback is exactly len("(no-wf)")=7, so the row
		// participates in the same alignment math without panicking.
		{WorkflowName: "", StepName: "", Target: "human", CreatedAt: old.Add(1 * time.Hour)},
		{WorkflowName: "wf", StepName: "c", Target: "done", CreatedAt: old},
	}
	task := datasource.Task{ID: "ask-516011", Status: store.StatusWork, CreatedAt: old}
	out := renderTaskDetail(task, nil, signals, 120)
	visible := ansiutil.Strip(out)

	var rows []string
	for _, ln := range strings.Split(visible, "\n") {
		if !strings.HasPrefix(ln, "│ ") || !strings.HasSuffix(ln, " │") {
			continue
		}
		if !strings.Contains(ln, "→") {
			continue
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(ln, "│ "), " │")
		rows = append(rows, strings.TrimRight(inner, " "))
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 signal rows; got %d: %q", len(rows), rows)
	}
	// Orphan row must render the muted `(no-wf)` fallback rather
	// than panic or produce a bare-step-name line.
	if !strings.Contains(visible, "(no-wf)") {
		t.Errorf("orphaned row did not render via renderWorkflowStep's (no-wf) fallback: %q", visible)
	}

	// Arrows align at the same x across all rows.
	arrowX := strings.Index(rows[0], "→")
	for i, r := range rows {
		if x := strings.Index(r, "→"); x != arrowX {
			t.Errorf("row %d: arrow not aligned (x=%d, want %d): %q", i, x, arrowX, r)
		}
	}

	// Source column width matches the widest workflowStepPlainWidth
	// in the row set. Layout: <19-cell datetime>  <source><pad>  →  <target>.
	// Strip the datetime + 2-space gutter and measure from the
	// start of source to the two-space gutter preceding the arrow.
	wantSrcW := 0
	for _, s := range signals {
		if w := workflowStepPlainWidth(s.WorkflowName, s.StepName); w > wantSrcW {
			wantSrcW = w
		}
	}
	for i, r := range rows {
		// row layout: 19 cells + 2 spaces + source + 2 spaces + "→" + ...
		const dateW = 19
		srcStart := dateW + 2
		srcEnd := arrowX - 2 // back over the "  " gutter before the arrow
		if srcEnd <= srcStart {
			t.Fatalf("row %d: cannot extract source column from %q", i, r)
		}
		if got := srcEnd - srcStart; got != wantSrcW {
			t.Errorf("row %d: source column width %d, want %d: %q", i, got, wantSrcW, r)
		}
	}
}

// TestRenderTaskDetail_SignalsStacked pins the per-column layout of
// the signals box:
//
//	<datetime>  <source>  →  <target>
//
// Two literal spaces between every column. Datetime is right-aligned
// in a 19-cell column sized for "YYYY-MM-DD HH:MM:SS"; today's events
// render time-only and pad on the LEFT so the HH:MM:SS portion lines
// up under the time half of older full stamps. The source column is
// padded to the longest step name across all rows so the arrow lands
// at the same x on every line.
func TestRenderTaskDetail_SignalsStacked(t *testing.T) {
	// All fixture timestamps fall on the same local day so
	// FormatDateTimeSmart's today/not-today branch is deterministic
	// w.r.t. each other (every row is in the same branch as the
	// reference "now"). We can't pin time.Now() from here, but we
	// can pin the relative layout: pick a stamp old enough that the
	// smart format is guaranteed to emit the full datetime form.
	old := time.Now().Add(-72 * time.Hour)
	signals := []datasource.Signal{
		{WorkflowName: "wf", StepName: "validate", Target: "human", CreatedAt: old.Add(2 * time.Hour)},
		{WorkflowName: "wf", StepName: "docs", Target: "validate", CreatedAt: old.Add(1 * time.Hour)},
		{WorkflowName: "wf", StepName: "review", Target: "docs", CreatedAt: old.Add(30 * time.Minute)},
		{WorkflowName: "wf", StepName: "dev", Target: "review", CreatedAt: old},
	}
	task := datasource.Task{ID: "ask-516003", Status: store.StatusWork, CreatedAt: old}
	out := renderTaskDetail(task, nil, signals, 80)
	visible := ansiutil.Strip(out)

	// Find the body lines inside the signals box (the ones with the
	// arrow). Strip the box frame so we look at raw column layout.
	var rows []string
	for _, ln := range strings.Split(visible, "\n") {
		if !strings.HasPrefix(ln, "│ ") || !strings.HasSuffix(ln, " │") {
			continue
		}
		if !strings.Contains(ln, "→") {
			continue
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(ln, "│ "), " │")
		rows = append(rows, strings.TrimRight(inner, " "))
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 signal rows; got %d: %q", len(rows), rows)
	}

	// Every row must have the arrow at the same x — the source
	// column is padded to the longest step name.
	arrowX := strings.Index(rows[0], "→")
	for i, r := range rows {
		if x := strings.Index(r, "→"); x != arrowX {
			t.Errorf("row %d: arrow not aligned (x=%d, want %d): %q", i, x, arrowX, r)
		}
	}

	// Two-space separators around the arrow and between datetime
	// and source.
	for i, r := range rows {
		if !strings.Contains(r, "  →  ") {
			t.Errorf("row %d: missing \" ─→─ \" with 2-space gutters: %q", i, r)
		}
	}

	// Datetime column is exactly 19 cells: the longest stamp form
	// ("YYYY-MM-DD HH:MM:SS"). For older rows the full stamp
	// already occupies the 19-cell column; subsequent assertions
	// pin that the source column starts at the same x.
	for i, r := range rows {
		// Take the substring up to (and including) the 2-space
		// separator before the source column.
		parts := strings.SplitN(r, "  ", 2)
		if len(parts) != 2 {
			t.Fatalf("row %d: cannot split on \"  \" gutter: %q", i, r)
		}
		if w := utf8.RuneCountInString(parts[0]); w != 19 {
			t.Errorf("row %d: datetime column width %d, want 19: %q", i, w, parts[0])
		}
	}
}

// TestRenderTaskDetail_FullWidthWrap pins the contract that the
// description body wraps at the FULL inner-box width, not at the
// old 120-cell readability cap. Symptom on a wide pane was a dead
// column of padding inside the right border because markdown
// wrapped at 120 even though the box extended further.
func TestRenderTaskDetail_FullWidthWrap(t *testing.T) {
	const paneW = 200
	long := strings.Repeat("word ", 80) // ~400 cells of fillable content
	task := datasource.Task{
		ID: "ask-71de01", Status: store.StatusNew, CreatedAt: fixedTime,
		Description: long,
	}
	out := renderTaskDetail(task, nil, nil, paneW)
	visible := ansiutil.Strip(out)

	// Locate the description body line(s) inside the box. Lines
	// inside the box start with "│ " (frame + padding). Measure the
	// LONGEST body line's visible-cell width and assert it stretches
	// closer to the box's inner content width than to the legacy
	// 120-cell cap.
	contentW := paneW - 4 // 2 frame cells + 2 padding cells
	maxBodyW := 0
	for _, ln := range strings.Split(visible, "\n") {
		if !strings.HasPrefix(ln, "│ ") || !strings.HasSuffix(ln, " │") {
			continue
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(ln, "│ "), " │")
		inner = strings.TrimRight(inner, " ")
		if w := utf8.RuneCountInString(inner); w > maxBodyW {
			maxBodyW = w
		}
	}
	// At paneW=200, contentW=196. We expect the wrap to land near
	// contentW (within a word's worth of slack). A regression to the
	// old 120-cell cap would land maxBodyW at ~120.
	if maxBodyW < contentW-10 {
		t.Errorf("description body wraps too early: maxBodyW=%d, contentW=%d (a regression to the legacy 120-cell cap would land here at ~120):\n%s",
			maxBodyW, contentW, visible)
	}
}

// TestRenderTaskDetail_CommentBoxLabel pins the per-comment box
// label contract: "<smart-time> <author>" on the top border, body
// inside the frame, no leftover "─ comments (N) ─" section header.
func TestRenderTaskDetail_CommentBoxLabel(t *testing.T) {
	task := datasource.Task{
		ID: "ask-c00001", Status: store.StatusNew, CreatedAt: fixedTime,
	}
	comments := []datasource.Comment{
		{ID: 1, AuthorName: "alice", Text: "hello world", CreatedAt: fixedTime},
	}
	out := renderTaskDetail(task, comments, nil, 80)
	visible := ansiutil.Strip(out)
	wantStamp := timeformat.FormatDateTimeSmart(fixedTime)
	wantHeader := "╭─ " + wantStamp + " alice "
	if !strings.Contains(visible, wantHeader) {
		t.Errorf("missing comment-box header %q in: %q", wantHeader, visible)
	}
	if !strings.Contains(visible, "hello world") {
		t.Errorf("comment body missing inside box: %q", visible)
	}
	// The old "─ comments (N) ─" section header must NOT survive —
	// each comment now wears its own box label.
	if strings.Contains(visible, "─ comments (") {
		t.Errorf("legacy \"─ comments (N) ─\" section header leaked: %q", visible)
	}
}

// TestWrapPlain spot-checks the plain-text word wrap helper used by
// the Title box.
func TestWrapPlain(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{"empty", "", 10, ""},
		{"short", "abc def", 10, "abc def"},
		{"wraps_at_word", "alpha beta gamma", 10, "alpha beta\ngamma"},
		{"hard_break_long_word", "abcdefghijklmnop", 5, "abcde\nfghij\nklmno\np"},
		// width=6 fits one 4-rune word per line but not "два три"
		// together (3+1+3=7>6), so the wrap drops to one word per
		// line for the short middles too.
		{"cyrillic", "Один два три четыре", 6, "Один\nдва\nтри\nчетыре"},
		{"cyrillic_wider", "Один два три четыре", 8, "Один два\nтри\nчетыре"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapPlain(tc.in, tc.width)
			if got != tc.want {
				t.Errorf("wrapPlain(%q, %d):\n got:  %q\n want: %q", tc.in, tc.width, got, tc.want)
			}
		})
	}
}

// TestDrawLabeledBox_Dimensions pins the box's width invariant: top
// and bottom borders are exactly `width` cells wide regardless of
// label length, and each body line is padded to the same visible
// width as the borders.
func TestDrawLabeledBox_Dimensions(t *testing.T) {
	const width = 30
	out := drawLabeledBox("Lbl", "hello\nworld", width)
	lines := strings.Split(out, "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (top + 2 body + bottom); got %d: %q", len(lines), out)
	}
	for i, ln := range lines {
		w := lipglossWidth(ln)
		if w != width {
			t.Errorf("line %d width %d, want %d: %q", i, w, width, ln)
		}
	}
	// Frame-less fallback at impossibly narrow widths.
	if got := drawLabeledBox("x", "hi", 3); got != "hi" {
		t.Errorf("narrow box should fall back to body; got %q", got)
	}
}

// TestRenderWorkflowDetail_HeaderLine pins the first line's order
// and shape, ANSI-stripped:
//
//	^<name>( \[wt\])? first step: <step>$
//
// The `[wt]` chip appears IFF !w.IsSynthetic && w.Isolation ==
// "worktree"; everything else is dropped. No leading `"workflow "`
// chip survives.
func TestRenderWorkflowDetail_HeaderLine(t *testing.T) {
	cases := []struct {
		name string
		w    datasource.Workflow
		want string
	}{
		{
			name: "worktree",
			w:    datasource.Workflow{Name: "iso", Isolation: "worktree", FirstStep: "dev"},
			want: "iso [wt] first step: dev",
		},
		{
			name: "none_mode",
			w:    datasource.Workflow{Name: "plain", Isolation: "none", FirstStep: "dev"},
			want: "plain first step: dev",
		},
		{
			name: "empty_isolation",
			w:    datasource.Workflow{Name: "blank", Isolation: "", FirstStep: "dev"},
			want: "blank first step: dev",
		},
		{
			name: "synthetic_pinned_worktree_no_chip",
			w: datasource.Workflow{
				Name: "single:@autosk/dev", Isolation: "worktree",
				IsSynthetic: true, FirstStep: "do",
			},
			want: "single:@autosk/dev first step: do",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			visible := ansiutil.Strip(renderWorkflowDetail(c.w, 80))
			firstLine := strings.SplitN(visible, "\n", 2)[0]
			if firstLine != c.want {
				t.Errorf("header line mismatch:\n got: %q\nwant: %q", firstLine, c.want)
			}
		})
	}
}

// TestRenderWorkflowDetail_StepsBoxBorder pins the Steps box top
// border shape: `╭─ Steps (N) ` (same chrome as `Recent signals
// (N)` in renderTaskDetail).
func TestRenderWorkflowDetail_StepsBoxBorder(t *testing.T) {
	wf := datasource.Workflow{
		Name: "wf", FirstStep: "dev",
		Steps: []datasource.WorkflowStep{
			{Name: "dev", AgentName: "a1", NextSteps: []string{"review"}},
			{Name: "review", AgentName: "a2", NextStatus: []string{"done"}},
		},
	}
	out := renderWorkflowDetail(wf, 80)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "╭─ Steps (2) ") {
		t.Errorf("missing \"Steps (2)\" box header: %q", visible)
	}
	if !strings.Contains(visible, "╰─") {
		t.Errorf("missing rounded-bottom-left corner: %q", visible)
	}
}

// TestRenderWorkflowDetail_StepsAligned pins the column-alignment
// contract: with step names and agent names of varying widths,
// `agent=` and `next=` chips land at the same x on every row.
func TestRenderWorkflowDetail_StepsAligned(t *testing.T) {
	wf := datasource.Workflow{
		Name: "feature-dev-generic", FirstStep: "dev",
		Steps: []datasource.WorkflowStep{
			{Name: "dev", AgentName: "@autogent/generic", NextSteps: []string{"review"}},
			{Name: "review", AgentName: "@your-org/custom-agent", NextSteps: []string{"docs", "dev"}},
			{Name: "docs", AgentName: "@autogent/generic", NextSteps: []string{"validator"}},
			{Name: "validator", AgentName: "@autogent/generic", NextSteps: []string{"dev"}, NextStatus: []string{"human"}},
		},
	}
	out := renderWorkflowDetail(wf, 120)
	visible := ansiutil.Strip(out)

	// Pull just the step rows (frame + interior containing `agent=`).
	var rows []string
	for _, ln := range strings.Split(visible, "\n") {
		if !strings.HasPrefix(ln, "│ ") || !strings.HasSuffix(ln, " │") {
			continue
		}
		if !strings.Contains(ln, "agent=") {
			continue
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(ln, "│ "), " │")
		rows = append(rows, strings.TrimRight(inner, " "))
	}
	if len(rows) != len(wf.Steps) {
		t.Fatalf("expected %d step rows; got %d: %q", len(wf.Steps), len(rows), rows)
	}

	// `agent=` chip starts at the same x on every row.
	agentX := strings.Index(rows[0], "agent=")
	if agentX < 0 {
		t.Fatalf("row 0 has no agent= chip: %q", rows[0])
	}
	for i, r := range rows {
		if x := strings.Index(r, "agent="); x != agentX {
			t.Errorf("row %d: agent= chip x=%d, want %d: %q", i, x, agentX, r)
		}
	}
	// `next=` chip starts at the same x on every row.
	nextX := strings.Index(rows[0], "next=")
	if nextX < 0 {
		t.Fatalf("row 0 has no next= chip: %q", rows[0])
	}
	for i, r := range rows {
		if x := strings.Index(r, "next="); x != nextX {
			t.Errorf("row %d: next= chip x=%d, want %d: %q", i, x, nextX, r)
		}
	}

	// On the widest-step row, the column separator is the ONLY
	// thing between the rightmost char of the step name and
	// `agent=` — i.e. exactly one space, no double-space gutter.
	// Pick the row whose step name equals the widest known step
	// (`validator` in this fixture) and verify the prefix is
	// `validator ` (one space) immediately followed by `agent=`.
	wantPrefix := "validator agent="
	foundWidest := false
	for _, r := range rows {
		if strings.HasPrefix(r, "validator ") {
			foundWidest = true
			if !strings.HasPrefix(r, wantPrefix) {
				t.Errorf("widest-step row must use a single-space column separator before `agent=`; got %q, want prefix %q", r, wantPrefix)
			}
			break
		}
	}
	if !foundWidest {
		t.Fatalf("could not find row for widest step `validator` in %q", rows)
	}
}

// TestRenderWorkflowDetail_NextNone pins the `next=(none)` rendering
// for a terminal step whose NextSteps and NextStatus are both empty.
// `(none)` itself must wear styleMuted.
func TestRenderWorkflowDetail_NextNone(t *testing.T) {
	forceTrueColor(t)
	wf := datasource.Workflow{
		Name: "wf", FirstStep: "terminal",
		Steps: []datasource.WorkflowStep{
			{Name: "terminal", AgentName: "a1"},
		},
	}
	out := renderWorkflowDetail(wf, 80)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "next=(none)") {
		t.Errorf("missing \"next=(none)\" for terminal step: %q", visible)
	}
	// `(none)` must wear styleMuted (renderer wraps it explicitly).
	if !strings.Contains(out, styleMuted.Render("(none)")) {
		t.Errorf("\"(none)\" should wear styleMuted hue: %q", out)
	}
}

// TestRenderWorkflowDetail_NoTasksSubstrings is a defence-in-depth
// check for acceptance criterion #1: the per-step `tasks=N` chip
// and the top-level `tasks: N` line are both gone for every input
// shape, alongside the other legacy substrings.
func TestRenderWorkflowDetail_NoTasksSubstrings(t *testing.T) {
	inputs := []datasource.Workflow{
		{
			Name: "iso", Isolation: "worktree", FirstStep: "dev",
			TaskCount: 7, NonTerminalTaskCount: 3,
			Steps: []datasource.WorkflowStep{
				{Name: "dev", AgentName: "a1", TaskCount: 2, NextSteps: []string{"review"}},
				{Name: "review", AgentName: "a2", TaskCount: 5, NextStatus: []string{"done"}},
			},
		},
		{
			Name: "single:@autosk/dev", IsSynthetic: true, Isolation: "none", FirstStep: "do",
			Steps: []datasource.WorkflowStep{
				{Name: "do", AgentName: "@autosk/dev", TaskCount: 1},
			},
		},
		{
			Name: "plain", Isolation: "", FirstStep: "dev",
			Steps: []datasource.WorkflowStep{
				{Name: "dev", AgentName: "a1", TaskCount: 0},
			},
		},
	}
	banned := []string{
		"isolation:",
		"tasks:",
		"tasks=",
		"synthetic single-step workflow",
		"currently use this",
		"pinned: synthetic workflows always",
	}
	for _, w := range inputs {
		t.Run(w.Name, func(t *testing.T) {
			out := renderWorkflowDetail(w, 80)
			visible := ansiutil.Strip(out)
			for _, s := range banned {
				if strings.Contains(visible, s) {
					t.Errorf("output contains banned substring %q in:\n%s", s, visible)
				}
			}
			// Raw output (pre-ANSI-strip) must not lead with the
			// legacy `"workflow "` chip from styleHeader.Render.
			if strings.HasPrefix(visible, "workflow ") {
				t.Errorf("output should not lead with \"workflow \" chip:\n%s", visible)
			}
		})
	}
}

// TestRenderTaskDetail_SignalsCrossDayUsesFullDate pins the fix for
// the midnight-boundary bug: when a task's signal history spans more
// than one local day, every row in the signals box must render the
// full "YYYY-MM-DD HH:MM:SS" form so the operator can unambiguously
// tell which day each transition belongs to.
func TestRenderTaskDetail_SignalsCrossDayUsesFullDate(t *testing.T) {
	now := time.Date(2026, 5, 21, 14, 0, 0, 0, time.Local)
	yesterday := time.Date(2026, 5, 20, 23, 55, 0, 0, time.Local)
	signals := []datasource.Signal{
		{WorkflowName: "wf", StepName: "dev", Target: "review", CreatedAt: yesterday},
		{WorkflowName: "wf", StepName: "review", Target: "done", CreatedAt: now},
	}
	task := datasource.Task{ID: "ask-cd01", Status: store.StatusWork, CreatedAt: yesterday}
	out := renderTaskDetail(task, nil, signals, 80)
	visible := ansiutil.Strip(out)

	for _, ts := range []time.Time{yesterday, now} {
		stamp := timeformat.FormatDateTime(ts)
		if !strings.Contains(visible, stamp) {
			t.Errorf("missing full datetime %q in output:\n%s", stamp, visible)
		}
	}
}
