/**
 * ExtensionRegistry (plan §3.6): registration + error isolation, step-shape
 * validation (inline agent vs statusStep), live-code validation, and the
 * rendered proto-v2 views.
 *
 * These exercise the registry directly (no filesystem); the loader test wires
 * the same registry up to on-disk fixtures.
 */

import { describe, expect, test } from "bun:test";

import { ExtensionRegistry, renderWorkflowInfo } from "../src/index.ts";
import { statusStep, type AgentDefinition, type StepDef, type WorkflowDefinition } from "@autosk/sdk";

const agentStep = (): AgentDefinition => ({ async onRun() {} });

const wf = (name: string, extra: Partial<WorkflowDefinition> = {}): WorkflowDefinition => ({
  name,
  firstStep: "dev",
  steps: { dev: agentStep(), review: agentStep(), accept: statusStep("human") },
  ...extra,
});

describe("ExtensionRegistry registration", () => {
  test("registers workflows (agents inline) and exposes sorted names", () => {
    const r = new ExtensionRegistry();
    r.addWorkflow("src", wf("beta"));
    r.addWorkflow("src", wf("alpha"));

    expect(r.workflowNames()).toEqual(["alpha", "beta"]);
    expect(r.diagnostics).toEqual([]);
  });

  test("a duplicate workflow name keeps the first and records a diagnostic", () => {
    const r = new ExtensionRegistry();
    r.addWorkflow("first", wf("alpha", { description: "the original" }));
    r.addWorkflow("second", wf("alpha", { description: "the loser" }));

    // First registration wins (higher priority source).
    expect(r.getWorkflowInfo("alpha")?.description).toBe("the original");
    expect(r.diagnostics).toEqual([{ source: "second", error: "duplicate workflow name: alpha" }]);
  });

  test("registers named agents, resolves + renders them sorted, and rejects bad/duplicate ones", () => {
    const r = new ExtensionRegistry();
    r.addAgent("src", { name: "pi", description: "chat", agent: agentStep() });
    r.addAgent("src", { name: "alpha", agent: agentStep() });
    // An empty name and a missing onRun are each a skipped diagnostic.
    r.addAgent("bad", { name: "", agent: agentStep() });
    r.addAgent("bad", { name: "noRun", agent: {} as unknown as AgentDefinition });
    // First-registered wins on a duplicate (surfaced via diagnostics).
    r.addAgent("second", { name: "pi", description: "loser", agent: agentStep() });

    expect(r.listAgents()).toEqual([
      { name: "alpha" },
      { name: "pi", description: "chat" },
    ]);
    expect(r.getAgentInfo("pi")).toEqual({ name: "pi", description: "chat" });
    expect(typeof r.resolveAgent("pi")?.onRun).toBe("function");
    expect(r.resolveAgent("nope")).toBeUndefined();
    expect(r.diagnostics.map((d) => d.error)).toEqual([
      "registerAgent: registration.name must be a non-empty string",
      'registerAgent: "noRun" agent must define an onRun function',
      "duplicate agent name: pi",
    ]);
  });

  test("rejects an empty name and a firstStep that is not a declared step", () => {
    const r = new ExtensionRegistry();
    r.addWorkflow("s", { name: "", firstStep: "do", steps: { do: agentStep() } });
    r.addWorkflow("s", { name: "bad", firstStep: "missing", steps: { do: agentStep() } });

    expect(r.workflowNames()).toEqual([]);
    expect(r.diagnostics.map((d) => d.error)).toEqual([
      "registerWorkflow: workflow.name must be a non-empty string",
      'registerWorkflow: "bad" firstStep "missing" is not a declared step',
    ]);
  });

  test("accepts a statusStep firstStep (a task that parks/closes on enroll)", () => {
    const r = new ExtensionRegistry();
    r.addWorkflow("s", { name: "park", firstStep: "accept", steps: { accept: statusStep("human") } });
    expect(r.workflowNames()).toEqual(["park"]);
    expect(r.diagnostics).toEqual([]);
  });

  test("rejects a step that is neither an agent nor a statusStep", () => {
    const r = new ExtensionRegistry();
    r.addWorkflow("ext", {
      name: "bogus",
      firstStep: "dev",
      steps: { dev: { foo: 1 } as unknown as StepDef },
    });
    expect(r.workflowNames()).toEqual([]);
    expect(r.diagnostics).toEqual([
      {
        source: "ext",
        error: 'registerWorkflow: "bogus" step "dev" must be an agent (with onRun) or a statusStep',
      },
    ]);
  });

  test("rejects a statusStep with an invalid status", () => {
    const r = new ExtensionRegistry();
    r.addWorkflow("ext", {
      name: "bogus",
      firstStep: "dev",
      steps: { dev: { status: "ship" } as unknown as StepDef },
    });
    expect(r.workflowNames()).toEqual([]);
    expect(r.diagnostics).toEqual([
      {
        source: "ext",
        error: 'registerWorkflow: "bogus" step "dev" has invalid status "ship" (expected done|cancel|human)',
      },
    ]);
  });

  test("diagnostics getter returns a defensive copy", () => {
    const r = new ExtensionRegistry();
    r.recordDiagnostic("a", "boom");
    const snap = r.diagnostics;
    snap.push({ source: "x", error: "y" });
    expect(r.diagnostics).toHaveLength(1);
  });
});

describe("validatePosition (live-code hazard)", () => {
  test("ok for a known workflow + step", () => {
    const r = new ExtensionRegistry();
    r.addWorkflow("s", wf("feature-dev"));
    expect(r.validatePosition("feature-dev", "dev")).toEqual({ ok: true });
  });

  test("workflow_missing for an unknown workflow", () => {
    const r = new ExtensionRegistry();
    expect(r.validatePosition("ghost", "dev")).toEqual({
      ok: false,
      error: "workflow_missing: ghost",
    });
  });

  test("workflow_missing for a known workflow with an unknown step", () => {
    const r = new ExtensionRegistry();
    r.addWorkflow("s", wf("feature-dev"));
    expect(r.validatePosition("feature-dev", "ship")).toEqual({
      ok: false,
      error: "workflow_missing: feature-dev has no step ship",
    });
  });
});

describe("renderWorkflowInfo (proto-v2 projection)", () => {
  test("per-step targets = every declared step (self included) + the three terminal statuses", () => {
    const info = renderWorkflowInfo(
      wf("feature-dev", {
        description: "two-stepper",
        steps: { recon: agentStep(), design: agentStep(), approve: statusStep("human") },
      }),
    );
    expect(info.name).toBe("feature-dev");
    expect(info.description).toBe("two-stepper");
    expect(info.first_step).toBe("dev");

    expect(info.steps.map((s) => s.name)).toEqual(["recon", "design", "approve"]);

    const design = info.steps.find((s) => s.name === "design")!;
    // An agent step renders status: null.
    expect(design.status).toBeNull();
    // The superset projection declares EVERY step, including the self-loop
    // ({step:"design"}) — a structurally valid retry target.
    expect(design.targets).toEqual([
      { step: "recon" },
      { step: "design" },
      { step: "approve" },
      { status: "done" },
      { status: "cancel" },
      { status: "human" },
    ]);

    const accept = info.steps.find((s) => s.name === "approve")!;
    // A statusStep renders its park/terminal status.
    expect(accept.status).toBe("human");
  });

  test("description is omitted when absent", () => {
    const info = renderWorkflowInfo(wf("iso", {}));
    expect("description" in info).toBe(false);
  });
});
