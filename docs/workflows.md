# Workflows & agents

autosk turns the task tracker into a small workflow engine. Tasks
move through a directed graph of **steps**; each step is owned by an
**agent** (a human or a background pi instance). The daemon picks up
tasks in `work` status, spawns the step's agent, and advances the
task when the agent emits a structural transition signal.

---

## Concepts

### Agent

A named actor that can own a task. Stored in the `agents` table with
the minimal shape `(id, name, is_human)`. `human` is seeded on first
migrate.

All other agents come from **npm packages** installed into the global
autosk packages prefix (`~/.autosk/packages/` by default; override with
`$AUTOSK_PACKAGES`). The agent's name in `agents.name` is the **full
npm package name** — there's no aliasing.

Install one and start using it:

```bash
autosk agent install @autosk/developer
autosk agent install @autosk/code-reviewer
autosk agent install @autosk/task-validator
```

Workflows reference agents by their full npm package name via the
per-step `agent` object: `{ "name": "@autosk/developer" }`. The
`human` name is the only one that doesn't need to come from a package.
Per-step overrides for the package's defaults live under
`agent.params` (see [Per-step agent overrides](#per-step-agent-overrides) below).

The agent's **executable configuration** (model, thinking depth, the
text prepended to the first user turn, extra pi args, custom runner)
lives inside the package's `package.json`:

```jsonc
{
  "name":    "@autosk/developer",
  "version": "0.3.1",
  "type":    "module",
  "autosk": {
    "agent": {
      "model":              "sonnet:high",
      "thinking":           "high",
      "first_message_file": "./prompts/first_message.md",
      "extra_args":         ["--no-tool", "web_fetch"],
      "pi_extensions":      ["./extensions/foo.ts"],
      "pi_skills":          ["./skills"]
    }
  }
}
```

> The `first_message`(_file) text is **not** a system-role prompt. pi
> has its own system prompt; this field is the leading block of the
> first user turn the executor sends. Earlier drafts called this
> `system_prompt_file` — the name was misleading and was renamed.

The daemon resolves this configuration at spawn time. The `human` agent
has no package — the workflow engine never tries to spawn it.

#### Two flavours of agent

A package can be one of:

- **Standard** — declares `model`, `thinking`, `first_message`(_file),
  `extra_args`, `pi_extensions`, `pi_skills`. The daemon spawns `pi
  --mode rpc` with these settings; the agent is just pi configured
  through the package.

- **Custom** — declares an `autosk.agent.runner` pointing at a TS/JS
  module. The daemon spawns the Node bootstrapper
  (`@autosk/agent-runtime`), which imports the runner module and awaits
  its default export. The runner is free to do anything (call any
  LLM, run a deterministic shell pipeline, delegate back into pi via
  `ctx.spawnPi(...)`); it just needs to call `ctx.stepNext(...)` once
  before returning.

  Minimal custom runner:

  ```ts
  // my-agent/src/agent.ts
  import type { RunAgent } from "@autosk/agent-sdk";

  const run: RunAgent = async (ctx) => {
    await ctx.cli(["comment", "add", ctx.task.id, "starting"]);
    // ... do work ...
    await ctx.stepNext("done");
  };
  export default run;
  ```

  Custom runners are **single-shot**: the bootstrapper's process runs
  once and the executor checks `step_signals` after it exits. There is
  no kickback loop for the custom branch — a missed signal fails the
  run with `agent_did_not_emit_transition` immediately.

  See [`@autosk/agent-sdk`](../extension/sdk/README.md) for the full
  contract and [`@autosk/agent-runtime`](../extension/runtime/README.md)
  for the bootstrapper.

#### Manage installed agents

```bash
autosk agent list                       # union of installed pkgs + DB rows
autosk agent show  @autosk/developer    # registry + DB view
autosk agent uninstall @autosk/developer
autosk agent runtime install            # eagerly install @autosk/agent-runtime
```

The `human` agent always shows up with `source=builtin`. Packages whose
row is in the project DB but no matching install exists locally are
flagged `source=db_only` so the diagnostic surface is obvious.

Plan: [`docs/plans/20260518-Agent-Packages.md`](plans/20260518-Agent-Packages.md).

> **Security:** the spawned agent inherits the daemon's environment.
> Never put API keys in `package.json`. pi reads credentials from env;
> custom runners should do the same.

### Workflow

A workflow is a small directed graph of steps. Define it as a JSON file
and import it, or install a packaged workflow bundle:

```bash
autosk workflow create --file docs/examples/workflows/workflow-example.json
autosk workflow install @your-org/workflows --workflow feature-dev
```

#### Canonical definitions and global registry foundation

Workflow definitions are canonicalized before they are hashed or stored
outside a project DB. Canonical JSON normalizes defaults (including
`isolation: "none"`), sorts step names, preserves authored transition
order, and treats empty `agent.params` arrays as absent. The canonical
SHA-256 digest is the stable `definition_hash` used by registry and
provenance records.

Global workflow definitions live outside any one project under a
workflow prefix resolved in this order:

1. `$AUTOSK_WORKFLOWS`
2. `$XDG_DATA_HOME/autosk/workflows`
3. `$HOME/.autosk/workflows`

The prefix contains a `registry.json` file plus canonical definition
files under `definitions/<definition_hash>.json`. Registry entries keep
the workflow name, definition hash/file, optional source and revision
metadata, enabled state, and install/update timestamps. Loading a
registry entry validates that the stored file's workflow name and
canonical hash still match `registry.json`, so stale or tampered files
fail loudly instead of silently substituting another workflow.

Project databases also keep a `workflow_origins` row for imported or
seeded workflows. The row records source type/path, source metadata,
canonical definition hash, revision, active state, and timestamps; it is
deleted automatically when the workflow row is deleted. This is the
foundation for global workflow reuse and provenance tracking. Use
`autosk workflow sync` to materialize enabled global registry entries
into a project.

#### Sync global workflows into a project

`autosk workflow sync` reads the enabled entries in the global workflow
registry and materializes them into the current project's `.autosk/db`:

```bash
autosk workflow sync
autosk workflow sync --dry-run
autosk workflow sync --force --json
```

The sync report includes one item per enabled global workflow. Text mode
prints `workflow <name>: <status>`; `--json` emits `prefix`, `dry_run`,
`force`, and a `workflows[]` array with `name`, `status`, hashes,
revision, reason/error text, and any `auto_installed_agents`. Statuses
are:

| Status | Meaning |
|---|---|
| `added` | The enabled global workflow was absent locally and was/would be created with a global origin row. |
| `noop` | The local global-managed workflow already matches the registry definition hash. |
| `skipped` | The workflow is intentionally not changed, for example the local origin is inactive or the global hash changed but `--force` was not passed. |
| `conflict` | A same-name local workflow exists but is not managed by the matching global origin; sync never overwrites it. |
| `updated` | With `--force`, a changed global-managed workflow was/would be replaced when the store's safety checks allow it. |
| `error` | The definition could not be loaded, validated, installed, or materialized. |

Referenced scoped npm agents are auto-installed through the same package
path as `autosk workflow install`. Dry-run mode validates and previews
without installing agents or writing the DB. Non-dry-run sync records a
single doltlite commit, `workflow sync global`, when any workflow or
agent rows were changed. `--force` only replaces workflows already
managed by the matching active global origin; local same-name conflicts
and workflows currently in use are reported instead of overwritten.

#### Global workflow registry CLI

The `autosk workflow global` command tree manages the global workflow registry outside any single project.

**File-backed workflows:**

```bash
autosk workflow global add my-flow.json
autosk workflow global list [--all]
autosk workflow global show my-flow
autosk workflow global remove my-flow [--force]
autosk workflow global enable my-flow
autosk workflow global disable my-flow
autosk workflow global sync [--force] [--dry-run]
autosk workflow global adopt my-flow [--dry-run]
```

**Package-backed workflows:**

```bash
autosk workflow global install @your-org/workflows [--workflow NAME] [--version VER] [--no-install] [--dry-run]
autosk workflow global update my-flow [--version VER] [--no-install] [--dry-run] [--force]
```

**Common flags:**

| Flag | Applies to | Meaning |
|---|---|---|
| `--workflow` | `install` | Select one workflow when the package declares multiple. |
| `--version` | `install`, `update` | Target npm version spec (default: latest). Cannot be combined with `--no-install`. |
| `--no-install` | `install`, `update` | Use the already-installed package only; do not shell npm. |
| `--dry-run` | `add`, `adopt`, `install`, `update`, `sync` | Preview without writing the registry or installing packages. |
| `--force` | `remove`, `update`, `sync` | Remove an enabled workflow, update even when the hash matches, or replace changed global-managed workflows. |
| `--json` | all | Emit structured JSON to stdout instead of human text. |
| `--quiet` | all | Suppress all non-error output. |

**Dry-run and JSON behaviour**

Every `--dry-run` path emits a structured preview and performs no mutations:
- It does not create `$AUTOSK_WORKFLOWS` or `$AUTOSK_PACKAGES` directories.
- It does not shell npm or modify `registry.json`.
- For remote packages that are already installed, `--dry-run` without `--version` returns a note instead of hashing potentially stale files.
- For remote packages with an explicit `--version` that differs from the installed copy, `--dry-run` returns a note instead of installing the requested version.

When `--dry-run --json` is used together, the output is a JSON object with at least `dry_run: true`, `action`, `name` or `package_name`, and `would_mutate`. The `would_mutate` field indicates whether the equivalent non-dry-run command would change state (`true` when it would, `false` for a no-op).

**Examples:**

```bash
# Register a local workflow JSON in the global registry
autosk workflow global add ./my-workflow.json

# Install a packaged workflow globally
autosk workflow global install @autosk/workflows --workflow bug-fix --json

# Preview an update without changing anything
autosk workflow global update bug-fix --dry-run

# Materialize all enabled globals into the current project
autosk workflow global sync --force --dry-run --json
```

#### Managed revisions

Managed workflows are workflows with active provenance that autosk owns,
such as the shipped bootstrap workflow and future global-registry
syncs. Re-applying the same canonical definition hash is a no-op except
for refreshing provenance metadata. When the managed definition hash
changes, autosk keeps the canonical workflow name attached to the new
active definition while protecting existing tasks and daemon runs:

- If the old managed row is unreferenced, autosk deletes it and creates
  a fresh active row with the canonical name.
- If tasks, daemon runs, or older revision links still reference the old
  row, autosk renames it to `<name>@rev-<workflow-id-suffix>`, marks its
  `workflow_origins` row inactive, creates a new active row with the
  canonical name, and links the old row to the new one via
  `superseded_by_id`.

Existing tasks and runs continue to point at the old row's steps; new
enrollments that use the canonical name resolve to the new active row.
The `@rev-` marker is reserved for these internal revision names, so
user-authored workflow names and global-registry entries may not contain
it. A same-name workflow with no provenance is treated as user-owned and
is left untouched.

#### Shipped default: `feature-dev-generic`

`autosk init` seeds one workflow into every new project so users can
start enrolling tasks without first authoring or copying a workflow
JSON. The seeded workflow is named `feature-dev-generic` and has the
shape:

```
dev → review → docs → validator → human
```

with bounce-back edges from `review → dev` and `validator → dev`.
Every step is owned by the same agent, `@autogent/generic`, which
`autosk init` also auto-installs through the same path used by
`autosk workflow create --file ...`.

The canonical definition lives at
[`internal/bootstrap/feature-dev-generic.json`](../internal/bootstrap/feature-dev-generic.json).
It is embedded into the `autosk` binary via `go:embed`, parsed through
the same `workflow.ParseReader` path as user-authored JSON, and applied
by `autosk init`. To reuse it as a template, copy that file and pass it
to `autosk workflow create --file ...` under a different name — do not
edit the embedded file in place if you only need a local variant.

The shipped definition carries `isolation: "worktree"`, so every task
enrolled into `feature-dev-generic` on a freshly-initialised project
allocates its own per-task git worktree under
`~/.autosk/worktrees/<slug>/<task-id>` on branch `autosk/<task-id>`.
This means `autosk create … --workflow feature-dev-generic` requires
the project root to be a git repository: a non-git root surfaces
`ErrNotGitRepo` from the worktree allocator. See
[Worktree isolation](#worktree-isolation) for the full mechanics.

Bootstrap is idempotent for unmanaged rows and unchanged managed
rows: re-running `autosk init` skips a same-name workflow with no
provenance, and it skips a managed `feature-dev-generic` row whose
canonical hash already matches the embedded definition. When the
embedded definition hash changes, `autosk init` applies the
[managed revision](#managed-revisions) rules above so existing tasks and
runs keep their old steps while new enrollments use the current shipped
workflow. Projects seeded before provenance support are treated as
unmanaged and are not auto-migrated; opt into individual settings such
as isolation explicitly with [`autosk workflow update`](#updating-isolation),
or delete and re-run `autosk init` if you want the current shipped
definition.

If you have forked the seeded workflow locally and want to avoid future
managed revisions, keep the fork under a different workflow name. If you
want the shipped default back, `autosk workflow delete
feature-dev-generic` followed by `autosk init` re-creates it with the
current embedded definition.

`autosk init` also syncs enabled global workflows after bootstrap (see
[`autosk workflow sync`](#sync-global-workflows-into-a-project)).
Pass `--skip-global-workflows` to opt out of that step.

If you want a minimal database with no workflows seeded, pass both
flags:

```bash
autosk init --skip-bootstrap --skip-global-workflows   # no workflows, no agents
```

Using `--skip-bootstrap` alone skips only the built-in seed; enabled
global workflows are still materialized unless `--skip-global-workflows`
is also set.

Bootstrap and global-workflow-sync failures (network down, npm registry
5xx, read-only packages prefix, missing agent, ...) are non-fatal:
`autosk init` warns on stderr, exits 0, and leaves a valid migrated DB
behind.

##### Implicit auto-init from other verbs

The write-side verbs (`autosk create`, `autosk lazy`, `autosk enroll`,
etc.) also bootstrap the default workflow the first time they reach a
directory tree that has no `.autosk/db`. The exact behaviour depends on
whether stdin/stderr are attached to a real terminal:

- **Interactive (TTY).** Before creating anything the CLI prompts on
  stderr:

  ```
  No autosk database found at or above /path/to/cwd.
  Create a new autosk database in /path/to/cwd/.autosk/db? [Y/n]
  ```

  Empty answer or `y`/`yes` accepts. `n`/`no` aborts the verb with an
  error pointing at `autosk init`, `--db`, and `AUTOSK_DB`. After an
  accepted prompt the bootstrap sequence is identical to
  `autosk init`: migrations, then the managed bootstrap decision for
  `feature-dev-generic` (including installing `@autogent/generic` only
  when a create or revision sync is needed), then syncing enabled global
  workflows into the project.

- **Non-interactive (no TTY, piped stdin, `--json`, `--quiet`).** The
  prompt is skipped and the CLI behaves as if the user answered `y`.
  This preserves the original "write verbs auto-init" contract for
  scripts, CI, the daemon executor, and editor terminals that
  intentionally hide the TTY.

Four environment knobs control the implicit path without authoring a
workflow JSON yourself:

| Variable | Effect |
| -------- | ------ |
| `AUTOSK_NO_AUTOINIT=1` | Refuse auto-init entirely; the verb errors out with `auto-init disabled by AUTOSK_NO_AUTOINIT`. Intended for tests and read-only environments. |
| `AUTOSK_AUTOINIT_ASSUME_YES=1` | Skip the interactive prompt even under a TTY and proceed as if the user said `y`. |
| `AUTOSK_AUTOINIT_SKIP_BOOTSTRAP=1` | Create `.autosk/db` (with the y/n prompt path unchanged) but skip the built-in workflow-seed step. Does not affect explicit `autosk init` — use its `--skip-bootstrap` flag for that. |
| `AUTOSK_AUTOINIT_SKIP_GLOBAL_WORKFLOWS=1` | Skip syncing enabled global workflows during auto-init. Does not affect explicit `autosk init` — use its `--skip-global-workflows` flag for that. |

The shipped example:

```jsonc
{
  "name": "feature-dev",
  "description": "Implement, review, validate, then ask the human.",
  "first_step": "dev",
  "steps": {
    "dev":       { "agent": { "name": "developer" },     "max_visits": 5, "next_steps": [{"step":"review", "prompt_rule":"…"}] },
    "review":    { "agent": { "name": "code-reviewer" }, "max_visits": 5, "next_steps": [
                     {"step":"validator", "prompt_rule":"…"},
                     {"step":"dev",       "prompt_rule":"…"}
                   ]},
    "validator": { "agent": { "name": "task-validator" },"next_steps": [
                     {"step":"dev",                  "prompt_rule":"…"},
                     {"task_status":"human",         "prompt_rule":"…"}
                   ]}
  }
}
```

The `agent` field is always an object. The bare-string form
(`"agent": "developer"`) used in earlier drafts is no longer
accepted; the parser rejects it with a hint.

A `next_steps[]` entry has either a `step` (sibling step name in the
same workflow) or a `task_status` (one of `done`, `cancel`,
`human`), never both. `prompt_rule` is natural-language guidance
shown to the agent in its prompt — the agent uses it to decide which
transition to emit.

### Visit limits (`max_visits`)

A workflow that loops (`dev → review → dev → …`) can churn indefinitely
if its agents keep bouncing the task back. Each step block accepts an
optional `max_visits: N` cap so the engine can fail loudly and escalate
to a human instead of burning tokens forever.

```jsonc
"review": {
  "agent":      { "name": "@autosk/code-reviewer" },
  "max_visits": 5,
  "next_steps": [ /* … */ ]
}
```

Semantics:

- **What counts as a visit.** Every transition INTO the step bumps the
  counter — including the very first entry when a task is created
  (`autosk create … --workflow X`) or enrolled (`autosk enroll …
  --workflow X`) at this step. The cap is checked *before* the
  counter is bumped, so the Nth visit is allowed and the (N+1)th
  fails.
- **`autosk resume <id>` without `--to` does NOT count.** It just flips
  `human → work` and the same step keeps its current
  counter — there was no transition. `autosk resume <id> --to STEP`
  DOES count, even when `STEP` is the step the task currently sits on:
  that's a deliberate re-entry and is treated as one.
- **`max_visits: 0` or absent → unlimited.** The counter still ticks,
  but the engine never compares it to anything. Synthetic
  `single:<agent>` workflows always have `max_visits = 0`.
- **What happens when the cap fires.** The current run is failed with
  `daemon_runs.status = 'failed'` and
  `daemon_runs.error = 'step_max_visits_exceeded: step "…" reached
  visits=N max=N'`. The task is parked to `status = 'human'`
  via the same `parkTaskOnFailure` path used by any other terminal
  failure. **`current_step_id` moves to the TARGET step** — the one
  the run was trying to enter when the cap fired (e.g. on a
  `dev → review` advance that breaches `review.max_visits`, the task
  parks on `review`, not on `dev`). After resetting the counter,
  `autosk resume <id>` (no `--to`) lands on the same target step so
  the loop can continue from there. This rule is specific to
  `advanceTask` failures — every other failure mode (pi crash,
  runner timeout, SendPrompt error, shutdown error,
  `agent_did_not_emit_transition`) leaves `current_step_id`
  untouched on the source step, because the run died before the
  target was known.
- **Counters live in `tasks.metadata.step_visits`** as a JSON object
  keyed by `step_id` (the engine-managed schema; see
  [Task metadata](#task-metadata) below).
- **Resets are humans-only.** Nothing auto-clears counters — not
  `reopen`, not `enroll`, not the daemon. Use
  `autosk metadata reset-visits <id>` (optionally with
  `--step NAME` or `--step-id ID`) as the escape hatch.
- **Validation.** Negative `max_visits` is rejected at workflow import
  time with `step %q: max_visits must be >= 0`.

Typical cap-fire recovery:

```bash
# Task is parked because review hit max_visits = 5.
autosk show ask-bea935
# status: human
# current_step: review   <-- TARGET of the failed advance (the
#                            cap fired on the dev→review transition,
#                            so the task parks on review and `resume`
#                            without --to resumes there once the
#                            counter is cleared)
# visits: dev 4/5, review 5/5*

autosk daemon messages <job-id>
# … error: step_max_visits_exceeded: step "review" reached visits=5 max=5

# Inspect, decide, reset the counter you want to clear:
autosk metadata reset-visits ask-bea935 --step review
# … and continue the loop:
autosk resume ask-bea935
```

### Worktree isolation

By default every step of every task on a project runs in the project
root. Two tasks of the same workflow running concurrently will race on
the filesystem. To give each task its own sandbox, mark the workflow as
isolated:

```jsonc
{
  "name":       "feature-dev",
  "first_step": "dev",
  "isolation": "worktree",        // NEW; optional, default "none"
  "steps":     { ... }
}
```

Accepted values: `"none"` (the default — preserves previous behaviour)
and `"worktree"`. Anything else is rejected at `workflow create` time
with `unknown isolation mode: %q (want none|worktree)`. The mode is
editable after create via
[`autosk workflow update`](#updating-isolation) (with safety
guards around in-flight tasks). Synthetic `single:<agent>` workflows
(created implicitly by `--agent NAME`) are pinned to `"none"` by the
engine and cannot be flipped at any point — not at create time and
not by `workflow update`.

When a task targets a workflow with `isolation: "worktree"`:

- `autosk create` and `autosk enroll` allocate a per-task git worktree
  **before** the task enters the workflow. Path:
  `~/.autosk/worktrees/<basename>-<8hex(sha256(canonRoot))>/<task-id>`.
  Branch: `autosk/<task-id>`. Both are **derived deterministically**
  from the canonical project root + task id — no row in any table
  stores a copy.
- The daemon spawns every step run with `cwd =` the worktree and
  `AUTOSK_DB =` the canonical project DB. The agent's writes against
  `.autosk/db` therefore still land in the project's single source of
  truth, while file edits stay inside the per-task worktree.
- On `done` / `cancel` (via `autosk step next …`, `autosk done`,
  `autosk cancel`, or any other terminal transition the engine
  records), the worktree **directory** is removed automatically and
  the **branch is preserved** for human review / merge.
- On `human` (parked) and sibling-step transitions the
  worktree is kept on disk — the human (or the next step's agent)
  needs it to inspect or continue the work.
- On a parked-failure (`parkTaskOnFailure`) the worktree is *also*
  preserved: the human needs the on-disk state to debug. Cleanup
  happens only at a real terminal status.

```bash
autosk create "Wire up auth" --workflow feature-dev
# task created; worktree allocated at ~/.autosk/worktrees/myproj-7a3b9c2e/ask-bea935
# branch autosk/ask-bea935 created off HEAD

autosk show ask-bea935
# …
# worktree:      ~/.autosk/worktrees/myproj-7a3b9c2e/ask-bea935 (exists)
# branch:        autosk/ask-bea935

autosk show ask-bea935 --json | jq .worktree
# {
#   "path":   "/Users/me/.autosk/worktrees/myproj-7a3b9c2e/ask-bea935",
#   "branch": "autosk/ask-bea935",
#   "exists": true
# }
```

The `worktree` block in `autosk show` (text + JSON) is **omitted
entirely** for non-isolated workflows and for tasks with no workflow,
so existing JSON consumers don't see a new always-present key.

`autosk workflow show <name> [--json]` carries the isolation mode
verbatim. The `--json` shape always includes an `isolation` field
(`"none"` or `"worktree"`); the text form only prints an
`isolation:` line when the mode is non-default.

#### Updating isolation

The isolation field is editable after `workflow create` via:

```bash
autosk workflow update <name> --isolation <none|worktree>
                              [--force] [--dry-run] [--json]
```

`--isolation` is the only mutable field in v0.4. Other workflow fields
(`description`, `first_step`, the step graph) are still immutable —
delete and recreate the workflow if you need to change them.

By default the verb refuses when any task currently references the
workflow in a non-terminal state (`new` with `workflow_id`, `work`,
`human`). Each offending id is printed on its own line under
`non-terminal task in workflow:` and the verb exits non-zero. Pass
`--force` to bypass the guard with mode-specific extra work.

Safety matrix (rows = current isolation; columns = target + `--force`):

| Current → Target | No non-terminal tasks | Non-terminal tasks, no `--force` | Non-terminal tasks, with `--force` |
|---|---|---|---|
| `none` → `none` | no-op | no-op | no-op |
| `worktree` → `worktree` | no-op | no-op | no-op |
| `none` → `worktree` | flip + commit | refuse, list tasks | per task: `wt.Ensure(root, id, HEAD)`; atomic rollback on any Ensure failure; then flip + commit |
| `worktree` → `none` | flip + commit | refuse, list tasks | flip + commit; report carries leftover worktree paths for human cleanup |

The two `--force` paths in detail:

- **`none → worktree --force`** allocates a fresh worktree at `HEAD`
  for every non-terminal task before flipping the column. If any one
  `Ensure` fails, every successful Ensure for this run is rolled back
  via `worktree.OnTerminal` and the column is left unchanged; the
  error names the failing task and lists the rolled-back set under
  `rolled back:`.
- **`worktree → none --force`** does **not** remove existing
  worktree directories (that would discard uncommitted state). The
  leftover `(taskID, path)` pairs are printed under
  `leftover worktree (not removed):`; reap them later with
  `autosk worktree rm <task-id>` once you have salvaged anything you
  need.

Synthetic `single:<agent>` workflows are always rejected with
`cannot update synthetic workflow %q (single:<agent> rows are pinned
to isolation=none)`, regardless of `--force`.

No-op flips (target equals the current mode) exit 0 without writing
or committing a doltlite change. `--json` still emits a well-formed
report with `noop=true`.

`--dry-run` runs the same guard logic and the same Ensure-planning
logic, but performs no git operations and no DB writes. The printed
report carries `dry_run=true`. Use it to preview the side-effects
before committing.

`--json` always emits `UpdateIsolationReport` to stdout — including
on the refusal / synthetic / Ensure-failure paths. The exit code
carries the failure signal; the JSON body carries the diagnosis
(offending task ids, rolled-back ensures, leftover worktrees, etc.).
Machine consumers should read the body unconditionally and inspect
the exit code separately.

On a successful flip the verb commits the doltlite write with
`workflow update <name> isolation=<old>→<new>`.

**Daemon race.** The verb does **not** lock against an in-flight
daemon. A run that the daemon has already picked up finishes in its
current cwd; the new mode takes effect on the next step run for each
affected task. Stop the daemon first (`autosk daemon stop`) if you
need a strict cutover.

```bash
# Preview a flip without committing — non-zero exit if the guard
# would refuse, just like the real run.
autosk workflow update feature-dev --isolation worktree --force --dry-run

# Actually flip; atomic rollback if any Ensure fails.
autosk workflow update feature-dev --isolation worktree --force

# Back to non-isolated; the leftover dirs survive and are listed for
# manual cleanup.
autosk workflow update feature-dev --isolation none --force
# workflow feature-dev: isolation worktree → none
# leftover worktree (not removed):
#   ask-bea935  /Users/me/.autosk/worktrees/myproj-7a3b9c2e/ask-bea935
#   ...
#   (run `autosk worktree rm <task-id>` once you have salvaged uncommitted state)
```

The legacy escape hatch (`autosk sql --write "UPDATE workflows SET
isolation = '…'"`) still works but bypasses every safety check, makes
no worktree-side-effect calls, and is invisible to `autosk lazy` —
prefer the verb.

Design: [`docs/plans/20260521-Workflow-Update-Isolation.md`](plans/20260521-Workflow-Update-Isolation.md).

#### `enroll --base-ref`

When enrolling an existing task into an isolated workflow you can pick
the base ref the worktree's branch is forked from:

```bash
autosk enroll ask-bea935 --workflow feature-dev --base-ref origin/main
```

`--base-ref` defaults to `HEAD`. It is **only** meaningful for
isolated workflows on a fresh allocation — passing it with `--agent`
or against a non-isolated workflow fails with `--base-ref requires the
target workflow to use isolation=worktree`. When the branch
`autosk/<task-id>` already exists (typical `cancel → enroll` or
`reopen → enroll` cycle after the worktree dir was reaped),
`--base-ref` is ignored with a stderr warning and the worktree is
re-allocated on the existing branch.

#### `autosk worktree`

Diagnostic / manual-recovery verbs. None of them mutate task rows.

| Verb | Behaviour |
|---|---|
| `autosk worktree list [--json]` | List every task in this project whose workflow has `isolation: "worktree"`, with task id, status, workflow, branch, on-disk presence, and path. Closed tasks whose dir survived a failed cleanup show `dir=missing` (`exists=false` in JSON). |
| `autosk worktree path <id> [--json]` | Print the derived worktree path. Pure helper; does not stat the path. Useful in shell aliases and scripts. |
| `autosk worktree rm <id> [--json]` | Force-remove the worktree directory; the branch is left intact. Refuses **only** `work` tasks (the daemon may be executing inside the dir right now). `new` / `human` / `done` / `cancel` are all accepted, so the `worktree_stranded` recovery flow below works without first closing the task. |

No `--all-projects` flag in v0.3: the CLI is single-process and has no
daemon round-trip for cross-project enumeration. Use it from each
project root individually.

#### Uncommitted work is discarded on terminal status

`git worktree remove --force` discards any uncommitted edits inside
the directory. The contract is **the agent commits its work before
signalling `done`**; everything else is intentionally lost. This is
why the kickback messages for `done` transitions in isolated
workflows remind the agent to commit first, and why the branch is
preserved (so a human can still inspect or merge what *was*
committed).

#### Missing worktree directory → auto-recovered

If the daemon picks up an isolated task and discovers its worktree
directory is gone, the executor transparently re-allocates it on the
existing `autosk/<task-id>` branch (via the same `worktree.Ensure`
the CLI uses on `enroll`) and the run proceeds. No park, no human
action required. The daemon emits an `executor: re-allocated missing
worktree` log line at `info` level so the recovery is auditable.

This covers the common cases where the dir went missing between
runs: terminal cleanup reaped it and the task was then
resumed/reopened, the operator manually `rm -rf`-ed
`~/.autosk/worktrees/...`, or a failed cleanup left only the branch.
The branch is the load-bearing piece (it survives terminal cleanup
by design), so re-creating the dir from the existing branch is
always safe.

If re-allocation itself fails (git not on PATH, project root no
longer a git repo, the slot is occupied by something that isn't a
registered worktree, etc.), the run fails with
`worktree_missing: re-allocate failed: ...` and the task is parked
to `human` for human triage.

#### Recovering from `worktree_stranded`

If the worktree directory is present but its `.git` no longer
resolves to the project's gitdir (the project was moved or
re-initialised), the run fails with `error=worktree_stranded` and the
task is parked to `human`. The worktree is **not**
auto-cleaned in this failure path — you may want to keep whatever is
on disk for debugging.

To recover, either give up:

```bash
autosk cancel ask-bea935          # branch preserved; dir reaped if any remained
```

or re-allocate the worktree and retry:

```bash
autosk worktree rm ask-bea935     # clean up the stranded dir
autosk cancel  ask-bea935         # human → cancel (‘reopen’ doesn’t
                                  # take human as a source state)
autosk enroll  ask-bea935 --workflow feature-dev   # Ensure re-allocates dir + branch
# the daemon picks it up automatically; the branch (autosk/ask-bea935)
# survived the round-trip so the worktree reuses it.
```

There is no need to detour through `reopen` before `enroll`: `enroll`
now accepts `cancel` directly. The longer `cancel → reopen → enroll`
chain remains valid if you specifically want the task to spend a beat
in `new` for inspection.

A shorter recovery via plain `autosk resume` is **not** possible:
`resume` flips status back to `work` at the same step without
cleaning the stranded dir, so the next run would `Verify`-fail and
park again.

#### Reopening a closed isolated task

`autosk reopen` itself never touches git. The branch survives the
reopen unmolested. A subsequent `enroll <id> --workflow <iso>`
re-allocates a fresh worktree directory on the existing branch (so
whatever was committed before is still in `git log`), and the engine
resumes the workflow from the entry step. `--base-ref` is ignored on
this path with a warning.

The `reopen` detour is optional: `enroll` accepts `done` / `cancel`
directly and re-allocates the same way. Use `reopen` when you want
the task to spend a beat in `new` for inspection before re-enrolling.

#### Limitations

- **Per-task, not per-step.** Every step of a given task shares the
  same worktree. Per-step isolation is out of scope for v0.3.
- **Project moves are unsupported.** The worktree path is derived
  from the canonical project root; moving the project directory
  invalidates every existing worktree. Recovery is `Verify`-fail →
  `human` → manual cleanup (`autosk worktree rm`) →
  re-enroll.
- **Network filesystems are unsupported.** Same constraint as
  `.autosk/db` itself.
- **No `--all-projects` on `worktree list`** in v0.3 (see above).
- **No orphan-branch GC.** `autosk worktree list` surfaces leaked
  directories from broken runs, but branches accumulate — prune
  them with normal git workflow once the task is well and truly
  done.

Design: [`docs/plans/20260521-Worktree-Isolation.md`](plans/20260521-Worktree-Isolation.md).
Example: [`docs/examples/workflows/workflow-isolated-example.json`](examples/workflows/workflow-isolated-example.json).

### Task metadata

Every task carries a free-form JSON `metadata` blob (the
`tasks.metadata` column, nullable). The engine reserves the top-level
key `step_visits` for the per-step visit counters used by
`max_visits`; every other key is user-defined and untouched by the
engine.

The `autosk metadata` verb tree is the human-facing surface:

```bash
autosk metadata show <id> [--visits-pretty]
autosk metadata set   <id> --key K  [--value V | --json-value FILE]
autosk metadata unset <id> --key K
autosk metadata reset-visits <id> [--step NAME | --step-id ID]
```

A few notes:

- `--key` is a dotted JSON object path (e.g. `tags`, `notes.tmp`,
  `step_visits.st-abcd`). Array indexing is not supported.
- `--value V` parses V as a JSON literal when V starts with one of
  `[`, `{`, `"`, `t`, `f`, `n`, `-`, or `0..9`; otherwise V is treated
  as a plain string. `--json-value FILE` (or `-` for stdin) reads a
  raw JSON value. Exactly one of `--value` / `--json-value` is
  required.
- Writes under `step_visits` are validated against the engine's typed
  shape (object of `step_id → non-negative integer`); attempts to set
  it to anything else fail before any DB write lands. This guards the
  engine from human-driven corruption.
- `metadata reset-visits` with no flag clears the entire
  `step_visits` map; with `--step NAME` it resolves the name against
  the task's current workflow and removes only that entry; with
  `--step-id ID` it removes the entry for that step id directly
  (handy for orphaned counters left behind when a workflow was
  recreated).
- No-op writes are detected — `metadata unset --key missing` or
  `reset-visits` on a task with no counters does not bump `updated_at`
  and does not produce a dolt commit.
- `autosk show <id>` (human renderer) surfaces a one-line
  `visits: dev 3/5, review 5/5*, validator 1` summary inline when the
  task has `step_visits`. The `*` marker flags steps at or over their
  cap; bare `N` (no denominator) means the step is uncapped.
  `autosk show --json` includes the raw `metadata` object verbatim.

See [`docs/examples/workflows/workflow-example.json`](examples/workflows/workflow-example.json)
for a working example with caps.

### Per-step agent overrides

A step's `agent.params` block overrides the agent package's defaults
for *this step only*. The whole block is optional; absent keys keep
the package's value.

```jsonc
{
  "name": "generic-wf",
  "first_step": "generic",
  "steps": {
    "generic": {
      "agent": {
        "name": "@autogent/generic",
        "params": {
          "model": "claude-sonnet-4-6",
          "thinking": "high",
          "first_message": "You are generic agent...",
          "extra_args": ["--no-tool", "web_fetch"],
          "pi_extensions": ["/abs/path/to/ext.ts"],
          "pi_skills":     ["/abs/path/to/skill"]
        }
      },
      "next_steps": [
        { "task_status": "human", "prompt_rule": "When a human should accept it." },
        { "task_status": "done",  "prompt_rule": "When the task is complete." }
      ]
    }
  }
}
```

Merge semantics:

- **Scalars** (`model`, `thinking`, `first_message`) replace the
  package default when the key is present (even when set to the empty
  string).
- **Arrays** (`extra_args`, `pi_extensions`, `pi_skills`) replace the
  package default wholesale when the key is present with a non-null
  slice. There is no append mode.
- **`first_message_file`** is accepted in `params` (paths are resolved
  relative to the workflow JSON file at parse time) and inlined into
  `first_message`. It is mutually exclusive with `first_message`.
- **`runner`** is intentionally NOT supported: overrides can only
  change standard-agent fields. Trying to override a custom-runner
  agent (a package that declares `autosk.agent.runner`) with any
  `params` block fails the run with `agent_config_invalid`.
- The `thinking` value, when set, must be one of
  `off|minimal|low|medium|high|xhigh` (same as the package field).
- Unknown keys inside `params` are rejected at parse time.

> **Edge case.** Because `params` is persisted as JSON with
> `omitempty`, an explicit empty array (e.g. `"extra_args": []`)
> collapses to "absent" after a round-trip through the DB. If you
> need to clear an array, edit the package instead.

Validation enforces:

- `name` is unique, does not start with the reserved prefix `single:`,
  and does not contain the reserved managed-revision marker `@rev-`.
- `first_step` exists in `steps`.
- Every step has ≥1 transition.
- Every referenced agent exists in the `agents` table.
- Every sibling-step target is defined.

### Task

In v0.2 a task carries three new nullable FKs: `author_id`,
`workflow_id`, `current_step_id`. The status enum is exactly:

```
new | work | human | done | cancel
```

Every spelling is ≤ 7 chars so the persisted columns stay tight and
the TUI doesn't need to truncate. (`claimed` is gone — replaced by
`enroll --agent`. `blocked` is still **not** a stored status: it's
derived from open blocker edges.) The legacy spellings
(`in_workflow`, `human_feedback`, `cancelled`) are not accepted on
any input surface (CLI flags, YAML/JSON workflow definitions, the
autosk_task / autosk_step JSON-RPC SDK). They were rewritten
in-place by a one-shot migration; see
[`docs/plans/20260521-Short-Statuses.md`](plans/20260521-Short-Statuses.md).

A SQL CHECK ties `status = 'work'` to `current_step_id IS NOT
NULL`. The CLI verbs (`create`, `enroll`, `resume`, `done`, `cancel`,
`reopen`) take care of keeping this invariant satisfied; you should not
need to think about it.

### `single:<agent>` synthetic workflows

`autosk create … --agent NAME` (and `autosk enroll … --agent NAME`)
auto-create a hidden workflow named `single:NAME` with one step `do`
whose transitions go to `{done, cancel, human}`. The
executor uses the same code path as a real workflow — there is no
second branch.

These rows have `is_synthetic = 1` and are hidden from
`autosk workflow list` by default. Use `--all` to reveal them.

---

## Make your own workflow

A workflow is a JSON file. Author it, import it, enroll tasks against it.

### 1. Write my-flow.json (shape below).

### 2. Import it.
autosk workflow create --file my-flow.json
autosk workflow show my-flow

### 3. Use it.
autosk create "Implement feature X" --workflow my-flow
autosk enroll ask-3f9b2c --workflow my-flow
```

### Minimal shape

```jsonc
{
  "name":        "my-flow",                     // unique; no "single:" prefix or "@rev-" marker
  "description": "…",                           // optional, free text
  "first_step":  "dev",                         // entry step; must exist in `steps`
  "isolation":   "worktree",                    // "none" (default) or "worktree"
  "steps": {
    "dev": {
      "agent": { "name": "@autogent/generic" }, // or your own "@your-org/developer"
      "max_visits": 5,                          // 0 / absent = unlimited
      "next_steps": [
        { "step": "review", "prompt_rule": "When the implementation builds and tests pass." }
      ]
    },
    "review": {
      "agent": { "name": "@autogent/generic" }, // or your own "@your-org/reviewer"
      "next_steps": [
        { "task_status": "done",  "prompt_rule": "When the review found nothing to fix." },
        { "step":        "dev",   "prompt_rule": "When fixes are needed." },
        { "task_status": "human", "prompt_rule": "When a human decision is required." }
      ]
    }
  }
}
```

A worked example lives at
[`docs/examples/workflows/workflow-example.json`](examples/workflows/workflow-example.json);
the isolated variant is
[`docs/examples/workflows/workflow-isolated-example.json`](examples/workflows/workflow-isolated-example.json).

### Install from a workflow package

Packages can ship one or more workflow JSON files and declare them in
`package.json` under `autosk.workflows`:

```jsonc
{
  "name": "@your-org/workflows",
  "version": "0.1.0",
  "autosk": {
    "agent": { "runner": "./src/agent.ts" }, // optional
    "workflows": [
      { "name": "feature-dev", "file": "./workflows/feature-dev.json" }
    ]
  }
}
```

Install one into the current project's `.autosk/db`:

```bash
autosk workflow install @your-org/workflows
autosk workflow install ../local-workflows --workflow feature-dev
autosk workflow install @your-org/workflows --version '^0.2.0' --json
```

If the package declares multiple workflows, pass `--workflow <name>`.
`--no-install` uses an already-installed package only and does not run
npm; any referenced agent packages must already be installed. If the
workflow package also declares `autosk.agent`, that agent is registered
in the current project. Scoped agents referenced by the workflow are
auto-installed unless `--no-install` is set. Custom-runner agents require
`@autosk/agent-runtime`; autosk installs it as needed unless `--no-install`
is set, in which case preinstall it with `autosk agent runtime install`.

`first_message_file` values inside packaged workflow JSON are resolved
relative to that workflow file and must stay inside the workflow file's
directory.

### What you can do in a workflow

| Capability | Where | What it does |
|---|---|---|
| **Sibling-step transition** | `next_steps[].step` | Move the task to another step in this workflow. |
| **Terminal transition** | `next_steps[].task_status` | Close (`done` / `cancel`) or park to a human (`human`). One per entry — never both `step` and `task_status`. |
| **Prompt rule** | `next_steps[].prompt_rule` | Natural-language guidance the agent reads to decide which transition to emit. |
| **Loop cap** | step `max_visits` | Fails the run with `step_max_visits_exceeded` and parks to `human` once the cap is hit. See [Visit limits](#visit-limits-max_visits). |
| **Per-task git worktree** | top-level `isolation: "worktree"` | Allocates `~/.autosk/worktrees/<proj>/<task-id>` on branch `autosk/<task-id>`; each step runs there. See [Worktree isolation](#worktree-isolation). |
| **Per-step agent overrides** | step `agent.params` | Override `model`, `thinking`, `first_message`/`first_message_file`, `extra_args`, `pi_extensions`, `pi_skills` for this step only. Standard agents only — see [Per-step agent overrides](#per-step-agent-overrides). |
| **Single-step shortcut** | `autosk create … --agent <pkg>` | Skip the JSON entirely; autosk builds a hidden `single:<pkg>` workflow on the fly. |

---

## CLI walkthrough

This mirrors the W9 acceptance scenario (see plan §9), updated for the npm-package agent model.

```bash
# 1. Bootstrap.
autosk init
# initialized .../.autosk/db (schema_version=N)
# bootstrapped workflow feature-dev-generic (agent @autogent/generic@X.Y.Z installed)

# `autosk init` already seeded `feature-dev-generic` and installed
# `@autogent/generic` (see "Shipped default" above). The verbose path
# below is only needed if you want extra agents and a custom workflow.
autosk agent install @autosk/developer
autosk agent install @autosk/code-reviewer
autosk agent install @autosk/task-validator

# (each install reads the package's autosk.agent block from
#  ~/.autosk/packages/node_modules/<pkg>/package.json and registers it)

autosk workflow create --file docs/examples/workflows/workflow-example.json
# wf-XXXX

# 2. Create a task in the workflow.
id=$(autosk create "Implement auth module" --workflow feature-dev --json | jq -r .id)
autosk show "$id"
# status: work, current_step: dev, current_agent: developer

# 3. Run the daemon (in another shell).
autosk daemon serve --workers 1 &

# 4. The poller picks up the task; the developer agent spawns; at the
# end of its turn it calls (via the autosk pi-extension):
#
#     autosk step next $id --to review
#
# The daemon records the signal, advances the task to step=review,
# and the loop continues until validator → human.

# 5. Watch transitions.
while true; do
  autosk show "$id" --json | jq -r '"status=\(.status) step=\(.current_step // \"-\")"'
  sleep 2
done

# 6. Resume after human review.
autosk resume "$id"          # back to status=work at validator
# … the workflow continues; eventually validator → done (via `done`
# transition you added, or a human running `autosk done $id`).
```

The single-agent shorthand:

```bash
id=$(autosk create "Bump version to 0.2" --agent @autogent/generic --json | jq -r .id)
autosk show "$id"
# status: work, workflow: single:@autogent/generic, current_step: do

# The developer agent runs once, picks `--to done`, and the task closes.
```

### Enrolling an existing task

If a task already exists (typically `status='new'`, created without
`--workflow` / `--agent`), use `enroll` instead of recreating it:

```bash
autosk enroll ask-bea935 --workflow feature-dev
autosk enroll ask-bea935 --agent    @autogent/generic   # single:@autogent/generic
```

`enroll` is the post-creation mirror of `create --workflow` /
`create --agent`: it sets `workflow_id`, `current_step_id =
workflow.first_step` and flips status to `work`. Exactly one of
`--workflow` / `--agent` is required.

`enroll` accepts `new`, `human`, `done`, and `cancel` source
statuses. The only refusal is `work`:

| Current status                | Behaviour                                                                                                                                                       |
|-------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `new`                         | enroll succeeds; the task is stamped with the chosen workflow + entry step and flipped to `work` atomically.                                                    |
| `human`                       | enroll succeeds; the same atomic stamp re-attaches the task to the workflow (same or different from before). `resume` remains available for the explicit `human → work` no-switch path. |
| `done` / `cancel`             | enroll succeeds; the task is re-attached without going through `reopen` first. `reopen` remains available for the explicit `done|cancel → new` path when you want to inspect the task in `new` state before re-enrolling. |
| `work`                        | rejected: the task is currently owned by the engine. To switch workflows, `cancel` then `enroll` (or `reopen` first if you want to inspect the task in `new` state). |

`metadata.step_visits` is **not** cleared at enroll time — re-enrolling
into the same workflow keeps `step.max_visits` honest. If you want a
clean slate, run `autosk metadata reset-visits <id>` first. Stale
entries from a prior workflow are left in place; clear them the same
way.

For `isolation=worktree` workflows the per-task `autosk/<id>` branch
is preserved across `done`/`cancel` (only the worktree dir is
reaped); the next enroll re-allocates the dir on the existing branch
without needing `--base-ref`.

`enroll --json` emits the same shape as `autosk show --json`
(including the derived `blocked` / `blocked_by` / `blocks` fields).

---

## Agent contract: `autosk step next`

When an agent runs **inside a workflow step** (i.e. the daemon spawned
it), it MUST call exactly one of:

```bash
autosk step next <task-id> --to <sibling-step-name>
autosk step next <task-id> --to done
autosk step next <task-id> --to cancel
autosk step next <task-id> --to human
```

before stopping its turn. The valid targets for the current step are
listed in the agent's initial prompt (one bullet per outgoing
transition). Calling `step next` twice in the same run is rejected.

If the agent ends its turn without emitting a signal, the daemon sends a
corrective message and retries up to `max_corrections` (default 3)
times. After that the run is marked `failed` with
`error="agent_did_not_emit_transition"`.

The pi-extension's `step_next` action is the agent's tool-level handle
into this CLI — same arguments, same semantics.

---

## What humans still do directly

For tasks in `human`, the agent has handed control back to a
human. Humans can:

- **Inspect**: `autosk show <id>`, `autosk comment list <id>`.
- **Reply**: `autosk comment add <id> "..."` — the comment is surfaced
  to the next step's prompt.
- **Resume**: `autosk resume <id> [--to STEP]`. By default returns to
  the same step (so the same agent sees the new comments); `--to STEP`
  jumps to a different step in the same workflow.
- **Close**: `autosk done <id>` / `autosk cancel <id>` — direct, never
  through `step next`. The poller only picks tasks whose current step
  has `is_human=0`, so these verbs never race with the engine.
- **Reopen**: `autosk reopen <id>` — `done`/`cancel` → `new`. Clears
  `current_step_id`; preserves `workflow_id` for audit.
- **Inspect / edit metadata**: `autosk metadata show <id>` (add
  `--visits-pretty` for a labelled visit-counter view),
  `autosk metadata set/unset <id> --key K …`, and
  `autosk metadata reset-visits <id> [--step NAME | --step-id ID]`
  — the escape hatch after a `step_max_visits_exceeded` park. See
  [Visit limits](#visit-limits-max_visits) above.

`autosk update --status` is rejected on `work` tasks: those
transitions are owned by the workflow engine.

---

## Wiring inside the daemon

```
┌───────────────────────────────────────────────────────┐
│            autosk daemon serve                        │
│                                                       │
│   ┌────────────┐   ┌────────────┐   ┌──────────────┐  │
│   │  poller    │──▶│ scheduler  │──▶│ step         │  │
│   │ (§5.2 SQL) │   │ N workers  │   │ executor     │  │
│   └────────────┘   └────────────┘   │  - load agent│  │
│                                     │    config    │  │
│                                     │  - spawn pi  │  │
│                                     │  - wait for  │  │
│                                     │    step_signals
│                                     │  - kickback  │  │
│                                     │  - advance   │  │
│                                     └──────────────┘  │
└───────────────────────────────────────────────────────┘
                          │
                          ▼
                pi --mode rpc (per spawn)
```

Per `--poll-interval` (default 2s), the poller scans:

```sql
SELECT t.id, t.current_step_id
  FROM tasks t
  JOIN steps s   ON t.current_step_id = s.id
  JOIN agents a  ON s.agent_id        = a.id
 WHERE t.status = 'work'
   AND a.is_human = 0
   AND NOT EXISTS (
     SELECT 1 FROM daemon_runs r
     WHERE r.task_id = t.id AND r.status IN ('queued','running')
   )
 ORDER BY t.priority ASC, t.created_at ASC;
```

Each row is materialised as a `daemon_runs` entry (status=queued) and
pushed onto the scheduler queue. A worker picks it up; the executor
spawns pi with the step's agent config and watches `step_signals` after
every end-of-turn.

---

## Glossary

| Term | Meaning |
|---|---|
| **Step** | One node in a workflow. Has a name (`dev`, `review`, …), an `agent_id`, and ≥1 outgoing transition. |
| **Transition** | One edge from a step. Carries either a sibling-step target or a `task_status` (`done`/`cancel`/`human`), plus a natural-language `prompt_rule`. |
| **Signal** | A row in `step_signals`. Inserted by `autosk step next` from inside the agent; consumed by the executor after end-of-turn. PK on `run_id` enforces "exactly one signal per run". |
| **Kickback** | A corrective message sent to the agent when it ends a turn without emitting a signal. Bounded by `max_corrections`. |
| **Synthetic workflow** | `single:<agent>` workflow auto-created by `--agent NAME`. `is_synthetic = 1`; hidden from `workflow list` unless `--all`. |

---

## Pointers

- Design: [`docs/plans/20260517-Workflows-Plan.md`](plans/20260517-Workflows-Plan.md)
- Worktree isolation: [`docs/plans/20260521-Worktree-Isolation.md`](plans/20260521-Worktree-Isolation.md)
- Daemon details: [`docs/daemon.md`](daemon.md)
- pi-extension surface: [`extension/src/`](../extension/src/)
- Example workflow: [`docs/examples/workflows/workflow-example.json`](examples/workflows/workflow-example.json)
- Isolated example: [`docs/examples/workflows/workflow-isolated-example.json`](examples/workflows/workflow-isolated-example.json)
