package datasource

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"autosk/internal/daemon/rpcclient"
)

// streamDaemon is a persistent line-delimited JSON-RPC server for session.subscribe:
// it answers the subscribe with an ack, writes a tail of `session-event`
// notifications, then keeps the connection open (mirroring autoskd, which never
// EOFs the tail on `done`). Returns an RPC datasource wired to it and a `gone`
// channel that closes when a served connection's read loop exits — i.e. when the
// client releases its end (the self-reap observable: the daemon never EOFs the
// tail, so the server only sees EOF when the client closes the connection).
func streamDaemon(t *testing.T, frames []map[string]any) (*RPC, <-chan struct{}) {
	t.Helper()
	// A short dir (not t.TempDir(), whose path embeds the long test name) so the
	// socket path stays under the macOS 104-byte sun_path limit.
	dir, err := os.MkdirTemp("", "as")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	gone := make(chan struct{})
	var goneOnce sync.Once
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				defer goneOnce.Do(func() { close(gone) })
				r := bufio.NewReader(c)
				line, err := r.ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct {
					ID     uint64 `json:"id"`
					Method string `json:"method"`
				}
				_ = json.Unmarshal(line, &req)
				enc := json.NewEncoder(c)
				_ = enc.Encode(map[string]any{"id": req.ID, "result": map[string]any{"ok": true}})
				// RPC.Watch multiplexes task/project/session lifecycle subscriptions
				// on one connection and waits for every acknowledgement before it
				// exposes the stream. SessionSubscribe still sends only one request.
				if req.Method == "task.subscribe" {
					for range 2 {
						line, err := r.ReadBytes('\n')
						if err != nil {
							return
						}
						var additional struct {
							ID uint64 `json:"id"`
						}
						_ = json.Unmarshal(line, &additional)
						_ = enc.Encode(map[string]any{"id": additional.ID, "result": map[string]any{"ok": true}})
					}
				}
				for _, f := range frames {
					_ = enc.Encode(f)
				}
				// Hold the connection open (do not EOF after `done`): the client
				// must tear down on the terminal frame itself.
				for {
					if _, err := r.ReadBytes('\n'); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	cli, err := rpcclient.New(rpcclient.Options{Sock: sock, Cwd: "/repo", NoAutoSpawn: true})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return NewRPC(cli), gone
}

func notifyFrame(kind string, payload map[string]any) map[string]any {
	params := map[string]any{"kind": kind, "session_id": "session-1"}
	for k, v := range payload {
		params[k] = v
	}
	return map[string]any{"method": "session-event", "params": params}
}

// TestStreamSession_MapsAndTearsDownOnDone asserts StreamSession maps each session-event
// frame to the right LiveEvent AND that the handle's channel CLOSES on the
// terminal `done` frame (the daemon keeps the socket open, so without the
// close-on-done teardown the channel would never close — a goroutine + fd leak).
func TestStreamSession_MapsAndTearsDownOnDone(t *testing.T) {
	ds, _ := streamDaemon(t, []map[string]any{
		notifyFrame("message", map[string]any{
			"line": 7, "event": map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "content": "hello"}}}),
		notifyFrame("status", map[string]any{
			"session": map[string]any{"id": "session-1", "status": "running"}}),
		notifyFrame("done", map[string]any{
			"session": map[string]any{"id": "session-1", "status": "done"}}),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	h, err := ds.StreamSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("StreamSession: %v", err)
	}
	defer h.Close()

	var got []LiveEvent
	closed := false
	for !closed {
		select {
		case ev, ok := <-h.Events:
			if !ok {
				closed = true
				break
			}
			got = append(got, ev)
		case <-time.After(2 * time.Second):
			t.Fatal("StreamSession did not tear down on `done` (channel never closed) — leak")
		}
	}

	if len(got) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(got), got)
	}
	if got[0].Kind != "message" || got[0].LineNum != 7 {
		t.Errorf("event 0 = %+v, want message/7", got[0])
	}
	if got[1].Kind != "status" {
		t.Errorf("event 1 = %+v, want status", got[1])
	}
	if got[2].Kind != "done" {
		t.Errorf("event 2 = %+v, want done", got[2])
	}
}

// TestStreamSession_ToleratesPartialFrame asserts that an ephemeral `partial`
// session-event frame (Phase 1: not rendered in the lazy TUI yet) is decoded
// without error and IGNORED by the adapter — it must not break tailing: the
// message before it and the terminal `done` after it still flow through, and the
// channel closes on `done`.
func TestStreamSession_ToleratesPartialFrame(t *testing.T) {
	ds, _ := streamDaemon(t, []map[string]any{
		notifyFrame("message", map[string]any{
			"line": 3, "event": map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "content": "hi"}}}),
		// An unknown-to-render partial snapshot lands mid-stream (no line).
		notifyFrame("partial", map[string]any{
			"partial": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "LIVE"}}}}),
		notifyFrame("done", map[string]any{
			"session": map[string]any{"id": "session-1", "status": "done"}}),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	h, err := ds.StreamSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("StreamSession: %v", err)
	}
	defer h.Close()

	var got []LiveEvent
	closed := false
	for !closed {
		select {
		case ev, ok := <-h.Events:
			if !ok {
				closed = true
				break
			}
			got = append(got, ev)
		case <-time.After(2 * time.Second):
			t.Fatal("a partial frame broke tailing (channel never closed) — leak")
		}
	}

	// The partial is dropped: only the message + done survive, in order.
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (partial must be ignored): %+v", len(got), got)
	}
	if got[0].Kind != "message" || got[0].LineNum != 3 {
		t.Errorf("event 0 = %+v, want message/3", got[0])
	}
	if got[1].Kind != "done" {
		t.Errorf("event 1 = %+v, want done", got[1])
	}
}

// TestStreamSession_ReleasesConnOnDone asserts that the terminal `done` frame does
// not just close the LiveEvent channel but actually RELEASES the underlying
// connection: the streamDaemon keeps its socket open, so the server only sees
// the client drop its end if StreamSession's close-on-done teardown ran
// (StreamSession → SessionStream.Close → conn.Close). The context is long-lived
// (WithCancel, never expires) so the teardown is driven solely by the terminal
// frame, not by ctx expiry — without the close-on-done the connection +
// goroutines leak past `done`.
func TestStreamSession_ReleasesConnOnDone(t *testing.T) {
	ds, gone := streamDaemon(t, []map[string]any{
		notifyFrame("status", map[string]any{
			"session": map[string]any{"id": "session-1", "status": "running"}}),
		notifyFrame("done", map[string]any{
			"session": map[string]any{"id": "session-1", "status": "done"}}),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := ds.StreamSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("StreamSession: %v", err)
	}
	defer h.Close()

	// Drain until the channel closes on `done`.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-h.Events:
			if ok {
				continue
			}
		case <-deadline:
			t.Fatal("StreamSession did not close the channel on `done`")
		}
		break
	}
	// The connection must have been released by the close-on-done teardown.
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("StreamSession did not release the connection on `done` (leak)")
	}
}

// TestStreamSession_ErrorFrame asserts a session-event error frame maps to a LiveEvent
// with Kind=="error" and a non-nil Err.
func TestStreamSession_ErrorFrame(t *testing.T) {
	ds, _ := streamDaemon(t, []map[string]any{
		notifyFrame("error", map[string]any{"error": "boom"}),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	h, err := ds.StreamSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("StreamSession: %v", err)
	}
	defer h.Close()

	select {
	case ev, ok := <-h.Events:
		if !ok {
			t.Fatal("channel closed before the error frame")
		}
		if ev.Kind != "error" || ev.Err == nil || ev.Err.Error() != "boom" {
			t.Errorf("event = %+v, want error/boom", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the error frame")
	}
}
