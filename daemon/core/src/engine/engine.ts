/**
 * The engine (plan §3.7(3-4)): the single scheduler loop + global worker pool,
 * session lifecycle, `ctx.transit` commit path, the per-session MCP server, the
 * transcript writer, steer/followup/abort routing, and crash recovery.
 *
 * Isolation is NOT here: the `IsolationProvider` abstraction was abolished, so
 * the engine never acquires/releases/reaps an env — agents own isolation via
 * `@autosk/sandbox` and tear it down with a cleanup workflow step.
 *
 * It is RPC-independent — the daemon (P5) constructs it once, feeds it projects
 * via {@link addProject}, and drives it through {@link enroll} / {@link resume} /
 * {@link sessionInput} / {@link sessionAbort}; tests drive the exact same API.
 *
 * Scheduling is event-driven, not polled (plan §3.7(3)): a transit / enroll /
 * resume / finished session kicks a coalesced scan that finds every schedulable
 * task (status=work, an agent step, not blocked, no live session), claims it by
 * creating a `queued` session, and enqueues it onto the FIFO pool capped at
 * `--workers`. Scans never overlap (a guard flag), so the "no live session"
 * check plus session-creation-as-claim guarantee one session per task.
 */

import {
  isStatusStep,
  newSessionId,
  type SessionMeta,
  type StepTarget,
  type TaskView,
  type TranscriptEntry,
  type TranscriptMessage,
  type WorkflowDefinition,
} from "@autosk/sdk";

import { systemClock, type Clock } from "../store/clock.ts";
import { consoleLogger, type Logger } from "../store/logger.ts";
import { TranscriptWriter } from "./transcript.ts";
import { buildTransitContext, positionFor, validateTarget } from "./transition.ts";
import { SessionRuntime } from "./session.ts";
import {
  ENGINE_COMMENT_AUTHOR,
  EngineError,
  errMsg,
  type EngineEvent,
  type EngineListener,
  type EngineProject,
  type SessionHost,
} from "./types.ts";

/** Default size of the global FIFO worker pool (plan §3.7(3)). */
export const DEFAULT_WORKERS = 4;

/**
 * Default period of the safety rescan (plan §3.7(3)): the net for schedulability
 * changes that emit NO engine event (e.g. a task unblocked or enrolled by an
 * external edit to a file the daemon did not author). Scheduling stays
 * event-driven; this is a slow backstop, not a poll loop.
 */
export const DEFAULT_RESCAN_MS = 30_000;

export interface EngineOptions {
  /** Global worker-pool size (shared across all projects). Defaults to {@link DEFAULT_WORKERS}. */
  workers?: number;
  /** Safety-rescan period in ms (0 disables). Defaults to {@link DEFAULT_RESCAN_MS}. */
  rescanMs?: number;
  clock?: Clock;
  logger?: Logger;
}

/** What `enroll` accepts: a named workflow and an optional starting step
 *  (defaults to the workflow's first step). */
export type EnrollTarget = { workflow: string; step?: string };

export class Engine implements SessionHost {
  readonly clock: Clock;
  readonly logger: Logger;
  private readonly workers: number;

  private readonly projects = new Map<string, EngineProject>();
  /** Live session runtimes keyed by session id (for input/abort routing). */
  private readonly running = new Map<string, SessionRuntime>();
  private readonly listeners = new Set<EngineListener>();

  /** The FIFO of claimed-but-not-yet-running session runtimes. */
  private queue: SessionRuntime[] = [];
  /** Currently-executing worker count (≤ workers). */
  private active = 0;

  private stopped = false;
  private scanning = false;
  private scanRequested = false;

  private readonly rescanMs: number;
  private rescanTimer: ReturnType<typeof setInterval> | null = null;

  constructor(opts: EngineOptions = {}) {
    this.clock = opts.clock ?? systemClock;
    this.logger = opts.logger ?? consoleLogger;
    this.workers = Math.max(1, opts.workers ?? DEFAULT_WORKERS);
    this.rescanMs = opts.rescanMs ?? DEFAULT_RESCAN_MS;
    this.ensureRescanTimer();
  }

  // -- project membership --------------------------------------------------

  /**
   * Adds a project to the engine: runs crash recovery (interrupted sessions →
   * `failed: daemon_restart`, their tasks → `human`) then kicks a scan so any
   * already-`work` task is dispatched. Idempotent per root.
   */
  async addProject(project: EngineProject): Promise<void> {
    if (this.projects.has(project.root)) return;
    this.projects.set(project.root, project);
    await this.recoverProject(project);
    this.kickScan();
  }

  /** Currently-registered project roots. */
  projectRoots(): string[] {
    return [...this.projects.keys()];
  }

  /**
   * Atomically swaps the extension registry the engine schedules a project over
   * (extension hot-reload). The {@link EngineProject.registry} field is read
   * FRESH on every dispatch/enroll/resume/interactive-create, so a single
   * synchronous reassignment makes every NEW scheduling decision see the new
   * set; a {@link SessionRuntime} already running keeps the workflow/agent object
   * it captured at construction and finishes undisturbed. Kicks a scan so a
   * freshly-available workflow picks up any `work`/ready task waiting for it.
   *
   * Returns whether the swap landed: `true` when the root was engine-registered
   * and its registry was replaced, `false` (no-op) when the root is unknown to
   * the engine. The caller gates its "reloaded" reporting on this so the reported
   * state can never diverge from the engine's actual scheduling registry.
   */
  setProjectRegistry(root: string, registry: EngineProject["registry"]): boolean {
    const project = this.projects.get(root);
    if (!project) return false;
    project.registry = registry;
    this.kickScan();
    return true;
  }

  // -- lifecycle -----------------------------------------------------------

  /** (Re)enables scheduling, restarts the safety rescan, and kicks a scan. */
  start(): void {
    this.stopped = false;
    this.ensureRescanTimer();
    this.kickScan();
  }

  /**
   * Halts scheduling and detaches every live session WITHOUT writing terminal
   * state (deviation: simulates a daemon stop/crash). The dropped queue + the
   * `queued`/`running` metas left on disk are reaped by {@link recoverProject} —
   * which runs from {@link addProject} (i.e. a fresh process re-registering its
   * projects), NOT from {@link start}. A `stop()`→`start()` on the SAME instance
   * (without re-adding projects) does not recover them, so P5's idle-shutdown
   * must re-add projects rather than merely restart. Detaching fires each
   * session's `AbortSignal` so a well-behaved agent unblocks promptly.
   */
  stop(): void {
    this.stopped = true;
    this.clearRescanTimer();
    this.queue = [];
    for (const rt of this.running.values()) rt.detach();
  }

  /** Starts the periodic safety rescan (idempotent; no-op when disabled). */
  private ensureRescanTimer(): void {
    if (this.rescanTimer !== null || this.rescanMs <= 0) return;
    this.rescanTimer = setInterval(() => this.kickScan(), this.rescanMs);
    // Never keep the process (or a test runner) alive just for the backstop.
    this.rescanTimer.unref?.();
  }

  /** Stops the periodic safety rescan (idempotent). */
  private clearRescanTimer(): void {
    if (this.rescanTimer === null) return;
    clearInterval(this.rescanTimer);
    this.rescanTimer = null;
  }

  // -- subscriptions -------------------------------------------------------

  on(listener: EngineListener): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  off(listener: EngineListener): void {
    this.listeners.delete(listener);
  }

  /** Pool stats for `meta.healthz` (P5). */
  stats(): { workers: number; queued: number; running: number } {
    return { workers: this.workers, queued: this.queue.length, running: this.active };
  }

  // -- enroll / resume (client-driven transitions) -------------------------

  /**
   * Enrolls a non-`work` task into a workflow (`{ workflow, step? }`),
   * transitioning it to `step` (default: the workflow's first step). Allowed
   * from `new`, `cancel`, and `human` — `work` (a live run) and `done` (use
   * `reopen`) are rejected. Runs `onTransit` for the entry edge (a throw rejects
   * the enroll). When re-enrolling a task that is already on THIS workflow the
   * step it sits at is the `onTransit` leaving step (so `onTransit` sees the
   * continued edge, matching `resume`); a fresh enroll (or a switch to another
   * workflow) leaves the empty pre-enrolment step `""`.
   */
  async enroll(root: string, taskId: string, target: EnrollTarget): Promise<TaskView> {
    const project = this.requireProject(root);
    const wf = project.registry.resolveWorkflow(target.workflow);
    if (!wf) throw EngineError.invalidParams(`unknown workflow: ${target.workflow}`);
    const workflowName = target.workflow;

    const view = await this.requireTask(project, taskId);
    if (view.status !== "new" && view.status !== "cancel" && view.status !== "human") {
      throw EngineError.conflict(
        `cannot enroll ${taskId}: status is ${view.status} (expected new, cancel, or human)`,
      );
    }

    const to: StepTarget = { step: target.step ?? wf.firstStep };
    const leavingStep = view.workflow === workflowName ? (view.step ?? "") : "";
    await this.runOnTransit(project, wf, taskId, workflowName, leavingStep, to);
    // `to` is always a `{ step }` here, so entering it counts a visit (atomically
    // with the position write); `onTransit` already ran, so the bump is post-read.
    const updated = await project.store.setPosition(taskId, positionFor(wf, leavingStep, to), {
      countVisit: to.step,
    });
    this.emitTask(project, updated);
    this.kickScan();
    return updated;
  }

  /**
   * Re-enters a parked (`human`) or closed (`cancel`) task into its workflow.
   * Defaults to the same step the task was parked at; `to` overrides the target.
   * Runs `onTransit`. The task must already be enrolled (have a workflow + step).
   */
  async resume(root: string, taskId: string, to?: StepTarget): Promise<TaskView> {
    const project = this.requireProject(root);
    const view = await this.requireTask(project, taskId);
    if (view.status !== "human" && view.status !== "cancel") {
      throw EngineError.conflict(
        `cannot resume ${taskId}: status is ${view.status} (expected human or cancel)`,
      );
    }
    if (view.workflow === null || view.step === null) {
      throw EngineError.conflict(`cannot resume ${taskId}: task is not enrolled`);
    }
    const wf = project.registry.resolveWorkflow(view.workflow);
    if (!wf) {
      throw EngineError.conflict(`cannot resume ${taskId}: workflow_missing: ${view.workflow}`);
    }
    const target: StepTarget = to ?? { step: view.step };
    // `runOnTransit` validates the target before invoking `onTransit`, so no
    // separate `validateTarget` call is needed here (ISSUE #9c).
    await this.runOnTransit(project, wf, taskId, view.workflow, view.step, target);
    // Only a `{ step }` resume target counts a visit (entering a named step);
    // a `{ status }` resume flips status without re-entering a step.
    const updated = await project.store.setPosition(taskId, positionFor(wf, view.step, target), {
      countVisit: "step" in target ? target.step : undefined,
    });
    this.emitTask(project, updated);
    this.kickScan();
    return updated;
  }

  // -- session input / abort ----------------------------------------------

  /** Routes a steer/followup into a live session (plan §3.4). */
  async sessionInput(
    root: string,
    sessionId: string,
    input: { message: string; kind: "steer" | "followup" },
  ): Promise<{ handled: boolean }> {
    const rt = this.requireSession(root, sessionId);
    return rt.input(input.kind, input.message);
  }

  /** Aborts a live session: fires its signal + `onAbort`, marks it aborted (plan §3.4). */
  async sessionAbort(root: string, sessionId: string): Promise<{ handled: boolean }> {
    const rt = this.requireSession(root, sessionId);
    return rt.abort();
  }

  // -- interactive (taskless) sessions -------------------------------------

  /**
   * Creates + dispatches an interactive (taskless) chat session for a registered
   * agent (plan §4.2). Unknown agent → {@link EngineError.invalidParams}. The
   * session is `kind:"interactive"` with empty `task_id`/`workflow`/`step`, runs
   * at the project root, and is dispatched DIRECTLY here — the task scanner is
   * never involved (it has no task to claim).
   *
   * Crucially the run is dispatched OFF the bounded task-worker pool (it does NOT
   * go through `queue`+`pump`+`this.active`). A chat's `onRun` blocks on the
   * abort signal for the WHOLE session lifetime — idle between turns and during
   * turns (composer turns arrive out-of-band via `onFollowup`/`driver.input`,
   * they do not unblock `onRun`). Occupying a `this.active` slot for that whole
   * time would starve task dispatch: the pool is global across projects and
   * capped at `--workers` (default 4), so a handful of idle chats would deadlock
   * every project's task scheduler. Returns the
   * freshly-created `queued` meta; the queued→running flip and the terminal
   * settle emit their own session-events.
   */
  async createInteractiveSession(root: string, agentName: string): Promise<SessionMeta> {
    const project = this.requireProject(root);
    const agent = project.registry.resolveAgent(agentName);
    if (!agent) throw EngineError.invalidParams(`unknown agent: ${agentName}`);

    const sessionId = newSessionId();
    const meta = await project.store.sessions.create({
      id: sessionId,
      kind: "interactive",
      task_id: "",
      workflow: "",
      step: "",
      agent: agentName,
      // An interactive session opens idle (empty, waiting for the first user
      // message); the chat agent flips it to busy/idle on each turn boundary.
      activity: "idle",
      cwd: project.root,
      timestamp: this.clock(),
    });
    // Announce the freshly-created (queued) interactive session on the project
    // session channel so subscribers see it appear immediately.
    this.emitSession(project, meta, "status");

    const runtime = new SessionRuntime({
      kind: "interactive",
      host: this,
      project,
      agent,
      agentName,
      sessionId,
    });
    this.running.set(sessionId, runtime);
    // Run directly, OFF the bounded task-worker pool (see the doc above): no
    // `queue.push` / `pump()` / `this.active` accounting, so an indefinitely-idle
    // chat never consumes a task-worker slot.
    void this.runInteractive(runtime);
    return meta;
  }

  /**
   * Drives an interactive runtime to completion off the bounded pool. Mirrors
   * {@link runJob} (de-register + kick a scan when it settles) MINUS the
   * `this.active`/`pump()` accounting, which interactive sessions deliberately
   * never participate in.
   */
  private async runInteractive(rt: SessionRuntime): Promise<void> {
    try {
      await rt.run();
    } catch (e) {
      this.logger.error(`session ${rt.id} crashed: ${errMsg(e)}`);
    } finally {
      this.running.delete(rt.id);
      this.kickScan();
    }
  }

  /**
   * Gracefully ends a live interactive session → status `done` (plan §4.2). A
   * `task` session cannot be ended this way (use `session.abort`); an unknown or
   * already-settled session is `not found`.
   */
  async sessionEnd(root: string, sessionId: string): Promise<{ handled: boolean }> {
    const rt = this.requireSession(root, sessionId);
    if (rt.kind !== "interactive") {
      throw EngineError.conflict(`cannot end ${sessionId}: not an interactive session`);
    }
    return rt.end();
  }

  // -- scheduler -----------------------------------------------------------

  /** Requests a coalesced scan (no-op while stopped). */
  kickScan(): void {
    if (this.stopped) return;
    this.scanRequested = true;
    void this.drainScans();
  }

  /** Drains scan requests one at a time — scans never overlap. */
  private async drainScans(): Promise<void> {
    if (this.scanning) return;
    this.scanning = true;
    try {
      while (this.scanRequested && !this.stopped) {
        this.scanRequested = false;
        await this.scanAll();
      }
    } catch (e) {
      this.logger.error(`scheduler scan failed: ${errMsg(e)}`);
    } finally {
      this.scanning = false;
    }
  }

  private async scanAll(): Promise<void> {
    for (const project of this.projects.values()) {
      if (this.stopped) return;
      await this.scanProject(project);
    }
    this.pump();
  }

  /** Finds schedulable tasks in one project and claims+enqueues a session for each. */
  private async scanProject(project: EngineProject): Promise<void> {
    const rows = await project.store.listSchedulable();
    for (const row of rows) {
      if (this.stopped) return;
      if (row.blocked) continue; // deviation #1: never dispatch a blocked task
      if (row.workflow === null || row.step === null) continue;
      if (project.store.sessions.hasLiveSession(row.id)) continue; // already running/queued
      await this.dispatch(project, row.id);
    }
  }

  /**
   * Claims a task for execution: re-read it FRESH, create the `queued` session
   * (the claim), build the runtime, and enqueue it onto the pool.
   *
   * The fresh re-read is load-bearing (BLOCKER #1): the enumerating scan's
   * snapshot can go stale across `await`s (a sibling transition may have advanced
   * THIS task and sealed its session while we were dispatching another), so the
   * snapshot is never trusted — workflow/step/agent are re-derived here and any
   * change (status≠work, a new live session, a different step) aborts the claim.
   */
  private async dispatch(project: EngineProject, taskId: string): Promise<void> {
    const row = await project.store.schedulingRow(taskId).catch(() => null);
    if (!row || row.status !== "work" || row.blocked) return;
    if (row.workflow === null || row.step === null) return;
    // Parking on a missing workflow/step here is always safe, for TWO reasons —
    // and a future reader must not move the `hasLiveSession` re-check (below) up
    // above these branches assuming only the first holds:
    //   (1) the enumerating scan pre-filters live tasks, so a `work` task
    //       normally reaches `dispatch` with no live session; and
    //   (2) a MISSING workflow means no path (dispatch/enroll/resume) can mint a
    //       NEW session for the task, so by the time `!wf`/`!step` is reached the
    //       task genuinely has none — even though the scan snapshot can go stale
    //       across awaits (which is exactly why the agent-dispatch path keeps its
    //       own `hasLiveSession` re-check further down).
    // This self-heals an orphan after its session settled: under extension
    // hot-reload there is no open-time hazard guard to re-run, so the scheduler
    // itself parks a task whose workflow/step was removed out from under it (the
    // removed workflow's live session, if any, already finished on its captured
    // object before the next scan reaches here).
    const wf = project.registry.resolveWorkflow(row.workflow);
    if (!wf) {
      await this.park(project, taskId, `workflow_missing: ${row.workflow}`);
      return;
    }
    const step = wf.steps[row.step];
    if (!step) {
      await this.park(project, taskId, `workflow_missing: ${row.workflow} has no step ${row.step}`);
      return;
    }
    if (isStatusStep(step)) return; // terminal/park step: never scheduled
    const agent = step; // an agent step IS the agent definition (inline)
    if (project.store.sessions.hasLiveSession(taskId)) return; // already running/queued

    const sessionId = newSessionId();
    const meta = await project.store.sessions.create({
      id: sessionId,
      task_id: taskId,
      workflow: row.workflow,
      step: row.step,
      agent: row.step, // the step key IS the agent name
      cwd: project.root, // the run dir is always the project root (isolation is agent-owned)
      timestamp: this.clock(),
    });
    // Announce the freshly-claimed (queued) session on the project session
    // channel so subscribers see it appear immediately, BEFORE a worker picks it
    // up. The queued→running flip and the terminal settle emit their own
    // session-events (so the project channel stays live across the lifecycle).
    this.emitSession(project, meta, "status");

    const runtime = new SessionRuntime({
      host: this,
      project,
      workflow: wf,
      workflowName: row.workflow,
      agent,
      agentName: row.step,
      sessionId,
      taskId,
      step: row.step,
    });
    this.running.set(sessionId, runtime);
    this.queue.push(runtime);
  }

  /** Starts as many queued sessions as the pool cap allows. */
  private pump(): void {
    while (!this.stopped && this.active < this.workers && this.queue.length > 0) {
      const rt = this.queue.shift()!;
      if (rt.isSettled()) {
        // Aborted (or otherwise finalised) while still queued — it already sealed
        // its own meta; drop it without occupying a worker slot (BLOCKER #2).
        this.running.delete(rt.id);
        continue;
      }
      this.active += 1;
      void this.runJob(rt);
    }
  }

  private async runJob(rt: SessionRuntime): Promise<void> {
    try {
      await rt.run();
    } catch (e) {
      this.logger.error(`session ${rt.id} crashed: ${errMsg(e)}`);
    } finally {
      this.running.delete(rt.id);
      this.active -= 1;
      this.pump();
      this.kickScan();
    }
  }

  // -- crash recovery ------------------------------------------------------

  /**
   * Reaps sessions interrupted by a daemon restart (plan §3.7(4)): any meta left
   * `running` OR `queued` (deviation #2: a queued meta is equally orphaned and
   * would deadlock the task) → `failed: daemon_restart`, and its `work` task →
   * `human`. No retention/GC — session files just accumulate.
   */
  private async recoverProject(project: EngineProject): Promise<void> {
    for (const meta of project.store.sessions.allMetas()) {
      if (meta.status !== "running" && meta.status !== "queued") continue;
      await project.store.sessions.patchMeta(meta.id, {
        status: "failed",
        error: "daemon_restart",
        ended_at: this.clock(),
        usage: null,
      });
      await this.appendRecoveryEntries(project, meta.id);
      // Interactive sessions have no task to park (v1: not auto-resumed after a
      // restart, plan §4.4/§8). Only a task session parks its `work` task.
      if (meta.kind === "interactive") continue;
      const view = await project.store.taskView(meta.task_id).catch(() => null);
      if (view && view.status === "work") {
        await this.park(project, meta.task_id, "daemon_restart");
      }
    }
  }

  /** Appends the structural `autosk:error` + `autosk:session_end` to a reaped transcript. */
  private async appendRecoveryEntries(project: EngineProject, sessionId: string): Promise<void> {
    const writer = new TranscriptWriter(project.store.sessions, sessionId, this.clock, this.logger);
    writer.error("daemon_restart");
    writer.sessionEnd("failed", "daemon_restart");
    await writer.flush();
  }

  // -- SessionHost ---------------------------------------------------------

  /** Parks a task to `human` with `error` (status flip keeps workflow/step + an `autosk` comment). */
  async park(project: EngineProject, taskId: string, error: string): Promise<void> {
    const view = await project.store.taskView(taskId).catch(() => null);
    if (!view) return;
    const updated = await project.store.setPosition(taskId, {
      status: "human",
      workflow: view.workflow,
      step: view.step,
    });
    await project.store.addComment(taskId, { author: ENGINE_COMMENT_AUTHOR, text: error });
    this.emitTask(project, updated);
  }

  /**
   * {@link SessionHost.enrollTask}: enrolls a (freshly-created) task into a
   * workflow on behalf of a per-session MCP server's `task create --workflow`.
   * Delegates to {@link enroll} (which validates + runs `onTransit`).
   */
  enrollTask(project: EngineProject, taskId: string, target: { workflow: string }): Promise<TaskView> {
    return this.enroll(project.root, taskId, target);
  }

  async notifyTaskChanged(project: EngineProject, taskId: string): Promise<void> {
    const view = await project.store.taskView(taskId).catch(() => null);
    if (view) this.emitTask(project, view);
  }

  emitSession(project: EngineProject, meta: SessionMeta, kind: "status" | "done" | "error"): void {
    const event: EngineEvent = { type: "session-event", root: project.root, session_id: meta.id, kind, meta };
    if (meta.error !== undefined) event.error = meta.error;
    this.emit(event);
  }

  emitSessionMessage(project: EngineProject, sessionId: string, entry: TranscriptEntry): void {
    this.emit({ type: "session-event", root: project.root, session_id: sessionId, kind: "message", entry });
  }

  emitSessionPartial(project: EngineProject, sessionId: string, message: TranscriptMessage): void {
    this.emit({ type: "session-event", root: project.root, session_id: sessionId, kind: "partial", partial: message });
  }

  // -- internals -----------------------------------------------------------

  private emitTask(project: EngineProject, task: TaskView): void {
    this.emit({ type: "task-changed", root: project.root, task });
  }

  private emit(event: EngineEvent): void {
    for (const listener of this.listeners) {
      try {
        listener(event);
      } catch (e) {
        this.logger.warn(`engine listener threw: ${errMsg(e)}`);
      }
    }
  }

  /** Shared `onTransit` invocation for enroll/resume (the session path has its own). */
  private async runOnTransit(
    project: EngineProject,
    wf: WorkflowDefinition,
    taskId: string,
    workflowName: string,
    leavingStep: string,
    to: StepTarget,
  ): Promise<void> {
    validateTarget(wf, to);
    if (!wf.onTransit) return;
    const tctx = buildTransitContext({
      store: project.store,
      taskId,
      workflow: workflowName,
      leavingStep,
      author: ENGINE_COMMENT_AUTHOR,
    });
    await wf.onTransit(tctx, to);
  }

  private requireProject(root: string): EngineProject {
    const project = this.projects.get(root);
    if (!project) throw EngineError.projectNotFound(`project not registered with engine: ${root}`);
    return project;
  }

  private async requireTask(project: EngineProject, taskId: string): Promise<TaskView> {
    try {
      return await project.store.taskView(taskId);
    } catch {
      throw EngineError.notFound(`task not found: ${taskId}`);
    }
  }

  private requireSession(root: string, sessionId: string): SessionRuntime {
    if (!this.projects.has(root)) {
      throw EngineError.projectNotFound(`project not registered with engine: ${root}`);
    }
    const rt = this.running.get(sessionId);
    // Cross-check the runtime actually belongs to the named project, not merely
    // that `root` is some registered project (ISSUE #6): a session id from project
    // A must not be routable under project B's root.
    if (!rt || rt.projectRoot !== root) {
      throw EngineError.notFound(`session not running: ${sessionId}`);
    }
    return rt;
  }
}
