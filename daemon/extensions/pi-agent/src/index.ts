/**
 * `@autosk/pi-agent` — drive `pi --mode rpc` as an autoskd v2 agent (plan §3.4).
 *
 * Ports v1's "standard branch" (the retired Rust daemon's pi + executor):
 * spawn `pi --mode rpc` with the role's model/thinking/first-message, drive it
 * over JSON-Lines stdio via {@link PiDriver}, mirror pi session entries into the
 * autosk transcript 1:1, and reimplement the kickback/corrections loop as this
 * extension's PRIVATE logic (the engine no longer has the concept).
 *
 * Transit channel (plan §3.4, resolved-#2): an injected pi extension registers
 * an `autosk_transit` tool into the spawned pi; the driver observes the tool
 * call on pi's RPC event stream and translates it into `ctx.transit(...)`. A
 * workflow `onTransit` rejection is fed back to the model as a corrective
 * follow-up (same model-visible effect as a tool error) so it can pick another
 * target — core stays closed.
 */

import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

import type { AgentDefinition, AgentRunContext, AutoskAPI, StepTarget, TranscriptMessage } from "@autosk/sdk";

import { PiDriver } from "./driver.ts";
import { kickbackMessage, renderInitialPrompt, rejectionMessage } from "./prompt.ts";

export {
  PiDriver,
  parseTarget,
  buildInputCommand,
  isStateMismatch,
  isBusyRejection,
  TRANSIT_TOOL_NAME,
  type TurnEnd,
} from "./driver.ts";
export {
  renderInitialPrompt,
  kickbackMessage,
  rejectionMessage,
  targetLabel,
  targetLabels,
} from "./prompt.ts";

/**
 * Configuration for one pi-backed agent role (plan §3.4, extended in P6).
 *
 * No `name`: a pi agent is an inline step value, so its display name is the
 * workflow step key (taken from `ctx.workflows.current.step` at run time).
 */
export interface PiAgentOptions {
  /** pi model spec, e.g. `"sonnet:high"`. */
  model?: string;
  /** pi thinking level: `off`|`minimal`|`low`|`medium`|`high`|`xhigh`. */
  thinking?: string;
  /** Inline first-message seed (wins over {@link firstMessageFile}). */
  firstMessage?: string;
  /** Path to a file whose contents seed the first message. */
  firstMessageFile?: string;
  /** Extra args forwarded verbatim to `pi`. */
  extraArgs?: string[];
  /** pi extensions to load (`-e <path>` each). */
  piExtensions?: string[];
  /** pi skills to enable (`--skill <name>` each). */
  piSkills?: string[];
  /**
   * Max corrective turns before giving up and returning without a transit
   * (the engine then parks via `agent_did_not_transit`). Default `3`.
   */
  maxCorrections?: number;
  /** `pi` binary to spawn. Defaults to `$AUTOSK_PI_BIN` or `"pi"`. The e2e tests point this at a stub. */
  piBin?: string;
  /**
   * The sandbox the harness runs inside (e.g. `worktreeSandbox()` /
   * `dockerSandbox({image})` from `@autosk/sandbox`, or any structural sandbox).
   * When set the agent resolves the per-task workspace and wraps the `pi` argv;
   * `onAbort` calls `sandbox.stop`.
   *
   * The agent ALWAYS injects only the ack-only `autosk_transit` tool
   * (`pi-transit-extension.ts`); `autosk_task` / `autosk_comment` come from the
   * single, transport-aware `@autosk/pi-tools` extension pi loads from its own
   * config. `sandbox.thin` only chooses pi-tools' TRANSPORT:
   *  - **thin** (a container with no `autosk` CLI / host FS, e.g.
   *    `dockerSandbox`): mint a per-session HTTP MCP server and inject its
   *    endpoint (`AUTOSK_MCP_URL` / `AUTOSK_MCP_TOKEN`, the URL rewritten via
   *    `sandbox.endpointFor(port)`) so `@autosk/pi-tools` POSTs task/comment to
   *    the host MCP server — no `autosk` shell-out is needed in the container;
   *  - **not thin** (host/worktree, or the `mountSocket` docker escape hatch):
   *    no MCP env, so `@autosk/pi-tools` shells out to `autosk` on PATH.
   *
   * When unset, `pi` runs on the host at `ctx.cwd` (pi-tools' CLI path).
   */
  sandbox?: AgentSandbox;
}

/** The task identity an agent hands its sandbox. */
interface SandboxId {
  projectRoot: string;
  taskId: string;
}

/**
 * The STRUCTURAL sandbox shape the agent consumes (a subset of `@autosk/sandbox`'s
 * `Sandbox`). Typed structurally so a workflow can pass a hand-rolled sandbox
 * without this agent depending on the `@autosk/sandbox` package's type.
 */
export interface AgentSandbox {
  /**
   * `true` for a thin container (no `autosk` CLI / host FS) — pi uses the HTTP
   * MCP tool surface; absent/false for host/worktree (pi keeps the pi-tools path).
   */
  thin?: boolean;
  /** Ensure the per-task workspace exists and return the dir the harness runs in. */
  workspace(id: SandboxId): Promise<{ cwd: string }>;
  /** Wrap the harness argv to run inside the sandbox (`docker run …`). Identity for host/worktree. */
  wrap(cmd: string[], o: { cwd: string; env?: Record<string, string>; id: SandboxId; roFiles?: string[] }): string[];
  /** Isolation-correct host endpoint an in-sandbox process uses to reach a host `port`. */
  endpointFor(port: number): string;
  /** Best-effort stop of the running sandbox process tree (agent `onAbort`). */
  stop(id: SandboxId): Promise<void>;
}

/**
 * The injected pi extension that registers the ack-only `autosk_transit` tool.
 * This is the ONLY tool the agent injects — `autosk_task` / `autosk_comment` come
 * from the transport-aware `@autosk/pi-tools` extension pi loads from its config
 * (MCP when `AUTOSK_MCP_URL` is set under a thin sandbox, else the `autosk` CLI).
 */
const TRANSIT_EXTENSION_PATH = fileURLToPath(new URL("./pi-transit-extension.ts", import.meta.url));

/** Per-session driver registry so steer/followup/abort reach the live pi. */
const liveSessions = new Map<string, PiDriver>();

/**
 * Builds the pi-backed {@link AgentDefinition} for one role (plan §3.4). The
 * returned agent spawns `pi --mode rpc` on each `onRun`, drives it to a single
 * `autosk_transit`, and forwards steer/followup/abort into the live process.
 */
export function piAgent(opts: PiAgentOptions = {}): AgentDefinition {
  const maxCorrections = opts.maxCorrections ?? 3;
  // Resolve the first-message seed at most once per process: extension code is
  // frozen until the daemon restarts (the loader caches modules), so re-reading
  // the prompt file on every `onRun` is wasted IO on the hot path.
  let firstMessageOnce: Promise<string> | null = null;
  const firstMessage = (): Promise<string> => (firstMessageOnce ??= resolveFirstMessage(opts));

  return {
    async onRun(ctx: AgentRunContext): Promise<void> {
      // Interactive (taskless) chat: a separate loop with no transit (plan §5).
      if (ctx.mode === "interactive") {
        await runChat(ctx, opts);
        return;
      }
      const id: SandboxId = { projectRoot: ctx.projectRoot, taskId: ctx.tasks.currentId };
      const ws = opts.sandbox ? await opts.sandbox.workspace(id) : { cwd: ctx.cwd };
      // Tool surface (plan §5.2): host/worktree pi (NOT thin) keeps the proven
      // pi-tools + transit-extension shell-out path (`autosk` is on PATH there) —
      // this is what the DEFAULT feature-dev workflow uses. Only a THIN sandbox (a
      // container with no `autosk`, e.g. dockerSandbox) switches to the per-session
      // HTTP MCP server: mint it, rewrite its host for the container, and inject
      // its endpoint + bearer as env for the in-container http pi-extension.
      const thin = opts.sandbox?.thin === true;
      let mcp: Awaited<ReturnType<AgentRunContext["newMCPServer"]>> | null = null;
      let env = autoskEnv(ctx);
      if (thin) {
        // Mint the per-session MCP server and hand its endpoint to the in-container
        // `@autosk/pi-tools` (which routes task/comment through it); the agent still
        // injects only the ack-only transit tool.
        mcp = await ctx.newMCPServer();
        env = { ...env, AUTOSK_MCP_URL: opts.sandbox!.endpointFor(mcp.port), AUTOSK_MCP_TOKEN: mcp.token };
      }
      const cmd = buildPiCommand(opts);
      // The injected transit pi-extension is a HOST file passed to `pi -e`; a
      // container can't see the host FS, so a sandbox must bind-mount it (identical
      // path) for `pi -e <path>` to resolve. A host/worktree `wrap` ignores roFiles.
      const argv = opts.sandbox
        ? opts.sandbox.wrap(cmd, { cwd: ws.cwd, env, id, roFiles: [TRANSIT_EXTENSION_PATH] })
        : cmd;
      const child = ctx.spawn(argv, { cwd: ws.cwd, env });
      const driver = new PiDriver(child, {
        onMessage: (m) => logMessage(ctx, m),
        onCustom: (t, d) => ctx.log.custom(t, d),
        // Stream in-progress assistant snapshots live (ephemeral; superseded by
        // the committed onMessage line).
        onPartial: (m) => ctx.partial(m),
        signal: ctx.signal,
        warn: (message) => ctx.log.custom("pi-agent:warn", { message }),
      });
      // Register the live driver BEFORE the first `await`: the engine marks the
      // session steerable as soon as `onRun` starts, so a steer/followup landing
      // while we resolve the first message must reach this driver instead of
      // being silently dropped-but-acked.
      liveSessions.set(ctx.session.id, driver);
      try {
        const seed = await firstMessage();
        await runTurns(ctx, driver, seed, maxCorrections);
      } finally {
        liveSessions.delete(ctx.session.id);
        await driver.shutdown().catch(() => {});
        // A persistent `pi --mode rpc` does not exit on the driver shutdown alone
        // (the `docker run` client is killed but the `--rm` container can linger),
        // so stop the sandbox process tree too. Best-effort; the next step's
        // workspace() also force-removes a stale container as a backstop.
        if (opts.sandbox && ctx.mode === "task") {
          await opts.sandbox.stop(id).catch(() => {});
        }
        if (mcp) await mcp.close().catch(() => {});
      }
    },

    onSteer: (ctx, message) => forward(ctx, "steer", message),
    onFollowup: (ctx, message) => forward(ctx, "followup", message),

    async onAbort(ctx): Promise<void> {
      // Best-effort stop of the sandbox process tree (covers a SIGKILL orphan).
      if (opts.sandbox && ctx.mode === "task") {
        await opts.sandbox
          .stop({ projectRoot: ctx.projectRoot, taskId: ctx.tasks.currentId })
          .catch(() => {});
      }
      const driver = liveSessions.get(ctx.session.id);
      // ctx.signal has already fired (the engine aborts before onAbort), so the
      // child is being killed; this just asks pi to wind down gracefully first.
      if (driver) await driver.shutdown().catch(() => {});
    },
  };
}

/**
 * The kickback/corrections turn loop (v1 `turn_loop`, now private to the agent):
 * prompt → wait for the turn to end → if the model called `autosk_transit`,
 * commit it (a rejection is fed back as a correction); else kick back. After the
 * budget is exhausted, return WITHOUT a transit so the engine parks the task via
 * `agent_did_not_transit`.
 */
async function runTurns(
  ctx: AgentRunContext,
  driver: PiDriver,
  firstMessage: string,
  maxCorrections: number,
): Promise<void> {
  const targets = ctx.workflows.current.targets;
  const task = await ctx.tasks.current();
  const comments = await ctx.tasks.comments();
  await driver.sendPrompt(
    renderInitialPrompt({
      firstMessage,
      // The step key IS the agent name (inline step agents).
      agentName: ctx.workflows.current.step,
      workflow: ctx.workflows.current.workflow,
      step: ctx.workflows.current.step,
      task,
      targets,
      comments,
    }),
  );

  let corrections = 0;
  for (;;) {
    const how = await driver.waitForTurnEnd();
    if (how === "aborted") return; // engine finalises the session as aborted
    if (how === "exited") {
      throw new Error(`pi exited (code=${driver.exitCode}) before recording a transition`);
    }

    const target = driver.takePendingTarget();
    if (target) {
      const rejection = await commitTransit(ctx, target);
      if (!rejection) return; // success — the engine sealed the session as done
      if (corrections >= maxCorrections) return; // give up → engine parks
      corrections++;
      await driver.sendPrompt(rejectionMessage(target, rejection, targets, corrections, maxCorrections));
      continue;
    }

    // No transit this turn → kick back (bounded by the same budget).
    if (corrections >= maxCorrections) return; // give up → engine parks
    corrections++;
    await driver.sendPrompt(kickbackMessage(task.id, targets, corrections, maxCorrections));
  }
}

/**
 * The interactive (taskless) chat loop (plan §5): spawn `pi --mode rpc` WITHOUT
 * the `autosk_transit` extension (transit is not offered in chat mode), send NO
 * initial prompt (the session opens idle), register the driver before the first
 * `await`, then wait for `ctx.signal`. Each composer message arrives via
 * `onFollowup` → `driver.input("followup", msg)`: idle → a fresh `pi` turn,
 * streaming → a `follow_up`. The runtime seals the session (`done` on a graceful
 * `end()`, `aborted` on an `abort()`); `onAbort` winds the driver down.
 */
async function runChat(ctx: AgentRunContext, opts: PiAgentOptions): Promise<void> {
  const cmd = buildPiCommand(opts, { interactive: true });
  const child = ctx.spawn(cmd, { cwd: ctx.cwd, env: autoskEnv(ctx) });
  const driver = new PiDriver(child, {
    onMessage: (m) => logMessage(ctx, m),
    onCustom: (t, d) => ctx.log.custom(t, d),
    // Stream in-progress assistant snapshots live (ephemeral; superseded by the
    // committed onMessage line).
    onPartial: (m) => ctx.partial(m),
    signal: ctx.signal,
    // Surface the chat's turn boundaries as session activity so a client shows
    // `idle` (waiting for the user) vs `working` (streaming a turn).
    onActivity: (busy) => ctx.setActivity(busy ? "busy" : "idle"),
    warn: (message) => ctx.log.custom("pi-agent:warn", { message }),
  });
  // Register the live driver BEFORE the first `await` so the very first composer
  // message (delivered via onFollowup) reaches this driver and starts a turn.
  liveSessions.set(ctx.session.id, driver);
  try {
    // No initial prompt: an interactive session is empty until the user types.
    await waitForSignal(ctx.signal);
  } finally {
    liveSessions.delete(ctx.session.id);
    await driver.shutdown().catch(() => {});
  }
}

function logMessage(ctx: AgentRunContext, message: TranscriptMessage): void {
  ctx.log.message(message);
  if (message.role === "assistant") ctx.log.usage(message.usage);
}

/** Resolves when the abort signal fires (the user ended or aborted the session). */
function waitForSignal(signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.resolve();
  return new Promise<void>((resolve) => {
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
}

/** Commits a transit; returns `null` on success or the rejection message string. */
async function commitTransit(ctx: AgentRunContext, target: StepTarget): Promise<string | null> {
  try {
    await ctx.transit(target);
    return null;
  } catch (e) {
    return e instanceof Error ? e.message : String(e);
  }
}

/** Forwards a steer/followup into the live pi for the calling session. */
async function forward(ctx: AgentRunContext, kind: "steer" | "followup", message: string): Promise<void> {
  const driver = liveSessions.get(ctx.session.id);
  if (driver) await driver.input(kind, message);
}

/** Resolves the role's first-message seed (inline wins, else file, else ""). */
async function resolveFirstMessage(opts: PiAgentOptions): Promise<string> {
  if (opts.firstMessage !== undefined) return opts.firstMessage;
  if (opts.firstMessageFile) return readFile(opts.firstMessageFile, "utf8");
  return "";
}

/**
 * The autosk env handed to the spawned pi so any `autosk` CLI it runs resolves
 * the ORIGINAL project (not the worktree it runs in) and attributes comments to
 * the running step. `ctx.spawn` merges this over `process.env`, so PATH/HOME and
 * the rest are preserved.
 */
export function autoskEnv(ctx: AgentRunContext): Record<string, string> {
  return {
    AUTOSK_CWD: ctx.projectRoot,
    AUTOSK_AGENT: ctx.workflows.current.step,
  };
}

/**
 * Builds the `pi --mode rpc …` argv (v1 `buildPiExtraArgs` + the daemon flags).
 *
 * In `interactive` (chat) mode the `autosk_transit` pi extension is NOT injected:
 * transit is unavailable in an interactive session, so the model must not be
 * offered a tool that would throw (plan §5, §9).
 */
export function buildPiCommand(opts: PiAgentOptions, flags: { interactive?: boolean } = {}): string[] {
  const bin = opts.piBin ?? process.env.AUTOSK_PI_BIN ?? "pi";
  const args = [bin, "--mode", "rpc"];
  if (opts.model) args.push("--model", opts.model);
  if (opts.thinking) args.push("--thinking", opts.thinking);
  // Inject the ack-only `autosk_transit` tool so it is always available — except
  // in interactive (chat) mode, where transit is not offered. `autosk_task` /
  // `autosk_comment` are NOT injected here: the transport-aware @autosk/pi-tools
  // extension pi loads from its config provides them (MCP under a thin sandbox
  // when AUTOSK_MCP_URL is set, else the `autosk` CLI).
  if (!flags.interactive) args.push("-e", TRANSIT_EXTENSION_PATH);
  for (const ext of opts.piExtensions ?? []) args.push("-e", ext);
  for (const skill of opts.piSkills ?? []) args.push("--skill", skill);
  args.push(...(opts.extraArgs ?? []));
  return args;
}

/**
 * Default extension factory. Workflow roles are still registered by
 * `@autosk/feature-dev` (and any operator extension) via `piAgent({...})` as
 * inline step values. Here we register a single NAMED agent, `"pi"`, so an
 * interactive (taskless) chat session can be opened against it (plan §5).
 */
export default function piAgentExtension(autosk: AutoskAPI): void {
  autosk.registerAgent({
    name: "@autosk/pi-agent",
    description: "system-wide pi.dev agent",
    agent: piAgent(), // default options (model from pi's own defaults)
  });
}
