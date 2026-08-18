# Workflows & agents

A **workflow** is a directed graph of **steps**; each step is owned by an
**agent** (or by a human). The daemon (`autoskd`) drives a task through its
workflow: it schedules the current step's agent, runs it, and follows the
transition the agent (or a human) chooses.

In v2, **workflows and agents are code** — TypeScript registered by
[extensions](extensions.md), not JSON loaded into a database and not npm-package
"agents" you install. The engine knows nothing about graphs, visit caps, or
prompts; it only drives the task status machine and calls the hooks your code
defines. This page is the reference for the contracts your extension registers
(from [`@autosk/sdk`](../daemon/sdk/src)) and the CLI verbs that drive them.

> **Clean break from v1.** There is no in-place workflow editor and no
> agent-package installer — editing a workflow means editing its extension code,
> and agents are code too (each step's agent is declared **inline** in the
> workflow). `autosk workflow` is now a **read-only** view of what the project's
> extensions registered.

## The task status machine

A task has one of five statuses (unchanged from v1):

| Status | Meaning |
| --- | --- |
| `new` | Open work, not enrolled in a workflow. |
| `work` | Enrolled and owned by the engine — an agent is (or will be) on it. |
| `human` | Parked, waiting for a person (a `human` step, a park, or a failure). |
| `done` | Completed. |
| `cancel` | Abandoned. |

`autosk ready` returns the *ready set*: `new` tasks with no open blocker. That's
what humans and agents pull from. Enrolling a task moves it to `work`; the engine
takes over from there. For the full task model, blockers, and `metadata` /
`step_visits`, see [docs/concepts.md](concepts.md).

## Workflow definitions

A workflow is a [`WorkflowDefinition`](../daemon/sdk/src/workflow.ts):

```ts
interface WorkflowDefinition {
  name: string;                       // unique within a project's registry
  description?: string;
  firstStep: string;                  // the step a freshly-enrolled task enters
  steps: Record<string, StepDef>;     // step name → definition
  onTransit?(ctx: TransitContext, to: StepTarget): void | Promise<void>;
}

// A step is EITHER an inline agent OR a terminal/park status step.
// Discriminated structurally: an AgentDefinition has `onRun`; a StatusStep
// has `status`. The step key is the agent name.
type StepDef = AgentDefinition | StatusStep;

interface StatusStep {
  status: "done" | "cancel" | "human";   // build one with statusStep(...)
}

type StepTarget =
  | { step: string }                              // a sibling step (self-loop allowed)
  | { status: "done" | "cancel" | "human" };      // a terminal / park status
```

- **`firstStep`** is where `task.enroll {workflow}` lands the task.
- An **agent step** is an inline [`AgentDefinition`](#agent-definitions) (it has
  an `onRun`): the daemon schedules it and runs that agent's `onRun`. The step
  key is the agent's name.
- A **status step** (`statusStep(...)`) is one the engine never schedules:
  entering it moves the task to that status. `statusStep("human")` parks the task
  and waits for a person to `resume` it; `statusStep("done")` /
  `statusStep("cancel")` close the task (recording the step it ended on).

A `statusStep` is built with the SDK helper:

```ts
import { statusStep } from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";

steps: {
  dev:     piAgent({ firstMessageFile: ".../dev.md" }),  // agent step (name "dev")
  accept:  statusStep("human"),                          // park for a human
  shipped: statusStep("done"),                           // close as done
}
```

### `onTransit` — the only graph authority

`onTransit` is called by the engine for **every** transition — enroll →
`firstStep`, step → step, step → terminal/park, and `resume --to`. Throw (or
return a rejected promise) to **reject** a transition; an absent `onTransit`
allows everything. This is where you put graph shape, guards, and visit caps —
the engine has no opinion of its own.

```ts
interface TransitContext {
  taskId: string;
  workflow: string;                   // the workflow this transition belongs to
  step: string;                       // the step the task is leaving
  visits(step: string): number;       // entries into `step` so far (from metadata.step_visits)
  tasks: TasksAPI;                    // live task access (re-reads the store)
  comment(text: string): Promise<void>;
}
```

`visits(step)` is the convenience for the common `max_visits` pattern. For
example, to cap how many times a task can bounce back to `dev`:

```ts
onTransit(ctx, to) {
  if ("step" in to && to.step === "dev" && ctx.visits("dev") >= 5) {
    throw new Error("dev bounced back too many times — park it");
  }
}
```

**How the count is kept.** `visits(step)` reads a **persistent** counter the
engine maintains in the task's [`metadata.step_visits`](daemon.md#task-metadata)
map — it no longer counts session files. The engine increments
`metadata.step_visits[step]` by one on **every transition INTO a named step**:
enroll → `firstStep`, a step→step `transit`, and a `resume` into a step
(including a *bare* `resume`, which re-enters the parked step). A `{ status }`
target (a `done` / `cancel` / `human` flip) and the administrative `reopen` /
park do **not** count. The bump commits atomically with the position write.

The count carries **prior-entries** semantics: `onTransit` runs *before* the
bump, so inside the hook `ctx.visits(target)` is the number of times the task
entered `target` **before** this transition (the cap above fires on the 6th
bounce when the threshold is `5`). Because the counter lives in human-editable
metadata, it is **resettable**: `autosk metadata unset <id> step_visits` (or
`metadata set <id> step_visits.dev 0`) lets a capped task proceed again — the
escape hatch for a task that legitimately needs more passes.

When `onTransit` throws on an agent-chosen target, the error propagates back to
the agent, which may retry with a different target (the `@autosk/pi-agent`
extension turns this into a corrective "kickback" message). If the agent never
produces an accepted transition, the task is parked to `human`.

## Agent definitions

An agent is an [`AgentDefinition`](../daemon/sdk/src/agent.ts) — an inline step
value. There is no `name` field and no separate agent registry: the **step key
is the agent name**, and registering the workflow registers its agents.

```ts
interface AgentDefinition {
  onRun(ctx: AgentRunContext): Promise<void>;
  onSteer?(ctx: AgentRunContext, message: string): Promise<void>;
  onFollowup?(ctx: AgentRunContext, message: string): Promise<void>;
  onAbort?(ctx: AgentRunContext): Promise<void>;
}
```

`onRun` executes **one full step** in-process and **MUST call `ctx.transit(...)`
exactly once** before returning. Returning without a transit fails the session
(`error="agent_did_not_transit"`) and parks the task to `human`. The engine has
no harness knowledge — CLI-driven agents are an extension on top of `ctx.spawn` +
`ctx.transit`. Two are shipped: [`@autosk/pi-agent`](../daemon/extensions/pi-agent/README.md)
(`piAgent({...})`, drives `pi --mode rpc`) and its structural twin
[`@autosk/claude-agent`](../daemon/extensions/claude-agent/README.md)
(`claudeAgent({...})`, drives Claude Code's `claude -p` headless stream-json).

### The run context

`onRun` receives an [`AgentRunContext`](../daemon/sdk/src/agent.ts):

```ts
interface AgentRunContext {
  session: { id: string };
  mode: "task" | "interactive";       // "task" = workflow step (must transit); "interactive" = chat (never transits)
  cwd: string;                        // run dir — ALWAYS the project root (the agent owns any sandbox run dir)
  projectRoot: string;                // canonical project root (the dir containing `.autosk/`)
  signal: AbortSignal;                // fired on abort / daemon shutdown

  tasks: TasksAPI;                    // live task access (current/get/list/comments)
  workflows: WorkflowsAPI;            // live registry + current { workflow, step, targets }
  log: TranscriptAPI;                 // transcript writer (message / usage / custom)
  partial(message: TranscriptMessage): void;  // ephemeral live snapshot (NOT persisted)

  comment(text: string): Promise<void>;    // shorthand: comment on the current task
  transit(to: StepTarget): Promise<void>;  // validate via onTransit, then commit (once)

  // Mint a per-session, host-side HTTP MCP server (for a sandboxed harness's tool surface).
  newMCPServer(): Promise<{ url: string; port: number; token: string; close(): Promise<void> }>;

  exec(cmd: string[], opts?): Promise<ExecResult>;  // one-shot child process
  spawn(cmd: string[], opts?): ChildHandle;         // long-lived interactive child
}
```

- **`transit`** resolves the target → calls `workflow.onTransit` → atomically
  updates `task.json` → emits notifications. A second `transit` in the same
  session throws.
- **`newMCPServer`** mints a per-session, host-side HTTP MCP server bound to this
  session's `{ projectRoot, taskId, author = step, transit = task-mode }` and
  returns `{ url, port, token, close() }` — a sandboxed harness's tool surface
  (see [Isolation](#isolation-agent-owned-sandboxes)). `close()` is an explicit
  early release; the engine also closes it on every settle, so no port leaks
  across steps.
- **`log`** writes the pi-format transcript: `log.message(...)` for a pi message
  entry, `log.custom(type, data)` for the opaque agent logging channel, and
  `log.usage(usage)` for authoritative usage from one completed provider turn.
  Pass `null` when a completed turn has no trustworthy usage. The engine writes
  each report as `autosk:usage` and aggregates complete reports into terminal
  `SessionMeta.usage`; one unavailable report makes the aggregate unavailable.
- **`partial`** streams an in-progress assistant message snapshot to live
  subscribers. It is **ephemeral**: never written to the transcript, carries no
  line, never advances the line cursor, and is superseded by the next committed
  `log.message`. Send the full **cumulative** snapshot each time — the client
  just replaces its current partial. It rides the same per-session subscription
  as committed lines; see [docs/daemon.md → Streaming partial
  messages](daemon.md#streaming-partial-messages) for the wire frame and the
  ordering/persistence guarantees. (`@autosk/pi-agent` drives this from pi's
  `message_update` events.)
- **`exec`** / **`spawn`** run child processes; `spawn` is how the pi-agent
  extension drives `pi --mode rpc` over JSON-lines stdio.
- **`cwd` vs `projectRoot`:** both are **always the project root** now — isolation
  no longer rewrites `cwd`. An agent that wants a different run dir (e.g. a git
  worktree) owns it: it resolves a [sandbox](#isolation-agent-owned-sandboxes)
  workspace and spawns its harness there. `@autosk/pi-agent` passes `projectRoot`
  to the spawned pi as `AUTOSK_CWD`, which the `autosk` CLI honors as its project
  selector, so any `autosk` call the agent makes targets the task's own project.
- **`onSteer`** / **`onFollowup`** receive a `session.input` message on a live
  session; **`onAbort`** runs on `session.abort`. All are optional.

### Sessions

One `onRun` for one task step = one **session** (`./.autosk/sessions/<id>.json`
meta + `<id>.jsonl` transcript). Sessions replace v1's jobs; see
[docs/daemon.md → Sessions & transcripts](daemon.md#sessions--transcripts) for
the on-disk format and the steer/abort surface.

## Named agents & interactive sessions

A workflow registers its agents **inline** (the step key is the agent name). An
extension can also publish a **named, standalone agent** so a user can chat with
it directly, outside any workflow:

```ts
import { type AutoskAPI } from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";

export default function (autosk: AutoskAPI) {
  autosk.registerAgent({
    name: "pi",
    description: "Interactive chat backed by `pi --mode rpc`.",
    agent: piAgent(),          // an AgentDefinition, run with its default options
  });
}
```

A registered agent backs an **interactive (taskless) session** — a chat the user
opens directly (no task, no workflow). The engine runs the same
`AgentDefinition.onRun`, but with `ctx.mode === "interactive"`:

- `ctx.transit(...)` is **unavailable** (it throws) and `ctx.tasks` /
  `ctx.workflows` are stub views — an interactive agent must not touch them.
- The agent runs a chat loop and **returns when `ctx.signal` fires** (rather than
  transiting). Returning is normal: a graceful end seals the session `done`, an
  abort seals it `aborted`, a crash seals it `failed` — **none park a task**
  (there is none). The `agent_did_not_transit` failure does not apply.

`@autosk/pi-agent` registers the `"pi"` agent and branches `onRun` on `ctx.mode`:
`"task"` runs the workflow transit loop; `"interactive"` runs a chat loop that
spawns `pi --mode rpc` **without** the `autosk_transit` tool (transit is not
offered in chat) and forwards each composer message as a follow-up.
[`@autosk/claude-agent`](../daemon/extensions/claude-agent/README.md) registers a
`"@autosk/claude-agent"` agent the same way (its interactive chat drops the
`mcp__autosk__transit` tool but keeps `task` / `comment`).

See [docs/daemon.md → Interactive sessions](daemon.md#interactive-taskless-sessions)
for the session lifecycle and the `registry.agent.list` / `session.create` /
`session.end` RPC surface.

## Isolation: agent-owned sandboxes

Isolation is **not** an engine or SDK concept. There is no `IsolationProvider`
contract and no `WorkflowDefinition.isolation` field: an agent owns whatever
isolation it needs by wrapping its harness with a **sandbox**, and teardown is a
normal workflow step. The building blocks live in the userspace
[`@autosk/sandbox`](../daemon/extensions/sandbox/README.md) package
(`worktreeSandbox()` / `dockerSandbox()` / `sandboxCleanupStep()`); it absorbed
the retired `@autosk/worktree` and `@autosk/docker` provider packages.

### The `Sandbox` shape (structural)

A `Sandbox` is a plain object a workflow author passes to an agent step
(`piAgent({ sandbox, … })` / `claudeAgent({ sandbox, … })`). The shape is
**structural** — agents accept any object with these methods, so a hand-rolled
sandbox needs no dependency on `@autosk/sandbox`:

```ts
type Sandbox = {
  workspace(id): Promise<{ cwd: string }>;          // the per-task dir the harness runs in (idempotent + deterministic)
  wrap(cmd, { cwd, env, id, roFiles }): string[];   // wrap the harness argv (docker run …); identity for host/worktree
  endpointFor(port): string;                        // host endpoint an in-sandbox process reaches a host port at
  stop(id): Promise<void>;                          // best-effort stop of the running harness (agent onAbort)
  cleanup(id, { force }): Promise<{ removed; dirty; detail? }>;  // terminal teardown (the cleanup step)
  thin?: boolean;                                   // true ⇒ no autosk/host FS in the sandbox; the agent uses the http MCP tool surface
};
type TaskIdentity = { projectRoot: string; taskId: string };
```

In `onRun` (task mode) an agent: resolves `workspace(id)` for its run dir, mints
a per-session [`ctx.newMCPServer()`](#the-host-side-mcp-server-ctxnewmcpserver) for its tool surface,
rewrites the server host with `endpointFor(mcp.port)`, `wrap()`s the harness
argv, and spawns at the workspace cwd. `onAbort` calls `sandbox.stop(id)`. With
**no** sandbox, the harness runs on the host at `ctx.cwd` (= the project root).

### The host-side MCP server (`ctx.newMCPServer()`)

The reachability problem — *how does a harness running inside a container reach
the daemon?* — is owned by the agent, not an engine seam. `ctx.newMCPServer()`
mints a **per-session, host-side HTTP MCP server** (a hand-rolled `Bun.serve()`
POST→JSON endpoint, ephemeral port, no `@modelcontextprotocol/sdk`) bound to this
session's `{ projectRoot, taskId, author = step, transit = task-mode }` and
returns `{ url, port, token, close() }`:

- the agent builds the harness' tool surface against `endpointFor(port)` +
  `Authorization: Bearer <token>` — for a worktree sandbox that is
  `http://127.0.0.1:<port>`, for a docker sandbox `http://host.docker.internal:<port>`;
- the server binds an ephemeral port (`0.0.0.0` so a Linux container reaches it
  over the docker bridge); the **bearer token + ephemeral port are the security
  boundary** (a wrong/absent bearer is `401`);
- `close()` is an explicit early release. The engine also closes the server on
  **every** settle / finaliser / detach regardless, so a forgetful agent never
  leaks a port across steps.

`@autosk/claude-agent` passes this to Claude Code as an `--mcp-config`
`type:"http"` server; `@autosk/pi-agent`, under a `thin` sandbox, injects it
(`AUTOSK_MCP_URL` / `AUTOSK_MCP_TOKEN`) into a small in-repo pi-extension that
POSTs transit/task/comment to it — so a **thin** container image needs neither
`autosk` nor a mounted daemon socket. (Off-sandbox pi keeps its existing
`@autosk/pi-tools` `autosk` shell-out path.)

### `worktreeSandbox()` — a per-task git worktree

[`worktreeSandbox()`](../daemon/extensions/sandbox/README.md) runs each task in
its own git worktree at `~/.autosk/worktrees/<slug>/<task-id>` on branch
`autosk/<task-id>`. `workspace()` creates (or reuses) the checkout, `wrap()` is
the identity (the harness runs on the host), `endpointFor()` returns
`http://127.0.0.1:<port>`, and `cleanup()` removes the worktree dir while
**preserving the branch** (so the work survives for review/merge). It is **not**
`thin` — the harness runs on the host with `autosk` available. The project root
must therefore be a git repo.

### `dockerSandbox({ image })` — a per-task container

[`dockerSandbox({ image })`](../daemon/extensions/sandbox/README.md) runs the
harness **inside a per-task `docker run -i --rm` container**. It composes
`worktreeSandbox()` for the filesystem (the worktree is bind-mounted 1:1, so
edits still land on `autosk/<task-id>` on the host) and `wrap()`s the argv into

```text
docker run -i --rm --name <det> --add-host=host.docker.internal:host-gateway \
  -v <ws>:<ws> -w <ws> -e … <image> <cmd…>
```

A container lives exactly as long as **one** harness process (`--rm`
self-removes on exit); there is no keep-alive container, no `docker exec`, and no
start/stop/reuse state machine. `endpointFor()` returns
`http://host.docker.internal:<port>` so the in-container harness reaches the
host MCP server; `stop()` is `docker stop <name>` (covers a SIGKILL orphan);
`cleanup()` removes the worktree plus a defensive `docker rm -f <name>`. The
image is **thin**: just the harness (`claude`/`pi`, authenticated) + the
build/test toolchain — **no** `socat`, `autosk`/`autoskd`, or mounted socket.
Inject credentials/caches with `mounts`, align ownership with `user`, and set
the container `home`; see the
[package README](../daemon/extensions/sandbox/README.md) for the full image
contract (UID/GID, HOME, Claude auth via `env: { ANTHROPIC_API_KEY }` or a
mounted `~/.claude/.credentials.json:ro`). An optional `mountSocket` escape
hatch mounts the daemon socket for an in-container `autosk` (the pi fallback).

### Cleanup is a workflow step

Because `done`/`cancel` are now a **raw status flip** with no engine teardown, a
workflow that allocates a sandbox MUST route its terminals through a cleanup
step or it leaks the worktree on every task.
[`sandboxCleanupStep(sandbox, { to?, force? })`](../daemon/extensions/sandbox/README.md)
builds an ordinary agent step whose `onRun` tears the env down
(`sandbox.cleanup(...)` — worktree dir removed, branch preserved; defensive
container rm), comments the outcome, and transits to its target (default
`{ status: "done" }`). It runs at the project root (so it never sits inside the
dir it removes) and is idempotent on a missing env:

```ts
steps: {
  dev:     piAgent({ sandbox, firstMessageFile: ".../dev.md" }),
  accept:  statusStep("human"),
  cleanup: sandboxCleanupStep(sandbox),     // accept → cleanup → done
},
```

A workflow author wires it into the graph wherever teardown should happen; a
user can also route a parked task into it on demand
(`autosk resume <id> --to cleanup`). A bare `autosk done`/`cancel` skips it and
leaks the worktree (an accepted trade-off — the branch is always preserved, and
no dirty-gate blocks a terminal flip).

## Driving a workflow from the CLI

```bash
# Create, optionally enrolling at the same time.
autosk create "Wire up the auth flow"
autosk create "Fix the flaky test" --workflow feature-dev   # create + enroll

# Enroll an existing task.
autosk enroll <id> --workflow feature-dev   # at the workflow's firstStep

# Resume a parked (human) task back into its workflow.
autosk resume <id>                 # at the current step
autosk resume <id> --to review     # at a chosen step (or done|cancel|human)

# Human status overrides (administrative — do NOT run onTransit).
autosk done <id>
autosk cancel <id>
autosk reopen <id>
```

`enroll` (and `create --workflow`) takes a `--workflow` target only — a task is
always enrolled into a named workflow at its `firstStep`.

### Read-only registry views

Workflows come from code, so the CLI only *shows* them:

```bash
autosk workflow list          # workflows registered by this project's extensions
autosk workflow show <name>   # steps (KIND: agent | done|cancel|human), targets
```

`workflow show` renders each step with a `KIND` column: `agent` for an inline
agent step (its `name` is the agent name), or the terminal/park status
(`done` / `cancel` / `human`) for a `statusStep`.

If a workflow you expect is missing, check `autosk project diagnostics` — a
broken extension (or an invalid step shape) records a load error there instead
of crashing the daemon (see [docs/extensions.md](extensions.md#error-isolation--projectdiagnostics)).

## The reference workflow: `feature-dev`

[`@autosk/feature-dev`](../daemon/extensions/feature-dev/README.md) is an npm
package the daemon installs on first run (see
[docs/extensions.md → First-run bootstrap](extensions.md#first-run-bootstrap)),
so every project can enroll into it with no per-project files:

```text
dev ──▶ review ──▶ docs ──▶ validator ──▶ accept (human) ──▶ cleanup ──▶ done
 ▲        │                    │
 └────────┴────────────────────┘   (review→dev and validator→dev bounce-backs)
```

| Step | Kind | Notes |
| --- | --- | --- |
| `dev` | agent (`piAgent`) | implements the task (first step) |
| `review` | agent (`piAgent`) | thorough review; can bounce back to `dev` |
| `docs` | agent (`piAgent`) | documentation pass |
| `validator` | agent (`piAgent`) | independent verification; can bounce back to `dev` |
| `accept` | `statusStep("human")` | the engine parks here for final acceptance |
| `cleanup` | agent (`sandboxCleanupStep`) | tears the worktree down (branch preserved), then transits to `done` |

- **Sandbox:** each agent step runs in a per-task `worktreeSandbox()` (its own
  git worktree on branch `autosk/<task-id>`), so the project root must be a git
  repo. The `cleanup` step removes the worktree on the way to `done` — the human
  resumes an accepted task into it (`autosk resume <id> --to cleanup`); routing
  every terminal through it is what keeps a task from leaking its worktree now
  that `done`/`cancel` are a raw status flip. A `feature-dev-cc` sibling drives
  Claude Code instead of pi, and `@autosk/feature-dev-docker` swaps in
  `dockerSandbox({ image })` to run the pi harness in a per-task container.
- **Visit cap:** `onTransit` rejects a bounce-back into `dev` once the task has
  entered `dev` 5 times (via `ctx.visits("dev")`), so a task that keeps failing
  review/validation parks for a human instead of looping forever. The count is
  the persistent, human-resettable [`metadata.step_visits.dev`](daemon.md#task-metadata)
  — clear it with `autosk metadata unset <id> step_visits` to let a parked task
  resume bouncing through `dev`.

```bash
id=$(autosk create "Fix the flaky test" --workflow feature-dev --json | jq -r .id)
# the daemon (auto-spawned) picks it up, runs dev → review → …, and either
# parks it at `accept` for you or bounces it back to `dev`.
```

### Customising it

Copy the extension into `~/.autosk/extensions/` (or your project's
`.autosk/extensions/`) and edit the `piAgent({...})` / `featureDevWorkflow({...})`
calls (model, thinking, the visit cap, the step graph, the prompts under
`prompts/`). Because a project/global extension overrides an npm one by name,
your `feature-dev` then replaces the provisioned one. See
[docs/extensions.md → Writing your own](extensions.md#writing-your-own) for the
discovery/override rules.

## Other shipped workflows

`feature-dev` is one of several workflows that ship as `@autosk/*` packages.
Three more are **opt-in** (add them with
[`autosk ext add`](extensions.md#managing-extensions)):

- **`@autosk/feature-dev-cc`** — the Claude Code twin of `feature-dev` (the same
  graph driven by [`claudeAgent`](#agent-definitions) roles).
- **`@autosk/feature-dev-docker`** — `feature-dev` with each agent step in a
  per-task [`dockerSandbox`](#dockersandbox-image--a-per-task-container) container.
- **`@autosk/merge-to-current`** — a single non-isolated `merge` step that
  integrates a task's `autosk/<task-id>` branch into the branch you currently
  have checked out (`merge → done | human`), with full rollback on failure.
- **`@autosk/gh-review`** — reviews a GitHub issue/PR (URL in the task title) in
  a per-task `dockerSandbox` container with a READ-ONLY `gh`
  (`review → accept (human) → cleanup → done`).

See **[docs/shipped.md](shipped.md)** for the full catalog — graphs, step
tables, config knobs, install/enroll how-tos, and an end-to-end tutorial — and
for the two shipped agents ([`@autosk/pi-agent`](shipped.md#autoskpi-agent) /
[`@autosk/claude-agent`](shipped.md#autoskclaude-agent)).

## Make your own workflow

> Prefer a guided walkthrough? The
> [extension tutorial](extensions-tutorial.md) builds a runnable workflow from
> an empty directory, then shows the discover → run → break → recover loop; the
> [Claude Code workflow tutorial](extensions-tutorial-claude.md) wires a real
> `claudeAgent` into a per-task worktree end to end.

A minimal two-step flow with a human gate:

```ts
// ~/.autosk/extensions/my-flow.ts
import { statusStep, type AutoskAPI } from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";
import { sandboxCleanupStep, worktreeSandbox } from "@autosk/sandbox";

export default function (autosk: AutoskAPI) {
  const sandbox = worktreeSandbox();   // a per-task git worktree the agent runs in
  autosk.registerWorkflow({
    name: "my-flow",
    firstStep: "dev",
    steps: {
      // The step key is the agent name; registering the workflow registers
      // its inline agents — there is no separate registerAgent call.
      dev: piAgent({
        sandbox,
        model: "sonnet:high",
        firstMessageFile: new URL("./prompts/dev.md", import.meta.url).pathname,
      }),
      accept: statusStep("human"),
      // Route terminals through cleanup so the worktree never leaks.
      cleanup: sandboxCleanupStep(sandbox),
    },
    onTransit(ctx, to) {
      // gate, count, or comment here; throw to reject a transition.
    },
  });
}
```

```bash
autosk workflow list                 # my-flow should appear
autosk enroll <task-id> --workflow my-flow
```

For the full extension lifecycle (discovery order, overrides, diagnostics, the
no-trust loading model), see [docs/extensions.md](extensions.md).
