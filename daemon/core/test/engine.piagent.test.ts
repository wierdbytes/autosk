/**
 * pi-agent end-to-end through the engine against a STUB `pi --mode rpc` process
 * (P6 acceptance #2/#3). The stub (`extensions/pi-agent/test/fixtures/stub-pi.ts`)
 * speaks the pi RPC line protocol; the real `@autosk/pi-agent` driver runs
 * unchanged, with `piBin` pointed at the stub.
 *
 * Covered: happy transit via `autosk_transit`; kickback after a missed turn then
 * success; max-corrections exhaustion → no transit → engine parks
 * (`agent_did_not_transit`); steer forwarded into the live pi; abort terminates
 * the run; and the transcript golden (mirrored pi message entries validate
 * against the P1 transcript types).
 */

import { afterEach, beforeAll, describe, expect, test } from "bun:test";
import { chmodSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  statusStep,
  type AgentDefinition,
  type AssistantMessage,
  type MessageEntry,
  type SessionHeader,
  type StepDef,
  type TranscriptLine,
  type WorkflowDefinition,
} from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";

import { parseTranscript } from "../src/index.ts";
import { makeEngine, makeProject, type TestProject } from "./engineHarness.ts";
import { waitFor, waitForComplete } from "./helpers.ts";

const STUB = fileURLToPath(new URL("../../extensions/pi-agent/test/fixtures/stub-pi.ts", import.meta.url));

beforeAll(() => {
  chmodSync(STUB, 0o755); // executable so the `#!/usr/bin/env bun` shebang runs it
});

describe("engine — pi-agent over a stub pi", () => {
  const cleanups: (() => void)[] = [];
  const engines: { stop(): void }[] = [];
  afterEach(() => {
    for (const e of engines.splice(0)) e.stop();
    for (const c of cleanups.splice(0)) c();
  });
  function track(p: TestProject): TestProject {
    cleanups.push(p.cleanup);
    return p;
  }

  interface ScenarioOpts {
    scenario: string;
    to?: string;
    transitOnTurn?: string;
    maxCorrections?: number;
    /**
     * Delay (ms) the stub keeps its run ACTIVE after `agent_end` before emitting
     * `agent_settled` — pi's post-run (compaction / retry / queued-drain) window,
     * in which a `prompt` is rejected with "Agent is already processing" (#19).
     */
    settleMs?: number;
    /** Builds the workflow steps from the pi agent (the step key is its name). */
    steps?: (ag: AgentDefinition) => Record<string, StepDef>;
    firstStep?: string;
  }

  async function start(opts: ScenarioOpts): Promise<{ p: TestProject; engine: ReturnType<typeof makeEngine>["engine"]; taskId: string }> {
    const ag = piAgent({ piBin: STUB, maxCorrections: opts.maxCorrections });
    const wf: WorkflowDefinition = {
      name: "w",
      firstStep: opts.firstStep ?? "do",
      steps: opts.steps ? opts.steps(ag) : { do: ag },
    };
    const p = track(await makeProject({ workflows: [wf] }));
    // The stub reads its scenario from `<cwd>/.stub-pi.json` (cwd = ctx.cwd =
    // project root), not env — Bun.spawn ignores the parent's runtime env edits.
    writeFileSync(
      join(p.root, ".stub-pi.json"),
      JSON.stringify({
        scenario: opts.scenario,
        to: opts.to ?? "done",
        transitOnTurn: opts.transitOnTurn ? Number.parseInt(opts.transitOnTurn, 10) : 2,
        settleMs: opts.settleMs ?? 0,
      }),
    );
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);
    const task = await p.store.createTask({ title: "pi task" });
    await engine.enroll(p.root, task.id, { workflow: "w" });
    return { p, engine, taskId: task.id };
  }

  async function liveSessionId(p: TestProject, taskId: string): Promise<string> {
    await waitFor(() => {
      const metas = p.store.sessions.sessionsForTask(taskId);
      return metas.length > 0 && metas[0]!.status === "running";
    }, 15000);
    return p.store.sessions.sessionsForTask(taskId)[0]!.id;
  }

  async function transcript(p: TestProject, sessionId: string): Promise<string> {
    return Bun.file(p.store.paths.sessionTranscript(sessionId)).text();
  }

  test("happy path: the model calls autosk_transit and the task completes", async () => {
    const { p, taskId } = await start({ scenario: "transit", to: "done" });
    await waitForComplete(p.store, taskId, "done", 15000);
    const sessions = p.store.sessions.sessionsForTask(taskId);
    expect(sessions).toHaveLength(1);
    expect(sessions[0]!.status).toBe("done");
  }, 20000);

  test("happy path with a sibling-step transit drives the next step", async () => {
    const { p, taskId } = await start({
      scenario: "transit",
      to: "review",
      // `review` is a statusStep(human) so the dev→review hop lands and the
      // task PARKS there — a clean settled terminal, not a review→review hot loop.
      steps: (ag) => ({ dev: ag, review: statusStep("human") }),
      firstStep: "dev",
    });
    // The task settles parked at `review`; assert the settled state, not a
    // transient one (waitForComplete also guarantees no session is still live).
    await waitForComplete(p.store, taskId, "human", 15000);
    const v = await p.store.taskView(taskId);
    expect(v.step).toBe("review");
    expect(v.status).toBe("human");
    // Exactly one session ran — the dev step that transited to review; the human
    // review step is never dispatched (so no stub-pi self-loop is left spinning).
    const steps = p.store.sessions.sessionsForTask(taskId).map((m) => m.step);
    expect(steps).toEqual(["dev"]);
  }, 20000);

  test("kickback: a missed turn is corrected, then the retry transits", async () => {
    const { p, taskId } = await start({
      scenario: "kickback_then_transit",
      to: "done",
      transitOnTurn: "2",
    });
    await waitForComplete(p.store, taskId, "done", 15000);
    const sid = p.store.sessions.sessionsForTask(taskId)[0]!.id;
    // The turn-2 prompt the stub echoed back is the corrective message, proving a
    // kickback was issued between the missed turn and the successful transit.
    expect(await transcript(p, sid)).toContain("correction attempt 1");
  }, 20000);

  /**
   * Regression for #19: pi clears its run-active flag only at `agent_settled`,
   * which lands AFTER `agent_end` (its post-run phase compacts / retries /
   * drains queued messages first). The kickback prompt the agent fires at the
   * turn boundary used to be written into that window and rejected with "Agent
   * is already processing", failing the session. The driver now gates prompts on
   * `agent_settled`, so the whole cycle completes. Fails against the old driver.
   */
  test("kickback survives pi's post-run window (agent_end → agent_settled delay, #19)", async () => {
    const { p, taskId } = await start({
      scenario: "kickback_then_transit",
      to: "done",
      transitOnTurn: "2",
      settleMs: 300, // the stub rejects any prompt for 300ms after agent_end
    });
    await waitForComplete(p.store, taskId, "done", 15000);
    const sid = p.store.sessions.sessionsForTask(taskId)[0]!.id;
    const text = await transcript(p, sid);
    expect(text).toContain("correction attempt 1"); // the kickback landed
    expect(text).not.toContain("already processing"); // and was never rejected
    expect(p.store.sessions.sessionsForTask(taskId)[0]!.status).toBe("done");
  }, 20000);

  test("exhaustion: never transiting parks the task (agent_did_not_transit)", async () => {
    const { p, taskId } = await start({ scenario: "never_transit", maxCorrections: 1 });
    await waitForComplete(p.store, taskId, "human", 15000);
    const sessions = p.store.sessions.sessionsForTask(taskId);
    expect(sessions[0]!.status).toBe("failed");
    expect(sessions[0]!.error).toBe("agent_did_not_transit");
  }, 20000);

  test("steer is forwarded into the live pi and lets the run complete", async () => {
    const { p, engine, taskId } = await start({ scenario: "steer", to: "done" });
    const sid = await liveSessionId(p, taskId);
    // Wait until pi's first turn is actually STREAMING (its echo of the initial
    // prompt has been mirrored into the transcript) before firing the steer, so
    // it is delivered MID-STREAM and therefore travels as a real `steer` command
    // (the driver's streaming branch) — genuinely exercising the live-forward
    // path end-to-end rather than racing the initial prompt (R2b).
    await waitFor(async () => (await transcript(p, sid)).includes("ack:"), 15000);
    const res = await engine.sessionInput(p.root, sid, { kind: "steer", message: "PLEASE_FOCUS_XYZ" });
    expect(res.handled).toBe(true);
    await waitForComplete(p.store, taskId, "done", 15000);
    // The steer text reached pi (the stub echoed it as an assistant message that
    // the driver mirrored into the transcript).
    expect(await transcript(p, sid)).toContain("PLEASE_FOCUS_XYZ");
  }, 20000);

  test("abort terminates the run: session aborted, task parked to human", async () => {
    const { p, engine, taskId } = await start({ scenario: "abort_hang" });
    const sid = await liveSessionId(p, taskId);
    const res = await engine.sessionAbort(p.root, sid);
    expect(res.handled).toBe(true);
    await waitFor(async () => (await p.store.taskView(taskId)).status === "human", 15000);
    await waitFor(() => p.store.sessions.sessionsForTask(taskId)[0]!.status === "aborted", 15000);
    expect(p.store.sessions.sessionsForTask(taskId)[0]!.status).toBe("aborted");
  }, 20000);

  test("transcript golden: mirrored pi message entries validate against the P1 types", async () => {
    const { p, taskId } = await start({ scenario: "transit", to: "done" });
    await waitForComplete(p.store, taskId, "done", 15000);
    const sid = p.store.sessions.sessionsForTask(taskId)[0]!.id;
    const lines = parseTranscript(await transcript(p, sid));

    // Header line.
    const header = lines[0] as SessionHeader;
    expect(header.type).toBe("session");
    expect(header.version).toBe(1);
    // The step key IS the agent name (inline step agents) — this is a single-step
    // `do` workflow, so the session's agent is "do".
    expect(header.agent).toBe("do");
    expect(header.task_id).toBe(taskId);

    const entries = lines.slice(1) as Exclude<TranscriptLine, SessionHeader>[];
    for (const e of entries) {
      expect(e.id).toMatch(/^[0-9a-f]{8}$/);
      expect(typeof e.timestamp).toBe("string");
      expect("parentId" in e).toBe(false);
    }

    // At least one mirrored pi message entry, a valid AssistantMessage.
    const messages = entries.filter((e) => e.type === "message") as MessageEntry[];
    expect(messages.length).toBeGreaterThan(0);
    const m = messages[0]!.message as AssistantMessage;
    expect(m.role).toBe("assistant");
    expect(m.provider).toBe("stub");
    expect(m.model).toBe("stub-model");
    expect(m.stopReason).toBe("stop");
    expect(m.usage.totalTokens).toBe(2);
    expect(m.content[0]).toMatchObject({ type: "text" });

    // The engine's structural entries close the transcript.
    const kinds = entries.map((e) =>
      e.type === "custom" ? `custom:${(e as { customType: string }).customType}` : e.type,
    );
    expect(kinds).toContain("custom:autosk:transit");
    expect(kinds).toContain("custom:autosk:session_end");
  }, 20000);
});
