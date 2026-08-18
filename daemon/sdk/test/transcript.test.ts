import { describe, expect, test } from "bun:test";
import {
  AUTOSK_CUSTOM_TYPES,
  isAutoskCustomType,
  type CustomEntry,
  type ErrorEntry,
  type MessageEntry,
  type SessionEndEntry,
  type SessionHeader,
  type SteerEntry,
  type TranscriptLine,
  type TransitEntry,
  type UsageEntry,
} from "../src/index.ts";

/** Parse a JSONL line and assert it round-trips byte-for-byte through the type. */
function roundTrip<T extends TranscriptLine>(value: T): T {
  const line = JSON.stringify(value);
  const parsed = JSON.parse(line) as T;
  expect(parsed).toEqual(value);
  // Re-serialising the parsed value must reproduce the original line.
  expect(JSON.stringify(parsed)).toBe(line);
  return parsed;
}

describe("transcript header", () => {
  test("parses + serialises the documented header line", () => {
    const raw =
      '{"type":"session","version":1,"id":"0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b","task_id":"ask-3f9b2c","workflow":"feature-dev","step":"dev","agent":"@autosk/pi-agent/dev","timestamp":"2026-06-12T09:00:00Z","cwd":"/repo"}';
    const header = JSON.parse(raw) as SessionHeader;
    expect(header.type).toBe("session");
    expect(header.version).toBe(1);
    expect(header.task_id).toBe("ask-3f9b2c");
    expect(header.workflow).toBe("feature-dev");
    expect(header.step).toBe("dev");
    expect(header.agent).toBe("@autosk/pi-agent/dev");
    expect(header.cwd).toBe("/repo");
    roundTrip(header);
  });
});

describe("pi message-schema entry", () => {
  test("round-trips a user message entry", () => {
    const entry: MessageEntry = {
      type: "message",
      id: "a1b2c3d4",
      timestamp: "2026-06-12T09:00:01Z",
      message: { role: "user", content: "Hello", timestamp: 1_760_000_000_000 },
    };
    const parsed = roundTrip(entry);
    expect(parsed.message.role).toBe("user");
  });

  test("round-trips an assistant message entry with a toolCall + usage", () => {
    const entry: MessageEntry = {
      type: "message",
      id: "b2c3d4e5",
      timestamp: "2026-06-12T09:00:02Z",
      message: {
        role: "assistant",
        content: [
          { type: "text", text: "Calling a tool" },
          { type: "toolCall", id: "call_1", name: "bash", arguments: { cmd: "ls" } },
        ],
        provider: "anthropic",
        model: "claude-sonnet-4-5",
        usage: {
          input: 10,
          output: 5,
          cacheRead: 0,
          cacheWrite: 0,
          totalTokens: 15,
          cost: { input: 0.01, output: 0.02, cacheRead: 0, cacheWrite: 0, total: 0.03 },
        },
        stopReason: "toolUse",
        timestamp: 1_760_000_000_001,
      },
    };
    const parsed = roundTrip(entry);
    expect(parsed.message.role).toBe("assistant");
    if (parsed.message.role === "assistant") {
      expect(parsed.message.stopReason).toBe("toolUse");
      expect(parsed.message.content.some((b) => b.type === "toolCall")).toBe(true);
    }
  });

  test("round-trips an assistant message whose content includes a thinking block", () => {
    const entry: MessageEntry = {
      type: "message",
      id: "c4d5e6f7",
      timestamp: "2026-06-12T09:00:03Z",
      message: {
        role: "assistant",
        content: [
          {
            type: "thinking",
            thinking: "Let me reason about this first.",
            thinkingSignature: "sig-abc",
          },
          { type: "text", text: "Here is the answer.", textSignature: "sig-def" },
        ],
        provider: "anthropic",
        model: "claude-sonnet-4-5",
        usage: {
          input: 20,
          output: 8,
          cacheRead: 4,
          cacheWrite: 2,
          totalTokens: 34,
          cost: { input: 0.02, output: 0.03, cacheRead: 0.001, cacheWrite: 0.002, total: 0.053 },
        },
        stopReason: "stop",
        timestamp: 1_760_000_000_002,
      },
    };
    const parsed = roundTrip(entry);
    if (parsed.message.role === "assistant") {
      const thinking = parsed.message.content.find((b) => b.type === "thinking");
      expect(thinking).toBeDefined();
      if (thinking?.type === "thinking") {
        expect(thinking.thinking).toBe("Let me reason about this first.");
        expect(thinking.thinkingSignature).toBe("sig-abc");
      }
    }
  });

  test("round-trips a toolResult message entry (with text + image content)", () => {
    const entry: MessageEntry = {
      type: "message",
      id: "d5e6f7a8",
      timestamp: "2026-06-12T09:00:04Z",
      message: {
        role: "toolResult",
        toolCallId: "call_1",
        toolName: "bash",
        content: [
          { type: "text", text: "file1\nfile2" },
          { type: "image", data: "aGVsbG8=", mimeType: "image/png" },
        ],
        isError: false,
        timestamp: 1_760_000_000_003,
      },
    };
    const parsed = roundTrip(entry);
    expect(parsed.message.role).toBe("toolResult");
    if (parsed.message.role === "toolResult") {
      expect(parsed.message.toolCallId).toBe("call_1");
      expect(parsed.message.toolName).toBe("bash");
      expect(parsed.message.isError).toBe(false);
      const image = parsed.message.content.find((b) => b.type === "image");
      expect(image).toBeDefined();
      if (image?.type === "image") {
        expect(image.mimeType).toBe("image/png");
        expect(image.data).toBe("aGVsbG8=");
      }
    }
  });

  test("round-trips a toolResult error entry with details", () => {
    const entry: MessageEntry = {
      type: "message",
      id: "e6f7a8b9",
      timestamp: "2026-06-12T09:00:05Z",
      message: {
        role: "toolResult",
        toolCallId: "call_2",
        toolName: "edit",
        content: [{ type: "text", text: "oldText not found" }],
        details: { path: "src/x.ts", reason: "no_match" },
        isError: true,
        timestamp: 1_760_000_000_004,
      },
    };
    const parsed = roundTrip(entry);
    if (parsed.message.role === "toolResult") {
      expect(parsed.message.isError).toBe(true);
      expect((parsed.message.details as { reason: string }).reason).toBe("no_match");
    }
  });

  test("round-trips a user message whose content is an array with an image block", () => {
    const entry: MessageEntry = {
      type: "message",
      id: "f7a8b9c0",
      timestamp: "2026-06-12T09:00:06Z",
      message: {
        role: "user",
        content: [
          { type: "text", text: "What is in this screenshot?" },
          { type: "image", data: "d29ybGQ=", mimeType: "image/jpeg" },
        ],
        timestamp: 1_760_000_000_005,
      },
    };
    const parsed = roundTrip(entry);
    expect(parsed.message.role).toBe("user");
    if (parsed.message.role === "user" && Array.isArray(parsed.message.content)) {
      const image = parsed.message.content.find((b) => b.type === "image");
      expect(image).toBeDefined();
      if (image?.type === "image") {
        expect(image.mimeType).toBe("image/jpeg");
      }
    }
  });
});

describe("generic custom entry", () => {
  test("round-trips an extension custom entry", () => {
    const entry: CustomEntry<{ count: number }> = {
      type: "custom",
      id: "c3d4e5f6",
      timestamp: "2026-06-12T09:00:03Z",
      customType: "my-extension",
      data: { count: 42 },
    };
    const parsed = roundTrip(entry);
    expect(parsed.customType).toBe("my-extension");
    expect((parsed.data as { count: number }).count).toBe(42);
  });
});

describe("engine structural entries (autosk:*)", () => {
  test("round-trips autosk:transit", () => {
    const entry: TransitEntry = {
      type: "custom",
      id: "11111111",
      timestamp: "2026-06-12T09:01:00Z",
      customType: "autosk:transit",
      data: { to: { step: "review" }, from: { workflow: "feature-dev", step: "dev" } },
    };
    const parsed = roundTrip(entry);
    expect(parsed.data.to).toEqual({ step: "review" });
  });

  test("round-trips autosk:steer", () => {
    const entry: SteerEntry = {
      type: "custom",
      id: "22222222",
      timestamp: "2026-06-12T09:02:00Z",
      customType: "autosk:steer",
      data: { kind: "steer", message: "focus on the auth module" },
    };
    const parsed = roundTrip(entry);
    expect(parsed.data.kind).toBe("steer");
  });

  test("round-trips autosk:error", () => {
    const entry: ErrorEntry = {
      type: "custom",
      id: "33333333",
      timestamp: "2026-06-12T09:03:00Z",
      customType: "autosk:error",
      data: { error: "agent_did_not_transit", message: "onRun returned without transit" },
    };
    const parsed = roundTrip(entry);
    expect(parsed.data.error).toBe("agent_did_not_transit");
  });

  test("round-trips autosk:session_end", () => {
    const entry: SessionEndEntry = {
      type: "custom",
      id: "44444444",
      timestamp: "2026-06-12T09:04:00Z",
      customType: "autosk:session_end",
      data: { status: "done" },
    };
    const parsed = roundTrip(entry);
    expect(parsed.data.status).toBe("done");
  });

  test("round-trips canonical usage and unavailable reports", () => {
    const entry: UsageEntry = {
      type: "custom",
      id: "55555555",
      timestamp: "2026-06-12T09:05:00Z",
      customType: "autosk:usage",
      data: {
        input: 10,
        output: 5,
        cacheRead: 2,
        cacheWrite: 1,
        totalTokens: 15,
        cost: { input: 0.1, output: 0.2, cacheRead: 0, cacheWrite: 0, total: 0.3 },
      },
    };
    expect(roundTrip(entry).data?.totalTokens).toBe(15);
    expect(roundTrip({ ...entry, data: null }).data).toBeNull();
  });

  test("isAutoskCustomType recognises exactly the engine-owned types", () => {
    for (const t of AUTOSK_CUSTOM_TYPES) {
      expect(isAutoskCustomType(t)).toBe(true);
    }
    expect(isAutoskCustomType("my-extension")).toBe(false);
    expect(isAutoskCustomType("autosk:unknown")).toBe(false);
    expect(AUTOSK_CUSTOM_TYPES).toEqual([
      "autosk:transit",
      "autosk:steer",
      "autosk:error",
      "autosk:session_end",
      "autosk:usage",
    ]);
  });
});
