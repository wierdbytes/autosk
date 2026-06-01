// Package tui is the gocui-backed TUI for `autosk lazy`.
//
// The package is small on purpose. Lazygit's split (35+ contexts) is
// overkill for v1 — autosk lazy only needs a 4-panel dashboard, an
// inline Job Detail pane (header + per-event transcript boxes + an
// optional input textarea for running jobs), and a handful of
// popups. The architecture still mirrors the lazygit shape (gui +
// state + arrangement + helpers + render) but in fewer files.
//
// The only seam between the TUI and the rest of autosk is
// internal/lazy/datasource. Nothing in this package imports
// internal/store, internal/daemon/client, or internal/workflow
// directly.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jesseduffield/gocui"

	"autosk/internal/daemon/runstore"
	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/theme"
)

// Options is the input to Run.
type Options struct {
	Datasource  datasource.Datasource
	ProjectRoot string
	Refresh     time.Duration
	// Headless makes the gocui screen a tcell simulation screen so
	// the TUI can be driven by tests without a real terminal.
	Headless bool
	Width    int
	Height   int
	// ChangelogModal, when non-nil, causes the gui to push a
	// popupChangelog state on the first layout pass. The caller
	// (cmd/autosk/lazy.go) decides whether to fire the popup based
	// on buildinfo.Version + userstate.State.LastSeenChangelog;
	// the TUI is intentionally dumb about the policy.
	ChangelogModal *ChangelogModalOptions
}

// ChangelogModalOptions carries the inputs to the startup-pushed
// changelog popup. Title + Body land on popupState; OnDismiss
// fires once on Esc / Enter so the caller can stamp
// last_seen_changelog into ~/.autosk/state.json.
type ChangelogModalOptions struct {
	Title     string
	Body      string
	OnDismiss func() error
}

// Gui ties together the gocui main loop, the model, the datasource,
// and the helper goroutines.
type Gui struct {
	g           *gocui.Gui
	st          *state
	ds          datasource.Datasource
	opts        Options
	ctx         context.Context
	cancel      context.CancelFunc
	stopRefresh chan struct{}

	// refreshInFlight coalesces redundant refresh requests. j-spam
	// (cursor moves that call applyScope → refresh) used to enqueue
	// one OnWorker job per keypress; now intermediate requests are
	// folded into a single trailing refresh via refreshPending so the
	// latest scope eventually drives the UI (lazygit's RefreshHelper
	// uses the same pattern).
	refreshInFlight atomic.Bool
	refreshPending  atomic.Bool

	// dispatch routes a body onto the gocui worker pool. Defaults to
	// gu.g.OnWorker(...) in production; tests replace it with a plain
	// goroutine spawn so scheduleRefresh's CAS dance can be exercised
	// without a real gocui.Gui.
	dispatch func(func())

	// lastFetchNS is the wall-clock duration of the most recent
	// fetchRefresh, in nanoseconds. The tick loop reads it to decide
	// how long to wait before the next refresh — if the datasource is
	// slow (e.g. doltlite's chunk-store WAL has grown and every query
	// pays a hundreds-of-milliseconds replay cost) we back off so the
	// dashboard never pegs a core trying to keep up. Reset to 0 on
	// scheduleRefresh from a user action so the next tick reverts to
	// the base cadence promptly when the slowdown clears.
	lastFetchNS atomic.Int64

	// spinnerTick advances once per spinnerLoop iteration (~100ms)
	// whenever the Tasks panel has at least one row with an active
	// job. renderTasksPanel reads it mod len(spinnerFrames) to pick
	// the braille frame for the spinner column. Plain uint64 (not a
	// time.Time) so the renderer doesn't need to know the cadence —
	// it just walks the frame list.
	spinnerTick atomic.Uint64

	// bodyCache memoises the last (body, view-dimensions) writeView
	// committed to each named view. On a follow-up writeView call
	// with byte-identical body AND unchanged dimensions we skip the
	// v.Clear()+v.Write() cost — gocui would otherwise re-parse the
	// ANSI SGR escape stream cell-by-cell, which is the dominant cost
	// of a spinner-only repaint after the markdown LRU eliminates
	// the glamour re-render.
	//
	// Invalidated on view (re-)creation (layout drops the entry when
	// SetView returns ErrUnknownView), on DeleteView (the layout loop
	// drops it alongside the gocui DeleteView call), and implicitly
	// on resize (cached dims won't match the new dims). All access is
	// from the gocui main goroutine (layout → renderViews →
	// writeView), so no mutex is needed.
	bodyCache map[string]bodyCacheEntry

	// lastDetailKey is the "kind:id" identifier of whatever entity
	// was rendered into winDetail on the previous renderViews pass.
	// When it changes between frames, renderViews resets winDetail's
	// viewport origin so the new entity shows from its natural
	// anchor (top for task / workflow / agent, bottom for job via
	// writeViewSticky). When it's unchanged (same entity, just a
	// repaint), the origin is preserved so the operator's scroll
	// position survives spinner ticks / refresh updates / live
	// event arrivals. Read & written only from the gocui main
	// goroutine (same as bodyCache), so no mutex is needed.
	lastDetailKey string

	// lastDetailOverlay records whether winJobInput existed during
	// the previous winDetail writeViewSticky pass. Sticky-tail uses it
	// to distinguish "overlay just appeared while the operator was at
	// the old full-height bottom" from "overlay was already visible
	// and the operator manually scrolled up inside the smaller visible
	// region".
	lastDetailOverlay bool
}

// bodyCacheEntry is one entry in Gui.bodyCache: the body string,
// the view dimensions it was paired with when it was committed,
// and the buffer-line count gocui will render for that body.
//
// We store dimensions because gocui's SetView calls clearViewLines()
// internally on resize, so a stale-body short-circuit after a
// resize would leave the pane visibly blank.
//
// lineCount is the viewBufferLineCount of the buffer AS IT EXISTS
// in gocui after the last real Write — i.e. strings.Count(body,
// "\n")+1 when body is non-empty. writeViewSticky reads it instead
// of calling strings.Count on v.Buffer() twice per frame (the
// previous shape did that even on cache-hit frames where the body
// was byte-identical to last time; see ask-beab99 for the
// benchmark). 0 means "no body committed" / "empty buffer".
type bodyCacheEntry struct {
	body           string
	x0, y0, x1, y1 int
	lineCount      int
}

// adaptiveDelay returns the next ticker delay given a base cadence and
// the most recent fetchRefresh duration.
//
// The rule is intentionally simple: keep the base cadence when reads
// finish in well under half the budget, double it when reads exceed
// half the budget, and cap at maxAdaptiveDelay so a wedged datasource
// doesn't stall the dashboard for minutes. The thresholds are picked
// so a healthy DB stays at the base 2s while a doltlite store mid-WAL
// stretch (each refresh ~500ms-3s) settles at 4-30s instead of
// firing back-to-back ticks and saturating a core.
func adaptiveDelay(base, elapsed time.Duration) time.Duration {
	if base <= 0 {
		base = 2 * time.Second
	}
	if elapsed <= base/2 {
		return base
	}
	// Back off to max(base*2, elapsed*2) so we always give the
	// datasource at least as much idle time as it just spent working
	// — otherwise a 4s refresh on a 2s base would still queue
	// back-to-back.
	delay := base * 2
	if elapsed*2 > delay {
		delay = elapsed * 2
	}
	if delay > maxAdaptiveDelay {
		delay = maxAdaptiveDelay
	}
	return delay
}

// maxAdaptiveDelay caps the adaptive ticker. 30s is long enough that a
// truly-wedged datasource (e.g. doltlite re-replaying a multi-MB WAL
// on every query) doesn't burn cycles, short enough that the operator
// still sees the dashboard reanimate within human-impatience range
// once compaction kicks in.
const maxAdaptiveDelay = 30 * time.Second

// Run constructs the gui, opens the alt-screen, and blocks on the
// main loop. Returns nil on a clean quit (ErrQuit), the underlying
// error otherwise.
func Run(ctx context.Context, opts Options) error {
	if opts.Datasource == nil {
		return errors.New("lazy tui: nil datasource")
	}
	if opts.Refresh <= 0 {
		opts.Refresh = 2 * time.Second
	}
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		// OutputTrue (24-bit) so the theme.Palette's RGB values render
		// faithfully on both sides of the rendering pipeline:
		//
		//   - View.FrameColor / View.TitleColor go through
		//     gocui.NewRGBColor → tcell.NewRGBColor without masking.
		//   - Lipgloss-generated "\x1b[38;2;R;G;Bm" escapes inside view
		//     buffers are accepted by gocui's escape interpreter
		//     (escape.go:327 explicitly rejects 38;2;… when mode<OutputTrue).
		//
		// Terminals without truecolor support degrade silently — tcell
		// downsamples to the closest 256-color slot.
		OutputMode: gocui.OutputTrue,
		Headless:   opts.Headless,
		Width:      opts.Width,
		Height:     opts.Height,
	})
	if err != nil {
		return fmt.Errorf("gocui init: %w", err)
	}
	defer g.Close()

	g.InputEsc = true
	g.SupportOverlaps = true
	// Mouse=true unlocks wheel-scroll bindings (MouseWheelUp/Down).
	// Without this gocui swallows wheel events at the tcell layer and
	// the per-view scroll handlers never fire — which is exactly the
	// bug operators hit on the Detail pane and (pre-redesign) inspector transcript:
	// content overflowed the viewport, j/k worked, the wheel didn't.
	g.Mouse = true

	cctx, cancel := context.WithCancel(ctx)
	gu := &Gui{
		g:           g,
		st:          newState(),
		ds:          opts.Datasource,
		opts:        opts,
		ctx:         cctx,
		cancel:      cancel,
		stopRefresh: make(chan struct{}),
	}

	g.SetManagerFunc(gu.layout)
	if err := gu.bindKeys(); err != nil {
		cancel()
		return fmt.Errorf("bind keys: %w", err)
	}

	// Push the startup changelog popup BEFORE the first refresh so
	// the popup is laid out on the very first frame the operator
	// sees. The popup state is captured under the model lock; the
	// layout pass picks it up on the next g.MainLoop iteration.
	if opts.ChangelogModal != nil {
		gu.openChangelog(
			opts.ChangelogModal.Title,
			opts.ChangelogModal.Body,
			opts.ChangelogModal.OnDismiss,
		)
	}

	// Kick an initial refresh + adaptive ticker. Both go via
	// g.OnWorker / g.Update — no goroutine-per-panel.
	gu.scheduleRefresh()
	go gu.adaptiveTickLoop(opts.Refresh)
	go gu.spinnerLoop()

	// gocui's MainLoop is hostile to non-fatal mid-flush errors: a
	// SetView / SetCurrentView call against a still-being-rebuilt view
	// can surface ErrUnknownView, which we'd rather log-and-continue
	// than tear the whole TUI down for. Install an ErrorHandler that
	// rewrites those into ignored / drained-and-continue results.
	g.ErrorHandler = func(err error) error {
		if isUnknownView(err) {
			return nil
		}
		return err
	}
	mainErr := g.MainLoop()
	if mainErr != nil && mainErr != gocui.ErrQuit && !errors.Is(mainErr, gocui.ErrQuit) {
		cancel()
		close(gu.stopRefresh)
		gu.stopJobLive()
		return mainErr
	}
	cancel()
	close(gu.stopRefresh)
	gu.stopJobLive()
	return nil
}

// adaptiveTickLoop schedules refreshes on a self-adjusting cadence.
//
// The classic time.NewTicker(base) loop has a nasty failure mode
// against doltlite: when the chunk-store WAL grows long enough that
// each fetchRefresh takes longer than the 2s tick, the worker queue
// stays permanently saturated. Each refresh re-reads ~6 panels, each
// panel cursor-open triggers btreeRefreshFromDisk → csReplayWal, and
// the whole thing pegs a core indefinitely (see the post-mortem in
// docs/daemon.md "100%-CPU lazy").
//
// The adaptive loop sleeps for max(base, elapsed*2) up to a 30s cap,
// so a slow datasource self-throttles instead of melting the CPU.
// Once compaction (autosk gc / the daemon's per-project compactor)
// shrinks the chunk-store again, the next refresh comes back fast,
// lastFetchNS drops, and the loop snaps back to the base cadence on
// the very next iteration.
//
// Runs in a goroutine; nothing touches the gocui screen here —
// Refresh is g.Update-driven.
func (gu *Gui) adaptiveTickLoop(base time.Duration) {
	if base <= 0 {
		base = 2 * time.Second
	}
	for {
		elapsed := time.Duration(gu.lastFetchNS.Load())
		delay := adaptiveDelay(base, elapsed)
		select {
		case <-gu.stopRefresh:
			return
		case <-gu.ctx.Done():
			return
		case <-time.After(delay):
			gu.scheduleRefresh()
		}
	}
}

// spinnerInterval is the cadence at which the Tasks panel's per-row
// spinner advances when at least one task has a live job. 100ms is
// the sweet spot lazygit uses on its loading indicators: fast enough
// that the animation reads as motion (not stutter), slow enough that
// the layout pass it triggers doesn't measurably show up in `top`.
const spinnerInterval = 100 * time.Millisecond

// spinnerLoop ticks Gui.spinnerTick and triggers a no-op g.Update
// (which causes gocui to re-run the layout function, which re-runs
// renderViews, which repaints the Tasks panel with the next braille
// frame) whenever the state has at least one task with a running
// job.
//
// The loop is deliberately data-aware: when no rows have live jobs,
// we sleep without advancing the counter and without poking the
// MainLoop, so a quiet dashboard stays quiet. This is the same
// pattern lazygit's loading-status helper uses to avoid burning
// cycles on an idle screen.
//
// Runs in its own goroutine started by Run(); exits when the gui
// context is cancelled or stopRefresh is closed (same shutdown
// signal adaptiveTickLoop honours).
func (gu *Gui) spinnerLoop() {
	t := time.NewTicker(spinnerInterval)
	defer t.Stop()
	for {
		select {
		case <-gu.stopRefresh:
			return
		case <-gu.ctx.Done():
			return
		case <-t.C:
			if !gu.hasActiveJob() {
				continue
			}
			gu.spinnerTick.Add(1)
			// No-op Update: re-runs layout, which re-runs
			// renderViews and repaints the Tasks panel.
			gu.g.Update(func(_ *gocui.Gui) error { return nil })
		}
	}
}

// hasActiveJob reports whether the jobs snapshot contains at least
// one row whose status is "running" (the only state that a spinner
// can usefully animate; queued jobs are waiting, not working).
// Held under the model's RLock so it's safe to call from the
// spinner goroutine without coordination.
func (gu *Gui) hasActiveJob() bool {
	var any bool
	gu.st.withRLock(func() {
		for i := range gu.st.jobs {
			if runstore.RunStatus(gu.st.jobs[i].Status) == runstore.StatusRunning {
				any = true
				return
			}
		}
	})
	return any
}

// scheduleRefresh enqueues a refresh via the gocui worker. If one is
// already in flight we set refreshPending; the running worker will
// loop until pending is clear, so the LATEST scope change always
// drives a trailing refresh. (The previous implementation dropped
// the latest request, which could leave the Jobs panel showing
// results filtered by a stale cursor row after j-spam.)
//
// scheduleRefresh is a thin convenience over scheduleRefreshWith
// that wires the production work function (gu.refreshAll). Tests
// drive the CAS dance through scheduleRefreshWith with a stub work
// callback so a future refactor of the loop can't silently regress
// the trailing-pickup invariant.
func (gu *Gui) scheduleRefresh() {
	gu.scheduleRefreshWith(gu.refreshAll)
}

// scheduleRefreshWith is the testable shape of scheduleRefresh. The
// CAS loop + pending-flag invariants live here; the production
// entry passes gu.refreshAll, tests pass a stub that mirrors the
// timing (a sleep, a counter increment) without touching gocui.
func (gu *Gui) scheduleRefreshWith(work func()) {
	if !gu.refreshInFlight.CompareAndSwap(false, true) {
		gu.refreshPending.Store(true)
		dlog("scheduleRefresh: coalesced (pending set)")
		return
	}
	gu.runDispatch(func() {
		for {
			work()
			gu.refreshInFlight.Store(false)
			// If a request arrived during work(), run it now. The CAS
			// reclaims the in-flight slot so a concurrent
			// scheduleRefresh either parks in pending or wins the race
			// (in which case we exit here).
			if !gu.refreshPending.CompareAndSwap(true, false) {
				return
			}
			if !gu.refreshInFlight.CompareAndSwap(false, true) {
				return
			}
		}
	})
}

// runDispatch dispatches f onto the gocui worker pool (or, in tests,
// whatever gu.dispatch was set to). The indirection is so
// scheduleRefresh's CAS loop can be exercised without a real gocui.
func (gu *Gui) runDispatch(f func()) {
	if gu.dispatch != nil {
		gu.dispatch(f)
		return
	}
	gu.g.OnWorker(func(_ gocui.Task) error { f(); return nil })
}

// quit is the standard handler for q / Ctrl-C.
func (gu *Gui) quit(*gocui.Gui, *gocui.View) error {
	gu.cancel()
	return gocui.ErrQuit
}

// flashf appends to the command log and sets a transient toast.
//
// State is updated synchronously under the model lock so the next
// render sees it; the redraw is requested via g.Update (a no-op
// closure that just wakes the event loop). The previous shape
// wrapped the state mutation inside g.Update, which meant the
// flash didn't land until MainLoop drained the userEvents channel
// — fine in production but invisible to tests that drive key
// handlers without a MainLoop, AND non-deterministic in terms of
// ordering when multiple flashes fire back-to-back. The lock
// already serialises writes against the layout goroutine, so
// applying directly is safe.
//
// Nil-safe on gu.g: tests that exercise key handlers without a
// real gocui.Gui (e.g. scope_test.go) skip the redraw poke.
func (gu *Gui) flashf(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	gu.st.withLock(func() {
		gu.st.appendLog(msg)
		gu.st.setFlash(msg, level)
	})
	if gu.g != nil {
		gu.g.Update(func(_ *gocui.Gui) error { return nil })
	}
}

// renderViews re-renders every visible view's content from the model.
// Called from the layout function (cheap; the model fields are the
// "rendered slice" we just project to lines).
func (gu *Gui) renderViews() {
	gu.st.withRLock(func() {
		focused := gu.st.focused

		// taskJobIdx is published by applyRefreshLocked from the
		// TaskID-unfiltered jobs read; reading it here under the
		// model's RLock is correct — the spinner ticker only triggers
		// a layout pass, it doesn't refetch.
		spinTick := gu.spinnerTick.Load()
		tasksBody, tasksHdr := renderTasksPanel(
			gu.st.tasks, gu.st.taskCursor,
			gu.st.scope, gu.st.filter.Tasks,
			gu.innerWidth(winTasks), spinTick, gu.st.taskJobIdx,
		)
		gu.writeView(winTasks, "[1] Tasks", tasksBody)
		gu.applyRowHighlight(winTasks, tasksHdr, gu.st.taskCursor, len(gu.st.tasks), focused == panelTasks)

		jobsBody, jobsHdr := renderJobsPanel(gu.st.jobs, gu.st.jobCursor, gu.st.scope, gu.st.filter.Jobs, gu.innerWidth(winJobs))
		gu.writeView(winJobs, "[2] Jobs", jobsBody)
		// Keep the Jobs row highlight on while the caret is in
		// winJobInput (panelJobInput) — the operator is still working
		// the selected running job, just typing instead of navigating.
		jobsHighlighted := focused == panelJobs || focused == panelJobInput
		gu.applyRowHighlight(winJobs, jobsHdr, gu.st.jobCursor, len(gu.st.jobs), jobsHighlighted)

		wfBody, wfHdr := renderWorkflowsPanel(gu.st.workflows, gu.st.workflowCursor, gu.st.filter.Workflows)
		gu.writeView(winWorkflows, "[3] Workflows", wfBody)
		gu.applyRowHighlight(winWorkflows, wfHdr, gu.st.workflowCursor, len(gu.st.workflows), focused == panelWorkflows)

		agBody, agHdr := renderAgentsPanel(gu.st.agents, gu.st.agentCursor, gu.st.filter.Agents)
		gu.writeView(winAgents, "[4] Agents", agBody)
		gu.applyRowHighlight(winAgents, agHdr, gu.st.agentCursor, len(gu.st.agents), focused == panelAgents)

		// Detail pane: writeViewSticky's bottom-anchor behaviour is
		// the right policy ONLY for the live job transcript — it auto-
		// tails new events as they arrive. For Task / Workflow / Agent
		// detail the same policy would wrongly scroll past the title /
		// description on the very first render (the first frame's
		// prevLines==0 path always fires sticky), so use a plain
		// writeView for those branches.
		//
		// When the entity rendered into the pane changes between
		// frames (cursor moves to a different task, focus flips from
		// Tasks to Jobs, etc.), clear winDetail's buffer + reset its
		// origin to (0,0) so the new entity starts from its natural
		// anchor: top for non-job, bottom for job (writeViewSticky
		// snaps to bottom because prevLines is now 0). Clearing the
		// buffer also matters for the sticky-tail computation:
		// without it, leftover lines from the previous entity would
		// fail the "at bottom" predicate and the new job's transcript
		// would land at the top instead of tailing the latest events.
		curDetailKey := detailEntityKey(gu.st)
		isJobDetail := detailShowsJob(gu.st)
		if curDetailKey != gu.lastDetailKey {
			if v, err := gu.g.View(winDetail); err == nil && v != nil {
				v.Clear()
				v.SetOrigin(0, 0)
			}
			gu.invalidateBodyCache(winDetail)
			gu.lastDetailKey = curDetailKey
		}
		detailBody := renderDetail(gu.st, gu.innerWidth(winDetail))
		if isJobDetail {
			gu.writeViewSticky(winDetail, "[0] Detail", detailBody)
		} else {
			gu.writeView(winDetail, "[0] Detail", detailBody)
		}
		gu.writeView(winLog, "log", renderCommandLog(gu.st.logBuf, gu.st.flash))
		gu.writeView(winStatusBar, "", renderStatusBar(gu.st, gu.opts.ProjectRoot))
		// Options strip: context-aware bottom-row hint for the
		// focused panel. Reads from the same bindingSpec registry
		// that drives the cheatsheet popup; the renderer truncates
		// to the strip's inner width with a "…" suffix when needed.
		optsWin := gu.st.focused.normalizeForDetail().window()
		gu.writeView(winOptionsStrip, "", renderOptionsStrip(gu.bindingSpecs(), optsWin, gu.innerWidth(winOptionsStrip)))

		// Job-input textarea is allocated by layout whenever the
		// selected job is non-terminal AND the Detail pane is
		// currently showing that job. The Detail-pane gate matters:
		// when the operator is on the Tasks panel (Detail shows the
		// task), the input must NOT appear just because the Jobs
		// panel's cursor happens to point at a live job. renderViews
		// mirrors layout's gate so writeView doesn't poke at a view
		// that doesn't exist this frame. When the job is terminal
		// the input view is absent and the Detail pane uses its full
		// inner height for the transcript.
		if isJobDetail {
			if j, ok := gu.st.selectedJob(); ok && isJobLive(j) {
				title := "input — ctrl+d send  ctrl+f follow_up  ctrl+a abort  esc cancel"
				gu.writeView(winJobInput, title, gu.st.jobInput)
			}
		}
	})
}

// applyRowHighlight wires gocui's per-line Highlight to the panel's
// model cursor: when the panel is focused AND has rows, we set
// SelBgColor to the palette's Selection swatch and move the view's
// cy to the cursor row so gocui paints that row's background.
//
// Non-focused panels and empty panels get Highlight=false so the
// cursor row is invisible — matching the lazygit / file-listed-view
// affordance: "only the active list shows where you are".
//
// cyAbs is the cursor's absolute y-index in the view buffer
// (headerLines + modelCursor). The viewport origin (v.oy) is updated
// here to keep cyAbs visible: keystroke-driven moves are handled
// proactively in scrollOffAdjust (lazygit's scroll-off-margin
// algorithm with the cursor's expected motion direction), but THIS
// function is the catch-all that snaps the cursor back into view
// when it leaves the viewport non-contiguously — a filter / scope
// change that shrinks the list, a refresh that lands the cursor on
// a different model index, etc. Without this snap a 100-item list
// scrolled to oy=80 that suddenly becomes 5 items would render
// nothing in the visible window.
//
// rowCount lets us suppress the highlight on the "(no tasks)" / etc.
// placeholder line; without that check the cursor would visibly land
// on the placeholder and look like the empty state is selectable.
func (gu *Gui) applyRowHighlight(name string, headerLines, modelCursor, rowCount int, focused bool) {
	v, err := gu.g.View(name)
	if err != nil || v == nil {
		return
	}
	if !focused || rowCount == 0 {
		v.Highlight = false
		return
	}
	ox, oy := v.Origin()
	vpH := viewportInnerHeight(v)
	cyAbs := headerLines + modelCursor
	totalLines := headerLines + rowCount

	// Clamp origin so blank rows past the content are never visible
	// (e.g. a filter just shrank the list and the old origin overshoots).
	//
	// Deliberately NOT snapping the origin to the cursor here: wheel
	// scrolling needs the viewport to stay where the user put it,
	// even if that hides the selection. Cursor moves (j/k) and focus
	// changes (1/2/3/4, Tab) call snapViewportToCursor / scrollOffAdjust
	// explicitly when they need the cursor back in view — same split
	// lazygit makes between "navigation" and "scroll" verbs.
	maxOY := totalLines - vpH
	if maxOY < 0 {
		maxOY = 0
	}
	if oy > maxOY {
		oy = maxOY
	}
	if oy < 0 {
		oy = 0
	}
	v.SetOrigin(ox, oy)

	v.Highlight = true
	v.SelBgColor = theme.Active().Selection.Gocui()
	// SelFgColor=Default so the per-column foregrounds we set in
	// renderXxxPanel survive the highlight overlay (gocui only forces
	// the cell's bg to SelBgColor and bolds the fg; it leaves the fg
	// hue alone for RGB Attributes).
	v.SelFgColor = gocui.ColorDefault
	// v.cy is interpreted by gocui as VISIBLE y (relative to v.oy), so
	// the absolute index has to be re-relativised here — SetCursor
	// writes v.cy verbatim and the highlight logic in gocui's
	// setCharacter compares it against the loop's visible y.
	v.SetCursor(0, cyAbs-oy)
}

// viewportInnerHeight returns the number of usable list rows in v
// (its outer height minus the 2-cell frame, clamped to >=1 so a
// degenerate height doesn't make divisions blow up).
func viewportInnerHeight(v *gocui.View) int {
	_, h := v.Size()
	vpH := h - 2
	if vpH < 1 {
		vpH = 1
	}
	return vpH
}

// scrollOffMargin is the look-ahead distance (in rows) the cursor
// keeps from the top/bottom of the viewport during keystroke moves.
// Matches lazygit's `gui.scrollOffMargin` default — the user
// reported expectation is two extra rows of context past the cursor
// in the direction of motion.
const scrollOffMargin = 2

// scrollOffAdjust positions the named view's origin so the cursor
// (whose absolute y went from cyAbsBefore to cyAbsAfter via one
// keystroke) is visible after the move. Two paths, both handled by
// the pure-Go scrollOffOrigin helper:
//
//  1. `before` was in the viewport — apply lazygit's
//     scrollOffMargin algorithm so the viewport edges have at least
//     `scrollOffMargin` rows of look-ahead in the direction of motion.
//  2. `after` is still outside the viewport (either because the user
//     wheel-scrolled the selection out of view and is now navigating
//     with j/k, or because the margin algorithm didn't pull it back
//     far enough) — centre the viewport on the cursor. Mirrors
//     gocui's calculateNewOrigin and lazygit's FocusPoint
//     (scrollIntoView=true) fallback.
func (gu *Gui) scrollOffAdjust(name string, cyAbsBefore, cyAbsAfter int) {
	v, err := gu.g.View(name)
	if err != nil || v == nil {
		return
	}
	ox, oy := v.Origin()
	vpH := viewportInnerHeight(v)
	newOY := scrollOffOrigin(cyAbsBefore, cyAbsAfter, oy, vpH, scrollOffMargin)
	v.SetOrigin(ox, newOY)
}

// scrollOffOrigin is the pure-Go core of scrollOffAdjust: returns the
// viewport origin that should be in effect after a keystroke moved
// the cursor from `before` to `after`. Extracted so the
// margin-then-centre composition is testable without a real gocui.View.
func scrollOffOrigin(before, after, oy, vpH, margin int) int {
	delta := scrollOffDelta(before, after, oy, vpH, margin)
	oy += delta
	if after < oy || after >= oy+vpH {
		oy = after - vpH/2
	}
	if oy < 0 {
		oy = 0
	}
	return oy
}

// snapViewportToCursor recentres the panel's viewport on its cursor
// when the cursor isn't currently visible — mirrors lazygit's
// FocusLine(scrollIntoView=true) behaviour for the case where focus
// returns to a panel that the user previously wheel-scrolled away
// from. No-op for the detail pane (no cursor to anchor) and for any
// panel whose cursor is already in view.
func (gu *Gui) snapViewportToCursor(p panelID) {
	if p == panelDetail {
		return
	}
	name := p.window()
	var header, cursor, rowCount int
	gu.st.withRLock(func() {
		header = headerLinesForLocked(p, gu.st)
		switch p {
		case panelTasks:
			cursor = gu.st.taskCursor
			rowCount = len(gu.st.tasks)
		case panelJobs:
			cursor = gu.st.jobCursor
			rowCount = len(gu.st.jobs)
		case panelWorkflows:
			cursor = gu.st.workflowCursor
			rowCount = len(gu.st.workflows)
		case panelAgents:
			cursor = gu.st.agentCursor
			rowCount = len(gu.st.agents)
		}
	})
	v, err := gu.g.View(name)
	if err != nil || v == nil || rowCount == 0 {
		return
	}
	ox, oy := v.Origin()
	vpH := viewportInnerHeight(v)
	cyAbs := header + cursor
	if cyAbs >= oy && cyAbs < oy+vpH {
		return // already visible
	}
	newOY := cyAbs - vpH/2
	if newOY < 0 {
		newOY = 0
	}
	totalLines := header + rowCount
	maxOY := totalLines - vpH
	if maxOY < 0 {
		maxOY = 0
	}
	if newOY > maxOY {
		newOY = maxOY
	}
	v.SetOrigin(ox, newOY)
}

// panelScrollStep is the wheel-tick scroll distance for side panels.
// lazygit's gui.scrollHeight defaults to 2 lines per wheel tick — same.
const panelScrollStep = 2

// panelScroll moves the named panel's vertical origin by `step` rows
// WITHOUT touching the model cursor. lazygit's wheel-on-list
// affordance: the viewport scrolls free of the selection so the user
// can peek at surrounding content while the selection stays anchored.
//
// Bounds: clamped to [0, max(0, totalLines - vpH)] so wheeling past
// the end of the list doesn't expose blank rows below the content.
func (gu *Gui) panelScroll(p panelID, step int) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		name := p.window()
		v, err := gu.g.View(name)
		if err != nil || v == nil {
			return nil
		}
		var totalLines int
		gu.st.withRLock(func() {
			header := headerLinesForLocked(p, gu.st)
			var rowCount int
			switch p {
			case panelTasks:
				rowCount = len(gu.st.tasks)
			case panelJobs:
				rowCount = len(gu.st.jobs)
			case panelWorkflows:
				rowCount = len(gu.st.workflows)
			case panelAgents:
				rowCount = len(gu.st.agents)
			}
			totalLines = header + rowCount
		})
		ox, oy := v.Origin()
		vpH := viewportInnerHeight(v)
		newOY := oy + step
		if newOY < 0 {
			newOY = 0
		}
		maxOY := totalLines - vpH
		if maxOY < 0 {
			maxOY = 0
		}
		if newOY > maxOY {
			newOY = maxOY
		}
		if newOY != oy {
			v.SetOrigin(ox, newOY)
		}
		return nil
	}
}

// scrollOffDelta is the pure-Go core of scrollOffAdjust — returns the
// number of lines to add to the viewport origin (signed) so the
// cursor sits at least `margin` rows from the leading edge after the
// move. Extracted so the scroll-off math is testable without a real
// gocui.View.
//
// The two branches mirror lazygit's calculateLinesToScroll{Up,Down}:
//
//   - The (vpH+1)/2 and (vpH-1)/2 caps ensure that on even-height
//     viewports the top margin is one row taller than the bottom,
//     matching lazygit's centring bias on j-spam.
//   - The `before` visibility guard avoids scrolling when the user
//     dragged the cursor out-of-view with the mouse (or via a
//     non-keystroke event) — there's no "before" position inside
//     the viewport to anchor the scroll to.
func scrollOffDelta(before, after, oy, vpH, margin int) int {
	switch {
	case after < before: // moving up
		capped := margin
		if c := (vpH + 1) / 2; capped > c {
			capped = c
		}
		if before >= oy && before < oy+vpH {
			marginEnd := oy + capped
			if after < marginEnd {
				return after - marginEnd // negative
			}
		}
	case after > before: // moving down
		capped := margin
		if c := (vpH - 1) / 2; capped > c {
			capped = c
		}
		if before >= oy && before < oy+vpH {
			marginStart := oy + vpH - capped - 1
			if after > marginStart {
				return after - marginStart // positive
			}
		}
	}
	return 0
}

// headerLinesForLocked returns the number of leading meta lines
// (scope notes + filter pill) that renderXxxPanel will emit before
// the data rows for panel p, given the current state. Must be called
// with the model lock held — it reads gu.st.scope / gu.st.filter.
//
// Kept in lock-step with the matching `header++` paths in
// renderTasksPanel / renderJobsPanel / renderWorkflowsPanel /
// renderAgentsPanel. If a render path adds a new meta line, mirror
// it here or the scroll-off math will be off by that many rows.
func headerLinesForLocked(p panelID, st *state) int {
	switch p {
	case panelTasks:
		h := 0
		if st.scope.WorkflowName != "" {
			h++
		}
		if st.scope.Agent != "" {
			h++
		}
		if st.filter.Tasks != "" {
			h++
		}
		return h
	case panelJobs:
		h := 0
		if st.scope.TaskID != "" {
			h++
		}
		if st.filter.Jobs != "" {
			h++
		}
		return h
	case panelWorkflows:
		if st.filter.Workflows != "" {
			return 1
		}
	case panelAgents:
		if st.filter.Agents != "" {
			return 1
		}
	}
	return 0
}

// taskJobIndex is the per-row projection of the jobs snapshot that
// the Tasks panel consults for its job-presence markers:
//
//   - Active is the set of tasks with at least one running job. It
//     drives the animated braille spinner (blue, task-id hue).
//   - Any is the set of tasks with at least one job in ANY status
//     (queued / running / done / failed / cancel). It drives the
//     static ">" marker (magenta, job-id hue) shown when no spinner
//     is animating.
//
// Active ⊆ Any by construction — the renderer picks the spinner
// first and falls back to the marker only when the row isn't
// actively running. Pre-sized maps from len(jobs) so the common
// "one task per job" case avoids a rehash.
//
// The index is built at refresh time from the TaskID-UNFILTERED
// jobs read (see fetchRefresh), NOT from the filtered slice that
// drives the Jobs panel. Otherwise an active scope.TaskID would
// shrink the Jobs query down to one task and every other row in
// Tasks would lose its marker — a regression noticed the first time
// an operator pressed Space on a row that had jobs.
type taskJobIndex struct {
	Active map[string]bool
	Any    map[string]bool
}

// taskJobIndexFromJobs builds a taskJobIndex from a jobs slice.
// Pure function so refresh.go can call it on the unfiltered fetch
// result (no lock needed; the slice is owned by the calling
// goroutine until applyRefreshLocked publishes it). Returns empty
// (but non-nil) maps when there are no jobs so the renderer can do
// a single map lookup per row without nil-checking.
func taskJobIndexFromJobs(jobs []datasource.Job) taskJobIndex {
	idx := taskJobIndex{
		Active: make(map[string]bool, len(jobs)),
		Any:    make(map[string]bool, len(jobs)),
	}
	for i := range jobs {
		tid := jobs[i].TaskID
		if tid == "" {
			continue
		}
		idx.Any[tid] = true
		if runstore.RunStatus(jobs[i].Status) == runstore.StatusRunning {
			idx.Active[tid] = true
		}
	}
	return idx
}

// innerWidth returns the named view's inner (frame-excluded) width
// in cells, or 0 if the view hasn't been laid out yet. Used by panel
// renderers that need to size their right-edge columns. Tolerates
// the view not existing yet (first-frame race against the layout
// pass) by returning 0 — callers fall back to a sensible default.
func (gu *Gui) innerWidth(name string) int {
	v, err := gu.g.View(name)
	if err != nil || v == nil {
		return 0
	}
	w, _ := v.InnerSize()
	return w
}

// writeView writes content into a view, clearing first. Tolerates the
// view not existing yet (the layout function may not have created it
// on the very first frame, and the current viewState may not include
// the requested window).
//
// Short-circuit: if the body string AND the view's outer dimensions
// match the last commit recorded in bodyCache, we skip the
// v.Clear()+v.Write() pair entirely. gocui parses the SGR escape
// stream byte-by-byte inside View.Write (escape.go), so on a
// multi-KB rendered detail pane the savings on a spinner-tick frame
// are substantial. Title and Wrap are still pushed onto the view
// every call because they're cheap field assignments and not
// covered by the body cache.
//
// Returns true when the body / dimensions ACTUALLY changed (we
// took the slow path and ran v.Clear()+v.Write()), false on a
// cache-hit short-circuit. writeViewSticky reads this to skip its
// own follow-up viewBufferLineCount calls — on a stable winDetail
// (no live events, no resize) the spinner-tick path now does no
// O(N) work at all past the body-string equality check.
func (gu *Gui) writeView(name, title, body string) bool {
	v, err := gu.g.View(name)
	if err != nil {
		return false
	}
	x0, y0, x1, y1 := v.Dimensions()
	if e, ok := gu.bodyCache[name]; ok && e.body == body && e.x0 == x0 && e.y0 == y0 && e.x1 == x1 && e.y1 == y1 {
		// Body and frame are unchanged; title / wrap are idempotent
		// field writes so still push them through.
		if title != "" {
			v.Title = title
		}
		v.Wrap = false
		return false
	}
	v.Clear()
	if title != "" {
		v.Title = title
	}
	v.Wrap = false
	_, _ = v.Write([]byte(body))
	if gu.bodyCache == nil {
		gu.bodyCache = make(map[string]bodyCacheEntry)
	}
	// Cache the line count alongside the body. viewBufferLineCount
	// over v.Buffer() would be O(N) over the freshly-rendered
	// (ANSI-laden) buffer; counting newlines in the source body is
	// cheaper AND lets writeViewSticky reuse the count without
	// re-scanning.
	//
	// The trailing '\n' on a non-editable view is held by gocui as
	// pendingNewline (view.go::write: "if until>0 && p[until-1]=='\n'
	// then pendingNewline=true; until--") so it does NOT add a line
	// to v.lines until the NEXT write. v.Buffer() reflects v.lines
	// only, which means the source body and v.Buffer() differ by
	// one trailing newline when the body ends with '\n'. The
	// trim+count formula below matches what viewBufferLineCount(v)
	// would return after this write, so writeViewSticky can read
	// the cached value instead of re-scanning the buffer twice
	// per frame.
	gu.bodyCache[name] = bodyCacheEntry{body: body, x0: x0, y0: y0, x1: x1, y1: y1, lineCount: bodyLineCount(body)}
	return true
}

// bodyLineCount predicts viewBufferLineCount(v) for the v.Buffer()
// gocui will hold after Write(body) on a non-editable view.
// Mirrors viewBufferLineCount's "0 for empty, otherwise newline
// count + 1" semantic, modulo the trailing-newline pendingNewline
// quirk: gocui holds a final '\n' as pendingNewline so it does
// NOT contribute a line to v.lines (and therefore to v.Buffer())
// until the next Write fires. The trim below mirrors that quirk
// so the cached count agrees byte-for-byte with what
// viewBufferLineCount would return.
func bodyLineCount(body string) int {
	if body == "" {
		return 0
	}
	if body[len(body)-1] == '\n' {
		body = body[:len(body)-1]
	}
	if body == "" {
		return 0
	}
	return strings.Count(body, "\n") + 1
}

// invalidateBodyCache drops the cached body for name, if any. Used
// by layout when SetView returns ErrUnknownView (the view was just
// re-created with an empty buffer, so a cached "matches" entry
// would skip the write and leave the pane visibly blank) and after
// DeleteView.
func (gu *Gui) invalidateBodyCache(name string) {
	delete(gu.bodyCache, name)
}

// viewBufferLineCount returns the number of lines that gocui's
// renderer will draw for the view's current buffer. The naive
// approach (`strings.Count(v.Buffer(), "\n")`) is off by one:
// gocui's linesToString joins v.lines with '\n' WITHOUT a
// trailing separator (see gocui/view.go::linesToString), so a
// buffer holding N visible lines reports N-1 newlines. Using the
// raw count for the bottom-anchor / scroll-clamp math leaves the
// viewport positioned one row higher than it should be, which
// visibly clips the very last line of content — in the Detail
// pane that's the bottom border of the last drawLabeledBox.
//
// Returns 0 for an empty buffer (the +1 doesn't apply when there
// are no lines at all).
func viewBufferLineCount(v *gocui.View) int {
	buf := v.Buffer()
	if buf == "" {
		return 0
	}
	return strings.Count(buf, "\n") + 1
}

// writeViewSticky is writeView + a sticky-tail behaviour: if the
// view was scrolled to the bottom BEFORE the new body landed (or the
// view was empty / freshly created), the viewport origin is moved
// after the write so the tail stays visible. If the user had scrolled
// up, the origin is left alone so wheel/PgUp positions are preserved
// across live-event appends and 2s refresh ticks.
//
// Used by winDetail only. Tasks / Jobs / Workflows / Agents already
// manage their origin via the cursor-highlight loop, and applying
// sticky-tail there would break the wheel-scroll-keeps-its-place
// affordance.
//
// For winDetail the at-bottom check uses the effective visible
// height while winJobInput is already present, so a small manual
// scroll-up above the overlay is respected on the next live redraw.
// The post-write target also uses detailEffectiveInnerH.
//
// The exception is the frame where winJobInput APPEARS (e.g. a
// queued job promotes to running; or the selection crosses into a
// non-terminal row from a terminal one). The user was "at the
// bottom" of the previous viewport — with the overlay absent, the
// full inner height was visible. Using the new (smaller) effective
// inner height for that first threshold would mis-classify them as
// "scrolled up" and the overlay would silently hide the last few
// rows of content they were just looking at. Using the full inner
// height for the overlay-appears threshold preserves the at-bottom
// anchor across that transition.
func (gu *Gui) writeViewSticky(name, title, body string) {
	v, err := gu.g.View(name)
	if err != nil || v == nil {
		return
	}
	// Snapshot the BEFORE state before writeView's potential clear.
	ox, oy := v.Origin()
	_, fullH := v.InnerSize()
	if fullH < 1 {
		fullH = 1
	}
	detailOverlayVisible := false
	if name == winDetail {
		if _, err := gu.g.View(winJobInput); err == nil {
			detailOverlayVisible = true
		}
	}
	thresholdH := fullH
	if name == winDetail && detailOverlayVisible && gu.lastDetailOverlay {
		thresholdH = gu.detailEffectiveInnerH()
	}
	// prevLines comes from the bodyCache when available — that's
	// the line count writeView stamped after the last actual write.
	// The viewBufferLineCount fallback covers two cases: the very
	// first writeViewSticky call (before any bodyCache entry has
	// been stamped) and a layout-driven invalidateBodyCache that
	// dropped the entry after a view recreate / DeleteView.
	prevLines := 0
	if e, ok := gu.bodyCache[name]; ok {
		prevLines = e.lineCount
	} else {
		prevLines = viewBufferLineCount(v)
	}
	// First frame OR user is at (or past) the bottom → sticky.
	// `oy + thresholdH >= prevLines` is true exactly when the visible
	// region [oy, oy+thresholdH) already covers the last line index
	// (prevLines-1). Using prevLines as a true line count (rather
	// than the off-by-one newline count) is required for both this
	// predicate AND the post-write target below.
	beforeSticky := prevLines == 0 || oy+thresholdH >= prevLines

	gu.writeView(name, title, body)
	if name == winDetail {
		gu.lastDetailOverlay = detailOverlayVisible
	}

	if !beforeSticky {
		return
	}
	// writeView may have short-circuited (body unchanged); in that
	// case the buffer is still the previous body and the cached
	// lineCount is still accurate. We deliberately DO NOT early-
	// return on the short-circuit path: the winJobInput overlay can
	// appear / disappear between two byte-identical body writes
	// (e.g. cursor crosses from a terminal job to a non-terminal
	// one), and that shrinks / grows the effective inner height
	// used by the post-write target below — pinned by
	// TestStickyTail_OverlayAppears_TailStaysVisible. The recompute
	// is still O(1) because we read the line count from bodyCache
	// (stamped by writeView, no second strings.Count pass over
	// the ANSI-laden buffer).
	var h int
	if name == winDetail {
		h = gu.detailEffectiveInnerH()
	} else {
		h = fullH
	}
	newLines := 0
	if e, ok := gu.bodyCache[name]; ok {
		newLines = e.lineCount
	} else {
		newLines = viewBufferLineCount(v)
	}
	target := newLines - h
	if target < 0 {
		target = 0
	}
	v.SetOrigin(ox, target)
}

// truncateLines crops a string to at most n lines (helps small views).
func truncateLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) <= n {
		return s
	}
	return strings.Join(parts[:n], "\n")
}
