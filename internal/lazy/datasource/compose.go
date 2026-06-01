package datasource

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"autosk/internal/daemon/client"
	"autosk/internal/store"
)

// Compose multiplexes Offline + Live behind one Datasource, flipping
// based on a 2s health probe. The TUI status-bar reads Health() to
// render the `daemon=ok|down|stale` chip.
//
// Read verbs route to live when the probe says daemon=ok; otherwise
// they go to offline. Writes that have an offline shim (most task /
// workflow / agent verbs) route through that shim; writes that
// fundamentally require a daemon (CancelJob, SendInput, AbortJob,
// StreamLive) return ErrDaemonRequired when daemon!=ok.
type Compose struct {
	off  *Offline
	live *Live

	mu     sync.RWMutex
	health Health
	usingL atomic.Bool

	stop chan struct{}
	once sync.Once
}

// NewCompose wires offline + live + the probe loop. The probe runs in
// the background until Close is called. Interval may be zero (defaults
// to 2s).
func NewCompose(off *Offline, cli *client.Client, interval time.Duration) *Compose {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	live := NewLive(off, cli)
	c := &Compose{
		off:    off,
		live:   live,
		health: Health{Daemon: "down", UpdatedAt: time.Now().UTC()},
		stop:   make(chan struct{}),
	}
	// Initial probe synchronously so the first frame has the right
	// `daemon=...` chip and the right Jobs source. Bounded at 500ms
	// so a hung daemon (socket file present but never replies) can't
	// wedge the TUI before its first frame — the loop's ticker probe
	// uses the same bound for the same reason.
	ctx0, cancel0 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	c.probeOnce(ctx0)
	cancel0()
	go c.loop(interval)
	return c
}

// Close stops the probe goroutine. Idempotent.
func (c *Compose) Close() error {
	c.once.Do(func() { close(c.stop) })
	return nil
}

// Health returns the most recent probe result.
func (c *Compose) Health() Health {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.health
}

// Fallbacks returns the live datasource's cumulative count of
// daemon-preferred reads that errored and fell back to the offline
// base. The TUI status bar reads this on every refresh and renders a
// chip when the counter advances since the last reading, so a daemon
// that's technically reachable but flaky (e.g. timing out on /v1/jobs
// while /v1/healthz still 200s) becomes visible. Zero in pure
// offline mode (Live is never invoked).
func (c *Compose) Fallbacks() uint64 { return c.live.Fallbacks() }

func (c *Compose) loop(d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			c.probeOnce(ctx)
			cancel()
		}
	}
}

func (c *Compose) probeOnce(ctx context.Context) {
	h, _ := c.live.Healthz(ctx)
	c.mu.Lock()
	c.health = h
	c.mu.Unlock()
	c.usingL.Store(h.IsOK())
}

func (c *Compose) usingLive() bool { return c.usingL.Load() }

// ---- Datasource impl. Reads route, writes route.

func (c *Compose) Tasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	return c.off.Tasks(ctx, f)
}
func (c *Compose) GetTask(ctx context.Context, id string) (Task, error) {
	return c.off.GetTask(ctx, id)
}
func (c *Compose) Jobs(ctx context.Context, f JobFilter) ([]Job, error) {
	if c.usingLive() {
		return c.live.Jobs(ctx, f)
	}
	return c.off.Jobs(ctx, f)
}
func (c *Compose) GetJob(ctx context.Context, id string) (Job, error) {
	if c.usingLive() {
		return c.live.GetJob(ctx, id)
	}
	return c.off.GetJob(ctx, id)
}
func (c *Compose) Workflows(ctx context.Context, includeSynthetic bool) ([]Workflow, error) {
	return c.off.Workflows(ctx, includeSynthetic)
}
func (c *Compose) Agents(ctx context.Context) ([]Agent, error) { return c.off.Agents(ctx) }
func (c *Compose) Comments(ctx context.Context, id string) ([]Comment, error) {
	return c.off.Comments(ctx, id)
}
func (c *Compose) Signals(ctx context.Context, jobID string) ([]Signal, error) {
	return c.off.Signals(ctx, jobID)
}
func (c *Compose) SignalsForTask(ctx context.Context, taskID string) ([]Signal, error) {
	return c.off.SignalsForTask(ctx, taskID)
}
func (c *Compose) Messages(ctx context.Context, jobID string, full bool, limit int) ([]MessageEvent, error) {
	if c.usingLive() {
		return c.live.Messages(ctx, jobID, full, limit)
	}
	return c.off.Messages(ctx, jobID, full, limit)
}
func (c *Compose) Healthz(ctx context.Context) (Health, error) { return c.Health(), nil }

// Reconnect forwards to the offline base so the underlying doltlite
// store drops its pooled connection. The live (daemon) branch has no
// connection of its own to refresh — it just goes over UDS — so the
// composed datasource only needs to forward the offline call.
func (c *Compose) Reconnect(ctx context.Context) error { return c.off.Reconnect(ctx) }

func (c *Compose) CreateTask(ctx context.Context, title, desc string, p int) (string, error) {
	return c.off.CreateTask(ctx, title, desc, p)
}
func (c *Compose) UpdateStatus(ctx context.Context, id string, st store.Status) error {
	return c.off.UpdateStatus(ctx, id, st)
}
func (c *Compose) UpdatePriority(ctx context.Context, id string, p int) error {
	return c.off.UpdatePriority(ctx, id, p)
}
func (c *Compose) UpdateTitleDescription(ctx context.Context, id, title, description string) error {
	return c.off.UpdateTitleDescription(ctx, id, title, description)
}
func (c *Compose) Enroll(ctx context.Context, id, wf, stepName string) error {
	return c.off.Enroll(ctx, id, wf, stepName)
}
func (c *Compose) Resume(ctx context.Context, id, to string) error {
	return c.off.Resume(ctx, id, to)
}
func (c *Compose) Block(ctx context.Context, id, b string) error {
	return c.off.Block(ctx, id, b)
}
func (c *Compose) Unblock(ctx context.Context, id, b string) error {
	return c.off.Unblock(ctx, id, b)
}
func (c *Compose) AddComment(ctx context.Context, id, text string) error {
	return c.off.AddComment(ctx, id, text)
}
func (c *Compose) SetMetadata(ctx context.Context, id string, m map[string]any) error {
	return c.off.SetMetadata(ctx, id, m)
}
func (c *Compose) CreateWorkflow(ctx context.Context, path string) (string, error) {
	return c.off.CreateWorkflow(ctx, path)
}
func (c *Compose) DeleteWorkflow(ctx context.Context, name string) error {
	return c.off.DeleteWorkflow(ctx, name)
}
func (c *Compose) UpdateWorkflowIsolation(ctx context.Context, name, mode string, force bool) (UpdateIsolationReport, error) {
	return c.off.UpdateWorkflowIsolation(ctx, name, mode, force)
}
func (c *Compose) SyncWorkflows(ctx context.Context, dryRun, force bool) (SyncReport, error) {
	return c.off.SyncWorkflows(ctx, dryRun, force)
}
func (c *Compose) InstallAgent(ctx context.Context, name, version string) error {
	if c.usingLive() {
		return c.live.InstallAgent(ctx, name, version)
	}
	return c.off.InstallAgent(ctx, name, version)
}
func (c *Compose) UninstallAgent(ctx context.Context, name string) error {
	if c.usingLive() {
		return c.live.UninstallAgent(ctx, name)
	}
	return c.off.UninstallAgent(ctx, name)
}
func (c *Compose) CancelJob(ctx context.Context, id string) error {
	if c.usingLive() {
		return c.live.CancelJob(ctx, id)
	}
	return ErrDaemonRequired
}
func (c *Compose) SendInput(ctx context.Context, id, msg, b string) (string, error) {
	if c.usingLive() {
		return c.live.SendInput(ctx, id, msg, b)
	}
	return "", ErrDaemonRequired
}
func (c *Compose) AbortJob(ctx context.Context, id string) error {
	if c.usingLive() {
		return c.live.AbortJob(ctx, id)
	}
	return ErrDaemonRequired
}
func (c *Compose) StreamLive(ctx context.Context, id string) (*LiveHandle, error) {
	if c.usingLive() {
		return c.live.StreamLive(ctx, id)
	}
	return nil, ErrDaemonRequired
}

// Compile-time check.
var _ Datasource = (*Compose)(nil)
