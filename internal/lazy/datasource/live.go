package datasource

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"autosk/internal/daemon/client"
)

// Live overlays the daemon's HTTP/UDS API on top of an Offline base.
// All read verbs prefer the daemon's view (so Jobs carry the live
// Streaming / AttachCount values, and StreamLive opens an SSE
// subscription) but fall back to the embedded Offline for anything
// the daemon doesn't serve: DB-only entities (tasks, workflows,
// agents, comments, signals) and writes.
//
// Constructed by NewLive — never directly. The compose layer flips
// between Live and Offline based on a 2s health probe.
type Live struct {
	*Offline
	cli *client.Client

	// fallbacks counts how many times a daemon-preferred read fell
	// back to the offline base (because the daemon errored). The
	// status bar reads it via Fallbacks() so a daemon that's
	// technically reachable but flaky becomes visible.
	fallbacks atomic.Uint64
}

// NewLive wires a Live datasource on top of an Offline base.
func NewLive(base *Offline, cli *client.Client) *Live {
	return &Live{Offline: base, cli: cli}
}

// recordFallback bumps the counter and pushes a debug-log line so
// the (test-installed) dlog hook surfaces daemon hiccups during a
// run. Production callers see this only via Compose.Fallbacks().
func (l *Live) recordFallback(verb string, err error) {
	l.fallbacks.Add(1)
	if dbg := DebugLog(); dbg != nil {
		dbg("live %s fell back: %v", verb, err)
	}
}

// Fallbacks returns the cumulative count of daemon-read errors that
// fell back to the DB. Status-bar consumers (Compose.Fallbacks)
// compare against a previous reading to render a 'daemon flaky' chip
// when the counter advances. Note that the offline fallback is
// silent at the TUI layer (the operator still sees data — just
// stale-by-tick) so this counter is the only mechanism that surfaces
// the hiccup.
func (l *Live) Fallbacks() uint64 { return l.fallbacks.Load() }

// Jobs prefers the daemon's view so Streaming/AttachCount are live.
//
// Step labels (workflow:step:agent names) are batched through the
// offline layer's lookupStepLabels — one SQL per refresh tick, not
// 3*N. Without that batch this verb was a N+1 hot spot during every
// 2s refresh.
func (l *Live) Jobs(ctx context.Context, f JobFilter) ([]Job, error) {
	resp, err := l.cli.ListJobs(ctx, f.TaskID, f.Statuses, f.Limit)
	if err != nil {
		l.recordFallback("Jobs", err)
		return l.Offline.Jobs(ctx, f)
	}
	stepIDs := make([]string, 0, len(resp.Jobs))
	for _, j := range resp.Jobs {
		if j.StepID != "" {
			stepIDs = append(stepIDs, j.StepID)
		}
	}
	decor := l.Offline.lookupStepLabels(ctx, stepIDs)
	out := make([]Job, 0, len(resp.Jobs))
	for _, j := range resp.Jobs {
		dj := Job{JobResponse: j}
		if j.StepID != "" {
			d, ok := decor[j.StepID]
			if ok {
				dj.StepName = d.StepName
				dj.AgentName = d.AgentName
				dj.WorkflowName = d.WorkflowName
				if f.WorkflowID != "" && d.WorkflowID != f.WorkflowID {
					continue
				}
			} else if f.WorkflowID != "" {
				continue
			}
		} else if f.WorkflowID != "" {
			continue
		}
		out = append(out, dj)
	}
	return out, nil
}

// GetJob hits the daemon's GET /v1/jobs/{id} for the live shape.
// Workflow/step/agent labels come from the offline base (one query,
// not three) so the inspector header decoration is cheap.
func (l *Live) GetJob(ctx context.Context, id string) (Job, error) {
	resp, err := l.cli.GetJob(ctx, id)
	if err != nil {
		l.recordFallback("GetJob", err)
		return l.Offline.GetJob(ctx, id)
	}
	j := Job{JobResponse: resp}
	if resp.StepID != "" {
		if d, ok := l.Offline.lookupStepLabels(ctx, []string{resp.StepID})[resp.StepID]; ok {
			j.StepName = d.StepName
			j.AgentName = d.AgentName
			j.WorkflowName = d.WorkflowName
		}
	}
	return j, nil
}

// Messages prefers the daemon's /messages?full=true so the transcript
// includes events that haven't been flushed to disk yet.
func (l *Live) Messages(ctx context.Context, jobID string, full bool, limit int) ([]MessageEvent, error) {
	resp, err := l.cli.GetMessages(ctx, jobID, full, limit)
	if err != nil {
		l.recordFallback("Messages", err)
		return l.Offline.Messages(ctx, jobID, full, limit)
	}
	out := make([]MessageEvent, 0, len(resp.Events))
	out = append(out, resp.Events...)
	return out, nil
}

// Reconnect is a no-op for the Live branch: it has no local sql.DB to
// refresh — every call goes over the daemon's UDS HTTP surface. The
// implementation exists only to satisfy the Datasource interface;
// freshness for the composed datasource is driven by the offline
// base's underlying doltlite store.
func (l *Live) Reconnect(ctx context.Context) error { return nil }

// Healthz pings the daemon and translates the response.
func (l *Live) Healthz(ctx context.Context) (Health, error) {
	resp, err := l.cli.Healthz(ctx)
	if err != nil {
		return Health{Daemon: "down", UpdatedAt: time.Now().UTC()}, nil
	}
	state := "ok"
	if !resp.OK {
		state = "stale"
	}
	return Health{
		Daemon:    state,
		Workers:   resp.Workers,
		Queued:    resp.Queued,
		Running:   resp.Running,
		UpdatedAt: time.Now().UTC(),
	}, nil
}

// CancelJob asks the daemon to cancel a run.
func (l *Live) CancelJob(ctx context.Context, jobID string) error {
	_, err := l.cli.CancelJob(ctx, jobID)
	return err
}

// SendInput dispatches operator text to the daemon's pi runner.
func (l *Live) SendInput(ctx context.Context, jobID, message, behavior string) (string, error) {
	resp, err := l.cli.SendInput(ctx, jobID, message, behavior)
	if err != nil {
		return "", err
	}
	return resp.Dispatched, nil
}

// AbortJob asks pi to stop its in-flight turn.
func (l *Live) AbortJob(ctx context.Context, jobID string) error {
	_, err := l.cli.Abort(ctx, jobID)
	return err
}

// StreamLive opens an SSE subscription with attach=true.
func (l *Live) StreamLive(ctx context.Context, jobID string) (*LiveHandle, error) {
	h, err := l.cli.Stream(ctx, jobID, client.StreamOptions{Attach: true, Full: true})
	if err != nil {
		return nil, err
	}
	ch := make(chan LiveEvent, 32)
	go func() {
		defer close(ch)
		for ev := range h.Events {
			out := LiveEvent{EventID: ev.EventID}
			switch ev.Type {
			case client.EventTypeMessage:
				out.Kind = "message"
				if ev.Message != nil {
					out.Message = *ev.Message
				}
			case client.EventTypeStatus:
				out.Kind = "status"
				if ev.Status != nil {
					out.Status = *ev.Status
				}
			case client.EventTypeDone:
				out.Kind = "done"
				if ev.Status != nil {
					out.Status = *ev.Status
				}
			case client.EventTypeError:
				out.Kind = "error"
				out.Err = ev.Err
			default:
				continue
			}
			select {
			case ch <- out:
			case <-ctx.Done():
				return
			}
		}
	}()
	return NewLiveHandle(ch, func() error { h.Close(); return nil }), nil
}

// SyncWorkflows forwards to the offline base; the daemon has no
// global-workflow sync endpoint.
func (l *Live) SyncWorkflows(ctx context.Context, dryRun, force bool) (SyncReport, error) {
	return l.Offline.SyncWorkflows(ctx, dryRun, force)
}

// InstallAgent / UninstallAgent: still no daemon endpoint for these,
// so they return ErrDaemonRequired even in Live mode. (The agent
// install verb shells out to `autosk agent install` outside the
// daemon; the lazy TUI documents this in the popup hint.)
func (l *Live) InstallAgent(ctx context.Context, name, version string) error {
	return errors.New("agent install: run `autosk agent install` outside lazy (TUI shell-out is out of scope)")
}

// UninstallAgent — see InstallAgent.
func (l *Live) UninstallAgent(ctx context.Context, name string) error {
	return errors.New("agent uninstall: run `autosk agent uninstall` outside lazy (TUI shell-out is out of scope)")
}

// Compile-time interface check.
var _ Datasource = (*Live)(nil)
var _ Datasource = (*Offline)(nil)
