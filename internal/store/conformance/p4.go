package conformance

import (
	"context"
	"testing"

	"autosk/internal/store"
)

func setStatus(t *testing.T, s store.Store, id string, st store.Status) {
	t.Helper()
	if _, err := s.UpdateTask(context.Background(), id, store.TaskPatch{Status: &st}); err != nil {
		t.Fatalf("UpdateTask(%s, %s): %v", id, st, err)
	}
}

func testBlockBasics(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	a := mustCreate(t, s, "a", 1)
	b := mustCreate(t, s, "b", 1)
	if err := s.Block(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("Block: %v", err)
	}
	inc, out, err := s.Deps(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inc) != 1 || inc[0] != a.ID {
		t.Fatalf("want incoming=[a], got %v", inc)
	}
	if len(out) != 0 {
		t.Fatalf("want outgoing=[], got %v", out)
	}

	incA, outA, err := s.Deps(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(outA) != 1 || outA[0] != b.ID {
		t.Fatalf("want a.outgoing=[b], got %v", outA)
	}
	if len(incA) != 0 {
		t.Fatalf("want a.incoming=[], got %v", incA)
	}
}

func testBlockRejectsSelfBlock(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	a := mustCreate(t, s, "a", 1)
	err := s.Block(context.Background(), a.ID, a.ID)
	AssertErrIs(t, err, store.ErrSelfBlock)
}

func testBlockRejectsDirectCycle(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	a := mustCreate(t, s, "a", 1)
	b := mustCreate(t, s, "b", 1)
	if err := s.Block(ctx, b.ID, a.ID); err != nil { // a blocks b
		t.Fatal(err)
	}
	err := s.Block(ctx, a.ID, b.ID) // b blocks a → cycle
	AssertErrIs(t, err, store.ErrCycle)
}

func testBlockRejectsTransitiveCycle(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	a := mustCreate(t, s, "a", 1)
	b := mustCreate(t, s, "b", 1)
	c := mustCreate(t, s, "c", 1)
	// a→b, b→c
	if err := s.Block(ctx, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Block(ctx, c.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	// c→a would close a→b→c→a
	err := s.Block(ctx, a.ID, c.ID)
	AssertErrIs(t, err, store.ErrCycle)
}

func testBlockIdempotent(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	a := mustCreate(t, s, "a", 1)
	b := mustCreate(t, s, "b", 1)
	if err := s.Block(ctx, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Block(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("re-adding existing edge should be no-op, got: %v", err)
	}
	inc, _, _ := s.Deps(ctx, b.ID)
	if len(inc) != 1 {
		t.Fatalf("expected exactly one edge, got %v", inc)
	}
}

func testUnblockSpecificEdge(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	a := mustCreate(t, s, "a", 1)
	b := mustCreate(t, s, "b", 1)
	c := mustCreate(t, s, "c", 1)
	if err := s.Block(ctx, c.ID, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Unblock(ctx, c.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	inc, _, _ := s.Deps(ctx, c.ID)
	if len(inc) != 1 || inc[0] != b.ID {
		t.Fatalf("want incoming=[b] after unblocking a, got %v", inc)
	}
}

func testUnblockAllRemovesIncoming(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	a := mustCreate(t, s, "a", 1)
	b := mustCreate(t, s, "b", 1)
	c := mustCreate(t, s, "c", 1)
	x := mustCreate(t, s, "x", 1)
	// a→x, b→x, c→x
	if err := s.Block(ctx, x.ID, a.ID, b.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	// x also outgoing-blocks nothing, but let's give x→a... wait that's cycle. Use new task y.
	y := mustCreate(t, s, "y", 1)
	if err := s.Block(ctx, y.ID, x.ID); err != nil { // x→y outgoing for x
		t.Fatal(err)
	}
	n, err := s.UnblockAll(ctx, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("want 3 edges removed, got %d", n)
	}
	inc, out, _ := s.Deps(ctx, x.ID)
	if len(inc) != 0 {
		t.Fatalf("incoming should be empty after UnblockAll, got %v", inc)
	}
	if len(out) != 1 {
		t.Fatalf("outgoing should be untouched by UnblockAll, got %v", out)
	}
}

func testReadyExcludesBlockedByOpen(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	a := mustCreate(t, s, "a", 1)
	b := mustCreate(t, s, "b", 1)
	if err := s.Block(ctx, b.ID, a.ID); err != nil { // a blocks b
		t.Fatal(err)
	}
	got, err := s.Ready(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("ready should be [a] only; got %v", taskIDs(got))
	}
	// Parking a for human input is still non-terminal, so b stays blocked.
	setStatus(t, s, a.ID, store.StatusHuman)
	got, err = s.Ready(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ready should be empty while a is human; got %v", taskIDs(got))
	}

	// Now close a → b becomes ready.
	setStatus(t, s, a.ID, store.StatusDone)
	got, err = s.Ready(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("after closing a, ready should be [b]; got %v", taskIDs(got))
	}
}

func testCancelledBlockerDoesNotBlock(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	a := mustCreate(t, s, "a", 1)
	b := mustCreate(t, s, "b", 1)
	if err := s.Block(ctx, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	setStatus(t, s, a.ID, store.StatusCancel)
	got, err := s.Ready(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	// b should be ready since its only blocker is cancelled (terminal).
	foundB := false
	for _, g := range got {
		if g.ID == b.ID {
			foundB = true
		}
	}
	if !foundB {
		t.Fatalf("b should be ready when its only blocker is cancelled; got %v", taskIDs(got))
	}
}

func testIsBlocked(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	a := mustCreate(t, s, "a", 1)
	b := mustCreate(t, s, "b", 1)
	if err := s.Block(ctx, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	// b has a as blocker (status=new) → blocked
	blocked, err := s.IsBlocked(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Fatal("want b blocked while a is new")
	}
	// a has no incoming → not blocked
	blocked, err = s.IsBlocked(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Fatal("a should not be blocked")
	}
	// Human is still non-terminal, so b remains blocked.
	setStatus(t, s, a.ID, store.StatusHuman)
	blocked, err = s.IsBlocked(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Fatal("want b blocked while a is human")
	}
	// Closing a flips b to not blocked, but edge remains.
	setStatus(t, s, a.ID, store.StatusDone)
	blocked, err = s.IsBlocked(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Fatal("want b unblocked after a done")
	}
	// Edge is historical: Deps still lists a.
	inc, _, _ := s.Deps(ctx, b.ID)
	if len(inc) != 1 || inc[0] != a.ID {
		t.Fatalf("edge should remain after blocker closes; got %v", inc)
	}
}
