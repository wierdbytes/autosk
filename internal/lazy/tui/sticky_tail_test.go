package tui

import (
	"strings"
	"testing"
)

// stickyTailFixture wires a small headless gui + a winDetail view
// sized to a known height. The caller writes a first body to seed
// the buffer, then writes a second body to exercise sticky-tail.
func stickyTailFixture(t *testing.T, w, h int) (*Gui, string) {
	t.Helper()
	gu := newHeadlessGui(t, w, h)
	// Create winDetail at a known size. The view height of 10 + frame
	// gives us inner height = 8 lines so the math is easy to reason
	// about.
	if _, err := gu.g.SetView(winDetail, 0, 0, w-1, 10, 0); err != nil && !isUnknownView(err) {
		t.Fatalf("SetView: %v", err)
	}
	return gu, winDetail
}

// TestStickyTail_AtBottom_FollowsNewContent: when the view is at the
// bottom and a longer body arrives, the origin moves so the tail
// stays visible.
func TestStickyTail_AtBottom_FollowsNewContent(t *testing.T) {
	gu, name := stickyTailFixture(t, 80, 24)
	v, _ := gu.g.View(name)
	if v == nil {
		t.Fatalf("view %q nil after SetView", name)
	}

	// First body: 5 lines (fits in the view, oy=0).
	first := strings.Repeat("line\n", 5)
	gu.writeViewSticky(name, "", first)
	if _, oy := v.Origin(); oy != 0 {
		t.Fatalf("first frame origin should be 0; got %d", oy)
	}

	// Second body: 100 lines. The user was at the bottom (since the
	// first body fit in the viewport), so the origin must advance.
	second := strings.Repeat("line\n", 100)
	gu.writeViewSticky(name, "", second)
	_, oy := v.Origin()
	if oy == 0 {
		t.Errorf("sticky-tail did not advance origin after body grew; oy=%d", oy)
	}
	// The bottom of the viewport must be at or past the body's last
	// line. Inner height is the writable area inside the frame.
	_, h := v.InnerSize()
	newLines := strings.Count(v.Buffer(), "\n")
	t.Logf("after second write: oy=%d innerH=%d bufferLines=%d", oy, h, newLines)
	bottom := oy + h
	if bottom < newLines {
		t.Errorf("sticky tail viewport bottom %d below body length %d", bottom, newLines)
	}
}

// TestStickyTail_ScrolledUp_OriginPreserved: when the user has
// scrolled up, a follow-up writeViewSticky must NOT yank the
// viewport back to the bottom.
func TestStickyTail_ScrolledUp_OriginPreserved(t *testing.T) {
	gu, name := stickyTailFixture(t, 80, 24)
	v, _ := gu.g.View(name)

	first := strings.Repeat("line\n", 100)
	gu.writeViewSticky(name, "", first)
	// Manually scroll up by setting origin to 5.
	ox, _ := v.Origin()
	v.SetOrigin(ox, 5)

	// New body lands; sticky-tail must respect the scrolled-up origin.
	second := strings.Repeat("line\n", 200)
	gu.writeViewSticky(name, "", second)
	_, oy := v.Origin()
	if oy != 5 {
		t.Errorf("sticky-tail clobbered scrolled-up origin; oy=%d want 5", oy)
	}
}

// TestStickyTail_FirstFrame_StartsAtBottom: an empty view receiving
// its first body must start anchored at the bottom (so live events
// arriving on first frame are immediately visible).
func TestStickyTail_FirstFrame_StartsAtBottom(t *testing.T) {
	gu, name := stickyTailFixture(t, 80, 24)
	v, _ := gu.g.View(name)

	body := strings.Repeat("line\n", 50)
	gu.writeViewSticky(name, "", body)
	_, oy := v.Origin()
	if oy == 0 {
		t.Errorf("first-frame sticky tail did not advance origin; oy=%d (body has 50 lines, view height ~10)", oy)
	}
}

// TestStickyTail_LiveOverlayVisible_ManualScrollUpPreserved pins the
// live-job overlay regression: once winJobInput is already visible,
// a one-line manual scroll-up must survive the next redraw instead
// of being classified as sticky by the full, overlay-covered height.
func TestStickyTail_LiveOverlayVisible_ManualScrollUpPreserved(t *testing.T) {
	gu := newHeadlessGui(t, 80, 40)
	v, err := gu.g.SetView(winDetail, 0, 0, 79, 30, 0)
	if err != nil && !isUnknownView(err) {
		t.Fatalf("SetView detail: %v", err)
	}
	if v == nil {
		t.Fatal("SetView returned nil")
	}
	v.Frame = true
	if _, err := gu.g.SetView(winJobInput, 5, 25, 75, 29, 0); err != nil && !isUnknownView(err) {
		t.Fatalf("SetView jobInput: %v", err)
	}

	body := strings.Repeat("line\n", 100)
	gu.writeViewSticky(winDetail, "", body)
	effH := gu.detailEffectiveInnerH()
	lineCount := viewBufferLineCount(v)
	wantBottom := lineCount - effH
	_, oy := v.Origin()
	if oy != wantBottom {
		t.Fatalf("setup: expected overlay bottom anchor at oy=%d, got %d (lineCount=%d effH=%d)", wantBottom, oy, lineCount, effH)
	}

	// Simulate the operator scrolling Detail up by only one line while
	// the live-job input overlay is visible. The old threshold used the
	// full inner height, so oy+fullH still reached the tail and the next
	// live redraw yanked the viewport back down.
	ox, _ := v.Origin()
	v.SetOrigin(ox, wantBottom-1)
	want := wantBottom - 1
	gu.writeViewSticky(winDetail, "", body)
	_, oy = v.Origin()
	if oy != want {
		t.Errorf("sticky-tail clobbered one-line manual scroll above live overlay; oy=%d want %d", oy, want)
	}
}

// TestStickyTail_OverlayAppears_TailStaysVisible pins the
// SUBSTANTIVE regression review flagged: when the winJobInput
// overlay appears between frames (e.g. the cursor moves from a
// terminal job to a non-terminal one, or a queued job promotes
// to running) the operator's at-bottom position must be
// preserved.
//
// The bug was that writeViewSticky computed beforeSticky against
// detailEffectiveInnerH (which is already shrunk by the overlay
// when the overlay is present this frame). On the appears
// transition the operator was at the bottom of the previous
// (overlay-less) viewport, but the new effective height makes
// the check fail, leaving the overlay covering the rows they
// were just reading. The fix is to compute beforeSticky against
// the full InnerSize() only on the overlay-appears frame and use
// the effective height for the post-write target.
func TestStickyTail_OverlayAppears_TailStaysVisible(t *testing.T) {
	gu := newHeadlessGui(t, 80, 40)
	// Create winDetail at a known size. Inner height ~ 28.
	v, err := gu.g.SetView(winDetail, 0, 0, 79, 30, 0)
	if err != nil && !isUnknownView(err) {
		t.Fatalf("SetView detail: %v", err)
	}
	if v == nil {
		t.Fatal("SetView returned nil")
	}
	v.Frame = true

	// Seed a 100-line body and anchor at the bottom (no overlay
	// present yet, so detailEffectiveInnerH == InnerSize).
	//
	// Use viewBufferLineCount() (not strings.Count(buf, "\n")):
	// gocui's linesToString joins v.lines with '\n' without a
	// trailing separator, so the raw newline count under-reports
	// the actual line count by 1. The bottom-anchor target is
	// `lineCount - effectiveH`, so the buggy formula would leave
	// the last line (the bottom border of the last drawLabeledBox
	// in production) one row past the visible region.
	body := strings.Repeat("line\n", 100)
	gu.writeViewSticky(winDetail, "", body)
	_, innerH := v.InnerSize()
	lineCount := viewBufferLineCount(v)
	wantBefore := lineCount - innerH
	_, oy := v.Origin()
	if oy != wantBefore {
		t.Fatalf("setup: expected sticky-tail anchor at oy=%d, got %d (lineCount=%d innerH=%d)", wantBefore, oy, lineCount, innerH)
	}

	// Now create the overlay (winJobInput). Its presence is what
	// detailEffectiveInnerH() consults to subtract jobInputOverlayH
	// from winDetail's visible region. We don't need real coords —
	// just the view's existence is the signal.
	if _, err := gu.g.SetView(winJobInput, 5, 25, 75, 29, 0); err != nil && !isUnknownView(err) {
		t.Fatalf("SetView jobInput: %v", err)
	}

	// Same body again — writeViewSticky should recognise the user is
	// (still) at the bottom of the underlying content (full inner
	// height threshold) and re-anchor against the SHRUNK effective
	// height so the tail stays visible above the overlay.
	gu.writeViewSticky(winDetail, "", body)
	effH := gu.detailEffectiveInnerH()
	if effH >= innerH {
		t.Fatalf("detailEffectiveInnerH did not shrink: was %d, still %d", innerH, effH)
	}
	wantAfter := lineCount - effH
	_, oy = v.Origin()
	if oy != wantAfter {
		t.Errorf("sticky-tail did not re-anchor on overlay-appears: oy=%d, want %d (effH=%d, lineCount=%d)", oy, wantAfter, effH, lineCount)
	}
	// The crucial invariant: the last line of content sits within
	// the visible region (i.e. above the overlay). After the
	// off-by-one fix, oy+effH == lineCount when anchored at the
	// bottom; the old formula left oy+effH == lineCount-1 and the
	// last line was permanently invisible.
	if oy+effH < lineCount {
		t.Errorf("sticky-tail viewport bottom %d < body length %d: overlay covers tail", oy+effH, lineCount)
	}
}

// TestDetailScrollTo_BottomUsesInnerSize pins two regressions in
// the bottom-anchor math:
//
//  1. detailScrollTo / detailScrollPage / scrollViewByLines must
//     clamp against InnerSize (frame-excluded), not Size — otherwise
//     G lands 2 lines short of the actual bottom and the last 2
//     lines of the transcript are permanently invisible via j or
//     wheel-down.
//  2. The line-count used in the clamp must be the actual line
//     count (`viewBufferLineCount`), NOT `strings.Count(buf, "\n")`.
//     gocui's linesToString joins v.lines with '\n' without a
//     trailing separator, so the newline count is off by one. The
//     buggy version left G one row short of the last line, which
//     in production rendered as a missing bottom border on the
//     last drawLabeledBox in winDetail (“у последнего блока не
//     видно нижней границы” operator report).
func TestDetailScrollTo_BottomUsesInnerSize(t *testing.T) {
	gu := newHeadlessGui(t, 80, 40)
	// Outer view height 31, inner 29 (frame eats 2 cells).
	v, err := gu.g.SetView(winDetail, 0, 0, 79, 30, 0)
	if err != nil && !isUnknownView(err) {
		t.Fatalf("SetView: %v", err)
	}
	if v == nil {
		t.Fatal("SetView returned nil")
	}
	v.Frame = true
	body := strings.Repeat("line\n", 100)
	v.Clear()
	if _, err := v.Write([]byte(body)); err != nil {
		t.Fatalf("v.Write: %v", err)
	}
	_, innerH := v.InnerSize()
	if innerH < 1 {
		t.Fatalf("innerH=%d; fixture broken", innerH)
	}
	lineCount := viewBufferLineCount(v)
	want := lineCount - innerH

	// G → bottom.
	if err := gu.detailScrollTo(true)(nil, nil); err != nil {
		t.Fatalf("detailScrollTo: %v", err)
	}
	_, oy := v.Origin()
	if oy != want {
		t.Errorf("detailScrollTo(true) origin oy=%d want %d (innerH=%d, lineCount=%d)", oy, want, innerH, lineCount)
	}
	// Sanity: the buggy Size()-based math would have landed at oy =
	// lineCount - outerH = lineCount - (innerH+2). Make sure we're
	// not there.
	bug := lineCount - (innerH + 2)
	if oy == bug {
		t.Errorf("detailScrollTo(true) landed at the buggy Size() offset oy=%d (would be %d short of bottom)", oy, want-oy)
	}
	// Belt-and-braces: also rule out the off-by-one (strings.Count)
	// regression. The buggy newline-count formula would land at
	// oy = (lineCount-1) - innerH = want - 1.
	if oy == want-1 {
		t.Errorf("detailScrollTo(true) landed at the off-by-one origin oy=%d (last line hidden)", oy)
	}

	// Verify scrollViewByLines also uses InnerSize: from oy=0 a
	// massive forward step must clamp at the same lines-innerH.
	v.SetOrigin(0, 0)
	if err := gu.scrollViewByLines(winDetail, 9999); err != nil {
		t.Fatalf("scrollViewByLines: %v", err)
	}
	_, oy = v.Origin()
	if oy != want {
		t.Errorf("scrollViewByLines(9999) origin oy=%d want %d", oy, want)
	}

	// The most important post-condition: the last line of the
	// buffer must actually be inside the viewport's visible
	// region [oy, oy+innerH).
	if oy+innerH < lineCount {
		t.Errorf("after G, viewport bottom %d does not cover last line %d (innerH=%d)", oy+innerH, lineCount, innerH)
	}
}
