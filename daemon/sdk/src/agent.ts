/**
 * Agent definitions and the run context the engine hands an agent (plan §3.4).
 *
 * Agents are *code* registered by extensions. `onRun` executes one full step
 * in-process and MUST call `ctx.transit(...)` exactly once before returning;
 * returning without a transit fails the session (`error="agent_did_not_transit"`)
 * and parks the task to `human`. Core has no pi knowledge — pi-based agents
 * ship as the `@autosk/pi-agent` extension on top of `ctx.spawn` + `ctx.transit`.
 */

import type { Comment, SessionActivity, TaskFilter, TaskView, WorkflowInfo } from "./types.ts";
import type { StepTarget } from "./workflow.ts";
import type { TranscriptMessage, Usage } from "./transcript.ts";

/**
 * An agent the engine can run for a step (plan §3.4).
 *
 * An agent is an inline step value: the workflow step key IS the agent name
 * (there is no separate `name` field and no separate agent registry). The
 * engine discriminates an agent step from a {@link StatusStep} structurally via
 * the presence of `onRun`.
 */
export interface AgentDefinition {
  /**
   * Runs one full step. MUST call `ctx.transit(...)` exactly once before
   * returning.
   */
  onRun(ctx: AgentRunContext): Promise<void>;
  /** Invoked when a client calls `session.input {kind:"steer"}` on a live session. */
  onSteer?(ctx: AgentRunContext, message: string): Promise<void>;
  /** Invoked when a client calls `session.input {kind:"followup"}` on a live session. */
  onFollowup?(ctx: AgentRunContext, message: string): Promise<void>;
  /** Invoked on `session.abort`. */
  onAbort?(ctx: AgentRunContext): Promise<void>;
}

/** Options for a one-shot child process via `ctx.exec` (plan §3.4). */
export interface ExecOptions {
  cwd?: string;
  env?: Record<string, string>;
  /** Data written to the child's stdin, then closed. */
  input?: string | Uint8Array;
  /** Aborts the child. Defaults to the session's `ctx.signal`. */
  signal?: AbortSignal;
  /** Kill the child after this many milliseconds. */
  timeoutMs?: number;
}

/** Result of a one-shot child process (plan §3.4). */
export interface ExecResult {
  code: number | null;
  stdout: string;
  stderr: string;
}

/** Options for a long-lived child process via `ctx.spawn` (plan §3.4). */
export interface SpawnOptions {
  cwd?: string;
  env?: Record<string, string>;
}

/**
 * A long-lived interactive child with stdio streaming (plan §3.4). This is how
 * the pi-agent extension drives `pi --mode rpc` over JSON-lines stdio.
 */
export interface ChildHandle {
  stdin: WritableStreamDefaultWriter<Uint8Array>;
  /** Line-buffered stdout. */
  onStdout(cb: (line: string) => void): void;
  /** Line-buffered stderr. */
  onStderr(cb: (line: string) => void): void;
  kill(signal?: string): void;
  exited: Promise<{ code: number | null }>;
}

/** Live task access for the running session (plan §3.4). */
export interface TasksAPI {
  /** The task this session runs for. */
  currentId: string;
  /** Re-reads the current task from the store. */
  current(): Promise<TaskView>;
  get(id: string): Promise<TaskView>;
  list(filter?: TaskFilter): Promise<TaskView[]>;
  /** Defaults to the current task. */
  comments(id?: string): Promise<Comment[]>;
}

/** Live registry access plus the session's current position (plan §3.4). */
export interface WorkflowsAPI {
  current: { workflow: string; step: string; targets: StepTarget[] };
  /** Rendered registry view (in-memory, synchronous). */
  get(name: string): WorkflowInfo | undefined;
  list(): WorkflowInfo[];
}

/** The pi-format transcript writer (plan §3.2, §3.4). */
export interface TranscriptAPI {
  /** Writes a pi message-schema entry. */
  message(message: TranscriptMessage): void;
  /**
   * Reports authoritative token usage for one completed provider turn.
   * Reports are aggregated into the session metadata. Pass `null` when the
   * provider completed a turn but did not supply trustworthy usage data.
   */
  usage(usage: Usage | null): void;
  /** Writes a `custom` entry — the generic agent logging channel. */
  custom(customType: string, data?: unknown): void;
}

/** The context the engine hands `onRun` / `onSteer` / `onFollowup` / `onAbort` (plan §3.4). */
export interface AgentRunContext {
  session: { id: string };
  /**
   * The run discriminator (plan §3.3):
   *  - `"task"` — a workflow step. The agent MUST call `ctx.transit(...)` exactly
   *    once before returning.
   *  - `"interactive"` — a taskless chat. `ctx.transit` is unavailable (it throws),
   *    and `ctx.tasks` / `ctx.workflows` are stub views (no real task/workflow).
   *    The agent runs a chat loop and returns when `ctx.signal` fires.
   */
  mode: "task" | "interactive";
  /**
   * The run directory. Isolation no longer rewrites this: it is **always the
   * project root** (identical to {@link projectRoot}). An agent that wants its
   * harness to run somewhere else (e.g. a per-task git worktree, or inside a
   * container) owns that itself — it resolves a workspace via `@autosk/sandbox`
   * and passes the chosen cwd to `ctx.spawn`.
   */
  cwd: string;
  /**
   * The canonical project root (the directory containing `.autosk/`). Identical
   * to {@link cwd} now that isolation is agent-owned (the engine never rewrites
   * the run dir). Retained as a distinct field so an agent that DOES run its
   * harness in a worktree/container still has an unambiguous handle on the
   * original project (e.g. to point an `autosk` CLI at it via `AUTOSK_CWD`).
   */
  projectRoot: string;
  /** Fired on abort / daemon shutdown. */
  signal: AbortSignal;

  tasks: TasksAPI;
  workflows: WorkflowsAPI;
  log: TranscriptAPI;

  /**
   * Streams a partial (in-progress) message snapshot to live subscribers.
   * EPHEMERAL: never written to the transcript, best-effort, superseded by the
   * next committed `log.message`. Cumulative — send the full current snapshot.
   *
   * Kept on `ctx` (a sibling of {@link log}), NOT inside {@link TranscriptAPI},
   * to make the "not durable" contract explicit: partials ride the same
   * per-session subscription as committed lines but carry no transcript line and
   * never advance the line cursor.
   */
  partial(message: TranscriptMessage): void;

  /** Shorthand: comment on the current task. */
  comment(text: string): Promise<void>;
  /** Validates via `workflow.onTransit`, then commits. A second call throws. */
  transit(to: StepTarget): Promise<void>;

  /**
   * Reports the agent's live turn activity: `"busy"` while streaming a turn,
   * `"idle"` when waiting for the next user message. The runtime persists it on
   * the session meta and pushes a `session-changed`/`session-event` so a client
   * can show idle vs working WITHOUT changing the lifecycle `status`. Scoped to
   * interactive (chat) sessions — a no-op in `"task"` mode.
   */
  setActivity(activity: SessionActivity): void;

  /** One-shot child process. */
  exec(cmd: string[], opts?: ExecOptions): Promise<ExecResult>;
  /** Long-lived interactive child with stdio streaming. */
  spawn(cmd: string[], opts?: SpawnOptions): ChildHandle;

  /**
   * Mints a per-session, host-side MCP server bound to this session's
   * `{ projectRoot, taskId, author = step, transit = task-mode }`. Returns the
   * loopback `url`, the bound `port`, and a bearer `token`; the agent rewrites
   * the host for its own isolation topology (e.g. `host.docker.internal` for a
   * container, `127.0.0.1` for host/worktree) using the `port`.
   *
   * This is about MCP *serving*, not isolation: it lets an agent hand its harness
   * a real `task` / `comment` tool surface (and, in task mode, an ack-only
   * `transit`) over HTTP without mounting the daemon socket or shipping `autosk`
   * in the sandbox. The server is closed automatically by the engine on every
   * settle / finaliser / detach (so a forgetful agent never leaks a port across
   * steps); the returned `close()` is an explicit early-release.
   */
  newMCPServer(): Promise<{ url: string; port: number; token: string; close(): Promise<void> }>;
}
