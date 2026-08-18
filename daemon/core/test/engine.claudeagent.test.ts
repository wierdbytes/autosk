/**
 * claude-agent end-to-end through the engine against a STUB `claude -p`
 * stream-json process (claude-agent acceptance #2/#4/#6). The stub
 * (`extensions/claude-agent/test/fixtures/stub-claude.ts`) speaks the Claude Code
 * stream-json line protocol; the real `@autosk/claude-agent` driver runs
 * unchanged, with `claudeBin` pointed at the stub.
 *
 * Covered: happy transit via `mcp__autosk__transit`; kickback after a missed
 * turn then success; max-corrections exhaustion → no transit → engine parks
 * (`agent_did_not_transit`); steer forwarded into the live claude; abort
 * terminates the run; and the transcript golden (mirrored assistant message
 * entries validate against the P1 transcript types).
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
import { claudeAgent } from "@autosk/claude-agent";

import { parseTranscript } from "../src/index.ts";
import { makeEngine, makeProject, type TestProject } from "./engineHarness.ts";
import { waitFor, waitForComplete } from "./helpers.ts";

const STUB = fileURLToPath(new URL("../../extensions/claude-agent/test/fixtures/stub-claude.ts", import.meta.url));

beforeAll(() => {
  chmodSync(STUB, 0o755); // executable so the `#!/usr/bin/env bun` shebang runs it
});

describe("engine — claude-agent over a stub claude", () => {
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
    transitOnTurn?: number;
    maxCorrections?: number;
    steps?: (ag: AgentDefinition) => Record<string, StepDef>;
    firstStep?: string;
  }

  async function start(opts: ScenarioOpts): Promise<{ p: TestProject; engine: ReturnType<typeof makeEngine>["engine"]; taskId: string }> {
    // autoskTools:false — the stub does not run a real MCP server, and these
    // tests drive the transit channel directly via the stub's tool_use events.
    const ag = claudeAgent({ claudeBin: STUB, maxCorrections: opts.maxCorrections, autoskTools: false });
    const wf: WorkflowDefinition = {
      name: "w",
      firstStep: opts.firstStep ?? "do",
      steps: opts.steps ? opts.steps(ag) : { do: ag },
    };
    const p = track(await makeProject({ workflows: [wf] }));
    writeFileSync(
      join(p.root, ".stub-claude.json"),
      JSON.stringify({ scenario: opts.scenario, to: opts.to ?? "done", transitOnTurn: opts.transitOnTurn ?? 2 }),
    );
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);
    const task = await p.store.createTask({ title: "claude task" });
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

  test("happy path: the model calls transit and the task completes", async () => {
    const { p, taskId } = await start({ scenario: "transit", to: "done" });
    await waitForComplete(p.store, taskId, "done", 15000);
    const sessions = p.store.sessions.sessionsForTask(taskId);
    expect(sessions).toHaveLength(1);
    expect(sessions[0]!.status).toBe("done");
    expect(sessions[0]!.usage).toMatchObject({ input: 1, output: 1, totalTokens: 2 });
  }, 20000);

  test("a sibling-step transit drives the next step", async () => {
    const { p, taskId } = await start({
      scenario: "transit",
      to: "review",
      steps: (ag) => ({ dev: ag, review: statusStep("human") }),
      firstStep: "dev",
    });
    await waitForComplete(p.store, taskId, "human", 15000);
    const v = await p.store.taskView(taskId);
    expect(v.step).toBe("review");
    expect(v.status).toBe("human");
    const steps = p.store.sessions.sessionsForTask(taskId).map((m) => m.step);
    expect(steps).toEqual(["dev"]);
  }, 20000);

  test("kickback: a missed turn is corrected, then the retry transits", async () => {
    const { p, taskId } = await start({ scenario: "kickback_then_transit", to: "done", transitOnTurn: 2 });
    await waitForComplete(p.store, taskId, "done", 15000);
    const sid = p.store.sessions.sessionsForTask(taskId)[0]!.id;
    expect(await transcript(p, sid)).toContain("correction attempt 1");
  }, 20000);

  test("exhaustion: never transiting parks the task (agent_did_not_transit)", async () => {
    const { p, taskId } = await start({ scenario: "never_transit", maxCorrections: 1 });
    await waitForComplete(p.store, taskId, "human", 15000);
    const sessions = p.store.sessions.sessionsForTask(taskId);
    expect(sessions[0]!.status).toBe("failed");
    expect(sessions[0]!.error).toBe("agent_did_not_transit");
  }, 20000);

  test("steer is forwarded into the live claude and lets the run complete", async () => {
    const { p, engine, taskId } = await start({ scenario: "steer", to: "done" });
    const sid = await liveSessionId(p, taskId);
    // Wait until the first turn is actually streaming (its ack is mirrored) so the
    // steer is delivered mid-stream and travels as a real interrupt + message.
    await waitFor(async () => (await transcript(p, sid)).includes("ack:"), 15000);
    const res = await engine.sessionInput(p.root, sid, { kind: "steer", message: "PLEASE_FOCUS_XYZ" });
    expect(res.handled).toBe(true);
    await waitForComplete(p.store, taskId, "done", 15000);
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

  test("transcript golden: mirrored assistant entries validate against the P1 types", async () => {
    const { p, taskId } = await start({ scenario: "transit", to: "done" });
    await waitForComplete(p.store, taskId, "done", 15000);
    const sid = p.store.sessions.sessionsForTask(taskId)[0]!.id;
    const lines = parseTranscript(await transcript(p, sid));

    const header = lines[0] as SessionHeader;
    expect(header.type).toBe("session");
    expect(header.version).toBe(1);
    expect(header.agent).toBe("do");
    expect(header.task_id).toBe(taskId);

    const entries = lines.slice(1) as Exclude<TranscriptLine, SessionHeader>[];
    for (const e of entries) {
      expect(e.id).toMatch(/^[0-9a-f]{8}$/);
      expect(typeof e.timestamp).toBe("string");
      expect("parentId" in e).toBe(false);
    }

    const messages = entries.filter((e) => e.type === "message") as MessageEntry[];
    const assistant = messages.map((m) => m.message).find((m) => m.role === "assistant") as AssistantMessage;
    expect(assistant).toBeDefined();
    expect(assistant.provider).toBe("anthropic");
    expect(assistant.model).toBe("stub-model");
    expect(assistant.content[0]).toMatchObject({ type: "text" });

    const kinds = entries.map((e) =>
      e.type === "custom" ? `custom:${(e as { customType: string }).customType}` : e.type,
    );
    expect(kinds).toContain("custom:autosk:transit");
    expect(kinds).toContain("custom:autosk:session_end");
  }, 20000);
});
