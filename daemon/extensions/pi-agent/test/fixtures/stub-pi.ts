#!/usr/bin/env bun
/**
 * `stub-pi` — a test stand-in for `pi --mode rpc` (the v2 analogue of v1's
 * `fakepi` test stub). It speaks the JSON-Lines RPC subset
 * the {@link PiDriver} relies on. The pi CLI flags (`--mode rpc -e … --model …`)
 * are ignored.
 *
 * Its scenario is read from `<cwd>/.stub-pi.json` (the autosk daemon spawns pi
 * with `cwd = ctx.cwd`, i.e. the project root). A config FILE — not env vars —
 * is used because `Bun.spawn` does not propagate a parent's runtime-mutated
 * `process.env` to the child, and the file keeps parallel test runs isolated:
 *
 * The stub models pi's RUN-ACTIVE flag (`_isAgentRunActive`): it goes up on
 * `agent_start` and down only at `agent_settled`, which pi emits AFTER its
 * post-run phase (retry backoff / auto-compaction / queued-message drain) — so
 * there is a real `agent_end → agent_settled` window, reproduced here with the
 * configurable `settleMs` delay. While the run is active a `prompt` is rejected
 * with {@link BUSY_REJECTION} (#19) and `steer` / `follow_up` are accepted;
 * when idle it is the other way round. This is what makes the driver's settled
 * gate, its idle-vs-streaming `input()` dispatch and its single state-mismatch
 * retry testable.
 *
 *   { "scenario": "transit" | "kickback_then_transit" | "never_transit"
 *                 | "steer" | "abort_hang",
 *     "to": "<autosk_transit target>",          // default "done"
 *     "transitOnTurn": <1-based turn>,           // default 2 (kickback scenario)
 *     "settleMs": <ms>                           // agent_end → agent_settled delay,
 *                                                // default $STUB_PI_SETTLE_MS or 0
 *   }
 */

import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

interface StubConfig {
  scenario: string;
  to: string;
  transitOnTurn: number;
  /** Delay (ms) between `agent_end` and `agent_settled` — pi's post-run window. */
  settleMs: number;
}

function loadConfig(): StubConfig {
  const path = join(process.cwd(), ".stub-pi.json");
  let raw: Partial<StubConfig> = {};
  if (existsSync(path)) {
    try {
      raw = JSON.parse(readFileSync(path, "utf8")) as Partial<StubConfig>;
    } catch {
      /* fall back to defaults */
    }
  }
  return {
    scenario: raw.scenario ?? "transit",
    to: raw.to ?? "done",
    transitOnTurn: raw.transitOnTurn ?? 2,
    settleMs: raw.settleMs ?? envInt("STUB_PI_SETTLE_MS"),
  };
}

/** Reads a non-negative integer env knob; 0 when unset or unparseable. */
function envInt(name: string): number {
  const n = Number.parseInt(process.env[name] ?? "", 10);
  return Number.isFinite(n) && n > 0 ? n : 0;
}

const cfg = loadConfig();

/**
 * pi's verbatim rejection of a `prompt` issued while a run is active
 * (`agent-session.js`: `prompt()` throws it when `isStreaming`, and rpc-mode
 * surfaces `e.message` unchanged). Copied exactly — this fixture is where the
 * contract is written down, and the sentence names the `streamingBehavior`
 * escape hatch that the wording alone would hide.
 */
const BUSY_REJECTION =
  "Agent is already processing. Specify streamingBehavior ('steer' or 'followUp') to queue the message.";

function emit(obj: unknown): void {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

function assistantMessage(text: string): unknown {
  return {
    role: "assistant",
    content: [{ type: "text", text }],
    provider: "stub",
    model: "stub-model",
    usage: {
      input: 1,
      output: 1,
      cacheRead: 0,
      cacheWrite: 0,
      totalTokens: 2,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
    },
    stopReason: "stop",
    timestamp: Date.now(),
  };
}

let turn = 0;
let toolCallSeq = 0;
/**
 * pi's run-active flag (`_isAgentRunActive`): up on `agent_start`, down only at
 * `agent_settled`. It gates the command contract — a `prompt` is rejected while
 * a run is active, `steer`/`follow_up` only accepted while one is (R2b) — so the
 * driver's settled gate and its idle-vs-streaming dispatch stay honest.
 */
let runActive = false;
/**
 * Bumped on every `agent_start`. A pending settle timer captures the generation
 * it belongs to and becomes a no-op if a new run started meanwhile: real pi
 * emits `agent_settled` from `_runAgentPrompt`'s `finally`, exactly ONCE per
 * cycle, so a stale timer must never clear `runActive` (nor emit a boundary) in
 * the middle of a live turn — which is what a steer/follow_up delivered inside
 * the post-run window starts.
 */
let runGeneration = 0;

function startTurn(): void {
  runGeneration += 1;
  emit({ type: "agent_start" });
  runActive = true;
}

/**
 * Ends the assistant stream, then settles the run after the configured post-run
 * window (pi's compaction / retry / queued-drain phase). The run stays ACTIVE
 * across that window, exactly like real pi — a `prompt` landing inside it is
 * rejected with {@link BUSY_REJECTION}.
 */
function endTurn(): void {
  const generation = runGeneration;
  emit({ type: "agent_end", messages: [] });
  if (cfg.settleMs > 0) {
    setTimeout(() => settle(generation), cfg.settleMs);
    return;
  }
  settle(generation);
}

/** Settles the run — unless a NEW run started inside the post-run window. */
function settle(generation: number): void {
  if (generation !== runGeneration) return; // superseded by a steer/follow_up turn
  runActive = false;
  emit({ type: "agent_settled" });
}

function emitTransit(to: string): void {
  toolCallSeq += 1;
  emit({
    type: "tool_execution_start",
    toolCallId: `call-${toolCallSeq}`,
    toolName: "autosk_transit",
    args: { to },
  });
  emit({
    type: "tool_execution_end",
    toolCallId: `call-${toolCallSeq}`,
    toolName: "autosk_transit",
    result: { content: [{ type: "text", text: `autosk: transition to "${to}" submitted.` }] },
    isError: false,
  });
}

/** Runs one "turn" in response to a prompt / steer / follow_up. */
function runTurn(message: string): void {
  turn += 1;
  startTurn();
  emit({ type: "message_end", message: assistantMessage(`ack: ${message}`) });

  switch (cfg.scenario) {
    case "transit":
      emitTransit(cfg.to);
      endTurn();
      return;
    case "kickback_then_transit":
      if (turn >= cfg.transitOnTurn) emitTransit(cfg.to);
      endTurn();
      return;
    case "never_transit":
      endTurn();
      return;
    case "steer":
      // Turn 1 hangs mid-stream (no agent_end → the run stays active). The
      // forwarded steer therefore arrives as a real `steer` command (the
      // driver's streaming branch), NOT a prompt; we run a turn for it, echo its
      // message (proving live delivery into pi), then transit.
      if (turn === 1) return;
      emitTransit(cfg.to);
      endTurn();
      return;
    case "abort_hang":
      // Hang forever — the run is ended only by an abort (signal kill / abort cmd).
      return;
    default:
      endTurn();
      return;
  }
}

function handle(cmd: Record<string, unknown>): void {
  const type = typeof cmd.type === "string" ? cmd.type : "";
  const id = typeof cmd.id === "string" ? cmd.id : "";
  const message = typeof cmd.message === "string" ? cmd.message : "";
  switch (type) {
    case "get_state":
      emit({
        type: "response",
        id,
        command: "get_state",
        success: true,
        data: { sessionId: "stub-sess", sessionFile: "/tmp/stub/session.jsonl", messageCount: 0 },
      });
      return;
    case "prompt":
      // pi accepts a fresh `prompt` ONLY when its run-active flag is clear; a
      // prompt issued before `agent_settled` is rejected with pi's real wording
      // (models real pi, and guards the driver's settled gate — #19).
      if (runActive) {
        emit({ type: "response", id, command: "prompt", success: false, error: BUSY_REJECTION });
        return;
      }
      emit({ type: "response", id, command: "prompt", success: true });
      runTurn(message);
      return;
    case "steer":
    case "follow_up":
      // steer / follow_up are valid ONLY mid-stream; when idle they are a state
      // mismatch (the driver then retries with the opposite `prompt` shape).
      if (!runActive) {
        emit({
          type: "response",
          id,
          command: type,
          success: false,
          error: "not streaming (no active run)",
        });
        return;
      }
      emit({ type: "response", id, command: type, success: true });
      runTurn(message);
      return;
    case "abort":
      emit({ type: "response", id, command: "abort", success: true });
      process.exit(0);
      return;
    case "extension_ui_response":
      return;
    default:
      emit({ type: "response", id, command: type, success: true });
      return;
  }
}

const decoder = new TextDecoder();
let buf = "";
for await (const chunk of Bun.stdin.stream()) {
  buf += decoder.decode(chunk, { stream: true });
  let nl: number;
  while ((nl = buf.indexOf("\n")) >= 0) {
    const line = buf.slice(0, nl);
    buf = buf.slice(nl + 1);
    const trimmed = line.trim();
    if (trimmed === "") continue;
    try {
      handle(JSON.parse(trimmed) as Record<string, unknown>);
    } catch {
      emit({ type: "response", id: "", command: "?", success: false, error: "parse error" });
    }
  }
}
