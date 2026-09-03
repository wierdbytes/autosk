package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/rpcclient"
)

type fakeWatchStream struct {
	events chan rpcclient.Notification
	errs   chan error
}

func (s *fakeWatchStream) Events() <-chan rpcclient.Notification { return s.events }
func (s *fakeWatchStream) Errors() <-chan error                  { return s.errs }
func (s *fakeWatchStream) Close() error                          { return nil }

func TestWatchCommandRegisteredWithHelpAndCompletion(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"watch"})
	if err != nil {
		t.Fatalf("find watch: %v", err)
	}
	if cmd.ValidArgsFunction == nil {
		t.Fatal("watch has no task-id completion function")
	}
	if cmd.Flags().Lookup("no-follow") == nil {
		t.Fatal("watch help is missing --no-follow")
	}
	if root.PersistentFlags().Lookup("json") == nil {
		t.Fatal("root help is missing --json")
	}
}

func TestWatchNoFollowIntegration(t *testing.T) {
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	created, err := runRoot(t, dir, "create", "Watch integration", "--json")
	if err != nil {
		t.Fatalf("create: %v\n%s", err, created)
	}
	var task rpcclient.Task
	if err := json.Unmarshal([]byte(created), &task); err != nil {
		t.Fatalf("decode created task: %v\n%s", err, created)
	}
	out, err := runRoot(t, dir, "watch", task.ID, "--no-follow", "--json")
	if err != nil {
		t.Fatalf("watch --no-follow: %v\n%s", err, out)
	}
	events := decodeWatchJSONL(t, out)
	if len(events) != 1 || events[0]["type"] != watchSnapshot || events[0]["task_id"] != task.ID {
		t.Fatalf("watch events = %#v", events)
	}
	completion, err := runRoot(t, dir, "__complete", "watch", "")
	if err != nil {
		t.Fatalf("watch completion: %v\n%s", err, completion)
	}
	if !strings.Contains(completion, task.ID+"\tWatch integration") {
		t.Fatalf("watch completion does not suggest the task:\n%s", completion)
	}
}

func TestLiveWatchJSONTimeline(t *testing.T) {
	start := mustWatchTime(t, "2026-08-18T10:00:00Z")
	stream := &fakeWatchStream{events: make(chan rpcclient.Notification, 8), errs: make(chan error, 1)}
	initial := watchTask("ask-one", api.StatusWork, "feature-dev", "dev", start, map[string]any{
		"step_visits": map[string]any{"dev": float64(1)},
	})
	dev := rpcclient.Session{
		ID: "se-dev", Kind: api.SessionTask, TaskID: initial.ID, Workflow: initial.Workflow,
		Step: "dev", Status: api.SessionRunning, StartedAt: watchTimePtr(start),
	}

	reviewAt := start.Add(5 * time.Minute)
	review := watchTask(initial.ID, api.StatusWork, initial.Workflow, "review", reviewAt, map[string]any{
		"step_visits": map[string]any{"dev": float64(1), "review": float64(1)},
	})
	stream.events <- taskNote(review)
	dev.Status = api.SessionDone
	dev.EndedAt = watchTimePtr(reviewAt)
	stream.events <- sessionNote(dev)
	reviewSession := rpcclient.Session{
		ID: "se-review", Kind: api.SessionTask, TaskID: initial.ID, Workflow: initial.Workflow,
		Step: "review", Status: api.SessionRunning, StartedAt: watchTimePtr(reviewAt),
	}
	stream.events <- sessionNote(reviewSession)

	blockedAt := reviewAt.Add(time.Minute)
	blocked := review
	blocked.UpdatedAt = blockedAt
	blocked.Blocked = true
	blocked.BlockedBy = []api.TaskRef{{ID: "ask-blocker", Status: api.StatusNew}}
	stream.events <- taskNote(blocked)

	doneAt := reviewAt.Add(5 * time.Minute)
	done := blocked
	done.Status = api.StatusDone
	done.Step = ""
	done.UpdatedAt = doneAt
	stream.events <- taskNote(done)
	reviewSession.Status = api.SessionDone
	reviewSession.EndedAt = watchTimePtr(doneAt)
	stream.events <- sessionNote(reviewSession)

	var out bytes.Buffer
	err := runLiveWatchStream(context.Background(), initial, []rpcclient.Session{devWithStatus(dev, api.SessionRunning, nil)}, stream, liveWatchOptions{
		follow: true,
		json:   true,
		out:    &out,
		now:    func() time.Time { return start.Add(20 * time.Minute) },
	})
	if err != nil {
		t.Fatalf("runLiveWatchStream: %v", err)
	}

	events := decodeWatchJSONL(t, out.String())
	wantTypes := []string{
		watchSnapshot, watchStepChange, watchSessionFinish, watchSessionStart,
		watchBlockedChange, watchStatusChange, watchStepChange, watchSessionFinish,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("event count = %d, want %d\n%s", len(events), len(wantTypes), out.String())
	}
	for i, want := range wantTypes {
		if got := events[i]["type"]; got != want {
			t.Errorf("event %d type = %v, want %s", i, got, want)
		}
		if events[i]["task_id"] != initial.ID {
			t.Errorf("event %d task_id = %v", i, events[i]["task_id"])
		}
		if _, err := time.Parse(time.RFC3339Nano, events[i]["timestamp"].(string)); err != nil {
			t.Errorf("event %d timestamp is not RFC3339: %v", i, err)
		}
	}
	if got := events[1]["duration_seconds"]; got != float64(300) {
		t.Errorf("dev duration = %v, want 300", got)
	}
	if got := events[4]["blocked_by"].([]any); len(got) != 1 || got[0] != "ask-blocker" {
		t.Errorf("blocked_by = %#v", got)
	}
	if events[7]["input_tokens"] != nil || events[7]["total_tokens"] != nil {
		t.Errorf("tokens should be explicitly unavailable: %#v", events[7])
	}
}

func TestLiveWatchDetectsStepSelfLoopFromVisitCount(t *testing.T) {
	start := mustWatchTime(t, "2026-08-18T10:00:00Z")
	initial := watchTask("ask-retry", api.StatusWork, "wf", "dev", start, map[string]any{
		"step_visits": map[string]any{"dev": float64(1)},
	})
	state := newLiveWatchState(initial, []rpcclient.Session{{
		ID: "se-one", TaskID: initial.ID, Step: "dev", Status: api.SessionRunning, StartedAt: watchTimePtr(start),
	}})
	next := initial
	next.UpdatedAt = start.Add(3 * time.Minute)
	next.Metadata = map[string]any{"step_visits": map[string]any{"dev": float64(2)}}
	events := state.apply(taskNote(next), next.UpdatedAt)
	if len(events) != 1 || events[0].typeName != watchStepChange {
		t.Fatalf("events = %+v, want one step_change", events)
	}
	if events[0].previousStep == nil || events[0].step == nil || *events[0].previousStep != "dev" || *events[0].step != "dev" {
		t.Fatalf("self-loop steps = %v -> %v", events[0].previousStep, events[0].step)
	}
	if events[0].durationSeconds == nil || *events[0].durationSeconds != 180 {
		t.Fatalf("self-loop duration = %v, want 180", events[0].durationSeconds)
	}
}

func TestLiveWatchHumanRendering(t *testing.T) {
	originalLocal := time.Local
	time.Local = time.FixedZone("CEST", 2*60*60)
	t.Cleanup(func() { time.Local = originalLocal })
	at := mustWatchTime(t, "2026-08-18T20:11:08Z")
	now := mustWatchTime(t, "2026-08-18T20:20:00Z")
	statusDone := api.StatusDone
	statusWork := api.StatusWork
	tokensIn, tokensOut, tokensTotal := int64(12345), int64(678), int64(13023)
	events := []watchEvent{
		{typeName: watchStatusChange, taskID: "ask-one", timestamp: at, previousStatus: &statusWork, status: &statusDone},
		{typeName: watchSessionFinish, taskID: "ask-one", timestamp: at, sessionID: "se-one", outcome: api.SessionDone,
			inputTokens: &tokensIn, outputTokens: &tokensOut, totalTokens: &tokensTotal},
	}
	var out bytes.Buffer
	opts := liveWatchOptions{out: &out, now: func() time.Time { return now }}
	for _, event := range events {
		if err := emitWatchEvent(opts, event); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	got := out.String()
	for _, want := range []string{
		"22:11:08  status   work -> done",
		"se-one finished: done (tokens: input 12,345, output 678, total 13,023)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestLiveWatchReturnsStreamError(t *testing.T) {
	start := mustWatchTime(t, "2026-08-18T10:00:00Z")
	stream := &fakeWatchStream{events: make(chan rpcclient.Notification), errs: make(chan error, 1)}
	stream.errs <- errors.New("socket disappeared")
	close(stream.errs)
	close(stream.events)
	var out bytes.Buffer
	err := runLiveWatchStream(context.Background(), watchTask("ask-one", api.StatusWork, "wf", "dev", start, nil), nil, stream, liveWatchOptions{
		follow: true, out: &out, now: func() time.Time { return start },
	})
	if err == nil || !strings.Contains(err.Error(), "socket disappeared") {
		t.Fatalf("error = %v", err)
	}
}

func TestLiveWatchContextCancellationIsClean(t *testing.T) {
	start := mustWatchTime(t, "2026-08-18T10:00:00Z")
	stream := &fakeWatchStream{events: make(chan rpcclient.Notification), errs: make(chan error)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if err := runLiveWatchStream(ctx, watchTask("ask-one", api.StatusWork, "wf", "dev", start, nil), nil, stream, liveWatchOptions{
		follow: true, out: &out, now: func() time.Time { return start },
	}); err != nil {
		t.Fatalf("cancelled watch returned %v", err)
	}
}

func TestWatchFormattingHelpers(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{65 * time.Second, "1m 5s"},
		{25*time.Hour + 2*time.Minute, "1d 1h 2m"},
	} {
		if got := formatWatchDuration(tc.in); got != tc.want {
			t.Errorf("formatWatchDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := formatCount(18420); got != "18,420" {
		t.Errorf("formatCount = %q", got)
	}
}

func watchTask(id string, status api.TaskStatus, workflow, step string, updatedAt time.Time, metadata map[string]any) rpcclient.Task {
	return rpcclient.Task{
		ID: id, Title: "Watch me", Status: status, Workflow: workflow, Step: step,
		UpdatedAt: updatedAt, Metadata: metadata,
	}
}

func taskNote(task rpcclient.Task) rpcclient.Notification {
	data, _ := json.Marshal(api.TaskChangedParams{Root: "/repo", Task: task})
	return rpcclient.Notification{Method: "task-changed", Params: data}
}

func sessionNote(session rpcclient.Session) rpcclient.Notification {
	data, _ := json.Marshal(api.SessionChangedParams{Root: "/repo", Session: session})
	return rpcclient.Notification{Method: "session-changed", Params: data}
}

func decodeWatchJSONL(t *testing.T, raw string) []map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	var out []map[string]any
	for dec.More() {
		var event map[string]any
		if err := dec.Decode(&event); err != nil {
			t.Fatalf("decode JSONL: %v\n%s", err, raw)
		}
		out = append(out, event)
	}
	return out
}

func mustWatchTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func watchTimePtr(value time.Time) *time.Time {
	copy := value
	return &copy
}

func devWithStatus(session rpcclient.Session, status api.SessionStatus, endedAt *time.Time) rpcclient.Session {
	session.Status = status
	session.EndedAt = endedAt
	return session
}
