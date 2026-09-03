package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/rpcclient"
	"autosk/internal/timeformat"
)

const (
	watchSnapshot      = "snapshot"
	watchStatusChange  = "status_change"
	watchStepChange    = "step_change"
	watchBlockedChange = "blocked_change"
	watchSessionStart  = "session_start"
	watchSessionFinish = "session_finish"
)

func newWatchCmd() *cobra.Command {
	var noFollow bool
	cmd := &cobra.Command{
		Use:   "watch <task-id>",
		Short: "Watch live progress for one task",
		Long: `Watch live progress for one task.

The command prints the current task state, then follows live status, workflow
step, blocker, and session lifecycle changes for that task. Human-readable
output uses local timestamps; --json emits one JSON object per line with
RFC3339 UTC timestamps.

The command exits successfully when the task reaches done or cancel and its
live sessions have finished. It also exits cleanly when interrupted.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeWatchTaskIDs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			cl, err := readClient(ctx)
			if err != nil {
				return err
			}
			return runLiveWatch(ctx, args[0], cl, liveWatchOptions{
				follow: !noFollow,
				json:   flagJSON,
				out:    cmd.OutOrStdout(),
				now:    time.Now,
			})
		},
	}
	cmd.Flags().BoolVar(&noFollow, "no-follow", false, "print the current state and exit")
	return cmd
}

type liveWatchOptions struct {
	follow        bool
	json          bool
	out           io.Writer
	now           func() time.Time
	stepStartedAt time.Time
}

type watchNotificationStream interface {
	Events() <-chan rpcclient.Notification
	Errors() <-chan error
	Close() error
}

func runLiveWatch(
	ctx context.Context,
	taskID string,
	cl *rpcclient.Client,
	opts liveWatchOptions,
) error {
	if opts.out == nil {
		return errors.New("watch: output is unavailable")
	}
	if opts.now == nil {
		opts.now = time.Now
	}

	if !opts.follow {
		task, sessions, err := watchSnapshotState(ctx, cl, taskID)
		if err != nil {
			return err
		}
		state := newLiveWatchState(task, sessions)
		return emitWatchEvent(opts, state.snapshot(opts.now().UTC()))
	}

	// Subscribe before reading the snapshot. SubscribeTaskProgress waits for
	// both daemon acknowledgements and buffers any notifications that race with
	// the task/session reads below.
	stream, err := cl.SubscribeTaskProgress(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("watch task %s: subscribe: %w", taskID, err)
	}
	defer stream.Close()

	task, sessions, err := watchSnapshotState(ctx, cl, taskID)
	if err != nil {
		return err
	}
	opts.stepStartedAt = currentStepStart(ctx, cl, task, sessions)
	return runLiveWatchStream(ctx, task, sessions, stream, opts)
}

func watchSnapshotState(
	ctx context.Context,
	cl *rpcclient.Client,
	taskID string,
) (rpcclient.Task, []rpcclient.Session, error) {
	task, err := cl.GetTask(ctx, taskID)
	if err != nil {
		if apiErr, ok := rpcclient.IsAPIError(err); ok && apiErr.Code == rpcclient.CodeNotFound {
			return rpcclient.Task{}, nil, errors.New("task not found: " + taskID)
		}
		return rpcclient.Task{}, nil, err
	}
	sessions, err := cl.Sessions(ctx, taskID)
	if err != nil {
		return rpcclient.Task{}, nil, err
	}
	return task, sessions, nil
}

// currentStepStart reads only the current live session's transcript header.
// Its durable timestamp is the queued/step-entry boundary and therefore covers
// attaching midway through a step. started_at is the fallback for an old or
// temporarily unreadable transcript.
func currentStepStart(
	ctx context.Context,
	cl *rpcclient.Client,
	task rpcclient.Task,
	sessions []rpcclient.Session,
) time.Time {
	var current *rpcclient.Session
	for i := range sessions {
		session := &sessions[i]
		if session.TaskID == task.ID && session.Step == task.Step && liveSession(session.Status) {
			current = session
		}
	}
	if current == nil {
		return time.Time{}
	}
	transcript, err := cl.Transcript(ctx, current.ID, 1, 1)
	if err == nil && len(transcript.Entries) == 1 {
		if started, parseErr := time.Parse(time.RFC3339Nano, transcript.Entries[0].Timestamp); parseErr == nil {
			return started
		}
	}
	if current.StartedAt != nil {
		return *current.StartedAt
	}
	return time.Time{}
}

func runLiveWatchStream(
	ctx context.Context,
	task rpcclient.Task,
	sessions []rpcclient.Session,
	stream watchNotificationStream,
	opts liveWatchOptions,
) error {
	if opts.out == nil {
		return errors.New("watch: output is unavailable")
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	state := newLiveWatchState(task, sessions)
	if !opts.stepStartedAt.IsZero() {
		state.stepStartedAt = opts.stepStartedAt
	}
	if err := emitWatchEvent(opts, state.snapshot(opts.now().UTC())); err != nil {
		return err
	}
	if state.complete() {
		return nil
	}

	events := stream.Events()
	errs := stream.Errors()
	for events != nil || errs != nil {
		select {
		case <-ctx.Done():
			return nil
		case note, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			for _, event := range state.apply(note, opts.now().UTC()) {
				if err := emitWatchEvent(opts, event); err != nil {
					return err
				}
			}
			if state.complete() {
				return nil
			}
		case streamErr, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if streamErr != nil && ctx.Err() == nil {
				return fmt.Errorf("watch task %s: %w", task.ID, streamErr)
			}
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("watch task %s: lost connection to autoskd", task.ID)
}

type liveWatchState struct {
	task          rpcclient.Task
	sessions      map[string]rpcclient.Session
	stepStartedAt time.Time
}

func newLiveWatchState(task rpcclient.Task, sessions []rpcclient.Session) *liveWatchState {
	s := &liveWatchState{task: task, sessions: make(map[string]rpcclient.Session, len(sessions))}
	for _, session := range sessions {
		s.sessions[session.ID] = session
		if session.TaskID != task.ID || session.Step != task.Step || session.StartedAt == nil || !liveSession(session.Status) {
			continue
		}
		if s.stepStartedAt.IsZero() || session.StartedAt.After(s.stepStartedAt) {
			s.stepStartedAt = *session.StartedAt
		}
	}
	return s
}

func (s *liveWatchState) snapshot(at time.Time) watchEvent {
	blocked := s.task.Blocked
	status := s.task.Status
	return watchEvent{
		typeName:       watchSnapshot,
		taskID:         s.task.ID,
		timestamp:      at,
		title:          s.task.Title,
		status:         &status,
		workflow:       stringPtrOrNil(s.task.Workflow),
		step:           stringPtrOrNil(s.task.Step),
		blocked:        &blocked,
		blockedBy:      blockerIDs(s.task.BlockedBy),
		liveSessionIDs: s.liveSessionIDs(),
	}
}

func (s *liveWatchState) apply(note rpcclient.Notification, receivedAt time.Time) []watchEvent {
	switch note.Method {
	case "task-changed":
		var params api.TaskChangedParams
		if json.Unmarshal(note.Params, &params) != nil || params.Task.ID != s.task.ID {
			return nil
		}
		return s.applyTask(params.Task, receivedAt)
	case "session-changed":
		var params api.SessionChangedParams
		if json.Unmarshal(note.Params, &params) != nil || params.Session.TaskID != s.task.ID {
			return nil
		}
		return s.applySession(params.Session, receivedAt)
	default:
		return nil
	}
}

func (s *liveWatchState) applyTask(next rpcclient.Task, receivedAt time.Time) []watchEvent {
	previous := s.task
	at := next.UpdatedAt
	if at.IsZero() {
		at = receivedAt
	}
	at = at.UTC()
	events := make([]watchEvent, 0, 3)
	if previous.Status != next.Status {
		from, to := previous.Status, next.Status
		events = append(events, watchEvent{
			typeName: watchStatusChange, taskID: next.ID, timestamp: at,
			previousStatus: &from, status: &to,
		})
	}

	stepChanged := previous.Step != next.Step || previous.Workflow != next.Workflow
	selfLoop := !stepChanged && next.Step != "" && stepVisitCount(next, next.Step) > stepVisitCount(previous, previous.Step)
	if stepChanged || selfLoop {
		duration := elapsedSeconds(s.stepStartedAt, at)
		events = append(events, watchEvent{
			typeName: watchStepChange, taskID: next.ID, timestamp: at,
			workflow: stringPtrOrNil(next.Workflow), previousStep: stringPtrOrNil(previous.Step),
			step: stringPtrOrNil(next.Step), durationSeconds: duration,
		})
		if next.Step == "" {
			s.stepStartedAt = time.Time{}
		} else {
			s.stepStartedAt = at
		}
	}
	if previous.Blocked != next.Blocked || !sameBlockers(previous.BlockedBy, next.BlockedBy) {
		from, to := previous.Blocked, next.Blocked
		events = append(events, watchEvent{
			typeName: watchBlockedChange, taskID: next.ID, timestamp: at,
			previousBlocked: &from, blocked: &to, blockedBy: blockerIDs(next.BlockedBy),
		})
	}
	s.task = next
	return events
}

func (s *liveWatchState) applySession(next rpcclient.Session, receivedAt time.Time) []watchEvent {
	previous, existed := s.sessions[next.ID]
	s.sessions[next.ID] = next
	events := make([]watchEvent, 0, 1)
	if next.Status == api.SessionRunning && (!existed || previous.Status != api.SessionRunning) {
		at := receivedAt
		if next.StartedAt != nil {
			at = *next.StartedAt
		}
		if s.stepStartedAt.IsZero() && next.Step == s.task.Step {
			s.stepStartedAt = at
		}
		events = append(events, watchEvent{
			typeName: watchSessionStart, taskID: s.task.ID, timestamp: at.UTC(),
			sessionID: next.ID, step: stringPtrOrNil(next.Step),
		})
	}
	if terminalSession(next.Status) && (!existed || !terminalSession(previous.Status)) {
		at := receivedAt
		if next.EndedAt != nil {
			at = *next.EndedAt
		}
		events = append(events, watchEvent{
			typeName: watchSessionFinish, taskID: s.task.ID, timestamp: at.UTC(),
			sessionID: next.ID, step: stringPtrOrNil(next.Step), outcome: next.Status,
			errorMessage: next.Error,
		})
	}
	return events
}

func (s *liveWatchState) complete() bool {
	return terminalTaskStatus(s.task.Status) && len(s.liveSessionIDs()) == 0
}

func (s *liveWatchState) liveSessionIDs() []string {
	ids := make([]string, 0)
	for id, session := range s.sessions {
		if session.TaskID == s.task.ID && liveSession(session.Status) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

type watchEvent struct {
	typeName        string
	taskID          string
	timestamp       time.Time
	title           string
	status          *api.TaskStatus
	previousStatus  *api.TaskStatus
	workflow        *string
	step            *string
	previousStep    *string
	blocked         *bool
	previousBlocked *bool
	blockedBy       []string
	liveSessionIDs  []string
	durationSeconds *float64
	sessionID       string
	outcome         api.SessionStatus
	errorMessage    string
	inputTokens     *int64
	outputTokens    *int64
	totalTokens     *int64
}

func emitWatchEvent(opts liveWatchOptions, event watchEvent) error {
	if opts.json {
		if err := json.NewEncoder(opts.out).Encode(event.jsonValue()); err != nil {
			return fmt.Errorf("watch: write JSON event: %w", err)
		}
		return nil
	}
	prefix := timeformat.FormatDateTimeSmartAt(event.timestamp, opts.now())
	switch event.typeName {
	case watchSnapshot:
		fields := []string{"status=" + string(*event.status)}
		if event.workflow != nil {
			fields = append(fields, "workflow="+*event.workflow)
		}
		if event.step != nil {
			fields = append(fields, "step="+*event.step)
		}
		if event.blocked != nil && *event.blocked {
			fields = append(fields, "blocked=true")
		}
		if len(event.liveSessionIDs) > 0 {
			fields = append(fields, "sessions="+strings.Join(event.liveSessionIDs, ","))
		}
		_, err := fmt.Fprintf(opts.out, "%s  %-8s %s %s\n", prefix, "snapshot", event.taskID, strings.Join(fields, " "))
		return err
	case watchStatusChange:
		_, err := fmt.Fprintf(opts.out, "%s  %-8s %s -> %s\n", prefix, "status", *event.previousStatus, *event.status)
		return err
	case watchStepChange:
		label := stepChangeLabel(event.previousStep, event.step)
		if event.previousStep != nil {
			if event.durationSeconds == nil {
				label += " (duration unavailable)"
			} else {
				label += " (" + formatWatchDuration(time.Duration(*event.durationSeconds*float64(time.Second))) + ")"
			}
		}
		_, err := fmt.Fprintf(opts.out, "%s  %-8s %s\n", prefix, "step", label)
		return err
	case watchBlockedChange:
		label := "unblocked"
		if event.blocked != nil && *event.blocked {
			label = "blocked"
			if len(event.blockedBy) > 0 {
				label += " by " + strings.Join(event.blockedBy, ", ")
			}
		}
		_, err := fmt.Fprintf(opts.out, "%s  %-8s %s\n", prefix, "blocked", label)
		return err
	case watchSessionStart:
		suffix := ""
		if event.step != nil {
			suffix = " (step " + *event.step + ")"
		}
		_, err := fmt.Fprintf(opts.out, "%s  %-8s %s started%s\n", prefix, "session", event.sessionID, suffix)
		return err
	case watchSessionFinish:
		details := []string{string(event.outcome)}
		if event.errorMessage != "" {
			details = append(details, oneLine(event.errorMessage))
		}
		if event.inputTokens == nil || event.outputTokens == nil || event.totalTokens == nil {
			details = append(details, "tokens unavailable")
		} else {
			details = append(details, fmt.Sprintf("tokens: input %s, output %s, total %s",
				formatCount(*event.inputTokens), formatCount(*event.outputTokens), formatCount(*event.totalTokens)))
		}
		_, err := fmt.Fprintf(opts.out, "%s  %-8s %s finished: %s (%s)\n",
			prefix, "session", event.sessionID, details[0], strings.Join(details[1:], "; "))
		return err
	default:
		return fmt.Errorf("watch: unsupported event type %q", event.typeName)
	}
}

func (e watchEvent) jsonValue() map[string]any {
	out := map[string]any{
		"type":      e.typeName,
		"task_id":   e.taskID,
		"timestamp": e.timestamp.UTC().Format(time.RFC3339Nano),
	}
	switch e.typeName {
	case watchSnapshot:
		out["title"] = e.title
		out["status"] = e.status
		out["workflow"] = e.workflow
		out["step"] = e.step
		out["blocked"] = e.blocked
		out["blocked_by"] = nonNilStrings(e.blockedBy)
		out["live_session_ids"] = nonNilStrings(e.liveSessionIDs)
	case watchStatusChange:
		out["previous_status"] = e.previousStatus
		out["status"] = e.status
	case watchStepChange:
		out["workflow"] = e.workflow
		out["previous_step"] = e.previousStep
		out["step"] = e.step
		out["duration_seconds"] = e.durationSeconds
	case watchBlockedChange:
		out["previous_blocked"] = e.previousBlocked
		out["blocked"] = e.blocked
		out["blocked_by"] = nonNilStrings(e.blockedBy)
	case watchSessionStart:
		out["session_id"] = e.sessionID
		out["step"] = e.step
	case watchSessionFinish:
		out["session_id"] = e.sessionID
		out["step"] = e.step
		out["outcome"] = e.outcome
		if e.errorMessage == "" {
			out["error"] = nil
		} else {
			out["error"] = e.errorMessage
		}
		out["input_tokens"] = e.inputTokens
		out["output_tokens"] = e.outputTokens
		out["total_tokens"] = e.totalTokens
	}
	return out
}

func stepChangeLabel(previous, next *string) string {
	switch {
	case previous == nil && next != nil:
		return *next + " started"
	case previous != nil && next == nil:
		return *previous + " completed"
	case previous != nil && next != nil:
		return *previous + " -> " + *next
	default:
		return "workflow position cleared"
	}
}

func stepVisitCount(task rpcclient.Task, step string) float64 {
	if step == "" || task.Metadata == nil {
		return 0
	}
	raw, ok := task.Metadata["step_visits"].(map[string]any)
	if !ok {
		return 0
	}
	value, _ := raw[step].(float64)
	return value
}

func elapsedSeconds(start, end time.Time) *float64 {
	if start.IsZero() || end.Before(start) {
		return nil
	}
	seconds := end.Sub(start).Seconds()
	return &seconds
}

func blockerIDs(refs []api.TaskRef) []string {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	sort.Strings(ids)
	return ids
}

func sameBlockers(a, b []api.TaskRef) bool {
	aIDs, bIDs := blockerIDs(a), blockerIDs(b)
	if len(aIDs) != len(bIDs) {
		return false
	}
	for i := range aIDs {
		if aIDs[i] != bIDs[i] {
			return false
		}
	}
	return true
}

func stringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func liveSession(status api.SessionStatus) bool {
	return status == api.SessionQueued || status == api.SessionRunning
}

func terminalSession(status api.SessionStatus) bool {
	return status == api.SessionDone || status == api.SessionFailed || status == api.SessionAborted
}

func terminalTaskStatus(status api.TaskStatus) bool {
	return status == api.StatusDone || status == api.StatusCancel
}

func completeWatchTaskIDs(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	cl, err := readClient(cmd.Context())
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	tasks, err := cl.Tasks(cmd.Context(), rpcclient.TaskListFilter{})
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	out := make([]string, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, task.ID+"\t"+oneLine(task.Title))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func formatWatchDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	duration = duration.Round(time.Second)
	days := duration / (24 * time.Hour)
	duration %= 24 * time.Hour
	hours := duration / time.Hour
	duration %= time.Hour
	minutes := duration / time.Minute
	seconds := (duration % time.Minute) / time.Second
	parts := make([]string, 0, 4)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}
	return strings.Join(parts, " ")
}

func formatCount(value int64) string {
	raw := strconv.FormatInt(value, 10)
	for i := len(raw) - 3; i > 0; i -= 3 {
		raw = raw[:i] + "," + raw[i:]
	}
	return raw
}
