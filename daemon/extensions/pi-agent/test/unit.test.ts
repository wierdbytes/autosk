/**
 * Unit tests for the pi-agent's pure pieces: target parsing, the pi command
 * builder, and prompt rendering. The full stdio drive is covered end-to-end
 * against a stub pi in `core/test/engine.piagent.test.ts`.
 */

import { describe, expect, test } from "bun:test";
import type { ChildHandle, Comment, StepTarget, TaskView, TranscriptMessage } from "@autosk/sdk";

import type { AgentRunContext } from "@autosk/sdk";

import {
  autoskEnv,
  buildInputCommand,
  buildPiCommand,
  isBusyRejection,
  isStateMismatch,
  kickbackMessage,
  parseTarget,
  PiDriver,
  piAgent,
  renderInitialPrompt,
  rejectionMessage,
  targetLabels,
} from "../src/index.ts";
// `Coalescer` is an internal driver helper (not part of the extension's public
// index surface); the test reaches into it directly to exercise the timer path.
import { Coalescer, type PiDriverHooks, type PiDriverOptions, type TurnEnd } from "../src/driver.ts";

/**
 * pi 0.82.1's verbatim rejection of a `prompt` issued while a run is active
 * (`agent-session.js` `prompt()` → rpc-mode surfaces `e.message` unchanged).
 * The tests use the REAL string so the matcher is exercised against what pi
 * actually sends, not a paraphrase.
 */
const BUSY_REJECTION =
  "Agent is already processing. Specify streamingBehavior ('steer' or 'followUp') to queue the message.";

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

describe("buildPiCommand", () => {
  test("assembles `pi --mode rpc` with model/thinking + the injected transit tool", () => {
    const cmd = buildPiCommand({
      piBin: "/bin/pi",
      model: "sonnet:high",
      thinking: "xhigh",
      piExtensions: ["/ext/a.ts"],
      piSkills: ["brave-search"],
      extraArgs: ["--no-session"],
    });
    expect(cmd[0]).toBe("/bin/pi");
    expect(cmd.slice(1, 3)).toEqual(["--mode", "rpc"]);
    expect(cmd).toContain("--model");
    expect(cmd).toContain("sonnet:high");
    expect(cmd).toContain("--thinking");
    expect(cmd).toContain("xhigh");
    // The autosk_transit tool is injected first (its path ends with the file).
    const eIdx = cmd.indexOf("-e");
    expect(eIdx).toBeGreaterThan(0);
    expect(cmd[eIdx + 1]).toMatch(/pi-transit-extension\.ts$/);
    expect(cmd).toContain("/ext/a.ts");
    expect(cmd).toContain("--skill");
    expect(cmd).toContain("brave-search");
    expect(cmd).toContain("--no-session");
  });

  test("always injects only the transit-only pi-extension (task/comment come from @autosk/pi-tools)", () => {
    // No sandbox/thin variant: the agent injects the ack-only transit tool in
    // every task-mode run; the transport-aware @autosk/pi-tools provides
    // task/comment (over MCP under a thin sandbox, else the `autosk` CLI).
    const cmd = buildPiCommand({ piBin: "pi" });
    const ext = cmd[cmd.indexOf("-e") + 1]!;
    expect(ext).toMatch(/pi-transit-extension\.ts$/);
    expect(cmd.filter((a) => a === "-e")).toHaveLength(1);
  });

  test("defaults the binary to $AUTOSK_PI_BIN or `pi`", () => {
    const prev = process.env.AUTOSK_PI_BIN;
    delete process.env.AUTOSK_PI_BIN;
    try {
      expect(buildPiCommand({})[0]).toBe("pi");
      process.env.AUTOSK_PI_BIN = "/custom/pi";
      expect(buildPiCommand({})[0]).toBe("/custom/pi");
    } finally {
      if (prev === undefined) delete process.env.AUTOSK_PI_BIN;
      else process.env.AUTOSK_PI_BIN = prev;
    }
  });
});

describe("autoskEnv — project selector + comment authorship for the spawned pi", () => {
  test("maps the canonical project root to AUTOSK_CWD and the step to AUTOSK_AGENT", () => {
    const ctx = {
      projectRoot: "/repo/project",
      cwd: "/home/.autosk/worktrees/slug/ask-1", // worktree under isolation
      workflows: { current: { workflow: "feature-dev", step: "review", targets: [] } },
    } as unknown as AgentRunContext;
    expect(autoskEnv(ctx)).toEqual({ AUTOSK_CWD: "/repo/project", AUTOSK_AGENT: "review" });
  });
});

describe("piAgent factory", () => {
  test("exposes the four hooks and carries no name (the step key is the agent name)", () => {
    const a = piAgent();
    expect("name" in a).toBe(false);
    expect(typeof a.onRun).toBe("function");
    expect(typeof a.onSteer).toBe("function");
    expect(typeof a.onFollowup).toBe("function");
    expect(typeof a.onAbort).toBe("function");
  });
});

describe("buildInputCommand — pi steer/followup wire shape (v1 build_input_command)", () => {
  test("idle pi always takes a fresh `prompt` (steer and followup alike)", () => {
    expect(buildInputCommand("steer", "m", false)).toEqual({ cmd: { type: "prompt", message: "m" }, label: "prompt" });
    expect(buildInputCommand("followup", "m", false)).toEqual({
      cmd: { type: "prompt", message: "m" },
      label: "prompt",
    });
  });
  test("streaming pi takes `steer` / `follow_up` (snake_case) command TYPES, not a prompt", () => {
    expect(buildInputCommand("steer", "m", true)).toEqual({ cmd: { type: "steer", message: "m" }, label: "steer" });
    expect(buildInputCommand("followup", "m", true)).toEqual({
      cmd: { type: "follow_up", message: "m" },
      label: "follow_up",
    });
  });
});

describe("isStateMismatch (v1 is_state_mismatch)", () => {
  test("matches pi state-mismatch phrasings, ignores unrelated errors / empty", () => {
    expect(isStateMismatch("agent already streaming")).toBe(true);
    expect(isStateMismatch("not streaming (no active run)")).toBe(true);
    expect(isStateMismatch("STATE_MISMATCH: idle")).toBe(true);
    expect(isStateMismatch("")).toBe(false);
    expect(isStateMismatch(undefined)).toBe(false);
    expect(isStateMismatch("some other failure")).toBe(false);
  });
});

describe("PiDriver.input — idle/streaming dispatch + state-mismatch retry", () => {
  /** A fake child that RECORDS the JSON commands written to stdin and lets the
   *  test feed stdout lines (so request/response round-trips can be driven). */
  function fakeChildIO(): {
    child: ChildHandle;
    writes: Record<string, unknown>[];
    emitStdout(line: string): void;
  } {
    let stdoutCb: ((l: string) => void) | null = null;
    const writes: Record<string, unknown>[] = [];
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
      onStderr: () => {},
      kill: () => {},
      exited: new Promise(() => {}),
    };
    return { child, writes, emitStdout: (l) => stdoutCb?.(l) };
  }

  const tick = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

  function makeDriver(): { f: ReturnType<typeof fakeChildIO>; driver: PiDriver; warnings: string[] } {
    const f = fakeChildIO();
    const warnings: string[] = [];
    const driver = new PiDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: new AbortController().signal,
      warn: (m) => warnings.push(m),
    });
    return { f, driver, warnings };
  }

  /** Drains the last recorded command and acks it with the given response. */
  function ack(f: ReturnType<typeof fakeChildIO>, success: boolean, error?: string): void {
    const last = f.writes.at(-1)!;
    f.emitStdout(JSON.stringify({ type: "response", id: last.id, command: last.type, success, error }));
  }

  test("idle steer is sent as a `prompt`", async () => {
    const { f, driver } = makeDriver();
    const p = driver.input("steer", "FOCUS");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "prompt", message: "FOCUS" });
    ack(f, true);
    await p;
  });

  test("a steer issued while streaming is sent as a `steer` command", async () => {
    const { f, driver } = makeDriver();
    f.emitStdout(JSON.stringify({ type: "agent_start" })); // pi is now streaming
    const p = driver.input("steer", "FOCUS");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "steer", message: "FOCUS" });
    ack(f, true);
    await p;
  });

  test("a followup issued while streaming is sent as a `follow_up` command", async () => {
    const { f, driver } = makeDriver();
    f.emitStdout(JSON.stringify({ type: "agent_start" }));
    const p = driver.input("followup", "ALSO_DO_X");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "follow_up", message: "ALSO_DO_X" });
    ack(f, true);
    await p;
  });

  test("a state-mismatch rejection retries once with the opposite dispatch shape", async () => {
    const { f, driver } = makeDriver();
    f.emitStdout(JSON.stringify({ type: "agent_start" })); // believed streaming
    const p = driver.input("steer", "FOCUS");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "steer" }); // first attempt
    ack(f, false, "not streaming"); // pi raced us back to idle
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "prompt", message: "FOCUS" }); // opposite shape
    ack(f, true);
    await p;
  });

  test("a non-state-mismatch rejection does NOT retry and is surfaced via warn", async () => {
    const { f, driver, warnings } = makeDriver();
    f.emitStdout(JSON.stringify({ type: "agent_start" }));
    const p = driver.input("steer", "FOCUS");
    await tick();
    const writesBefore = f.writes.length;
    ack(f, false, "boom");
    await p;
    expect(f.writes.length).toBe(writesBefore); // no retry command written
    expect(warnings.some((w) => w.includes("pi rejected steer"))).toBe(true);
  });
});

describe("PiDriver — diagnostics (R1)", () => {
  /** A fake {@link ChildHandle} whose stdout/stderr/exit the test drives by hand. */
  function fakeChild(): {
    child: ChildHandle;
    emitStdout(line: string): void;
    emitStderr(line: string): void;
  } {
    let stdoutCb: ((l: string) => void) | null = null;
    let stderrCb: ((l: string) => void) | null = null;
    const stdin = {
      write: async () => {},
      close: async () => {},
    } as unknown as WritableStreamDefaultWriter<Uint8Array>;
    const child: ChildHandle = {
      stdin,
      onStdout: (cb) => void (stdoutCb = cb),
      onStderr: (cb) => void (stderrCb = cb),
      kill: () => {},
      exited: new Promise(() => {}), // never exits during these synchronous tests
    };
    return {
      child,
      emitStdout: (l) => stdoutCb?.(l),
      emitStderr: (l) => stderrCb?.(l),
    };
  }

  function driverWithWarnSink(): { f: ReturnType<typeof fakeChild>; warnings: string[] } {
    const f = fakeChild();
    const warnings: string[] = [];
    new PiDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: new AbortController().signal,
      warn: (m) => warnings.push(m),
    });
    return { f, warnings };
  }

  test("forwards non-empty pi stderr lines through the warn hook, tagged `pi:stderr`", () => {
    const { f, warnings } = driverWithWarnSink();
    f.emitStderr("Error: bad -e extension at mod.ts:10");
    f.emitStderr("   "); // blank/whitespace → drained but not surfaced
    expect(warnings).toEqual(["pi:stderr: Error: bad -e extension at mod.ts:10"]);
  });

  test("drops pi's terminal-teardown escape burst instead of surfacing it", () => {
    const { f, warnings } = driverWithWarnSink();
    // The exact teardown line pi writes to stderr on exit (mouse off, leave
    // alt-screen, end synchronized output, …) — all control bytes, no text.
    f.emitStderr(
      "\u001b[?2026h\u001b[r\u001b[?1006l\u001b[?1002l\u001b[?1000l\u001b[?1007h\u001b[?1049l\u001b[<999u\u001b[>4;0m\u001b[?2026l",
    );
    expect(warnings).toEqual([]);
  });

  test("keeps real stderr text while stripping embedded escape codes", () => {
    const { f, warnings } = driverWithWarnSink();
    f.emitStderr("\u001b[31mError: boom\u001b[0m at mod.ts:10");
    expect(warnings).toEqual(["pi:stderr: Error: boom at mod.ts:10"]);
  });

  test("warns when autosk_transit is called with no usable target", () => {
    const { f, warnings } = driverWithWarnSink();
    f.emitStdout(JSON.stringify({ type: "tool_execution_start", toolName: "autosk_transit", args: { junk: 1 } }));
    expect(warnings.some((w) => w.includes("had no usable target"))).toBe(true);
  });

  test("reports activity busy on agent_start and idle on agent_end (chat turn boundaries)", () => {
    const f = fakeChild();
    const activity: boolean[] = [];
    new PiDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: new AbortController().signal,
      onActivity: (busy) => activity.push(busy),
    });
    f.emitStdout(JSON.stringify({ type: "agent_start" })); // turn begins → busy
    f.emitStdout(JSON.stringify({ type: "agent_end" })); // turn ends → idle
    expect(activity).toEqual([true, false]);
  });
});

describe("PiDriver — partial streaming (message_update coalescing)", () => {
  function fakeChild(): { child: ChildHandle; emitStdout(line: string): void } {
    let stdoutCb: ((l: string) => void) | null = null;
    const stdin = {
      write: async () => {},
      close: async () => {},
    } as unknown as WritableStreamDefaultWriter<Uint8Array>;
    const child: ChildHandle = {
      stdin,
      onStdout: (cb) => void (stdoutCb = cb),
      onStderr: () => {},
      kill: () => {},
      exited: new Promise(() => {}),
    };
    return { child, emitStdout: (l) => stdoutCb?.(l) };
  }

  /** A minimal cumulative assistant snapshot carrying one text block. */
  function assistantSnap(text: string): unknown {
    return { role: "assistant", content: [{ type: "text", text }], provider: "stub", model: "m" };
  }
  function snapText(m: TranscriptMessage): string {
    const blocks = (m as { content: { type: string; text?: string }[] }).content;
    return blocks.map((b) => (b.type === "text" ? (b.text ?? "") : "")).join("");
  }

  /** A driver that records the ordered partial/commit event log. */
  function recordingDriver(): { f: ReturnType<typeof fakeChild>; events: string[] } {
    const f = fakeChild();
    const events: string[] = [];
    new PiDriver(f.child, {
      onMessage: (m) => events.push(`commit:${snapText(m)}`),
      onCustom: () => {},
      onPartial: (m) => events.push(`partial:${snapText(m)}`),
      signal: new AbortController().signal,
    });
    return { f, events };
  }

  const mu = (text: string) => JSON.stringify({ type: "message_update", message: assistantSnap(text) });
  const me = (text: string) => JSON.stringify({ type: "message_end", message: assistantSnap(text) });

  test("leading-edge emits the first snapshot; intermediates coalesce to the latest on message_end flush", () => {
    const { f, events } = recordingDriver();
    f.emitStdout(mu("a")); // leading edge → emitted immediately
    f.emitStdout(mu("ab")); // buffered within the window
    f.emitStdout(mu("abc")); // overwrites the buffered snapshot
    f.emitStdout(me("abc")); // flush the buffered "abc" then commit the durable line
    expect(events).toEqual(["partial:a", "partial:abc", "commit:abc"]);
  });

  test("message_end stops partial emission: no partial frame follows the committed message", () => {
    const { f, events } = recordingDriver();
    f.emitStdout(mu("x"));
    f.emitStdout(me("x")); // commit
    // A new turn's message starts a fresh leading-edge partial (proves the
    // coalescer reset), but nothing partial fires AFTER a commit within a cycle.
    f.emitStdout(mu("y"));
    f.emitStdout(me("y"));
    // The exact sequence proves that within each turn the committed line is the
    // LAST frame (no partial trails it); a new turn's leading-edge partial is a
    // fresh cycle, not a late partial for the prior committed message.
    expect(events).toEqual(["partial:x", "commit:x", "partial:y", "commit:y"]);
  });

  test("a message_end with no preceding updates commits without emitting any partial", () => {
    const { f, events } = recordingDriver();
    f.emitStdout(me("only"));
    expect(events).toEqual(["commit:only"]);
  });

  test("non-assistant message_update snapshots are ignored (no in-progress form)", () => {
    const { f, events } = recordingDriver();
    f.emitStdout(JSON.stringify({ type: "message_update", message: { role: "user", content: "hi" } }));
    f.emitStdout(JSON.stringify({ type: "message_update", message: null }));
    expect(events).toEqual([]);
  });

  test("with no onPartial hook, message_update is inert (mirrors only the commit)", () => {
    const f = fakeChild();
    const events: string[] = [];
    new PiDriver(f.child, {
      onMessage: (m) => events.push(`commit:${snapText(m)}`),
      onCustom: () => {},
      signal: new AbortController().signal,
    });
    f.emitStdout(mu("a"));
    f.emitStdout(me("a"));
    expect(events).toEqual(["commit:a"]);
  });
});

// The five tests above drive `message_end` (or no updates), so they only exercise
// the synchronous leading-edge + `flush()` paths. These cover the timer-based
// trailing edge in `Coalescer.arm()`: the re-arm loop that bounds the frame rate
// in production. Bun has no built-in timer mocking (its `useFakeTimers` only fakes
// `Date`, not `setTimeout`), so we use real timers with generous windows; the
// assertions are chosen to be robust to scheduling slack (each lands far from a
// timer boundary, or where a boundary firing is a no-op).
describe("Coalescer — trailing-edge timer (re-arm loop)", () => {
  const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));
  const WINDOW = 50;

  test("re-arms across windows, each trailing flush emits the LATEST snapshot, far fewer than the pushes", async () => {
    const emitted: number[] = [];
    const c = new Coalescer<number>(WINDOW, (v) => emitted.push(v));

    // Drive a steady stream several times faster than the window for a handful of
    // windows: the leading edge fires once, then the timer must re-arm and flush
    // the trailing LATEST value once per window while input keeps arriving.
    let pushed = 0;
    const deadline = Date.now() + WINDOW * 5;
    while (Date.now() < deadline) {
      c.push(++pushed);
      await sleep(8);
    }

    // BEFORE any flush(): a leading edge PLUS at least one timer-driven trailing
    // emit (the re-arm loop ran), yet far fewer emissions than pushes (coalescing).
    expect(emitted.length).toBeGreaterThan(1);
    expect(emitted.length).toBeLessThan(pushed);
    expect(emitted[0]).toBe(1); // the leading edge is the first push

    c.flush(); // settle the final buffered snapshot deterministically
    expect(emitted.at(-1)).toBe(pushed); // the latest cumulative value, never an intermediate
    // Every emission is the latest-at-flush-time, so they strictly increase.
    for (let i = 1; i < emitted.length; i++) expect(emitted[i]!).toBeGreaterThan(emitted[i - 1]!);
  });

  test("goes idle after an empty window so the next push is a fresh leading edge", async () => {
    const emitted: number[] = [];
    const c = new Coalescer<number>(WINDOW, (v) => emitted.push(v));

    c.push(1); // leading edge → emitted immediately; timer armed
    expect(emitted).toEqual([1]);
    await sleep(WINDOW * 3); // a window passes EMPTY → timer fires with no buffer → idle
    expect(emitted).toEqual([1]); // nothing emitted while idle (no trailing flush)
    c.push(2); // timer is null again → a FRESH leading edge, emitted immediately
    expect(emitted).toEqual([1, 2]);
  });

  test("stop() cancels an armed trailing timer and drops the buffer WITHOUT emitting", async () => {
    const emitted: number[] = [];
    const c = new Coalescer<number>(WINDOW, (v) => emitted.push(v));
    c.push(1); // leading edge → emit 1; timer armed
    c.push(2); // buffered for the trailing edge
    c.stop(); // teardown: cancel the timer and drop the buffered 2 WITHOUT emitting
    expect(emitted).toEqual([1]);
    await sleep(WINDOW * 2); // the cancelled timer must NOT fire the dropped 2
    expect(emitted).toEqual([1]);
  });
});

/**
 * pi clears its run-active flag (`_isAgentRunActive`) only at `agent_settled` —
 * AFTER the post-run phase (retry backoff, auto-compaction, queued-message
 * drain), each round of which emits an EXTRA `agent_start`/`agent_end` pair. A
 * `prompt` written in that window is rejected with "Agent is already
 * processing" (#19). These tests pin the driver's mirror of that state machine.
 */
describe("PiDriver — the agent_settled gate (#19)", () => {
  /** A fake child whose stdout, stdin writes and EXIT the test drives by hand. */
  function fakeChildIO(): {
    child: ChildHandle;
    writes: Record<string, unknown>[];
    emitStdout(line: string): void;
    exit(code: number): void;
  } {
    let stdoutCb: ((l: string) => void) | null = null;
    let exitResolve: ((v: { code: number | null }) => void) | null = null;
    const exited = new Promise<{ code: number | null }>((r) => {
      exitResolve = r;
    });
    const writes: Record<string, unknown>[] = [];
    const stdin = {
      write: async (bytes: Uint8Array) => {
        for (const line of new TextDecoder().decode(bytes).split("\n")) {
          if (line.trim() === "") continue;
          writes.push(JSON.parse(line) as Record<string, unknown>);
        }
      },
      close: async () => {},
    } as unknown as WritableStreamDefaultWriter<Uint8Array>;
    const child: ChildHandle = {
      stdin,
      onStdout: (cb) => void (stdoutCb = cb),
      onStderr: () => {},
      kill: () => {},
      exited,
    };
    return { child, writes, emitStdout: (l) => stdoutCb?.(l), exit: (code) => exitResolve?.({ code }) };
  }

  const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));
  const tick = (): Promise<void> => sleep(0);

  function makePi(
    opts: PiDriverOptions = {},
    hooks: Partial<PiDriverHooks> = {},
  ): {
    f: ReturnType<typeof fakeChildIO>;
    driver: PiDriver;
    ctl: AbortController;
    warnings: string[];
  } {
    const f = fakeChildIO();
    const ctl = new AbortController();
    const warnings: string[] = [];
    // A grace far longer than any test's timeline unless a test overrides it: the
    // default keeps the legacy feature-detect from firing mid-test.
    const driver = new PiDriver(
      f.child,
      { onMessage: () => {}, onCustom: () => {}, signal: ctl.signal, warn: (m) => warnings.push(m), ...hooks },
      { settleGraceMs: 10_000, promptRetryBackoffMs: 1, ...opts },
    );
    return { f, driver, ctl, warnings };
  }

  /** Feeds one pi event line. */
  function ev(f: ReturnType<typeof fakeChildIO>, type: string): void {
    f.emitStdout(JSON.stringify({ type }));
  }
  /** Acks the last written command with the given response. */
  function ack(f: ReturnType<typeof fakeChildIO>, success: boolean, error?: string): void {
    const last = f.writes.at(-1)!;
    f.emitStdout(JSON.stringify({ type: "response", id: last.id, command: last.type, success, error }));
  }
  const prompts = (f: ReturnType<typeof fakeChildIO>): Record<string, unknown>[] =>
    f.writes.filter((w) => w.type === "prompt");
  /**
   * Opens a prompt CYCLE the way production does — by having pi accept a
   * `prompt`. A cycle is what a turn-end belongs to, so nothing is expected from
   * `waitForTurnEnd` until one is open.
   */
  async function openCycle(f: ReturnType<typeof fakeChildIO>, driver: PiDriver, message = "go"): Promise<void> {
    const p = driver.sendPrompt(message);
    await tick();
    ack(f, true);
    await p;
  }
  /** Resolves to `"pending"` if `p` has not settled within `ms`. */
  async function within<T>(p: Promise<T>, ms: number): Promise<T | "pending"> {
    return Promise.race([p, sleep(ms).then(() => "pending" as const)]);
  }
  /** Polls until `cond()` holds; throws after `ms` so a regression fails loudly. */
  async function waitUntil(cond: () => boolean, ms = 2000): Promise<void> {
    const deadline = Date.now() + ms;
    while (!cond()) {
      if (Date.now() > deadline) throw new Error("waitUntil: timed out");
      await sleep(2);
    }
  }

  test("a prompt issued after agent_end is held until agent_settled, then accepted", async () => {
    const { f, driver } = makePi();
    ev(f, "agent_start");
    ev(f, "agent_end"); // the response finished streaming — but pi is still running
    const p = driver.sendPrompt("next");
    await tick();
    expect(prompts(f)).toHaveLength(0); // gated: nothing written into the window
    ev(f, "agent_settled"); // pi's post-run phase is over — promptable again
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "prompt", message: "next" });
    ack(f, true);
    await p; // resolves — no "Agent is already processing"
  });

  test("exactly one turn-end per prompt cycle despite intra-cycle start/end rounds", async () => {
    const { f, driver } = makePi();
    await openCycle(f, driver); // the accepted prompt opens the cycle
    ev(f, "agent_start"); // the model's turn
    ev(f, "agent_end");
    ev(f, "agent_start"); // pi's post-run compaction round (agent.continue())
    ev(f, "agent_end");
    ev(f, "agent_start"); // …and a retry round
    ev(f, "agent_end");
    ev(f, "agent_settled"); // the cycle boundary
    expect(await driver.waitForTurnEnd()).toBe("ended");
    // No second turn-end was queued by the extra rounds (it would burn a
    // correction from the kickback budget).
    expect(await within(driver.waitForTurnEnd(), 30)).toBe("pending");
  });

  test("a steer landing in the agent_end → agent_settled window travels as `steer`, not a prompt", async () => {
    const { f, driver } = makePi();
    ev(f, "agent_start");
    ev(f, "agent_end"); // inside pi's post-run window: the run is still ACTIVE
    const p = driver.input("steer", "FOCUS");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "steer", message: "FOCUS" });
    ack(f, true);
    await p;
  });

  test("child exit releases the gate and the turn wait; sendPrompt fails fast with `pi exited`", async () => {
    const { f, driver } = makePi();
    ev(f, "agent_start"); // gate closed — a settled event would normally be needed
    const turn = driver.waitForTurnEnd();
    const p = driver.sendPrompt("next");
    f.exit(1);
    expect(await within(turn, 200)).toBe("exited");
    expect(await within(p.then(() => "ok").catch((e: Error) => e.message), 200)).toContain("pi exited");
    expect(driver.exitCode).toBe(1);
  });

  test("abort releases the gate and the turn wait (no hang on a killed pi)", async () => {
    const { f, driver, ctl } = makePi();
    ev(f, "agent_start");
    const turn = driver.waitForTurnEnd();
    const p = driver.sendPrompt("next");
    ctl.abort();
    expect(await within(turn, 200)).toBe("aborted");
    expect(await within(driver.waitUntilSettled().then(() => "released"), 200)).toBe("released");
    expect(await within(p.then(() => "ok").catch((e: Error) => e.message), 200)).toContain("pi aborted");
  });

  test("a residual `already processing` rejection re-closes the gate and retries on the real boundary", async () => {
    const { f, driver } = makePi();
    const p = driver.sendPrompt("go"); // pi is idle at construction — gate open
    await tick();
    expect(prompts(f)).toHaveLength(1);
    ack(f, false, BUSY_REJECTION); // pi's own wording
    // pi PROVED it is busy: the gate re-closes, so the retry waits for the real
    // boundary instead of spinning on a fixed sleep.
    await sleep(20);
    expect(prompts(f)).toHaveLength(1);
    ev(f, "agent_settled");
    await tick();
    expect(prompts(f)).toHaveLength(2);
    ack(f, true);
    await p;
  });

  test("the retry net is bounded: after maxPromptAttempts busy rejections the prompt throws", async () => {
    // A short grace bounds the gate re-open the healing waits for (a legacy pi
    // re-fires its verdict off the re-armed grace), so the whole budget runs in ms.
    const { f, driver } = makePi({ maxPromptAttempts: 3, settleGraceMs: 10 });
    const p = driver.sendPrompt("go");
    const failed = p.then(() => "ok").catch((e: Error) => e.message);
    for (let i = 0; i < 3; i++) {
      await waitUntil(() => prompts(f).length === i + 1);
      ack(f, false, BUSY_REJECTION);
    }
    expect(await within(failed, 500)).toContain("already processing");
    expect(prompts(f)).toHaveLength(3); // bounded — no unbounded hammering
  });

  test("a non-busy rejection is NOT retried", async () => {
    const { f, driver } = makePi();
    const p = driver.sendPrompt("go");
    const failed = p.then(() => "ok").catch((e: Error) => e.message);
    await tick();
    ack(f, false, "boom");
    expect(await within(failed, 200)).toContain("boom");
    expect(prompts(f)).toHaveLength(1);
  });

  test("legacy pi (no agent_settled) is feature-detected and falls back to the agent_end boundary", async () => {
    const { f, driver, warnings } = makePi({ settleGraceMs: 30 });
    await openCycle(f, driver);
    ev(f, "agent_start");
    ev(f, "agent_end"); // this pi will never settle — the grace decides
    expect(await within(driver.waitForTurnEnd(), 500)).toBe("ended");
    expect(warnings.some((w) => w.includes("no agent_settled"))).toBe(true);
    // The gate is open again, so a prompt goes out without waiting.
    const p = driver.sendPrompt("next");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "prompt", message: "next" });
    ack(f, true);
    await p;
    // The verdict sticks: the NEXT cycle ends at agent_end immediately (no grace).
    ev(f, "agent_start");
    const turn2 = driver.waitForTurnEnd();
    ev(f, "agent_end");
    expect(await within(turn2, 10)).toBe("ended");
  });

  test("a late agent_settled after a legacy verdict heals the detection without a double turn-end", async () => {
    const { f, driver } = makePi({ settleGraceMs: 20 });
    await openCycle(f, driver);
    ev(f, "agent_start");
    ev(f, "agent_end");
    expect(await within(driver.waitForTurnEnd(), 500)).toBe("ended"); // legacy verdict
    ev(f, "agent_settled"); // …a very slow pi settles after all
    expect(await within(driver.waitForTurnEnd(), 30)).toBe("pending"); // no duplicate
    // Back on the modern boundary: agent_end alone no longer ends the cycle.
    await openCycle(f, driver, "next");
    ev(f, "agent_start");
    const turn2 = driver.waitForTurnEnd();
    ev(f, "agent_end");
    expect(await within(turn2, 60)).toBe("pending");
    ev(f, "agent_settled");
    expect(await within(turn2, 200)).toBe("ended");
  });

  test("a stray boundary event with no open cycle enqueues no phantom turn", async () => {
    const { f, driver } = makePi();
    ev(f, "agent_settled"); // e.g. a settled left over from pi's own startup
    ev(f, "agent_end");
    expect(await within(driver.waitForTurnEnd(), 30)).toBe("pending");
  });

  test("activity still reports idle at agent_end (the assistant stream is done)", async () => {
    const activity: boolean[] = [];
    const { f, driver } = makePi({}, { onActivity: (b) => activity.push(b) });
    await openCycle(f, driver);
    ev(f, "agent_start");
    ev(f, "agent_end");
    expect(activity).toEqual([true, false]); // UI idles before pi settles
    ev(f, "agent_settled");
    const turns: TurnEnd[] = [await driver.waitForTurnEnd()];
    expect(turns).toEqual(["ended"]);
  });

  test("a WRONG legacy verdict is un-learned: the busy rejection re-closes the gate and the late agent_settled lets the prompt through", async () => {
    // A modern pi whose post-run phase stayed quiet longer than the grace (an
    // unannounced pause) is mis-read as legacy — the gate re-opens while pi is
    // still running. That must not fail the session (#19).
    const { f, driver, warnings } = makePi({ settleGraceMs: 25 });
    await openCycle(f, driver);
    ev(f, "agent_start");
    ev(f, "agent_end"); // pi is compacting; no agent_settled for now
    expect(await within(driver.waitForTurnEnd(), 500)).toBe("ended"); // wrong verdict

    const sent = prompts(f).length; // the cycle-opening prompt
    const p = driver.sendPrompt("kickback"); // the agent fires its kickback
    const failed = p.then(() => "ok").catch((e: Error) => e.message);
    await waitUntil(() => prompts(f).length === sent + 1);
    ack(f, false, BUSY_REJECTION); // pi: still running
    await tick();
    expect(warnings.some((w) => w.includes("re-testing for agent_settled"))).toBe(true);

    ev(f, "agent_settled"); // pi's post-run phase finally ends
    await waitUntil(() => prompts(f).length === sent + 2);
    ack(f, true);
    expect(await within(failed, 500)).toBe("ok"); // the session survives
    // Healed back to the modern boundary: agent_end alone no longer ends a cycle.
    ev(f, "agent_start");
    const turn2 = driver.waitForTurnEnd();
    ev(f, "agent_end");
    expect(await within(turn2, 60)).toBe("pending");
    ev(f, "agent_settled");
    expect(await within(turn2, 200)).toBe("ended");
  });

  test("an announced retry backoff (auto_retry_start.delayMs) extends the detect grace instead of tripping it", async () => {
    // pi's stock retry settings sleep 2s/4s/8s between attempts, emitting only
    // `auto_retry_start` — a quiet window that must not be read as "legacy".
    const { f, driver } = makePi({ settleGraceMs: 20 });
    await openCycle(f, driver);
    ev(f, "agent_start");
    ev(f, "agent_end");
    f.emitStdout(JSON.stringify({ type: "auto_retry_start", attempt: 1, delayMs: 120 }));
    const turn = driver.waitForTurnEnd();
    // Well past the bare grace, still no verdict: the announced sleep is waited out.
    expect(await within(turn, 60)).toBe("pending");
    ev(f, "agent_start"); // the retry round starts — pi was alive all along
    ev(f, "agent_end");
    ev(f, "agent_settled");
    expect(await within(turn, 200)).toBe("ended");
  });

  test("a compaction suspends the detect grace until compaction_end (unbounded summarization call)", async () => {
    const { f, driver } = makePi({ settleGraceMs: 20 });
    await openCycle(f, driver);
    ev(f, "agent_start");
    ev(f, "agent_end");
    ev(f, "compaction_start"); // an LLM summarization of unknown length
    const turn = driver.waitForTurnEnd();
    expect(await within(turn, 80)).toBe("pending"); // no verdict while it runs
    ev(f, "compaction_end"); // the deadline is armed again from here
    ev(f, "agent_settled");
    expect(await within(turn, 200)).toBe("ended");
  });

  test("a whole cycle arriving in ONE stdout batch still produces its turn-end", async () => {
    // pi acks the prompt and then streams the entire turn without yielding — the
    // driver sees `response`, `agent_start`, `agent_end`, `agent_settled` back to
    // back. Opening the cycle must therefore happen while the RESPONSE line is
    // handled, not in a promise continuation that runs after the batch (which
    // would swallow the turn-end and hang the agent's turn loop).
    const { f, driver } = makePi();
    const p = driver.sendPrompt("go");
    await tick();
    ack(f, true);
    ev(f, "agent_start");
    ev(f, "agent_end");
    ev(f, "agent_settled");
    await p;
    expect(await within(driver.waitForTurnEnd(), 200)).toBe("ended");
  });

  test("a busy rejection batched with the agent_settled that follows it does not stall the retry", async () => {
    // pi rejects the prompt and settles a beat later, both in one stdout batch.
    // The rejection must be applied in STREAM ORDER (before the settled line), or
    // the healed "pi is busy" view would land on top of the fresh settled one and
    // park the retry on a gate nothing will re-open.
    const { f, driver } = makePi(); // grace 10s: a stall would be plainly visible
    const p = driver.sendPrompt("go");
    await tick();
    ack(f, false, BUSY_REJECTION);
    ev(f, "agent_settled");
    await waitUntil(() => prompts(f).length === 2, 300); // retried at once
    ack(f, true);
    await p;
  });

  test("an accepted prompt closes the gate: pi is running before its agent_start arrives", async () => {
    const { f, driver } = makePi();
    await openCycle(f, driver); // accepted — pi's run-active flag is set
    const second = driver.sendPrompt("too early");
    await sleep(20);
    expect(prompts(f)).toHaveLength(1); // held: no second prompt into a live run
    ev(f, "agent_start");
    ev(f, "agent_end");
    ev(f, "agent_settled");
    await waitUntil(() => prompts(f).length === 2);
    ack(f, true);
    await second;
  });

  test("against a MODERN pi a busy rejection waits for agent_settled for as long as it takes — no hammering", async () => {
    // A real agent turn runs for MINUTES. Once this pi has proven it emits
    // `agent_settled`, the gate's re-open is guaranteed (pi emits it from
    // `_runAgentPrompt`'s finally; exit/abort release it too), so the wait must
    // not be ceiling-timed — a timed retry would hammer a legitimately busy pi,
    // burn the whole attempt budget and fail the session with the very error #19
    // is about.
    const { f, driver } = makePi({ settleGraceMs: 20, maxPromptAttempts: 3, promptRetryBackoffMs: 1 });
    // One clean cycle → settleSupport = "modern".
    await openCycle(f, driver);
    ev(f, "agent_start");
    ev(f, "agent_end");
    ev(f, "agent_settled");
    expect(await driver.waitForTurnEnd()).toBe("ended");

    // A run started behind our back (e.g. a user followup that our idle view
    // dispatched as a `prompt`), so the kickback is busy-rejected.
    const sent = prompts(f).length;
    const outcome = driver
      .sendPrompt("kickback")
      .then(() => "ok")
      .catch((e: Error) => e.message);
    await waitUntil(() => prompts(f).length === sent + 1);
    ack(f, false, BUSY_REJECTION);

    // Far past settleGraceMs and every backoff: still exactly ONE outstanding
    // write, and the prompt has not failed.
    await sleep(200);
    expect(prompts(f)).toHaveLength(sent + 1);
    expect(await within(outcome, 20)).toBe("pending");

    ev(f, "agent_settled"); // the foreign run ends — pi is promptable again
    await waitUntil(() => prompts(f).length === sent + 2);
    ack(f, true);
    expect(await within(outcome, 500)).toBe("ok");
  });

  test("the retry's ceiling timer is cancelled once the gate wins the race", async () => {
    // The ceiling is `settleGraceMs + margin` — ten seconds in production — and
    // fires long after sendPrompt returned, several per prompt. Track the LIVE
    // (armed, never cleared) timers of exactly that duration.
    const settleGraceMs = 200;
    const promptRetryBackoffMs = 64;
    const ceilingMs = settleGraceMs + promptRetryBackoffMs; // retryWaitFor(1)
    const live = new Set<unknown>();
    const realSetTimeout = globalThis.setTimeout;
    const realClearTimeout = globalThis.clearTimeout;
    globalThis.setTimeout = ((fn: () => void, ms?: number, ...rest: unknown[]) => {
      const h = realSetTimeout(fn, ms, ...(rest as []));
      if (ms === ceilingMs) live.add(h);
      return h;
    }) as typeof globalThis.setTimeout;
    globalThis.clearTimeout = ((h: Parameters<typeof globalThis.clearTimeout>[0]) => {
      live.delete(h);
      realClearTimeout(h);
    }) as typeof globalThis.clearTimeout;
    try {
      const { f, driver } = makePi({ settleGraceMs, promptRetryBackoffMs });
      const p = driver.sendPrompt("go");
      await waitUntil(() => prompts(f).length === 1);
      ack(f, false, BUSY_REJECTION); // support is still `unknown` → ceiling armed
      await waitUntil(() => live.size === 1);
      ev(f, "agent_settled"); // the gate wins
      await waitUntil(() => prompts(f).length === 2);
      ack(f, true);
      await p;
      expect(live.size).toBe(0); // the loser was cancelled, not left burning
    } finally {
      globalThis.setTimeout = realSetTimeout;
      globalThis.clearTimeout = realClearTimeout;
    }
  });

  test("a compaction outside a prompt cycle cannot CREATE a legacy verdict (pi's manual `compact`)", async () => {
    // pi emits compaction_start/compaction_end for the MANUAL compact() command
    // too, with no agent_end in sight. A post-run signal may only extend, suspend
    // or resume a deadline an `agent_end` created — never invent one, or a legacy
    // verdict (gate open + turn-end) would be reached from an event that says
    // nothing about whether this pi emits `agent_settled`.
    const { f, driver, warnings } = makePi({ settleGraceMs: 20 });
    await openCycle(f, driver); // a cycle is open and pi is mid-turn
    ev(f, "agent_start");
    ev(f, "compaction_start");
    ev(f, "compaction_end");
    await sleep(100); // several graces
    expect(warnings.some((w) => w.includes("no agent_settled"))).toBe(false);
    expect(await within(driver.waitForTurnEnd(), 30)).toBe("pending"); // no phantom turn
    // …and the gate stayed shut: pi is still running.
    const sent = prompts(f).length;
    void driver.sendPrompt("next").catch(() => {});
    await sleep(30);
    expect(prompts(f)).toHaveLength(sent);
  });
});

describe("PiDriver.input — healing the run-state view from pi's rejections", () => {
  /** The same hand-driven fake child as the gate suite. */
  function fakeChildIO(): {
    child: ChildHandle;
    writes: Record<string, unknown>[];
    emitStdout(line: string): void;
  } {
    let stdoutCb: ((l: string) => void) | null = null;
    const writes: Record<string, unknown>[] = [];
    const stdin = {
      write: async (bytes: Uint8Array) => {
        for (const line of new TextDecoder().decode(bytes).split("\n")) {
          if (line.trim() === "") continue;
          writes.push(JSON.parse(line) as Record<string, unknown>);
        }
      },
      close: async () => {},
    } as unknown as WritableStreamDefaultWriter<Uint8Array>;
    const child: ChildHandle = {
      stdin,
      onStdout: (cb) => void (stdoutCb = cb),
      onStderr: () => {},
      kill: () => {},
      exited: new Promise(() => {}),
    };
    return { child, writes, emitStdout: (l) => stdoutCb?.(l) };
  }

  const tick = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

  function makePi(): { f: ReturnType<typeof fakeChildIO>; driver: PiDriver } {
    const f = fakeChildIO();
    const driver = new PiDriver(
      f.child,
      { onMessage: () => {}, onCustom: () => {}, signal: new AbortController().signal },
      { settleGraceMs: 10_000 },
    );
    return { f, driver };
  }

  function ack(f: ReturnType<typeof fakeChildIO>, success: boolean, error?: string): void {
    const last = f.writes.at(-1)!;
    f.emitStdout(JSON.stringify({ type: "response", id: last.id, command: last.type, success, error }));
  }

  test("a busy rejection teaches the driver pi IS running: the next input goes out as `follow_up` on the FIRST write", async () => {
    const { f, driver } = makePi();
    // Our view says idle (no agent_start seen), pi says otherwise.
    const first = driver.input("followup", "ONE");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "prompt", message: "ONE" });
    ack(f, false, BUSY_REJECTION);
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "follow_up", message: "ONE" }); // retry shape
    ack(f, true);
    await first;

    // The view is healed, so the SECOND input pays no double round-trip.
    const before = f.writes.length;
    const second = driver.input("followup", "TWO");
    await tick();
    expect(f.writes).toHaveLength(before + 1);
    expect(f.writes.at(-1)).toMatchObject({ type: "follow_up", message: "TWO" });
    ack(f, true);
    await second;
  });

  test("a `not streaming` rejection teaches the opposite: the next input goes out as a `prompt` first try", async () => {
    const { f, driver } = makePi();
    f.emitStdout(JSON.stringify({ type: "agent_start" })); // believed streaming
    const first = driver.input("steer", "ONE");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "steer" });
    ack(f, false, "not streaming (no active run)"); // pi raced us back to idle
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "prompt", message: "ONE" });
    ack(f, true);
    await first;

    // Healed to idle — but the accepted prompt re-opened a run, so the next input
    // is a `steer` again. (Both directions of the view are now pi's, not ours.)
    const second = driver.input("steer", "TWO");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "steer", message: "TWO" });
    ack(f, true);
    await second;
  });
});

describe("isBusyRejection", () => {
  test("matches pi's run-active phrasings only", () => {
    expect(isBusyRejection(BUSY_REJECTION)).toBe(true); // pi 0.82.1, verbatim
    expect(isBusyRejection("Agent is already processing a request")).toBe(true);
    expect(isBusyRejection("agent already streaming (in_progress)")).toBe(true);
    expect(isBusyRejection("not streaming (no active run)")).toBe(false);
    expect(isBusyRejection("boom")).toBe(false);
    expect(isBusyRejection(undefined)).toBe(false);
  });
});

describe("prompt rendering", () => {
  const task: TaskView = {
    id: "ask-1",
    title: "Build auth",
    description: "Add login",
    status: "work",
    workflow: "feature-dev",
    step: "dev",
    blocked: false,
    blocked_by: [],
    blocks: [],
    comment_count: 1,
    metadata: {},
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
  const targets: StepTarget[] = [{ step: "dev" }, { step: "review" }, { status: "done" }, { status: "cancel" }, { status: "human" }];
  const comments: Comment[] = [
    { id: "c1", author: "alice", text: "use bcrypt", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z" },
  ];

  test("initial prompt names the agent/step, lists targets, and instructs the tool", () => {
    const p = renderInitialPrompt({
      firstMessage: "You are the DEVELOPER.\n\n",
      agentName: "@x/dev",
      workflow: "feature-dev",
      step: "dev",
      task,
      targets,
      comments,
    });
    expect(p).toContain("You are the DEVELOPER.");
    expect(p).toContain('You are agent "@x/dev" on step "dev" of workflow "feature-dev".');
    expect(p).toContain("Task: ask-1");
    expect(p).toContain("Title: Build auth");
    expect(p).toContain("Add login");
    expect(p).toContain("  - review");
    expect(p).toContain("  - done");
    expect(p).toContain("call the `autosk_transit` tool exactly once");
    expect(p).toContain("[alice] use bcrypt");
  });

  test("targetLabels dedups and preserves order", () => {
    expect(targetLabels(targets)).toEqual(["dev", "review", "done", "cancel", "human"]);
  });

  test("kickback and rejection messages reference the corrective attempt", () => {
    expect(kickbackMessage("ask-1", targets, 1, 3)).toContain("correction attempt 1 of 3");
    expect(kickbackMessage("ask-1", targets, 1, 3)).toContain("autosk_transit");
    const r = rejectionMessage({ step: "docs" }, "park it", targets, 2, 3);
    expect(r).toContain('to "docs" was rejected: park it');
    expect(r).toContain("correction attempt 2 of 3");
  });
});
