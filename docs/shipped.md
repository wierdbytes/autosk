# Shipped workflows & agents

autosk ships a small catalog of ready-to-use **workflows** and **agents** as
`@autosk/*` npm packages. This page is the **reference** for what each one does,
how to install and enroll into it, and the knobs it exposes — plus a short
how-to per package and an end-to-end tutorial.

These are ordinary [extensions](extensions.md): one is provisioned for you on a
fresh machine ([`@autosk/feature-dev`](#autoskfeature-dev) — the
[first-run bootstrap](extensions.md#first-run-bootstrap)), and the rest you add
explicitly with [`autosk ext add`](extensions.md#managing-extensions). For the
**contracts** these packages implement (`WorkflowDefinition`,
`AgentDefinition`, `AgentRunContext`, `Sandbox`), see
[docs/workflows.md](workflows.md); for **discovery / loading / overrides**, see
[docs/extensions.md](extensions.md).

> **One workflow per task.** A task is enrolled into exactly one workflow at a
> time (`autosk enroll <id> --workflow <name>`, or `autosk create … --workflow
> <name>`). Picking from the catalog below is picking how that task gets driven.

## At a glance

| Package | Kind | Harness | Sandbox | Provisioned? | Get it |
| --- | --- | --- | --- | --- | --- |
| [`@autosk/feature-dev`](#autoskfeature-dev) | workflow | pi (`pi --mode rpc`) | per-task git worktree | first-run bootstrap | `--workflow feature-dev` |
| [`@autosk/feature-dev-cc`](#autoskfeature-dev-cc) | workflow | Claude Code (`claude -p`) | per-task git worktree | opt-in | `autosk ext add npm:@autosk/feature-dev-cc` |
| [`@autosk/feature-dev-docker`](#autoskfeature-dev-docker) | workflow | pi, in a container | per-task `docker run` | opt-in | `autosk ext add npm:@autosk/feature-dev-docker` |
| [`@autosk/gh-review`](#autoskgh-review) | workflow | pi, in a container (read-only `gh`) | per-task `docker run` | opt-in | `autosk ext add npm:@autosk/gh-review` |
| [`@autosk/merge-to-current`](#autoskmerge-to-current) | workflow | pi (`pi --mode rpc`) | none (host working tree) | opt-in | `autosk ext add npm:@autosk/merge-to-current` |
| [`@autosk/pi-agent`](#autoskpi-agent) | agent | pi (`pi --mode rpc`) | per-step (caller's) | bootstrap (transitive) | inline step value / `"pi"` chat |
| [`@autosk/claude-agent`](#autoskclaude-agent) | agent | Claude Code (`claude -p`) | per-step (caller's) | opt-in | inline step value / `"@autosk/claude-agent"` chat |

"Provisioned?" is whether the [first-run bootstrap](extensions.md#first-run-bootstrap)
installs it. `@autosk/pi-agent` is not bootstrapped on its own, but it arrives as
a transitive dependency of `@autosk/feature-dev`, so it is present on every
bootstrapped machine. Everything else you add explicitly.

---

# Shipped workflows

All three `feature-dev*` workflows share one graph and differ only in the
**harness** (pi vs Claude Code) and the **sandbox** (host worktree vs container).
`gh-review` is a separate single-step review workflow (a GitHub issue/PR,
read-only `gh` in a container), and `merge-to-current` a single-step branch
integration workflow.

## `@autosk/feature-dev`

The **reference workflow** — a full feature-development cycle wired to four
[`@autosk/pi-agent`](#autoskpi-agent) roles, each running in its own per-task
[`worktreeSandbox()`](workflows.md#worktreesandbox--a-per-task-git-worktree).
This is the one the daemon installs on first run, so every project can enroll
into it with no per-project files. Full contract:
[docs/workflows.md → The reference workflow](workflows.md#the-reference-workflow-feature-dev).

```text
dev ──▶ review ──▶ docs ──▶ validator ──▶ accept (human) ──▶ cleanup ──▶ done
 ▲        │                    │
 └────────┴────────────────────┘   (review→dev and validator→dev bounce-backs)
```

| Step | Kind | What it does |
| --- | --- | --- |
| `dev` | `piAgent` | first step; implements the task |
| `review` | `piAgent` (`thinking: xhigh`) | thorough code review; can bounce back to `dev` |
| `docs` | `piAgent` | documentation pass (leaves `CHANGELOG.md` to `validator`) |
| `validator` | `piAgent` | independent verification; on success runs release hygiene (CHANGELOG `[Unreleased]` + a clean, committed worktree) before `accept`; can bounce back to `dev` |
| `accept` | `statusStep("human")` | the engine parks here for a person's final acceptance |
| `cleanup` | `sandboxCleanupStep` | removes the worktree (branch preserved), then transits to `done` |

- **Sandbox.** Each agent step runs in a per-task git worktree at
  `~/.autosk/worktrees/<slug>/<task-id>` on branch `autosk/<task-id>`. The
  project root **must be a git repo**. The `cleanup` step removes the worktree
  on the way to `done` while **preserving the branch** (so the work survives for
  review/merge). Route every terminal through `cleanup`, or the worktree leaks —
  `done`/`cancel` are now a raw status flip with no engine teardown.
- **Visit cap.** `onTransit` rejects a bounce-back into `dev` once the task has
  entered `dev` 5 times (`DEV_VISIT_CAP`), so a task that keeps failing
  review/validation parks for a human instead of looping forever. The count is
  the persistent, human-resettable [`metadata.step_visits.dev`](daemon.md#task-metadata).

### How to run `feature-dev`

```bash
# create + enroll in one shot (or: autosk enroll <id> --workflow feature-dev)
id=$(autosk create "Fix the flaky auth test" --workflow feature-dev --json | jq -r .id)

# the auto-spawned daemon picks it up and drives dev → review → … → accept
autosk session list                     # one row per agent run
autosk session transcript <session-id>  # follow a run (or watch live in `autosk lazy`)

# the task parks at `accept` for you; once you're happy, route it through cleanup:
autosk resume "$id" --to cleanup        # tears the worktree down, transits to done
```

If the run keeps bouncing and parks for a human at the visit cap, clear the
counter to let it bounce through `dev` again:

```bash
autosk metadata unset "$id" step_visits
autosk resume "$id" --to dev
```

**Customise it** by copying the extension into `~/.autosk/extensions/` (or your
project's `.autosk/extensions/`) and editing the `piAgent({...})` /
`featureDevWorkflow({...})` calls and the prompts under `prompts/`; a
project/global extension overrides the npm one by name. See
[docs/workflows.md → Customising it](workflows.md#customising-it).

## `@autosk/feature-dev-cc`

The **Claude Code twin** of `feature-dev`: the exact same
`dev → review → docs → validator → accept → cleanup → done` graph (same
bounce-backs, same `dev` visit cap, same per-task `worktreeSandbox()`), but every
agent step is an inline [`@autosk/claude-agent`](#autoskclaude-agent) role
driving `claude -p` instead of a pi role. It registers the **`feature-dev-cc`**
workflow.

| Step | Kind | Notes |
| --- | --- | --- |
| `dev` | `claudeAgent` | first step; implements the task |
| `review` | `claudeAgent` (`effort: xhigh`) | thorough review; bounces back to `dev` |
| `docs` | `claudeAgent` | documentation pass |
| `validator` | `claudeAgent` | independent verification + release hygiene; bounces back to `dev` |
| `accept` | `statusStep("human")` | the engine parks here |
| `cleanup` | `sandboxCleanupStep` | removes the worktree, transits to `done` |

- **Permissions.** Every agent step runs with `dangerouslySkipPermissions: true`
  (`--dangerously-skip-permissions`). The run is unattended — a headless
  permission prompt would abort the turn — and isolated in its per-task worktree,
  so the **worktree is the safety boundary**.
- **Requirements.** `claude` (the Claude Code CLI) on `PATH` or at
  `$AUTOSK_CLAUDE_BIN`, already authenticated; a git repo at the project root.
  **No `autosk` is needed in the run environment** — the `task` / `comment` /
  `transit` tools come from the per-session host HTTP MCP server.

> A Claude **`dockerSandbox`** variant is deferred; the thin operator image
> already lives at `daemon/extensions/claude-agent/docker/` for when it lands.

### How to run `feature-dev-cc`

`feature-dev-cc` is **not** bootstrapped — add it (the add hot-applies to open
projects, no restart):

```bash
autosk ext add npm:@autosk/feature-dev-cc   # or a local checkout path; hot-applies
autosk workflow list                        # feature-dev-cc should appear
autosk create "Fix the flaky auth test" --workflow feature-dev-cc
```

If it does not show up, check `autosk project diagnostics` for a load error
(e.g. an unresolved `@autosk/claude-agent`). See
[docs/extensions.md → When it takes effect](extensions.md#managing-extensions).

## `@autosk/feature-dev-docker`

The **Docker variant** of `feature-dev`: it reuses `featureDevWorkflow()`
verbatim (same graph, bounce-backs, visit cap, and `cleanup` step) but swaps the
default `worktreeSandbox()` for a credential- and git-aware
[`dockerSandbox`](workflows.md#dockersandbox-image--a-per-task-container), so
every agent step runs inside a **per-task `docker run -i --rm` container**
(`ghcr.io/wierdbytes/pi-runtime`) instead of on the host. It registers the
**`feature-dev-docker`** workflow.

- **Thin image, host MCP.** Under a `dockerSandbox` (`thin === true`), the pi
  agent mints a per-session host HTTP MCP server and injects the ack-only
  `autosk_transit` tool; the transport-aware `@autosk/pi-tools` (loaded from the
  mounted `~/.pi`) POSTs `task`/`comment` to it over `host.docker.internal`. The
  image therefore needs neither `autosk` nor a mounted daemon socket.
- **Auth.** pi keeps its provider tokens in `~/.pi/agent/auth.json` (a portable
  file), so the host `~/.pi` is bind-mounted at the container's
  `/home/agent/.pi` (read-write). No export step.
- **Git.** A worktree's `.git` points into `<projectRoot>/.git/worktrees/<id>`,
  so the package also bind-mounts the project `.git` at its identical path (so
  in-container `git`/`go`/`make` resolve the repo).

| Env var | Default | What |
| --- | --- | --- |
| `AUTOSK_PI_DOCKER_IMAGE` | `ghcr.io/wierdbytes/pi-runtime:latest` | image to run |
| `AUTOSK_PI_DIR` | `~/.pi` | host pi config (auth + models) bind-mounted into the container |

### How to run `feature-dev-docker`

```bash
# 1. build (or pull) the pi-runtime image
daemon/extensions/pi-agent/docker/build.sh

# 2. install the extension (hot-applies to open projects, no restart)
autosk ext add npm:@autosk/feature-dev-docker
autosk workflow list                        # feature-dev-docker should appear

# 3. enroll a task
autosk enroll <task-id> --workflow feature-dev-docker
```

Docker isolation here is just `dockerSandbox({ image })` from the bootstrapped
`@autosk/sandbox` — there is no separate isolation-provider extension to add.

## `@autosk/gh-review`

A **single-step review workflow**: enroll a task whose **title carries a GitHub
issue/PR URL** (`https://github.com/<owner>/<repo>/issues/9` or `…/pull/12`) and
a pi agent reviews it inside a per-task
[`dockerSandbox`](workflows.md#dockersandbox-image--a-per-task-container)
container (`ghcr.io/wierdbytes/pi-runtime`, which ships `gh`) — with a
**READ-ONLY** GitHub token. The verdict lands as one structured comment on the
autosk task; nothing is ever written to GitHub.

```text
review ──▶ accept (human) ──▶ cleanup ──▶ done
```

| Step | Kind | What it does |
| --- | --- | --- |
| `review` | `piAgent` (xhigh) | first/only agent step: reads the issue/PR with `gh` (clones the repo inside the container when the diff is not enough), posts the review as a task comment, transits to `accept` |
| `accept` | `statusStep("human")` | parks the task so you can read the review |
| `cleanup` | `sandboxCleanupStep` | removes the per-task worktree/container, transits to `done` |

- **Enroll is validated before any agent runs** (`onTransit`): a task with no
  parsable GitHub URL (title, falling back to the description) is rejected with
  an explanatory comment and stays `new`; so is an enroll when the daemon has no
  gh config.
- **Read-only `gh`.** The operator provisions a fine-grained PAT (Permissions:
  `Contents`, `Issues`, `Pull requests`, `Metadata` — all **Read**; restricted
  to the repos to review) in `~/.autosk/github/ro-token.json`
  (`{ "token": "github_pat_…" }`). The extension reads the file at
  container-start and passes the token as the `GH_TOKEN` env — gh's native
  mechanism: no gh config file exists in the container, so gh never tries to
  rewrite one (gh ≥2.40 rewrites `hosts.yml` on most invocations, which breaks
  a read-only config mount). The token is visible in `docker inspect` while the
  container lives; every write is rejected by GitHub (403) regardless.
- **Thin image, host MCP / git** — same mechanics as `feature-dev-docker` above
  (host MCP over `host.docker.internal`, `~/.pi` mounted rw, the project `.git`
  layered in at its identical path).

| Env var | Default | What |
| --- | --- | --- |
| `AUTOSK_GH_REVIEW_IMAGE` | `ghcr.io/wierdbytes/pi-runtime:latest` | image to run (must ship `gh`) |
| `AUTOSK_GH_DIR` | `~/.autosk/github` | dir holding `ro-token.json` (read fresh per run) |
| `AUTOSK_PI_DIR` | `~/.pi` | host pi config (auth + models) bind-mounted into the container |

### How to run `gh-review`

```bash
# 1. build (or pull) the pi-runtime image (ships gh)
daemon/extensions/pi-agent/docker/build.sh

# 2. provision the read-only gh token (fine-grained PAT, see README)
mkdir -p ~/.autosk/github
echo '{ "token": "github_pat_…" }' > ~/.autosk/github/ro-token.json && chmod 600 ~/.autosk/github/ro-token.json

# 3. install the extension (hot-applies to open projects, no restart)
autosk ext add npm:@autosk/gh-review

# 4. enroll a task whose title carries the URL
autosk create "https://github.com/wierdbytes/autosk/pull/12" --workflow gh-review
# the review comment lands on the task, which parks at `accept`:
autosk resume <id> --to cleanup            # → done
```

## `@autosk/merge-to-current`

A **single-step integration workflow** that merges a task's autosk-managed branch
`autosk/<task-id>` **into the branch you currently have checked out** (whatever
`HEAD` points at), running **non-isolated** in the project's working tree. It is
the v2 port of v1's `merge-to-main`, with the destination changed from the
mainline branch to the current branch — there is no `main`/`master` detection and
no branch switch.

```text
merge ──▶ done      (the branch landed cleanly on the current branch)
   └────▶ human     (rolled back; a human must take over)
```

| Step | Kind | Notes |
| --- | --- | --- |
| `merge` | `piAgent` (`thinking: high`) | first/only step; integrates `autosk/<task-id>` into the current branch |

`done` and `human` are always-available terminal/park targets the agent transits
to directly (there is no `statusStep`). There is **no sandbox** — the `merge`
step runs on the host at the project root and operates on the real working tree,
which is the whole point of a merge step.

**What the `merge` step does** (it drives `git` directly and never touches the
network — no `fetch`/`pull`/`push`):

1. Verifies the task branch `autosk/<task-id>` exists.
2. Auto-commits any pending edits in the task's worktree onto the task branch
   first (these auto-commits are **preserved** even if the merge later rolls back).
3. Snapshots the current branch (`DEST = HEAD`) and **refuses** if HEAD is
   detached, the project root has a dirty tree, or the current branch *is* the
   task branch.
4. Analyses overlap; any non-trivial reconciliation parks for a human before any
   merge work.
5. Fast-forwards (`git merge --ff-only`) when the current branch hasn't moved
   since the fork; otherwise creates a `--no-ff` merge commit (trivial conflict
   resolutions only).
6. On success it lands on the current branch → `done`; on any failure it resets
   the current branch to its starting SHA → `human`. `DEST` is both the merge
   target and the rollback target.

### How to run `merge-to-current`

The branch `autosk/<task-id>` is produced by a worktree-based workflow (e.g.
[`feature-dev`](#autoskfeature-dev)). The typical flow is to merge that work onto
your current branch after acceptance:

```bash
autosk ext add npm:@autosk/merge-to-current   # hot-applies to open projects (no restart)

git switch main                               # check out wherever you want it to land
autosk enroll <task-id> --workflow merge-to-current
```

It refuses on a dirty working tree or a detached HEAD, so commit/stash first. On
rollback your current branch is reset to exactly where it started.

---

# Shipped agents

An **agent** owns a workflow step (it drives a harness and calls `ctx.transit`
exactly once). The two shipped agents are structural twins — same contract,
different harness. You use them two ways:

- **inline in a workflow** — `piAgent({...})` / `claudeAgent({...})` as a step
  value; the step key is the agent name (see the workflows above, and
  [docs/workflows.md → Agent definitions](workflows.md#agent-definitions));
- **as a named chat agent** — each package's default export registers a named
  agent so you can open an [interactive (taskless) chat
  session](daemon.md#interactive-taskless-sessions) against it.

For the `AgentDefinition` / `AgentRunContext` contract both implement, see
[docs/workflows.md → Agent definitions](workflows.md#agent-definitions).

## `@autosk/pi-agent`

Drives [`pi`](https://github.com/earendil-works/pi) (`pi --mode rpc`).
`piAgent({...})` returns an `AgentDefinition` the engine runs for a step: it
spawns pi, injects an `autosk_transit` pi-tool, seeds the step prompt, mirrors
pi's transcript entries 1:1 (streaming in-progress snapshots via `ctx.partial`),
reports assistant usage through `ctx.log.usage()`, observes the transit tool
call, and runs a kickback/corrections loop. It backs
all of [`feature-dev`](#autoskfeature-dev),
[`feature-dev-docker`](#autoskfeature-dev-docker), and
[`merge-to-current`](#autoskmerge-to-current). Full internals:
[package README](../daemon/extensions/pi-agent/README.md).

The agent name is **not** an option — it's the workflow step key. Options
(`PiAgentOptions`):

| Option | Default | Description |
| --- | --- | --- |
| `sandbox` | none (host at project root) | a `Sandbox` deciding where the harness runs and which transport `@autosk/pi-tools` uses |
| `model` | pi default | pi model spec, e.g. `"sonnet:high"` (`--model`) |
| `thinking` | pi default | thinking level `off`…`xhigh` (`--thinking`) |
| `firstMessage` | `""` | inline first-message seed (wins over `firstMessageFile`) |
| `firstMessageFile` | — | path to a file whose contents seed the first message |
| `extraArgs` | `[]` | extra args forwarded verbatim to `pi` |
| `piExtensions` | `[]` | pi extensions to load (`-e <path>` each) |
| `piSkills` | `[]` | pi skills to enable (`--skill <name>` each) |
| `maxCorrections` | `3` | corrective turns before giving up (then the engine parks the task) |
| `piBin` | `$AUTOSK_PI_BIN` or `"pi"` | `pi` binary to spawn |

### Chat with `pi`

The default export registers a named `"pi"` agent (`registry.agent.list` returns
it). Open an [interactive (taskless) chat](daemon.md#interactive-taskless-sessions)
from the **desktop GUI's Sessions panel** (the `＋` action), picking **pi** from
the agent list — that calls `session.create {agent:"pi"}`. The chat spawns
`pi --mode rpc` **without** the transit tool (transit is meaningless in a chat)
and forwards each composer message as a turn. (Interactive chat is a GUI action;
the `autosk` CLI `session` verbs only `list` / `get` / `transcript` / `input` /
`abort` existing sessions.)

## `@autosk/claude-agent`

The structural twin of `pi-agent` driving [Claude
Code](https://docs.anthropic.com/en/docs/claude-code) (`claude -p` headless
stream-json). `claudeAgent({...})` returns an `AgentDefinition`; it backs
[`feature-dev-cc`](#autoskfeature-dev-cc). It is **not** bootstrapped — add it
(`autosk ext add npm:@autosk/claude-agent`) and wire it into your own workflow in
place of `piAgent({...})`. Full internals:
[package README](../daemon/extensions/claude-agent/README.md).

Each Claude `result` event supplies the authoritative per-turn report passed to
`ctx.log.usage()`; a missing or incomplete report is recorded as unavailable.

Its tool surface is the **per-session, host-side HTTP MCP server** the daemon
mints (`ctx.newMCPServer()`), registered with Claude via an inline `--mcp-config`
`type:"http"` server carrying a bearer token. The model sees `mcp__autosk__transit`
(ack-only; the driver drives the real transition), `mcp__autosk__task`, and
`mcp__autosk__comment` — so a thin container needs neither `autosk` nor a mounted
daemon socket.

The agent name is **not** an option — it's the workflow step key. Selected
options (`ClaudeAgentOptions`; full table in the
[README](../daemon/extensions/claude-agent/README.md)):

| Option | Default | Description |
| --- | --- | --- |
| `sandbox` | none (host at project root) | a `Sandbox` deciding where the harness runs |
| `model` | Claude Code default | model alias/name, e.g. `"sonnet"` / `"opus"` (`--model`) |
| `effort` | Claude Code default | effort level (`--effort`): `low`…`max` (model-dependent) |
| `firstMessage` / `firstMessageFile` | `""` / — | first-message seed (inline wins over file) |
| `permissionMode` | `"acceptEdits"` | unattended permission mode (`--permission-mode`); must be non-interactive-safe |
| `dangerouslySkipPermissions` | `false` | skip all permission prompts (`--dangerously-skip-permissions`); wins over `permissionMode` |
| `bare` | `false` | `--bare` for hermetic runs (skip project `CLAUDE.md` / `.mcp.json` / hooks) |
| `autoskTools` | `true` | register the `autosk` MCP server (`false` omits it → no transit → the run parks) |
| `maxCorrections` | `3` | corrective turns before giving up |
| `claudeBin` | `$AUTOSK_CLAUDE_BIN` or `"claude"` | `claude` binary to spawn |

**Requirements:** `claude` on `PATH` (or `$AUTOSK_CLAUDE_BIN`), already
authenticated (the headless run is unattended).

### Chat with `@autosk/claude-agent`

The default export registers a named `"@autosk/claude-agent"` agent. Open a chat
the same way — the GUI's Sessions panel `＋`, picking **@autosk/claude-agent** —
and its interactive chat drops the `transit` tool but keeps `task` / `comment`.

---

# Tutorial: drive a task through `feature-dev`

This walks a single task through the bootstrapped reference workflow end to end —
from creation, through the agent steps, to acceptance and cleanup. By the end
you'll have watched the daemon run a multi-step pipeline and landed the result on
a preserved git branch.

## What you'll need

- `autosk` and `autoskd` built (`make build` and `make build-autoskd`).
- A project directory that is a **git repo** with at least one commit (the
  worktree sandbox forks from `HEAD`).
- pi authenticated and on `PATH` (`feature-dev`'s agents drive `pi --mode rpc`).
- `jq` (only for the JSON snippets below — optional).

The first time you run any `autosk` command on a brand-new machine, the daemon
auto-spawns and provisions `@autosk/feature-dev` from npm (the
[first-run bootstrap](extensions.md#first-run-bootstrap)). Confirm it's there:

```bash
cd /path/to/your/git/project
autosk workflow list        # feature-dev should be listed
```

## Step 1: Create and enroll a task

One command creates the task and enrolls it at the workflow's `firstStep`
(`dev`). The auto-spawned daemon picks it up immediately.

```bash
id=$(autosk create "Add a --version flag" --workflow feature-dev --json | jq -r .id)
echo "$id"          # ask-XXXXXX
```

The task is now in `work` status and the `dev` agent is running in a fresh git
worktree on branch `autosk/$id`.

## Step 2: Watch it run

Each step is one **session**. List them, or follow a transcript live:

```bash
autosk session list                       # one row per agent run, newest first
autosk session transcript <session-id>    # the pi-format transcript for a run
```

For a live, scrolling view of the transcript as it streams — plus the task,
session, and workflow panes — open the TUI:

```bash
autosk lazy
```

The daemon drives `dev → review → docs → validator`, bouncing back to `dev` from
`review`/`validator` if something's off (capped at 5 `dev` entries).

## Step 3: Accept, then clean up

When the pipeline reaches `accept`, the engine **parks the task to `human`** and
waits for you. Inspect the work (the branch `autosk/$id` holds the commits), then
route the task through `cleanup` to finish:

```bash
autosk show "$id"                  # status: human, step: accept
autosk resume "$id" --to cleanup   # tears the worktree down, transits to done
```

## What you built

You drove a task through a six-step workflow without writing any workflow code:
the daemon scheduled each agent, isolated every step in a per-task git worktree,
enforced a visit cap, parked for your acceptance, and tore the worktree down on
the way to `done` — leaving the work on branch `autosk/$id` for review or
merge.

```bash
autosk show "$id"                  # status: done
git branch --list "autosk/$id"     # the branch is preserved
```

**Next steps:**

- Land that branch on your current branch with
  [`merge-to-current`](#autoskmerge-to-current).
- Swap pi for Claude Code with [`feature-dev-cc`](#autoskfeature-dev-cc), or run
  each step in a container with
  [`feature-dev-docker`](#autoskfeature-dev-docker).
- Build your own pipeline:
  [docs/workflows.md → Make your own workflow](workflows.md#make-your-own-workflow),
  or follow the guided [extension tutorial](extensions-tutorial.md).

## Related

- [docs/workflows.md](workflows.md) — the `WorkflowDefinition` / `AgentDefinition`
  / `Sandbox` contracts and `onTransit` semantics.
- [docs/extensions.md](extensions.md) — discovery, precedence, `autosk ext`
  management, and the first-run bootstrap.
- [docs/daemon.md → Interactive sessions](daemon.md#interactive-taskless-sessions)
  — the taskless chat lifecycle the named agents back.
- [docs/concepts.md](concepts.md) — the task model, status machine, and
  `metadata.step_visits`.
- Package READMEs:
  [feature-dev](../daemon/extensions/feature-dev/README.md) ·
  [feature-dev-cc](../daemon/extensions/feature-dev-cc/README.md) ·
  [feature-dev-docker](../daemon/extensions/feature-dev-docker/README.md) ·
  [merge-to-current](../daemon/extensions/merge-to-current/README.md) ·
  [pi-agent](../daemon/extensions/pi-agent/README.md) ·
  [claude-agent](../daemon/extensions/claude-agent/README.md).
