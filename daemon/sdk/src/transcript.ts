/**
 * pi-format transcript entry types (plan §3.2).
 *
 * A session transcript lives at `./.autosk/sessions/<session-id>.jsonl`. Its
 * entry schema deliberately mirrors pi's session format (see
 * `pi/packages/coding-agent/docs/session-format.md` and `SessionEntryBase` in
 * `pi/.../core/session-manager.ts`) so pi-based agents can pipe pi session
 * entries through verbatim and pi tooling/renderers stay reusable.
 *
 * Differences from pi:
 *  - line 1 is pi's `SessionHeader` with autosk fields added (`task_id`,
 *    `workflow`, `step`, `agent`) and a fixed `version: 1`;
 *  - entries extend pi's base `{ type, id, timestamp }` but DROP `parentId` —
 *    autosk transcripts are linear, there is no `/tree` branching;
 *  - the engine emits its own structural `custom` entries (`autosk:*`).
 */

import type { StepTarget } from "./workflow.ts";
import type { SessionKind } from "./types.ts";

// ---------------------------------------------------------------------------
// pi message schema (reused unchanged — see pi-ai `types.ts`).
// ---------------------------------------------------------------------------

export interface TextContent {
  type: "text";
  text: string;
  textSignature?: string;
}

export interface ThinkingContent {
  type: "thinking";
  thinking: string;
  thinkingSignature?: string;
  redacted?: boolean;
}

export interface ImageContent {
  type: "image";
  /** base64-encoded image data. */
  data: string;
  /** e.g. `"image/png"`. */
  mimeType: string;
}

export interface ToolCall {
  type: "toolCall";
  id: string;
  name: string;
  arguments: Record<string, unknown>;
  thoughtSignature?: string;
}

/** A block of message content. */
export type ContentBlock = TextContent | ThinkingContent | ImageContent | ToolCall;

/** Token usage + cost accounting attached to assistant messages. */
export interface Usage {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  totalTokens: number;
  cost: {
    input: number;
    output: number;
    cacheRead: number;
    cacheWrite: number;
    total: number;
  };
}

export type StopReason = "stop" | "length" | "toolUse" | "error" | "aborted";

export interface UserMessage {
  role: "user";
  content: string | (TextContent | ImageContent)[];
  /** Unix timestamp in milliseconds. */
  timestamp: number;
}

export interface AssistantMessage {
  role: "assistant";
  content: (TextContent | ThinkingContent | ToolCall)[];
  /**
   * pi declares this REQUIRED (`api: Api`). autosk narrows it to optional on
   * purpose: pi entries pipe through verbatim (the field survives the JSON
   * round-trip), but autosk's own non-pi agents may write a message entry
   * without an `api`. Plan §3.2 only mandates role/content[]/usage/cost/
   * stopReason. pi's other optional assistant fields (`responseModel`,
   * `responseId`, `diagnostics`) are likewise not modelled but survive at
   * runtime as extra props.
   */
  api?: string;
  provider: string;
  model: string;
  usage: Usage;
  stopReason: StopReason;
  errorMessage?: string;
  /** Unix timestamp in milliseconds. */
  timestamp: number;
}

export interface ToolResultMessage {
  role: "toolResult";
  toolCallId: string;
  toolName: string;
  content: (TextContent | ImageContent)[];
  details?: unknown;
  isError: boolean;
  /** Unix timestamp in milliseconds. */
  timestamp: number;
}

/** A pi message, written via `ctx.log.message(...)`. */
export type TranscriptMessage = UserMessage | AssistantMessage | ToolResultMessage;

// ---------------------------------------------------------------------------
// Header + entry base.
// ---------------------------------------------------------------------------

/**
 * Line 1 of a transcript: pi's `SessionHeader` with autosk fields added
 * (plan §3.2). `version` is pinned to `1` — the autosk transcript schema is
 * versioned independently of pi's session version.
 */
export interface SessionHeader {
  type: "session";
  version: 1;
  id: string;
  /** Mirrors {@link SessionMeta.kind}: `"task"` or `"interactive"`. */
  kind: SessionKind;
  /** `""` for an interactive (taskless) session. */
  task_id: string;
  /** `""` for an interactive (taskless) session. */
  workflow: string;
  /** `""` for an interactive (taskless) session. */
  step: string;
  agent: string;
  /** RFC3339 UTC. */
  timestamp: string;
  cwd: string;
}

/**
 * Base for every non-header entry. Mirrors pi's `SessionEntryBase` minus
 * `parentId` (transcripts are linear).
 */
export interface TranscriptEntryBase {
  type: string;
  /** 8-char hex id. */
  id: string;
  /** RFC3339 UTC. */
  timestamp: string;
}

/** A pi message-schema entry (`ctx.log.message`). */
export interface MessageEntry extends TranscriptEntryBase {
  type: "message";
  message: TranscriptMessage;
}

/** A generic custom entry (`ctx.log.custom`) — the agent logging channel. */
export interface CustomEntry<T = unknown> extends TranscriptEntryBase {
  type: "custom";
  customType: string;
  data?: T;
}

// ---------------------------------------------------------------------------
// Engine structural entries (autosk-specific custom types, plan §3.2).
// These let a transcript stay self-contained without consulting the meta file.
// ---------------------------------------------------------------------------

/** The structural custom types the engine emits itself. */
export const AUTOSK_CUSTOM_TYPES = [
  "autosk:transit",
  "autosk:steer",
  "autosk:error",
  "autosk:session_end",
  "autosk:usage",
] as const;

export type AutoskCustomType = (typeof AUTOSK_CUSTOM_TYPES)[number];

/** `autosk:transit` payload — one committed transition. */
export interface TransitData {
  to: StepTarget;
  /** The position the task was at before this transition. */
  from?: { workflow: string; step: string };
}

/** `autosk:steer` payload — a steer/followup message injected into a live session. */
export interface SteerData {
  kind: "steer" | "followup";
  message: string;
}

/** `autosk:error` payload — an error surfaced during the session. */
export interface ErrorData {
  error: string;
  message?: string;
}

/** `autosk:session_end` payload — the session's terminal status. */
export interface SessionEndData {
  status: "done" | "failed" | "aborted";
  error?: string;
}

/** Canonical per-turn usage reported through {@link TranscriptAPI.usage}. */
export type UsageData = Usage | null;

export interface TransitEntry extends TranscriptEntryBase {
  type: "custom";
  customType: "autosk:transit";
  data: TransitData;
}

export interface SteerEntry extends TranscriptEntryBase {
  type: "custom";
  customType: "autosk:steer";
  data: SteerData;
}

export interface ErrorEntry extends TranscriptEntryBase {
  type: "custom";
  customType: "autosk:error";
  data: ErrorData;
}

export interface SessionEndEntry extends TranscriptEntryBase {
  type: "custom";
  customType: "autosk:session_end";
  data: SessionEndData;
}

export interface UsageEntry extends TranscriptEntryBase {
  type: "custom";
  customType: "autosk:usage";
  data: UsageData;
}

/** The union of engine structural entries. */
export type EngineEntry = TransitEntry | SteerEntry | ErrorEntry | SessionEndEntry | UsageEntry;

/** Any non-header transcript entry. */
export type TranscriptEntry = MessageEntry | CustomEntry | EngineEntry;

/** Any line of a transcript file. */
export type TranscriptLine = SessionHeader | TranscriptEntry;

/** Type guard: is `s` one of the engine's structural custom types? */
export function isAutoskCustomType(s: string): s is AutoskCustomType {
  return (AUTOSK_CUSTOM_TYPES as readonly string[]).includes(s);
}
