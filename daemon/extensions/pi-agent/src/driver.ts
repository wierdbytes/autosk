/**
 * `pi --mode rpc` wire driver — the v2 TypeScript port of v1's `pi --mode rpc`
 * driver.
 *
 * Drives a spawned `pi --mode rpc` child over JSON-Lines stdio via the engine's
 * {@link ChildHandle} (`ctx.spawn`). It:
 *  - sends commands (`prompt`, `steer`, `abort`, …) and matches `response` lines
 *    by `id`;
 *  - tracks pi's run state (`agent_start` → `agent_settled`) and turn boundaries;
 *  - observes the `autosk_transit` tool call on the event stream and exposes the
 *    requested {@link StepTarget} to the agent (the design's transit channel —
 *    "observe the tool call on pi's RPC stream and translate to `ctx.transit`",
 *    plan §3.4, resolved-#2);
 *  - mirrors pi message / custom session entries into the autosk transcript 1:1;
 *  - auto-cancels blocking `extension_ui_request` dialogs so headless runs never
 *    hang.
 */

import type { ChildHandle, StepTarget, TranscriptMessage } from "@autosk/sdk";

/** How a turn (one `prompt` cycle) ended. */
export type TurnEnd = "ended" | "exited" | "aborted";

/** Hooks the agent wires into the driver. */
export interface PiDriverHooks {
  /** Mirror a pi message-schema entry into the transcript (`ctx.log.message`). */
  onMessage(message: TranscriptMessage): void;
  /** Mirror a pi custom session entry into the transcript (`ctx.log.custom`). */
  onCustom(customType: string, data: unknown): void;
  /** The session's abort signal (`ctx.signal`). */
  signal: AbortSignal;
  /**
   * Turn-boundary activity callback: `true` when pi starts streaming a turn
   * (`agent_start`), `false` when the turn ends (`agent_end`). The interactive
   * chat loop wires this to `ctx.setActivity` so a client shows idle vs working.
   */
  onActivity?(busy: boolean): void;
  /**
   * Streams an EPHEMERAL, cumulative assistant-message snapshot as pi generates
   * a turn (from pi's `message_update`). Coalesced (~40ms) on the producer side;
   * never written to the transcript and always superseded by the committed
   * {@link onMessage}. The agent wires this to `ctx.partial`.
   */
  onPartial?(message: TranscriptMessage): void;
  /** Optional diagnostic sink. */
  warn?(message: string): void;
}

/** Min interval (ms) between coalesced partial snapshots emitted to subscribers. */
export const PARTIAL_COALESCE_MS = 40;

/**
 * How long, after an `agent_end`, the driver waits for pi's `agent_settled`
 * before concluding this pi build never emits one (legacy feature-detect, see
 * {@link PiDriver}). Detection happens ONCE per child: the first verdict sticks
 * (and a late `agent_settled`, or a busy `prompt` rejection, flips it back), so
 * a legacy pi pays this grace only on its first turn.
 *
 * The value has to exceed the longest QUIET window inside pi's post-run phase,
 * because a wrong "legacy" verdict re-opens the prompt gate while pi is still
 * running — the very race this driver exists to close (#19). The two long
 * phases announce themselves and are handled precisely instead of being waited
 * out blind (see {@link PiDriver.onPostRunSignal}): `auto_retry_start` carries
 * its own `delayMs` (2s/4s/8s under pi's stock retry settings) and EXTENDS the
 * deadline by it, and `compaction_start` SUSPENDS the deadline until
 * `compaction_end` (an unbounded summarization call). So this value only has to
 * cover the small unannounced gaps — auth refresh, `agent_end` extension
 * handlers, queued-message drain — for which 10s is generous.
 */
export const SETTLE_GRACE_MS = 10_000;

/**
 * Max `prompt` attempts before {@link PiDriver.sendPrompt} gives up. Each retry
 * waits for pi's real boundary (see {@link PiDriver.waitForGate}), so an attempt
 * is spent only when pi answers "still busy" to a prompt written at a moment we
 * believed it promptable — not once per unit of time.
 */
export const MAX_PROMPT_ATTEMPTS = 7;

/**
 * Exponential MARGIN added on top of {@link SETTLE_GRACE_MS} when waiting for
 * the gate between prompt attempts (250, 500, 1000, … ms — doubled per
 * attempt). This ceiling applies ONLY while pi's `agent_settled` support is
 * still `unknown` or has been ruled `legacy`, i.e. when the gate's re-open
 * depends on a timer verdict that could be wrong; against a `modern` pi the
 * wait is unbounded because `agent_settled` is guaranteed (see
 * {@link PiDriver.waitForGate}).
 */
export const PROMPT_RETRY_BACKOFF_MS = 250;

/**
 * Coalesces a high-frequency stream of cumulative snapshots into bounded-rate
 * emissions: the first snapshot fires immediately (leading edge) and opens a
 * min-interval window; further snapshots within the window are buffered as the
 * single latest value and flushed on the trailing edge. Loss-tolerant by design
 * — each snapshot is the whole message, so dropping an intermediate one only
 * costs a frame of freshness.
 */
export class Coalescer<T> {
  private latest: T | null = null;
  private hasLatest = false;
  private timer: ReturnType<typeof setTimeout> | null = null;

  constructor(
    private readonly intervalMs: number,
    private readonly emit: (value: T) => void,
  ) {}

  /** Offers a new snapshot (leading-edge immediate, then trailing-edge coalesced). */
  push(value: T): void {
    if (this.timer === null) {
      this.emit(value); // leading edge
      this.arm();
    } else {
      this.latest = value; // keep only the freshest for the trailing edge
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
        this.arm(); // keep coalescing while snapshots keep arriving
      }
    }, this.intervalMs);
  }

  /**
   * Emits any buffered trailing snapshot NOW and stops the timer. Called at a
   * message/turn boundary so the last partial never lingers past — nor fires
   * after — the committed durable line.
   */
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

  /**
   * Cancels the timer and DROPS any buffered snapshot WITHOUT emitting. Called on
   * teardown (child exit / abort) so an armed trailing-edge timer can never fire
   * `emit` after the session is sealed and its terminal frame delivered — which
   * would otherwise re-set a stale live bubble on the client. Unlike `flush`,
   * this emits nothing.
   */
  stop(): void {
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    this.latest = null;
    this.hasLatest = false;
  }
}

/** pi RPC extension-UI methods that expect no response (fire-and-forget). */
const FIRE_AND_FORGET = new Set(["notify", "setStatus", "setWidget", "setTitle", "set_editor_text"]);

/** The tool name the injected pi extension registers (see `pi-transit-extension.ts`). */
export const TRANSIT_TOOL_NAME = "autosk_transit";

/** Cap on how many pi stderr lines are forwarded through `warn` per session. */
const STDERR_FORWARD_CAP = 100;

interface PendingResponse {
  resolve(value: { success: boolean; error?: string; data?: unknown }): void;
  /**
   * Ran SYNCHRONOUSLY when the `response` line is handled — before the next
   * stdout line is parsed. Run-state the response carries (the cycle an accepted
   * `prompt` opens; the "still running" / "idle" truth a rejection reveals) MUST
   * be applied here rather than in the awaiting continuation: pi routinely acks
   * and then streams without yielding, so the continuation can run after the
   * whole batch — including that cycle's `agent_settled` — was processed, and
   * would then apply stale state on top of fresh events.
   */
  onResponse?(r: { success: boolean; error?: string }): void;
}

/** Test/tuning knobs; production callers use the defaults. */
export interface PiDriverOptions {
  /** Override {@link SETTLE_GRACE_MS} (the legacy-pi feature-detect window). */
  settleGraceMs?: number;
  /** Override {@link MAX_PROMPT_ATTEMPTS}. */
  maxPromptAttempts?: number;
  /** Override {@link PROMPT_RETRY_BACKOFF_MS} (the retry-wait margin unit). */
  promptRetryBackoffMs?: number;
}

/**
 * pi's per-child run-state protocol support, feature-detected at run time:
 *  - `unknown` — no `agent_settled` seen yet (we are inside the grace window);
 *  - `modern`  — this pi emits `agent_settled`; it is the turn boundary + gate;
 *  - `legacy`  — this pi never emitted one; `agent_end` is the boundary + gate.
 */
type SettleSupport = "unknown" | "modern" | "legacy";

/**
 * Drives one `pi --mode rpc` child.
 *
 * ## Run-state model (why `agent_settled`, not `agent_end`)
 *
 * pi keeps a single run-active flag (`_isAgentRunActive`) for the whole
 * `prompt()` cycle and rejects a fresh `{type:"prompt"}` with "Agent is already
 * processing" while it is set. The flag is set at `agent_start` and cleared only
 * in `_runAgentPrompt`'s `finally`, which emits `agent_settled` — AFTER the
 * post-run phase (retry backoff, auto-compaction, queued-message drain), each
 * round of which emits its own EXTRA `agent_start`/`agent_end` pair. So
 * `agent_end` marks "the assistant response finished streaming", NOT "pi is
 * promptable again".
 *
 * The driver therefore mirrors pi exactly:
 *  - `agent_end`     → flush partials + `onActivity(false)` (the UI goes idle);
 *  - `agent_settled` → the TURN BOUNDARY: clear `streaming`, open the prompt
 *    gate, resolve exactly one {@link waitForTurnEnd} per prompt cycle.
 *
 * A prompt cycle is OPENED by an accepted `prompt` (not by `agent_start`), so
 * the extra `agent_start`/`agent_end` pairs of the post-run phase can never
 * enqueue a second turn-end for the same cycle.
 *
 * ## Keeping the view honest
 *
 * The driver's picture of pi's run state is a mirror, and a mirror can be
 * wrong; three mechanisms keep it converging on the truth:
 *  - **feature detection** — legacy pi builds that never emit `agent_settled`
 *    fall back to the old `agent_end` boundary after {@link SETTLE_GRACE_MS},
 *    so the driver can never hang waiting for an event that never comes;
 *  - **post-run signals** — `auto_retry_start` / `compaction_start` /
 *    `compaction_end` prove pi is still working inside the window and reshape
 *    that deadline (see {@link onPostRunSignal}), so a modern pi in a long
 *    compaction or retry backoff is not mistaken for a legacy one;
 *  - **rejection healing** — pi's own "Agent is already processing" / "not
 *    streaming" rejections are ground truth about its run state: they correct
 *    `streaming`/`settled`, un-learn a wrong `legacy` verdict, and let the
 *    retry ride the real boundary instead of a fixed sleep.
 */
export class PiDriver {
  private nextId = 0;
  private readonly pending = new Map<string, PendingResponse>();
  private streaming = false;
  private exited = false;
  private aborted = false;
  private shuttingDown = false;
  exitCode: number | null = null;

  /**
   * `true` while pi is promptable (its run-active flag is clear). Starts `true`
   * (a fresh pi is idle), goes `false` when pi accepts a `prompt` (or at
   * `agent_start`, whichever we see first) and back to `true` at the cycle
   * boundary — plus unconditionally on child exit / abort so no waiter can hang
   * on a dead pi.
   */
  private settled = true;
  /** Resolvers parked in {@link waitUntilSettled}. */
  private settleWaiters: (() => void)[] = [];
  /** Per-child feature detection of pi's `agent_settled` event. */
  private settleSupport: SettleSupport = "unknown";
  /** Armed at `agent_end` while support is `unknown` (legacy-detect deadline). */
  private settleGraceTimer: ReturnType<typeof setTimeout> | null = null;
  /**
   * `true` while a `compaction_start` has SUSPENDED that deadline (the summarizing
   * LLM call is unbounded); the paired `compaction_end` resumes it. The suspended
   * state is the second form a pending deadline can take — see
   * {@link clearSettleGrace}.
   */
  private compactionSuspended = false;
  /**
   * Whether the current prompt cycle already produced its single turn-end. A
   * cycle is opened by an ACCEPTED `prompt` ({@link openCycle}) and closed by the
   * next boundary event, so pi's intra-cycle `agent.continue()` rounds — and any
   * boundary event arriving with no cycle open — can never enqueue a phantom
   * turn. Starts `true`: nothing is open before the first prompt.
   */
  private turnEnded = true;

  private readonly settleGraceMs: number;
  private readonly maxPromptAttempts: number;
  private readonly promptRetryBackoffMs: number;

  /** Buffered turn-end events (so a boundary that races the await isn't lost). */
  private readonly turnQueue: TurnEnd[] = [];
  private turnResolve: ((r: TurnEnd) => void) | null = null;

  /** The transit target observed during the current turn, or `null`. */
  private pendingTarget: StepTarget | null = null;

  /** How many pi stderr lines have already been forwarded through `warn`. */
  private stderrForwarded = 0;

  /** Coalesces `message_update` snapshots into bounded-rate `onPartial` calls. */
  private readonly partials: Coalescer<TranscriptMessage>;

  constructor(
    private readonly child: ChildHandle,
    private readonly hooks: PiDriverHooks,
    opts: PiDriverOptions = {},
  ) {
    this.settleGraceMs = opts.settleGraceMs ?? SETTLE_GRACE_MS;
    this.maxPromptAttempts = opts.maxPromptAttempts ?? MAX_PROMPT_ATTEMPTS;
    this.promptRetryBackoffMs = opts.promptRetryBackoffMs ?? PROMPT_RETRY_BACKOFF_MS;
    this.partials = new Coalescer<TranscriptMessage>(
      PARTIAL_COALESCE_MS,
      (m) => this.hooks.onPartial?.(m),
    );
    child.onStdout((line) => this.onLine(line));
    // Always subscribe (so the pipe is drained and never fills), but forward the
    // bytes through `warn` instead of black-holing them: when pi dies on a
    // runtime/module error its stack trace lives ONLY on stderr.
    child.onStderr((line) => this.onStderrLine(line));
    void child.exited.then(({ code }) => {
      this.exitCode = code;
      this.exited = true;
      // Drop any buffered/armed partial WITHOUT emitting: the session is being
      // torn down, so a late trailing-edge frame must not fire after its terminal
      // done/error (see Coalescer.stop). `flush` would emit one more stale frame.
      this.partials.stop();
      for (const p of this.pending.values()) p.resolve({ success: false, error: "pi exited" });
      this.pending.clear();
      // Release the prompt gate: a dead pi will never emit `agent_settled`, and a
      // `sendPrompt` parked on the gate must fail fast with "pi exited" instead
      // of hanging forever.
      this.openGate();
      this.emitTurn(this.aborted ? "aborted" : "exited");
    });
    if (hooks.signal.aborted) this.onAbort();
    else hooks.signal.addEventListener("abort", () => this.onAbort(), { once: true });
  }

  // -- outbound ------------------------------------------------------------

  /**
   * Sends a `prompt`; resolves once pi acks acceptance. Throws if rejected.
   *
   * Waits for pi to be SETTLED first (its run-active flag clear) — a prompt
   * written in the `agent_end → agent_settled` window is rejected with "Agent is
   * already processing" and would fail the session (#19).
   *
   * A residual busy rejection means our view of pi was wrong (a legacy verdict
   * mis-fired, or we raced pi's own state change). That rejection is also PROOF
   * that pi is still running, so it heals the view ({@link noteStillRunning}) and
   * the next attempt WAITS FOR THE REAL BOUNDARY — `agent_settled`, the re-armed
   * legacy verdict, or child exit / abort — instead of a fixed sleep, so the
   * budget scales with pi rather than with a magic number (see
   * {@link waitForGate}). The attempt count is bounded by
   * {@link MAX_PROMPT_ATTEMPTS}. Any other rejection (including "pi exited")
   * fails immediately.
   */
  async sendPrompt(message: string): Promise<void> {
    // The gate: on the first write we wait for pi as long as it takes (released
    // by `agent_settled`, the legacy `agent_end`, child exit or abort).
    await this.waitUntilSettled();
    let error = "unknown";
    for (let attempt = 1; attempt <= this.maxPromptAttempts; attempt++) {
      // Run-state healing is shared with every other command (see
      // {@link applyResponse}) so a `prompt` can never drift from it.
      const resp = await this.request({ type: "prompt", message }, (r) => this.applyResponse(r, "prompt"));
      if (resp.success) return;
      error = resp.error ?? "unknown";
      // Only a "pi is busy" rejection is transient; everything else (a dead pi, a
      // malformed command, …) will not get better by waiting.
      if (!isBusyRejection(error) || this.exited || this.aborted) break;
      if (attempt < this.maxPromptAttempts) await this.waitForGate(attempt);
    }
    throw new Error(`pi rejected prompt: ${error}`);
  }

  /**
   * Waits for the prompt gate to re-open before the next {@link sendPrompt}
   * attempt, having just been told by pi that a run is active.
   *
   * Against a **modern** pi the wait is UNBOUNDED: `agent_settled` is emitted
   * from `_runAgentPrompt`'s `finally`, so a pi that reports itself busy will
   * always settle eventually — and {@link openGate} additionally fires on child
   * exit and on abort, which is what makes an unbounded wait safe (AC4). A
   * ceiling here would be actively harmful: a real agent turn runs for minutes,
   * so a timed retry would hammer a legitimately busy pi and burn the whole
   * attempt budget against a run it must simply wait out (#19 again).
   *
   * While support is `unknown` or has been ruled `legacy`, the gate's re-open
   * depends on the feature-detect TIMER rather than on an event pi guarantees,
   * so the wait is raced against a ceiling ({@link retryWaitFor}) — a wedged
   * gate must not hang the session. The losing timer is always cancelled: it
   * outlives the call by up to a grace otherwise.
   */
  private async waitForGate(attempt: number): Promise<void> {
    if (this.settleSupport === "modern") {
      await this.waitUntilSettled();
      return;
    }
    const ceiling = cancellableDelay(this.retryWaitFor(attempt));
    try {
      await Promise.race([this.waitUntilSettled(), ceiling.promise]);
    } finally {
      ceiling.cancel();
    }
  }

  /**
   * Resolves once pi is promptable again (or the child exited / the session was
   * aborted, so a waiter can never hang on a dead pi).
   */
  waitUntilSettled(): Promise<void> {
    if (this.settled || this.exited || this.aborted) return Promise.resolve();
    return new Promise<void>((resolve) => this.settleWaiters.push(resolve));
  }

  /**
   * Forwards a steer/followup into the live pi (plan §3.4), mirroring v1's
   * `dispatch_input` / `build_input_command` (crates/autoskd/src/server.rs). The
   * pi input command TYPE depends on whether a pi run is currently ACTIVE (our
   * `streaming` view now tracks pi's run-active flag exactly: up on an accepted
   * `prompt` / at `agent_start`, down at `agent_settled`), so an input landing in the
   * `agent_end → agent_settled` post-run window travels as a `steer`/`follow_up`
   * — which pi queues and drains — instead of a doomed `prompt`:
   *  - idle (no active run)  → `{ type: "prompt", message }` — starts a fresh turn;
   *  - streaming + steer     → `{ type: "steer", message }`;
   *  - streaming + followup  → `{ type: "follow_up", message }` (snake_case — the
   *    dedicated pi command type; see {@link buildInputCommand} for why not the
   *    `streamingBehavior` field on `prompt`).
   * On a state-mismatch rejection (our streaming view raced pi's), HEAL the view
   * from pi's answer — "Agent is already processing" proves a run is active,
   * "not streaming" proves none is — then retry ONCE with the opposite dispatch
   * shape. Healing matters beyond this call: without it every subsequent input
   * in the same window would rebuild the same wrong shape and pay the same
   * double round-trip. Best-effort — never throws; failures are surfaced
   * through `warn`.
   */
  async input(kind: "steer" | "followup", message: string): Promise<void> {
    try {
      const streaming = this.streaming;
      const first = buildInputCommand(kind, message, streaming);
      const resp = await this.request(first.cmd, (r) => this.applyResponse(r, first.label));
      if (resp.success) return;
      // A busy rejection ("Agent is already processing") is the prompt-side face
      // of the same race, so it retries with the opposite shape too.
      if (!isStateMismatch(resp.error) && !isBusyRejection(resp.error)) {
        this.hooks.warn?.(`pi-agent: pi rejected ${first.label} (${resp.error ?? "unknown"})`);
        return;
      }
      // State-mismatch: our streaming view raced pi's. pi's rejection IS its run
      // state and {@link applyResponse} has already adopted it, so the retry with
      // the opposite dispatch shape (v1 `dispatch_input`) is now also the shape
      // the NEXT input will pick first try.
      const retry = buildInputCommand(kind, message, !streaming);
      const retryResp = await this.request(retry.cmd, (r) => this.applyResponse(r, retry.label));
      if (retryResp.success) return;
      this.hooks.warn?.(
        `pi-agent: pi rejected ${retry.label} after retry from ${first.label} (${retryResp.error ?? "unknown"})`,
      );
    } catch (e) {
      this.hooks.warn?.(`pi-agent: forwarding ${kind} failed (${errMsg(e)})`);
    }
  }

  /**
   * Waits for the current PROMPT CYCLE to end (or the child to exit / be
   * aborted). Resolves exactly once per `sendPrompt`: on a modern pi the
   * boundary is `agent_settled`, so the extra `agent_start`/`agent_end` pairs
   * pi's post-run phase (compaction, retries, queued-message drain) emits do NOT
   * produce spurious turn-ends. On a legacy pi (no `agent_settled`) the boundary
   * falls back to `agent_end`.
   */
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
   * Graceful pi shutdown: `abort` command, close stdin, brief grace, then kill.
   * Idempotent — both `onAbort` and `onRun`'s finally can call it for one session.
   */
  async shutdown(graceMs = 500): Promise<void> {
    if (this.shuttingDown) return;
    this.shuttingDown = true;
    this.writeRaw({ type: "abort" }); // fire-and-forget; pi may already be gone
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

  /** Forwards a (bounded count of) pi stderr lines through the `warn` sink. */
  private onStderrLine(line: string): void {
    // Strip ANSI/terminal control sequences first: pi writes a burst of these to
    // stderr on teardown (disable mouse tracking, leave the alt-screen, end
    // synchronized output, …) even under `--mode rpc`, and they'd otherwise land
    // verbatim as an unreadable final `pi:stderr:` line in the transcript.
    const trimmed = stripAnsi(line).trim();
    if (trimmed === "") return; // pure-control teardown / blank → drained, not surfaced
    if (this.stderrForwarded >= STDERR_FORWARD_CAP) return;
    this.stderrForwarded++;
    this.hooks.warn?.(`pi:stderr: ${trimmed}`);
    if (this.stderrForwarded === STDERR_FORWARD_CAP) {
      this.hooks.warn?.(`pi:stderr: (further stderr suppressed after ${STDERR_FORWARD_CAP} lines)`);
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
      case "response":
        this.deliverResponse(msg);
        return;
      case "agent_start":
        this.onAgentStart();
        return;
      case "agent_end":
        this.onAgentEnd();
        return;
      case "agent_settled":
        this.onAgentSettled();
        return;
      // pi's post-run phase, narrating itself: the run is STILL ACTIVE here (see
      // `onPostRunSignal`). `auto_retry_start` announces the exact backoff it is
      // about to sleep; a compaction brackets an unbounded summarization call.
      case "auto_retry_start":
        this.onPostRunSignal("retry", nonNegativeNumber(msg.delayMs));
        return;
      case "compaction_start":
        this.onPostRunSignal("compaction_start");
        return;
      case "compaction_end":
        this.onPostRunSignal("compaction_end");
        return;
      case "tool_execution_start":
        this.observeToolCall(msg);
        return;
      case "message_update":
        this.observePartial(msg.message);
        return;
      case "message_start":
        return;
      case "message_end":
        // Flush any buffered partial, then commit the durable line: the committed
        // message supersedes the live bubble and no partial follows it.
        this.partials.flush();
        this.mirrorMessage(msg.message);
        return;
      case "extension_ui_request":
        this.replyToExtensionUi(msg);
        return;
      default:
        return;
    }
  }

  private deliverResponse(msg: Record<string, unknown>): void {
    const id = typeof msg.id === "string" ? msg.id : "";
    const p = this.pending.get(id);
    if (!p) return;
    this.pending.delete(id);
    const success = msg.success === true;
    const error = typeof msg.error === "string" ? msg.error : undefined;
    // Synchronously, so the run-state it records is in place before the NEXT line
    // of the same stdout batch is handled (see PendingResponse.onResponse).
    p.onResponse?.({ success, error });
    p.resolve({ success, error, data: msg.data });
  }

  private observeToolCall(msg: Record<string, unknown>): void {
    if (msg.toolName !== TRANSIT_TOOL_NAME) return;
    const target = parseTarget(msg.args);
    if (target) this.pendingTarget = target;
    else this.hooks.warn?.(`pi-agent: ${TRANSIT_TOOL_NAME} call had no usable target (${JSON.stringify(msg.args)})`);
  }

  /**
   * Feeds a cumulative `message_update` snapshot into the partial coalescer. Only
   * assistant snapshots stream (user/toolResult messages have no in-progress
   * form); a missing `onPartial` hook makes this inert.
   */
  private observePartial(message: unknown): void {
    if (!this.hooks.onPartial) return;
    if (message === null || typeof message !== "object") return;
    const m = message as Record<string, unknown>;
    if (m.role !== "assistant") return;
    this.partials.push(m as unknown as TranscriptMessage);
  }

  private mirrorMessage(message: unknown): void {
    if (message === null || typeof message !== "object") return;
    const m = message as Record<string, unknown>;
    const role = m.role;
    if (role === "user" || role === "assistant" || role === "toolResult") {
      this.hooks.onMessage(m as unknown as TranscriptMessage);
    } else if (typeof m.customType === "string") {
      this.hooks.onCustom(m.customType, m);
    }
  }

  private replyToExtensionUi(msg: Record<string, unknown>): void {
    const id = typeof msg.id === "string" ? msg.id : "";
    const method = typeof msg.method === "string" ? msg.method : "";
    if (id === "" || FIRE_AND_FORGET.has(method)) return;
    this.writeRaw({ type: "extension_ui_response", id, cancelled: true });
  }

  // -- run state (agent_start / agent_end / agent_settled) -----------------

  /** pi's run-active flag went up: a (sub-)run is streaming, prompts are refused. */
  private onAgentStart(): void {
    this.clearSettleGrace();
    this.streaming = true;
    this.settled = false;
    // NOTE: the CYCLE is opened by an accepted `prompt` ({@link openCycle}), not
    // here — pi's post-run rounds (`agent.continue()`) emit their own
    // `agent_start`, and they belong to the cycle that is already open.
    this.hooks.onActivity?.(true);
  }

  /**
   * The assistant response finished streaming. This is NOT pi's run boundary
   * (its post-run phase may still compact / retry / drain queued messages), so
   * only the presentation side settles here: flush partials and report idle. The
   * gate and the turn-end wait for `agent_settled` — unless this pi build has
   * been detected as legacy (or is about to be, when the grace expires).
   */
  private onAgentEnd(): void {
    // Stop coalescing at the message boundary so no buffered partial fires after
    // the turn's committed line (message_end already flushed within-message).
    this.partials.flush();
    this.hooks.onActivity?.(false);
    if (this.settleSupport === "legacy") {
      this.closeCycle(); // on a legacy pi this IS the boundary
      return;
    }
    if (this.settleSupport === "unknown") this.armSettleGrace();
  }

  /**
   * pi's post-run phase announced itself: `auto_retry_start` (a provider retry,
   * carrying the `delayMs` it is about to sleep), `compaction_start` (an
   * unbounded summarization LLM call) or `compaction_end`. Each is proof that
   * the run is STILL ACTIVE even though `agent_end` already fired — exactly the
   * phases that make the post-run window long (#19).
   *
   * Only the legacy feature-detect deadline cares: `modern` has its boundary and
   * `legacy` has committed (a wrong verdict there is healed by the busy
   * rejection it produces, see {@link noteStillRunning} — re-testing on every
   * post-run signal would instead make a genuinely legacy pi re-pay the grace
   * over and over). While still `unknown`, the deadline is reshaped to the
   * announced phase: extended by a known sleep, or suspended entirely for a
   * compaction (whose `compaction_end` counterpart is guaranteed) rather than
   * guessed at with a fixed grace.
   *
   * Strictly RESHAPING: a signal may extend, suspend or resume a deadline that
   * an `agent_end` created, never invent one. The auto paths only ever fire from
   * pi's `_handlePostAgentRun`, but `compaction_start`/`compaction_end` are also
   * emitted by the MANUAL `compact()` command, which runs outside any prompt
   * cycle — and a legacy verdict must never be reached from an event that says
   * nothing about whether this pi emits `agent_settled`.
   */
  private onPostRunSignal(kind: "retry" | "compaction_start" | "compaction_end", announcedMs = 0): void {
    if (this.settleSupport !== "unknown") return;
    switch (kind) {
      case "retry":
        if (this.settleGraceTimer === null) return; // no deadline to extend
        this.armSettleGrace(announcedMs);
        return;
      case "compaction_start":
        if (this.settleGraceTimer === null) return; // no deadline to suspend
        this.clearSettleGrace();
        this.compactionSuspended = true; // resumed by `compaction_end`
        return;
      case "compaction_end":
        // Resume ONLY the deadline a `compaction_start` suspended (armSettleGrace
        // clears the flag through clearSettleGrace).
        if (!this.compactionSuspended) return;
        this.armSettleGrace();
        return;
    }
  }

  /** pi's run-active flag went down: the prompt cycle is over and pi is promptable. */
  private onAgentSettled(): void {
    this.settleSupport = "modern"; // proof; also heals a wrong `legacy` verdict
    this.clearSettleGrace();
    this.closeCycle();
  }

  /**
   * Ends the current prompt cycle: pi is promptable again and the single
   * turn-end fires (at most once per cycle — a late `agent_settled` after a
   * legacy-fallback `agent_end` must not enqueue a second one).
   */
  private closeCycle(): void {
    this.streaming = false;
    this.openGate();
    if (this.turnEnded) return;
    this.turnEnded = true;
    this.emitTurn("ended");
  }

  /**
   * Adopts what a command's `response` says about pi's run state, in stream
   * order (see {@link PendingResponse.onResponse}): an accepted `prompt` opens a
   * cycle, a busy rejection proves a run is active, a "not streaming" rejection
   * proves none is. Anything else (a real error) leaves the view alone.
   *
   * EVERY command routes through here, `prompt` included: the rejection strings
   * pi can answer with are not a set this driver controls (rpc-mode surfaces
   * `session.prompt()`'s message verbatim), so the healing must not depend on
   * which command was in flight. The arms are keyed off pi's answer, not off
   * `label`; `label` only marks which success opens a cycle.
   */
  private applyResponse(r: { success: boolean; error?: string }, label: string): void {
    if (r.success) {
      if (label === "prompt") this.openCycle();
      return;
    }
    if (isBusyRejection(r.error)) this.noteStillRunning();
    else if (isStateMismatch(r.error)) this.noteIdle();
  }

  /**
   * Records that pi ACCEPTED a `prompt`: its run-active flag is now set (pi sets
   * it before acking, so no second prompt may be written) and a fresh cycle is
   * open — the next boundary event resolves exactly one {@link waitForTurnEnd}.
   */
  private openCycle(): void {
    this.streaming = true;
    this.settled = false;
    this.turnEnded = false;
  }

  /**
   * pi told us it is still running (a busy rejection of a `prompt` / a
   * `follow_up`-shaped mismatch). That answer is ground truth, so: re-close the
   * gate, restore the streaming view — and un-learn a `legacy` verdict, which
   * this rejection just disproved (the verdict re-opened the gate mid post-run
   * phase, #19). The cycle is NOT re-opened: its turn-end has already been
   * delivered, and a second one would burn a correction from the agent's
   * kickback budget.
   */
  private noteStillRunning(): void {
    this.streaming = true;
    this.settled = false;
    if (this.settleSupport === "legacy") {
      this.settleSupport = "unknown";
      this.hooks.warn?.(
        "pi-agent: pi is still running past its agent_end boundary — re-testing for agent_settled",
      );
    }
    if (this.settleSupport === "unknown") this.armSettleGrace();
  }

  /**
   * pi told us it is NOT running ("not streaming (no active run)" in answer to a
   * steer/follow_up). The mirror of {@link noteStillRunning}: adopt the idle view
   * so the next input is dispatched as a `prompt` on the first try, and release
   * the gate — pi is promptable right now.
   */
  private noteIdle(): void {
    this.streaming = false;
    this.openGate();
  }

  /** Marks pi promptable and releases everyone parked on {@link waitUntilSettled}. */
  private openGate(): void {
    this.settled = true;
    const waiters = this.settleWaiters;
    this.settleWaiters = [];
    for (const w of waiters) w();
  }

  /**
   * Ceiling on the wait for the gate before prompt attempt `n+1`, used ONLY
   * while support is `unknown`/`legacy` (see {@link waitForGate}).
   *
   * In those states {@link noteStillRunning} re-arms the feature-detect grace,
   * so the gate re-opens within one {@link SETTLE_GRACE_MS} — by the re-fired
   * verdict at the latest, and unconditionally on exit/abort. The ceiling is
   * therefore one grace plus an exponential margin (`base · 2^(n-1)`): a real
   * re-open always wins the race, and the ceiling only ever fires if the gate is
   * truly wedged — which must not hang the session.
   */
  private retryWaitFor(attempt: number): number {
    return this.settleGraceMs + this.promptRetryBackoffMs * 2 ** (attempt - 1);
  }

  /**
   * Starts (or restarts) the legacy feature-detect deadline: if no
   * `agent_settled` arrives within the grace, this pi build is assumed not to
   * emit the event and the driver reverts to the historical `agent_end`
   * boundary. `extraMs` prepends a quiet window pi has ANNOUNCED it is about to
   * spend (a retry backoff's `delayMs`), so an announced pause is waited out
   * rather than counted as evidence of a legacy pi.
   */
  private armSettleGrace(extraMs = 0): void {
    this.clearSettleGrace();
    const waitMs = this.settleGraceMs + Math.max(0, extraMs);
    this.settleGraceTimer = setTimeout(() => {
      this.settleGraceTimer = null;
      if (this.settleSupport !== "unknown") return;
      this.settleSupport = "legacy";
      this.hooks.warn?.(
        `pi-agent: no agent_settled within ${waitMs}ms of agent_end — falling back to the agent_end turn boundary`,
      );
      this.closeCycle();
    }, waitMs);
    // Never hold the daemon's event loop open just for the detect deadline.
    (this.settleGraceTimer as { unref?: () => void }).unref?.();
  }

  /**
   * Drops the detect deadline in EITHER of its live forms — an armed timer or a
   * compaction suspension — so the invariant stays "a deadline is pending, or
   * suspended, or absent", and a stale suspension can never be resumed by a much
   * later `compaction_end`.
   */
  private clearSettleGrace(): void {
    this.compactionSuspended = false;
    if (this.settleGraceTimer === null) return;
    clearTimeout(this.settleGraceTimer);
    this.settleGraceTimer = null;
  }

  // -- internals -----------------------------------------------------------

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
    // Same teardown guard as child-exit: cancel + drop any buffered partial so an
    // armed timer cannot emit after the aborted session is sealed.
    this.partials.stop();
    this.clearSettleGrace();
    // An aborted pi will never settle: release the gate so a parked `sendPrompt`
    // (and any other waiter) unblocks instead of hanging.
    this.openGate();
    this.emitTurn("aborted");
  }

  private request(
    cmd: Record<string, unknown>,
    onResponse?: (r: { success: boolean; error?: string }) => void,
  ): Promise<{ success: boolean; error?: string; data?: unknown }> {
    const id = `d${++this.nextId}`;
    return new Promise((resolve) => {
      // Fail fast instead of parking a pending response that can never arrive:
      // a dead (or aborted-and-being-killed) pi answers nothing.
      if (this.exited) {
        resolve({ success: false, error: "pi exited" });
        return;
      }
      if (this.aborted) {
        resolve({ success: false, error: "pi aborted" });
        return;
      }
      this.pending.set(id, { resolve, onResponse });
      this.writeRaw({ id, ...cmd });
    });
  }

  private writeRaw(obj: Record<string, unknown>): void {
    const bytes = new TextEncoder().encode(JSON.stringify(obj) + "\n");
    void this.child.stdin.write(bytes).catch((e) => this.hooks.warn?.(`pi-agent: stdin write failed (${errMsg(e)})`));
  }
}

/**
 * Maps the `autosk_transit` tool arguments to a {@link StepTarget}. Accepts the
 * primary `{ to: "<step>|done|cancel|human" }` shape plus the explicit
 * `{ step }` / `{ status }` shapes for robustness.
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

/**
 * Builds the pi input command for a steer/followup given pi's streaming state
 * (port of v1 `build_input_command`, crates/autoskd/src/server.rs). While idle
 * pi only takes a fresh `prompt`; while streaming a steer maps to `steer` and a
 * followup to `follow_up` — the dedicated pi command TYPES.
 *
 * pi's rpc mode does ALSO accept a `streamingBehavior: "steer" | "followUp"`
 * field on `prompt` and queues rather than rejects when it is present (it is
 * forwarded straight into `session.prompt()`), so that shape is a second way to
 * reach the same two calls. The dedicated commands are preferred because they
 * say what they mean on the wire, they are what v1 sent, and — unlike a queued
 * `prompt` — they cannot be mistaken for the start of a new prompt CYCLE, which
 * is what the driver's turn accounting is built on.
 */
export function buildInputCommand(
  kind: "steer" | "followup",
  message: string,
  streaming: boolean,
): { cmd: Record<string, unknown>; label: string } {
  if (!streaming) return { cmd: { type: "prompt", message }, label: "prompt" };
  if (kind === "followup") return { cmd: { type: "follow_up", message }, label: "follow_up" };
  return { cmd: { type: "steer", message }, label: "steer" };
}

/**
 * Conservative state-mismatch detector (port of v1 `is_state_mismatch`). A
 * `true` here means pi rejected an input command because our streaming view
 * raced its own — the cue to retry with the opposite dispatch shape.
 */
export function isStateMismatch(error: string | undefined): boolean {
  if (!error || error === "") return false;
  const lower = error.toLowerCase();
  const tokens = [
    "not streaming",
    "already streaming",
    "no run",
    "no active run",
    "no_active_run",
    "idle",
    "in_progress",
    "state mismatch",
    "state_mismatch",
  ];
  return tokens.some((t) => lower.includes(t));
}

/**
 * `true` when pi rejected a `prompt` because a run is still active. pi 0.82.1
 * words it "Agent is already processing. Specify streamingBehavior ('steer' or
 * 'followUp') to queue the message."; the equivalent phrasings of other builds
 * match too. Such a rejection is TRANSIENT — pi becomes promptable again at
 * `agent_settled` — so {@link PiDriver.sendPrompt} waits for that boundary and
 * retries instead of failing the session. Deliberately narrower than
 * {@link isStateMismatch}, which also covers the "not streaming" direction (a
 * steer/followup dispatch mismatch).
 */
export function isBusyRejection(error: string | undefined): boolean {
  if (!error || error === "") return false;
  const lower = error.toLowerCase();
  const tokens = ["already processing", "already streaming", "already running", "in_progress", "in progress"];
  return tokens.some((t) => lower.includes(t));
}

/**
 * A cancellable sleep. The caller MUST `cancel()` once the value it was racing
 * has arrived: these ceilings are seconds long, fire after the racing call has
 * returned, and several can be in flight per prompt — an abandoned handle would
 * hold the daemon's event loop open long after the session is done. `unref` is
 * the second line of defence for the same reason `armSettleGrace` uses it.
 */
function cancellableDelay(ms: number): { promise: Promise<void>; cancel(): void } {
  let timer: ReturnType<typeof setTimeout> | null = null;
  const promise = new Promise<void>((resolve) => {
    timer = setTimeout(() => {
      timer = null;
      resolve();
    }, ms);
    (timer as { unref?: () => void }).unref?.();
  });
  return {
    promise,
    cancel: () => {
      if (timer === null) return;
      clearTimeout(timer);
      timer = null;
    },
  };
}

/** Reads a non-negative numeric event field (pi's `delayMs`); `0` when absent/junk. */
function nonNegativeNumber(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) && v > 0 ? v : 0;
}

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/**
 * Matches ANSI / terminal control sequences:
 *  - CSI: `ESC [` + parameter bytes (`0x30-0x3f`, covers `?`/`<`/`>`/`=`/digits/`;`)
 *    + intermediate bytes (`0x20-0x2f`) + a final byte (`0x40-0x7e`);
 *  - OSC: `ESC ]` … terminated by BEL (`0x07`) or ST (`ESC \`);
 *  - bare two-byte escapes: `ESC` + a single byte in `0x40-0x5f`.
 * pi emits a teardown burst of these (mouse tracking off, leave alt-screen, end
 * synchronized output) on exit; the example `\u001b[?2026h…\u001b[?2026l` is all CSI.
 */
const ANSI_CONTROL =
  // eslint-disable-next-line no-control-regex
  /\u001b\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\u001b\][^\u0007\u001b]*(?:\u0007|\u001b\\)|\u001b[@-_]/g;

/** Removes ANSI/terminal control sequences from a (stderr) line. */
export function stripAnsi(s: string): string {
  return s.replace(ANSI_CONTROL, "");
}
