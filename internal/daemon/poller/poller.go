// Package poller drives the daemon's workflow engine: every poll
// interval it selects unblocked `work` tasks whose current step's agent is
// non-human and which have no queued/running daemon_runs row, then
// enqueues a fresh run for each.
//
// See docs/plans/20260517-Workflows-Plan.md §5.2.
package poller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/scheduler"
)

// DefaultInterval is the poll cadence used when Config.Interval is zero.
const DefaultInterval = 2 * time.Second

// Config tunes the poller.
type Config struct {
	// Interval between scans. ≤ 0 → DefaultInterval.
	Interval time.Duration
	// ProjectKey identifies the project this poller belongs to. Each
	// scheduler.Job enqueued by this poller is tagged with it so the
	// global executor knows which project to dispatch into.
	ProjectKey string
	// Logger receives info/warn output. nil → slog.Default().
	Logger *slog.Logger
}

// Poller scans the DB and feeds the scheduler.
type Poller struct {
	db         *sql.DB
	runs       *runstore.Store
	sched      *scheduler.Scheduler
	cfg        Config
	log        *slog.Logger
	projectKey string

	startMu sync.Mutex
	started bool
	cancel  context.CancelFunc
	doneCh  chan struct{}
}

// New constructs a Poller. cfg.ProjectKey must be set when the poller
// is wired into the multi-project daemon — tests that don't care can
// leave it empty.
func New(db *sql.DB, runs *runstore.Store, sched *scheduler.Scheduler, cfg Config) *Poller {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Poller{db: db, runs: runs, sched: sched, cfg: cfg, log: log, projectKey: cfg.ProjectKey}
}

// Sentinel errors.
var (
	ErrAlreadyStarted = errors.New("poller: already started")
	ErrNotStarted     = errors.New("poller: not started")
)

// Start launches the poll loop. It runs one immediate scan, then ticks
// every cfg.Interval until Stop is called.
func (p *Poller) Start(ctx context.Context) error {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if p.started {
		return ErrAlreadyStarted
	}
	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.doneCh = make(chan struct{})
	p.started = true
	go p.loop(runCtx)
	return nil
}

// Stop cancels the loop and waits for it to exit, or for graceCtx.
func (p *Poller) Stop(graceCtx context.Context) error {
	p.startMu.Lock()
	if !p.started {
		p.startMu.Unlock()
		return ErrNotStarted
	}
	p.started = false
	p.cancel()
	doneCh := p.doneCh
	p.startMu.Unlock()
	select {
	case <-doneCh:
		return nil
	case <-graceCtx.Done():
		return graceCtx.Err()
	}
}

// loop runs scans on a ticker until ctx is cancelled.
func (p *Poller) loop(ctx context.Context) {
	defer close(p.doneCh)
	// Immediate first scan so newly-created tasks don't wait a full
	// interval before being picked up.
	p.scanOnce(ctx)
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.scanOnce(ctx)
		}
	}
}

// Candidate is one task the scan picked up.
type Candidate struct {
	TaskID string
	StepID string
}

// scanOnce runs one scan and enqueues every candidate.
func (p *Poller) scanOnce(ctx context.Context) {
	cands, err := p.Scan(ctx)
	if err != nil {
		p.log.Warn("poller: scan failed", "err", err)
		return
	}
	for _, c := range cands {
		if err := p.enqueueCandidate(ctx, c); err != nil {
			p.log.Warn("poller: enqueue failed", "task", c.TaskID, "err", err)
			continue
		}
	}
}

// Scan runs the candidate query and returns rows ordered by priority ASC,
// created_at ASC.
//
// Public so tests / `autosk daemon scan` style commands could call it.
func (p *Poller) Scan(ctx context.Context) ([]Candidate, error) {
	if p.db == nil {
		return nil, errors.New("poller: db is nil")
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT t.id, t.current_step_id
		  FROM tasks t
		  JOIN steps s   ON t.current_step_id = s.id
		  JOIN agents a  ON s.agent_id        = a.id
		 WHERE t.status = 'work'
		   AND a.is_human = 0
		   AND NOT EXISTS (
		         SELECT 1 FROM task_deps d
		           JOIN tasks b ON b.id = d.blocker_id
		          WHERE d.blocked_id = t.id
		            AND d.kind = 'blocks'
		            AND b.status NOT IN ('done','cancel')
		       )
		   AND NOT EXISTS (
		         SELECT 1 FROM daemon_runs r
		          WHERE r.task_id = t.id
		            AND r.status IN ('queued','running')
		       )
		 ORDER BY t.priority ASC, t.created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("poll query: %w", err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.TaskID, &c.StepID); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// enqueueCandidate creates a daemon_runs row for the candidate and
// pushes it to the scheduler. Errors from the scheduler (e.g. queue
// full) are surfaced; we don't retry here — the next tick will pick
// the task up again (the daemon_runs row stays queued).
func (p *Poller) enqueueCandidate(ctx context.Context, c Candidate) error {
	run, err := p.runs.CreateRun(ctx, runstore.NewRun{
		TaskID: c.TaskID,
		StepID: c.StepID,
	})
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	p.log.Info("poller: enqueued", "task", c.TaskID, "step", c.StepID, "job", run.JobID, "project", p.projectKey)
	if err := p.sched.Enqueue(scheduler.Job{Project: p.projectKey, ID: run.JobID}); err != nil {
		if errors.Is(err, scheduler.ErrQueueFull) {
			// Leave the row queued; the worker will pick it up when a slot
			// frees. The next tick will skip it (it's already in
			// status='queued').
			p.log.Warn("poller: scheduler queue full; row queued in DB", "job", run.JobID)
			return nil
		}
		return fmt.Errorf("enqueue %s: %w", run.JobID, err)
	}
	return nil
}
