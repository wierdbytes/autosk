package rpcclient

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func noteFrame(method string, params map[string]any) map[string]any {
	return map[string]any{"method": method, "params": params}
}

// TestSubscribe_ForwardsNotifications asserts the readLoop forwards
// `task-changed` / `project-changed` notifications onto the channel in order
// (ignoring the subscribe ack) and that Close() is idempotent and emits
// task.unsubscribe.
func TestSubscribe_ForwardsNotifications(t *testing.T) {
	srv := newStreamServer(t, func(enc *json.Encoder, subID uint64) {
		// Acknowledge task/project/session subscriptions. Notifications arriving
		// during this handshake must be buffered and forwarded in order.
		_ = enc.Encode(map[string]any{"id": subID, "result": map[string]any{"subscribed": true}})
		_ = enc.Encode(noteFrame("task-changed", map[string]any{
			"root": "/repo", "task": map[string]any{"id": "ask-1"}}))
		_ = enc.Encode(map[string]any{"id": subID + 1, "result": map[string]any{"subscribed": true}})
		_ = enc.Encode(noteFrame("project-changed", map[string]any{
			"project": map[string]any{"root": "/repo", "name": "repo"}}))
		_ = enc.Encode(map[string]any{"id": subID + 2, "result": map[string]any{"subscribed": true}})
		_ = enc.Encode(noteFrame("task-changed", map[string]any{
			"root": "/repo", "task": map[string]any{"id": "ask-2"}}))
	})
	cli := mustClient(t, srv.sock)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := cli.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	want := []string{"task-changed", "project-changed", "task-changed"}
	for i, wm := range want {
		select {
		case n := <-stream.Events():
			if n.Method != wm {
				t.Fatalf("note %d method = %q, want %q", i, n.Method, wm)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for notification %d (%s)", i, wm)
		}
	}

	// Close is idempotent and must emit task.unsubscribe to the server.
	if err := stream.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return srv.sawMethod("task.unsubscribe") },
		"server never received task.unsubscribe after Close")
}

// TestSubscribe_ReturnsHandshakeError asserts a failed subscription is returned
// synchronously and the underlying connection is released.
func TestSubscribe_ReturnsHandshakeError(t *testing.T) {
	srv := newStreamServer(t, func(enc *json.Encoder, subID uint64) {
		_ = enc.Encode(map[string]any{"id": subID, "error": map[string]any{
			"code": CodeMethodNotFound, "message": "unknown method: task.subscribe"}})
	})
	cli := mustClient(t, srv.sock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := cli.Subscribe(ctx); err == nil {
		t.Fatal("Subscribe succeeded, want handshake error")
	}
	select {
	case <-srv.gone:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not release the connection after a subscribe error")
	}
}

func TestSubscribeTaskProgress_UsesExistingChannels(t *testing.T) {
	srv := newStreamServer(t, func(enc *json.Encoder, subID uint64) {
		_ = enc.Encode(map[string]any{"id": subID, "result": map[string]any{"ok": true}})
		_ = enc.Encode(noteFrame("task-changed", map[string]any{
			"root": "/repo", "task": map[string]any{"id": "ask-1"}}))
		_ = enc.Encode(map[string]any{"id": subID + 1, "result": map[string]any{"ok": true}})
		_ = enc.Encode(noteFrame("session-changed", map[string]any{
			"root": "/repo", "session": map[string]any{"id": "se-1", "task_id": "ask-1"}}))
	})
	cli := mustClient(t, srv.sock)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := cli.SubscribeTaskProgress(ctx)
	if err != nil {
		t.Fatalf("SubscribeTaskProgress: %v", err)
	}
	for i, want := range []string{"task-changed", "session-changed"} {
		select {
		case note := <-stream.Events():
			if note.Method != want {
				t.Fatalf("event %d method = %q, want %q", i, note.Method, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}
	_ = stream.Close()
	waitFor(t, 2*time.Second, func() bool {
		return srv.sawMethod("task.unsubscribe") && srv.sawMethod("session.unsubscribeProject")
	}, "progress stream did not send both unsubscribe requests")
	if srv.sawMethod("project.subscribe") {
		t.Fatal("progress stream unexpectedly subscribed to project changes")
	}
}
