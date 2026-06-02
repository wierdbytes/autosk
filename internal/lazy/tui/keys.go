package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
)

// bindingSpec describes one TUI keybinding plus the metadata the
// cheatsheet popup and the bottom options strip need to render it.
//
// View / Key / Mod / Handler are the gocui plumbing: bindKeys()
// iterates the slice and pushes each entry through g.SetKeybinding.
// The remaining fields drive the cheatsheet and options-strip
// renderers:
//
//   - Description: human-readable label shown in the cheatsheet's
//     description column. Empty means the binding is hidden from
//     the cheatsheet (use for redundant aliases like Ctrl-C/q quit,
//     or for popup-scoped keys that aren't part of the user's
//     dashboard surface).
//   - Short: optional compact label for the options strip. When
//     empty the strip falls back to Description.
//   - Tag: bucket the cheatsheet groups the entry under. "global"
//     means the binding is in the Global bucket regardless of
//     View. "navigation" means the binding is a per-panel viewport
//     verb (j/k/arrows/page/wheel) and goes into the Navigation
//     bucket. Empty Tag is treated as Local (panel-specific
//     action).
//   - DisplayOnScreen: true to surface the binding on the bottom
//     options strip for whichever panel is focused. Plan picks a
//     small set of high-traffic verbs so the strip stays scannable.
type bindingSpec struct {
	View    string
	Key     any
	Mod     gocui.Modifier
	Handler func(*gocui.Gui, *gocui.View) error

	Description     string
	Short           string
	Tag             string
	DisplayOnScreen bool
}

// bindKeys wires every keybinding the TUI uses. Bindings are
// per-view: the global ones bind ViewName="" (all views).
//
// We split into three tiers:
//
//   - globals (quit, tab switching, palette, filter, help, scope clear)
//   - per-panel (Tasks/Jobs/Workflows/Agents list nav + write verbs)
//   - Detail pane scroll (winDetail) + Job-input textarea
//     (winJobInput) when a running job is selected
//
// The slice is materialised by bindingSpecs(); bindKeys() is the
// thin loop that registers each entry with gocui.
func (gu *Gui) bindKeys() error {
	for _, b := range gu.bindingSpecs() {
		if err := gu.g.SetKeybinding(b.View, b.Key, b.Mod, b.Handler); err != nil {
			return err
		}
	}
	return nil
}

// bindingSpecs returns the full registry of dashboard keybindings
// plus their cheatsheet / options-strip metadata. Re-built on every
// call (handler closures over `gu` are constructed in-line).
// Callers: bindKeys() (once at startup), openHelp (once per popup
// open), and renderOptionsStrip via renderViews (every redraw —
// per input event and per refresh tick, ~80 closure allocations
// per redraw). That's well within the budget for terminal-frame
// rebuilds; if the strip ever becomes a measurable hotspot the
// slice can be memoised on *Gui (the closures over `gu` are stable
// for the lifetime of the Gui).
func (gu *Gui) bindingSpecs() []bindingSpec {
	return []bindingSpec{
		// global quit + help. Space is intentionally NOT global — it
		// has per-panel semantics (Tasks: commit scope) and a global
		// space binding would swallow the per-view ones underneath.
		//
		// Ctrl-C and `q` both quit; only `q` carries the cheatsheet
		// Description so the row isn't duplicated (lazygit's
		// uniqueBindings dedup at registration time).
		{View: "", Key: gocui.KeyCtrlC, Mod: gocui.ModNone, Handler: gu.quit, Tag: "global"},
		{View: "", Key: 'q', Mod: gocui.ModNone, Handler: gu.quit, Description: "quit", Short: "quit", Tag: "global", DisplayOnScreen: true},
		{View: "", Key: '?', Mod: gocui.ModNone, Handler: gu.openHelp, Description: "help cheatsheet", Short: "help", Tag: "global", DisplayOnScreen: true},
		// Ctrl-W is the "what's new" re-opener: pops the changelog
		// modal with the FULL embedded CHANGELOG.md regardless of
		// `last_seen_changelog`, and does NOT mutate state.json on
		// dismiss. The auto-popup that fires once per release on
		// the first start of a new release uses the same
		// popupChangelog kind but carries an OnDismiss callback;
		// see internal/changelog and cmd/autosk/lazy.go.
		{View: "", Key: gocui.KeyCtrlW, Mod: gocui.ModNone, Handler: gu.handleWhatsNew, Description: "what's new (open CHANGELOG.md)", Short: "what's new", Tag: "global"},
		// Esc is universal across the TUI: it closes the active popup
		// (or, with no popup open, clears the focused panel's filter).
		// Deliberately NOT cheatsheet-visible: selecting a "back /
		// close" row in the cheatsheet would route through handleEsc
		// AFTER the popup is already cleared by cheatsheetAccept,
		// which would wipe the focused panel's filter as a destructive
		// side effect the operator did not signal. Discoverable by
		// trying Esc once.
		{View: "", Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.handleEsc, Tag: "global"},
		// panel switching
		{View: "", Key: '1', Mod: gocui.ModNone, Handler: gu.focusPanel(panelTasks), Description: "focus Tasks", Tag: "global"},
		{View: "", Key: '2', Mod: gocui.ModNone, Handler: gu.focusPanel(panelJobs), Description: "focus Jobs", Tag: "global"},
		{View: "", Key: '3', Mod: gocui.ModNone, Handler: gu.focusPanel(panelWorkflows), Description: "focus Workflows", Tag: "global"},
		{View: "", Key: '4', Mod: gocui.ModNone, Handler: gu.focusPanel(panelAgents), Description: "focus Agents", Tag: "global"},
		{View: "", Key: '0', Mod: gocui.ModNone, Handler: gu.focusPanel(panelDetail), Description: "focus Detail", Tag: "global"},
		{View: "", Key: gocui.KeyTab, Mod: gocui.ModNone, Handler: gu.cyclePanel(+1), Description: "cycle side panel", Tag: "global"},
		// refresh + scope + filter + palette
		{View: "", Key: 'R', Mod: gocui.ModNone, Handler: gu.handleRefresh, Description: "refresh now", Tag: "global"},
		// Ctrl-R is the "hard refresh": force the doltlite store to drop
		// its pooled connection (recover from a cross-process dolt_gc
		// that atomic-rewrote .autosk/db). Plain R alone fires the same
		// adaptive tick the 2s timer uses — useful as a one-shot kick;
		// Ctrl-R is the escape hatch when the operator suspects the
		// underlying fd is on an orphan inode.
		{View: "", Key: gocui.KeyCtrlR, Mod: gocui.ModNone, Handler: gu.handleForceReconnect, Description: "hard refresh (reopen db conn)", Tag: "global"},
		{View: "", Key: '*', Mod: gocui.ModNone, Handler: gu.handleClearScope, Description: "clear scope chips", Short: "clear scope", Tag: "global", DisplayOnScreen: true},
		{View: "", Key: '/', Mod: gocui.ModNone, Handler: gu.openFilter, Description: "filter focused panel", Short: "filter", Tag: "global", DisplayOnScreen: true},
		{View: "", Key: ':', Mod: gocui.ModNone, Handler: gu.openPalette, Description: "command palette", Short: "palette", Tag: "global", DisplayOnScreen: true},
		{View: "", Key: '@', Mod: gocui.ModNone, Handler: gu.toggleLog, Description: "toggle log", Tag: "global"},

		// list nav (per panel; gocui binds per-view, so we attach once
		// per side panel + the detail pane). Only j/k carry the
		// cheatsheet Description — arrow keys + wheel are equivalent
		// aliases and would just duplicate the Navigation bucket if
		// they each surfaced their own row.
		{View: winTasks, Key: 'j', Mod: gocui.ModNone, Handler: gu.cursorDown(panelTasks), Description: "down", Tag: "navigation"},
		{View: winTasks, Key: 'k', Mod: gocui.ModNone, Handler: gu.cursorUp(panelTasks), Description: "up", Tag: "navigation"},
		{View: winTasks, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.cursorDown(panelTasks), Tag: "navigation"},
		{View: winTasks, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.cursorUp(panelTasks), Tag: "navigation"},
		{View: winTasks, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.tasksEnter, Description: "filter Jobs + focus"},
		// Space — commit the cursor row as the Jobs-panel scope chip
		// without leaving the Tasks panel. The counterpart to Enter
		// (which commits + jumps). j/k do NOT auto-commit anymore
		// (operator complaint: cursor-driven re-filtering was noisy),
		// so this is the only path on Tasks for setting scope.TaskID.
		// Press * to clear.
		{View: winTasks, Key: ' ', Mod: gocui.ModNone, Handler: gu.tasksScopeFromCursor, Description: "set Jobs scope to selected task"},

		{View: winJobs, Key: 'j', Mod: gocui.ModNone, Handler: gu.cursorDown(panelJobs), Description: "down", Tag: "navigation"},
		{View: winJobs, Key: 'k', Mod: gocui.ModNone, Handler: gu.cursorUp(panelJobs), Description: "up", Tag: "navigation"},
		{View: winJobs, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.cursorDown(panelJobs), Tag: "navigation"},
		{View: winJobs, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.cursorUp(panelJobs), Tag: "navigation"},
		{View: winJobs, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.jobsEnter, Description: "focus input (live) / Detail (terminal)"},

		{View: winWorkflows, Key: 'j', Mod: gocui.ModNone, Handler: gu.cursorDown(panelWorkflows), Description: "down", Tag: "navigation"},
		{View: winWorkflows, Key: 'k', Mod: gocui.ModNone, Handler: gu.cursorUp(panelWorkflows), Description: "up", Tag: "navigation"},
		{View: winWorkflows, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.cursorDown(panelWorkflows), Tag: "navigation"},
		{View: winWorkflows, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.cursorUp(panelWorkflows), Tag: "navigation"},
		{View: winWorkflows, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.workflowsEnter, Description: "filter Tasks by workflow + focus"},

		{View: winAgents, Key: 'j', Mod: gocui.ModNone, Handler: gu.cursorDown(panelAgents), Description: "down", Tag: "navigation"},
		{View: winAgents, Key: 'k', Mod: gocui.ModNone, Handler: gu.cursorUp(panelAgents), Description: "up", Tag: "navigation"},
		{View: winAgents, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.cursorDown(panelAgents), Tag: "navigation"},
		{View: winAgents, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.cursorUp(panelAgents), Tag: "navigation"},
		{View: winAgents, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.agentsEnter, Description: "agent-scope picker"},

		// Tasks write verbs.
		{View: winTasks, Key: 'n', Mod: gocui.ModNone, Handler: gu.taskNew, Description: "new task", Short: "new", DisplayOnScreen: true},
		{View: winTasks, Key: 'c', Mod: gocui.ModNone, Handler: gu.taskEdit, Description: "edit task", Short: "edit", DisplayOnScreen: true},
		{View: winTasks, Key: 'd', Mod: gocui.ModNone, Handler: gu.taskDone, Description: "mark done", Short: "done", DisplayOnScreen: true},
		{View: winTasks, Key: 'x', Mod: gocui.ModNone, Handler: gu.taskCancel, Description: "cancel task", Short: "cancel", DisplayOnScreen: true},
		{View: winTasks, Key: 'e', Mod: gocui.ModNone, Handler: gu.taskEnroll, Description: "enroll into workflow", Short: "enroll", DisplayOnScreen: true},
		{View: winTasks, Key: 'r', Mod: gocui.ModNone, Handler: gu.taskResume, Description: "resume (human → work)", Short: "resume", DisplayOnScreen: true},
		{View: winTasks, Key: 'b', Mod: gocui.ModNone, Handler: gu.taskBlock, Description: "add blocker", Short: "block", DisplayOnScreen: true},
		{View: winTasks, Key: 'u', Mod: gocui.ModNone, Handler: gu.taskUnblock, Description: "remove blocker", Short: "unblock", DisplayOnScreen: true},
		{View: winTasks, Key: 'm', Mod: gocui.ModNone, Handler: gu.taskComment, Description: "add comment", Short: "comment", DisplayOnScreen: true},
		{View: winTasks, Key: 'M', Mod: gocui.ModNone, Handler: gu.taskMetadataEdit, Description: "edit metadata", Short: "metadata", DisplayOnScreen: true},
		{View: winTasks, Key: 'p', Mod: gocui.ModNone, Handler: gu.taskPriority, Description: "set priority", Short: "prio", DisplayOnScreen: true},
		{View: winTasks, Key: 'o', Mod: gocui.ModNone, Handler: gu.taskReopen, Description: "reopen task", Short: "reopen", DisplayOnScreen: true},
		// Note: there is no `c claim` binding. The v0.2 schema has no
		// claim verb — tasks self-advance via workflow steps. `c` is
		// bound to `change` (edit title + description in the two-pane
		// compose); use `e` to enroll instead.

		// Workflows write verbs.
		{View: winWorkflows, Key: 'n', Mod: gocui.ModNone, Handler: gu.workflowNew, Description: "new workflow (from file)", Short: "new", DisplayOnScreen: true},
		{View: winWorkflows, Key: 'D', Mod: gocui.ModNone, Handler: gu.workflowDelete, Description: "delete workflow", Short: "delete", DisplayOnScreen: true},
		{View: winWorkflows, Key: 'i', Mod: gocui.ModNone, Handler: gu.workflowIsolation, Description: "toggle isolation", Short: "iso", DisplayOnScreen: true},
		{View: winWorkflows, Key: 's', Mod: gocui.ModNone, Handler: gu.workflowSync, Description: "sync global workflows", Short: "sync", DisplayOnScreen: true},

		// Agents "write" verbs are intentionally informational only:
		// the daemon has no /v1/agents endpoint in v1, so both keys
		// flash a toast directing the operator at the corresponding
		// `autosk agent {install|uninstall} <pkg>` CLI invocation. The
		// bindings stay so the help screen's 'i install / u uninstall'
		// row is honest and discoverable.
		{View: winAgents, Key: 'i', Mod: gocui.ModNone, Handler: gu.agentInstall, Description: "install (via CLI hint)", Short: "install", DisplayOnScreen: true},
		{View: winAgents, Key: 'u', Mod: gocui.ModNone, Handler: gu.agentUninstall, Description: "uninstall (via CLI hint)", Short: "uninstall", DisplayOnScreen: true},

		// Jobs hotkeys.
		{View: winJobs, Key: 'K', Mod: gocui.ModNone, Handler: gu.jobCancel, Description: "cancel job", Short: "kill", DisplayOnScreen: true},

		// Detail pane scroll bindings. Same shape that used to live on
		// the Inspector body view — j/k single line, Ctrl-F/Ctrl-B page,
		// g/G start/end. Applies to BOTH task-detail and job-detail
		// content (the pane is one view; the renderer picks the body
		// based on focused panel).
		{View: winDetail, Key: 'j', Mod: gocui.ModNone, Handler: gu.detailScroll(+1), Description: "line down", Tag: "navigation"},
		{View: winDetail, Key: 'k', Mod: gocui.ModNone, Handler: gu.detailScroll(-1), Description: "line up", Tag: "navigation"},
		{View: winDetail, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.detailScroll(+1), Tag: "navigation"},
		{View: winDetail, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.detailScroll(-1), Tag: "navigation"},
		{View: winDetail, Key: gocui.KeyCtrlF, Mod: gocui.ModNone, Handler: gu.detailScrollPage(+1), Description: "page down", Tag: "navigation"},
		{View: winDetail, Key: gocui.KeyCtrlB, Mod: gocui.ModNone, Handler: gu.detailScrollPage(-1), Description: "page up", Tag: "navigation"},
		{View: winDetail, Key: gocui.KeyPgdn, Mod: gocui.ModNone, Handler: gu.detailScrollPage(+1), Tag: "navigation"},
		{View: winDetail, Key: gocui.KeyPgup, Mod: gocui.ModNone, Handler: gu.detailScrollPage(-1), Tag: "navigation"},
		{View: winDetail, Key: 'g', Mod: gocui.ModNone, Handler: gu.detailScrollTo(false), Description: "top", Tag: "navigation"},
		{View: winDetail, Key: 'G', Mod: gocui.ModNone, Handler: gu.detailScrollTo(true), Description: "bottom", Tag: "navigation"},

		// Job-input bindings (relocated from the old fullscreen-inspector input view).
		// Bound only on winJobInput; the view itself only exists when
		// the selected job is running, so these bindings are dormant
		// otherwise.
		{View: winJobInput, Key: gocui.KeyCtrlD, Mod: gocui.ModNone, Handler: gu.liveSend, Description: "send"},
		{View: winJobInput, Key: gocui.KeyCtrlF, Mod: gocui.ModNone, Handler: gu.liveFollowUp, Description: "follow up"},
		{View: winJobInput, Key: gocui.KeyCtrlA, Mod: gocui.ModNone, Handler: gu.liveAbort, Description: "abort agent turn"},
		{View: winJobInput, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.jobInputEscape, Description: "cancel input"},
		{View: winJobInput, Key: gocui.KeyCtrlC, Mod: gocui.ModNone, Handler: gu.quit},
		// Scroll the Detail pane above without leaving the textarea.
		{View: winJobInput, Key: gocui.KeyCtrlB, Mod: gocui.ModNone, Handler: gu.detailScrollPage(-1), Description: "page up Detail", Tag: "navigation"},
		{View: winJobInput, Key: gocui.KeyPgup, Mod: gocui.ModNone, Handler: gu.detailScrollPage(-1), Tag: "navigation"},
		{View: winJobInput, Key: gocui.KeyPgdn, Mod: gocui.ModNone, Handler: gu.detailScrollPage(+1), Description: "page down Detail", Tag: "navigation"},
		{View: winJobInput, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.detailScroll(+1), Tag: "navigation"},
		{View: winJobInput, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.detailScroll(-1), Tag: "navigation"},

		// Mouse-wheel scroll bindings. gocui surfaces wheel events as
		// MouseWheelUp/Down keys per-view; without these bindings the
		// terminal's wheel forwarding is dropped on the floor and
		// overflowing content (Detail-pane transcripts, lists) is
		// only reachable via j/k or PgUp/PgDn.
		//
		// Side panels: wheel scrolls the VIEWPORT without moving the
		// selection — lazygit's convention (see
		// pkg/gui/controllers/list_controller.go:HandleScrollUp/Down).
		// j/k after a wheel-scroll snap the viewport back so the
		// selection is visible again (scrollOffAdjust's centring path).
		{View: winTasks, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.panelScroll(panelTasks, +panelScrollStep), Tag: "navigation"},
		{View: winTasks, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.panelScroll(panelTasks, -panelScrollStep), Tag: "navigation"},
		{View: winJobs, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.panelScroll(panelJobs, +panelScrollStep), Tag: "navigation"},
		{View: winJobs, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.panelScroll(panelJobs, -panelScrollStep), Tag: "navigation"},
		{View: winWorkflows, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.panelScroll(panelWorkflows, +panelScrollStep), Tag: "navigation"},
		{View: winWorkflows, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.panelScroll(panelWorkflows, -panelScrollStep), Tag: "navigation"},
		{View: winAgents, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.panelScroll(panelAgents, +panelScrollStep), Tag: "navigation"},
		{View: winAgents, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.panelScroll(panelAgents, -panelScrollStep), Tag: "navigation"},

		// Detail pane: wheel scrolls the viewport (no cursor concept).
		{View: winDetail, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.detailScroll(+1), Tag: "navigation"},
		{View: winDetail, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.detailScroll(-1), Tag: "navigation"},

		// Popup menus also benefit from wheel-driven cursor motion.
		{View: winPopupMenu, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.popupCursor(+1)},
		{View: winPopupMenu, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.popupCursor(-1)},

		// Popup keys.
		{View: winPopupMenu, Key: 'j', Mod: gocui.ModNone, Handler: gu.popupCursor(+1)},
		{View: winPopupMenu, Key: 'k', Mod: gocui.ModNone, Handler: gu.popupCursor(-1)},
		{View: winPopupMenu, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.popupCursor(+1)},
		{View: winPopupMenu, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.popupCursor(-1)},
		{View: winPopupMenu, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.popupAccept},
		{View: winPopupMenu, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},
		{View: winPopupConfirm, Key: 'y', Mod: gocui.ModNone, Handler: gu.popupConfirmYes},
		{View: winPopupConfirm, Key: 'Y', Mod: gocui.ModNone, Handler: gu.popupConfirmYes},
		{View: winPopupConfirm, Key: 'n', Mod: gocui.ModNone, Handler: gu.popupClose},
		{View: winPopupConfirm, Key: 'N', Mod: gocui.ModNone, Handler: gu.popupClose},
		{View: winPopupConfirm, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.popupConfirmYes},
		{View: winPopupConfirm, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},
		{View: winPopupPrompt, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.popupAccept},
		{View: winPopupPrompt, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},

		// Task-compose popup keys (lazygit commit-message editor parity).
		//
		// Summary pane: Enter submits (so the editor never sees the
		// keystroke and can't insert "\n" into a single-line title),
		// Tab toggles to Description, Esc closes, Ctrl+S also submits
		// for symmetry with the description pane.
		//
		// Description pane: Enter is INTENTIONALLY NOT bound here —
		// gocui falls through to SimpleEditor which inserts "\n", which
		// is the whole point of having a description pane. Submitting
		// requires Ctrl+S. Tab toggles back to Summary, Esc closes.
		{View: winTaskComposeSummary, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.taskComposeConfirm},
		{View: winTaskComposeSummary, Key: gocui.KeyCtrlS, Mod: gocui.ModNone, Handler: gu.taskComposeConfirm},
		{View: winTaskComposeSummary, Key: gocui.KeyTab, Mod: gocui.ModNone, Handler: gu.taskComposeToggle},
		{View: winTaskComposeSummary, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},
		{View: winTaskComposeDescription, Key: gocui.KeyCtrlS, Mod: gocui.ModNone, Handler: gu.taskComposeConfirm},
		{View: winTaskComposeDescription, Key: gocui.KeyTab, Mod: gocui.ModNone, Handler: gu.taskComposeToggle},
		{View: winTaskComposeDescription, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},

		// Single-pane multi-line compose popup (comment / metadata).
		//
		// Plain Enter is intentionally NOT bound — it falls through to
		// gocui.DefaultEditor which inserts "\n", which is the whole
		// point of the multi-line popup. Submitting requires Ctrl+S.
		// Esc cancels without invoking OnAccept.
		{View: winSingleCompose, Key: gocui.KeyCtrlS, Mod: gocui.ModNone, Handler: gu.singleComposeConfirm},
		{View: winSingleCompose, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},
		{View: winSingleCompose, Key: gocui.KeyCtrlC, Mod: gocui.ModNone, Handler: gu.quit},

		// Enroll / resume two-pane picker.
		//
		// Workflow pane: j/k/arrows + mouse wheel move the cursor and
		// re-render the step pane (no datasource call). Enter confirms
		// the workflow and moves focus to the step pane (with the step
		// cursor reset to 0). Esc closes the popup without dispatching.
		{View: winEnrollWorkflowList, Key: 'j', Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneWorkflow, +1)},
		{View: winEnrollWorkflowList, Key: 'k', Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneWorkflow, -1)},
		{View: winEnrollWorkflowList, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneWorkflow, +1)},
		{View: winEnrollWorkflowList, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneWorkflow, -1)},
		{View: winEnrollWorkflowList, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneWorkflow, +1)},
		{View: winEnrollWorkflowList, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneWorkflow, -1)},
		{View: winEnrollWorkflowList, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.enrollPickerWorkflowAccept},
		{View: winEnrollWorkflowList, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},

		// Step pane: j/k/arrows + mouse wheel move the cursor. Enter
		// fires OnPick with (workflow, step) names. Esc returns focus
		// to the workflow pane (preserving the workflow cursor); on
		// the resume flow (WorkflowLocked) Esc falls through to
		// popupClose instead because there is no workflow pane to
		// return to.
		{View: winEnrollStepList, Key: 'j', Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneStep, +1)},
		{View: winEnrollStepList, Key: 'k', Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneStep, -1)},
		{View: winEnrollStepList, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneStep, +1)},
		{View: winEnrollStepList, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneStep, -1)},
		{View: winEnrollStepList, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneStep, +1)},
		{View: winEnrollStepList, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneStep, -1)},
		{View: winEnrollStepList, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.enrollPickerStepAccept},
		{View: winEnrollStepList, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.enrollPickerStepEscape},

		// Cheatsheet popup (winPopupCheatsheet). The view is editable
		// so printable runes flow through its Editor and append to
		// state.popup.CheatsheetFilter; non-char keys (Enter / Esc /
		// Backspace / arrows / wheel) are routed via these view-scoped
		// keybindings. gocui's matchView rejects char bindings on
		// editable views, so we don't need to enumerate every printable
		// rune here — the editor is the one source of truth for them.
		{View: winPopupCheatsheet, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.cheatsheetAccept},
		{View: winPopupCheatsheet, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.cheatsheetEscape},
		{View: winPopupCheatsheet, Key: gocui.KeyBackspace, Mod: gocui.ModNone, Handler: gu.cheatsheetBackspace},
		{View: winPopupCheatsheet, Key: gocui.KeyBackspace2, Mod: gocui.ModNone, Handler: gu.cheatsheetBackspace},
		{View: winPopupCheatsheet, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.cheatsheetCursor(+1)},
		{View: winPopupCheatsheet, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.cheatsheetCursor(-1)},
		{View: winPopupCheatsheet, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.cheatsheetCursor(+1)},
		{View: winPopupCheatsheet, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.cheatsheetCursor(-1)},

		// Changelog popup (winPopupChangelog). Same scroll key set as
		// the Detail pane (j/k/arrows/ctrl-f/ctrl-b/pgup/pgdn/g/G +
		// mouse wheel) so the operator browses the body with muscle
		// memory; Esc / Enter dismisses (firing OnDismissChangelog
		// once on the auto-popup path so last_seen_changelog gets
		// stamped into ~/.autosk/state.json). The bindings carry no
		// Description so they don't pollute the cheatsheet — the
		// popup itself shows a scroll/dismiss hint in its top frame.
		{View: winPopupChangelog, Key: 'j', Mod: gocui.ModNone, Handler: gu.changelogScroll(+1)},
		{View: winPopupChangelog, Key: 'k', Mod: gocui.ModNone, Handler: gu.changelogScroll(-1)},
		{View: winPopupChangelog, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.changelogScroll(+1)},
		{View: winPopupChangelog, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.changelogScroll(-1)},
		{View: winPopupChangelog, Key: gocui.KeyCtrlF, Mod: gocui.ModNone, Handler: gu.changelogScrollPage(+1)},
		{View: winPopupChangelog, Key: gocui.KeyCtrlB, Mod: gocui.ModNone, Handler: gu.changelogScrollPage(-1)},
		{View: winPopupChangelog, Key: gocui.KeyPgdn, Mod: gocui.ModNone, Handler: gu.changelogScrollPage(+1)},
		{View: winPopupChangelog, Key: gocui.KeyPgup, Mod: gocui.ModNone, Handler: gu.changelogScrollPage(-1)},
		{View: winPopupChangelog, Key: 'g', Mod: gocui.ModNone, Handler: gu.changelogScrollTo(false)},
		{View: winPopupChangelog, Key: 'G', Mod: gocui.ModNone, Handler: gu.changelogScrollTo(true)},
		{View: winPopupChangelog, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.changelogScroll(+1)},
		{View: winPopupChangelog, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.changelogScroll(-1)},
		{View: winPopupChangelog, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.changelogDismiss},
		{View: winPopupChangelog, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.changelogDismiss},
	}
}

// focusPanel returns a handler that focuses the named panel. After
// focus moves we snap the new panel's viewport so its selected row
// is visible — the user may have wheel-scrolled it out of view
// during the previous visit and now expects to see where they were.
// Matches lazygit's HandleFocus(ScrollSelectionIntoView=true).
//
// When p is panelDetail we also stash the OUTGOING focus into
// state.detailFocus so renderDetail can keep rendering the right
// entity body (the panelDetail arm itself has nothing to draw).
// The stash normalises panelJobInput → panelJobs so a transition
// from the input flow into the Detail pane still renders Job
// Detail (renderDetail also normalises on the read side, but the
// model field stays sensible this way too).
func (gu *Gui) focusPanel(p panelID) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		changed := false
		gu.st.withLock(func() {
			if gu.st.focused != p {
				changed = true
			}
			if p == panelDetail && gu.st.focused != panelDetail {
				gu.st.detailFocus = gu.st.focused.normalizeForDetail()
			}
			gu.st.focused = p
		})
		if changed {
			gu.snapViewportToCursor(p)
			if p == panelJobs {
				// Hydrate the transcript cache for the now-selected job.
				// Without this the Detail pane stays on "(loading...)"
				// until the operator presses j/k — see settleOnPanelJobs.
				gu.settleOnPanelJobs()
			}
		}
		return nil
	}
}

// cyclePanel cycles through the four side panels by step.
//
// Source-side normalisation: cyclePanel may fire from any view,
// including the synthetic panelJobInput (caret in winJobInput) and
// panelDetail (focus parked on the right pane via '0'). Neither is
// in the 0..3 cycle range, so we normalise them first —
// panelJobInput→panelJobs (predictable: the operator was working a
// running job, so Tab steps OFF the job they were composing for to
// the next side panel), panelDetail→detailFocus (the side panel
// that owned the selection before '0' was pressed). Without this
// normalisation, (int(panelJobInput=5) + 1) % 4 = 2 = panelWorkflows
// would skip the Jobs column entirely on Tab from the input — a
// UX regression vs. the pre-redesign state where Tab inside the
// fullscreen inspector was scoped to inspector-tab cycling.
func (gu *Gui) cyclePanel(step int) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		var landed panelID
		changed := false
		gu.st.withLock(func() {
			src := gu.st.focused
			switch src {
			case panelJobInput:
				src = panelJobs
			case panelDetail:
				src = gu.st.detailFocus.normalizeForDetail()
				if src == panelDetail {
					src = panelTasks
				}
			}
			n := 4
			next := (int(src) + step + n) % n
			if int(gu.st.focused) != next {
				changed = true
			}
			gu.st.focused = panelID(next)
			landed = panelID(next)
		})
		if changed {
			gu.snapViewportToCursor(landed)
			if landed == panelJobs {
				// Same hydration reason as focusPanel: Tab/Shift-Tab
				// landing on Jobs without moving the cursor would
				// otherwise leave the Detail pane stuck on
				// "(loading...)". See settleOnPanelJobs.
				gu.settleOnPanelJobs()
			}
		}
		return nil
	}
}

// cursorDown / cursorUp move the cursor for a given list panel. After
// the move we call scrollOffAdjust so the viewport follows the
// cursor with a 2-row look-ahead in the direction of motion (lazygit
// scrollOffMargin parity). Without that call j-spam past the bottom
// of a non-empty viewport leaves the highlight off-screen and the
// list visually stuck on the top page — the bug this fixes.
func (gu *Gui) cursorDown(p panelID) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		var before, after, header int
		gu.st.withLock(func() {
			switch p {
			case panelTasks:
				before = gu.st.taskCursor
				gu.st.taskCursor = clampCursor(gu.st.taskCursor+1, len(gu.st.tasks))
				after = gu.st.taskCursor
			case panelJobs:
				before = gu.st.jobCursor
				gu.st.jobCursor = clampCursor(gu.st.jobCursor+1, len(gu.st.jobs))
				after = gu.st.jobCursor
			case panelWorkflows:
				before = gu.st.workflowCursor
				gu.st.workflowCursor = clampCursor(gu.st.workflowCursor+1, len(gu.st.workflows))
				after = gu.st.workflowCursor
			case panelAgents:
				before = gu.st.agentCursor
				gu.st.agentCursor = clampCursor(gu.st.agentCursor+1, len(gu.st.agents))
				after = gu.st.agentCursor
			}
			header = headerLinesForLocked(p, gu.st)
		})
		gu.scrollOffAdjust(p.window(), header+before, header+after)
		gu.afterCursorMove(p)
		return nil
	}
}

func (gu *Gui) cursorUp(p panelID) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		var before, after, header int
		gu.st.withLock(func() {
			switch p {
			case panelTasks:
				before = gu.st.taskCursor
				gu.st.taskCursor = clampCursor(gu.st.taskCursor-1, len(gu.st.tasks))
				after = gu.st.taskCursor
			case panelJobs:
				before = gu.st.jobCursor
				gu.st.jobCursor = clampCursor(gu.st.jobCursor-1, len(gu.st.jobs))
				after = gu.st.jobCursor
			case panelWorkflows:
				before = gu.st.workflowCursor
				gu.st.workflowCursor = clampCursor(gu.st.workflowCursor-1, len(gu.st.workflows))
				after = gu.st.workflowCursor
			case panelAgents:
				before = gu.st.agentCursor
				gu.st.agentCursor = clampCursor(gu.st.agentCursor-1, len(gu.st.agents))
				after = gu.st.agentCursor
			}
			header = headerLinesForLocked(p, gu.st)
		})
		gu.scrollOffAdjust(p.window(), header+before, header+after)
		gu.afterCursorMove(p)
		return nil
	}
}

// afterCursorMove fires the per-panel side-effect that should follow
// a cursor change. Pulled out of cursorDown/cursorUp so the policy
// lives in one place — and so we can give Tasks a different
// behaviour from Workflows (which auto-scopes Tasks) without
// duplicating the switch in both directions.
//
// Per-panel contract:
//
//   - panelTasks: schedule a refresh so the detail pane picks up
//     the highlighted task's comments and signals.
//   - panelWorkflows: applyScope so the Tasks panel auto-filters
//     to the highlighted workflow.
//   - panelJobs: delegates to settleOnPanelJobs — the same per-job
//     side-effect block that focusPanel('2'), cyclePanel Tab, and
//     tasksEnter invoke when focus LANDS on Jobs without a cursor
//     move. The chain is:
//     afterCursorMove(panelJobs)
//     → settleOnPanelJobs
//     → scheduleJobLive (debounced timer)
//     → scheduleJobArchive (eager OnWorker dispatch, coalesced).
//     The Detail pane reads state.jobTranscript[jobID] every
//     frame; the SSE pump appends into it.
//   - panelAgents: scheduleRefresh only.
func (gu *Gui) afterCursorMove(p panelID) {
	switch p {
	case panelWorkflows:
		gu.applyScope()
	case panelJobs:
		gu.settleOnPanelJobs()
	default:
		gu.scheduleRefresh()
	}
}

// settleOnPanelJobs runs the per-job side effects that must follow
// the operator arriving on the Jobs panel — whether by a j/k cursor
// move OR by a focus change that LANDS on panelJobs (focusPanel '2',
// cyclePanel Tab/Shift-Tab landing on Jobs, tasksEnter on a task).
//
// Without this on the focus-change paths the Detail pane would stay
// stuck on "(loading...)" until the operator pressed j/k:
// scheduleJobArchive is the only thing that seeds state.jobTranscript
// for a fresh selection, and afterCursorMove was the only caller, so
// arriving at panelJobs without moving the cursor left the transcript
// cache unhydrated. Reusing the same body keeps cursor moves and
// focus moves observationally equivalent — both arm the live SSE +
// archive fetch and both drop a stale input draft authored against a
// different job.
func (gu *Gui) settleOnPanelJobs() {
	gu.scheduleRefresh()
	if j, ok := gu.st.selectedJobLocked(); ok {
		running := isJobRunning(j)
		// Discard any input draft that belongs to a different job
		// BEFORE we kick the live/archive scheduling — the next
		// renderViews pass will repopulate the view from
		// state.jobInput, which is now empty for this job.
		gu.clearJobInputIfStale(j.JobID)
		gu.scheduleJobLive(j.JobID, running)
		gu.scheduleJobArchive(j.JobID, running)
	} else {
		gu.clearJobInputIfStale("")
		gu.stopJobLive()
	}
}

// tasksScopeFromCursor is the Space-key handler on the Tasks panel:
// commits the current cursor row as scope.TaskID (filtering the
// Jobs panel) without changing focus. The complement to tasksEnter
// (which commits + jumps) and the only path j/k navigation leaves
// open after the cursor-move auto-scope was removed.
func (gu *Gui) tasksScopeFromCursor(*gocui.Gui, *gocui.View) error {
	var id string
	gu.st.withRLock(func() {
		if t, ok := gu.st.selectedTask(); ok {
			id = t.ID
		}
	})
	if id == "" {
		return nil
	}
	gu.applyScope()
	gu.flashf("info", "filter: %s (press * to reset)", id)
	return nil
}

// applyScope re-derives the scope chips from the current cursor in
// Tasks / Workflows. Workflows call this on every cursor move via
// afterCursorMove; Tasks call it only from explicit commit paths
// (Enter / Space) per the cursor-move policy documented above.
func (gu *Gui) applyScope() {
	gu.st.withLock(func() {
		switch gu.st.focused {
		case panelTasks:
			if t, ok := gu.st.selectedTask(); ok {
				gu.st.scope.TaskID = t.ID
			} else {
				gu.st.scope.TaskID = ""
			}
		case panelWorkflows:
			if w, ok := gu.st.selectedWorkflow(); ok {
				gu.st.scope.WorkflowID = w.ID
				gu.st.scope.WorkflowName = w.Name
			} else {
				gu.st.scope.WorkflowID = ""
				gu.st.scope.WorkflowName = ""
			}
		}
	})
	gu.scheduleRefresh()
}

// handleClearScope clears every scope chip.
func (gu *Gui) handleClearScope(*gocui.Gui, *gocui.View) error {
	gu.st.withLock(func() { gu.st.scope = scope{} })
	gu.flashf("info", "scope cleared")
	gu.scheduleRefresh()
	return nil
}

// handleRefresh re-fetches every panel on demand.
func (gu *Gui) handleRefresh(*gocui.Gui, *gocui.View) error {
	gu.scheduleRefresh()
	gu.flashf("info", "refresh")
	return nil
}

// handleForceReconnect is the Ctrl-R handler: drop any pooled
// *sqlite3.SQLiteConn inside the doltlite store, clear the per-job
// transcript cache (so the next selection re-hydrates from a fresh
// archive read), tear down any active SSE subscription, and
// schedule a refresh. The reconnect happens on a worker so the UI
// thread stays responsive (the Ping inside Reconnect blocks until
// the fresh conn handshakes). On error we still fall through to
// scheduleRefresh — a stale read is better than no read.
func (gu *Gui) handleForceReconnect(*gocui.Gui, *gocui.View) error {
	gu.flashf("info", "reconnect")
	gu.stopJobLive()
	gu.st.withLock(func() {
		gu.st.jobTranscript = map[string]*jobTranscriptEntry{}
	})
	gu.g.OnWorker(func(_ gocui.Task) error {
		ctx, cancel := context.WithTimeout(gu.ctx, 5*time.Second)
		defer cancel()
		if err := gu.ds.Reconnect(ctx); err != nil {
			dlog("reconnect: %v", err)
			gu.g.Update(func(_ *gocui.Gui) error {
				gu.flashf("warn", "reconnect failed: %v", err)
				return nil
			})
		}
		gu.scheduleRefresh()
		return nil
	})
	return nil
}

// handleEsc closes the active popup; falls back to clearing filter
// chips. Esc on winJobInput is bound directly to jobInputEscape so
// it doesn't route through here.
func (gu *Gui) handleEsc(g *gocui.Gui, v *gocui.View) error {
	// Snapshot popup.Kind under the lock so concurrent mutations
	// from g.Update closures don't race against the read.
	var popupKind popupKind
	gu.st.withRLock(func() {
		popupKind = gu.st.popup.Kind
	})
	if popupKind != popupNone {
		return gu.popupClose(g, v)
	}
	// Otherwise: clear the focused panel's filter.
	gu.st.withLock(func() {
		switch gu.st.focused {
		case panelTasks:
			gu.st.filter.Tasks = ""
		case panelJobs:
			gu.st.filter.Jobs = ""
		case panelWorkflows:
			gu.st.filter.Workflows = ""
		case panelAgents:
			gu.st.filter.Agents = ""
		}
	})
	gu.scheduleRefresh()
	return nil
}

// toggleLog flips command-log visibility.
func (gu *Gui) toggleLog(*gocui.Gui, *gocui.View) error {
	gu.st.withLock(func() { gu.st.logHide = !gu.st.logHide })
	return nil
}

// tasksEnter cross-links to Jobs (filter by this task) and focuses Jobs.
func (gu *Gui) tasksEnter(*gocui.Gui, *gocui.View) error {
	gu.applyScope()
	gu.st.withLock(func() { gu.st.focused = panelJobs })
	// Hydrate the Detail pane for the now-selected job. Without this
	// the operator lands on the Jobs panel with the transcript stuck
	// on "(loading...)" until they press j/k — see settleOnPanelJobs.
	gu.settleOnPanelJobs()
	return nil
}

// workflowsEnter cross-links to Tasks (filter by this workflow).
func (gu *Gui) workflowsEnter(*gocui.Gui, *gocui.View) error {
	gu.applyScope()
	gu.st.withLock(func() { gu.st.focused = panelTasks })
	return nil
}

// agentsEnter prompts the user to opt into the agent scope.
//
// Design plan §3.4: the relation is ambiguous (author vs.
// current_step.agent are different concepts), so we force the
// operator to pick ONE. The choice flows into scope.AgentRel and is
// surfaced both on the Tasks-panel scope chip and in the status bar.
func (gu *Gui) agentsEnter(*gocui.Gui, *gocui.View) error {
	a, ok := gu.st.selectedAgentLocked()
	if !ok {
		return nil
	}
	gu.openMenu("Filter Tasks by agent "+a.Name+"?", []string{
		"by author (author_id == " + a.Name + ")",
		"by current step (current_step.agent == " + a.Name + ")",
		"cancel",
	}, func(i int) error {
		var rel agentRel
		switch i {
		case 0:
			rel = agentRelAuthor
		case 1:
			rel = agentRelStep
		default:
			return nil
		}
		gu.st.withLock(func() {
			gu.st.scope.Agent = a.Name
			gu.st.scope.AgentRel = rel
			gu.st.focused = panelTasks
		})
		gu.scheduleRefresh()
		return nil
	})
	return nil
}

// jobsEnter focuses the appropriate view for the highlighted job:
//   - non-terminal job (queued or running, daemon accepts SendInput)
//     → winJobInput, caret blinks inside the textarea.
//   - terminal job (no SendInput dispatch surface) → logical focus
//     to panelDetail so j/k scroll the transcript above.
//
// The visibility predicate (isJobLive) intentionally mirrors the
// layout-pass gate on winJobInput: Enter only routes to the input
// view when that view actually exists this frame.
//
// On the live-job branch we set state.focused = panelJobInput
// (a synthetic focus identity whose window() is winJobInput). The
// layout pass calls g.SetCurrentView(focused.window()) on every
// frame; without recording the focus in the model the next layout
// would yank current-view back to winJobs within ~100ms (spinner
// tick cadence) and the caret would jump out of the textarea.
//
// On the terminal-job branch we stash detailFocus = panelJobs so
// renderDetail keeps emitting the Job Detail body even though
// focused now reads panelDetail.
func (gu *Gui) jobsEnter(*gocui.Gui, *gocui.View) error {
	j, ok := gu.st.selectedJobLocked()
	if !ok {
		return nil
	}
	if isJobLive(j) {
		gu.st.withLock(func() { gu.st.focused = panelJobInput })
		// Also SetCurrentView synchronously so tests that don't
		// drive a layout pass between jobsEnter and the assertion
		// observe the focus change immediately. The layout pass
		// would otherwise be the one to actually move the caret.
		if gu.g != nil {
			_, _ = gu.g.SetCurrentView(winJobInput)
		}
		return nil
	}
	gu.st.withLock(func() {
		gu.st.detailFocus = panelJobs
		gu.st.focused = panelDetail
	})
	return nil
}

// openFilter opens the per-panel filter prompt.
func (gu *Gui) openFilter(*gocui.Gui, *gocui.View) error {
	gu.openPrompt("filter: facets like p:1 status:done wf:name agent:name + free text", "", func(value string) error {
		gu.st.withLock(func() {
			switch gu.st.focused {
			case panelTasks:
				gu.st.filter.Tasks = value
			case panelJobs:
				gu.st.filter.Jobs = value
			case panelWorkflows:
				gu.st.filter.Workflows = value
			case panelAgents:
				gu.st.filter.Agents = value
			}
		})
		gu.scheduleRefresh()
		return nil
	})
	return nil
}

// openPalette opens the command palette.
func (gu *Gui) openPalette(*gocui.Gui, *gocui.View) error {
	cmds := []string{
		"task new",
		"task edit",
		"task done",
		"task cancel",
		"task reopen",
		"task priority",
		"task resume",
		"task enroll",
		"task block",
		"task unblock",
		"task comment",
		"task metadata",
		"workflow create",
		"workflow delete",
		"job cancel",
		"scope clear",
		"refresh",
		"quit",
	}
	gu.openMenu("command palette", cmds, func(i int) error {
		gu.dispatchPaletteCommand(cmds[i])
		return nil
	})
	return nil
}

func (gu *Gui) dispatchPaletteCommand(cmd string) {
	switch cmd {
	case "task new":
		_ = gu.taskNew(nil, nil)
	case "task edit":
		_ = gu.taskEdit(nil, nil)
	case "task done":
		_ = gu.taskDone(nil, nil)
	case "task cancel":
		_ = gu.taskCancel(nil, nil)
	case "task reopen":
		_ = gu.taskReopen(nil, nil)
	case "task priority":
		_ = gu.taskPriority(nil, nil)
	case "task resume":
		_ = gu.taskResume(nil, nil)
	case "task enroll":
		_ = gu.taskEnroll(nil, nil)
	case "task block":
		_ = gu.taskBlock(nil, nil)
	case "task unblock":
		_ = gu.taskUnblock(nil, nil)
	case "task comment":
		_ = gu.taskComment(nil, nil)
	case "task metadata":
		_ = gu.taskMetadataEdit(nil, nil)
	case "workflow create":
		_ = gu.workflowNew(nil, nil)
	case "workflow delete":
		_ = gu.workflowDelete(nil, nil)
	case "job cancel":
		_ = gu.jobCancel(nil, nil)
	case "scope clear":
		_ = gu.handleClearScope(nil, nil)
	case "refresh":
		_ = gu.handleRefresh(nil, nil)
	case "quit":
		gu.cancel()
		gu.g.Update(func(_ *gocui.Gui) error { return gocui.ErrQuit })
	}
}

// openHelp opens the lazygit-style sectioned + filterable
// cheatsheet popup. The bucket build is driven entirely by the
// bindingSpecs() registry — nothing in the help text is
// hand-curated anymore.
//
// Bucketing rules (lazygit's uniqueBindings parity):
//
//   - Local      = entries whose View == focused panel's window AND
//     Tag != "navigation".
//   - Global     = entries with Tag == "global" (or View == "").
//   - Navigation = entries whose View == focused panel's window AND
//     Tag == "navigation".
//
// Dedup is by Description within bucket (keep first occurrence) so
// equivalent aliases (j vs ArrowDown, q vs Ctrl-C) only surface
// once. Bindings whose Description is empty are excluded entirely
// — that's how we hide popup-scoped registrations and other
// non-discoverable plumbing.
func (gu *Gui) openHelp(*gocui.Gui, *gocui.View) error {
	var focused panelID
	gu.st.withRLock(func() {
		focused = gu.st.focused
	})
	gu.openCheatsheet(focused)
	return nil
}

// openCheatsheet builds the sectioned bucket of items for the given
// focused panel and seeds the popup state. Pulled out of openHelp
// so unit tests can drive the build directly without going through
// the openHelp keybinding handler.
func (gu *Gui) openCheatsheet(focused panelID) {
	specs := gu.bindingSpecs()
	items := buildCheatsheetItems(specs, focused)
	// Routes through replacePopup so any active popupChangelog
	// auto-popup gets its OnDismissChangelog fired before this
	// popup replaces it (review R10) — same contract as openMenu.
	gu.replacePopup(popupState{
		Kind:              popupCheatsheet,
		Title:             "Keybindings — type to filter · enter executes · esc close",
		CheatsheetItems:   items,
		CheatsheetFilter:  "",
		CheatsheetCursor:  0,
		CheatsheetFocused: focused,
	})
	gu.requestRedraw()
}

// buildCheatsheetItems assembles the section markers + binding rows
// for the cheatsheet given the binding spec registry and the
// currently focused panel. Pure function so the bucketing /
// deduplication contracts (AC2) can be exercised without a gui.
//
// The returned slice is the FULL list (header markers + non-header
// rows). The popup applies the filter at render time — the items
// slice is captured once at open and never mutated for the
// lifetime of the popup.
func buildCheatsheetItems(specs []bindingSpec, focused panelID) []cheatsheetItem {
	focusedWin := focused.window()

	dedup := map[string]map[string]bool{}
	local := []cheatsheetItem{}
	global := []cheatsheetItem{}
	navigation := []cheatsheetItem{}

	push := func(section string, dst *[]cheatsheetItem, sp bindingSpec) {
		if sp.Description == "" {
			return
		}
		if dedup[section] == nil {
			dedup[section] = map[string]bool{}
		}
		if dedup[section][sp.Description] {
			return
		}
		dedup[section][sp.Description] = true
		handler := sp.Handler
		item := cheatsheetItem{
			Section:     section,
			KeyLabel:    keyLabel(sp.Key),
			Description: sp.Description,
			Handler: func() error {
				if handler == nil {
					return nil
				}
				return handler(nil, nil)
			},
		}
		*dst = append(*dst, item)
	}

	for _, sp := range specs {
		isGlobal := sp.Tag == "global" || sp.View == ""
		isFocusedView := focusedWin != "" && sp.View == focusedWin
		switch {
		case isFocusedView && sp.Tag == "navigation":
			push("Navigation", &navigation, sp)
		case isFocusedView:
			push("Local", &local, sp)
		case isGlobal:
			push("Global", &global, sp)
		}
	}

	var items []cheatsheetItem
	appendBucket := func(section string, rows []cheatsheetItem) {
		if len(rows) == 0 {
			return
		}
		items = append(items, cheatsheetItem{IsHeader: true, Section: section})
		items = append(items, rows...)
	}
	appendBucket("Local", local)
	appendBucket("Global", global)
	appendBucket("Navigation", navigation)
	return items
}

// keyLabel renders a binding key (rune or gocui.Key) as the
// canonical operator-facing string. Project rule (kept stable
// across the cheatsheet, the options strip, and any future
// inline hint): plain letters are lowercase; uppercase letters
// mean shift+letter; modifier chords use the `ctrl+x` /
// `alt+enter` shape.
func keyLabel(k any) string {
	switch v := k.(type) {
	case rune:
		switch v {
		case ' ':
			return "space"
		}
		return string(v)
	case gocui.Key:
		switch v {
		case gocui.KeyEnter:
			return "enter"
		case gocui.KeyEsc:
			return "esc"
		case gocui.KeyTab:
			return "tab"
		case gocui.KeyArrowUp:
			return "↑"
		case gocui.KeyArrowDown:
			return "↓"
		case gocui.KeyArrowLeft:
			return "←"
		case gocui.KeyArrowRight:
			return "→"
		case gocui.KeyPgup:
			return "pgup"
		case gocui.KeyPgdn:
			return "pgdn"
		case gocui.KeyHome:
			return "home"
		case gocui.KeyEnd:
			return "end"
		case gocui.KeySpace:
			return "space"
		case gocui.KeyBackspace, gocui.KeyBackspace2:
			return "backspace"
		case gocui.KeyDelete:
			return "delete"
		case gocui.KeyCtrlA:
			return "ctrl+a"
		case gocui.KeyCtrlB:
			return "ctrl+b"
		case gocui.KeyCtrlC:
			return "ctrl+c"
		case gocui.KeyCtrlD:
			return "ctrl+d"
		case gocui.KeyCtrlE:
			return "ctrl+e"
		case gocui.KeyCtrlF:
			return "ctrl+f"
		case gocui.KeyCtrlR:
			return "ctrl+r"
		case gocui.KeyCtrlS:
			return "ctrl+s"
		case gocui.KeyCtrlU:
			return "ctrl+u"
		case gocui.KeyCtrlW:
			return "ctrl+w"
		case gocui.MouseWheelUp:
			return "wheel↑"
		case gocui.MouseWheelDown:
			return "wheel↓"
		}
	}
	return fmt.Sprintf("%v", k)
}

// taskNew opens the lazygit-style two-pane compose editor and
// creates a task on submit. The summary pane carries the task title
// (required); the description pane carries an optional multi-line
// body that goes straight into the tasks.description column. Empty
// summary is treated as a silent cancel (matches the previous
// single-line prompt's behaviour).
func (gu *Gui) taskNew(*gocui.Gui, *gocui.View) error {
	gu.openTaskCompose("New task", "", "", func(summary, description string) error {
		if strings.TrimSpace(summary) == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			id, err := gu.ds.CreateTask(gu.ctx, summary, description, store.DefaultPriority)
			if err != nil {
				gu.flashf("err", "task new: %v", err)
				return nil
			}
			gu.flashf("info", "created %s", id)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

// taskEdit opens the two-pane compose editor pre-filled with the
// selected task's title + description. On submit the values flow to
// Datasource.UpdateTitleDescription. An empty title (post-trim) is
// rejected with a flash and the popup is re-opened with the typed
// text intact so the user can fix it without losing their work.
func (gu *Gui) taskEdit(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openTaskComposeForEdit(t.ID, t.Title, t.Description)
	return nil
}

// taskMetadataEdit opens the single-pane multi-line compose editor
// pre-filled with the selected task's metadata as pretty-printed
// JSON (`{}` when empty/nil). On submit the text is parsed back
// into a map[string]any and pushed via Datasource.SetMetadata. On
// parse failure (invalid JSON, or a JSON value that isn't an
// object) the popup is re-opened with the typed text intact so the
// user can fix it.
func (gu *Gui) taskMetadataEdit(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	initial := "{}"
	if len(t.Metadata) > 0 {
		b, err := json.MarshalIndent(t.Metadata, "", "  ")
		if err != nil {
			// Defensive: a map of any may carry a non-marshalable
			// value (e.g. a func smuggled in by a buggy caller).
			// Surface the error and bail without opening the popup.
			gu.flashf("err", "metadata: %v", err)
			return nil
		}
		initial = string(b)
	}
	gu.openSingleComposeForMetadata(t.ID, initial)
	return nil
}

// openSingleComposeForMetadata is the openSingleCompose wrapper for
// the metadata-edit flow. Same shape as openTaskComposeForEdit: the
// validation-error branch re-invokes itself with the same typed
// text so the popup "stays open" without the user re-typing.
//
// JSON validation has two gates: json.Unmarshal must succeed AND
// the decoded value must actually be a JSON object. Both `null` and
// non-object JSON (arrays, strings, numbers, bool) decode into a
// map[string]any without an error — `null` populates a nil map and
// non-objects fail mid-decode — but the user-visible contract per
// plan AC #9 is that only a JSON OBJECT is acceptable. Treat the
// nil-map case as a validation failure so typing the literal `null`
// doesn't silently wipe metadata. (Note: json.Unmarshal of an
// array / string / number / bool into a map[string]any returns a
// proper error, so they go through the existing err branch.)
func (gu *Gui) openSingleComposeForMetadata(id, initial string) {
	gu.openSingleCompose("Metadata "+id, "JSON object", initial, func(text string) error {
		var m map[string]any
		if err := json.Unmarshal([]byte(text), &m); err != nil {
			gu.flashf("err", "invalid JSON: %v", err)
			gu.openSingleComposeForMetadata(id, text)
			return nil
		}
		if m == nil {
			// `null` unmarshals cleanly into a nil map. Reject so it
			// doesn't act as a silent metadata-wipe (plan AC #9).
			gu.flashf("err", "invalid JSON: metadata must be a JSON object")
			gu.openSingleComposeForMetadata(id, text)
			return nil
		}
		gu.runDispatch(func() {
			if err := gu.ds.SetMetadata(gu.ctx, id, m); err != nil {
				gu.flashf("err", "metadata: %v", err)
				return
			}
			gu.flashf("info", "metadata updated %s", id)
			gu.refreshAll()
		})
		return nil
	})
}

// openTaskComposeForEdit is the openTaskCompose wrapper for the
// edit flow. Extracted so the validation-error branch (empty title)
// can re-invoke itself with the same (id, summary, description)
// triple — the compose confirm handler resets popup state to
// popupNone before invoking the accept callback, so re-opening from
// inside the callback is the supported way to "keep the popup
// open".
//
// The write itself goes through runDispatch (not gu.g.OnWorker
// directly) so tests can swap in a synchronous dispatcher and
// observe the datasource call without needing a gocui MainLoop.
func (gu *Gui) openTaskComposeForEdit(id, summary, description string) {
	gu.openTaskCompose("Edit "+id, summary, description, func(s, d string) error {
		if strings.TrimSpace(s) == "" {
			gu.flashf("err", "title required")
			gu.openTaskComposeForEdit(id, s, d)
			return nil
		}
		gu.runDispatch(func() {
			if err := gu.ds.UpdateTitleDescription(gu.ctx, id, s, d); err != nil {
				gu.flashf("err", "edit: %v", err)
				return
			}
			gu.flashf("info", "edited %s", id)
			gu.refreshAll()
		})
		return nil
	})
}

func (gu *Gui) taskDone(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.confirmThen(fmt.Sprintf("mark %s done?", t.ID), func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.UpdateStatus(gu.ctx, t.ID, store.StatusDone); err != nil {
				gu.flashf("err", "done: %v", err)
				return nil
			}
			gu.flashf("info", "done %s", t.ID)
			gu.refreshAll()
			return nil
		})
	})
	return nil
}

func (gu *Gui) taskCancel(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.confirmThen(fmt.Sprintf("cancel %s?", t.ID), func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.UpdateStatus(gu.ctx, t.ID, store.StatusCancel); err != nil {
				gu.flashf("err", "cancel: %v", err)
				return nil
			}
			gu.flashf("info", "cancelled %s", t.ID)
			gu.refreshAll()
			return nil
		})
	})
	return nil
}

func (gu *Gui) taskReopen(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.g.OnWorker(func(_ gocui.Task) error {
		if err := gu.ds.UpdateStatus(gu.ctx, t.ID, store.StatusNew); err != nil {
			gu.flashf("err", "reopen: %v", err)
			return nil
		}
		gu.flashf("info", "reopened %s", t.ID)
		gu.refreshAll()
		return nil
	})
	return nil
}

// taskClaim was removed: the v0.2 schema has no claim verb. See the
// design plan + the help screen — 'e' enrolls into a workflow (or,
// with `--agent NAME`, into a synthetic single:<agent> flow); there is
// no separate assign verb anymore.

// taskEnroll opens the two-pane workflow + step picker. Synthetic
// single:<agent> workflows are filtered out (operators still drive
// those via `autosk enroll --agent NAME` on the CLI). On open the
// workflow cursor pre-selects the task's current workflow when it
// exists in the cached slice; otherwise row 0. Enter on the step
// pane dispatches Datasource.Enroll(ctx, taskID, wfName, stepName).
func (gu *Gui) taskEnroll(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	var wfs []datasource.Workflow
	gu.st.withRLock(func() {
		wfs = filterPickerWorkflows(gu.st.workflows)
	})
	if len(wfs) == 0 {
		gu.flashf("info", "no workflows defined")
		return nil
	}
	wfCursor := pickerInitialWorkflowCursor(wfs, t.WorkflowID)
	stepCursor := pickerInitialStepCursor(wfs, wfCursor, t.CurrentStepID)
	taskID := t.ID
	gu.openEnrollPicker(
		"Enroll "+taskID+" — pick workflow",
		wfs, wfCursor, stepCursor, false,
		func(wfName, stepName string) error {
			gu.runDispatch(func() {
				if err := gu.ds.Enroll(gu.ctx, taskID, wfName, stepName); err != nil {
					gu.flashf("err", "enroll: %v", err)
					return
				}
				gu.flashf("info", "enroll %s -> %s:%s ok", taskID, wfName, stepName)
				gu.refreshAll()
			})
			return nil
		})
	return nil
}

// taskResume opens the same picker with the workflow pane locked to
// the task's current workflow (resolved from the cached workflows
// slice). Focus starts on the step pane. Enter dispatches
// Datasource.Resume(ctx, taskID, stepName). If the task has no
// current workflow in the cached slice (e.g. stale cache; the task
// has WorkflowID == "") the picker is NOT opened and a flash hints
// at a refresh.
//
// CLI-parity for the no-bump path (review R7): the CLI has two
// distinct resume modes:
//
//   - `autosk resume <id>`           — status-only flip
//     (StatusHuman → StatusWork), step_visits untouched,
//     max_visits NOT enforced. Routes through Resume(id, "").
//   - `autosk resume <id> --to STEP` — deliberate transition,
//     step_visits[STEP]++, max_visits enforced. Routes through
//     Resume(id, STEP).
//
// The picker always pre-selects the task's current step, so the
// natural "just resume" gesture (Enter on the pre-selected row)
// would otherwise dispatch the bumping --to path against the same
// step the operator was parked on — firing max_visits on tasks
// that hit human via the cap in the first place
// (docs/plans/20260520-Step-Visit-Limits.md). To restore CLI
// parity we collapse "picked step == current step" to the no-bump
// branch; picking a DIFFERENT step still bumps as before.
func (gu *Gui) taskResume(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	if t.WorkflowID == "" {
		gu.flashf("warn", "task has no workflow; enroll first")
		return nil
	}
	var wf *datasource.Workflow
	gu.st.withRLock(func() {
		for i := range gu.st.workflows {
			if gu.st.workflows[i].ID == t.WorkflowID {
				w := gu.st.workflows[i]
				wf = &w
				break
			}
		}
	})
	if wf == nil {
		gu.flashf("warn", "task workflow not loaded; refresh and retry")
		return nil
	}
	wfs := []datasource.Workflow{*wf}
	stepCursor := pickerInitialStepCursor(wfs, 0, t.CurrentStepID)
	taskID := t.ID
	currentStepName := resumeCurrentStepName(*wf, t.CurrentStepID)
	gu.openEnrollPicker(
		"Resume "+taskID+" — pick step",
		wfs, 0, stepCursor, true,
		func(_, stepName string) error {
			// CLI parity: Enter on the pre-selected current step
			// is the no-bump path (Resume(id, "")). Picking a
			// different step routes through the bumping --to STEP
			// path. resumeCurrentStepName returns "" when the task
			// has no current step (or it isn't in the workflow's
			// step list), which can't collide with any real step
			// name — so the equality test stays inert in that case
			// and Resume(id, stepName) takes the --to path.
			toStep := stepName
			noBump := currentStepName != "" && stepName == currentStepName
			if noBump {
				toStep = ""
			}
			gu.runDispatch(func() {
				if err := gu.ds.Resume(gu.ctx, taskID, toStep); err != nil {
					gu.flashf("err", "resume: %v", err)
					return
				}
				if noBump {
					gu.flashf("info", "resumed %s (no transition)", taskID)
				} else {
					gu.flashf("info", "resumed %s -> %s", taskID, stepName)
				}
				gu.refreshAll()
			})
			return nil
		})
	return nil
}

// resumeCurrentStepName returns the human-readable name of the
// task's current step inside wf, or "" when the task has no
// current step OR the step id isn't present in the workflow's
// step list (stale cache / hand-edit). Used by taskResume to
// detect the no-bump branch (review R7).
func resumeCurrentStepName(wf datasource.Workflow, currentStepID string) string {
	if currentStepID == "" {
		return ""
	}
	for _, s := range wf.Steps {
		if s.ID == currentStepID {
			return s.Name
		}
	}
	return ""
}

// pickerInitialWorkflowCursor returns the index of the workflow row
// matching workflowID, or 0 when no match exists (empty workflowID
// or stale cache). Pure helper so the open-flow tests can pin the
// pre-selection logic without driving the full handler chain.
func pickerInitialWorkflowCursor(wfs []datasource.Workflow, workflowID string) int {
	if workflowID == "" {
		return 0
	}
	for i := range wfs {
		if wfs[i].ID == workflowID {
			return i
		}
	}
	return 0
}

// pickerInitialStepCursor returns the index of the step row matching
// stepID inside workflows[wfCursor].Steps, or 0 when no match
// exists. Symmetric to pickerInitialWorkflowCursor and pulled out
// for the same testing-isolation reason.
func pickerInitialStepCursor(wfs []datasource.Workflow, wfCursor int, stepID string) int {
	if stepID == "" || wfCursor < 0 || wfCursor >= len(wfs) {
		return 0
	}
	for i, s := range wfs[wfCursor].Steps {
		if s.ID == stepID {
			return i
		}
	}
	return 0
}

func (gu *Gui) taskBlock(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openPrompt("block "+t.ID+" by id:", "", func(blocker string) error {
		blocker = strings.TrimSpace(blocker)
		if blocker == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.Block(gu.ctx, t.ID, blocker); err != nil {
				gu.flashf("err", "block: %v", err)
				return nil
			}
			gu.flashf("info", "blocked %s <- %s", t.ID, blocker)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskUnblock(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openPrompt("unblock "+t.ID+" from id:", "", func(blocker string) error {
		blocker = strings.TrimSpace(blocker)
		if blocker == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.Unblock(gu.ctx, t.ID, blocker); err != nil {
				gu.flashf("err", "unblock: %v", err)
				return nil
			}
			gu.flashf("info", "unblocked %s <- %s", t.ID, blocker)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskComment(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	// Single-pane multi-line compose: Enter inserts \n, Ctrl+S
	// submits, Esc cancels. An empty submit (whitespace only) is a
	// silent cancel — same semantics as the previous one-line
	// prompt the comment popup used.
	gu.openSingleCompose("Comment on "+t.ID, "markdown ok", "", func(text string) error {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.AddComment(gu.ctx, t.ID, text); err != nil {
				gu.flashf("err", "comment: %v", err)
				return nil
			}
			gu.flashf("info", "comment on %s", t.ID)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskPriority(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openMenu("priority for "+t.ID, []string{"0", "1", "2", "3"}, func(i int) error {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.UpdatePriority(gu.ctx, t.ID, i); err != nil {
				gu.flashf("err", "priority: %v", err)
				return nil
			}
			gu.flashf("info", "P%d on %s", i, t.ID)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) workflowNew(*gocui.Gui, *gocui.View) error {
	gu.openPrompt("workflow file path:", "", func(path string) error {
		path = strings.TrimSpace(path)
		if path == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			name, err := gu.ds.CreateWorkflow(gu.ctx, path)
			if err != nil {
				gu.flashf("err", "workflow new: %v", err)
				return nil
			}
			gu.flashf("info", "workflow %s created", name)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) workflowDelete(*gocui.Gui, *gocui.View) error {
	w, ok := gu.st.selectedWorkflowLocked()
	if !ok {
		return nil
	}
	gu.confirmThen("delete workflow "+w.Name+"?", func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.DeleteWorkflow(gu.ctx, w.Name); err != nil {
				gu.flashf("err", "wf delete: %v", err)
				return nil
			}
			gu.flashf("info", "wf %s deleted", w.Name)
			gu.refreshAll()
			return nil
		})
	})
	return nil
}

// workflowIsolation is the `i` handler on the Workflows panel. It
// opens a two-option popupMenu listing the available isolation
// modes; confirming a different value chains into a popupConfirm
// whose body enumerates the affected non-terminal tasks (or notes
// that none are affected). Synthetic rows respond with a status-bar
// message instead — single:<agent> workflows are pinned to none by
// construction so the menu has no useful action.
//
// The menu is implemented atop popupMenu (so j/k/Enter wiring is
// reused); we wrap the OnSelect in a closure that captures the
// selected workflow and the operator's choice. The destructive
// branch (different value) chains into openConfirm.
func (gu *Gui) workflowIsolation(*gocui.Gui, *gocui.View) error {
	w, ok := gu.st.selectedWorkflowLocked()
	if !ok {
		return nil
	}
	if w.IsSynthetic {
		gu.flashf("info", "isolation is locked to 'none' on synthetic workflows")
		return nil
	}
	cur := w.Isolation
	if cur == "" {
		cur = "none"
	}
	modes := []string{"none", "worktree"}
	lines := make([]string, 0, len(modes))
	for _, m := range modes {
		label := "isolation: " + m
		if m == cur {
			label += "   ← current"
		}
		lines = append(lines, label)
	}
	name := w.Name
	nonTerminalCount := w.NonTerminalTaskCount
	// Copy the slice so the closure isn't tied to the lifetime of the
	// model under the lock (the refresh goroutine rewrites
	// w.NonTerminalTasks wholesale on the next tick).
	nonTerminalTasks := make([]datasource.NonTerminalTaskRef, len(w.NonTerminalTasks))
	copy(nonTerminalTasks, w.NonTerminalTasks)
	gu.openIsolationMenu("Flip isolation of "+name, lines, func(idx int) error {
		if idx < 0 || idx >= len(modes) {
			return nil
		}
		target := modes[idx]
		if target == cur {
			// No-op: close the popup silently.
			return nil
		}
		body := buildIsolationConfirmBody(cur, target, nonTerminalCount, nonTerminalTasks)
		force := nonTerminalCount > 0
		gu.openConfirm(body, func() error {
			gu.g.OnWorker(func(_ gocui.Task) error {
				rep, err := gu.ds.UpdateWorkflowIsolation(gu.ctx, name, target, force)
				if err != nil {
					gu.flashf("err", "workflow isolation: %v", err)
					return nil
				}
				// Single flash: state.flash is a one-slot field, so a
				// success followed by a leftover-warning would otherwise
				// overwrite the success ack between the Enter and the
				// next paint. Combine the two pieces of information into
				// one info-level toast; the command log keeps both
				// lines via appendLog anyway.
				switch {
				case rep.Noop:
					gu.flashf("info", "workflow %s: already %s", name, target)
				case len(rep.LeftoverWorktrees) > 0:
					gu.flashf("info",
						"workflow %s: isolation %s→%s; %d worktree(s) left on disk, use `autosk worktree rm`",
						name, rep.From, rep.To, len(rep.LeftoverWorktrees))
				default:
					gu.flashf("info", "workflow %s: isolation %s→%s", name, rep.From, rep.To)
				}
				gu.refreshAll()
				return nil
			})
			return nil
		})
		return nil
	})
	return nil
}

// workflowSync is the `s` handler on the Workflows panel. It runs
// `autosk workflow sync` through the datasource so the TUI and CLI
// share one code path. The report is surfaced via flash + command
// log so the operator sees conflicts and per-workflow outcomes
// without leaving the dashboard.
func (gu *Gui) workflowSync(*gocui.Gui, *gocui.View) error {
	gu.runDispatch(func() {
		rep, err := gu.ds.SyncWorkflows(gu.ctx, false, false)
		counts := map[string]int{}
		for _, w := range rep.Workflows {
			counts[w.Status]++
		}
		var summary string
		if len(rep.Workflows) == 0 {
			summary = "no enabled global workflows"
		} else {
			var parts []string
			for _, status := range []string{"added", "updated", "noop", "skipped", "conflict", "error"} {
				if n := counts[status]; n > 0 {
					parts = append(parts, fmt.Sprintf("%d %s", n, status))
				}
			}
			summary = strings.Join(parts, ", ")
		}
		if err != nil {
			if len(rep.Workflows) > 0 {
				gu.flashf("err", "workflow sync failed: %s", summary)
			} else {
				gu.flashf("err", "workflow sync failed: %v", err)
			}
		} else {
			gu.flashf("info", "workflow sync: %s", summary)
		}
		// Append per-workflow details to the command log regardless
		// of top-level success/failure so the operator can scroll
		// back and see every outcome.
		for _, w := range rep.Workflows {
			line := fmt.Sprintf("sync %s: %s", w.Name, w.Status)
			if w.Reason != "" {
				line += " (" + w.Reason + ")"
			}
			if w.Error != "" {
				line += ": " + w.Error
			}
			gu.st.withLock(func() {
				gu.st.appendLog(line)
			})
			if len(w.AutoInstalledAgents) > 0 {
				gu.st.withLock(func() {
					gu.st.appendLog("  auto_agents: " + strings.Join(w.AutoInstalledAgents, ", "))
				})
			}
		}
		gu.refreshAll()
	})
	return nil
}

// isolationConfirmTaskCap bounds the number of per-task lines
// rendered inside the confirm popup body. Above this we append a
// `... and N more` suffix. Sized to match
// datasource.NonTerminalTaskSampleSize so the popup never asks for
// more rows than the data source is willing to ship; plan §8 risk
// #4.
const isolationConfirmTaskCap = datasource.NonTerminalTaskSampleSize

// buildIsolationConfirmBody renders the popupConfirm body for the
// isolation flip. Pure function so the layout pins in
// popup_isolation_test.go can exercise the wording without driving a
// gocui screen.
//
// total is the FULL non-terminal-task count (not len(tasks); tasks
// may be a capped sample). The body enumerates every task in `tasks`
// up to isolationConfirmTaskCap and appends `... and N more` when
// the total exceeds the rendered prefix.
func buildIsolationConfirmBody(from, to string, total int, tasks []datasource.NonTerminalTaskRef) string {
	head := fmt.Sprintf("Flip isolation: %s → %s", from, to)
	if total == 0 {
		return head + "\nNo tasks are currently affected."
	}

	var header, tail string
	switch {
	case from == "none" && to == "worktree":
		header = fmt.Sprintf("\n%d non-terminal task(s) will get a fresh worktree allocated from HEAD:", total)
		tail = "\nThis is the --force path. Continue?"
	case from == "worktree" && to == "none":
		header = fmt.Sprintf("\n%d non-terminal task(s) keep their worktree directories on disk:", total)
		tail = "\nRemove them later with `autosk worktree rm <task-id>` once you have salvaged anything you need. Continue?"
	default:
		header = fmt.Sprintf("\n%d non-terminal task(s) affected:", total)
		tail = "\nContinue?"
	}

	var b strings.Builder
	b.WriteString(head)
	b.WriteString(header)
	rendered := len(tasks)
	if rendered > isolationConfirmTaskCap {
		rendered = isolationConfirmTaskCap
	}
	for i := 0; i < rendered; i++ {
		t := tasks[i]
		status := string(t.Status)
		if status == "" {
			status = "unknown"
		}
		step := t.StepName
		if step == "" {
			step = "(none)"
		}
		fmt.Fprintf(&b, "\n  - %s (status=%s, current step=%s)", t.ID, status, step)
	}
	if total > rendered {
		fmt.Fprintf(&b, "\n  ... and %d more", total-rendered)
	}
	b.WriteString(tail)
	return b.String()
}

// agentInstall and agentUninstall are intentionally informational in
// v1: the daemon has no /v1/agents endpoint so live mode returns the
// same error as offline. A popup with two no-op options is a fake
// choice; flashf is the right verb — one piece of info, no demand
// for an action that doesn't exist. The hotkeys stay bound so the
// help screen line 'i install / u uninstall' is honest (it points
// to the CLI workaround).
func (gu *Gui) agentInstall(*gocui.Gui, *gocui.View) error {
	gu.flashf("info", "agent install: quit lazy and run 'autosk agent install <pkg>'")
	return nil
}

func (gu *Gui) agentUninstall(*gocui.Gui, *gocui.View) error {
	a, ok := gu.st.selectedAgentLocked()
	name := "<pkg>"
	if ok && a.Name != "" {
		name = a.Name
	}
	gu.flashf("info", "agent uninstall: quit lazy and run 'autosk agent uninstall %s'", name)
	return nil
}

func (gu *Gui) jobCancel(*gocui.Gui, *gocui.View) error {
	j, ok := gu.st.selectedJobLocked()
	if !ok {
		return nil
	}
	gu.confirmThen("cancel job "+j.JobID+"?", func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.CancelJob(gu.ctx, j.JobID); err != nil {
				gu.flashf("err", "cancel job: %v", err)
				return nil
			}
			gu.flashf("info", "job %s cancelled", j.JobID)
			gu.refreshAll()
			return nil
		})
	})
	return nil
}

// confirmThen opens a confirm popup; on yes runs f.
func (gu *Gui) confirmThen(prompt string, f func()) {
	gu.openConfirm(prompt, func() error { f(); return nil })
}
