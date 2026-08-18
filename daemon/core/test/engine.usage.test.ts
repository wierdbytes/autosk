import { afterEach, describe, expect, test } from "bun:test";
import type { AgentDefinition, Usage, WorkflowDefinition } from "@autosk/sdk";

import { makeEngine, makeProject, type TestProject } from "./engineHarness.ts";
import { waitForComplete } from "./helpers.ts";

const usage = (input: number, output: number, cost = 0): Usage => ({
  input,
  output,
  cacheRead: 0,
  cacheWrite: 0,
  totalTokens: input + output,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: cost },
});

describe("engine — session usage", () => {
  const cleanups: (() => void)[] = [];
  const engines: { stop(): void }[] = [];

  afterEach(() => {
    for (const engine of engines.splice(0)) engine.stop();
    for (const cleanup of cleanups.splice(0)) cleanup();
  });

  async function run(agent: AgentDefinition): Promise<{ project: TestProject; taskId: string }> {
    const workflow: WorkflowDefinition = { name: "usage", firstStep: "run", steps: { run: agent } };
    const project = await makeProject({ workflows: [workflow] });
    cleanups.push(project.cleanup);
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(project.project);
    const task = await project.store.createTask({ title: "usage accounting" });
    await engine.enroll(project.root, task.id, { workflow: workflow.name });
    return { project, taskId: task.id };
  }

  test("aggregates canonical turn reports into terminal session metadata", async () => {
    const { project, taskId } = await run({
      async onRun(ctx) {
        ctx.log.usage(usage(10, 4, 0.2));
        ctx.log.usage(usage(6, 2, 0.1));
        await ctx.transit({ status: "done" });
      },
    });
    await waitForComplete(project.store, taskId, "done");

    const session = project.store.sessions.sessionsForTask(taskId)[0]!;
    expect(session.usage).toMatchObject({ input: 16, output: 6, totalTokens: 22 });
    expect(session.usage?.cost.total).toBeCloseTo(0.3);
    const { lines } = await project.store.sessions.readTranscript(session.id);
    expect(lines.filter((line) => line.type === "custom" && line.customType === "autosk:usage"))
      .toHaveLength(2);
  });

  test("keeps unavailable, missing, and explicit zero distinct", async () => {
    const unavailable = await run({
      async onRun(ctx) {
        ctx.log.usage(usage(3, 1));
        ctx.log.usage(null);
        await ctx.transit({ status: "done" });
      },
    });
    await waitForComplete(unavailable.project.store, unavailable.taskId, "done");
    expect(unavailable.project.store.sessions.sessionsForTask(unavailable.taskId)[0]!.usage).toBeNull();

    const missing = await run({
      async onRun(ctx) {
        await ctx.transit({ status: "done" });
      },
    });
    await waitForComplete(missing.project.store, missing.taskId, "done");
    expect(missing.project.store.sessions.sessionsForTask(missing.taskId)[0]!.usage).toBeNull();

    const zero = await run({
      async onRun(ctx) {
        ctx.log.usage(usage(0, 0));
        await ctx.transit({ status: "done" });
      },
    });
    await waitForComplete(zero.project.store, zero.taskId, "done");
    expect(zero.project.store.sessions.sessionsForTask(zero.taskId)[0]!.usage).toEqual(usage(0, 0));
  });

  test("preserves completed-turn usage when the session fails", async () => {
    const { project, taskId } = await run({
      async onRun(ctx) {
        ctx.log.usage(usage(7, 2));
        throw new Error("provider failed");
      },
    });
    await waitForComplete(project.store, taskId, "human");
    const session = project.store.sessions.sessionsForTask(taskId)[0]!;
    expect(session.status).toBe("failed");
    expect(session.usage).toEqual(usage(7, 2));
  });
});
