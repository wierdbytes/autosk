/**
 * Unit tests for the {@link ClaudeDriver} state machine, driven with synthetic
 * stream-json NDJSON lines (no real `claude`). Mirrors pi-agent's PiDriver tests
 * in the Claude vocabulary: turn boundaries (`result`) + queueing/await race,
 * `takePendingTarget` (observed `mcp__autosk__transit`), partial coalescing +
 * stop on exit/abort, steer idle-vs-streaming dispatch, shutdown idempotency.
 */

import { describe, expect, test } from "bun:test";
import type { ChildHandle, TranscriptMessage } from "@autosk/sdk";

import { ClaudeDriver, Coalescer } from "../src/index.ts";

/** A fake {@link ChildHandle} the test drives by hand. */
function fakeChild(): {
  child: ChildHandle;
  writes: Record<string, unknown>[];
  emitStdout(line: string): void;
  emitStderr(line: string): void;
  exit(code: number): void;
  killed: number;
} {
  let stdoutCb: ((l: string) => void) | null = null;
  let stderrCb: ((l: string) => void) | null = null;
  let resolveExit: (v: { code: number | null }) => void = () => {};
  const writes: Record<string, unknown>[] = [];
  const state = { killed: 0 };
  const stdin = {
    write: async (bytes: Uint8Array) => {
      const text = new TextDecoder().decode(bytes);
      for (const line of text.split("\n")) {
        if (line.trim() === "") continue;
        writes.push(JSON.parse(line) as Record<string, unknown>);
      }
    },
    close: async () => {},
  } as unknown as WritableStreamDefaultWriter<Uint8Array>;
  const child: ChildHandle = {
    stdin,
    onStdout: (cb) => void (stdoutCb = cb),
    onStderr: (cb) => void (stderrCb = cb),
    kill: () => {
      state.killed++;
    },
    exited: new Promise((r) => {
      resolveExit = r;
    }),
  };
  return {
    child,
    writes,
    emitStdout: (l) => stdoutCb?.(l),
    emitStderr: (l) => stderrCb?.(l),
    exit: (code) => resolveExit({ code }),
    get killed() {
      return state.killed;
    },
  };
}

const tick = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

function assistantEvent(content: unknown[], extra: Record<string, unknown> = {}): string {
  return JSON.stringify({ type: "assistant", message: { role: "assistant", model: "m", content, ...extra } });
}
function resultEvent(extra: Record<string, unknown> = {}): string {
  return JSON.stringify({ type: "result", subtype: "success", is_error: false, total_cost_usd: 0, usage: {}, ...extra });
}

describe("ClaudeDriver — turn boundaries", () => {
  function makeDriver() {
    const f = fakeChild();
    const messages: TranscriptMessage[] = [];
    const customs: { type: string; data: unknown }[] = [];
    const usages: unknown[] = [];
    const driver = new ClaudeDriver(f.child, {
      onMessage: (m) => messages.push(m),
      onUsage: (usage) => usages.push(usage),
      onCustom: (t, d) => customs.push({ type: t, data: d }),
      signal: new AbortController().signal,
    });
    return { f, driver, messages, customs, usages };
  }

  test("a `result` event ends the turn (await then emit)", async () => {
    const { f, driver } = makeDriver();
    const p = driver.waitForTurnEnd();
    f.emitStdout(resultEvent());
    expect(await p).toBe("ended");
  });

  test("a `result` that races the await is queued and delivered (emit then await)", async () => {
    const { f, driver } = makeDriver();
    f.emitStdout(resultEvent());
    expect(await driver.waitForTurnEnd()).toBe("ended");
  });

  test("`result` reports authoritative usage and mirrors assistant messages", async () => {
    const { f, driver, messages, customs, usages } = makeDriver();
    f.emitStdout(assistantEvent([{ type: "text", text: "hi" }], { usage: { input_tokens: 1, output_tokens: 1 } }));
    f.emitStdout(resultEvent({ total_cost_usd: 0.5, usage: { input_tokens: 7, output_tokens: 3 } }));
    await tick();
    expect(messages).toHaveLength(1);
    expect(messages[0]).toMatchObject({ role: "assistant", content: [{ type: "text", text: "hi" }] });
    const result = customs.find((c) => c.type === "claude-agent:result");
    expect(result).toBeDefined();
    expect((result!.data as { total_cost_usd: number }).total_cost_usd).toBe(0.5);
    expect(usages).toEqual([
      {
        input: 7,
        output: 3,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 10,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0.5 },
      },
    ]);
  });

  test("`result` reports unavailable usage instead of a synthetic zero", async () => {
    const { f, usages } = makeDriver();
    f.emitStdout(resultEvent({ usage: {} }));
    await tick();
    expect(usages).toEqual([null]);
  });
});

describe("ClaudeDriver — onActivity (busy/idle)", () => {
  function makeDriver() {
    const f = fakeChild();
    const activity: boolean[] = [];
    const driver = new ClaudeDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      onActivity: (busy) => activity.push(busy),
      signal: new AbortController().signal,
    });
    return { f, driver, activity };
  }

  test("sendPrompt fires busy=true (once) and `result` fires busy=false", async () => {
    const { f, driver, activity } = makeDriver();
    await driver.sendPrompt("seed");
    expect(activity).toEqual([true]);
    f.emitStdout(resultEvent());
    await tick();
    expect(activity).toEqual([true, false]);
  });

  test("an idle input (a fresh turn) fires busy=true", async () => {
    const { driver, activity } = makeDriver();
    await driver.input("followup", "hi"); // idle → a fresh turn
    expect(activity).toEqual([true]);
  });

  test("a steer/followup while already streaming does NOT re-fire busy", async () => {
    const { f, driver, activity } = makeDriver();
    await driver.sendPrompt("seed"); // busy=true
    await driver.input("followup", "more"); // queued, still streaming → no re-fire
    expect(activity).toEqual([true]);
    f.emitStdout(resultEvent());
    await tick();
    expect(activity).toEqual([true, false]);
  });
});

describe("ClaudeDriver — transit observation", () => {
  test("observes mcp__autosk__transit and exposes the target", async () => {
    const f = fakeChild();
    const driver = new ClaudeDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: new AbortController().signal,
    });
    f.emitStdout(
      assistantEvent([{ type: "tool_use", id: "tu_t", name: "mcp__autosk__transit", input: { to: "done" } }]),
    );
    expect(driver.takePendingTarget()).toEqual({ status: "done" });
    // The target is cleared after being taken.
    expect(driver.takePendingTarget()).toBeNull();
  });

  test("warns when transit is called with no usable target", async () => {
    const f = fakeChild();
    const warnings: string[] = [];
    const driver = new ClaudeDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: new AbortController().signal,
      warn: (m) => warnings.push(m),
    });
    f.emitStdout(assistantEvent([{ type: "tool_use", id: "x", name: "mcp__autosk__transit", input: { junk: 1 } }]));
    expect(driver.takePendingTarget()).toBeNull();
    expect(warnings.some((w) => w.includes("no usable target"))).toBe(true);
  });

  test("resolves tool_result toolName from the preceding assistant id→name map", async () => {
    const f = fakeChild();
    const messages: TranscriptMessage[] = [];
    const driver = new ClaudeDriver(f.child, {
      onMessage: (m) => messages.push(m),
      onCustom: () => {},
      signal: new AbortController().signal,
    });
    f.emitStdout(assistantEvent([{ type: "tool_use", id: "tu_9", name: "Bash", input: {} }]));
    f.emitStdout(JSON.stringify({ type: "user", message: { role: "user", content: [{ type: "tool_result", tool_use_id: "tu_9", content: "ok" }] } }));
    await tick();
    const toolResult = messages.find((m) => m.role === "toolResult");
    expect(toolResult).toMatchObject({ toolName: "Bash", toolCallId: "tu_9" });
    void driver;
  });
});

describe("ClaudeDriver — steer/followup dispatch", () => {
  function makeDriver() {
    const f = fakeChild();
    const driver = new ClaudeDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: new AbortController().signal,
    });
    return { f, driver };
  }

  test("idle input writes a fresh user message", async () => {
    const { f, driver } = makeDriver();
    await driver.input("steer", "FOCUS");
    expect(f.writes.at(-1)).toMatchObject({ type: "user", message: { role: "user", content: [{ type: "text", text: "FOCUS" }] } });
  });

  test("a steer while streaming interrupts then sends the message", async () => {
    const { f, driver } = makeDriver();
    await driver.sendPrompt("seed"); // now streaming
    f.writes.length = 0;
    await driver.input("steer", "FOCUS");
    expect(f.writes[0]).toMatchObject({ type: "control_request", request: { subtype: "interrupt" } });
    expect(f.writes[1]).toMatchObject({ type: "user", message: { role: "user", content: [{ type: "text", text: "FOCUS" }] } });
  });

  test("a followup while streaming is queued (user message, no interrupt)", async () => {
    const { f, driver } = makeDriver();
    await driver.sendPrompt("seed"); // streaming
    f.writes.length = 0;
    await driver.input("followup", "ALSO");
    expect(f.writes).toHaveLength(1);
    expect(f.writes[0]).toMatchObject({ type: "user", message: { role: "user", content: [{ type: "text", text: "ALSO" }] } });
  });

  test("sendPrompt writes a stream-json user message", async () => {
    const { f, driver } = makeDriver();
    await driver.sendPrompt("the prompt");
    expect(f.writes.at(-1)).toMatchObject({ type: "user", message: { role: "user", content: [{ type: "text", text: "the prompt" }] } });
  });
});

describe("ClaudeDriver — partial streaming (stream_event coalescing)", () => {
  function recordingDriver() {
    const f = fakeChild();
    const events: string[] = [];
    const driver = new ClaudeDriver(f.child, {
      onMessage: (m) => events.push(`commit:${snapText(m)}`),
      onCustom: () => {},
      onPartial: (m) => events.push(`partial:${snapText(m)}`),
      signal: new AbortController().signal,
    });
    return { f, driver, events };
  }
  function snapText(m: TranscriptMessage): string {
    const blocks = (m as { content: { type: string; text?: string }[] }).content;
    return blocks.map((b) => (b.type === "text" ? (b.text ?? "") : "")).join("");
  }
  const start = () => JSON.stringify({ type: "stream_event", event: { type: "message_start", message: { model: "m" } } });
  const blockStart = (index: number) => JSON.stringify({ type: "stream_event", event: { type: "content_block_start", index, content_block: { type: "text" } } });
  const delta = (index: number, text: string) => JSON.stringify({ type: "stream_event", event: { type: "content_block_delta", index, delta: { type: "text_delta", text } } });

  test("leading-edge partial, then the committed assistant message supersedes it", async () => {
    const { f, driver, events } = recordingDriver();
    f.emitStdout(start());
    f.emitStdout(blockStart(0));
    f.emitStdout(delta(0, "a")); // leading edge → "a"
    f.emitStdout(delta(0, "b")); // buffered → "ab"
    f.emitStdout(assistantEvent([{ type: "text", text: "ab" }])); // flush "ab" then commit
    await tick();
    expect(events).toEqual(["partial:a", "partial:ab", "commit:ab"]);
    void driver;
  });

  test("child exit drops a buffered partial WITHOUT emitting it", async () => {
    const { f, events } = recordingDriver();
    f.emitStdout(start());
    f.emitStdout(blockStart(0));
    f.emitStdout(delta(0, "x")); // leading edge → "x"
    f.emitStdout(delta(0, "y")); // buffered (not yet emitted)
    f.exit(0);
    await tick();
    expect(events).toEqual(["partial:x"]); // the buffered "xy" is dropped on teardown
  });
});

describe("ClaudeDriver — exit / abort", () => {
  test("child exit resolves a pending turn as `exited` and records the code", async () => {
    const f = fakeChild();
    const driver = new ClaudeDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: new AbortController().signal,
    });
    const p = driver.waitForTurnEnd();
    f.exit(7);
    expect(await p).toBe("exited");
    expect(driver.exitCode).toBe(7);
  });

  test("abort resolves a pending turn as `aborted`", async () => {
    const f = fakeChild();
    const ac = new AbortController();
    const driver = new ClaudeDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: ac.signal,
    });
    const p = driver.waitForTurnEnd();
    ac.abort();
    expect(await p).toBe("aborted");
  });
});

describe("ClaudeDriver — shutdown idempotency", () => {
  test("shutdown is safe to call twice and kills the child at most once", async () => {
    const f = fakeChild();
    const driver = new ClaudeDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: new AbortController().signal,
    });
    await Promise.all([driver.shutdown(1), driver.shutdown(1)]);
    expect(f.killed).toBe(1);
  });
});

describe("ClaudeDriver — diagnostics", () => {
  test("forwards non-empty stderr lines through warn, tagged claude:stderr", () => {
    const f = fakeChild();
    const warnings: string[] = [];
    new ClaudeDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: new AbortController().signal,
      warn: (m) => warnings.push(m),
    });
    f.emitStderr("\u001b[31mError: boom\u001b[0m at x.ts:1");
    f.emitStderr("   ");
    expect(warnings).toEqual(["claude:stderr: Error: boom at x.ts:1"]);
  });
});

describe("Coalescer — trailing edge + stop", () => {
  const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));
  test("stop() cancels an armed timer and drops the buffer without emitting", async () => {
    const emitted: number[] = [];
    const c = new Coalescer<number>(40, (v) => emitted.push(v));
    c.push(1); // leading edge
    c.push(2); // buffered
    c.stop();
    expect(emitted).toEqual([1]);
    await sleep(80);
    expect(emitted).toEqual([1]);
  });
});
