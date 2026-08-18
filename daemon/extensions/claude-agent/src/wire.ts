/**
 * Claude Code stream-json wire → pi-format transcript mapping.
 *
 * Claude Code's `--output-format stream-json` emits one JSON event per stdout
 * line. The events relevant to the transcript are:
 *  - `assistant` — `{ message: <Anthropic API assistant message> }`: text /
 *    thinking / tool_use content blocks → a pi {@link AssistantMessage}.
 *  - `user` — `{ message: { role:"user", content } }`: `tool_result` blocks → pi
 *    {@link ToolResultMessage}s (one per block), or a replayed plain user message
 *    → a pi {@link UserMessage}.
 *  - `result` — the turn boundary, carrying run-level usage + `total_cost_usd`.
 *  - `stream_event` — a raw Anthropic streaming delta (partials), accumulated by
 *    the driver, not mapped here.
 *
 * `tool_result` blocks carry only `tool_use_id`, so the driver keeps an
 * id→name map from the preceding `assistant` event's `tool_use` blocks (built-in
 * tools included) and passes it in to fill {@link ToolResultMessage.toolName}.
 * `task` / `comment` tool calls flow through as ordinary tool_use/tool_result —
 * only `mcp__autosk__transit` is special-cased (on the driver's observe side).
 */

import type {
  AssistantMessage,
  StopReason,
  TextContent,
  ThinkingContent,
  ToolCall,
  ToolResultMessage,
  TranscriptMessage,
  Usage,
  UserMessage,
} from "@autosk/sdk";

/** A raw Anthropic content block from a Claude `assistant` message. */
interface AnthropicBlock {
  type: string;
  text?: string;
  thinking?: string;
  signature?: string;
  data?: string;
  id?: string;
  name?: string;
  input?: unknown;
  // tool_result fields (appear on user messages)
  tool_use_id?: string;
  content?: unknown;
  is_error?: boolean;
}

/** A raw Anthropic assistant message envelope (the `message` of an `assistant` event). */
export interface ClaudeAssistantMessage {
  id?: string;
  role?: string;
  model?: string;
  content?: AnthropicBlock[];
  stop_reason?: string | null;
  usage?: Record<string, unknown>;
}

/** A tool_use observed on an `assistant` event (the driver records id→name + transit). */
export interface ObservedToolUse {
  id: string;
  name: string;
  input: unknown;
}

/** The result of mapping one `assistant` event. */
export interface MappedAssistant {
  message: AssistantMessage;
  toolUses: ObservedToolUse[];
}

const PROVIDER = "anthropic";

/** Maps Anthropic's `stop_reason` to a pi {@link StopReason}. */
export function mapStopReason(raw: string | null | undefined): StopReason {
  switch (raw) {
    case "end_turn":
    case "stop_sequence":
      return "stop";
    case "max_tokens":
      return "length";
    case "tool_use":
      return "toolUse";
    case null:
    case undefined:
      return "stop";
    default:
      return "stop";
  }
}

/**
 * Maps an Anthropic `usage` object to a pi {@link Usage}. Per-message `cost` is
 * not reported by the CLI per assistant message (only `total_cost_usd` at the
 * `result` level — see {@link mapResultUsage}), so per-message costs are `0`;
 * tokens are mapped faithfully.
 */
export function mapUsage(raw: Record<string, unknown> | undefined): Usage {
  const u = raw ?? {};
  const input = num(u.input_tokens);
  const output = num(u.output_tokens);
  const cacheRead = num(u.cache_read_input_tokens);
  const cacheWrite = num(u.cache_creation_input_tokens);
  return {
    input,
    output,
    cacheRead,
    cacheWrite,
    totalTokens: input + output,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
  };
}

/** Maps one Anthropic content block to a pi assistant content block (or null to drop). */
function mapAssistantBlock(b: AnthropicBlock): TextContent | ThinkingContent | ToolCall | null {
  switch (b.type) {
    case "text":
      return { type: "text", text: b.text ?? "" };
    case "thinking": {
      const block: ThinkingContent = { type: "thinking", thinking: b.thinking ?? "" };
      if (b.signature) block.thinkingSignature = b.signature;
      return block;
    }
    case "redacted_thinking":
      return { type: "thinking", thinking: "", redacted: true };
    case "tool_use":
      return {
        type: "toolCall",
        id: b.id ?? "",
        name: b.name ?? "",
        arguments: isObject(b.input) ? (b.input as Record<string, unknown>) : {},
      };
    default:
      return null;
  }
}

/**
 * Maps a Claude `assistant` event's message into a pi {@link AssistantMessage}
 * and the list of `tool_use` blocks the driver observes (for id→name + transit).
 */
export function mapAssistant(message: ClaudeAssistantMessage, timestamp = Date.now()): MappedAssistant {
  const content: (TextContent | ThinkingContent | ToolCall)[] = [];
  const toolUses: ObservedToolUse[] = [];
  for (const b of message.content ?? []) {
    const mapped = mapAssistantBlock(b);
    if (mapped) content.push(mapped);
    if (b.type === "tool_use") {
      toolUses.push({ id: b.id ?? "", name: b.name ?? "", input: b.input });
    }
  }
  const out: AssistantMessage = {
    role: "assistant",
    content,
    provider: PROVIDER,
    model: message.model ?? "",
    usage: mapUsage(message.usage),
    stopReason: mapStopReason(message.stop_reason),
    timestamp,
  };
  return { message: out, toolUses };
}

/**
 * Maps a Claude `user` event's message into pi transcript messages. A message
 * whose content is `tool_result` blocks yields one {@link ToolResultMessage} per
 * block (toolName resolved from `toolNames`); a plain text/string message (a
 * replayed user prompt) yields one {@link UserMessage}.
 */
export function mapUser(
  message: { role?: string; content?: unknown },
  toolNames: Map<string, string>,
  timestamp = Date.now(),
): TranscriptMessage[] {
  const content = message.content;
  // A replayed plain-text user message (our own prompt echoed back).
  if (typeof content === "string") {
    return [{ role: "user", content, timestamp } satisfies UserMessage];
  }
  if (!Array.isArray(content)) return [];

  const blocks = content as AnthropicBlock[];
  const toolResults = blocks.filter((b) => b.type === "tool_result");
  if (toolResults.length > 0) {
    return toolResults.map((b) => mapToolResult(b, toolNames, timestamp));
  }
  // Otherwise it's a user message with text/image content blocks.
  const userContent = blocks
    .map((b): TextContent | null => (b.type === "text" ? { type: "text", text: b.text ?? "" } : null))
    .filter((x): x is TextContent => x !== null);
  if (userContent.length === 0) return [];
  return [{ role: "user", content: userContent, timestamp } satisfies UserMessage];
}

/** Maps one Anthropic `tool_result` block to a pi {@link ToolResultMessage}. */
function mapToolResult(
  b: AnthropicBlock,
  toolNames: Map<string, string>,
  timestamp: number,
): ToolResultMessage {
  const toolCallId = b.tool_use_id ?? "";
  return {
    role: "toolResult",
    toolCallId,
    toolName: toolNames.get(toolCallId) ?? "",
    content: toolResultContent(b.content),
    isError: b.is_error === true,
    timestamp,
  };
}

/** Normalizes a tool_result `content` (string | block[]) to pi text/image blocks. */
function toolResultContent(content: unknown): TextContent[] {
  if (typeof content === "string") return [{ type: "text", text: content }];
  if (Array.isArray(content)) {
    return (content as AnthropicBlock[])
      .map((b): TextContent | null => {
        if (b.type === "text") return { type: "text", text: b.text ?? "" };
        return null;
      })
      .filter((x): x is TextContent => x !== null);
  }
  return [];
}

/** Run-level usage + cost extracted from a `result` event (for a custom log entry). */
export interface ResultUsage {
  usage: Usage | null;
  totalCostUsd: number;
  isError: boolean;
  subtype: string;
}

/** Maps a Claude `result` event's usage + `total_cost_usd` for a custom log entry. */
export function mapResultUsage(event: Record<string, unknown>): ResultUsage {
  const rawUsage = isObject(event.usage) ? (event.usage as Record<string, unknown>) : undefined;
  const usage = completeResultUsage(rawUsage);
  const totalCostUsd = num(event.total_cost_usd);
  if (usage) usage.cost.total = totalCostUsd;
  return {
    usage,
    totalCostUsd,
    isError: event.is_error === true,
    subtype: typeof event.subtype === "string" ? event.subtype : "",
  };
}

function completeResultUsage(raw: Record<string, unknown> | undefined): Usage | null {
  if (!raw || !tokenCount(raw.input_tokens) || !tokenCount(raw.output_tokens)) return null;
  for (const key of ["cache_read_input_tokens", "cache_creation_input_tokens"] as const) {
    if (Object.hasOwn(raw, key) && !tokenCount(raw[key])) return null;
  }
  return mapUsage(raw);
}

function tokenCount(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}
