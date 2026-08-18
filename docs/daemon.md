# `autoskd` — the daemon

`autoskd` is the long-running process that drives tasks through their workflows.
It is the **sole owner** of a project's `.autosk/` directory and the single
authority on task state. Every front end — the `autosk` CLI, the `autosk lazy`
TUI, and the Tauri desktop GUI — is a thin **JSON-RPC client** of `autoskd`;
none of them open or write `.autosk/` directly.

`autoskd` is a **Bun/TypeScript** program (it lives in [`daemon/`](../daemon)).
For distribution it is compiled to a standalone binary with `bun build
--compile`, which embeds the Bun runtime — **no global `bun` is needed at
runtime**.

> **New here?** [docs/daemon-tutorial.md](daemon-tutorial.md) is the
> learn-by-doing companion: run your own daemon, drive a task through a live
> session + transcript, serve two projects from one daemon, and expose it over
> TCP for the GUI.

> **Clean break from v1.** `autoskd` stores tasks as **files** under `.autosk/`.
> It does **not** read the old `.autosk/db` database; there is no migrator. A
> project that still has a v1 `.autosk/db` must be opened with the last v1
> release (`v0.1.6`). See [the README clean-break note](../README.md#a-clean-break-from-v1).

## What lives in `.autosk/`

A project is just a directory that contains an `.autosk/` folder, resolved by
walking up from your current directory. There is **no database** — everything is
plain files the daemon writes atomically (tmp + rename):

```
.autosk/
  tasks/<id>/task.json        # one task: title, description, status, workflow, step, blocked_by, metadata, timestamps
  tasks/<id>/comments.jsonl   # the task's comments (one JSON object per line)
  sessions/<session-id>.json  # session meta (one agent run for one step)
  sessions/<session-id>.jsonl # the session transcript (pi-format; see "Sessions" below)
  extensions/                 # (optional) project-local extensions: workflows + agents as code
  settings.json               # (optional) extension entries to load (npm:<spec> or a local path)
```

Because tasks are files, you can read or hand-edit them. The daemon picks up
external edits via a startup scan + filesystem watcher (see [Hybrid file
ownership](#hybrid-file-ownership) below). The registry of known projects lives
at `~/.autosk/projects.json`. For the task model, status machine, blockers, and
`metadata` that these files hold, see [docs/concepts.md](concepts.md).

## Running it

### Auto-spawn (the normal path)

You almost never start `autoskd` by hand. The first time a front end needs the
daemon, it **auto-spawns** one and waits for it to come up:

1. resolve the `autoskd` binary: `$AUTOSKD_BIN` → a sibling of the calling
   `autosk` binary → `autoskd` on `PATH`;
2. spawn `autoskd serve --sock <sock>` detached;
3. connect over the Unix socket once it is listening.

`autoskd`'s single-instance binding makes a double-spawn harmless: a second
launcher that loses the bind race exits `0` and the client uses the daemon that
won. The Homebrew **cask** (and `make install`) put `autoskd` right next to
`autosk` on `PATH`, so auto-spawn works with zero configuration.

### Foreground / explicit

To run a daemon yourself (e.g. as a service, or to watch its logs), run the
binary directly:

```bash
autoskd                      # serve on the default socket + TCP 0.0.0.0:7077
autoskd serve --sock /tmp/autosk.sock
autoskd serve --tcp 0.0.0.0:7777     # override the default TCP address (token auth, see below)
autoskd serve --workers 8            # worker-pool size (default 4)
```

`serve` is the default verb (there is one other, non-default verb —
[`autoskd mcp`](#the-autoskd-mcp-tool-server)). `serve` flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--sock PATH` | `$AUTOSK_SOCK` → `~/.autosk/daemon.sock` | Unix-domain socket path. |
| `--tcp [HOST:]PORT` | `0.0.0.0:7077` | TCP listener address (token auth). Bare `PORT` binds `127.0.0.1`. Pass this to change host/port; TCP is on by default. |
| `--workers N` | `4` | Size of the global FIFO worker pool (shared across all projects). |

Environment knobs:

| Variable | Meaning |
| --- | --- |
| `AUTOSK_SOCK` | UDS path (when `--sock` is not passed). |
| `AUTOSK_IDLE_SECS` | Idle-shutdown window in seconds (default `1800`; `0` or negative disables; ignored in TCP mode). |
| `AUTOSK_TOKEN_FILE` | Path to the TCP auth token file (default `~/.autosk/daemon-token`). |
| `AUTOSK_NPM_BIN` | `npm` binary used for every extension install — the first-run bootstrap, the auto-install reconcile, the registry version check + re-install behind `autosk ext update`, and an explicit `autosk ext add npm:<spec>` (default `npm` on `PATH`). |
| `AUTOSK_NO_AUTO_INSTALL` | When set (to any value other than empty / `0` / `false`), disables the automatic first-run bootstrap *and* the reconcile pass; explicit `autosk ext add` / `autosk ext update` still work. |
| `AUTOSK_SKIP_SHELL_PATH` | When set (to any value other than empty / `0`), skips the startup login-shell `PATH` probe (see below). |
| `AUTOSKD_BIN` | (front-end side) explicit path to the `autoskd` binary for auto-spawn. |

#### Login-shell `PATH` enrichment

A daemon launched from a macOS `.app` bundle (or any Finder/launchd/GUI context)
inherits the **minimal launchd `PATH`** — `/usr/bin:/bin:/usr/sbin:/sbin` — which
omits Homebrew (`/opt/homebrew/bin`), nvm, asdf, pyenv, and friends. Every child
the daemon then spawns breaks with *command not found*: `git worktree add` shells
out to `git-lfs`, the first-run bootstrap to `npm`, the Docker sandbox to
`docker`, and the agent steps to `pi` / `claude`.

To prevent this, at startup the daemon asks the operator's **login shell**
(`$SHELL -ilc`, so it sources `.zprofile` *and* `.zshrc`) for its real `PATH`
and merges any missing directories into its own `process.env.PATH` (login-shell
entries take precedence, matching the terminal). The probe is bounded by a 2s
timeout and is a no-op when it finds nothing new — so a terminal-launched daemon
(already-rich `PATH`) is unaffected. It is skipped on Windows, under `bun test`,
and whenever `AUTOSK_SKIP_SHELL_PATH` is set.

### One daemon per host, many projects

A single `autoskd` serves **any number of projects**. Each request carries a
`{cwd}` selector; the daemon walks up from that cwd to find the project's
`.autosk/`, opens it lazily (file store + extension registry + scheduler), and
keeps it loaded. The worker pool is global and FIFO across every loaded project.

### The `autoskd mcp` tool server

`autoskd mcp` is a second, **non-default** verb: a minimal **stdio MCP** (Model
Context Protocol) server that exposes autosk's tools to a CLI harness that speaks
MCP. It is a standalone tool surface for external harnesses that speak MCP over
stdio. (The shipped [`@autosk/claude-agent`](workflows.md#agent-definitions) no
longer uses this stdio path — it points Claude Code at a per-session, host-side
HTTP MCP server minted by [`ctx.newMCPServer()`](workflows.md#the-host-side-mcp-server-ctxnewmcpserver),
so a sandboxed harness needs neither `autosk` nor a mounted socket.) It speaks
JSON-RPC 2.0 over stdio
(`initialize` → `tools/list` → `tools/call`), binds **no** socket, and returns
when stdin closes. It is hand-rolled (no `@modelcontextprotocol/sdk` dependency),
so it bundles into the compiled `autoskd` binary with no extra runtime.

It advertises up to three tools (the harness sees them prefixed `mcp__autosk__`):

- **`transit`** — *ack-only*, advertised only when `AUTOSK_MCP_TRANSIT=1` (task
  mode). It returns an immediate ack; the agent driver observes the call and
  drives the real `ctx.transit`.
- **`task`** — `create` / `update` / `show` / `list`.
- **`comment`** — `add` / `list` (`add` defaults the author to `AUTOSK_AGENT`).

`task` / `comment` **execute for real** by shelling out to `autosk … --json`
(not an embedded RPC client): the CLI already centralizes the project
(`AUTOSK_CWD`), socket (`AUTOSK_SOCK`), and author (`AUTOSK_AGENT`) resolution,
which the spawning agent bakes into the `--mcp-config` env block. `autosk` must
be on `PATH` (or `$AUTOSK_BIN`); a missing binary returns a clear actionable
error, not a silent failure.

The **same three tools** (`transit` / `task` / `comment`) are also served by the
daemon's other MCP surface — the [per-session host HTTP server](#the-per-session-host-http-mcp-server)
below — over a different transport (HTTP, not stdio) and with a *direct-store*
backend instead of the `autosk` shell-out. One tool surface, two transports.

### The per-session host HTTP MCP server

The `autoskd mcp` stdio server is for **external** harnesses you wire up
yourself. The shipped agents get the same tools over a different transport,
minted by the engine — you never start this one by hand: on every run-start the
engine mints a **per-session, host-side HTTP MCP server** behind
`ctx.newMCPServer()` (the agent-facing SDK detail is in
[workflows.md → The host-side MCP server](workflows.md#the-host-side-mcp-server-ctxnewmcpserver)).
It is the reachability seam for a **sandboxed** harness — a Claude Code process
in a container, or a `pi` run in a worktree — that cannot see the daemon's Unix
socket and may not have `autosk` on `PATH`.

It is a hand-rolled `Bun.serve()` Streamable-HTTP endpoint (again no
`@modelcontextprotocol/sdk`, so it survives `bun build --compile`):

- **Ephemeral port, bound `0.0.0.0`.** It binds `port: 0` on all interfaces so a
  Linux container reaching the host over the docker bridge
  (`host.docker.internal`) connects — a loopback-only bind would refuse it. The
  returned `url` still reads loopback (`http://127.0.0.1:<port>`) for the
  host/worktree case.
- **Bearer-token auth.** Each server mints a fresh 32-byte token; a `POST` whose
  `Authorization: Bearer <token>` is wrong or missing is a hard `401`. The
  **token + ephemeral port are the security boundary**, not the bind interface.
- **Stateless `POST` → JSON.** One JSON-RPC request per `POST`, one response as
  `application/json`; no `Mcp-Session-Id`.
- **Direct-store backend.** Where the stdio server shells out to `autosk … --json`,
  this runs `task` / `comment` straight against the daemon's own store (and the
  engine for a workflow enroll) with **no `autosk` child** — so a thin container
  image needs neither the CLI nor a mounted socket. Results are byte-identical to
  the shell-out path. `transit` stays ack-only (advertised in task mode), exactly
  as on the stdio server.

The agent gets back `{ url, port, token, close }`, rewrites the host for its
isolation topology with `sandbox.endpointFor(port)`, and hands the harness the
bearer. `close()` is an explicit early release; the engine is the backstop owner
and closes every minted server on each settle / finaliser / detach, so a
forgetful agent never leaks a port across steps.

## What the daemon does for each task

The engine has exactly one scheduling rule:

> a task in `status=work` whose current step is an **agent step** (not a
> `statusStep`) and has no live session ⇒ start a **session** that runs that
> agent's `onRun`.

For each such task:

1. Create a session (`sessions/<id>.json` + `.jsonl`) and run the agent's
   `onRun` on the worker pool, at `ctx.cwd = projectRoot`. The agent writes
   pi-format transcript entries as it works. **Isolation is the agent's concern,
   not the engine's:** a step's agent may wrap its harness in a
   [sandbox](workflows.md#isolation-agent-owned-sandboxes) (a git worktree or a
   container) and run it there, but the engine knows nothing about it.
2. On run start the engine mints a [per-session, host-side HTTP MCP
   server](#the-per-session-host-http-mcp-server) (`ctx.newMCPServer()`) for a
   sandboxed harness's tool surface, and closes it on every settle / finaliser /
   detach so no port leaks across steps.
3. The agent must call `ctx.transit(target)` exactly once — a sibling step, or a
   terminal/park status (`done` / `cancel` / `human`). `transit` validates the
   target through the workflow's `onTransit` hook, atomically updates `task.json`,
   and emits notifications. (A `task.done` / `task.cancel` RPC is a raw status
   flip with no engine teardown — a sandbox is torn down by a
   [cleanup step](workflows.md#cleanup-is-a-workflow-step), not the terminal.)
4. If `onRun` returns **without** transiting, the session fails
   (`error="agent_did_not_transit"`) and the task is parked to `human`.

The loop is **event-driven** (it re-scans on every transit) with a slow safety
rescan as a backstop — there is no 2-second polling.

**Crash recovery.** On startup, any session left `running`/`queued` by a previous
daemon is sealed `failed: daemon_restart` and its task is parked to `human` —
interrupted work is never silently resumed.

## Sessions & transcripts

One invocation of an agent's `onRun` for one task step = one **session**.
Sessions replace v1's "jobs".

- **Meta** (`sessions/<id>.json`): `{ id, kind: task|interactive, task_id,
  workflow, step, agent, status: queued|running|done|failed|aborted,
  activity?: idle|busy, error?, started_at, ended_at, usage? }`. Terminal
  `usage` is the aggregate of authoritative `ctx.log.usage()` reports, or `null`
  when a complete total is unavailable. `activity` is the live
  **turn** state (orthogonal to the lifecycle `status`): `busy` while the agent
  is streaming a turn, `idle` when it is waiting for the next user message. It is
  set for interactive (chat) sessions only and is absent on task sessions and
  once a session is terminal. A `task` session is created by the scheduler for a
  workflow step; an `interactive` session is a taskless chat (see [Interactive
  sessions](#interactive-taskless-sessions) below) whose
  `task_id`/`workflow`/`step` are the empty-string sentinel (`""`).
- **Transcript** (`sessions/<id>.jsonl`): a line 1 header followed by typed
  entries, in a format that **deliberately mirrors pi's session format** so
  pi-based agents can pipe pi entries through verbatim and pi renderers stay
  reusable:
  - **`message` entries** — pi's message schema (`role`, `content[]` blocks incl.
    `text` / `thinking` / `toolCall` / `image`, `usage`/`cost`, `stopReason`).
  - **`custom` entries** — the generic agent logging channel.
  - **engine structural entries** — autosk-specific custom types the engine
    emits itself: `autosk:transit`, `autosk:steer`, `autosk:error`,
    `autosk:session_end`, and canonical per-turn `autosk:usage` reports — so a
    transcript is self-contained.

There is **no retention/GC** in this version: session files accumulate, and
cleanup is manual (`rm .autosk/sessions/…`).

### Streaming partial messages

While an agent streams a model turn, the daemon can push the **in-progress**
assistant message live, before it commits the durable transcript line. This is
the `session-event` `kind:"partial"` frame (carried in `partial?:
TranscriptMessage` on the wire). It rides the same per-session
`session.subscribe` replay-then-tail subscription as committed lines, so a client
renders a growing assistant bubble (text / thinking / tool-call blocks) as the
turn is produced — today the Tauri GUI does this; `autosk lazy` only tolerates
(ignores) the frame for now.

The frame is deliberately minimal and **ephemeral**:

- **Cumulative snapshot.** Each frame carries the full current message snapshot,
  not a delta. A client just *replaces* its current partial — idempotent,
  loss-tolerant, and correct for a client that joins mid-stream (it replays the
  committed lines, then receives the next whole snapshot, with no delta backlog
  to reconstruct). The producer coalesces frames (~40 ms) so the rate is bounded.
- **Never persisted.** A partial is **not** written to `.jsonl`, carries no
  `line`, and does **not** advance the subscription's monotonic line cursor. The
  eventual `message_end` commits the one durable line exactly as before, and that
  committed line supersedes (and clears) the live bubble.
- **Ordered against commits.** Partial emission is funnelled through the same
  serial transcript chain as durable appends, so a partial of message *N+1* can
  never overtake the commit of message *N*.
- **Per-session only.** Partials are delivered to per-session subscribers and are
  excluded from the project-scope `session-changed` broadcast (which carries only
  the `status`/`done`/`error` lifecycle frames).

An agent produces partials via the SDK's ephemeral `ctx.partial(message)` (see
[docs/workflows.md → The run context](workflows.md#the-run-context)); the shipped
`@autosk/pi-agent` wires pi's `message_update` events into it.

You can steer or abort a **live** session:

- `session.input {kind: "steer"|"followup"}` injects a message into the running
  agent (if the agent supports it);
- `session.abort` fires the session's `AbortSignal`, seals the meta `aborted`,
  and parks the task to `human`.

## Interactive (taskless) sessions

Not every session belongs to a task. An **interactive session** is a taskless
chat you open directly against a registered agent (the GUI's Sessions panel `＋`
button) and drive turn-by-turn — there is no workflow, no synthetic task, and no
`ctx.transit`. It reuses the same `Session` entity, transcript format, and
steer/subscribe surface as a task session; only `kind: "interactive"` and the
`""` sentinels for `task_id`/`workflow`/`step` distinguish it.

**Agent registry.** An extension publishes a named agent with
`AutoskAPI.registerAgent({ name, description?, agent })` (see
[docs/workflows.md → Named agents](workflows.md#named-agents--interactive-sessions)).
`registry.agent.list` returns every registered agent; the GUI picker lists them.
The shipped `@autosk/pi-agent` registers a `"pi"` agent (chat backed by
`pi --mode rpc`).

**Lifecycle.**

1. `session.create {agent}` resolves the named agent (unknown name → invalid
   params), creates a `kind:interactive` session with `cwd = projectRoot`, and
   dispatches it **directly** — interactive sessions run **off**
   the bounded task-worker pool, so an idle chat never occupies a slot a task
   session needs, and the scheduler is never involved.
2. The session opens **empty** (no first prompt); the first composer message
   starts the first turn. `session.input {kind:"followup"}` delivers each turn —
   idle → a fresh turn, streaming → a mid-turn follow-up.
3. `session.end` winds the agent down gracefully and seals the session `done`
   (distinct from `session.abort`, which seals `aborted`). Neither parks a task —
   there is none. An interrupted interactive session is sealed
   `failed: daemon_restart` on the next daemon start (again, no park); v1 does
   **not** auto-resume it.

While a chat is live the agent reports its **turn activity** via `ctx.setActivity`
(`busy` on the turn's `agent_start`, `idle` on `agent_end`). The runtime writes it
to `meta.activity` and pushes a `status` session-event / `session-changed`, so a
client can show *idle* (waiting for you) vs *working* (streaming a turn) without
the lifecycle `status` ever leaving `running`. The GUI renders this as the
session badge: `idle` / `working` instead of a bare `running`.

A live interactive session counts as pending work, so an idle (waiting-for-user)
chat keeps the daemon from idle-shutting-down until the chat is ended or aborted.
(Interactive sessions run off the worker pool, so they are **not** reflected in
`meta.healthz`'s `running` counter, which reports task-pool jobs only.)

## Hybrid file ownership

The daemon is the writer for all RPC-driven mutations, but it also honours
external (human/script) edits picked up by its startup scan + fs watcher:

- external edits to `title` / `description` / `blocked_by` / `metadata` /
  comments are accepted as-is;
- external edits to `status` / `step` / `workflow` of a task **with a live
  session** are rejected (the file is rewritten from engine state and a warning
  is logged) — the engine owns enrolled tasks.

Because `metadata` is human-editable, hand-editing (or `autosk metadata unset`)
the reserved `step_visits` counter is the supported escape hatch for a workflow
visit cap (last-writer-wins against a concurrent engine bump). See [Task
metadata](#task-metadata) below.

## Task metadata

Every task carries a free-form `metadata` object in `task.json` — an opaque,
human-editable key/value bag (always present, an empty object `{}` when the task
has none). On disk the key is **omitted entirely when empty**, so pre-metadata
`task.json` files round-trip byte-for-byte; a non-empty bag serialises in a fixed
slot (after `blocked_by`, before `created_at`). A missing / corrupt / non-object
`metadata` parses defensively to `{}`.

The daemon treats the bag as opaque data **except** for one reserved sub-object,
`step_visits` (a `step name → entry count` map) that the engine auto-maintains
for workflow visit caps — see [workflows.md → `onTransit`](workflows.md#ontransit--the-only-graph-authority).

Two dedicated RPC methods edit it server-side, under the per-task lock (so
concurrent edits serialise with no lost updates); both bump `updated_at`, emit
`task-changed`, and return the updated `TaskView`:

- **`task.metadata.set {id, patch}`** — `patch` keys are **dot-paths** (e.g.
  `step_visits.dev`); each value is written at that leaf, creating intermediate
  objects along the way (a merge, not a whole-document replace).
- **`task.metadata.unset {id, keys}`** — each `keys` entry is a dot-path that is
  removed; an ancestor object emptied by the removal is pruned.

From the CLI:

```bash
autosk metadata show <id>                       # pretty JSON (honors --json)
autosk metadata set <id> step_visits.dev 0      # value parsed as JSON, else a string
autosk metadata unset <id> step_visits          # reset a workflow's dev visit count
```

`autosk show <id> --json` also includes the full `metadata` object. There is no
`task.update` passthrough for metadata — the dedicated `set`/`unset` family is
the only RPC write path.

## JSON-RPC v2 surface

The protocol is JSON-lines over the transport: one JSON object per line. Every
project-scoped method carries a `{cwd}` selector; all timestamps on the wire are
RFC3339 UTC. The wire types are defined once in
[`daemon/sdk/src/proto.ts`](../daemon/sdk/src/proto.ts) and mirrored by the Go
(`internal/daemon/api`) and Tauri (`gui/src-tauri`) clients.

| Domain | Methods |
| --- | --- |
| meta | `version`, `auth`, `healthz`, `shutdown` |
| project | `list`, `add`, `remove`, `init`, `diagnostics` (extension load errors), `subscribe`/`unsubscribe` |
| task | `list`, `get`, `create`, `update`, `enroll {workflow, step?}`, `resume {to?}`, `done`, `cancel`, `reopen`, `block`/`unblock`, `metadata.set`/`metadata.unset`, `comment.add/list/edit/delete`, `subscribe`/`unsubscribe` |
| registry | `workflow.list`, `workflow.get`, `agent.list` (rendered from code — read-only) |
| extension | `install`, `remove`, `update`, `list`, `reload` — the `autosk ext` family (`install`/`remove`/`reload` hot-apply the rebuilt registry to open projects, no restart); see [docs/extensions.md](extensions.md) for the management how-to |
| session | `list {task_id?}`, `get`, `transcript {from_line?, limit?}`, `subscribe`/`unsubscribe` (per-session replay-then-tail), `subscribeProject`/`unsubscribeProject` (project-scope lifecycle pushes), `input {message, kind}`, `abort`, `create {agent}` (open an interactive session), `end` (gracefully end one → `done`) |

Notifications (server→client push): `task-changed`, `project-changed`,
`session-event` (`message`|`status`|`done`|`error`|`partial`),
`session-changed` (a project-scope push of one session's decorated meta on every
create / status change — delivered to `session.subscribeProject` connections, and
never carrying transcript lines, so a client keeps its session list live without
subscribing per-session), and `registry-changed` (a project's extension registry
was hot-reloaded by an `ext add` / `remove` / `reload`; it carries only the
affected `root`, and subscribers re-fetch `registry.workflow.list` /
`extension.list` / `project.diagnostics` to refresh their view). All are fed by
engine events and the fs watcher (so external file edits surface too). The `partial` frame is the ephemeral
live-streaming channel — see [Streaming partial
messages](#streaming-partial-messages) above; it is delivered only to
per-session (`session.subscribe`) subscribers and is **not** broadcast on
`session-changed`.

**Error codes.** The reserved JSON-RPC range carries protocol failures
(`PARSE_ERROR` -32700, `INVALID_REQUEST` -32600, `METHOD_NOT_FOUND` -32601,
`INVALID_PARAMS` -32602, `INTERNAL_ERROR` -32603); the domain errors live in the
`1xxx` range: `PROJECT_NOT_FOUND` (1001 — the `{cwd}` selector did not resolve to
a project), `INVALID_PROJECT` (1002 — a malformed/empty/non-absolute cwd),
`NOT_FOUND` (1003), `CONFLICT` (1004 — the entity exists but isn't in a state
that accepts the request now; retryable). Code 1005 (`ENVIRONMENT_DIRTY`) is
reserved-retired and never emitted (terminals no longer tear down isolation).

A few RPC semantics worth knowing:

- `task.done` / `task.cancel` / `task.reopen` are **administrative overrides**:
  they write via the store and do **not** run `workflow.onTransit` (they reject a
  task with a live session with `CONFLICT`).
- `project.remove` is **lazy**: it forgets the project in the registry and emits
  `project-changed`, but leaves an already-open handle running until the next
  daemon start.
- `extension.install` / `extension.remove` / `extension.reload` **hot-apply** the
  rebuilt extension registry to open projects (no daemon restart) and emit
  `registry-changed`; a running session keeps the workflow/agent code it captured
  at dispatch, and a task whose workflow vanished parks only after its session
  settles. `extension.update` (and editing installed code in place) stays
  restart-only (the Bun module-cache wall).

## Transports, auth, single-instance, idle-shutdown

- **UDS (default).** A Unix-domain socket; parent dir `0700`, socket `0600`. No
  auth — filesystem permissions are the gate. Single-instance is enforced by an
  atomic pidfile lock (`<sock>.lock`, with dead-pid reclaim).
- **TCP (on by default).** The daemon also listens on `0.0.0.0:7077` (all
  interfaces, even when auto-spawned), gated by a token: the first request on a
  TCP connection must be `meta.auth {token}`. The token lives at
  `~/.autosk/daemon-token` (`$AUTOSK_TOKEN_FILE`) and is created on first use.
  `--tcp [HOST:]PORT` overrides the address — e.g. `--tcp 127.0.0.1:7077` to
  restrict it to loopback. Remote front ends (the GUI in remote mode) connect to
  this; a remote daemon must be started explicitly (you can't auto-spawn across
  hosts). A TCP bind failure (e.g. port in use) is non-fatal — the daemon logs it
  and keeps serving UDS-only.
- **Idle-shutdown.** A daemon shuts itself down after the idle window
  (`AUTOSK_IDLE_SECS`, default 30 min) when there are no live connections, no
  queued/running sessions, and no `status=work` tasks across loaded projects.
  Disabled with `0`, and always off when a TCP listener is active (a TCP daemon
  is a service) — which, since TCP is on by default, means idle-shutdown is off
  unless the TCP bind fails.

## Inspecting the daemon from the CLI

```bash
autosk session list [--task ID]      # sessions in this project (one row per agent run)
autosk session get <id>              # one session's meta
autosk session transcript <id>       # render the pi-format transcript
autosk session abort <id>            # abort a live session (parks the task to human)
autosk session input <id> "<msg>"    # steer (or --followup) a live session

autosk project list                  # known projects on this host
autosk project diagnostics           # extension load errors for this project

autosk version                       # CLI + daemon version
```

The Tauri GUI and `autosk lazy` render the same surface live via the subscribe
streams. For the complete CLI verb/flag/env reference, see
[docs/cli.md](cli.md).
