/**
 * `@autosk/claude-agent` — drive Claude Code (`claude -p` headless stream-json)
 * as an autoskd v2 agent. The structural twin of `@autosk/pi-agent`, with Claude
 * Code as the harness instead of `pi --mode rpc`.
 *
 * `claudeAgent({...})` returns an {@link AgentDefinition} the engine runs for a
 * workflow step: spawn `claude -p --output-format stream-json --input-format
 * stream-json`, drive it over NDJSON stdio via {@link ClaudeDriver}, mirror
 * assistant/tool messages 1:1 into the autosk transcript, and run the
 * kickback/corrections loop to a single transition.
 *
 * Tool surface (plan §5): the per-session, host-side HTTP MCP server minted by
 * `ctx.newMCPServer()`, registered via `--mcp-config` as `type:"http"` with a
 * bearer token, exposing `transit` (ack-only, observed by the driver), `task`,
 * and `comment` (executed for real by the daemon's direct-store backend). The
 * model sees `mcp__autosk__transit` / `mcp__autosk__task` / `mcp__autosk__comment`.
 * No `autosk`/`autoskd` and no mounted socket are needed in the sandbox — the
 * harness reaches the host server via `sandbox.endpointFor(port)` (e.g.
 * `host.docker.internal`).
 */

import { readFile } from "node:fs/promises";

import type { AgentDefinition, AgentRunContext, AutoskAPI, StepTarget } from "@autosk/sdk";

import { ClaudeDriver, TRANSIT_TOOL_NAME } from "./driver.ts";
import { kickbackMessage, renderInitialPrompt, rejectionMessage } from "./prompt.ts";

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
  /** Ensure the per-task workspace exists and return the dir the harness runs in. */
  workspace(id: SandboxId): Promise<{ cwd: string }>;
  /** Wrap the harness argv to run inside the sandbox (`docker run …`). Identity for host/worktree. */
  wrap(cmd: string[], o: { cwd: string; env?: Record<string, string>; id: SandboxId }): string[];
  /** Isolation-correct host endpoint an in-sandbox process uses to reach a host `port`. */
  endpointFor(port: number): string;
  /** Best-effort stop of the running sandbox process tree (agent `onAbort`). */
  stop(id: SandboxId): Promise<void>;
}

export {
  ClaudeDriver,
  Coalescer,
  parseTarget,
  stripAnsi,
  TRANSIT_TOOL_NAME,
  type TurnEnd,
  type ClaudeDriverHooks,
} from "./driver.ts";
export {
  mapAssistant,
  mapUser,
  mapResultUsage,
  mapStopReason,
  mapUsage,
  type ClaudeAssistantMessage,
} from "./wire.ts";
export { renderInitialPrompt, kickbackMessage, rejectionMessage, targetLabel, targetLabels } from "./prompt.ts";

/** The full MCP tool names Claude sees, used for the headless allowlist. */
const TASK_TOOL_NAME = "mcp__autosk__task";
const COMMENT_TOOL_NAME = "mcp__autosk__comment";

/**
 * Configuration for one Claude-Code-backed agent role. No `name`: a claude agent
 * is an inline step value, so its display name is the workflow step key (taken
 * from `ctx.workflows.current.step` at run time).
 */
export interface ClaudeAgentOptions {
  /** Claude model alias/name, e.g. `"sonnet"` / `"opus"` (`--model`). */
  model?: string;
  /**
   * Effort level for the run (`--effort`). The available levels depend on the
   * model. Overrides the `effortLevel` setting for this session (it does not
   * persist). Omitted by default (Claude Code uses its configured `effortLevel`).
   */
  effort?: "low" | "medium" | "high" | "xhigh" | "max";
  /** Inline first-message seed (wins over {@link firstMessageFile}). */
  firstMessage?: string;
  /** Path to a file whose contents seed the first message. */
  firstMessageFile?: string;
  /** Role guidance appended to the system prompt (`--append-system-prompt`). */
  appendSystemPrompt?: string;
  /**
   * Permission mode for an unattended run (`--permission-mode`). Defaults to
   * `"acceptEdits"`. A headless run that hits an un-allowed tool ABORTS (it
   * cannot prompt), so the posture must be non-interactive-safe.
   */
  permissionMode?: "default" | "acceptEdits" | "plan" | "dontAsk" | "bypassPermissions";
  /**
   * Skip all permission prompts (`--dangerously-skip-permissions`); wins over
   * {@link permissionMode}. Intended for runs under worktree isolation (the
   * worktree is the sandbox).
   */
  dangerouslySkipPermissions?: boolean;
  /** Auto-approved tools (`--allowedTools`, csv). The autosk MCP tools are added automatically. */
  allowedTools?: string[];
  /** Denied tools (`--disallowedTools`, csv). */
  disallowedTools?: string[];
  /** `--bare` for hermetic runs (skip project CLAUDE.md / .mcp.json / hooks discovery). */
  bare?: boolean;
  /**
   * The sandbox the harness runs inside (e.g. `worktreeSandbox()` /
   * `dockerSandbox({image})` from `@autosk/sandbox`, or any structural sandbox).
   * When set the agent resolves the per-task workspace, rewrites the MCP host via
   * `sandbox.endpointFor(port)`, and wraps the `claude` argv; `onAbort` calls
   * `sandbox.stop`. When unset, `claude` runs on the host at `ctx.cwd`.
   */
  sandbox?: AgentSandbox;
  /**
   * Register the `autosk` MCP server (transit + task + comment). Default `true`.
   * `false` omits `--mcp-config` (no autosk tools; transit then never fires → the
   * run always parks).
   */
  autoskTools?: boolean;
  /** Extra args forwarded verbatim to `claude`. */
  extraArgs?: string[];
  /**
   * Max corrective turns before giving up and returning without a transit (the
   * engine then parks via `agent_did_not_transit`). Default `3`.
   */
  maxCorrections?: number;
  /** `claude` binary to spawn. Defaults to `$AUTOSK_CLAUDE_BIN` or `"claude"`. The e2e tests point this at a stub. */
  claudeBin?: string;
}

/** Per-session driver registry so steer/followup/abort reach the live claude. */
const liveSessions = new Map<string, ClaudeDriver>();

/**
 * Builds the Claude-Code-backed {@link AgentDefinition} for one role. The
 * returned agent spawns `claude -p` stream-json on each `onRun`, drives it to a
 * single `mcp__autosk__transit`, and forwards steer/followup/abort into the live
 * process.
 */
export function claudeAgent(opts: ClaudeAgentOptions = {}): AgentDefinition {
  const maxCorrections = opts.maxCorrections ?? 3;
  // Resolve the first-message seed at most once per process (the loader caches
  // extension modules, so re-reading the prompt file every onRun is wasted IO).
  let firstMessageOnce: Promise<string> | null = null;
  const firstMessage = (): Promise<string> => (firstMessageOnce ??= resolveFirstMessage(opts));

  return {
    async onRun(ctx: AgentRunContext): Promise<void> {
      // Interactive (taskless) chat: a separate loop with no transit (plan §7).
      if (ctx.mode === "interactive") {
        await runChat(ctx, opts);
        return;
      }
      const id: SandboxId = { projectRoot: ctx.projectRoot, taskId: ctx.tasks.currentId };
      const ws = opts.sandbox ? await opts.sandbox.workspace(id) : { cwd: ctx.cwd };
      // Mint the per-session host-side MCP server and point claude at it over HTTP,
      // rewriting the host for the sandbox topology (e.g. host.docker.internal).
      let mcp: Awaited<ReturnType<AgentRunContext["newMCPServer"]>> | null = null;
      let mcpConfig: string | undefined;
      if (opts.autoskTools !== false) {
        mcp = await ctx.newMCPServer();
        const url = opts.sandbox ? opts.sandbox.endpointFor(mcp.port) : mcp.url;
        mcpConfig = buildMcpConfig(url, mcp.token);
      }
      const cmd = buildClaudeCommand(opts, { mcpConfig });
      // Spawn claude at the workspace cwd (a worktree/container under a sandbox,
      // else the project root). `autoskEnv` carries AUTOSK_CWD/AUTOSK_AGENT for any
      // host-side `autosk` the model may run via bash; the MCP tool surface itself
      // is server-bound and needs no env. Under a sandbox the argv is wrapped.
      const env = autoskEnv(ctx);
      const argv = opts.sandbox ? opts.sandbox.wrap(cmd, { cwd: ws.cwd, env, id }) : cmd;
      const child = ctx.spawn(argv, { cwd: ws.cwd, env });
      const driver = new ClaudeDriver(child, {
        onMessage: (m) => ctx.log.message(m),
        onUsage: (usage) => ctx.log.usage(usage),
        onCustom: (t, d) => ctx.log.custom(t, d),
        onPartial: (m) => ctx.partial(m),
        signal: ctx.signal,
        warn: (message) => ctx.log.custom("claude-agent:warn", { message }),
      });
      // Register the live driver BEFORE the first `await`: the engine marks the
      // session steerable as soon as onRun starts, so a steer/followup landing
      // while we resolve the first message must reach this driver.
      liveSessions.set(ctx.session.id, driver);
      try {
        const seed = await firstMessage();
        await runTurns(ctx, driver, seed, maxCorrections);
      } finally {
        liveSessions.delete(ctx.session.id);
        await driver.shutdown().catch(() => {});
        // Explicit early-release (the engine backstop closes it on settle anyway).
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
      // child is being killed; this just asks claude to wind down gracefully.
      if (driver) await driver.shutdown().catch(() => {});
    },
  };
}

/**
 * The kickback/corrections turn loop (ported from pi-agent): prompt → wait for
 * the turn to end → if the model called the transit tool, commit it (a rejection
 * is fed back as a correction); else kick back. After the budget is exhausted,
 * return WITHOUT a transit so the engine parks the task (`agent_did_not_transit`).
 */
async function runTurns(
  ctx: AgentRunContext,
  driver: ClaudeDriver,
  firstMessage: string,
  maxCorrections: number,
): Promise<void> {
  const targets = ctx.workflows.current.targets;
  const task = await ctx.tasks.current();
  const comments = await ctx.tasks.comments();
  await driver.sendPrompt(
    renderInitialPrompt({
      firstMessage,
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
      throw new Error(`claude exited (code=${driver.exitCode}) before recording a transition`);
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
 * The interactive (taskless) chat loop (plan §7): spawn `claude -p` stream-json
 * with the `autosk` MCP server but WITHOUT transit (it has nothing to transit in
 * a chat), send NO initial prompt (the session opens idle), register the driver
 * before the first `await`, wire `onActivity` → `ctx.setActivity`, then wait for
 * `ctx.signal`. Each composer message arrives via `onFollowup` →
 * `driver.input("followup", msg)` (idle → a fresh turn).
 */
async function runChat(ctx: AgentRunContext, opts: ClaudeAgentOptions): Promise<void> {
  // A chat has no task, so `newMCPServer()` binds with transit:false (the model
  // is never offered a tool that has nothing to transit). Interactive mode runs
  // on the host at ctx.cwd (no per-task workspace).
  let mcp: Awaited<ReturnType<AgentRunContext["newMCPServer"]>> | null = null;
  let mcpConfig: string | undefined;
  if (opts.autoskTools !== false) {
    mcp = await ctx.newMCPServer();
    mcpConfig = buildMcpConfig(mcp.url, mcp.token);
  }
  const cmd = buildClaudeCommand(opts, { mcpConfig, interactive: true });
  const child = ctx.spawn(cmd, { cwd: ctx.cwd, env: autoskEnv(ctx) });
  const driver = new ClaudeDriver(child, {
    onMessage: (m) => ctx.log.message(m),
    onUsage: (usage) => ctx.log.usage(usage),
    onCustom: (t, d) => ctx.log.custom(t, d),
    onPartial: (m) => ctx.partial(m),
    signal: ctx.signal,
    onActivity: (busy) => ctx.setActivity(busy ? "busy" : "idle"),
    warn: (message) => ctx.log.custom("claude-agent:warn", { message }),
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
    if (mcp) await mcp.close().catch(() => {});
  }
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

/** Forwards a steer/followup into the live claude for the calling session. */
async function forward(ctx: AgentRunContext, kind: "steer" | "followup", message: string): Promise<void> {
  const driver = liveSessions.get(ctx.session.id);
  if (driver) await driver.input(kind, message);
}

/** Resolves the role's first-message seed (inline wins, else file, else ""). */
async function resolveFirstMessage(opts: ClaudeAgentOptions): Promise<string> {
  if (opts.firstMessage !== undefined) return opts.firstMessage;
  if (opts.firstMessageFile) return readFile(opts.firstMessageFile, "utf8");
  return "";
}

/**
 * The autosk env handed to the spawned claude so any host-side `autosk` CLI the
 * model runs via bash resolves the ORIGINAL project (`AUTOSK_CWD`) and attributes
 * comments to the running step (`AUTOSK_AGENT`). The MCP tool surface itself is
 * the per-session HTTP server (server-bound), so it needs NO env — and there is
 * no mounted socket. `ctx.spawn` merges this over `process.env`.
 */
export function autoskEnv(ctx: AgentRunContext): Record<string, string> {
  return {
    AUTOSK_CWD: ctx.projectRoot,
    AUTOSK_AGENT: ctx.workflows.current.step,
  };
}

/**
 * Builds the inline `--mcp-config` JSON pointing Claude at the per-session,
 * host-side HTTP MCP server (`ctx.newMCPServer()`), with the bearer token in the
 * `Authorization` header. The `url` is already isolation-correct (the agent
 * rewrote the host via `sandbox.endpointFor(port)` for a container). The server
 * advertises `transit` only in task mode (it is bound that way), so no client-side
 * gate is needed here.
 */
export function buildMcpConfig(url: string, token: string): string {
  return JSON.stringify({
    mcpServers: {
      autosk: {
        type: "http",
        url,
        headers: { Authorization: `Bearer ${token}` },
      },
    },
  });
}

/**
 * Builds the `claude -p … stream-json` argv. In interactive (chat) mode the
 * transit tool is not registered (the MCP config is built with `transit:false`),
 * matching pi-agent skipping the transit tool in a chat.
 */
export function buildClaudeCommand(
  opts: ClaudeAgentOptions,
  flags: { mcpConfig?: string; interactive?: boolean } = {},
): string[] {
  const bin = opts.claudeBin ?? process.env.AUTOSK_CLAUDE_BIN ?? "claude";
  const args = [
    bin,
    "-p",
    "--output-format",
    "stream-json",
    "--input-format",
    "stream-json",
    "--verbose",
    "--include-partial-messages",
    "--replay-user-messages",
  ];
  if (flags.mcpConfig !== undefined) {
    args.push("--mcp-config", flags.mcpConfig);
    // Pre-approve the autosk MCP tools so a headless run never silently aborts on
    // a permission prompt for transit/task/comment.
    const autoskTools = flags.interactive
      ? [TASK_TOOL_NAME, COMMENT_TOOL_NAME]
      : [TRANSIT_TOOL_NAME, TASK_TOOL_NAME, COMMENT_TOOL_NAME];
    opts = { ...opts, allowedTools: [...autoskTools, ...(opts.allowedTools ?? [])] };
  }
  if (opts.model) args.push("--model", opts.model);
  if (opts.effort) args.push("--effort", opts.effort);
  if (opts.dangerouslySkipPermissions) {
    args.push("--dangerously-skip-permissions");
  } else {
    args.push("--permission-mode", opts.permissionMode ?? "acceptEdits");
  }
  if (opts.allowedTools && opts.allowedTools.length > 0) args.push("--allowedTools", opts.allowedTools.join(","));
  if (opts.disallowedTools && opts.disallowedTools.length > 0) {
    args.push("--disallowedTools", opts.disallowedTools.join(","));
  }
  if (opts.appendSystemPrompt) args.push("--append-system-prompt", opts.appendSystemPrompt);
  if (opts.bare) args.push("--bare");
  args.push(...(opts.extraArgs ?? []));
  return args;
}

/**
 * Default extension factory. Workflow roles are registered by a consuming
 * extension via `claudeAgent({...})` as inline step values. Here we register a
 * single NAMED agent so an interactive (taskless) chat session can be opened
 * against it (plan §7).
 */
export default function claudeAgentExtension(autosk: AutoskAPI): void {
  autosk.registerAgent({
    name: "@autosk/claude-agent",
    description: "system-wide Claude Code agent",
    agent: claudeAgent(), // default options (model from Claude's own defaults)
  });
}
