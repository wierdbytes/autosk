/**
 * Claude Code stream-json wire driver — the structural twin of
 * `@autosk/pi-agent`'s `PiDriver`, but driving a spawned `claude -p
 * --output-format stream-json --input-format stream-json` child instead of
 * `pi --mode rpc`.
 *
 * It wraps the engine's {@link ChildHandle} (`ctx.spawn`) and exposes the same
 * surface the kickback loop expects (`sendPrompt` / `input` / `waitForTurnEnd` /
 * `takePendingTarget` / `shutdown`). The differences from pi are purely the wire
 * vocabulary (`assistant` / `user` / `result` / `stream_event` instead of pi's
 * `message_end` / `agent_end` / `message_update`), so the turn-boundary state
 * machine and its tests transfer directly. It:
 *  - reads the NDJSON event stream and mirrors `assistant` / `user` messages into
 *    the transcript 1:1 (via {@link wire});
 *  - accumulates `stream_event` partial deltas into a cumulative assistant
 *    snapshot streamed through the {@link Coalescer} as `onPartial`;
 *  - ends a turn on `result` (pi's `agent_end`);
 *  - OBSERVES the `mcp__autosk__transit` `tool_use` on the stream and exposes the
 *    requested {@link StepTarget} (the transit channel — the driver, not the MCP
 *    server, drives `ctx.transit`).
 */

import type { ChildHandle, StepTarget, TranscriptMessage, Usage } from "@autosk/sdk";

import {
  mapAssistant,
  mapResultUsage,
  mapUser,
  type ClaudeAssistantMessage,
} from "./wire.ts";

/** How a turn (one prompt→result cycle) ended. */
export type TurnEnd = "ended" | "exited" | "aborted";

/** Hooks the agent wires into the driver (identical shape to pi-agent's). */
export interface ClaudeDriverHooks {
  /** Mirror a message-schema entry into the transcript (`ctx.log.message`). */
  onMessage(message: TranscriptMessage): void;
  /** Report authoritative usage from a completed Claude result. */
  onUsage?(usage: Usage | null): void;
  /** Mirror a custom session entry into the transcript (`ctx.log.custom`). */
  onCustom(customType: string, data: unknown): void;
  /** The session's abort signal (`ctx.signal`). */
  signal: AbortSignal;
  /**
   * Turn-boundary activity callback: `true` when a turn starts (a prompt/input
   * is sent), `false` when the turn ends (`result`). The interactive chat loop
   * wires this to `ctx.setActivity` so a client shows idle vs working.
   */
  onActivity?(busy: boolean): void;
  /**
   * Streams an EPHEMERAL, cumulative assistant-message snapshot as Claude
   * generates a turn (from `stream_event` deltas). Coalesced (~40ms); never
   * written to the transcript and always superseded by the committed
   * {@link onMessage}. The agent wires this to `ctx.partial`.
   */
  onPartial?(message: TranscriptMessage): void;
  /** Optional diagnostic sink. */
  warn?(message: string): void;
}

/** Min interval (ms) between coalesced partial snapshots emitted to subscribers. */
export const PARTIAL_COALESCE_MS = 40;

/**
 * Coalesces a high-frequency stream of cumulative snapshots into bounded-rate
 * emissions (copied from `@autosk/pi-agent`). The first snapshot fires
 * immediately (leading edge) and opens a min-interval window; further snapshots
 * within the window are buffered as the single latest value and flushed on the
 * trailing edge.
 */
export class Coalescer<T> {
  private latest: T | null = null;
  private hasLatest = false;
  private timer: ReturnType<typeof setTimeout> | null = null;

  constructor(
    private readonly intervalMs: number,
    private readonly emit: (value: T) => void,
  ) {}

  push(value: T): void {
    if (this.timer === null) {
      this.emit(value); // leading edge
      this.arm();
    } else {
      this.latest = value;
      this.hasLatest = true;
    }
  }

  private arm(): void {
    this.timer = setTimeout(() => {
      this.timer = null;
      if (this.hasLatest) {
        const v = this.latest as T;
        this.latest = null;
        this.hasLatest = false;
        this.emit(v);
        this.arm();
      }
    }, this.intervalMs);
  }

  /** Emits any buffered trailing snapshot NOW and stops the timer. */
  flush(): void {
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    if (this.hasLatest) {
      const v = this.latest as T;
      this.latest = null;
      this.hasLatest = false;
      this.emit(v);
    }
  }

  /** Cancels the timer and DROPS any buffered snapshot WITHOUT emitting (teardown). */
  stop(): void {
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    this.latest = null;
    this.hasLatest = false;
  }
}

/** The MCP tool name Claude sees for the transit tool (server "autosk", tool "transit"). */
export const TRANSIT_TOOL_NAME = "mcp__autosk__transit";

/** Cap on how many claude stderr lines are forwarded through `warn` per session. */
const STDERR_FORWARD_CAP = 100;

/**
 * Accumulates Anthropic `stream_event` deltas into a cumulative assistant-message
 * snapshot for the partial channel. Tracks text / thinking content per block
 * index; tool_use input deltas are reflected only as a tool-call-in-progress
 * (empty args) since partial JSON is not yet valid.
 */
class PartialAccumulator {
  private blocks: Map<number, { type: "text" | "thinking" | "tool"; text: string; name?: string }> = new Map();
  private model = "";

  reset(model?: string): void {
    this.blocks.clear();
    if (model) this.model = model;
  }

  startBlock(index: number, block: { type?: string; name?: string }): void {
    if (block.type === "text") this.blocks.set(index, { type: "text", text: "" });
    else if (block.type === "thinking") this.blocks.set(index, { type: "thinking", text: "" });
    else if (block.type === "tool_use") this.blocks.set(index, { type: "tool", text: "", name: block.name });
  }

  delta(index: number, delta: { type?: string; text?: string; thinking?: string }): void {
    const b = this.blocks.get(index);
    if (!b) {
      // A delta without a preceding start (text-only fast path) — open a text block.
      if (delta.type === "text_delta") this.blocks.set(index, { type: "text", text: delta.text ?? "" });
      else if (delta.type === "thinking_delta") this.blocks.set(index, { type: "thinking", text: delta.thinking ?? "" });
      return;
    }
    if (delta.type === "text_delta" && b.type === "text") b.text += delta.text ?? "";
    else if (delta.type === "thinking_delta" && b.type === "thinking") b.text += delta.thinking ?? "";
  }

  /** Builds the current cumulative assistant snapshot, or null if empty. */
  snapshot(): TranscriptMessage | null {
    if (this.blocks.size === 0) return null;
    const content: ({ type: "text"; text: string } | { type: "thinking"; thinking: string } | { type: "toolCall"; id: string; name: string; arguments: Record<string, unknown> })[] = [];
    for (const index of [...this.blocks.keys()].sort((a, b) => a - b)) {
      const b = this.blocks.get(index)!;
      if (b.type === "text") content.push({ type: "text", text: b.text });
      else if (b.type === "thinking") content.push({ type: "thinking", thinking: b.text });
      else content.push({ type: "toolCall", id: "", name: b.name ?? "", arguments: {} });
    }
    return {
      role: "assistant",
      content,
      provider: "anthropic",
      model: this.model,
      usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
      stopReason: "stop",
      timestamp: Date.now(),
    } as TranscriptMessage;
  }
}

export class ClaudeDriver {
  private streaming = false;
  private exited = false;
  private aborted = false;
  private shuttingDown = false;
  exitCode: number | null = null;
  /** The Claude session id from the `system`/`init` event (for future `--resume`). */
  sessionId: string | null = null;

  /** Buffered turn-end events (so a `result` that races the await isn't lost). */
  private readonly turnQueue: TurnEnd[] = [];
  private turnResolve: ((r: TurnEnd) => void) | null = null;

  /** The transit target observed during the current turn, or `null`. */
  private pendingTarget: StepTarget | null = null;

  /** tool_use id → name, from each `assistant` event (resolves tool_result names). */
  private readonly toolNames = new Map<string, string>();

  private stderrForwarded = 0;
  private readonly partials: Coalescer<TranscriptMessage>;
  private readonly accumulator = new PartialAccumulator();

  constructor(
    private readonly child: ChildHandle,
    private readonly hooks: ClaudeDriverHooks,
  ) {
    this.partials = new Coalescer<TranscriptMessage>(PARTIAL_COALESCE_MS, (m) => this.hooks.onPartial?.(m));
    child.onStdout((line) => this.onLine(line));
    child.onStderr((line) => this.onStderrLine(line));
    void child.exited.then(({ code }) => {
      this.exitCode = code;
      this.exited = true;
      this.partials.stop();
      this.emitTurn(this.aborted ? "aborted" : "exited");
    });
    if (hooks.signal.aborted) this.onAbort();
    else hooks.signal.addEventListener("abort", () => this.onAbort(), { once: true });
  }

  // -- outbound ------------------------------------------------------------

  /** Sends a prompt (first turn / a corrective turn) as a stream-json user message. */
  async sendPrompt(message: string): Promise<void> {
    this.beginTurn();
    this.writeUserMessage(message);
  }

  /**
   * Forwards a steer/followup into the live claude. While idle, both start a
   * fresh turn (a new user message). While streaming, a `steer` interrupts the
   * current turn (control_request) before its message; a `followup` is written
   * as a queued user message consumed when the current turn ends (the documented
   * queue-after-turn behavior — the CLI stream-json input has no mid-turn
   * `follow_up` command, only `interrupt`).
   *
   * Unlike pi-agent's `input()` this does NOT retry on a state-mismatch: Claude's
   * stream-json stdin is fire-and-forget with no per-message ack/response channel
   * to retry against, so there is nothing to retry. Note also that `streaming` is
   * set optimistically by {@link beginTurn} (not from an observed stream-start
   * event, as in pi): a steer landing in the tiny window after `sendPrompt` but
   * before Claude actually streams writes interrupt+message that the CLI may treat
   * as an appended turn rather than a true interrupt — acceptable (the message
   * still lands), just not a guaranteed mid-turn interrupt.
   */
  async input(kind: "steer" | "followup", message: string): Promise<void> {
    try {
      if (this.streaming) {
        if (kind === "steer") {
          this.writeInterrupt();
          this.writeUserMessage(message);
          return;
        }
        // followup while streaming → queue after the current turn.
        this.writeUserMessage(message);
        return;
      }
      // idle → a fresh turn.
      this.beginTurn();
      this.writeUserMessage(message);
    } catch (e) {
      this.hooks.warn?.(`claude-agent: forwarding ${kind} failed (${errMsg(e)})`);
    }
  }

  /** Waits for the current turn to end (or the child to exit / be aborted). */
  waitForTurnEnd(): Promise<TurnEnd> {
    if (this.turnQueue.length > 0) return Promise.resolve(this.turnQueue.shift()!);
    if (this.aborted) return Promise.resolve("aborted");
    if (this.exited) return Promise.resolve("exited");
    return new Promise<TurnEnd>((resolve) => {
      this.turnResolve = resolve;
    });
  }

  /** Takes (and clears) the transit target observed this turn, if any. */
  takePendingTarget(): StepTarget | null {
    const t = this.pendingTarget;
    this.pendingTarget = null;
    return t;
  }

  /**
   * Graceful shutdown: best-effort interrupt, close stdin, brief grace, then
   * kill. Idempotent — both `onAbort` and `onRun`'s finally can call it.
   */
  async shutdown(graceMs = 500): Promise<void> {
    if (this.shuttingDown) return;
    this.shuttingDown = true;
    try {
      this.writeInterrupt();
    } catch {
      /* child may already be gone */
    }
    try {
      await this.child.stdin.close();
    } catch {
      /* already closed */
    }
    const timeout = new Promise<void>((r) => setTimeout(r, graceMs));
    await Promise.race([this.child.exited.then(() => undefined), timeout]);
    try {
      this.child.kill();
    } catch {
      /* already dead */
    }
  }

  // -- inbound -------------------------------------------------------------

  private onStderrLine(line: string): void {
    const trimmed = stripAnsi(line).trim();
    if (trimmed === "") return;
    if (this.stderrForwarded >= STDERR_FORWARD_CAP) return;
    this.stderrForwarded++;
    this.hooks.warn?.(`claude:stderr: ${trimmed}`);
    if (this.stderrForwarded === STDERR_FORWARD_CAP) {
      this.hooks.warn?.(`claude:stderr: (further stderr suppressed after ${STDERR_FORWARD_CAP} lines)`);
    }
  }

  private onLine(line: string): void {
    const trimmed = line.trim();
    if (trimmed === "") return;
    let msg: Record<string, unknown>;
    try {
      msg = JSON.parse(trimmed) as Record<string, unknown>;
    } catch {
      return; // line-oriented resync: skip a non-JSON line, keep reading
    }
    const type = typeof msg.type === "string" ? msg.type : "";
    switch (type) {
      case "system":
        this.onSystem(msg);
        return;
      case "assistant":
        this.onAssistant(msg);
        return;
      case "user":
        this.onUser(msg);
        return;
      case "stream_event":
        this.onStreamEvent(msg);
        return;
      case "result":
        this.onResult(msg);
        return;
      default:
        return;
    }
  }

  private onSystem(msg: Record<string, unknown>): void {
    if (msg.subtype === "init") {
      if (typeof msg.session_id === "string") this.sessionId = msg.session_id;
      // Surface an MCP server that failed to connect so a vanished transit/task
      // tool surfaces in diagnostics rather than silently degrading the run.
      const servers = Array.isArray(msg.mcp_servers) ? (msg.mcp_servers as { name?: string; status?: string }[]) : [];
      for (const s of servers) {
        if (s && s.status && s.status !== "connected" && s.status !== "ok") {
          this.hooks.warn?.(`claude-agent: mcp server "${s.name ?? "?"}" status=${s.status}`);
        }
      }
      return;
    }
    if (msg.subtype === "api_retry" || msg.subtype === "api_error") {
      this.hooks.warn?.(`claude:system:${String(msg.subtype)}`);
    }
  }

  private onAssistant(msg: Record<string, unknown>): void {
    const message = (isObject(msg.message) ? msg.message : {}) as ClaudeAssistantMessage;
    const { message: mapped, toolUses } = mapAssistant(message);
    // Record id→name (built-in tools included) and observe transit.
    for (const tu of toolUses) {
      if (tu.id) this.toolNames.set(tu.id, tu.name);
      if (tu.name === TRANSIT_TOOL_NAME) {
        const target = parseTarget(tu.input);
        if (target) this.pendingTarget = target;
        else this.hooks.warn?.(`claude-agent: ${TRANSIT_TOOL_NAME} call had no usable target (${JSON.stringify(tu.input)})`);
      }
    }
    // The committed assistant message supersedes the live partial bubble.
    this.partials.flush();
    this.accumulator.reset();
    this.hooks.onMessage(mapped);
  }

  private onUser(msg: Record<string, unknown>): void {
    const message = (isObject(msg.message) ? msg.message : {}) as { role?: string; content?: unknown };
    for (const m of mapUser(message, this.toolNames)) this.hooks.onMessage(m);
  }

  private onStreamEvent(msg: Record<string, unknown>): void {
    if (!this.hooks.onPartial) return;
    const event = isObject(msg.event) ? (msg.event as Record<string, unknown>) : null;
    if (!event) return;
    const etype = typeof event.type === "string" ? event.type : "";
    switch (etype) {
      case "message_start": {
        const m = isObject(event.message) ? (event.message as { model?: string }) : {};
        this.accumulator.reset(typeof m.model === "string" ? m.model : undefined);
        return;
      }
      case "content_block_start": {
        const cb = isObject(event.content_block) ? (event.content_block as { type?: string; name?: string }) : {};
        this.accumulator.startBlock(num(event.index), cb);
        return;
      }
      case "content_block_delta": {
        const delta = isObject(event.delta) ? (event.delta as { type?: string; text?: string; thinking?: string }) : {};
        this.accumulator.delta(num(event.index), delta);
        const snap = this.accumulator.snapshot();
        if (snap) this.partials.push(snap);
        return;
      }
      default:
        return;
    }
  }

  private onResult(msg: Record<string, unknown>): void {
    this.streaming = false;
    this.partials.flush();
    // Anthropic emits every `tool_result` within the same turn as its `tool_use`
    // (always before that turn's `result`), so the id→name map can be reset at
    // the turn boundary — otherwise it grows unbounded over a long chat session.
    this.toolNames.clear();
    const usage = mapResultUsage(msg);
    this.hooks.onCustom("claude-agent:result", {
      usage: usage.usage,
      total_cost_usd: usage.totalCostUsd,
      subtype: usage.subtype,
      is_error: usage.isError,
    });
    this.hooks.onUsage?.(usage.usage);
    if (usage.isError) {
      this.hooks.warn?.(`claude-agent: turn ended with error (subtype=${usage.subtype || "?"})`);
    }
    this.hooks.onActivity?.(false);
    this.emitTurn("ended");
  }

  // -- internals -----------------------------------------------------------

  private beginTurn(): void {
    if (!this.streaming) {
      this.streaming = true;
      this.hooks.onActivity?.(true);
    }
  }

  private emitTurn(r: TurnEnd): void {
    if (this.turnResolve) {
      const fn = this.turnResolve;
      this.turnResolve = null;
      fn(r);
    } else {
      this.turnQueue.push(r);
    }
  }

  private onAbort(): void {
    this.aborted = true;
    this.partials.stop();
    this.emitTurn("aborted");
  }

  /** Writes a stream-json user message line (a turn / queued follow-up). */
  private writeUserMessage(message: string): void {
    this.writeRaw({ type: "user", message: { role: "user", content: [{ type: "text", text: message }] } });
  }

  /**
   * Writes a stream-json control_request to interrupt the current turn. The exact
   * envelope the CLI accepts is `{type:"control_request", request_id, request:{
   * subtype:"interrupt"}}`; if the installed CLI ignores it the queued user
   * message still lands after the current turn (queue-after-turn fallback).
   */
  private writeInterrupt(): void {
    this.writeRaw({ type: "control_request", request_id: `int-${Date.now()}`, request: { subtype: "interrupt" } });
  }

  private writeRaw(obj: Record<string, unknown>): void {
    const bytes = new TextEncoder().encode(JSON.stringify(obj) + "\n");
    void this.child.stdin.write(bytes).catch((e) => this.hooks.warn?.(`claude-agent: stdin write failed (${errMsg(e)})`));
  }
}

/**
 * Maps the transit tool arguments to a {@link StepTarget}. Accepts the primary
 * `{ to: "<step>|done|cancel|human" }` shape plus the explicit `{ step }` /
 * `{ status }` shapes for robustness (copied from pi-agent).
 */
export function parseTarget(args: unknown): StepTarget | null {
  if (args === null || typeof args !== "object") return null;
  const a = args as Record<string, unknown>;
  if (typeof a.to === "string" && a.to.trim() !== "") {
    const to = a.to.trim();
    if (to === "done" || to === "cancel" || to === "human") return { status: to };
    return { step: to };
  }
  if (typeof a.step === "string" && a.step.trim() !== "") return { step: a.step.trim() };
  if (a.status === "done" || a.status === "cancel" || a.status === "human") return { status: a.status };
  return null;
}

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/**
 * Matches ANSI / terminal control sequences (copied from pi-agent): CSI, OSC,
 * and bare two-byte escapes. Claude may emit color/control codes to stderr.
 */
const ANSI_CONTROL =
  // eslint-disable-next-line no-control-regex
  /\u001b\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\u001b\][^\u0007\u001b]*(?:\u0007|\u001b\\)|\u001b[@-_]/g;

/** Removes ANSI/terminal control sequences from a (stderr) line. */
export function stripAnsi(s: string): string {
  return s.replace(ANSI_CONTROL, "");
}
