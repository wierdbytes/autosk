/**
 * Unit tests for the Claude stream-json → pi-format wire mapping (`src/wire.ts`):
 * content-block mapping (text / thinking / tool_use / tool_result), usage +
 * stopReason, and the `mcp__autosk__transit` tool target resolution.
 */

import { describe, expect, test } from "bun:test";
import type { AssistantMessage, ToolResultMessage, UserMessage } from "@autosk/sdk";

import { mapAssistant, mapResultUsage, mapStopReason, mapUsage, mapUser } from "../src/index.ts";
import { parseTarget } from "../src/driver.ts";

describe("mapStopReason", () => {
  test("maps Anthropic stop reasons to pi StopReason", () => {
    expect(mapStopReason("end_turn")).toBe("stop");
    expect(mapStopReason("stop_sequence")).toBe("stop");
    expect(mapStopReason("max_tokens")).toBe("length");
    expect(mapStopReason("tool_use")).toBe("toolUse");
    expect(mapStopReason(null)).toBe("stop");
    expect(mapStopReason(undefined)).toBe("stop");
  });
});

describe("mapUsage", () => {
  test("maps token counts and zeroes per-message cost", () => {
    const u = mapUsage({
      input_tokens: 10,
      output_tokens: 5,
      cache_read_input_tokens: 3,
      cache_creation_input_tokens: 2,
    });
    expect(u).toEqual({
      input: 10,
      output: 5,
      cacheRead: 3,
      cacheWrite: 2,
      totalTokens: 15,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
    });
  });
  test("tolerates a missing usage object", () => {
    expect(mapUsage(undefined).totalTokens).toBe(0);
  });
});

describe("mapAssistant", () => {
  test("maps text / thinking / tool_use blocks and surfaces tool_use", () => {
    const { message, toolUses } = mapAssistant({
      id: "msg_1",
      role: "assistant",
      model: "claude-sonnet",
      content: [
        { type: "thinking", thinking: "hmm", signature: "sig" },
        { type: "text", text: "hello" },
        { type: "tool_use", id: "tu_1", name: "Bash", input: { command: "ls" } },
      ],
      stop_reason: "tool_use",
      usage: { input_tokens: 4, output_tokens: 2 },
    });
    const m = message as AssistantMessage;
    expect(m.role).toBe("assistant");
    expect(m.provider).toBe("anthropic");
    expect(m.model).toBe("claude-sonnet");
    expect(m.stopReason).toBe("toolUse");
    expect(m.usage.totalTokens).toBe(6);
    expect(m.content[0]).toMatchObject({ type: "thinking", thinking: "hmm", thinkingSignature: "sig" });
    expect(m.content[1]).toMatchObject({ type: "text", text: "hello" });
    expect(m.content[2]).toMatchObject({ type: "toolCall", id: "tu_1", name: "Bash", arguments: { command: "ls" } });
    expect(toolUses).toEqual([{ id: "tu_1", name: "Bash", input: { command: "ls" } }]);
  });

  test("maps redacted_thinking to a redacted ThinkingContent", () => {
    const { message } = mapAssistant({ content: [{ type: "redacted_thinking", data: "xx" }] });
    expect((message as AssistantMessage).content[0]).toEqual({ type: "thinking", thinking: "", redacted: true });
  });

  test("surfaces the transit tool_use so parseTarget can resolve it", () => {
    const { toolUses } = mapAssistant({
      content: [{ type: "tool_use", id: "tu_t", name: "mcp__autosk__transit", input: { to: "review" } }],
    });
    expect(toolUses[0]!.name).toBe("mcp__autosk__transit");
    expect(parseTarget(toolUses[0]!.input)).toEqual({ step: "review" });
  });
});

describe("mapUser", () => {
  test("maps tool_result blocks to ToolResultMessages with names from the id→name map", () => {
    const names = new Map<string, string>([["tu_1", "Bash"]]);
    const msgs = mapUser(
      {
        role: "user",
        content: [{ type: "tool_result", tool_use_id: "tu_1", content: "ok", is_error: false }],
      },
      names,
    );
    expect(msgs).toHaveLength(1);
    const tr = msgs[0] as ToolResultMessage;
    expect(tr.role).toBe("toolResult");
    expect(tr.toolCallId).toBe("tu_1");
    expect(tr.toolName).toBe("Bash");
    expect(tr.isError).toBe(false);
    expect(tr.content).toEqual([{ type: "text", text: "ok" }]);
  });

  test("resolves an unknown tool_use_id to an empty toolName (not a crash)", () => {
    const msgs = mapUser({ content: [{ type: "tool_result", tool_use_id: "missing", content: [{ type: "text", text: "x" }] }] }, new Map());
    expect((msgs[0] as ToolResultMessage).toolName).toBe("");
  });

  test("maps a replayed plain-text user message to a UserMessage", () => {
    const msgs = mapUser({ role: "user", content: "do the thing" }, new Map());
    expect(msgs).toHaveLength(1);
    expect(msgs[0] as UserMessage).toMatchObject({ role: "user", content: "do the thing" });
  });

  test("maps text content blocks (non-tool-result) to a UserMessage", () => {
    const msgs = mapUser({ role: "user", content: [{ type: "text", text: "hi" }] }, new Map());
    expect(msgs[0] as UserMessage).toMatchObject({ role: "user", content: [{ type: "text", text: "hi" }] });
  });

  test("an error tool_result keeps isError", () => {
    const msgs = mapUser({ content: [{ type: "tool_result", tool_use_id: "t", content: "boom", is_error: true }] }, new Map([["t", "Read"]]));
    expect((msgs[0] as ToolResultMessage).isError).toBe(true);
  });
});

describe("mapResultUsage", () => {
  test("extracts run-level usage + total_cost_usd and the error subtype", () => {
    const r = mapResultUsage({
      type: "result",
      subtype: "success",
      is_error: false,
      total_cost_usd: 0.0123,
      usage: { input_tokens: 100, output_tokens: 50 },
    });
    expect(r.totalCostUsd).toBeCloseTo(0.0123);
    expect(r.usage?.totalTokens).toBe(150);
    expect(r.usage?.cost.total).toBeCloseTo(0.0123);
    expect(r.isError).toBe(false);
    expect(r.subtype).toBe("success");
  });

  test("flags an error result", () => {
    const r = mapResultUsage({ subtype: "error_during_execution", is_error: true });
    expect(r.isError).toBe(true);
    expect(r.subtype).toBe("error_during_execution");
    expect(r.usage).toBeNull();
  });
});

describe("parseTarget", () => {
  test("maps `to` to a step or terminal status", () => {
    expect(parseTarget({ to: "review" })).toEqual({ step: "review" });
    expect(parseTarget({ to: "done" })).toEqual({ status: "done" });
    expect(parseTarget({ to: "cancel" })).toEqual({ status: "cancel" });
    expect(parseTarget({ to: "human" })).toEqual({ status: "human" });
  });
  test("accepts explicit {step} / {status} shapes", () => {
    expect(parseTarget({ step: "dev" })).toEqual({ step: "dev" });
    expect(parseTarget({ status: "done" })).toEqual({ status: "done" });
  });
  test("rejects junk", () => {
    expect(parseTarget(null)).toBeNull();
    expect(parseTarget({})).toBeNull();
    expect(parseTarget({ to: "  " })).toBeNull();
    expect(parseTarget("review")).toBeNull();
  });
});
