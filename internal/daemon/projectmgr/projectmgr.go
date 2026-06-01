// Package projectmgr owns the daemon's per-process cache of "loaded"
// autosk projects. The multi-project daemon listens on a single UDS;
// each HTTP request carries an X-Autosk-Cwd header that names the
// project root. The manager resolves that header into a canonical
// Key (= absolute project root with symlinks evaluated) and lazily
// opens the project's database + stores + per-project executor and
// poller on first sight.
//
// Per docs/plans/20260518-Daemon-UDS-Plan.md §4.1.
package projectmgr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/comments"
	"autosk/internal/daemon/compactor"
	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/pirunners"
	"autosk/internal/daemon/poller"
	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/scheduler"

	"autosk/internal/pathcanon"
	"autosk/internal/projectdb"
	"autosk/internal/step"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
	"autosk/internal/worktree"
)

// Key is the canonical absolute project root (filepath.Clean +
// EvalSymlinks). Two requests that target the same on-disk directory
// always reduce to the same Key, regardless of how the user spelled it.
type Key string

// Sentinel errors. Server handlers map these to 4xx responses.
var (
	// ErrInvalidCwd — the X-Autosk-Cwd header was empty or non-absolute.
	ErrInvalidCwd = errors.New("projectmgr: invalid cwd")
	// ErrProjectNotFound — no .autosk/db could be found from the given
	// cwd (walking up) and no override was provided.
	ErrProjectNotFound = errors.New("projectmgr: project not found")
)

// Deps bundles the cross-project collaborators the manager needs to
// construct each Project's executor and poller.
//
// Runners and Attachments are the daemon-wide attach hubs (see
// internal/daemon/pirunners). The manager threads them into the
// per-project executor so live pi runners register / unregister and
// the executor can consult the attach counter on turn boundaries.
// Both fields default to nil (= attach features disabled).
type Deps struct {
	Sched        *scheduler.Scheduler
	Packages     *pkgregistry.Registry
	ExecCfg      executor.Config // PIBin, Grace, IdleTimeout, SessionDirRoot (no ProjectRoot)
	PollInterval time.Duration
	// GCInterval tunes the per-project doltlite chunk-store
	// compactor. 0 → compactor.DefaultInterval (30m); <0 → disabled
	// (the operator opts out via `autosk daemon serve --gc-interval=-1`
	// and runs `autosk gc` on demand instead).
	GCInterval  time.Duration
	Logger      *slog.Logger
	Runners     *pirunners.Registry
	Attachments *pirunners.Attachments
}

// Project is a single opened autosk project. All fields are immutable
// after open returns.
type Project struct {
	Key       Key
	Root      string // canonical root (== string(Key))
	DBPath    string // absolute path to .autosk/db
	Tasks     *doltlite.Store
	Runs      *runstore.Store
	Agents    *agent.Store
	Workflows *workflow.Store
	Comments  *comments.Store
	Signals   *step.Store
	Executor  *executor.Executor
	Poller    *poller.Poller
	Compactor *compactor.Compactor

	OpenedAt time.Time

	mu               sync.Mutex
	closed           bool
	pollerStopped    bool
	compactorStopped bool
	closeFns         []func() error
}

// Manager is the per-daemon project cache.
type Manager struct {
	mu       sync.Mutex
	projects map[Key]*projectEntry
	deps     Deps
	wtMgr    worktree.Manager
}

// projectEntry tracks the lifecycle of a project so concurrent Resolve
// calls on the same key serialise on a readyCh and observe either the
// opened Project or the open error.
type projectEntry struct {
	readyCh chan struct{}
	proj    *Project
	err     error
}

// New constructs a manager. The caller wires Deps once at daemon start.
//
// A single worktree.Manager is created here and shared with every
// per-project executor so racing Ensure/Verify calls on the same task
// serialise correctly across projects.
func New(deps Deps) *Manager {
	return &Manager{
		projects: make(map[Key]*projectEntry),
		deps:     deps,
		wtMgr:    worktree.NewManager(),
	}
}

// Resolve returns the Project for the given cwd, opening it if needed.
//
// Concurrency: callers that race on the same key block on the entry's
// readyCh and observe the same *Project (or the same open error).
// Distinct keys open in parallel.
func (m *Manager) Resolve(ctx context.Context, cwd, dbOverride string) (*Project, error) {
	cwd = filepath.Clean(cwd)
	if cwd == "" || cwd == "." || !filepath.IsAbs(cwd) {
		return nil, fmt.Errorf("%w: %q (must be absolute)", ErrInvalidCwd, cwd)
	}
	// Use the env-blind resolver so the daemon's own AUTOSK_DB cannot
	// leak into a request: only the per-request override and walk-up
	// from the supplied cwd are consulted.
	dbPath, err := projectdb.ResolveNoEnv(cwd, dbOverride)
	if err != nil {
		if errors.Is(err, projectdb.ErrNotFound) {
			return nil, fmt.Errorf("%w: from %s", ErrProjectNotFound, cwd)
		}
		return nil, err
	}
	dbPath, err = filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("projectmgr: absolutise db path: %w", err)
	}
	// The daemon never auto-creates a database. ResolveNoEnv returns the
	// override path as-is (without statting); without this guard a request
	// with X-Autosk-DB pointing at a missing path would silently create a
	// fresh empty .autosk/db when the doltlite store opens it. Per
	// docs/plans/20260518-Daemon-UDS-Plan.md §4.1.
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: db file missing at %s", ErrProjectNotFound, dbPath)
		}
		return nil, fmt.Errorf("projectmgr: stat db %s: %w", dbPath, statErr)
	}
	// Canonical root is the directory containing .autosk/ (== parent of .autosk/db's parent).
	rawRoot := filepath.Dir(filepath.Dir(dbPath))
	canonRoot, cerr := pathcanon.Existing(rawRoot)
	if cerr != nil {
		// If the root cannot be canonicalised (e.g. permissions), fall
		// back to the lexical clean — still deterministic per request.
		canonRoot = filepath.Clean(rawRoot)
	}
	key := Key(canonRoot)

	m.mu.Lock()
	entry, ok := m.projects[key]
	if ok {
		m.mu.Unlock()
		// Wait for the in-flight open (or instant return if already opened).
		select {
		case <-entry.readyCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if entry.err != nil {
			return nil, entry.err
		}
		return entry.proj, nil
	}
	entry = &projectEntry{readyCh: make(chan struct{})}
	m.projects[key] = entry
	m.mu.Unlock()

	// Open outside the manager lock so distinct keys can open in
	// parallel and slow opens never block resolves of other projects.
	proj, openErr := m.openProject(ctx, key, dbPath)
	if openErr != nil {
		// Remove the entry so subsequent calls can retry with a fresh
		// state (otherwise we'd memoise the failure forever).
		m.mu.Lock()
		delete(m.projects, key)
		m.mu.Unlock()
		entry.err = openErr
		close(entry.readyCh)
		return nil, openErr
	}
	// Publish the entry *before* starting the poller. Otherwise the
	// poller's first scan could enqueue a job whose scheduler closure
	// then races Get(key) against the still-open readyCh and drops the
	// job. After this point Get returns proj synchronously.
	entry.proj = proj
	close(entry.readyCh)

	if err := proj.Poller.Start(ctx); err != nil {
		// poller already started or refused to start; the project entry
		// is still usable (mgr.Get returns it) but autonomous progress
		// is broken. Surface the breadcrumb and continue.
		if m.deps.Logger != nil {
			m.deps.Logger.Warn("projectmgr: poller start failed",
				"project", proj.Root, "err", err)
		}
	}
	if proj.Compactor != nil {
		if err := proj.Compactor.Start(ctx); err != nil && !errors.Is(err, compactor.ErrDisabled) {
			if m.deps.Logger != nil {
				m.deps.Logger.Warn("projectmgr: compactor start failed",
					"project", proj.Root, "err", err)
			}
		} else if errors.Is(err, compactor.ErrDisabled) && m.deps.Logger != nil {
			m.deps.Logger.Info("projectmgr: compactor disabled (--gc-interval<0)",
				"project", proj.Root)
		}
	}
	if m.deps.Logger != nil {
		m.deps.Logger.Info("projectmgr: opened project",
			"project", proj.Root, "db", proj.DBPath)
	}
	return proj, nil
}

// Get returns the opened Project for key, or nil if not currently
// loaded. Used by the scheduler executor closure (which only ever fires
// for already-loaded projects, since the poller opened them).
func (m *Manager) Get(key Key) (*Project, bool) {
	m.mu.Lock()
	entry, ok := m.projects[key]
	m.mu.Unlock()
	if !ok {
		return nil, false
	}
	select {
	case <-entry.readyCh:
	default:
		// Open in progress — caller shouldn't block here.
		return nil, false
	}
	if entry.err != nil {
		return nil, false
	}
	return entry.proj, true
}

// Loaded returns a snapshot of currently-loaded, fully-opened projects.
// Order is unspecified.
func (m *Manager) Loaded() []*Project {
	m.mu.Lock()
	entries := make([]*projectEntry, 0, len(m.projects))
	for _, e := range m.projects {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	out := make([]*Project, 0, len(entries))
	for _, e := range entries {
		select {
		case <-e.readyCh:
		default:
			continue
		}
		if e.err != nil || e.proj == nil {
			continue
		}
		out = append(out, e.proj)
	}
	return out
}

// snapshot returns a copy of currently-tracked entries and clears the
// manager's map. Callers can then drain pollers and close DBs without
// holding the manager lock. Safe to call once: a second call returns
// an empty slice.
func (m *Manager) snapshot() []*projectEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*projectEntry, 0, len(m.projects))
	for _, e := range m.projects {
		out = append(out, e)
	}
	m.projects = make(map[Key]*projectEntry)
	return out
}

// StopPollers stops every per-project poller (and the parallel
// chunk-store compactor). Run this *before* scheduler.Stop so no new
// daemon_runs rows are inserted while the scheduler is draining;
// otherwise an enqueued job could attempt to MarkRunning /
// MarkCancelled against a project DB that's already closed.
//
// Per docs/plans/20260518-Daemon-UDS-Plan.md §6.2.
func (m *Manager) StopPollers(graceCtx context.Context) error {
	entries := m.stoppingSnapshot()
	var errs []error
	for _, e := range entries {
		if e.proj == nil {
			continue
		}
		if e.proj.Poller != nil {
			if err := e.proj.stopPoller(graceCtx); err != nil {
				errs = append(errs, fmt.Errorf("poller stop %s: %w", e.proj.Root, err))
				if m.deps.Logger != nil {
					m.deps.Logger.Warn("projectmgr: poller stop",
						"project", e.proj.Root, "err", err)
				}
			}
		}
		if e.proj.Compactor != nil {
			if err := e.proj.stopCompactor(graceCtx); err != nil {
				errs = append(errs, fmt.Errorf("compactor stop %s: %w", e.proj.Root, err))
				if m.deps.Logger != nil {
					m.deps.Logger.Warn("projectmgr: compactor stop",
						"project", e.proj.Root, "err", err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// stoppingSnapshot returns the same entries that snapshot() does but
// without clearing the manager map — StopPollers is intentionally
// non-destructive so that handlers serving in-flight requests can still
// see the project until CloseDBs runs.
func (m *Manager) stoppingSnapshot() []*projectEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*projectEntry, 0, len(m.projects))
	for _, e := range m.projects {
		select {
		case <-e.readyCh:
		default:
			continue
		}
		out = append(out, e)
	}
	return out
}

// CloseDBs closes every per-project DB handle. Run this *after*
// scheduler.Stop has returned so no worker can be holding a transaction
// on a project's *sql.DB at close time. Per
// docs/plans/20260518-Daemon-UDS-Plan.md §6.2.
//
// Errors from individual projects are aggregated via errors.Join.
func (m *Manager) CloseDBs(graceCtx context.Context) error {
	entries := m.snapshot()
	var errs []error
	for _, e := range entries {
		select {
		case <-e.readyCh:
		default:
			continue
		}
		if e.proj == nil {
			continue
		}
		if err := e.proj.closeDB(graceCtx); err != nil {
			errs = append(errs, fmt.Errorf("close db %s: %w", e.proj.Root, err))
			if m.deps.Logger != nil {
				m.deps.Logger.Warn("projectmgr: close db",
					"project", e.proj.Root, "err", err)
			}
		}
	}
	return errors.Join(errs...)
}

// CloseAll stops every per-project poller and closes every per-project
// DB *in that order*. graceCtx bounds both phases.
//
// Note: in the daemon's shutdown sequence the scheduler must be stopped
// between StopPollers and CloseDBs (so in-flight workers can flush
// MarkCancelled/MarkFailed against an open DB). Callers that need that
// contract should call StopPollers, sched.Stop, then CloseDBs directly
// instead of CloseAll. CloseAll is retained for tests and any caller
// that does not own a scheduler.
//
// Returns the aggregated error from both phases (errors.Join).
func (m *Manager) CloseAll(graceCtx context.Context) error {
	var errs []error
	if err := m.StopPollers(graceCtx); err != nil {
		errs = append(errs, err)
	}
	if err := m.CloseDBs(graceCtx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// openProject opens the per-project DB, runs migrations + restart
// recovery, and constructs all per-project stores. It builds the poller
// but does *not* start it — Resolve does that after publishing the
// project entry so the scheduler closure can see the project on first
// scan (otherwise the very first enqueued job races readyCh and is
// silently dropped). Per the open-vs-poll race fix.
func (m *Manager) openProject(ctx context.Context, key Key, dbPath string) (*Project, error) {
	root := string(key)
	tasks := doltlite.New()
	if err := tasks.Open(ctx, dbPath); err != nil {
		return nil, fmt.Errorf("open db %s: %w", dbPath, err)
	}
	closeFns := []func() error{tasks.Close}
	closeOnErr := func(p *Project, err error) (*Project, error) {
		for i := len(closeFns) - 1; i >= 0; i-- {
			_ = closeFns[i]()
		}
		return nil, err
	}
	if err := tasks.Migrate(ctx); err != nil {
		return closeOnErr(nil, fmt.Errorf("migrate %s: %w", dbPath, err))
	}

	runs := runstore.New(tasks.DB())
	ag := agent.New(tasks.DB()).WithResolver(m.deps.Packages)
	wfs := workflow.New(tasks.DB(), ag)
	cs := comments.New(tasks.DB())
	sigs := step.New(tasks.DB())

	// Restart recovery: rewrite any rows the daemon last left in
	// 'running' to failed/'daemon_restart'. Must happen before the
	// poller starts (otherwise the poller would see them as queued/
	// running and skip the underlying tasks).
	if n, err := runs.SweepRunningOnStartup(ctx); err != nil {
		return closeOnErr(nil, fmt.Errorf("restart recovery for %s: %w", root, err))
	} else if n > 0 && m.deps.Logger != nil {
		m.deps.Logger.Warn("projectmgr: rewrote stale running rows on first open",
			"project", root, "count", n)
	}

	// Per-project executor. ProjectRoot is per-project; everything else
	// inherits from manager-level config. The executor uses
	// cfg.SessionDirRoot verbatim (a literal path, shared across
	// projects when set) and falls back to <ProjectRoot>/.autosk/sessions
	// when empty. DBPath is the canonical .autosk/db path the executor
	// threads into the spawned child as AUTOSK_DB for worktree-isolated
	// runs (the agent's cwd lives outside the project tree).
	execCfg := m.deps.ExecCfg
	execCfg.ProjectRoot = root
	execCfg.DBPath = dbPath
	ex := executor.New(executor.Deps{
		Runs:        runs,
		Tasks:       tasks,
		Agents:      ag,
		Workflows:   wfs,
		Comments:    cs,
		Signals:     sigs,
		Packages:    m.deps.Packages,
		Runners:     m.deps.Runners,
		Attachments: m.deps.Attachments,
		Worktree:    m.wtMgr,
	}, executor.DefaultFactory, execCfg)

	pl := poller.New(tasks.DB(), runs, m.deps.Sched, poller.Config{
		Interval:   m.deps.PollInterval,
		ProjectKey: root,
		Logger:     m.deps.Logger,
	})

	co := compactor.New(tasks, compactor.Config{
		Interval:   m.deps.GCInterval,
		ProjectKey: root,
		Logger:     m.deps.Logger,
	})

	proj := &Project{
		Key:       key,
		Root:      root,
		DBPath:    dbPath,
		Tasks:     tasks,
		Runs:      runs,
		Agents:    ag,
		Workflows: wfs,
		Comments:  cs,
		Signals:   sigs,
		Executor:  ex,
		Poller:    pl,
		Compactor: co,
		OpenedAt:  time.Now().UTC(),
		closeFns:  closeFns,
	}
	return proj, nil
}

// stopPoller stops the per-project poller. Idempotent. graceCtx bounds
// the wait for the poll loop to exit.
func (p *Project) stopPoller(graceCtx context.Context) error {
	p.mu.Lock()
	if p.pollerStopped {
		p.mu.Unlock()
		return nil
	}
	p.pollerStopped = true
	p.mu.Unlock()
	if p.Poller == nil {
		return nil
	}
	if err := p.Poller.Stop(graceCtx); err != nil {
		// Already-stopped is fine for shutdown idempotency.
		if errors.Is(err, poller.ErrNotStarted) {
			return nil
		}
		return err
	}
	return nil
}

// stopCompactor stops the per-project chunk-store compactor.
// Idempotent, mirrors stopPoller. Disabled compactors (cfg.Interval<0)
// never started, so ErrNotStarted is swallowed here.
func (p *Project) stopCompactor(graceCtx context.Context) error {
	p.mu.Lock()
	if p.compactorStopped {
		p.mu.Unlock()
		return nil
	}
	p.compactorStopped = true
	p.mu.Unlock()
	if p.Compactor == nil {
		return nil
	}
	if err := p.Compactor.Stop(graceCtx); err != nil {
		if errors.Is(err, compactor.ErrNotStarted) {
			return nil
		}
		return err
	}
	return nil
}

// closeDB releases the per-project DB handle (and any future cleanup
// captured in closeFns). Idempotent.
func (p *Project) closeDB(_ context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	closeFns := p.closeFns
	p.closeFns = nil
	p.mu.Unlock()

	var firstErr error
	// closeFns holds the doltlite.Close handle. Iterate LIFO for parity
	// with future additions.
	for i := len(closeFns) - 1; i >= 0; i-- {
		fn := closeFns[i]
		if fn == nil {
			continue
		}
		if err := fn(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// close is the legacy combined operation: stop poller (+ compactor)
// then close DB. Retained for callers that don't separate the two
// phases (tests, Manager.CloseAll).
func (p *Project) close(graceCtx context.Context) error {
	var firstErr error
	if err := p.stopPoller(graceCtx); err != nil {
		firstErr = err
	}
	if err := p.stopCompactor(graceCtx); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := p.closeDB(graceCtx); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
