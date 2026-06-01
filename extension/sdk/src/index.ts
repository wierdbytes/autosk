/**
 * @autosk/agent-sdk
 *
 * Types and tiny helpers for authoring custom-runner autosk agents.
 *
 * A custom agent package exports a default function of type
 * {@link RunAgent}. The autosk daemon spawns the Node bootstrapper
 * (@autosk/agent-runtime), which imports the runner module, constructs
 * a {@link RunContext}, and calls the default export. The runner is
 * expected to call `ctx.stepNext(...)` exactly once before the function
 * resolves; otherwise the run is failed with
 * `agent_did_not_emit_transition`.
 *
 * See docs/plans/20260518-Agent-Packages.md for the design.
 */

/** Snapshot of a task as seen by an agent at spawn time. */
export interface TaskSnapshot {
  id: string;
  title: string;
  description: string;
  status: "work";
  priority: number;
  workflow_id: string;
  current_step_id: string;
  created_at: string; // ISO-8601
  updated_at: string; // ISO-8601
}

/** Snapshot of the workflow step the agent is currently working on. */
export interface StepSnapshot {
  id: string;
  name: string;
  /** Full npm package name of the agent that owns this step. */
  agent: string;
}

/** Snapshot of the workflow containing the current step. */
export interface WorkflowSnapshot {
  id: string;
  name: string;
  description: string;
}

/** A comment rendered as `[<author>@<ts>]: <text>`. */
export interface CommentSnapshot {
  /** The pre-formatted line; agents that need structured access should
   * use `ctx.cli(["comment", "list", task.id, "--json"])` instead. */
  line: string;
}

/**
 * One outgoing transition from the current step. The agent must pick
 * exactly one before completing the run.
 */
export interface Transition {
  /** "step" means a sibling step within the same workflow; "task_status"
   * means one of the terminal states (done|cancel|human). */
  kind: "step" | "task_status";
  /** The target name: sibling step name, or one of done|cancel|human. */
  target: string;
  /** Author-supplied natural-language rule describing when this transition applies. */
  prompt_rule: string;
}

/** Result of a child process shelled by `ctx.cli`. */
export interface CliResult {
  stdout: string;
  stderr: string;
  code: number;
}

/** Options for `ctx.spawnPi`. */
export interface PiSpawnOpts {
  /** Text written to the spawned pi's stdin as its first user message.
   * Not a system-role prompt — pi has its own system prompt; this is
   * the leading user-turn content. */
  firstMessage?: string;
  model?: string;
  thinking?: "off" | "minimal" | "low" | "medium" | "high" | "xhigh";
  extraArgs?: string[];
  extensions?: string[];
  skills?: string[];
}

export interface PiResult {
  exitCode: number;
  /** Tail of the spawned pi stdout JSONL stream, capped by the runtime. */
  stdout?: string;
  /** Tail of the spawned pi stderr stream, capped by the runtime. */
  stderr?: string;
  /** Diagnosable failure summary when the subprocess/protocol failed. */
  error?: string;
  /** Whether an `agent_end` event was observed for the submitted prompt. */
  agentEnd?: boolean;
}

/**
 * The context object passed to a custom runner's default export.
 *
 * Helpers (`cli`, `stepNext`, `spawnPi`) are wired by the bootstrapper
 * to shell the autosk CLI and pi binaries with the right cwd and env.
 */
export interface RunContext {
  task: TaskSnapshot;
  step: StepSnapshot;
  workflow: WorkflowSnapshot;
  comments: CommentSnapshot[];
  transitions: Transition[];

  projectRoot: string;
  jobId: string;
  /** Full npm package name of this agent (= agents.name in the DB). */
  agentName: string;
  agentVersion: string;
  agentInstall: string;

  /** Shell the autosk CLI with cwd = projectRoot and AUTOSK_AGENT = agentName. */
  cli(args: string[]): Promise<CliResult>;
  /** Convenience over `cli(["step","next",task.id,"--to",to])`. Must be called exactly once. */
  stepNext(to: string): Promise<void>;
  /** Spawn `pi --mode rpc` and await `agent_end`. */
  spawnPi(opts: PiSpawnOpts): Promise<PiResult>;
}

/** Custom-runner entry-point signature. */
export type RunAgent = (ctx: RunContext) => Promise<void>;

/** Default export shape every custom-runner module is expected to ship. */
export default interface AgentModule {
  default: RunAgent;
}
