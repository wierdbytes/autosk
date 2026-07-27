# @autosk/pi-agent

Drive [`pi`](https://github.com/earendil-works/pi) (`pi --mode rpc`) as an
autoskd v2 **agent**. `piAgent({...})` returns an
[`AgentDefinition`](../../sdk/src/agent.ts) the engine can run for a workflow
step; it ports v1's "standard branch" (spawn pi, seed the first message, drive
turns, kickback loop) onto the v2 `ctx.spawn` + `ctx.transit` API (design
`docs/plans/20260612-Bun-Daemon-Extensions.md` §3.4).

## Usage

```ts
import { piAgent } from "@autosk/pi-agent";
import { sandboxCleanupStep, worktreeSandbox } from "@autosk/sandbox";
import { statusStep } from "@autosk/sdk";

export default function (autosk) {
  const sandbox = worktreeSandbox(); // or dockerSandbox({ image })
  autosk.registerWorkflow({
    name: "my-flow",
    firstStep: "dev",
    steps: {
      // The step key IS the agent name; registering the workflow registers
      // its inline agents. Each runs its harness in the per-task `sandbox`.
      dev: piAgent({
        sandbox,
        model: "sonnet:high",
        firstMessageFile: new URL("./prompts/dev.md", import.meta.url).pathname,
      }),
      review: piAgent({ sandbox, thinking: "xhigh", firstMessageFile: ".../review.md" }),
      accept: statusStep("human"),
      // Teardown is a normal step (no engine reap): route terminals through it.
      cleanup: sandboxCleanupStep(sandbox),
    },
  });
}
```

A `piAgent({...})` is an inline **step value**: the step key is the agent name
(there is no `name` option — the driver takes its display name from
`ctx.workflows.current.step`), so registering the workflow registers its agents.

## How it works

On each `onRun` the agent:

1. spawns `pi --mode rpc` (with the role's `model` / `thinking` / extra args)
   and **injects a pi extension** that registers an `autosk_transit` tool. The
   spawn env also carries `AUTOSK_CWD` (the canonical project root, from
   `ctx.projectRoot`) and `AUTOSK_AGENT` (the step name), so any `autosk` CLI
   the agent runs — directly, or via the `@autosk/pi-tools` `autosk_task` /
   `autosk_comment` tools — targets the task's own project and attributes
   comments to the step, even when the run is in a throwaway worktree;
2. seeds pi with the rendered step prompt (role first-message + task context +
   the available transitions + "call `autosk_transit`");
3. mirrors pi's session entries (messages / custom) into the autosk transcript
   1:1, so existing pi renderers stay reusable; while a turn streams it also
   forwards pi's `message_update` events as **ephemeral partial snapshots** via
   `ctx.partial(m)` (coalesced ~40 ms), so a client renders the in-progress
   assistant message live before the durable line commits (see [docs/daemon.md →
   Streaming partial messages](../../../docs/daemon.md#streaming-partial-messages));
4. **observes the `autosk_transit` tool call** on pi's RPC event stream and
   translates it into `ctx.transit(...)` (the transit channel — core stays
   closed, no session-scoped daemon RPC);
5. runs a **kickback/corrections loop** (private to this extension): if a turn
   ends without a transit — or a chosen transition is rejected by the workflow's
   `onTransit` — it feeds a corrective message back to the model and retries, up
   to `maxCorrections` times. After the budget is spent it returns without a
   transit and the engine parks the task (`agent_did_not_transit`).

`onSteer` / `onFollowup` forward a `session.input` message into the **live** pi;
`onAbort` asks pi to wind down gracefully (the engine's abort signal already
terminates the child).

### Run state & turn boundaries (`agent_end` vs `agent_settled`)

pi keeps ONE run-active flag for a whole `prompt()` cycle and rejects a fresh
`{type:"prompt"}` with *"Agent is already processing"* while it is set. The flag
goes up at `agent_start` and comes down only in `_runAgentPrompt`'s `finally`,
which emits **`agent_settled`** — after pi's *post-run phase* (retry backoff,
auto-compaction, queued-message drain), each round of which emits its own EXTRA
`agent_start`/`agent_end` pair. So `agent_end` means "the assistant response
finished streaming", NOT "pi is promptable again", and a kickback sent at
`agent_end` can land in that window and fail the session
([#19](https://github.com/wierdbytes/autosk/issues/19)).

The driver therefore splits the two:

| pi event        | driver reaction                                                                                          |
| --------------- | -------------------------------------------------------------------------------------------------------- |
| `agent_start`   | `onActivity(true)`; pi is streaming, bare prompts are refused.                                             |
| `agent_end`     | **presentation only** — flush partial snapshots, `onActivity(false)` (the chat UI goes idle).             |
| `agent_settled` | **the turn boundary** — clear `streaming`, open the prompt gate, resolve exactly one `waitForTurnEnd()`. |

What that means for anyone reading or reusing `PiDriver`:

- **`waitForTurnEnd()` resolves once per PROMPT CYCLE**, at `agent_settled` — not
  once per `agent_end` (its pre-0.1.5 semantics). A cycle is opened by an
  *accepted* `prompt`, so the post-run phase's extra `agent_start`/`agent_end`
  pairs can never enqueue a phantom turn-end — nor burn a `maxCorrections` slot.
- **`sendPrompt()` waits for that gate** before writing. If pi still answers
  "already processing", the driver treats the rejection as ground truth (pi IS
  running), heals its view and re-waits for the real boundary rather than
  sleeping a fixed amount.
- **Steer / followup inside the window** dispatch as `steer` / `follow_up` (which
  pi queues and drains post-run) instead of a doomed `prompt`, because the
  driver's `streaming` flag now mirrors pi's `isStreaming` exactly.
- **Legacy pi builds** that never emit `agent_settled` are feature-detected per
  child: after a grace with no `agent_settled` the driver falls back to the old
  `agent_end` boundary. The grace is extended by an announced
  `auto_retry_start.delayMs` and suspended for a `compaction_start`…
  `compaction_end` bracket, and a late `agent_settled` (or a busy rejection)
  un-learns a wrong verdict.
- **No hangs:** the gate and `waitForTurnEnd()` are released unconditionally on
  child exit and on abort; a `sendPrompt` against a dead pi fails fast.

pi's side of this contract is written up in
[`docs/notes/pi-rpc-contract.md`](../../../docs/notes/pi-rpc-contract.md); the
full rationale (and the exact pi source lines it rests on) lives in the
`PiDriver` class doc comment in `src/driver.ts`.

## Interactive (chat) mode

Besides backing a workflow step, this package's **default export** registers a
named agent, `"pi"`, via `autosk.registerAgent(...)`, so the daemon can open an
**interactive (taskless) chat session** against it (see
[docs/daemon.md → Interactive sessions](../../../docs/daemon.md#interactive-taskless-sessions)).
`onRun` branches on `ctx.mode`:

- `"task"` — the workflow transit loop above (unchanged).
- `"interactive"` — a chat loop: spawn `pi --mode rpc` **without** the
  `autosk_transit` extension (transit is unavailable in a chat, so the tool is
  not offered), send **no** initial prompt (the session is empty until the user
  types), then await `ctx.signal`. Each composer message arrives via `onFollowup`
  and is forwarded to the live pi (idle → a fresh turn, streaming → a follow-up).
  The agent returns when the signal fires; the engine seals the session `done`
  (graceful end), `aborted` (abort), or `failed` (crash) — no transit, no park.

## Configuration — `PiAgentOptions`

The agent name is **not** an option — it is the workflow step key the `piAgent`
is assigned to (taken from `ctx.workflows.current.step` at run time).

| Option             | Default                       | Description                                                              |
| ------------------ | ----------------------------- | ------------------------------------------------------------------------ |
| `model`            | pi default                    | pi model spec, e.g. `"sonnet:high"` (`--model`).                          |
| `thinking`         | pi default                    | Thinking level `off`…`xhigh` (`--thinking`).                             |
| `firstMessage`     | `""`                          | Inline first-message seed (wins over `firstMessageFile`).                |
| `firstMessageFile` | —                             | Path to a file whose contents seed the first message.                   |
| `extraArgs`        | `[]`                          | Extra args forwarded verbatim to `pi`.                                   |
| `piExtensions`     | `[]`                          | pi extensions to load (`-e <path>` each).                                |
| `piSkills`         | `[]`                          | pi skills to enable (`--skill <name>` each).                            |
| `maxCorrections`   | `3`                           | Corrective turns before giving up (then the engine parks the task).     |
| `piBin`            | `$AUTOSK_PI_BIN` or `"pi"`    | `pi` binary to spawn (the e2e tests point this at a stub).              |

## The injected `autosk_transit` tool

`src/pi-transit-extension.ts` is the pi extension this package injects via
`pi -e`. It registers the `autosk_transit` tool (one `to` string: a sibling step
name, or `done` | `cancel` | `human`). The tool returns an immediate ack; the
real transition (and any rejection fed back as a correction) is driven by the
autosk daemon, which observes the call on pi's RPC event stream. That file is
loaded by **pi's** toolchain, not the daemon, so it is excluded from this
package's `tsc` typecheck.

## Tool surface under a sandbox

The agent ALWAYS injects only the ack-only `autosk_transit` extension;
`autosk_task` / `autosk_comment` come from the single, **transport-aware**
[`@autosk/pi-tools`](https://www.npmjs.com/package/@autosk/pi-tools) extension pi
loads from its own config. The `sandbox?` option (a `Sandbox` from
`@autosk/sandbox`, or any structural sandbox) decides where the harness runs AND
which TRANSPORT pi-tools uses:

- **host / `worktreeSandbox`** (not thin): pi runs on the host at the worktree
  (`~/.autosk/worktrees/<slug>/<task>`); no MCP env is set, so `@autosk/pi-tools`
  shells out to the `autosk` CLI. The daemon sets **`AUTOSK_CWD`**
  (= `ctx.projectRoot`) so those calls resolve the original project, not the
  worktree.
- **`dockerSandbox`** (thin — `sandbox.thin === true`): the agent mints a
  per-session HTTP MCP server (`ctx.newMCPServer()`) and injects
  `AUTOSK_MCP_URL` (rewritten to `host.docker.internal` via
  `sandbox.endpointFor(port)`) + `AUTOSK_MCP_TOKEN`, so the same
  `@autosk/pi-tools` POSTs `autosk_task`/`autosk_comment` to it instead of
  shelling out — the image needs neither `autosk` nor a mounted socket. The
  sandbox bind-mounts the injected transit extension so `pi -e <path>` resolves
  inside the container.

## Exports

- `piAgent(options)` → `AgentDefinition`
- `buildPiCommand(options, { interactive? })` (the `interactive` flag skips the
  injected transit extension; task/comment always come from `@autosk/pi-tools`),
  `PiDriver` (note the `waitForTurnEnd()` semantics above — one resolve per
  prompt cycle, at `agent_settled`),
  `parseTarget`, `buildInputCommand`, `isStateMismatch`, `isBusyRejection`,
  the prompt renderers
  (`renderInitialPrompt`, `kickbackMessage`, `rejectionMessage`, `targetLabels`)
  — exported for tooling / tests.
- default export — an extension factory that registers the named `"pi"` agent for
  interactive chat sessions. (Workflow roles are still registered separately, by
  the consuming extension, e.g. `@autosk/feature-dev`, as inline `piAgent({...})`
  step values.)
