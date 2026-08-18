/**
 * The per-session transcript writer (plan §3.2, §3.4).
 *
 * Backs `ctx.log.message` / `ctx.log.custom` and the engine's own structural
 * `autosk:*` entries. The SDK declares `log.message` / `log.custom` synchronous
 * (`void`), but appending to the `.jsonl` is async IO, so writes are funnelled
 * through a serial promise chain that preserves append order; the engine awaits
 * {@link flush} before sealing a session so no entry is lost. Each enqueued
 * append catches its own error (a transcript write must never crash a session),
 * keeping the chain free of rejections.
 */

import {
  newEntryId,
  type CustomEntry,
  type ErrorData,
  type MessageEntry,
  type SessionEndData,
  type SteerData,
  type StepTarget,
  type TranscriptEntry,
  type TranscriptMessage,
  type TransitData,
  type Usage,
  type UsageEntry,
} from "@autosk/sdk";

import type { Clock } from "../store/clock.ts";
import type { Logger } from "../store/logger.ts";
import type { SessionStore } from "../store/sessionStore.ts";

export class TranscriptWriter {
  private chain: Promise<void> = Promise.resolve();
  private readonly taken = new Set<string>();
  private usageTotal: Usage | null | undefined;

  constructor(
    private readonly sessions: SessionStore,
    private readonly sessionId: string,
    private readonly clock: Clock,
    private readonly logger: Logger,
    /** Called after each entry is durably appended (the engine's message fan-out). */
    private readonly onEntry?: (entry: TranscriptEntry) => void,
    /**
     * Called for an EPHEMERAL partial snapshot (the engine's partial fan-out).
     * Fired on the serial {@link chain} but never persisted (no disk, no cursor).
     */
    private readonly onPartial?: (message: TranscriptMessage) => void,
  ) {}

  // -- agent channels (ctx.log) -------------------------------------------

  /** Writes a pi message-schema entry. */
  message(message: TranscriptMessage): void {
    this.append({ type: "message", id: this.id(), timestamp: this.now(), message } as MessageEntry);
  }

  /** Writes a generic `custom` entry. */
  custom(customType: string, data?: unknown): void {
    const entry: CustomEntry = { type: "custom", id: this.id(), timestamp: this.now(), customType };
    if (data !== undefined) entry.data = data;
    this.append(entry);
  }

  /** Writes one canonical usage report and folds it into the session total. */
  usage(value: Usage | null): void {
    const usage = cloneUsage(value);
    this.usageTotal = mergeUsage(this.usageTotal, usage);
    this.append({
      type: "custom",
      id: this.id(),
      timestamp: this.now(),
      customType: "autosk:usage",
      data: usage,
    } satisfies UsageEntry);
  }

  /** Returns the authoritative aggregate, or `null` when a complete total is unavailable. */
  usageSummary(): Usage | null {
    return cloneUsage(this.usageTotal ?? null);
  }

  /**
   * Emits an EPHEMERAL partial (in-progress) message snapshot (`ctx.partial`).
   * Unlike {@link message}, this writes NOTHING to disk and consumes no entry
   * id or line cursor — it only fires {@link onPartial}. It is enqueued on the
   * same serial {@link chain} as the durable appends so engine-emission order
   * stays equal to the producer's event order (a partial(N+1) can never be
   * emitted before commit(N)).
   */
  partial(message: TranscriptMessage): void {
    this.chain = this.chain.then(() => {
      try {
        this.onPartial?.(message);
      } catch (e) {
        this.logger.warn(
          `transcript ${this.sessionId}: partial emit failed (${e instanceof Error ? e.message : String(e)})`,
        );
      }
    });
  }

  // -- engine structural channels (autosk:*) ------------------------------

  /** `autosk:transit` — one committed transition. */
  transit(to: StepTarget, from?: TransitData["from"]): void {
    const data: TransitData = { to };
    if (from) data.from = from;
    this.append(this.engine("autosk:transit", data));
  }

  /** `autosk:steer` — a steer/followup message injected into the live session. */
  steer(kind: SteerData["kind"], message: string): void {
    this.append(this.engine("autosk:steer", { kind, message } satisfies SteerData));
  }

  /** `autosk:error` — an error surfaced during the session. */
  error(error: string, message?: string): void {
    const data: ErrorData = { error };
    if (message !== undefined) data.message = message;
    this.append(this.engine("autosk:error", data));
  }

  /** `autosk:session_end` — the session's terminal status. */
  sessionEnd(status: SessionEndData["status"], error?: string): void {
    const data: SessionEndData = { status };
    if (error !== undefined) data.error = error;
    this.append(this.engine("autosk:session_end", data));
  }

  /** Awaits every queued append (the engine calls this before sealing a session). */
  async flush(): Promise<void> {
    await this.chain;
  }

  // -- internals ----------------------------------------------------------

  private engine(customType: string, data: unknown): CustomEntry {
    return { type: "custom", id: this.id(), timestamp: this.now(), customType, data };
  }

  private append(entry: TranscriptEntry): void {
    this.chain = this.chain.then(async () => {
      try {
        await this.sessions.appendEntry(this.sessionId, entry);
        this.onEntry?.(entry);
      } catch (e) {
        this.logger.warn(
          `transcript ${this.sessionId}: append failed (${e instanceof Error ? e.message : String(e)})`,
        );
      }
    });
  }

  private id(): string {
    const id = newEntryId(this.taken);
    this.taken.add(id);
    return id;
  }

  private now(): string {
    return this.clock();
  }
}

function cloneUsage(value: Usage | null): Usage | null {
  if (!isValidUsage(value)) return null;
  return {
    input: value.input,
    output: value.output,
    cacheRead: value.cacheRead,
    cacheWrite: value.cacheWrite,
    totalTokens: value.totalTokens,
    cost: { ...value.cost },
  };
}

function mergeUsage(total: Usage | null | undefined, next: Usage | null): Usage | null {
  if (total === null || next === null) return null;
  if (total === undefined) return next;
  const merged: Usage = {
    input: total.input + next.input,
    output: total.output + next.output,
    cacheRead: total.cacheRead + next.cacheRead,
    cacheWrite: total.cacheWrite + next.cacheWrite,
    totalTokens: total.totalTokens + next.totalTokens,
    cost: {
      input: total.cost.input + next.cost.input,
      output: total.cost.output + next.cost.output,
      cacheRead: total.cost.cacheRead + next.cost.cacheRead,
      cacheWrite: total.cost.cacheWrite + next.cost.cacheWrite,
      total: total.cost.total + next.cost.total,
    },
  };
  return isValidUsage(merged) ? merged : null;
}

function isValidUsage(value: unknown): value is Usage {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const usage = value as Partial<Usage>;
  const tokenCount = (candidate: unknown): candidate is number =>
    typeof candidate === "number" && Number.isSafeInteger(candidate) && candidate >= 0;
  if (
    !tokenCount(usage.input) ||
    !tokenCount(usage.output) ||
    !tokenCount(usage.cacheRead) ||
    !tokenCount(usage.cacheWrite) ||
    !tokenCount(usage.totalTokens)
  ) {
    return false;
  }
  if (!usage.cost || typeof usage.cost !== "object" || Array.isArray(usage.cost)) return false;
  return (["input", "output", "cacheRead", "cacheWrite", "total"] as const).every((key) => {
    const cost = usage.cost?.[key];
    return typeof cost === "number" && Number.isFinite(cost) && cost >= 0;
  });
}
