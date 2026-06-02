// Package executor drives a single workflow-step run end to end.
//
// Per docs/plans/20260518-Agent-Packages.md §6:
//
//  1. MarkRunning(job_id).
//  2. Resolve agent from step_id → steps.agent_id → agent.name, then
//     resolve the npm package config via pkgregistry.
//  3. Render the prompt from (task, current step, agent config, comments).
//  4. Spawn the right runner:
//     - cfg.Runner == "" → spawn pi --mode rpc with the package's
//     model / thinking / first_message / extra_args /
//     pi_extensions / pi_skills.
//     - cfg.Runner != "" → spawn the Node bootstrapper (handled in a
//     later phase; today this path returns ErrCustomRunnerNotWired).
//  5. SendPrompt; WaitForAgentEnd.
//  6. Read step_signals for the run. If present, advance the task
//     atomically and MarkDone with transition_id. If absent, kickback
//     up to max_corrections times; then fail with
//     error="agent_did_not_emit_transition".
//  7. Clean shutdown.
//
// Single code path: `--agent NAME` runs traverse the auto-generated
// `single:<NAME>` workflow, so there is no second branch here.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/comments"
	"autosk/internal/daemon/agentnode"
	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/pirunners"
	"autosk/internal/daemon/runstore"

	"autosk/internal/step"
	"autosk/internal/store"
	"autosk/internal/workflow"
	"autosk/internal/worktree"
)

// PiRunner is the subset of *pi.Runner the executor uses. Tests substitute
// a fake.
type PiRunner interface {
	PID() int
	Events() <-chan pi.Event
	GetState(ctx context.Context) (pi.SessionInfo, error)
	SendPrompt(ctx context.Context, message string) error
	WaitForAgentEnd(ctx context.Context) error
	Abort(ctx context.Context) error
	CloseStdin() error
	Terminate() error
	Kill() error
	Wait(ctx context.Context, grace time.Duration) (int, error)
}

// Factory spawns a new PiRunner. Production wraps pi.Spawn; tests inject a stub.
type Factory func(ctx context.Context, opts pi.Opts) (PiRunner, error)

// DefaultFactory wraps pi.Spawn.
var DefaultFactory Factory = func(ctx context.Context, opts pi.Opts) (PiRunner, error) {
	return pi.Spawn(ctx, opts)
}

// NodeFactory spawns a new PiRunner backed by a Node bootstrapper, for
// custom-runner agent packages. Tests inject a stub; production wraps
// agentnode.Spawn.
type NodeFactory func(ctx context.Context, opts agentnode.Opts) (PiRunner, error)

// DefaultNodeFactory wraps agentnode.Spawn.
var DefaultNodeFactory NodeFactory = func(ctx context.Context, opts agentnode.Opts) (PiRunner, error) {
	return agentnode.Spawn(ctx, opts)
}

// TaskStore is the subset of the autosk task store the executor depends on.
// store.Store satisfies it.
type TaskStore interface {
	GetTask(ctx context.Context, id string) (store.Task, error)
	UpdateTask(ctx context.Context, id string, p store.TaskPatch) (store.Task, error)
	UpdateMetadataAndPatch(ctx context.Context, id string, fn func(m map[string]any) error, p store.TaskPatch) (store.Task, error)
}

// Deps bundles every store/handle the executor needs.
type Deps struct {
	Runs      *runstore.Store
	Tasks     TaskStore
	Agents    *agent.Store
	Workflows *workflow.Store
	Comments  *comments.Store
	Signals   *step.Store
	// Packages resolves agent names to installed npm package configs.
	// Required; nil means "no agents available" and every spawn fails.
	Packages *pkgregistry.Registry
	// Runners is the daemon-wide in-memory map of live pi runner
	// handles. The executor registers the spawned RunnerHandle on Run
	// and unregisters in deferred cleanup; nil disables the hook.
	Runners *pirunners.Registry
	// Attachments is the daemon-wide attach counter. The executor
	// consults Attached(jobID) on turn boundaries to skip correction
	// prompts while a client is attached; nil disables the hook.
	Attachments *pirunners.Attachments
	// Worktree manages per-task git worktrees for workflows that opt
	// into isolation=worktree. nil falls back to a fresh manager; the
	// daemon shares one instance per process so racing Ensure/Verify
	// calls on the same task serialise correctly.
	Worktree worktree.Manager
}

// Config tunes the executor.
type Config struct {
	// PIBin overrides the binary used by the default factory.
	PIBin string
	// SessionDirRoot is the parent dir for per-job pi session dirs.
	// Empty → "<projectRoot>/.autosk/sessions".
	SessionDirRoot string
	// ProjectRoot is where .autosk/ lives. Used as cwd for the spawned
	// agent process and to default SessionDirRoot.
	ProjectRoot string
	// DBPath is the absolute path to the project's .autosk/db file.
	// Threaded into the child process as AUTOSK_DB when the workflow
	// opts into worktree isolation — the agent's cwd lives outside
	// the project tree and autosk CLI calls inside the worktree need
	// the explicit pointer back to the canonical DB.
	DBPath string
	// Grace is the SIGTERM → SIGKILL grace period.
	Grace time.Duration
	// IdleTimeout caps a single WaitForAgentEnd.
	IdleTimeout time.Duration
	// SessionPollBudget caps the total wall-clock the background
	// session-info poller spends waiting for pi to stamp sessionFile
	// after SendPrompt. Zero falls back to sessionPollBudget (~30s).
	// Tests set this small so the goroutine doesn't outlive their DB.
	SessionPollBudget time.Duration
}

// Executor wires everything together. Implements scheduler.Executor.
type Executor struct {
	deps        Deps
	factory     Factory
	nodeFactory NodeFactory
	cfg         Config
}

// New constructs the executor with the default Node factory.
func New(deps Deps, factory Factory, cfg Config) *Executor {
	if factory == nil {
		factory = DefaultFactory
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 10 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	if deps.Worktree == nil {
		deps.Worktree = worktree.NewManager()
	}
	return &Executor{deps: deps, factory: factory, nodeFactory: DefaultNodeFactory, cfg: cfg}
}

// WithNodeFactory overrides the Node bootstrapper factory. Used by
// tests to inject a stub runner that doesn't actually shell node.
func (e *Executor) WithNodeFactory(nf NodeFactory) *Executor {
	if nf == nil {
		nf = DefaultNodeFactory
	}
	e.nodeFactory = nf
	return e
}

// ErrAgentDidNotEmit is the terminal failure produced when the agent runs
// out of correction attempts without emitting a `step next` signal.
var ErrAgentDidNotEmit = errors.New("agent_did_not_emit_transition")

// advanceTargetError wraps any error returned from advanceTask's
// sibling-step branch (sig.NextStepName != "") together with the id of
// the step the run was trying to enter. parkTaskOnFailure inspects the
// error chain for this wrapper and, when present, parks the task on
// the TARGET step instead of the SOURCE step.
//
// The motivation is the cap-exceeded case: once the SOURCE step has
// emitted a valid `step next --to TARGET` signal it is done, and a
// human resuming the parked task wants to re-run TARGET, not SOURCE.
// The same target-routing applies to any failure inside EnterStep
// (cap, store error, etc.) — the target is the run's intent and
// preserving it gives `autosk resume <id>` without `--to` the right
// landing step once visits are reset.
//
// Failure modes that are NOT routed through advanceTask (pi crash,
// runner timeout, SendPrompt error, shutdown error) intentionally do
// NOT use this wrapper: the target step is unknown there, so the task
// stays parked on the source step.
type advanceTargetError struct {
	TargetStepID string
	Err          error
}

func (e *advanceTargetError) Error() string { return e.Err.Error() }
func (e *advanceTargetError) Unwrap() error { return e.Err }

// Run is the scheduler.Executor entry point.
func (e *Executor) Run(ctx context.Context, jobID string) error {
	bg := context.Background() // for persisting terminal state after ctx cancel

	run, err := e.deps.Runs.GetRun(ctx, jobID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}

	// 1. queued → running.
	run, err = e.deps.Runs.MarkRunning(ctx, jobID, 0)
	if err != nil {
		return fmt.Errorf("mark running: %w", err)
	}

	// 2. Resolve step → agent → agent config.
	stepRow, err := e.deps.Workflows.FindStepByID(ctx, run.StepID)
	if err != nil {
		return e.failTerminal(bg, jobID, nil, fmt.Errorf("find step %s: %w", run.StepID, err))
	}
	wf, err := e.deps.Workflows.GetByID(ctx, stepRow.WorkflowID)
	if err != nil {
		return e.failTerminal(bg, jobID, nil, fmt.Errorf("get workflow %s: %w", stepRow.WorkflowID, err))
	}
	tk, err := e.deps.Tasks.GetTask(ctx, run.TaskID)
	if err != nil {
		return e.failTerminal(bg, jobID, nil, fmt.Errorf("get task %s: %w", run.TaskID, err))
	}
	if e.deps.Packages == nil {
		return e.failTerminal(bg, jobID, nil, fmt.Errorf("agent_config_invalid: executor has no package registry attached"))
	}
	agentCfg, err := e.deps.Packages.Resolve(stepRow.AgentName)
	if err != nil {
		return e.failTerminal(bg, jobID, nil, fmt.Errorf("agent_config_invalid: %w", err))
	}
	if merr := applyAgentParamOverrides(&agentCfg, stepRow.AgentParams); merr != nil {
		return e.failTerminal(bg, jobID, nil, fmt.Errorf("agent_config_invalid: %w", merr))
	}
	// 3. Render prompt + (for custom runners) a JSON seed.
	commentLines, _ := e.deps.Comments.RenderForPrompt(ctx, run.TaskID)
	prompt := RenderPrompt(tk, wf, stepRow, agentCfg, commentLines)

	// 4. Spawn the right runner.
	sessionDir := e.sessionDirFor(run)
	if err := ensureDir(sessionDir); err != nil {
		return e.failTerminal(bg, jobID, nil, fmt.Errorf("session dir: %w", err))
	}

	// Per-workflow isolation: when the workflow opts into worktree
	// isolation we point the child at the per-task worktree and thread
	// AUTOSK_DB so autosk CLI calls inside the worktree still find the
	// canonical project DB. See docs/plans/20260521-Worktree-Isolation.md.
	cwd := e.cfg.ProjectRoot
	var runEnv []string
	if wf.Isolation == workflow.IsolationWorktree {
		path, perr := worktree.PathFor(e.cfg.ProjectRoot, tk.ID)
		if perr != nil {
			return e.failTerminal(bg, jobID, nil, fmt.Errorf("worktree path: %w", perr))
		}
		if verr := e.deps.Worktree.Verify(ctx, e.cfg.ProjectRoot, tk.ID); verr != nil {
			// Auto-recovery for the simple "directory just isn't there"
			// case: a previous terminal cleanup reaped the dir, the task
			// was reopened/re-enrolled and the dir got cleaned again, or
			// the operator nuked ~/.autosk/worktrees by hand. The branch
			// is the load-bearing piece (it survives terminal cleanup
			// by design); re-allocating the dir on the existing branch
			// is a safe no-op-equivalent and saves the human a manual
			// cancel + enroll dance.
			//
			// Stranded / not-a-git-repo are NOT auto-recovered: they
			// signal that the project itself moved or git state broke,
			// neither of which we can safely fix from here. Those keep
			// the documented park flow.
			if errors.Is(verr, worktree.ErrWorktreeMissing) {
				res, eerr := e.deps.Worktree.Ensure(ctx, e.cfg.ProjectRoot, tk.ID, "")
				if eerr != nil {
					return e.failTerminal(bg, jobID, nil, fmt.Errorf("worktree_missing: re-allocate failed: %w", eerr))
				}
				slog.Default().Info("executor: re-allocated missing worktree",
					"task", tk.ID,
					"job", jobID,
					"path", res.Path,
					"branch", res.Branch)
			} else {
				// ErrWorktreeStranded / ErrNotGitRepo (and any future
				// Verify-only error) collapse to worktree_stranded:
				// the on-disk state exists in some form but isn't safe
				// to spawn into.
				return e.failTerminal(bg, jobID, nil, fmt.Errorf("worktree_stranded: %w", verr))
			}
		}
		cwd = path
		dbPath := e.cfg.DBPath
		if dbPath == "" {
			dbPath = filepath.Join(e.cfg.ProjectRoot, ".autosk", "db")
		}
		runEnv = append(os.Environ(), "AUTOSK_DB="+dbPath)
	}

	var (
		runner     PiRunner
		initialMsg = prompt
	)
	if agentCfg.Runner != "" {
		// Custom JS runner: spawn the Node bootstrapper. The initial
		// stdin payload is a JSON RunContextSeed, not the rendered
		// prompt. Custom runners are single-shot; we force the local
		// MaxCorrections to 0 so a missed signal fails immediately.
		bootstrap := e.deps.Packages.RuntimeBootstrapPath()
		seed, serr := RenderSeedJSON(tk, wf, stepRow, agentCfg, commentLines, run.JobID)
		if serr != nil {
			return e.failTerminal(bg, jobID, nil, fmt.Errorf("render seed: %w", serr))
		}
		initialMsg = seed
		runner, err = e.nodeFactory(ctx, agentnode.Opts{
			BootstrapPath: bootstrap,
			PackageName:   agentCfg.Name,
			RunnerPath:    agentCfg.Runner,
			Cwd:           cwd,
			Env:           runEnv,
			UseTsxLoader:  true,
		})
		if err != nil {
			return e.failTerminal(bg, jobID, nil, fmt.Errorf("spawn runner: %w", err))
		}
		run.MaxCorrections = 0
	} else {
		extraArgs := buildPiExtraArgs(agentCfg)
		runner, err = e.factory(ctx, pi.Opts{
			PIBin:      e.cfg.PIBin,
			Cwd:        cwd,
			Env:        runEnv,
			Model:      agentCfg.Model,
			Thinking:   agentCfg.Thinking,
			SessionDir: sessionDir,
			ExtraArgs:  extraArgs,
		})
		if err != nil {
			return e.failTerminal(bg, jobID, nil, fmt.Errorf("spawn pi: %w", err))
		}
	}
	defer e.cleanup(runner, bg)

	// Register the runner for the attach surface. The handle interface is
	// the narrow pirunners.RunnerHandle subset; *pi.Runner satisfies it.
	// Custom Node runners do not (no IsStreaming today) — skip registration
	// rather than panicking on a runtime type assertion.
	if e.deps.Runners != nil {
		if h, ok := runner.(pirunners.RunnerHandle); ok {
			e.deps.Runners.Register(jobID, h)
			defer e.deps.Runners.Unregister(jobID)
		}
	}

	if pid := runner.PID(); pid > 0 {
		_ = e.deps.Runs.SetPID(ctx, jobID, pid)
	}

	// 5. Initial prompt / JSON seed.
	if err := runner.SendPrompt(ctx, initialMsg); err != nil {
		return e.handleRunError(ctx, bg, jobID, runner, err)
	}

	// 5a. Record pi's session info in the background.
	//
	// pi 0.74-0.75 creates session.jsonl lazily — only on the first
	// persist *after* SendPrompt has been preflight-accepted. The
	// previous synchronous pre-SendPrompt get_state call returned
	// SessionFile="" and we never re-queried, leaving daemon_runs
	// .session_path empty for the life of the run (and the lazy
	// Detail pane stuck on "(no transcript events yet)"). Polling in
	// the background lets us record the path as soon as pi stamps
	// it, without blocking the turn loop — the executor proceeds
	// into WaitForAgentEnd immediately after this go.
	//
	// Gated on the pi-runner branch: agentnode.Runner.GetState
	// returns an empty SessionInfo unconditionally (custom Node
	// runners don't have a pi session at all), so polling them is
	// pure waste — a fresh goroutine burning the full budget on no-op
	// RPCs for every Node-runner job. Mirrors the agentCfg.Runner
	// switch that picked the runner type above.
	if agentCfg.Runner == "" {
		go pollSessionInfo(ctx, runner, e.deps.Runs, jobID, pollSessionOpts{
			Budget: e.cfg.SessionPollBudget,
		})
	}

	// 6. Turn loop: WaitForAgentEnd, then check step_signals; kickback on miss.
	//
	// While at least one client is attached we disable both the idle
	// timeout and the kickback consumption: an operator typing into the
	// attach TUI is the authoritative driver, and we don't want the
	// executor to either time them out or burn correction budget on
	// turn boundaries that aren't terminal.
	var signaled step.Emitted
	for {
		attached := e.deps.Attachments != nil && e.deps.Attachments.Attached(jobID)
		var (
			turnCtx    context.Context
			turnCancel context.CancelFunc
		)
		if attached {
			turnCtx, turnCancel = context.WithCancel(ctx)
		} else {
			turnCtx, turnCancel = context.WithTimeout(ctx, e.cfg.IdleTimeout)
		}
		werr := runner.WaitForAgentEnd(turnCtx)
		turnCancel()
		if werr != nil {
			// Defensive: if the agent recorded its transition before
			// WaitForAgentEnd returned an error, honour the signal
			// rather than parking the task. Without this, a successful
			// agent run can still end up in human_feedback. This guard
			// covers all non-cancel error sources from WaitForAgentEnd:
			//
			//   - read-loop errors: pi pipe died, unexpected EOF,
			//     extension-RPC payload malformed, etc. (the original
			//     bufio.ErrTooLong regression that motivated the guard).
			//   - turnCtx.DeadlineExceeded: the unattached IdleTimeout
			//     fired between the agent's step_signal write and the
			//     agent_end event landing on the reader. We deliberately
			//     advance in this case too: the agent's recorded
			//     transition is the source of truth, even when the
			//     subsequent shutdown stalled long enough to trip the
			//     idle timer. Pinned by
			//     TestRun_IdleTimeoutWithSignal_HonoursSignal.
			//
			// We skip the lookup when the executor itself is cancelled
			// (ctx.Err() != nil, or werr unwraps to context.Canceled)
			// — cancellation routes through handleRunError's
			// cancel-aware cleanup and explicitly does NOT advance.
			if ctx.Err() == nil && !errors.Is(werr, context.Canceled) {
				if sig, gerr := e.deps.Signals.GetForRun(ctx, jobID); gerr == nil {
					// Leave a forensic breadcrumb: the run records
					// status=done from here on, so the original werr
					// would otherwise be silently swallowed. The slog
					// line is the only signal an operator gets that the
					// pipe / idle-timer misbehaved on this turn.
					slog.Default().Warn("executor: WaitForAgentEnd errored but step_signal already recorded; honoring signal",
						"job", jobID,
						"task", run.TaskID,
						"err", werr.Error())
					signaled = sig
					break
				}
			}
			return e.handleRunError(ctx, bg, jobID, runner, werr)
		}
		sig, gerr := e.deps.Signals.GetForRun(ctx, jobID)
		if gerr == nil {
			signaled = sig
			break
		}
		if !errors.Is(gerr, step.ErrNoActiveRun) {
			return e.handleRunError(ctx, bg, jobID, runner, gerr)
		}
		// While attached, a missing signal at agent_end is NOT a closure
		// miss — it just means the operator is mid-conversation. Loop
		// back to WaitForAgentEnd without burning a correction.
		if e.deps.Attachments != nil && e.deps.Attachments.Attached(jobID) {
			continue
		}
		// Invalid closure: kick back if budget remains, else fail.
		if run.CorrectionsUsed >= run.MaxCorrections {
			_ = runner.Abort(ctx)
			return e.failTerminal(bg, jobID, nil, ErrAgentDidNotEmit)
		}
		used, ierr := e.deps.Runs.IncCorrections(ctx, jobID)
		if ierr != nil {
			return e.handleRunError(ctx, bg, jobID, runner, ierr)
		}
		run.CorrectionsUsed = used
		msg := CorrectiveMessage(run.TaskID, stepRow, used, run.MaxCorrections)
		if err := runner.SendPrompt(ctx, msg); err != nil {
			return e.handleRunError(ctx, bg, jobID, runner, err)
		}
	}

	// 7. Clean shutdown of pi.
	exit, werr := e.shutdown(runner, ctx, bg)
	if werr != nil && !errors.Is(werr, context.Canceled) {
		return e.failTerminal(bg, jobID, &exit, fmt.Errorf("pi exit: %w", werr))
	}

	// 7.5 Honour operator-initiated task cancellation: if the task
	// was cancelled while the agent was still running, let the job
	// finish gracefully but discard any transition other than human.
	tk, gerr := e.deps.Tasks.GetTask(bg, run.TaskID)
	if gerr == nil && tk.Status == store.StatusCancel {
		if signaled.TaskStatus != "human" {
			if _, err := e.deps.Runs.MarkDone(bg, jobID, exit, nil); err != nil {
				return fmt.Errorf("mark done after task cancel: %w", err)
			}
			return nil
		}
		// signaled.TaskStatus == "human" falls through to advanceTask.
	}

	// 8. Advance the task atomically + persist transition_id.
	if err := e.advanceTask(bg, run.TaskID, signaled); err != nil {
		// MaxVisitsExceededError already carries the documented
		// `step_max_visits_exceeded: ...` form; surface it verbatim so
		// `daemon_runs.error` matches docs/workflows.md exactly. The
		// error may be wrapped in advanceTargetError; errors.As walks
		// the Unwrap chain, so this still fires for both forms.
		var mve workflow.MaxVisitsExceededError
		if errors.As(err, &mve) {
			return e.failTerminal(bg, jobID, &exit, err)
		}
		return e.failTerminal(bg, jobID, &exit, fmt.Errorf("advance task: %w", err))
	}
	tid := signaled.TransitionID
	if _, err := e.deps.Runs.MarkDone(bg, jobID, exit, &tid); err != nil {
		return fmt.Errorf("mark done: %w", err)
	}
	// On terminal transitions (done/cancel) for isolated workflows,
	// reap the per-task worktree. Cleanup failures are surfaced as a
	// warning + agent comment so a stuck `git worktree remove` never
	// masks a successful run. human / sibling-step transitions
	// preserve the worktree (the human / next step may need it).
	if wf.Isolation == workflow.IsolationWorktree && isTerminalStatus(signaled.TaskStatus) {
		e.cleanupWorktreeBestEffort(bg, run.TaskID, signaled.TaskStatus)
	}
	return nil
}

// isTerminalStatus reports whether the emitted task_status closes the
// task and so warrants worktree cleanup. human is NOT terminal
// — the human may inspect / resume from the worktree.
func isTerminalStatus(status string) bool {
	return status == "done" || status == "cancel"
}

// cleanupWorktreeBestEffort removes the per-task worktree directory
// after a successful terminal transition. Errors are logged via an
// agent comment authored as `human` so the failure survives past the
// run — but never propagated, because the task is already terminally
// closed and the agent did its job.
//
// The call is bounded by worktreeCleanupTimeout so a hung
// `git worktree remove` (NFS, dead lock file, broken FS) can't block
// the executor's worker slot indefinitely. The executor swaps the
// turn ctx for context.Background() during the terminal phase, so
// daemon shutdown cannot cancel us either — a hard local timeout is
// the only backstop.
//
// The breadcrumb-comment path runs on a SEPARATE timeout off the
// parent ctx, not the OnTerminal one: if OnTerminal blocked until
// its deadline expired, that's exactly the case the breadcrumb is
// meant to capture and we don't want the comment write to inherit
// the already-expired context.
func (e *Executor) cleanupWorktreeBestEffort(parentCtx context.Context, taskID, status string) {
	cleanupCtx, cleanupCancel := context.WithTimeout(parentCtx, worktreeCleanupTimeout)
	defer cleanupCancel()
	_, werr := e.deps.Worktree.OnTerminal(cleanupCtx, e.cfg.ProjectRoot, taskID)
	if werr == nil {
		return
	}
	if e.deps.Comments == nil || e.deps.Agents == nil {
		return
	}
	// Fresh ctx off parentCtx so the breadcrumb survives a
	// deadline-exceeded OnTerminal. Tight bound (5s) keeps a wedged
	// agents / comments store from pinning the worker slot.
	crumbCtx, crumbCancel := context.WithTimeout(parentCtx, worktreeBreadcrumbTimeout)
	defer crumbCancel()
	humanAg, gerr := e.deps.Agents.GetByName(crumbCtx, agent.HumanAgentName)
	if gerr != nil {
		return
	}
	_, _ = e.deps.Comments.Add(crumbCtx, taskID, humanAg.ID,
		fmt.Sprintf("worktree cleanup failed on %s: %v", status, werr))
}

// worktreeCleanupTimeout bounds a single cleanup invocation. 30s is
// generous — `git worktree remove --force` on a non-trivial worktree
// can run a few seconds, but past half a minute we'd rather fail and
// surface the breadcrumb than pin a worker slot.
const worktreeCleanupTimeout = 30 * time.Second

// worktreeBreadcrumbTimeout bounds the breadcrumb-comment write that
// runs after a cleanup failure. Kept small so a wedged store can't
// pin the worker slot for another full cleanup-budget on top of the
// OnTerminal timeout.
const worktreeBreadcrumbTimeout = 5 * time.Second

// advanceTask applies the transition's effect to tasks per §5.4.
//
//   - next_step_id   → current_step_id = next; status = work.
//     The visit counter is bumped + cap enforced via
//     workflow.EnterStep; a max-visits exceedance
//     surfaces as MaxVisitsExceededError, which the
//     caller maps to a failed run + parked task.
//   - human → current_step_id preserved (resume rewinds here);
//     status = human.
//   - done|cancel → current_step_id = NULL; status flipped.
func (e *Executor) advanceTask(ctx context.Context, taskID string, sig step.Emitted) error {
	switch {
	case sig.NextStepName != "":
		tk, err := e.deps.Tasks.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		if tk.WorkflowID == "" {
			return fmt.Errorf("task %s has no workflow_id", taskID)
		}
		st, err := e.deps.Workflows.FindStepByName(ctx, tk.WorkflowID, sig.NextStepName)
		if err != nil {
			return fmt.Errorf("find next step %q: %w", sig.NextStepName, err)
		}
		// EnterStep enforces step.max_visits + bumps step_visits +
		// updates the task pointer atomically. On cap-exceeded the
		// returned MaxVisitsExceededError propagates up to Run, where
		// the deferred failure path persists it as the run's error and
		// parks the task. Any error here is wrapped in advanceTargetError
		// so parkTaskOnFailure knows which step the run was aiming for
		// — the task is parked on the TARGET step (the run's intent),
		// not the SOURCE step (already finished). `autosk resume <id>`
		// without --to then lands on the right step once visits are
		// reset.
		if err := workflow.EnterStep(ctx, e.deps.Tasks, e.deps.Workflows, workflow.EnterStepInput{
			TaskID: taskID,
			StepID: st.ID,
		}); err != nil {
			return &advanceTargetError{TargetStepID: st.ID, Err: err}
		}
		return nil
	case sig.TaskStatus == "human":
		status := store.StatusHuman
		if _, err := e.deps.Tasks.UpdateTask(ctx, taskID, store.TaskPatch{Status: &status}); err != nil {
			return err
		}
		return nil
	case sig.TaskStatus == "done":
		status := store.StatusDone
		empty := ""
		if _, err := e.deps.Tasks.UpdateTask(ctx, taskID, store.TaskPatch{Status: &status, CurrentStepID: &empty}); err != nil {
			return err
		}
		return nil
	case sig.TaskStatus == "cancel":
		status := store.StatusCancel
		empty := ""
		if _, err := e.deps.Tasks.UpdateTask(ctx, taskID, store.TaskPatch{Status: &status, CurrentStepID: &empty}); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("invalid transition signal (neither sibling nor recognised task_status): %+v", sig)
	}
}

// applyAgentParamOverrides merges per-step AgentParams overrides on top
// of the agent package's resolved PackageConfig. Per docs/workflows.md:
//
//   - Scalar fields (`model`, `thinking`, `first_message`) are replaced
//     when the params block sets them (including to the empty string).
//   - Array fields (`extra_args`, `pi_extensions`, `pi_skills`) are
//     replaced wholesale when the params block carries a non-nil slice.
//     `pi_extensions` and `pi_skills` paths are interpreted as absolute
//     paths because we have no notion of a package install dir for
//     workflow-level overrides; callers should supply absolute paths.
//
// Custom (runner-based) packages cannot be overridden because their
// fields don't apply to the Node bootstrapper. We reject any non-zero
// params with a clear error rather than silently dropping the overrides.
func applyAgentParamOverrides(cfg *pkgregistry.PackageConfig, p *workflow.AgentParams) error {
	if p.IsZero() {
		return nil
	}
	if cfg.Runner != "" {
		return fmt.Errorf("step's agent.params cannot override custom-runner package %q (the runner code path ignores standard fields)", cfg.Name)
	}
	if p.Model != nil {
		cfg.Model = *p.Model
	}
	if p.Thinking != nil {
		cfg.Thinking = *p.Thinking
	}
	if p.FirstMessage != nil {
		cfg.FirstMessage = *p.FirstMessage
	}
	if p.ExtraArgs != nil {
		cfg.ExtraArgs = append([]string(nil), p.ExtraArgs...)
	}
	if p.PiExtensions != nil {
		cfg.PiExtensions = append([]string(nil), p.PiExtensions...)
	}
	if p.PiSkills != nil {
		cfg.PiSkills = append([]string(nil), p.PiSkills...)
	}
	return nil
}

// ---- prompt rendering ----------------------------------------------------

// buildPiExtraArgs translates a resolved package config into the slice
// of CLI flags appended after the daemon-managed flags (--model,
// --thinking, --session-dir). pi_extensions / pi_skills are appended
// as `-e <path>` / `--skill <path>` pairs, matching pi's own CLI.
func buildPiExtraArgs(cfg pkgregistry.PackageConfig) []string {
	out := make([]string, 0, len(cfg.ExtraArgs)+2*len(cfg.PiExtensions)+2*len(cfg.PiSkills))
	out = append(out, cfg.ExtraArgs...)
	for _, ext := range cfg.PiExtensions {
		out = append(out, "-e", ext)
	}
	for _, sk := range cfg.PiSkills {
		out = append(out, "--skill", sk)
	}
	return out
}

// RenderPrompt builds the user-facing prompt sent to pi at the start of a
// step run. Public for unit tests / golden snapshots.
func RenderPrompt(
	t store.Task,
	wf workflow.Workflow,
	stepRow workflow.Step,
	cfg pkgregistry.PackageConfig,
	commentLines []string,
) string {
	var sb strings.Builder
	if cfg.FirstMessage != "" {
		sb.WriteString(strings.TrimRight(cfg.FirstMessage, "\n"))
		sb.WriteString("\n\n")
	}
	fmt.Fprintf(&sb, "You are agent %q on step %q of workflow %q.\n",
		stepRow.AgentName, stepRow.Name, wf.Name)
	fmt.Fprintf(&sb, "Task: %s\n", t.ID)
	if t.Title != "" {
		fmt.Fprintf(&sb, "Title: %s\n", t.Title)
	}
	if t.Description != "" {
		sb.WriteString("\nDescription:\n")
		sb.WriteString(t.Description)
		sb.WriteString("\n")
	}
	sb.WriteString("\nAvailable transitions (pick exactly one before you stop):\n")
	for _, tr := range stepRow.Transitions {
		switch {
		case tr.IsTaskStatus():
			fmt.Fprintf(&sb, "  - task_status=%s — %s\n", tr.TaskStatus, tr.PromptRule)
		default:
			fmt.Fprintf(&sb, "  - step=%s — %s\n", tr.NextStepName, tr.PromptRule)
		}
	}
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "When you have decided, call: `autosk step next %s --to <name>` (sibling step name OR done|cancel|human).\n", t.ID)
	sb.WriteString("Do not stop before issuing exactly one such call.\n")
	if len(commentLines) > 0 {
		sb.WriteString("\nComments (oldest first):\n")
		for _, line := range commentLines {
			sb.WriteString("  ")
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	} else {
		sb.WriteString("\nNo comments on this task yet.\n")
	}
	return sb.String()
}

// CorrectiveMessage is the kickback message sent when the agent ends a turn
// without emitting `step next`. Plan §6.1.2 (adapted for v0.2).
func CorrectiveMessage(taskID string, stepRow workflow.Step, attempt, max int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "You stopped without recording a transition on task %s.\n", taskID)
	sb.WriteString("Before you stop you MUST call `autosk step next` exactly once with one of:\n")
	for _, tr := range stepRow.Transitions {
		if tr.IsTaskStatus() {
			fmt.Fprintf(&sb, "  - autosk step next %s --to %s\n", taskID, tr.TaskStatus)
		} else {
			fmt.Fprintf(&sb, "  - autosk step next %s --to %s\n", taskID, tr.NextStepName)
		}
	}
	fmt.Fprintf(&sb, "This is correction attempt %d of %d. If you ignore it the run will be marked failed.\n", attempt, max)
	return sb.String()
}

// ---- error / shutdown plumbing -------------------------------------------

func (e *Executor) handleRunError(ctx, bg context.Context, jobID string, runner PiRunner, runErr error) error {
	if errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		_ = runner.Abort(bg)
		_ = runner.CloseStdin()
		_ = runner.Terminate()
		exit, _ := runner.Wait(bg, e.cfg.Grace)
		if exit < 0 && runner.PID() > 0 {
			_ = runner.Kill()
			exit, _ = runner.Wait(bg, e.cfg.Grace)
		}
		_, _ = e.deps.Runs.MarkCancelled(bg, jobID, &exit)
		// Cancellation: do NOT park the task. The caller asked to stop;
		// they will decide whether to resume or cancel the task itself.
		return runErr
	}
	exit, _ := e.shutdown(runner, ctx, bg)
	_, _ = e.deps.Runs.MarkFailed(bg, jobID, &exit, runErr.Error())
	e.parkTaskOnFailure(bg, jobID, runErr)
	return runErr
}

func (e *Executor) shutdown(runner PiRunner, ctx, bg context.Context) (int, error) {
	_ = runner.CloseStdin()
	exit, err := runner.Wait(bg, e.cfg.Grace)
	if pi.IsWaitTimeout(err) {
		_ = runner.Terminate()
		exit, err = runner.Wait(bg, e.cfg.Grace)
		if pi.IsWaitTimeout(err) {
			_ = runner.Kill()
			exit, err = runner.Wait(bg, e.cfg.Grace)
		}
	}
	return exit, err
}

func (e *Executor) failTerminal(ctx context.Context, jobID string, exit *int, cause error) error {
	_, _ = e.deps.Runs.MarkFailed(ctx, jobID, exit, cause.Error())
	e.parkTaskOnFailure(ctx, jobID, cause)
	return cause
}

// parkTaskOnFailure moves the run's task into `human` so the
// poller stops re-picking it. Without this, a permanent failure (e.g.
// agent_config_invalid, pi binary missing) spams daemon_runs forever
// because the task stays in work with no queued/running row.
//
// Step-pointer policy:
//
//   - advanceTask failures (the only place we know the run's intent)
//     are surfaced as advanceTargetError; the task is parked on the
//     TARGET step. This way `autosk resume <id>` without --to lands on
//     the step the run was trying to enter (e.g. on a
//     step_max_visits_exceeded park: source step is already done, so
//     re-running it would burn tokens).
//   - Every other failure (pi crash, runner timeout, SendPrompt error,
//     shutdown error, agent_did_not_emit, etc.) leaves current_step_id
//     untouched — the run died before emitting `step next`, so the
//     target is unknown and the only safe landing is the source step.
//
// The failure reason is in daemon_runs.error (visible via
// `daemon list` / HTTP API).
//
// Best-effort: if Tasks/Runs lookups fail here we swallow the error
// since we're already on a failure path.
func (e *Executor) parkTaskOnFailure(ctx context.Context, jobID string, cause error) {
	run, err := e.deps.Runs.GetRun(ctx, jobID)
	if err != nil || run.TaskID == "" {
		return
	}
	tk, err := e.deps.Tasks.GetTask(ctx, run.TaskID)
	if err != nil {
		return
	}
	// Only park tasks that are still work. If a human raced us
	// (e.g. typed `autosk done` while the executor was failing), leave
	// their terminal status intact.
	if tk.Status != store.StatusWork {
		return
	}
	parked := store.StatusHuman
	patch := store.TaskPatch{Status: &parked}
	var ate *advanceTargetError
	if errors.As(cause, &ate) && ate.TargetStepID != "" {
		patch.CurrentStepID = &ate.TargetStepID
	}
	_, _ = e.deps.Tasks.UpdateTask(ctx, run.TaskID, patch)
}

func (e *Executor) cleanup(runner PiRunner, bg context.Context) {
	go func() {
		_ = runner.CloseStdin()
		_, _ = runner.Wait(bg, e.cfg.Grace)
	}()
}

// sessionDirFor returns the directory passed as `--session-dir` to pi.
// One per job so transcripts are easy to find.
func (e *Executor) sessionDirFor(run runstore.Run) string {
	root := e.cfg.SessionDirRoot
	if root == "" {
		root = filepath.Join(e.cfg.ProjectRoot, ".autosk", "sessions")
	}
	return filepath.Join(root, run.JobID)
}
