/**
 * Contract tests for the `stub-pi` fixture itself.
 *
 * The stub is the stand-in every pi-agent end-to-end test drives, so its model
 * of pi's RUN-ACTIVE flag has to stay honest: the flag goes up at `agent_start`
 * and down only at `agent_settled`, which real pi emits ONCE per prompt cycle
 * from `_runAgentPrompt`'s `finally` — after its post-run phase (retry backoff /
 * auto-compaction / queued-message drain). The `settleMs` knob reproduces that
 * window, and everything the driver does about #19 is tested against it.
 *
 * The window is where the interesting behaviour lives (AC2): a `prompt` landing
 * inside it is rejected with pi's verbatim busy message, while a `steer` /
 * `follow_up` is accepted and starts a new run — which must NOT be settled by
 * the previous cycle's still-pending timer.
 */

import { afterEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const STUB = fileURLToPath(new URL("./fixtures/stub-pi.ts", import.meta.url));

/** A running stub with its stdout parsed into a growing list of JSON lines. */
interface Stub {
  send(cmd: Record<string, unknown>): void;
  events: Record<string, unknown>[];
  /** Waits until `pred` matches an event, and returns its index. */
  waitFor(pred: (e: Record<string, unknown>) => boolean, ms?: number): Promise<number>;
  kill(): void;
}

function startStub(cfg: Record<string, unknown>): { stub: Stub; cleanup: () => void } {
  const dir = mkdtempSync(join(tmpdir(), "stub-pi-"));
  writeFileSync(join(dir, ".stub-pi.json"), JSON.stringify(cfg));
  const proc = Bun.spawn([process.execPath, STUB, "--mode", "rpc"], {
    cwd: dir,
    stdin: "pipe",
    stdout: "pipe",
    stderr: "inherit",
  });
  const events: Record<string, unknown>[] = [];
  void (async () => {
    const decoder = new TextDecoder();
    let buf = "";
    for await (const chunk of proc.stdout) {
      buf += decoder.decode(chunk, { stream: true });
      let nl: number;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (line !== "") events.push(JSON.parse(line) as Record<string, unknown>);
      }
    }
  })();
  const stub: Stub = {
    send(cmd) {
      proc.stdin.write(JSON.stringify(cmd) + "\n");
      void proc.stdin.flush();
    },
    events,
    async waitFor(pred, ms = 5000) {
      const deadline = Date.now() + ms;
      for (;;) {
        const i = events.findIndex(pred);
        if (i >= 0) return i;
        if (Date.now() > deadline) throw new Error(`stub-pi: timed out waiting; saw ${JSON.stringify(events)}`);
        await new Promise((r) => setTimeout(r, 5));
      }
    },
    kill: () => proc.kill(),
  };
  return {
    stub,
    cleanup: () => {
      proc.kill();
      rmSync(dir, { recursive: true, force: true });
    },
  };
}

describe("stub-pi fixture — pi's run-active flag and its post-run window", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const c of cleanups.splice(0)) c();
  });

  function start(cfg: Record<string, unknown>): Stub {
    const { stub, cleanup } = startStub(cfg);
    cleanups.push(cleanup);
    return stub;
  }

  const isType = (t: string) => (e: Record<string, unknown>): boolean => e.type === t;
  const count = (stub: Stub, t: string): number => stub.events.filter((e) => e.type === t).length;

  test("a follow_up delivered inside the post-run window is accepted and its run is settled exactly once", async () => {
    // 400ms of "pi is still running" after agent_end — the window a kickback used
    // to be written into (#19).
    const stub = start({ scenario: "never_transit", settleMs: 400 });

    stub.send({ id: "1", type: "prompt", message: "one" });
    await stub.waitFor(isType("agent_end"));
    expect(count(stub, "agent_settled")).toBe(0); // still running: the flag is up

    // AC2: an input landing in the window travels as `follow_up` (the shape the
    // driver picks while pi is streaming) and pi QUEUES it — a `prompt` here
    // would be rejected instead (asserted below).
    stub.send({ id: "2", type: "follow_up", message: "IN_WINDOW" });
    const ack = await stub.waitFor((e) => e.type === "response" && e.id === "2");
    expect(stub.events[ack]).toMatchObject({ success: true, command: "follow_up" });

    // The follow_up starts a NEW run inside the old cycle's settle window. The
    // stale timer must not fire mid-run (real pi settles once per cycle, from
    // `_runAgentPrompt`'s finally) — so no `agent_settled` may appear before this
    // second run's own `agent_end`.
    const secondStart = await stub.waitFor(isType("agent_start"), 5000);
    expect(secondStart).toBeGreaterThan(0);
    await stub.waitFor((e) => e.type === "message_end" && JSON.stringify(e).includes("IN_WINDOW"));
    const settledBeforeSecondEnd = stub.events
      .slice(0, stub.events.findLastIndex(isType("agent_end")) + 1)
      .filter(isType("agent_settled")).length;
    expect(settledBeforeSecondEnd).toBe(0);

    // …and the cycle then ends exactly once.
    await stub.waitFor(isType("agent_settled"));
    await new Promise((r) => setTimeout(r, 500)); // outlive the stale timer
    expect(count(stub, "agent_settled")).toBe(1);
    expect(count(stub, "agent_start")).toBe(2);
  }, 15000);

  test("a prompt inside the post-run window is rejected with pi's own wording", async () => {
    const stub = start({ scenario: "never_transit", settleMs: 400 });
    stub.send({ id: "1", type: "prompt", message: "one" });
    await stub.waitFor(isType("agent_end"));

    stub.send({ id: "2", type: "prompt", message: "kickback" });
    const i = await stub.waitFor((e) => e.type === "response" && e.id === "2");
    expect(stub.events[i]).toMatchObject({ success: false });
    // pi 0.82.1's exact text (agent-session.js `prompt()`, surfaced verbatim by
    // rpc-mode) — including the `streamingBehavior` escape hatch it names.
    expect(stub.events[i]!.error).toBe(
      "Agent is already processing. Specify streamingBehavior ('steer' or 'followUp') to queue the message.",
    );

    // Once settled, the same prompt is accepted.
    await stub.waitFor(isType("agent_settled"));
    stub.send({ id: "3", type: "prompt", message: "kickback" });
    const j = await stub.waitFor((e) => e.type === "response" && e.id === "3");
    expect(stub.events[j]).toMatchObject({ success: true });
  }, 15000);

  test("without the knob the stub settles immediately (no window)", async () => {
    const stub = start({ scenario: "never_transit" });
    stub.send({ id: "1", type: "prompt", message: "one" });
    await stub.waitFor(isType("agent_settled"));
    const end = stub.events.findIndex(isType("agent_end"));
    const settled = stub.events.findIndex(isType("agent_settled"));
    expect(settled).toBe(end + 1); // back-to-back
  }, 15000);
});
