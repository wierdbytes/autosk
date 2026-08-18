# The `autosk` CLI

`autosk` is the command-line front end: a small, scriptable, agent-friendly verb
set for creating tasks, driving them through workflows, and inspecting what the
engine is doing. It is a **pure JSON-RPC client** of the `autoskd` daemon — it
owns no storage and never touches `.autosk/` directly. The daemon is
**auto-spawned on first use**, so there is nothing to start by hand.

This page is the complete CLI reference. For the concepts the verbs operate on —
the task model, the status machine and *ready set*, blockers, metadata, comments,
and the `.autosk/` layout — read [docs/concepts.md](concepts.md) first; the verbs
make a lot more sense once those click. For the process behind the socket, see
[docs/daemon.md](daemon.md).

> **Conventions in this page.** `<angle>` is a required argument, `[square]` is
> optional, `…` means "one or more". Human output renders timestamps in your
> **local timezone**; `--json` output stays **RFC3339 UTC** (see
> [Global behavior](#global-behavior)).

---

## Global behavior

Every verb shares the same two persistent flags and the same daemon-client
plumbing.

### Flags

| Flag | Effect |
| --- | --- |
| `--json` | Emit machine-readable JSON instead of the human form. `watch` uses JSON Lines; timestamps stay RFC3339 UTC. |
| `-q`, `--quiet` | Suppress non-essential output. **Write** verbs print nothing on success; **read** verbs still print their result (you asked for it). |

`--json` and `--quiet` are global — they work on any verb. A few verbs that only
emit a confirmation line (`block`, `unblock`, `dep list`, `session abort`,
`session input`, `project add`) ignore `--json` and always print text.

### Human vs. machine output

The same task in both forms:

```text
$ autosk show ask-a1b2c3
[ask-a1b2c3]: Wire up the file store
status:        work
workflow:      feature-dev
step:          dev
blocked:       no
blocked_by:    -
blocks:        ask-3c4d5e
comments:      0
created_at:    2026-05-13 13:00:00
updated_at:    2026-05-13 14:42:13
description:
  Implement the task store and the smoke test.
```

```json
$ autosk show ask-a1b2c3 --json
{"id":"ask-a1b2c3","title":"Wire up the file store","description":"Implement the task store and the smoke test.","status":"work","workflow":"feature-dev","step":"dev","created_at":"2026-05-13T10:00:00Z","updated_at":"2026-05-13T11:42:13Z","blocked":false,"blocked_by":[],"blocks":["ask-3c4d5e"],"comment_count":0,"metadata":{}}
```

The human form prints `created_at: 2026-05-13 13:00:00` (local, `+03:00` here)
while `--json` prints `10:00:00Z` (UTC) — same instant, different rendering. Pipe
`--json` to `jq`; never scrape the human table.

### Daemon auto-spawn

The first verb that needs the daemon starts it: the client resolves the socket
(`$AUTOSK_SOCK`, else `~/.autosk/daemon.sock`), and if nothing is listening it
launches `autoskd` and waits for it to come up. One daemon per host serves every
project; each request carries a `{cwd}` selector so the daemon knows which
`.autosk/` you mean. The only verb that **never** auto-spawns is
[`version`](#version) — it probes a running daemon best-effort and reports
"not running" otherwise.

### Read clients vs. write clients

Verbs split into three project-resolution behaviors:

- **Read verbs** (`show`, `list`, `ready`, `next`, `dep list`, `comment list`,
  `metadata show`, `workflow …`, `session list/get/transcript`,
  `watch`, `project diagnostics`) require a **discoverable** `.autosk/` project (walk-up
  from the working directory). Missing project → hard error telling you to run
  `autosk init`.
- **Write verbs** (`create`, `update`, `done`, `cancel`, `reopen`, `block`,
  `unblock`, `enroll`, `resume`, `comment add/edit/delete`,
  `metadata set/unset`, `session abort/input`) run the **auto-init gate**
  (see [`init`](#init--project-setup)) when no project exists yet.
- **Daemon-scoped verbs** (`version`, `init`, `project list/add`, `ext …`) don't
  require a pre-existing project.

### Exit codes & errors

`autosk` exits `0` on success and `1` on failure. A daemon or argument error
prints `autosk: <message>` to **stderr**. Two verbs use the exit code as a
signal even on a "clean" run:

- [`next`](#ready--next) exits `1` (and prints `(nothing ready)` / `null`) when
  the ready set is empty, so `autosk next && …` short-circuits in a script.
- [`ext update`](#ext--extensions) exits `1` if any package failed to update
  (in both `--json` and table modes).

---

## init — project setup

### `init`

```bash
autosk init
```

Create `./.autosk` (the `tasks/`, `sessions/`, `extensions/` skeleton) and
register the project in `~/.autosk/projects.json`. **Idempotent** — re-running is
a no-op. There is no database and no per-project workflow seeding: workflows and
agents ship as [extensions](extensions.md) available to every project.

```text
$ autosk init
initialized /Users/you/project/.autosk
```

### Implicit auto-init

You rarely need `init`: the first **write** verb in a fresh directory offers to
create the project for you.

```text
$ autosk create "Tidy the README"
No autosk project found at or above /Users/you/project.
Create a new autosk project in /Users/you/project/.autosk? [Y/n] ⏎
autosk: created /Users/you/project/.autosk
ask-7a1e4d
```

The prompt only appears on an interactive TTY in normal output mode. It is
suppressed — and the answer assumed **yes** — under `--json`, `--quiet`, or when
`AUTOSK_AUTOINIT_ASSUME_YES` is set (handy for CI that runs with a TTY attached).
Set `AUTOSK_NO_AUTOINIT` to disable auto-init entirely: a write verb in a
project-less directory then fails instead of creating one. See
[Environment variables](#environment-variables).

---

## Task lifecycle: reads & edits

### `create`

```bash
autosk create [title] [--title T] [-d|--description TEXT] \
              [--blocks ID,…] [--blocked-by ID,…] [--workflow NAME]
```

Create a task. The title may be positional **or** `--title` (giving both with
different values is an error). The task starts in `new` unless `--workflow NAME`
enrolls it on creation (status becomes `work`).

| Flag | Effect |
| --- | --- |
| `--title T` | Title (alternative to the positional arg). |
| `-d`, `--description TEXT` | Description. `-` reads the body from **stdin**. |
| `--blocks ID,…` | This new task **blocks** each listed task (adds the reverse edge). |
| `--blocked-by ID,…` | This new task **waits on** each listed task. |
| `--workflow NAME` | Enroll into `NAME` at its first step right after creation. |

Human output is just the new id; `--json` emits the full task wire view.

```text
$ autosk create "Wire up the auth flow"
ask-3f9b2c

$ printf 'Multi-line\nbody\n' | autosk create "From stdin" -d -
ask-9c8b7a

$ autosk create "Fix flaky test" --workflow feature-dev --json | jq -r .id
ask-1d2e3f
```

To enroll an **existing** task, use [`enroll`](#enroll) instead of recreating it.

### `show`

```bash
autosk show <id>
```

Print one task: the stored fields plus the derived `blocked` / `blocked_by` /
`blocks` edges and `comment_count`. A missing id errors with
`task not found: <id>`. Honors `--json`. (Examples above in
[Global behavior](#human-vs-machine-output).)

### `watch`

```bash
autosk watch <id> [--no-follow] [--json]
```

Print the current state of one task, then follow its live progress. The default
timeline is intended for people; `--json` emits one complete JSON object per
line for agents and scripts. The command reports:

- status and workflow-step transitions, including self-loop retries;
- blocker changes;
- session starts and finishes, including the session id and outcome;
- elapsed wall-clock time when a step finishes;
- token totals on the session-finish line when authoritative usage is available,
  or `tokens unavailable` / JSON `null` values otherwise.

```text
$ autosk watch ask-3f9b2c
14:32:10  snapshot ask-3f9b2c status=work workflow=feature-dev step=dev sessions=019f…
14:44:45  step     dev -> review (12m 35s)
14:44:45  session  019f… finished: done (tokens unavailable)
14:44:46  session  019a… started (step review)
```

The opening snapshot includes any queued/running session ids. After the
snapshot, task changes come from the existing `task.subscribe` channel and
session lifecycle changes from `session.subscribeProject`; there is no polling
and no watch-specific daemon storage. When attaching mid-step, `watch` reads the
current session transcript header for the durable step-start timestamp. This is
a live stream, not retained task history: earlier events are not replayed.

`--no-follow` prints only the current snapshot and exits. In follow mode the
command exits successfully after a task reaches `done` or `cancel` and its live
sessions finish. `Ctrl+C` also exits cleanly. An unknown task or a lost daemon
connection is an error.

JSON Lines events always contain `type`, `task_id`, and an RFC3339 UTC
`timestamp`. Event-specific fields are:

| `type` | Additional fields |
| --- | --- |
| `snapshot` | `title`, `status`, nullable `workflow` / `step`, `blocked`, `blocked_by`, `live_session_ids` |
| `status_change` | `previous_status`, `status` |
| `step_change` | nullable `workflow` / `previous_step` / `step` / `duration_seconds` |
| `blocked_change` | `previous_blocked`, `blocked`, `blocked_by` |
| `session_start` | `session_id`, nullable `step` |
| `session_finish` | `session_id`, nullable `step`, `outcome`, nullable `error` and token counts |

```json
{"type":"step_change","task_id":"ask-3f9b2c","timestamp":"2026-08-18T12:44:45Z","workflow":"feature-dev","previous_step":"dev","step":"review","duration_seconds":755}
```

`watch` appears in root help and `autosk watch --help`. Cobra's bash, zsh,
fish, and PowerShell completion generators include the command, its
`--no-follow` flag, and task-id suggestions.

### `list` / `ls`

```bash
autosk list [--status S,…] [--limit N]
```

List tasks as a compact `ID  STATUS  TITLE` table. Default scope is **open work**
(`new`, `work`, `human`). A blocked task shows a trailing `*` on its status.

| Flag | Effect |
| --- | --- |
| `--status S,…` | Comma-separated filter: `new`, `work`, `human`, `done`, `cancel`. Use `--status all` for no filter. Invalid values error. |
| `--limit N` | Cap the rows (`0` = unlimited). Applied client-side. |

```text
$ autosk list
ID          STATUS  TITLE
ask-3f9b2c  new*    Wire up the auth flow
ask-7a1e4d  new     Tidy the README

$ autosk list --status done,cancel --limit 20
$ autosk list --status all --json | jq length
```

An empty result prints `(no tasks)` to stderr (suppressed under `--quiet`).

### `ready` / `next`

```bash
autosk ready [--limit N]
autosk next
```

The **ready set** is the tasks in `new` status with no open blocker — "what
should I work on right now?". `ready` lists them (same table as `list`); `next`
is `ready --limit 1` that prints the single top task in `show` form.

`next` is script-friendly: it prints `null` (`--json`) or `(nothing ready)` and
**exits non-zero** when the set is empty.

```bash
# Enroll the next ready task, or do nothing if there is none:
id=$(autosk next --json | jq -r .id) && autosk enroll "$id" --workflow feature-dev
```

An enrolled (`work`) task is never in the ready set, even if unblocked — once the
engine owns a task you don't pull it by hand.

### `update`

```bash
autosk update <id> [--title T] [-d|--description TEXT]
```

Change a task's `title` and/or `description`. At least one flag is required.
Status changes do **not** go through `update` — use
[`done`/`cancel`/`reopen`](#status-flips) or [`resume`](#resume); metadata goes
through [`metadata set/unset`](#metadata). Honors `--json` / `--quiet`.

---

## Status flips

```bash
autosk done   <id>
autosk cancel <id>
autosk reopen <id>
```

Administrative status overrides. Each is a **raw status flip** — it does not run
the workflow's `onTransit` hook, and the engine rejects it (`CONFLICT`) for a
task with a **live session** (the engine owns enrolled tasks while they run).

- `done` → `done` (completed).
- `cancel` → `cancel` (abandoned).
- `reopen` reopens a closed task: a never-enrolled task returns to the `new`
  backlog; a task that has a workflow parks to `human` so you can
  [`resume`](#resume) it.

Each prints the updated task (honors `--json` / `--quiet`). Isolation is
agent-owned and torn down by a workflow cleanup step, so there is no `--force`
dirty-gate — a worktree branch is always preserved.

---

## Workflow operations

### `enroll`

```bash
autosk enroll <id> --workflow NAME [--step STEP]
```

(Re-)attach an existing task to a workflow; status becomes `work`. `--workflow`
is required. `--step` starts at a chosen step (default: the workflow's first
step). Enroll is allowed from `new` / `cancel` / `human`; `work` and `done` are
rejected.

```bash
autosk enroll ask-bea935 --workflow feature-dev
autosk enroll ask-bea935 --workflow feature-dev --step review
```

### `resume`

```bash
autosk resume <id> [--to STEP]
```

Move a task out of `human` back into `work`. With no `--to`, it re-enters the
step it was parked in. `--to STEP` jumps to a sibling step in the same workflow;
`--to done|cancel|human` relocates to a terminal/park status instead.

```bash
autosk resume ask-bea935                # back to the parked step
autosk resume ask-bea935 --to dev       # jump to the dev step
autosk resume ask-bea935 --to cancel    # park-to-terminal
```

Prints the updated task (honors `--json` / `--quiet`).

---

## Dependencies

```bash
autosk block   <id> <blocker-id>…
autosk unblock <id> <blocker-id>… | --all
autosk dep list <id>
```

- `block` adds blocker edges: each `<blocker-id>` blocks `<id>`. Idempotent; a
  self-edge is rejected; a blocker that doesn't exist yet is stored and stays
  hidden until it appears.
- `unblock` removes specific edges, or **every** incoming edge with `--all`
  (which takes only the task id, no blocker list).
- `dep list` prints the derived incoming (`blocked_by`) and outgoing (`blocks`)
  edges. Read-only; always prints text.

```text
$ autosk block ask-3f9b2c ask-7a1e4d
blocked ask-3f9b2c by [ask-7a1e4d]

$ autosk dep list ask-3f9b2c
blocked_by: [ask-7a1e4d]
blocks:     []

$ autosk unblock ask-3f9b2c --all
unblocked ask-3f9b2c (1 edge(s) removed)
```

A blocker that reaches `done`/`cancel` no longer blocks, so finishing a
dependency re-admits the waiting tasks to the ready set automatically. See
[docs/concepts.md → Blockers](concepts.md#blockers--the-dependency-graph).

---

## Comments

```bash
autosk comment add    <id> [text] [--author NAME]
autosk comment list   <id>
autosk comment edit   <id> <comment-id> [text]
autosk comment delete <id> <comment-id>      # alias: rm
```

Comments are the **cross-agent channel**: the engine surfaces every prior comment
at the top of each step's prompt, so a comment is how one step hands off to the
next. For `add` and `edit`, the text may be a positional argument, piped via
**stdin** (omit the arg or pass `-`), and the **author** defaults to
`$AUTOSK_AGENT` (or `human`), overridable with `--author`.

```text
$ autosk comment add ask-3f9b2c "Refactored the retry loop; see PR notes."
[alice@2026-06-24 12:30:00] (id=cm-1a2b3c):
Refactored the retry loop; see PR notes.

$ git log -1 --format=%B | autosk comment add ask-3f9b2c -    # body from stdin

$ autosk comment list ask-3f9b2c
ID         AUTHOR  CREATED              TEXT
cm-1a2b3c  alice   2026-06-24 12:30:00  Refactored the retry loop; see PR notes.
```

`add`/`list`/`edit` honor `--json`; `add`/`edit` honor `--quiet`. `delete`
prints a one-line confirmation.

---

## Metadata

```bash
autosk metadata show  <id>
autosk metadata set   <id> <dotted.key> <value>
autosk metadata unset <id> <dotted.key>…
```

Read and write a task's free-form `metadata` bag.

- `show` prints the object (pretty 2-space JSON; compact under `--json`). A task
  with no metadata renders `{}`.
- `set` writes one **dot-path** key. The `value` is parsed as a JSON literal
  (number / bool / null / object / array / quoted string), falling back to a
  plain string when it isn't valid JSON. Intermediate objects are created.
- `unset` removes one or more dot-path keys, pruning emptied parent objects.

```text
$ autosk metadata set ask-3f9b2c owner alice         # string
$ autosk metadata set ask-3f9b2c sprint 42           # number 42
$ autosk metadata set ask-3f9b2c labels.priority high
$ autosk metadata show ask-3f9b2c
{
  "labels": {
    "priority": "high"
  },
  "owner": "alice",
  "sprint": 42
}
```

The engine reserves `step_visits` (a `step → count` map for workflow visit caps).
Reset a stuck counter by hand — this is the supported escape hatch:

```bash
autosk metadata unset ask-3f9b2c step_visits        # reset every step's count
autosk metadata set   ask-3f9b2c step_visits.dev 0  # reset just the dev count
```

See [docs/concepts.md → metadata & `step_visits`](concepts.md#task-metadata--step_visits).

---

## Workflows (read-only)

```bash
autosk workflow list            # alias: ls
autosk workflow show <name>
```

Inspect the workflows registered by the project's extensions. **Read-only** —
v2 workflows are code, so there is no create/delete/update; editing a workflow
means editing its [extension](extensions.md). Both honor `--json`.

```text
$ autosk workflow list
NAME         FIRST_STEP  STEPS
feature-dev  dev         accept,cleanup,dev,docs,review,validator

$ autosk workflow show feature-dev
name:       feature-dev
first_step: dev
steps:
  STEP       KIND    TARGETS
  accept     human   accept, cleanup, dev, docs, review, validator, →done, →cancel, →human
  cleanup    agent   accept, cleanup, dev, docs, review, validator, →done, →cancel, →human
  dev        agent   accept, cleanup, dev, docs, review, validator, →done, →cancel, →human
  …
```

Steps are listed **alphabetically** (the workflow's runtime order lives in its
`onTransit`, not the projection). In the KIND column, an **agent** step (and the
cleanup step) has `status=null` and renders `agent`; a `statusStep` renders its
terminal/park status (`human` here for `accept`). TARGETS is the **conservative
declared superset** — every step plus `→done` / `→cancel` / `→human` — because
the real edges are decided at runtime by the workflow's `onTransit`.

---

## Sessions

```bash
autosk session list [--task ID]               # alias: sess
autosk session get  <session-id>
autosk session transcript <session-id> [--from-line N] [--limit N]   # aliases: messages, log
autosk session abort <session-id>
autosk session input <session-id> <message> [--followup]
```

A **session** is one invocation of an agent's `onRun` for one task step. These
verbs inspect and steer the daemon's live and historical sessions.

- `list` — one row per agent run (`SESSION TASK STEP AGENT STATUS ERROR`);
  `--task` scopes to one task.
- `get` — a single session's metadata (task, workflow, step, agent, status,
  timestamps).
- `transcript` — render the pi-format transcript; `--from-line` (1-based, header
  is line 1) and `--limit` page it.
- `abort` — abort a running session (parks the task to `human`).
- `input` — send a message to a **live** session: a **steer** mid-turn by
  default, or `--followup` to queue it after the current turn.

```text
$ autosk session list
SESSION    TASK        STEP  AGENT  STATUS   ERROR
ses-4f2a1c ask-3f9b2c  dev   pi     running  -

$ autosk session input ses-4f2a1c "focus on the error path first"
ses-4f2a1c: steer sent
```

`list`/`get`/`transcript` honor `--json`. For the session model — partial
streaming, transcripts, steer/abort — see
[docs/daemon.md → Sessions & transcripts](daemon.md#sessions--transcripts).

---

## Projects

```bash
autosk project list                    # known projects on this host
autosk project add [--name NAME]       # register the current directory
autosk project diagnostics             # alias: diag
```

- `list` — the projects registered in `~/.autosk/projects.json`
  (`NAME  ROOT`). Honors `--json`.
- `add` — register the current directory's project with the daemon (`--name`
  defaults to the root's basename). Resolution itself never auto-registers; this
  is the explicit register step.
- `diagnostics` — extension **load errors** for the current project (empty =
  `extensions: ok`). Honors `--json`. This is the first place to look when a
  workflow or agent you expected isn't showing up.

```text
$ autosk project diagnostics
project: /Users/you/project
extension load errors (1):
  - npm:@you/my-ext: SyntaxError: Unexpected token ')'
```

---

## ext — extensions

```bash
autosk ext add    <source> [-l|--local]
autosk ext list
autosk ext remove <source> [-l|--local]
autosk ext update [source] [-l|--local] [--global] [--dry-run|--check]
autosk ext reload
```

Manage the extensions recorded in `settings.json`. A **source** is either an
**npm spec** (`npm:@scope/pkg`, `npm:@scope/pkg@1.2.3`) or a **local path**
(`/abs`, `./rel`, `../rel`, `~/path`) — there is no bare-name → npm shorthand.
npm packages install into a packages prefix; a local path is referenced in place
(never copied).

By default these target the **global** scope (`~/.autosk`); `-l/--local` targets
the current project's `.autosk/`.

- `add` — install/register a source and record it, then **hot-apply** it to open
  projects (no daemon restart). Prints the scope, `settings.json` path, and
  `applied live to N open project(s)` (a global add reloads every open project,
  `-l/--local` reloads just that one); when no project is open it prints a soft
  restart hint instead (the change lands on the next open).
- `list` — installed extensions across both scopes (`SCOPE KIND RESOLVED
  SOURCE`).
- `remove` — drop the entry from `settings.json` (match npm by name, local by
  path); `node_modules` is left untouched. Also hot-applies (no restart): a
  removed workflow stops being scheduled and any non-live `work` task on it
  parks to `human`.
- `update` — bump **floating** npm entries (`npm:foo`) to newer registry
  versions in place. Version-pinned (`npm:foo@1.2.3`) and local-path entries are
  skipped. Outside a project it updates global only; inside, the union of global
  + project (force with `--global` / `-l/--local`, which are mutually
  exclusive). `--dry-run` (alias `--check`) reports without installing.
  **Still restart-only** — editing installed code in place (the Bun module-cache
  wall) — so it keeps its restart hint when something changed.
- `reload` — rebuild the current project's merged (global + project) registry
  and swap it onto the live daemon on demand. Picks up added/removed extensions
  (including a brand-new or deleted `.autosk/extensions/` file); prints the
  root, registered workflows, and any load diagnostics + parked tasks.

```text
$ autosk ext add npm:@autosk/merge-to-current
installed npm:@autosk/merge-to-current (global scope)
  settings: /Users/you/.autosk/settings.json
applied live to 2 open projects (no restart needed)

$ autosk ext reload
reloaded /Users/you/code/app
  workflows: feature-dev, merge-to-current

$ autosk ext list
SCOPE   KIND  RESOLVED  SOURCE
global  npm   yes       npm:@autosk/feature-dev
project local yes       ./extensions/my-ext.ts

$ autosk ext update --check
SCOPE   PACKAGE                  FROM   TO     STATUS
global  @autosk/feature-dev      1.2.0  1.3.0  available
available 1 · unknown 0 · up-to-date 0 · skipped 0
```

All five honor `--json`. `ext update` **exits non-zero** if any package failed
(both `--json` and table modes). For the discovery/precedence model, see
[docs/extensions.md](extensions.md).

---

## version

```bash
autosk version
```

Print the CLI version (and commit, backend, Go/OS/arch) plus the daemon version
**when a daemon is already running**. This verb **never auto-spawns** — it has
zero side effects.

```text
$ autosk version
autosk 0.2.0 (a1b2c3d)
  backend:        autoskd
  daemon:         0.2.0 (a1b2c3d)
  go:             go1.25.0 darwin/arm64
```

When no daemon is running, the `daemon:` line reads `-   (not running)`. Honors
`--json`.

---

## Environment variables

| Variable | Effect |
| --- | --- |
| `AUTOSK_AGENT` | Default comment author (and the identity the CLI runs as). Falls back to `human`. Each enrolled agent runs with this set to its step name, which is why comments attribute to `dev` / `review` / … automatically. |
| `AUTOSK_CWD` | Override the project selector working directory (resolved to absolute). The daemon sets this for spawned workflow agents so every `autosk` call they make targets the real project root, not the isolated worktree. Defaults to the process working directory. |
| `AUTOSK_SOCK` | Daemon Unix-domain socket path. Defaults to `~/.autosk/daemon.sock`. |
| `AUTOSKD_BIN` | Explicit path to the `autoskd` binary for auto-spawn. Otherwise the client looks alongside the `autosk` binary, then on `PATH`. |
| `AUTOSK_NO_AUTOINIT` | When set, a write verb in a project-less directory **fails** instead of auto-creating `.autosk/`. |
| `AUTOSK_AUTOINIT_ASSUME_YES` | When set, the auto-init y/n prompt is suppressed and assumed **yes** (for automation that runs with a TTY attached). |

`--json` / `--quiet` also suppress the auto-init prompt (and assume yes). See
[docs/daemon.md → Transports, auth](daemon.md#transports-auth-single-instance-idle-shutdown)
for daemon-side env knobs (`AUTOSK_IDLE_SECS`, `AUTOSK_TOKEN_FILE`, …).

---

## Recipes

Short task-oriented snippets that compose the verbs above.

### Enroll the next ready task

```bash
id=$(autosk next --json | jq -r .id) && autosk enroll "$id" --workflow feature-dev
```

`autosk next` exits non-zero when nothing is ready, so the `&&` skips the enroll
on an empty backlog.

### Create a task with a long description from a file

```bash
autosk create "Migrate the config loader" -d - < notes/migration.md
```

`-d -` reads the description body from stdin; the same works for
`comment add <id> -`.

### Wire up a dependency chain at creation time

```bash
base=$(autosk create "Land the schema change")
autosk create "Backfill rows"  --blocked-by "$base"
autosk create "Flip the flag"  --blocked-by "$base"
```

Both follow-ups stay blocked (and out of the ready set) until `$base` reaches
`done`/`cancel`, at which point they re-enter the ready set automatically.

### Drive a whole project from a script

```bash
# Open work as JSON, newest first by id, just the titles:
autosk list --json | jq -r '.[].title'

# Tail a running session's transcript:
sid=$(autosk session list --json | jq -r '.[0].id')
autosk session transcript "$sid" --from-line 1
```

### Reset a workflow visit cap

```bash
# A task parked because it hit the dev visit cap — give it more passes:
autosk metadata set ask-3f9b2c step_visits.dev 0
autosk resume ask-3f9b2c
```

---

## Related

- [docs/concepts.md](concepts.md) — the task model, status machine, ready set,
  blockers, metadata, comments, `.autosk/` layout (the *why* behind the verbs).
- [docs/daemon.md](daemon.md) — `autoskd`: auto-spawn, the JSON-RPC surface,
  sessions & transcripts, transports & env.
- [docs/lazy.md](lazy.md) — the `autosk lazy` TUI: the same surface, interactive.
- [docs/workflows.md](workflows.md) / [docs/shipped.md](shipped.md) — the
  workflows and agents that `enroll` / `resume` drive.
- [docs/extensions.md](extensions.md) — how `ext …` sources are discovered,
  resolved, and loaded.
</content>
</invoke>
