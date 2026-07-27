# pi `--mode rpc` contract (D0 reconnaissance)

**Probed pi version:** `0.74.1`; the run-state model below re-verified against
`0.82.1`.
**Source-of-truth files** (read from the installed `dist/`):

- `@earendil-works/pi-coding-agent/dist/modes/rpc/rpc-types.d.ts`
- `@earendil-works/pi-coding-agent/dist/modes/rpc/rpc-mode.{js,d.ts}`
- `@earendil-works/pi-agent-core/dist/types.d.ts` (defines `AgentEvent`)
- `@earendil-works/pi-coding-agent/dist/core/agent-session.d.ts` (defines
  `AgentSessionEvent = AgentEvent | …`)
- `@earendil-works/pi-coding-agent/dist/core/agent-session.js` (the run-active
  flag + post-run phase: where `agent_settled` comes from)

Reproduce with `scripts/pi-rpc-probe.sh`.

> This is a reconnaissance note on **pi's** `--mode rpc` wire contract. The pi
> wire facts below are pi's own; the autosk-side mechanism descriptions refer to
> the `@autosk/pi-agent` extension (`daemon/extensions/pi-agent/`), which is what
> drives pi and mirrors its events into the session transcript.

---

## Wire format

JSON Lines on both stdin and stdout. One object per `\n`-terminated line.
No JSON-RPC framing, just bare objects with a `type` discriminator.

```
stdin   →  RpcCommand
stdout  ←  RpcResponse | AgentSessionEvent | RpcExtensionUIRequest
stdin   →  RpcExtensionUIResponse  (only as replies to a request)
```

---

## Commands we send (subset the daemon uses)

| `type`               | Required fields                       | Used for |
|----------------------|---------------------------------------|----------|
| `prompt`             | `message`                             | Start a fresh user turn on an idle agent (initial run prompt + every kickback). |
| `follow_up`          | `message`                             | Queue an additional user turn while a run is active; pi drains it in the post-run phase. Used by `session.input {kind:"followup"}` when pi is streaming. |
| `steer`              | `message`                             | Same, but steers the run in flight. Used by `session.input {kind:"steer"}` when pi is streaming. |
| `abort`              | —                                     | Stop the current run. Used by cancel before SIGTERM. |
| `get_state`          | —                                     | Read `sessionFile`, `sessionId`, `messageCount`. Re-polled with backoff after the first `prompt` until `sessionFile` populates, then recorded by the pi-agent driver for the session. |
| `set_model`          | `provider`, `modelId`                 | Reserved; daemon prefers `--model` CLI flag. |
| `set_thinking_level` | `level` (`off|minimal|low|medium|high|xhigh`) | Reserved; daemon prefers `--thinking` CLI flag. |

Each command MAY carry an `id: string` for correlation; the response echoes it.

**Important: `prompt` response semantics.** `rpc-mode.js` emits the
`{type:"response", command:"prompt", success:true}` line as soon as
**preflight succeeds**, NOT when the agent is done. The response only tells
you the command was accepted — and that acceptance is what opens one *prompt
cycle*, which ends at `agent_settled` (see
[End-of-turn](#end-of-turn-rule-for-the-daemon)), not at `agent_end`.

A `prompt` written while a cycle is still open is REJECTED, verbatim:

```
Agent is already processing. Specify streamingBehavior ('steer' or 'followUp') to queue the message.
```

Rejections are surfaced verbatim from `session.prompt()` by rpc-mode's
`.catch(e => output(error(id, "prompt", e.message)))`, so their exact wording is
pi's, not a set the caller controls.

---

## Events we observe

### From `AgentEvent` (the core loop)

| `type`                    | Meaning |
|---------------------------|---------|
| `agent_start`             | Loop started for one prompt (pi's run-active flag goes up). Also emitted for each post-run `agent.continue()` round. |
| `agent_end`               | The assistant response finished streaming; `{ messages }`. **Presentation boundary only** — pi is NOT promptable yet. |
| `turn_start`              | One assistant request inside the run is starting. |
| `turn_end`                | One assistant request finished; `{ message, toolResults }`. |
| `message_start`           | New user/assistant/toolResult message; `{ message }`. |
| `message_update`          | Streamed assistant delta; `{ message, assistantMessageEvent }`. |
| `message_end`             | Final form of a message; `{ message }`. |
| `tool_execution_start`    | `{ toolCallId, toolName, args }`. |
| `tool_execution_update`   | `{ toolCallId, toolName, args, partialResult }`. |
| `tool_execution_end`      | `{ toolCallId, toolName, result, isError }`. |

### From `AgentSessionEvent` extensions

| `type`                    | Meaning |
|---------------------------|---------|
| `agent_settled`           | **The prompt cycle is over and pi is promptable again.** Emitted from `_runAgentPrompt`'s `finally`, exactly once per `prompt()`. |
| `compaction_start` / `compaction_end` | Auto- or manual compaction. The auto pair brackets an unbounded summarization LLM call inside the post-run phase. |
| `auto_retry_start` / `auto_retry_end` | Provider retry. `auto_retry_start` carries its own `delayMs` (2s/4s/8s under pi's stock `baseDelayMs: 2000`, `maxRetries: 3`). |
| `queue_update`            | Queued (`steer` / `follow_up`) messages changed. |
| `session_info_changed`, `thinking_level_changed` | Informational. |

The pi-agent extension mirrors the relevant ones into the session transcript
(`ctx.log`); clients tail them via `session.subscribe`.

### Responses and requests

- `{ type:"response", command, success, data?, error? }` — async ack/error.
- `{ type:"extension_ui_request", id, method, … }` — pi asks the host for
  input. **The daemon replies with `cancelled:true`** for `select`, `input`,
  `confirm`, `editor` (so blocking dialogs don't hang headless runs);
  `notify`, `setStatus`, `setWidget`, `setTitle`, `set_editor_text` are
  fire-and-forget — no reply needed.

---

## End-of-turn rule for the daemon

**End-of-turn = receipt of the `agent_settled` event for the prompt cycle a
`prompt` ack opened.** Concretely:

1. Send `{type:"prompt", message}` on stdin; its ack opens exactly one cycle.
2. Stream stdout until you see `{type:"agent_settled"}` — IGNORING any
   intermediate `agent_start`/`agent_end` pairs, which belong to the same cycle.
3. That's one checkpoint. Run `verifyClosure` (plan §6.1.1).
4. If invalid and corrections remaining: send another
   `{type:"prompt", message: correctiveMessage}` and goto 2.
5. If valid or corrections exhausted: close stdin to request shutdown.

### Why not `agent_end`

pi keeps ONE run-active flag (`_isAgentRunActive`) for the whole
`session.prompt()` cycle. It is set before the `prompt` ack and cleared only in
`_runAgentPrompt`'s `finally`, which is what emits `agent_settled`. Between
`agent_end` and `agent_settled` pi runs its **post-run phase** — retry backoff
(`_prepareRetry`), auto-compaction (`_checkCompaction`), queued-message drain —
and each round of it calls `agent.continue()`, emitting EXTRA
`agent_start`/`agent_end` pairs. So a caller that treats `agent_end` as the turn
boundary both over-counts turns and writes its next `prompt` into a window where
pi still refuses it (autosk#19).

`agent_end` still matters, but only for **presentation**: the assistant response
has finished streaming, so that is where a client's "working" indicator goes idle
and any partial-message stream is flushed.

pi's `isStreaming` IS `_isAgentRunActive`, so "pi is streaming" and "pi is not
promptable" are the same bit — flipped at `agent_start` and `agent_settled`, never
at `agent_end`. While it is set, deliver messages with `steer` / `follow_up` (or a
`prompt` carrying a `streamingBehavior` field, which rpc-mode forwards to
`session.prompt()`): pi QUEUES those instead of rejecting them, and drains them in
the post-run phase. A queued message joins the RUNNING cycle — pi still emits only
one `agent_settled` for it — so it is not a way to start a new turn.

### Legacy pi builds

pi versions that never emit `agent_settled` exist. A driver must feature-detect
(fall back to the `agent_end` boundary after a grace) rather than wait forever —
and the grace has to survive pi's long quiet windows, which is what
`auto_retry_start.delayMs` and the `compaction_start`…`compaction_end` bracket are
good for. `@autosk/pi-agent` does this per child; see
[its README](../../daemon/extensions/pi-agent/README.md#run-state--turn-boundaries-agent_end-vs-agent_settled).

---

## Clean shutdown

Closing stdin triggers `shutdown(0)` (see `process.stdin.on("end", …)` in
`rpc-mode.js`). SIGTERM and SIGINT also work, via the same `shutdown`
path. SIGHUP exits 129; otherwise the process exits 143.

The daemon's strategy:

1. Stop sending. Close stdin.
2. Wait `grace` (default 10 s) for clean exit.
3. SIGTERM. Wait the rest of the grace window.
4. SIGKILL.

---

## CLI flags the daemon spawns pi with

```
pi --mode rpc
   --model     <model>          # iff request.model != ""
   --thinking  <level>          # iff request.thinking != ""
   --session-dir <per-session dir>  # always; resolves session.jsonl
   --no-voice                   # avoid audio side-effects in headless runs
   --no-peon                    # ditto
   (request.extra_args appended)
```

We deliberately do NOT pass `-p` / `--print` — that disables RPC's
long-lived stdin handling.

---

## Pi session id & session file path

Captured via `{type:"get_state"}`. The response's `data` contains
`sessionId` and `sessionFile`. The pi-agent extension records both for the
autosk session.

**Why we poll, not query once.** pi 0.74-0.75 creates `session.jsonl`
lazily inside its session manager — the file path is stamped on the
first persist (after the first prompt is preflight-accepted), not at
spawn time. A `get_state` issued right after spawn therefore comes
back with `sessionFile == ""`. The pi-agent driver re-issues `get_state`
with exponential backoff (100 ms → 5 s cap, ~30 s total budget) until
`sessionFile` populates.

autosk does NOT tail pi's own `session.jsonl`: the pi-agent extension
mirrors pi's events into the autosk session transcript
(`./.autosk/sessions/<id>.jsonl`) as it observes them, and clients read
that via `session.transcript` / `session.subscribe`.

---

## Known unknowns (deferred past v0)

- Image inputs in `prompt` — not in the v0 API.
- ~~`compact` lifecycle and how it interleaves with `agent_end`~~ — **answered**
  by `agent_settled`: auto-compaction runs in the post-run phase, i.e. between
  `agent_end` and `agent_settled`, and emits its own `agent_start`/`agent_end`
  pair via `agent.continue()`. Closure is still not re-checked on
  `compaction_start|end`; the pi-agent driver only reads them as proof pi is
  alive inside the window.
- `auto_retry_*` events — the pi-agent extension mirrors them into the
  session transcript (`ctx.log`); clients tail them via `session.subscribe`.
  The daemon does not intervene in pi's retry loop (it only reads
  `auto_retry_start.delayMs` to keep its legacy feature-detect honest).
- Pi version skew — if `dist/modes/rpc/rpc-types.d.ts` changes, only the
  projection layer + wire types in the pi-agent driver
  (`daemon/extensions/pi-agent/src/driver.ts`) need updating.
