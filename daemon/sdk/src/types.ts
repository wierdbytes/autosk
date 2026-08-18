/**
 * Rendered view types for tasks, comments, sessions, and the registry
 * (plan §3.1, §3.2, §4).
 *
 * These are the shapes clients (CLI / lazy / GUI) and extensions read. They
 * are deliberately flat and derived server-side so a client never has to join
 * by hand. All timestamps are RFC3339 UTC strings on the wire.
 */

import type { Usage } from "./transcript.ts";
import type { StepTarget } from "./workflow.ts";

/** The five-status task enum, unchanged from v1 (plan §3.1). */
export type TaskStatus = "new" | "work" | "human" | "done" | "cancel";

/** A lightweight reference to a related task (dependency edges). */
export interface TaskRef {
  id: string;
  status: TaskStatus;
}

/**
 * The enriched task view (plan §3.1). v2 drops `priority` and `author_id`.
 * `blocked` and `blocks` are derived server-side and never stored.
 * `workflow` / `step` are `null` until the task is enrolled.
 */
export interface TaskView {
  id: string;
  title: string;
  description: string;
  status: TaskStatus;
  workflow: string | null;
  step: string | null;
  /** Derived: an open blocker exists. */
  blocked: boolean;
  blocked_by: TaskRef[];
  /** Derived: tasks this one blocks. */
  blocks: TaskRef[];
  comment_count: number;
  /**
   * Free-form, human-editable key/value bag persisted in `task.json`. Always
   * present (an empty object when the task has none). The engine reserves the
   * `step_visits` sub-object (step name → entry count) which it auto-maintains;
   * everything else is opaque to the daemon.
   */
  metadata: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

/**
 * One comment on a task (plan §3.1). Comments are a flat list, editable and
 * deletable (not strictly append-only); the daemon is the sole writer in the
 * normal path.
 */
export interface Comment {
  id: string;
  author: string;
  text: string;
  created_at: string;
  updated_at: string;
}

/** Filter for `tasks.list` / `task.list` (plan §3.4, §4). */
export interface TaskFilter {
  status?: TaskStatus | TaskStatus[];
  workflow?: string;
  step?: string;
  blocked?: boolean;
}

/** Session lifecycle status (plan §3.2). */
export type SessionStatus = "queued" | "running" | "done" | "failed" | "aborted";

/**
 * The live turn activity of a session: `"busy"` while an agent is streaming a
 * turn, `"idle"` when it has finished and is waiting for the next user message.
 * Orthogonal to {@link SessionStatus} (the lifecycle): activity is only
 * meaningful while a session is `running`. v1 surfaces it for interactive (chat)
 * sessions; it is absent on task sessions and once a session is terminal.
 */
export type SessionActivity = "idle" | "busy";

/**
 * Session origin (plan §2). A `"task"` session is created by the scheduler when
 * a `status=work` task hits an agent step; an `"interactive"` session is a
 * taskless chat opened directly against a registered agent. For interactive
 * sessions `task_id`/`workflow`/`step` are the empty-string sentinel (`""`).
 */
export type SessionKind = "task" | "interactive";

/**
 * Session metadata (`./.autosk/sessions/<id>.json`, plan §3.2). Listing a
 * task's sessions = filtering metas by `task_id`.
 *
 * For an interactive (taskless) session `kind` is `"interactive"` and
 * `task_id`/`workflow`/`step` are `""` (the unset sentinel); `agent` is the
 * registered agent name.
 */
export interface SessionMeta {
  id: string;
  kind: SessionKind;
  task_id: string;
  workflow: string;
  step: string;
  agent: string;
  status: SessionStatus;
  /**
   * Live turn activity for an interactive (`running`) session: `"busy"` while the
   * agent streams a turn, `"idle"` when it waits for the next user message.
   * Absent for task sessions and once the session is terminal.
   */
  activity?: SessionActivity;
  error?: string;
  started_at: string | null;
  ended_at: string | null;
  /**
   * Aggregated authoritative usage once the session is terminal. `null` means
   * that no complete total is available; absent means the session is still
   * live or predates usage accounting.
   */
  usage?: Usage | null;
}

/**
 * A registered agent, rendered for `registry.agent.*` (plan §3.2, parallels
 * {@link WorkflowInfo}). An agent registered via `AutoskAPI.registerAgent` can
 * back an interactive (taskless) chat session.
 */
export interface AgentInfo {
  name: string;
  description?: string;
}

/** One step of a workflow as rendered from code for `registry.workflow.*`. */
export interface WorkflowStepInfo {
  name: string;
  /**
   * The terminal/park status for a `statusStep` (`done`/`cancel`/`human`), or
   * `null` for an agent step. An agent step's `name` is the agent name.
   */
  status: "done" | "cancel" | "human" | null;
  /**
   * Targets the engine can reach from this step. Because the actual graph is
   * decided at runtime by `onTransit`, this is the conservative declared set (a
   * superset): every step in the workflow — including the step itself, since a
   * self-loop is a valid retry target — plus the terminal/park statuses.
   */
  targets: StepTarget[];
}

/**
 * A workflow rendered from code (plan §4 `registry.workflow.get`). Workflows
 * are code, not data, so this is a projection — not the source of truth.
 */
export interface WorkflowInfo {
  name: string;
  description?: string;
  /** snake_case on the wire (the `WorkflowDefinition.firstStep` projection). */
  first_step: string;
  steps: WorkflowStepInfo[];
}
