![autosk: the lazy TUI, the desktop GUI, and the mobile web view](docs/hero.png)

## What is autosk?

1. **task tracker**: small, local, file-based. Tasks live as files under
   `.autosk/` in your repo. You can use it to scope agent attention to concrete
   context.
2. **workflow engine**: each workflow is a directed graph of **steps**, and each
   step is owned by an **agent** managed by the daemon. Workflows and agents are
   **code** registered by extensions.
3. **interface**: a convenient way to manage and observe the system — the
   `autosk` CLI, the `autosk lazy` TUI, and the desktop/mobile GUI.

You can stop at step 1 if all you want is a backlog. Step 2 is opt-in.

Inspired by:
- [beads](https://github.com/steveyegge/beads) — but simpler and more flexible
- [pi.dev](https://pi.dev) — for its approach to extensibility
- [sandcastle](https://github.com/mattpocock/sandcastle) — for its programmatic approach to workflows

```bash
$ autosk create "Wire up the auth flow"
ask-3f9b2c

$ autosk enroll ask-3f9b2c --workflow feature-dev
# the daemon picks it up, runs the agent pipeline, and returns to you when done or parked
```

> ### A clean break from v1
>
> autosk v2 stores tasks as **files** under `.autosk/` and is driven by the
> `autoskd` daemon. It does **not** read the old `.autosk/db` database, and
> there is **no migrator**. If you have an existing v1 project, keep using
> the last v1 release — **[`v0.1.6`](https://github.com/wierdbytes/autosk/releases/tag/v0.1.6)**
> — to open it; v2 treats a directory as a fresh project. Workflows and agents
> are now code (extensions), not database rows or installed npm-package agents.

## Prerequisites

- **Install autosk:**

  - **macOS — Homebrew cask** (installs the desktop GUI **and** puts `autosk` +
    `autoskd` on `PATH`):
    ```bash
    brew install --cask wierdbytes/autosk/autosk
    ```
    The cask is signed + notarized (Apple Silicon); the GUI bundles the `autosk`
    CLI/TUI and the `autoskd` daemon as sidecars, so a Finder launch auto-spawns
    the embedded daemon with no shell `PATH` dependency.

  - **Linux** — grab the `autosk` + `autoskd` binaries (and the GUI
    `.AppImage`/`.deb`, if you want the desktop app) from the
    [latest GitHub Release](https://github.com/wierdbytes/autosk/releases/latest).
    Linux is **not** served via Homebrew.

  - **From source** (any platform):
    ```bash
    make install
    ```

  Every path installs **both** binaries: the `autosk` CLI/TUI and the `autoskd`
  daemon it auto-spawns. The `feature-dev` workflow is fetched from npm on first
  run (see below).

- **[pi.dev](https://pi.dev)** — installed and configured for at least one LLM
  provider (the shipped agents drive `pi --mode rpc`).

- **Node.js 22+ (with `npm`)** — needed on first run so the daemon can install
  the default `@autosk/feature-dev` workflow into `~/.autosk/packages/`, and
  whenever you add other **npm-packaged extensions** (listed in `settings.json`).
  Your own local `.autosk/extensions/*.ts` need nothing extra; the daemon runs
  them in-process.

## Quick start

### [Lazy mode](docs/lazy.md)

Tasks, sessions, workflows, and agents in one screen. Selecting a session streams its transcript live into the Detail pane.

A TUI for easy manipulation and observability. Or go further and use the CLI (see below).
```bash
cd ~/your/project
autosk lazy
```
Here you can press `n` to create a new task or `?` to see hotkeys.

### [Desktop GUI](gui/README.md)

A native desktop app (Tauri: React/Vite UI + Rust backend) at feature parity
with `autosk lazy` — projects in a sidebar, a live session transcript, and a
state-aware composer (steer / follow-up / abort, comment / resume, enroll).
The Tauri backend is a **pure JSON-RPC client of `autoskd`**; the front end is
transport-agnostic and runs in either mode:

- **Local** — connects over the Unix-domain socket and **auto-spawns** `autoskd`
  when it isn't already running. Zero configuration, exactly like `autosk lazy`.
- **Remote** — dials a configured `host:port` and authenticates with a token
  (first request is `meta.auth{token}`). The remote `autoskd` must be running
  explicitly — you can't auto-spawn a process on another host. Set the mode and
  host/token in the in-app **Settings** view.

Run it from a checkout:

```bash
cd gui
npm install
npm run tauri:dev     # launch the desktop app (needs a display + webkit)
```

See [`gui/README.md`](gui/README.md) for the architecture, the IPC chokepoints,
and the full script list. To build and install a **release** build on desktop,
iPad, or iPhone (a compact single-pane layout), see
[docs/gui-release.md](docs/gui-release.md).

### CLI

The `autosk` CLI is the scriptable front end — every verb, flag, env var, and a
few scripting recipes are in **[docs/cli.md](docs/cli.md)**. A taste:

1. **Create your first task.** using the CLI:
   ```bash
   cd ~/your/project
   autosk create "Tidy the README"
   autosk list             # everything that's open
   autosk ready            # what should I work on right now?
   autosk done ask-3f9b2c  # mark it finished
   ```

   The first write verb in a fresh directory prompts you to create `.autosk/`.
   Press `Enter` (or `y`) to accept; press `n` to abort. `autosk init` is the
   explicit form and is idempotent. Scripts and CI auto-accept silently — set
   `AUTOSK_AUTOINIT_ASSUME_YES=1` (or disable the behaviour entirely with
   `AUTOSK_NO_AUTOINIT=1`). The same prompt fires when you launch `autosk lazy`
   for the first time in a fresh project. There is no database and no per-project
   workflow seeding — the `feature-dev` workflow is provisioned once (on the
   daemon's first run) and is then available to every project.

2. **(Optional) Hand a task to the developer workflow.** The `feature-dev`
   workflow (`dev → review → docs → validator → accept`) is installed from npm on
   the daemon's first run and is available in every project, so all that's left
   is to enroll a task:
   ```bash
   id=$(autosk create "Fix the flaky test" --workflow feature-dev --json | jq -r .id)
   ```
   The daemon — `autoskd`, auto-spawned on first use (there is no manual `serve`
   step) — picks up the task, runs the workflow, and either closes it to `done`
   or parks it to `human` for review.

   Follow that task from a terminal or another agent with a human timeline or
   a JSON Lines stream:
   ```bash
   autosk watch "$id"
   autosk watch "$id" --json
   ```

   `feature-dev` runs each agent step in its own git worktree (a per-task
   `worktreeSandbox()`), so the project root must be a git repo; a final
   `cleanup` step tears the worktree down on the way to `done`.

3. **(Optional) Use your own workflow.** Drop a TypeScript extension into
   `~/.autosk/extensions/` (or your project's `.autosk/extensions/`) that
   registers a workflow (its agents are inline step values), then enroll into
   it:
   ```bash
   # ~/.autosk/extensions/mine.ts registers a workflow named my-flow
   id=$(autosk create "Fix the flaky test" --json | jq -r .id)
   autosk enroll "$id" --workflow my-flow
   ```
   To pull in a published or local extension package instead, use
   `autosk ext add npm:@scope/pkg` (or `autosk ext add ./my-ext`); add
   `-l/--local` to scope it to the current project, and `autosk ext list` /
   `autosk ext remove` to inspect or drop entries, or `autosk ext update` to
   bump floating npm extensions to their latest registry version. New to
   extensions? The [extension tutorial](docs/extensions-tutorial.md) builds one
   from scratch and the [Claude Code workflow tutorial](docs/extensions-tutorial-claude.md)
   wires in a real agent; [docs/extensions.md](docs/extensions.md) is the full
   contract, and [docs/workflows.md](docs/workflows.md) covers full workflows.

## How it works

autosk has four moving parts. You only need to touch them as you grow into them.
For the full mental model, see [docs/concepts.md](docs/concepts.md).

### Tasks

Tasks live as files under `.autosk/` inside your repo
(`tasks/<id>/task.json` + `comments.jsonl`). Each one has:

- An **id** like `ask-3f9b2c` and a **title**.
- A **status**: `new` (open work), `work` (an agent is on it), `human` (waiting for a person), `done`, or `cancel`.
- Optional **blockers** — `autosk block <id> <blocker-id>` makes a task wait for another.
- A free-form **metadata** bag — `autosk metadata show/set/unset <id> …` reads and edits it; the engine keeps each workflow's visit counts under the reserved `step_visits` key (resettable by hand).

`autosk ready` returns the *ready set*: tasks in `new` status with no open blocker. That's what humans and agents pull from.

### Agents

An **agent** owns a task step. AI agents are **code** defined **inline** in a
workflow's steps by [extensions](docs/extensions.md) — the npm-published
`@autosk/pi-agent` drives `pi --mode rpc`, its twin `@autosk/claude-agent` drives
Claude Code (`claude -p` headless stream-json), and you can write your own. There is
no install step: a step whose value is an `AgentDefinition` (it has an `onRun`)
is an agent step, and the **step key is the agent's name**, so registering a
workflow registers its agents — no per-step registry needed. A
`statusStep("human")` is the only non-agent step (a human gate). (Extensions can
also publish a **named** agent via `registerAgent` to back an [interactive chat
session](docs/daemon.md#interactive-taskless-sessions) — a taskless conversation
that is not part of any workflow.)

### [Workflows](docs/workflows.md)

A **workflow** is a directed graph of **steps**, where each step has an agent and
one or more outgoing transitions. Workflows can be as small as *one step, one
agent*, or as branchy as *developer → reviewer → either back to developer or on
to validator*.

Workflows are **code** registered by extensions — you write a
`WorkflowDefinition` and the daemon drives it. `autosk workflow` is a read-only
view:

```bash
autosk workflow list          # workflows registered by this project's extensions
autosk workflow show feature-dev
```

The daemon installs `feature-dev` (`dev → review → docs → validator → accept
→ cleanup → done`, each step in a per-task `worktreeSandbox()`) from npm on first
run and makes it available to every project. For a one-off
agent, register a tiny workflow with a single agent step (plus a terminal
`statusStep`).

The catalog of ready-to-use workflows and agents that ship as `@autosk/*`
packages — `feature-dev` and its Claude Code / Docker twins, the
`merge-to-current` integration step, and the `pi`/`claude` agents — is documented
in [docs/shipped.md](docs/shipped.md). See
[Make your own workflow](docs/workflows.md#make-your-own-workflow) to adapt one
for your dev pipeline, and [docs/extensions.md](docs/extensions.md) for how
extensions are discovered and loaded.

### The [daemon](docs/daemon.md)

The daemon — `autoskd`, a Bun/TypeScript program compiled to a standalone binary
— is a long-running process that drives tasks through their workflows and owns
the `.autosk/` directory. It is **auto-spawned on first use**; for a foreground
daemon, run `autoskd` directly. **One daemon per host serves any number of
projects** — it picks the project from the `{cwd}` each request carries.

What the daemon does for each task in `work` status:

1. Resolves the current step's agent (code, from the project's extension registry).
2. Runs the agent's `onRun` in a **session** at the project root. Isolation is
   the agent's concern, not the engine's: a step's agent may wrap its harness in
   a [sandbox](docs/workflows.md#isolation-agent-owned-sandboxes) (a git worktree
   or a container) and run it there.
3. Streams the agent's pi-format transcript to `.autosk/sessions/<id>.jsonl` and
   to any attached viewer (`autosk lazy`, the GUI).
4. Follows the transition the agent commits (`ctx.transit`) — a sibling step or a
   terminal/park status.

If the agent fails to transition cleanly, the daemon parks the task to `human` and waits for you to resume it (`autosk resume <id>`).

Want to see all of this for yourself? The [daemon tutorial](docs/daemon-tutorial.md)
walks you through running your own daemon, watching a live session + transcript,
serving two projects from one daemon, and exposing it over TCP for the GUI.

```bash
autosk session list                  # one row per agent run in this project
autosk session get <id>
autosk session transcript <id>
autosk session abort <id>            # abort a live session
autosk project diagnostics           # extension load errors for this project
```

## License

MIT — see [LICENSE](LICENSE).
