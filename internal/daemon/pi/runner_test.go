package pi_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"autosk/internal/daemon/pi"
)

// fakepiBin is built once for the test binary lifetime.
var (
	fakepiOnce sync.Once
	fakepiPath string
	fakepiErr  error
)

func buildFakepi(t *testing.T) string {
	t.Helper()
	fakepiOnce.Do(func() {
		dir, err := os.MkdirTemp("", "autosk-fakepi-")
		if err != nil {
			fakepiErr = err
			return
		}
		bin := filepath.Join(dir, "fakepi")
		// Build the fakepi binary so we can exec it in tests.
		cmd := exec.Command("go", "build", "-o", bin, "./fakepi")
		// Run inside this package's directory.
		cmd.Dir = "."
		out, err := cmd.CombinedOutput()
		if err != nil {
			fakepiErr = err
			t.Logf("go build fakepi: %s", out)
			return
		}
		fakepiPath = bin
	})
	if fakepiErr != nil {
		t.Skipf("cannot build fakepi: %v", fakepiErr)
	}
	return fakepiPath
}

func newRunner(t *testing.T, extraEnv ...string) *pi.Runner {
	t.Helper()
	bin := buildFakepi(t)
	env := append(os.Environ(), extraEnv...)
	r, err := pi.Spawn(context.Background(), pi.Opts{
		PIBin: bin,
		Env:   env,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() {
		_ = r.CloseStdin()
		_, _ = r.Wait(context.Background(), 2*time.Second)
	})
	return r
}

func TestRunner_GetState(t *testing.T) {
	r := newRunner(t, "FAKEPI_SESSION_ID=abc", "FAKEPI_SESSION_FILE=/tmp/x.jsonl")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := r.GetState(ctx)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if info.SessionID != "abc" || info.SessionFile != "/tmp/x.jsonl" {
		t.Fatalf("session info: %+v", info)
	}
}

func TestRunner_PromptThenAgentEnd(t *testing.T) {
	r := newRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.SendPrompt(ctx, "hello"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if err := r.WaitForAgentEnd(ctx); err != nil {
		t.Fatalf("WaitForAgentEnd: %v", err)
	}

	// Drain events to confirm we saw the expected kinds.
	timeout := time.After(500 * time.Millisecond)
	seen := map[pi.EventKind]bool{}
LOOP:
	for {
		select {
		case e, ok := <-r.Events():
			if !ok {
				break LOOP
			}
			seen[e.Kind] = true
			if seen[pi.KindAgentEnd] {
				// Once agent_end is seen we can stop reading.
				break LOOP
			}
		case <-timeout:
			break LOOP
		}
	}
	for _, want := range []pi.EventKind{
		pi.KindResponse, pi.KindAgentStart, pi.KindTurnStart,
		pi.KindMessageStart, pi.KindMessageEnd, pi.KindTurnEnd,
		pi.KindAgentEnd,
	} {
		if !seen[want] {
			t.Errorf("did not observe %q event", want)
		}
	}
}

func TestRunner_PromptError(t *testing.T) {
	r := newRunner(t, "FAKEPI_SCENARIO=prompt_error")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.SendPrompt(ctx, "x"); err == nil {
		t.Fatal("expected prompt error, got nil")
	}
}

func TestRunner_DialogIsAutoCancelled(t *testing.T) {
	// fakepi emits a `select` dialog mid-run; the runner should reply
	// {cancelled:true} and pi should still finish (we'd see agent_end).
	r := newRunner(t, "FAKEPI_SCENARIO=dialog")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.SendPrompt(ctx, "x"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if err := r.WaitForAgentEnd(ctx); err != nil {
		t.Fatalf("WaitForAgentEnd: %v", err)
	}
}

func TestRunner_CleanShutdownOnStdinClose(t *testing.T) {
	r := newRunner(t)
	if err := r.CloseStdin(); err != nil {
		t.Fatalf("CloseStdin: %v", err)
	}
	code, err := r.Wait(context.Background(), 3*time.Second)
	if pi.IsWaitTimeout(err) {
		t.Fatalf("child did not exit on stdin close")
	}
	if code != 0 {
		t.Errorf("exit code: %d", code)
	}
}

// TestRunner_LargePayloadDoesNotCrashReader exercises the reader on a
// JSON line significantly larger than bufio.Scanner's old 1 MiB cap.
// The historical reader (bufio.Scanner with a 1 MiB buffer) would
// fail this with bufio.ErrTooLong; the bufio.Reader.ReadBytes-based
// reader has no per-line cap and must stream the message through to
// the consumer before emitting agent_end.
func TestRunner_LargePayloadDoesNotCrashReader(t *testing.T) {
	const payloadBytes = 4 << 20 // 4 MiB
	r := newRunner(t, fmt.Sprintf("FAKEPI_HUGE_PAYLOAD_BYTES=%d", payloadBytes))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.SendPrompt(ctx, "big"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if err := r.WaitForAgentEnd(ctx); err != nil {
		t.Fatalf("WaitForAgentEnd: %v", err)
	}
}

// TestRunner_GarbageLineDoesNotWedgeReader pins the second contract the
// runner's read loop promises (see runner.go readLoop comment, reason 2):
// a single non-JSON line between two valid frames must surface as exactly
// one KindOther event and the reader must keep parsing subsequent lines.
//
// The old bufio.Scanner reader did this naturally; the round-1 json.Decoder
// reader did NOT (the decoder cursor wedges on the first non-JSON byte and
// silently drops every subsequent value); the round-2 bufio.Reader.ReadBytes
// reader restores the resync behaviour. Without this test the next refactor
// of the reader could silently re-regress reason 2 — the size-cap test
// would still pass.
func TestRunner_GarbageLineDoesNotWedgeReader(t *testing.T) {
	const garbage = "garbage-not-json"
	r := newRunner(t, "FAKEPI_INJECT_GARBAGE_LINE="+garbage)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.SendPrompt(ctx, "ok"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if err := r.WaitForAgentEnd(ctx); err != nil {
		t.Fatalf("WaitForAgentEnd: %v", err)
	}

	// Drain events to confirm the resync contract: the garbage line
	// surfaced as exactly one KindOther event AND the reader kept going
	// to deliver the post-garbage frames (turn_end, agent_end).
	deadline := time.After(500 * time.Millisecond)
	var (
		otherCount  int
		otherRaw    []byte
		sawTurnEnd  bool
		sawAgentEnd bool
	)
LOOP:
	for {
		select {
		case e, ok := <-r.Events():
			if !ok {
				break LOOP
			}
			switch e.Kind {
			case pi.KindOther:
				otherCount++
				otherRaw = append([]byte(nil), e.Raw...)
			case pi.KindTurnEnd:
				sawTurnEnd = true
			case pi.KindAgentEnd:
				sawAgentEnd = true
				break LOOP
			}
		case <-deadline:
			break LOOP
		}
	}
	if otherCount != 1 {
		t.Fatalf("want exactly 1 KindOther event, got %d (raw=%q)", otherCount, string(otherRaw))
	}
	if string(otherRaw) != garbage {
		t.Fatalf("KindOther.Raw = %q, want %q", string(otherRaw), garbage)
	}
	if !sawTurnEnd {
		t.Error("did not observe turn_end after the garbage line (reader wedged?)")
	}
	if !sawAgentEnd {
		t.Error("did not observe agent_end after the garbage line (reader wedged?)")
	}
}

func TestRunner_TerminateThenWait(t *testing.T) {
	// no_agent_end keeps pi alive: SIGTERM should bring it down.
	r := newRunner(t, "FAKEPI_SCENARIO=no_agent_end")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.SendPrompt(ctx, "x"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	// Make sure we never get agent_end here.
	wctx, wcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer wcancel()
	if err := r.WaitForAgentEnd(wctx); err == nil {
		t.Fatal("expected no agent_end")
	}
	if err := r.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	_, err := r.Wait(context.Background(), 3*time.Second)
	if pi.IsWaitTimeout(err) {
		_ = r.Kill()
		t.Fatal("child did not exit after SIGTERM")
	}
}
