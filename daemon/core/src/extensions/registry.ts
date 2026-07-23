/**
 * The per-project extension registry (plan §3.6, §3.7(1)).
 *
 * Each opened project gets its own registry, populated by running the union of
 * global + project-local extension factories (see `loader.ts`). It holds the
 * registered workflows (code, not data — agents are inline step values, not a
 * separate map), the load diagnostics, and the rendered views the RPC layer
 * (P5) serves over `registry.workflow.*` and `project.diagnostics`.
 *
 * Design rule from the plan:
 *  - **Error isolation.** A name collision or an invalid definition is never
 *    fatal: it is recorded as a diagnostic and the offending registration is
 *    skipped (first-registered wins, so the higher-priority source — project
 *    beats global — keeps the name). The factory keeps running; the rest of the
 *    registry stays usable (pi's `ExtensionRunner.onError` model).
 */

import {
  isStatusStep,
  type AgentDefinition,
  type AgentInfo,
  type AgentRegistration,
  type ExtensionLoadError,
  type StepDef,
  type StepTarget,
  type WorkflowDefinition,
  type WorkflowInfo,
  type WorkflowStepInfo,
} from "@autosk/sdk";

/** The result of validating a `(workflow, step)` position against the registry. */
export interface PositionValidation {
  ok: boolean;
  /** Present (with the `workflow_missing:` prefix) when `ok` is false. */
  error?: string;
}

export class ExtensionRegistry {
  private readonly workflows = new Map<string, WorkflowDefinition>();
  /** Named agents registered via `AutoskAPI.registerAgent` (interactive sessions). */
  private readonly agents = new Map<string, AgentRegistration>();
  private readonly loadErrors: ExtensionLoadError[] = [];

  // -- diagnostics ---------------------------------------------------------

  /** The accumulated load diagnostics (a fresh copy). */
  get diagnostics(): ExtensionLoadError[] {
    return this.loadErrors.map((e) => ({ ...e }));
  }

  /** Records one load diagnostic (an extension that failed or collided). */
  recordDiagnostic(source: string, error: string): void {
    this.loadErrors.push({ source, error });
  }

  // -- registration (called by the per-extension AutoskAPI handle) ---------

  /**
   * Registers a workflow on behalf of `source`. A duplicate name, an empty
   * name, an absent `steps`, a `firstStep` that is not a declared step, or a
   * step value that is neither an agent (`onRun`) nor a valid `statusStep`
   * (`status` ∈ {done,cancel,human}) are each recorded as a diagnostic and skip
   * the registration (never throw — the factory keeps running). First-registered
   * wins on a collision. The definition is stored exactly as written — agents
   * live inline in `steps`, so there is no harvest into a separate agent map.
   */
  addWorkflow(source: string, workflow: WorkflowDefinition): void {
    const name = workflow?.name;
    if (typeof name !== "string" || name.length === 0) {
      this.recordDiagnostic(source, "registerWorkflow: workflow.name must be a non-empty string");
      return;
    }
    if (this.workflows.has(name)) {
      this.recordDiagnostic(source, `duplicate workflow name: ${name}`);
      return;
    }
    if (!workflow.steps || typeof workflow.steps !== "object") {
      this.recordDiagnostic(source, `registerWorkflow: "${name}" has no steps`);
      return;
    }
    if (!(workflow.firstStep in workflow.steps)) {
      this.recordDiagnostic(
        source,
        `registerWorkflow: "${name}" firstStep "${workflow.firstStep}" is not a declared step`,
      );
      return;
    }
    for (const [stepName, step] of Object.entries(workflow.steps)) {
      const err = validateStep(name, stepName, step);
      if (err) {
        this.recordDiagnostic(source, err);
        return;
      }
    }
    this.workflows.set(name, workflow);
  }

  /**
   * Registers a named agent on behalf of `source` (plan §3.3, §4.1). An empty
   * name, a missing `onRun`, or a duplicate name are each recorded as a
   * diagnostic and skip the registration (never throw — the factory keeps
   * running). First-registered wins on a collision, so the higher-priority
   * source (project beats global) keeps the name.
   */
  addAgent(source: string, registration: AgentRegistration): void {
    const name = registration?.name;
    if (typeof name !== "string" || name.length === 0) {
      this.recordDiagnostic(source, "registerAgent: registration.name must be a non-empty string");
      return;
    }
    const agent = registration.agent;
    if (!agent || typeof agent.onRun !== "function") {
      this.recordDiagnostic(source, `registerAgent: "${name}" agent must define an onRun function`);
      return;
    }
    if (this.agents.has(name)) {
      this.recordDiagnostic(source, `duplicate agent name: ${name}`);
      return;
    }
    this.agents.set(name, registration);
  }

  // -- resolution (engine-facing) ------------------------------------------

  /** Resolves a registered workflow by name. Unknown ⇒ `undefined`. */
  resolveWorkflow(name: string): WorkflowDefinition | undefined {
    return this.workflows.get(name);
  }

  /** Resolves a registered agent's definition by name. Unknown ⇒ `undefined`. */
  resolveAgent(name: string): AgentDefinition | undefined {
    return this.agents.get(name)?.agent;
  }

  // -- live-code hazard validation -----------------------------------------

  /**
   * Validates an in-flight task's `(workflow, step)` against the registry
   * (plan §3.6). An unknown workflow OR a step missing from a known workflow
   * both yield `{ ok:false, error:"workflow_missing: …" }`.
   */
  validatePosition(workflow: string, step: string): PositionValidation {
    const wf = this.workflows.get(workflow);
    if (!wf) return { ok: false, error: `workflow_missing: ${workflow}` };
    if (!(step in wf.steps)) {
      return { ok: false, error: `workflow_missing: ${workflow} has no step ${step}` };
    }
    return { ok: true };
  }

  // -- rendered views (proto-v2) -------------------------------------------

  /** Registered workflows rendered for `registry.workflow.list`, sorted by name. */
  listWorkflows(): WorkflowInfo[] {
    return [...this.workflows.values()]
      .map(renderWorkflowInfo)
      .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  }

  /** A single workflow rendered for `registry.workflow.get`. */
  getWorkflowInfo(name: string): WorkflowInfo | undefined {
    const wf = this.workflows.get(name);
    return wf ? renderWorkflowInfo(wf) : undefined;
  }

  /** A single registered agent rendered for `registry.agent.*`. Unknown ⇒ `undefined`. */
  getAgentInfo(name: string): AgentInfo | undefined {
    const reg = this.agents.get(name);
    return reg ? renderAgentInfo(reg) : undefined;
  }

  /** Registered agents rendered for `registry.agent.list`, sorted by name. */
  listAgents(): AgentInfo[] {
    return [...this.agents.values()]
      .map(renderAgentInfo)
      .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  }

  /** Registered workflow names (sorted) — the deterministic merge result. */
  workflowNames(): string[] {
    return [...this.workflows.keys()].sort();
  }
}

/** Renders an {@link AgentRegistration} to its {@link AgentInfo} wire projection. */
function renderAgentInfo(reg: AgentRegistration): AgentInfo {
  const info: AgentInfo = { name: reg.name };
  if (reg.description !== undefined) info.description = reg.description;
  return info;
}

/**
 * Validates one workflow step value. Returns an error message (for a
 * diagnostic) or `null` when the step is a valid agent or `statusStep`. A
 * `statusStep`'s `status` must be one of the three terminal/park statuses; an
 * agent step must carry an `onRun` function.
 */
function validateStep(workflow: string, stepName: string, step: StepDef): string | null {
  if (step && typeof step === "object") {
    if (isStatusStep(step)) {
      if (step.status === "done" || step.status === "cancel" || step.status === "human") {
        return null;
      }
      return `registerWorkflow: "${workflow}" step "${stepName}" has invalid status ${JSON.stringify(step.status)} (expected done|cancel|human)`;
    }
    if (typeof (step as { onRun?: unknown }).onRun === "function") return null;
  }
  return `registerWorkflow: "${workflow}" step "${stepName}" must be an agent (with onRun) or a statusStep`;
}

/**
 * Renders a {@link WorkflowDefinition} (code) to its {@link WorkflowInfo} wire
 * projection (plan §4 `registry.workflow.get`).
 *
 * Per-step `targets` is the **conservative declared set** (a superset): because
 * the real graph is decided at runtime by `onTransit`, the engine cannot know
 * the exact edges, so it declares every step in the workflow — INCLUDING the
 * step itself, since a self-loop (re-run the same agent) is a structurally valid
 * retry target — plus the three terminal/park statuses. Step names preserve the
 * workflow author's declaration order.
 */
export function renderWorkflowInfo(wf: WorkflowDefinition): WorkflowInfo {
  const stepNames = Object.keys(wf.steps);
  const statusTargets: StepTarget[] = [
    { status: "done" },
    { status: "cancel" },
    { status: "human" },
  ];
  // Every step (the self-loop included) is a declared target — the projection is
  // a superset of the runtime graph, not the sibling-only subset.
  const stepTargets: StepTarget[] = stepNames.map((s) => ({ step: s }));
  const steps: WorkflowStepInfo[] = stepNames.map((stepName) => {
    const def = wf.steps[stepName]!;
    return {
      name: stepName,
      status: isStatusStep(def) ? def.status : null,
      targets: [...stepTargets, ...statusTargets],
    };
  });
  const info: WorkflowInfo = {
    name: wf.name,
    first_step: wf.firstStep,
    steps,
  };
  if (wf.description !== undefined) info.description = wf.description;
  return info;
}
