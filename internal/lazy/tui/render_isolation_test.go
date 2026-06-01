package tui

import (
	"strings"
	"testing"

	"autosk/internal/lazy/ansiutil"
	"autosk/internal/lazy/datasource"
)

// TestRenderWorkflowsPanel_WTMarker pins that the `[wt]` marker is
// emitted exactly for non-synthetic workflows whose Isolation =
// "worktree". Synthetic rows never carry the marker; rows with
// Isolation == "" or "none" likewise don't.
func TestRenderWorkflowsPanel_WTMarker(t *testing.T) {
	wfs := []datasource.Workflow{
		{Name: "iso-wf", Isolation: "worktree", FirstStep: "dev"},
		{Name: "plain-wf", Isolation: "none", FirstStep: "dev"},
		{Name: "empty-wf" /* Isolation: "" */, FirstStep: "dev"},
		{Name: "single:@autosk/dev", Isolation: "none", IsSynthetic: true, FirstStep: "do"},
	}
	out, _ := renderWorkflowsPanel(wfs, 0, "")
	visible := ansiutil.Strip(out)
	lines := strings.Split(strings.TrimRight(visible, "\n"), "\n")
	if len(lines) != len(wfs) {
		t.Fatalf("expected %d lines, got %d:\n%s", len(wfs), len(lines), visible)
	}
	// First row: iso-wf → contains [wt].
	if !strings.Contains(lines[0], "[wt]") {
		t.Errorf("iso-wf row missing [wt]:\n%q", lines[0])
	}
	// plain-wf and empty-wf and synthetic must NOT carry [wt].
	for i, name := range []string{"plain-wf", "empty-wf", "single:@autosk/dev"} {
		if strings.Contains(lines[i+1], "[wt]") {
			t.Errorf("%s row should NOT carry [wt]:\n%q", name, lines[i+1])
		}
	}
}

// TestRenderWorkflowsPanel_NonIsolatedUnchanged pins the existing
// behaviour: a workflow without isolation produces a row with no
// new visual surprises (no spurious marker tokens).
func TestRenderWorkflowsPanel_NonIsolatedUnchanged(t *testing.T) {
	wfs := []datasource.Workflow{
		{Name: "plain", Isolation: "none", FirstStep: "dev"},
	}
	out, _ := renderWorkflowsPanel(wfs, 0, "")
	visible := ansiutil.Strip(out)
	if strings.Contains(visible, "[wt]") {
		t.Errorf("non-isolated row should not carry [wt]:\n%q", visible)
	}
	// Plain row should still contain the workflow name + the muted
	// 'first=' chunk verbatim — pin the column anchors that existed
	// before the marker column was inserted.
	for _, want := range []string{"plain", "first="} {
		if !strings.Contains(visible, want) {
			t.Errorf("plain row missing %q: %q", want, visible)
		}
	}
}

// TestRenderWorkflowsPanel_GlobalMarkers verifies that [global],
// [stale], and [rev:X] badges appear in the Workflows panel.
func TestRenderWorkflowsPanel_GlobalMarkers(t *testing.T) {
	wfs := []datasource.Workflow{
		{Name: "global-up-to-date", SourceType: "global", Isolation: "none", FirstStep: "dev"},
		{Name: "global-stale", SourceType: "global", IsStale: true, Isolation: "none", FirstStep: "dev"},
		{Name: "global-with-rev", SourceType: "global", Revision: "rev-abc123", Isolation: "none", FirstStep: "dev"},
		{Name: "plain-local", Isolation: "none", FirstStep: "dev"},
	}
	out, _ := renderWorkflowsPanel(wfs, 0, "")
	visible := ansiutil.Strip(out)
	lines := strings.Split(strings.TrimRight(visible, "\n"), "\n")
	if len(lines) != len(wfs) {
		t.Fatalf("expected %d lines, got %d:\n%s", len(wfs), len(lines), visible)
	}
	if !strings.Contains(lines[0], "[global]") {
		t.Errorf("global-up-to-date missing [global]:\n%q", lines[0])
	}
	if strings.Contains(lines[0], "[stale]") {
		t.Errorf("global-up-to-date should NOT carry [stale]:\n%q", lines[0])
	}
	if !strings.Contains(lines[1], "[global]") {
		t.Errorf("global-stale missing [global]:\n%q", lines[1])
	}
	if !strings.Contains(lines[1], "[stale]") {
		t.Errorf("global-stale missing [stale]:\n%q", lines[1])
	}
	if !strings.Contains(lines[2], "[rev:rev-abc123]") {
		t.Errorf("global-with-rev missing [rev:rev-abc123]:\n%q", lines[2])
	}
	if strings.Contains(lines[3], "[global]") || strings.Contains(lines[3], "[stale]") {
		t.Errorf("plain-local should NOT carry global/stale markers:\n%q", lines[3])
	}
}

// TestRenderWorkflowDetail_OriginSection verifies that the Detail
// pane renders source type, source, revision, and hash for managed
// workflows, and uses styleWarn for [stale].
func TestRenderWorkflowDetail_OriginSection(t *testing.T) {
	w := datasource.Workflow{
		Name: "global-wf", SourceType: "global", Source: "global-wf",
		DefinitionHash: "abc123", Revision: "rev-1",
		Isolation: "none", FirstStep: "dev",
	}
	out := renderWorkflowDetail(w, 80)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "source: global") {
		t.Errorf("missing source line:\n%s", visible)
	}
	if !strings.Contains(visible, "(global-wf)") {
		t.Errorf("missing source name:\n%s", visible)
	}
	if !strings.Contains(visible, "revision: rev-1") {
		t.Errorf("missing revision line:\n%s", visible)
	}
	if !strings.Contains(visible, "hash: abc123") {
		t.Errorf("missing hash line:\n%s", visible)
	}
	// Revision chip in header.
	if !strings.Contains(visible, "[rev:rev-1]") {
		t.Errorf("missing [rev:rev-1] chip in header:\n%s", visible)
	}

	// Stale workflow: [global] and [stale] chips appear in header.
	w.IsStale = true
	out = renderWorkflowDetail(w, 80)
	visible = ansiutil.Strip(out)
	if !strings.Contains(visible, "[global]") {
		t.Errorf("stale workflow missing [global] chip:\n%s", visible)
	}
	if !strings.Contains(visible, "[stale]") {
		t.Errorf("stale workflow missing [stale] chip:\n%s", visible)
	}

	// Long revision is truncated to 12 chars in the header chip.
	w.IsStale = false
	w.Revision = "a-very-long-revision-string"
	out = renderWorkflowDetail(w, 80)
	visible = ansiutil.Strip(out)
	if !strings.Contains(visible, "[rev:a-very-long-]") {
		t.Errorf("long revision not truncated to 12 chars in header; got:\n%s", visible)
	}

	// Local workflow: no origin lines.
	local := datasource.Workflow{Name: "local", Isolation: "none", FirstStep: "dev"}
	out = renderWorkflowDetail(local, 80)
	visible = ansiutil.Strip(out)
	if strings.Contains(visible, "source:") {
		t.Errorf("local workflow should NOT render source line:\n%s", visible)
	}
}

// TestRenderWorkflowDetail_IsolationLine pins the new header-chip
// behaviour for the workflow Detail pane:
//
//   - the `[wt]` chip appears IFF !w.IsSynthetic &&
//     w.Isolation == "worktree";
//   - synthetic rows never carry the chip, regardless of their
//     pinned isolation value;
//   - the legacy `isolation: <mode>` line, the
//     `... currently use this` suffix, the
//     `synthetic single-step workflow` line, the
//     `pinned: synthetic workflows always 'none'` suffix and the
//     top-level `tasks:` / per-step `tasks=` chips are all gone.
//
// Run as a single table-driven test so every input case has the
// banned-substring assertions applied in lockstep with the
// chip-presence check.
func TestRenderWorkflowDetail_IsolationLine(t *testing.T) {
	cases := []struct {
		name     string
		w        datasource.Workflow
		wantChip bool
	}{
		{
			name: "isolated_worktree",
			w: datasource.Workflow{
				Name: "iso", Isolation: "worktree", FirstStep: "dev",
			},
			wantChip: true,
		},
		{
			name: "isolated_worktree_with_tasks",
			w: datasource.Workflow{
				Name: "iso", Isolation: "worktree", FirstStep: "dev",
				NonTerminalTaskCount: 3,
			},
			wantChip: true,
		},
		{
			name: "plain_none",
			w: datasource.Workflow{
				Name: "plain", Isolation: "none", FirstStep: "dev",
			},
			wantChip: false,
		},
		{
			name: "plain_empty_string",
			w: datasource.Workflow{
				Name: "plain", Isolation: "", FirstStep: "dev",
			},
			wantChip: false,
		},
		{
			name: "synthetic_worktree_no_chip",
			w: datasource.Workflow{
				Name: "single:@autosk/dev", Isolation: "worktree", IsSynthetic: true, FirstStep: "do",
			},
			wantChip: false,
		},
		{
			name: "synthetic_none",
			w: datasource.Workflow{
				Name: "single:@autosk/dev", Isolation: "none", IsSynthetic: true, FirstStep: "do",
			},
			wantChip: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := renderWorkflowDetail(c.w, 80)
			visible := ansiutil.Strip(out)
			if got := strings.Contains(visible, "[wt]"); got != c.wantChip {
				t.Errorf("[wt] chip presence = %v, want %v in:\n%s", got, c.wantChip, visible)
			}
			// None of the legacy substrings may appear for any input.
			for _, banned := range []string{
				"isolation:",
				"currently use this",
				"synthetic single-step workflow",
				"pinned: synthetic workflows always",
				"tasks:",
				"tasks=",
			} {
				if strings.Contains(visible, banned) {
					t.Errorf("output contains banned substring %q in:\n%s", banned, visible)
				}
			}
			// And the raw output must not lead with a `"workflow "` chip
			// (the old styleHeader.Render("workflow") prefix).
			if strings.HasPrefix(visible, "workflow ") {
				t.Errorf("output should not start with legacy \"workflow \" chip:\n%s", visible)
			}
		})
	}
}
