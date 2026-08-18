/**
 * On-disk record shapes + their canonical serialisers (plan §3.1, §3.2).
 *
 * These bytes are the **new public contract** (acceptance: "golden tests pin
 * the on-disk formats … any byte-level change must fail a test"). To keep the
 * output byte-stable regardless of how a caller built the in-memory object, the
 * serialisers re-assemble each record with its keys in a FIXED order before
 * `JSON.stringify` — never trusting incidental insertion order.
 *
 *   - `task.json`     — pretty-printed (2-space) + trailing "\n" (human-editable).
 *   - `comments.jsonl`— one compact JSON object per line, each line "\n"-terminated.
 *   - session `.json` — pretty-printed (2-space) + trailing "\n".
 *   - session `.jsonl`— header line + one compact entry per line, "\n"-terminated.
 */

import type {
  Comment,
  SessionMeta,
  SessionStatus,
  TaskStatus,
  SessionHeader,
  TranscriptLine,
  Usage,
} from "@autosk/sdk";

import { isEmptyMetadata } from "./metadata.ts";

// ---------------------------------------------------------------------------
// Stored task (`tasks/<id>/task.json`). Note `blocked_by` is a flat string[] of
// task ids on disk; the derived `TaskView.blocked_by` (TaskRef[]) and `blocks`
// are computed by the store, never stored (plan §3.1).
// ---------------------------------------------------------------------------

export interface StoredTask {
  id: string;
  title: string;
  description: string;
  status: TaskStatus;
  workflow: string | null;
  step: string | null;
  blocked_by: string[];
  /**
   * Free-form, human-editable key/value bag (plan §2). Always an object
   * in memory ({} when none); the engine reserves the `step_visits` sub-object.
   * It is OMITTED from the serialised `task.json` when empty (see
   * {@link serializeTask}) so pre-existing files round-trip byte-for-byte.
   */
  metadata: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

/** The five engine-owned vs human-editable fields split (plan §3.7(2)). */
export const ENGINE_OWNED_TASK_FIELDS = ["status", "step", "workflow"] as const;

/**
 * Serialises a task to its canonical `task.json` bytes.
 *
 * `metadata` occupies a fixed slot (after `blocked_by`, before `created_at`)
 * but is OMITTED entirely when empty, so a task that never carried metadata
 * serialises byte-for-byte identically to the pre-metadata format (the golden
 * contract). A non-empty bag is serialised verbatim (its own key order is
 * preserved as authored — it is opaque free-form data).
 */
export function serializeTask(t: StoredTask): string {
  const ordered: Record<string, unknown> = {
    id: t.id,
    title: t.title,
    description: t.description,
    status: t.status,
    workflow: t.workflow,
    step: t.step,
    blocked_by: [...t.blocked_by],
  };
  if (!isEmptyMetadata(t.metadata)) ordered.metadata = t.metadata;
  ordered.created_at = t.created_at;
  ordered.updated_at = t.updated_at;
  return JSON.stringify(ordered, null, 2) + "\n";
}

/** Parses + validates `task.json` bytes into a {@link StoredTask}. */
export function parseTask(text: string): StoredTask {
  const raw = JSON.parse(text) as Record<string, unknown>;
  if (typeof raw.id !== "string" || raw.id.length === 0) {
    throw new Error("task.json: missing/invalid id");
  }
  const status = raw.status as TaskStatus;
  if (!isTaskStatus(status)) {
    throw new Error(`task.json: invalid status ${JSON.stringify(raw.status)}`);
  }
  return {
    id: raw.id,
    title: typeof raw.title === "string" ? raw.title : "",
    description: typeof raw.description === "string" ? raw.description : "",
    status,
    workflow: typeof raw.workflow === "string" ? raw.workflow : null,
    step: typeof raw.step === "string" ? raw.step : null,
    blocked_by: normalizeIdList(raw.blocked_by),
    metadata: normalizeMetadata(raw.metadata),
    created_at: typeof raw.created_at === "string" ? raw.created_at : "",
    updated_at: typeof raw.updated_at === "string" ? raw.updated_at : "",
  };
}

/**
 * Defensively coerces an on-disk `metadata` value into a plain object. A
 * missing, null, array, or otherwise non-object value (a fat-fingered human
 * edit under hybrid ownership) is treated as no metadata — `{}` — rather than
 * throwing, so one bad edit never bricks a `task.json` parse.
 */
function normalizeMetadata(v: unknown): Record<string, unknown> {
  if (typeof v !== "object" || v === null || Array.isArray(v)) return {};
  return { ...(v as Record<string, unknown>) };
}

function isTaskStatus(s: unknown): s is TaskStatus {
  return s === "new" || s === "work" || s === "human" || s === "done" || s === "cancel";
}

function normalizeIdList(v: unknown): string[] {
  if (!Array.isArray(v)) return [];
  return v.filter((x): x is string => typeof x === "string" && x.length > 0);
}

// ---------------------------------------------------------------------------
// Comments (`tasks/<id>/comments.jsonl`). One object per line (plan §3.1).
// ---------------------------------------------------------------------------

/** Serialises one comment to its canonical compact JSONL line (no trailing "\n"). */
export function serializeCommentLine(c: Comment): string {
  return JSON.stringify({
    id: c.id,
    author: c.author,
    text: c.text,
    created_at: c.created_at,
    updated_at: c.updated_at,
  });
}

/** Serialises the whole comment list to `comments.jsonl` bytes. */
export function serializeComments(comments: Comment[]): string {
  if (comments.length === 0) return "";
  return comments.map(serializeCommentLine).join("\n") + "\n";
}

/** A lenient parse result: the valid comments plus how many lines were skipped. */
export interface ParsedComments {
  comments: Comment[];
  /** Number of non-blank lines that failed to parse and were skipped. */
  skipped: number;
}

/**
 * Parses `comments.jsonl` bytes (blank lines tolerated) into comments, skipping
 * any malformed line rather than throwing.
 *
 * `comments.jsonl` is explicitly human-editable/deletable under hybrid
 * ownership, so a single fat-fingered line is foreseeable. Skipping (instead of
 * throwing) isolates the damage to that one line — one corrupt line must never
 * take down `commentCount`/`listTaskViews` for the whole project. The skipped
 * count lets callers surface the loss without re-reading the bytes.
 */
export function parseCommentsLenient(text: string): ParsedComments {
  const comments: Comment[] = [];
  let skipped = 0;
  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (trimmed.length === 0) continue;
    let raw: Record<string, unknown>;
    try {
      raw = JSON.parse(trimmed) as Record<string, unknown>;
    } catch {
      skipped++;
      continue;
    }
    comments.push({
      id: String(raw.id ?? ""),
      author: typeof raw.author === "string" ? raw.author : "",
      text: typeof raw.text === "string" ? raw.text : "",
      created_at: typeof raw.created_at === "string" ? raw.created_at : "",
      updated_at: typeof raw.updated_at === "string" ? raw.updated_at : "",
    });
  }
  return { comments, skipped };
}

/** Parses `comments.jsonl` bytes into comments (malformed lines skipped). */
export function parseComments(text: string): Comment[] {
  return parseCommentsLenient(text).comments;
}

// ---------------------------------------------------------------------------
// Session meta (`sessions/<id>.json`). `error` is omitted when absent
// (plan §3.2). `started_at` / `ended_at` are explicit null until set.
// ---------------------------------------------------------------------------

/** Serialises session meta to its canonical `.json` bytes. */
export function serializeSessionMeta(m: SessionMeta): string {
  const ordered: Record<string, unknown> = {
    id: m.id,
    kind: m.kind,
    task_id: m.task_id,
    workflow: m.workflow,
    step: m.step,
    agent: m.agent,
    status: m.status,
  };
  if (m.activity !== undefined) ordered.activity = m.activity;
  if (m.error !== undefined) ordered.error = m.error;
  ordered.started_at = m.started_at;
  ordered.ended_at = m.ended_at;
  if (m.usage !== undefined) ordered.usage = m.usage;
  return JSON.stringify(ordered, null, 2) + "\n";
}

/** Parses session-meta `.json` bytes into a {@link SessionMeta}. */
export function parseSessionMeta(text: string): SessionMeta {
  const raw = JSON.parse(text) as Record<string, unknown>;
  const status = raw.status as SessionStatus;
  if (!isSessionStatus(status)) {
    throw new Error(`session meta: invalid status ${JSON.stringify(raw.status)}`);
  }
  const meta: SessionMeta = {
    id: String(raw.id ?? ""),
    // Sessions written before the interactive-session work have no `kind`;
    // default such legacy metas to `"task"`.
    kind: raw.kind === "interactive" ? "interactive" : "task",
    task_id: String(raw.task_id ?? ""),
    workflow: typeof raw.workflow === "string" ? raw.workflow : "",
    step: typeof raw.step === "string" ? raw.step : "",
    agent: typeof raw.agent === "string" ? raw.agent : "",
    status,
    started_at: typeof raw.started_at === "string" ? raw.started_at : null,
    ended_at: typeof raw.ended_at === "string" ? raw.ended_at : null,
  };
  if (raw.activity === "idle" || raw.activity === "busy") meta.activity = raw.activity;
  if (typeof raw.error === "string") meta.error = raw.error;
  if (Object.hasOwn(raw, "usage")) meta.usage = parseUsage(raw.usage);
  return meta;
}

function parseUsage(value: unknown): Usage | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  const raw = value as Record<string, unknown>;
  const token = (candidate: unknown): number | null =>
    typeof candidate === "number" && Number.isSafeInteger(candidate) && candidate >= 0
      ? candidate
      : null;
  const costValue = (candidate: unknown): number | null =>
    typeof candidate === "number" && Number.isFinite(candidate) && candidate >= 0
      ? candidate
      : null;
  const input = token(raw.input);
  const output = token(raw.output);
  const cacheRead = token(raw.cacheRead);
  const cacheWrite = token(raw.cacheWrite);
  const totalTokens = token(raw.totalTokens);
  const costs = raw.cost;
  if (
    input === null ||
    output === null ||
    cacheRead === null ||
    cacheWrite === null ||
    totalTokens === null ||
    !costs ||
    typeof costs !== "object" ||
    Array.isArray(costs)
  ) {
    return null;
  }
  const cost = costs as Record<string, unknown>;
  const costInput = costValue(cost.input);
  const costOutput = costValue(cost.output);
  const costCacheRead = costValue(cost.cacheRead);
  const costCacheWrite = costValue(cost.cacheWrite);
  const costTotal = costValue(cost.total);
  if (
    costInput === null ||
    costOutput === null ||
    costCacheRead === null ||
    costCacheWrite === null ||
    costTotal === null
  ) {
    return null;
  }
  return {
    input,
    output,
    cacheRead,
    cacheWrite,
    totalTokens,
    cost: {
      input: costInput,
      output: costOutput,
      cacheRead: costCacheRead,
      cacheWrite: costCacheWrite,
      total: costTotal,
    },
  };
}

function isSessionStatus(s: unknown): s is SessionStatus {
  return (
    s === "queued" || s === "running" || s === "done" || s === "failed" || s === "aborted"
  );
}

// ---------------------------------------------------------------------------
// Session transcript (`sessions/<id>.jsonl`). Header on line 1, then entries
// (plan §3.2). Entries are written verbatim (pi entries pipe through); the
// header is re-assembled in canonical key order.
// ---------------------------------------------------------------------------

/** Serialises the transcript header to its canonical compact line (no "\n"). */
export function serializeSessionHeader(h: SessionHeader): string {
  return JSON.stringify({
    type: h.type,
    version: h.version,
    id: h.id,
    kind: h.kind,
    task_id: h.task_id,
    workflow: h.workflow,
    step: h.step,
    agent: h.agent,
    timestamp: h.timestamp,
    cwd: h.cwd,
  });
}

/** Serialises one transcript line (header or entry) as a compact, "\n"-free line. */
export function serializeTranscriptLine(line: TranscriptLine): string {
  if ((line as SessionHeader).type === "session") {
    return serializeSessionHeader(line as SessionHeader);
  }
  return JSON.stringify(line);
}

/** A lenient transcript parse: the valid lines plus how many were skipped. */
export interface ParsedTranscript {
  lines: TranscriptLine[];
  /** Number of non-blank lines that failed to parse and were skipped. */
  skipped: number;
}

/**
 * Parses transcript `.jsonl` bytes into its lines (header first), skipping any
 * malformed line rather than throwing.
 *
 * Sessions are engine-owned (not in the plan's human-editable set of
 * task.json/comments.jsonl), so leniency is not strictly required here. But the
 * same hybrid-ownership filesystem lets a human corrupt a transcript, and
 * `readTranscript` backs an RPC read (`session.transcript`); skipping a bad line
 * keeps one fat-fingered byte from throwing the whole paged read (mirrors
 * {@link parseCommentsLenient}). The skipped count lets callers surface the loss.
 */
export function parseTranscriptLenient(text: string): ParsedTranscript {
  const lines: TranscriptLine[] = [];
  let skipped = 0;
  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (trimmed.length === 0) continue;
    try {
      lines.push(JSON.parse(trimmed) as TranscriptLine);
    } catch {
      skipped++;
    }
  }
  return { lines, skipped };
}

/** Parses transcript `.jsonl` bytes into its lines (malformed lines skipped). */
export function parseTranscript(text: string): TranscriptLine[] {
  return parseTranscriptLenient(text).lines;
}
