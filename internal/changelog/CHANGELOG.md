# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **`autosk watch`** streams live task, step, blocker, and session progress (#18).

### Fixed
- **pi-agent:** gate prompts on pi's `agent_settled`, ending "Agent is already processing" session failures (#19).
- **daemon:** large task descriptions no longer corrupt a multibyte UTF-8 char at a transport chunk boundary (#15).
- **workflow:** step lists now preserve declaration order (#14).

## [0.2.5] — 2026-07-21

### Added
- **`autosk ext reload`** — rebuild a project's extension registry live, no daemon restart.
- **`gh-review` workflow** — opt-in: review a GitHub issue/PR in docker with read-only `gh`.

### Changed
- **`ext add` / `ext remove`** now hot-apply to open projects (no restart); running sessions undisturbed.

### Fixed
- **GUI:** restore the last active project after restarting the desktop application.
- **GUI:** session-log HTTP(S) links open externally without leaving autosk (ask-acf7cc).
- **lazy:** enroll picker now starts the task at the selected step instead of the first step.
- **lazy:** manual Detail scroll-up now survives live redraws while the session input overlay is visible.

## [0.2.4] — 2026-06-28

### Added
- **GUI:** transcript thinking and tool-call blocks now fold (collapsed by default) with rich per-tool headers.

## [0.2.3] — 2026-06-24

### Added
- **`AUTOSK_SKIP_SHELL_PATH`** env knob disables the daemon's startup login-shell `PATH` probe.

### Fixed
- **GUI-launched `autoskd` now enriches `PATH` from the login shell**, so `git-lfs`/`npm`/`docker` resolve.

## [0.2.2] — 2026-06-24

### Fixed
- **gui (iOS):** iOS builds now ship the custom autosk app icon instead of the stock Tauri logo.

## [0.2.1] — 2026-06-24

### Changed
- **`autoskd` now listens on TCP `0.0.0.0:7077` by default**; override via `autoskd serve --tcp [HOST:]PORT`.

## [0.2.0] — 2026-06-24

> **Clean break — autosk v2.** A full rewrite: tasks, comments, and sessions are
> now plain **files** under `.autosk/` — there is **no database** and **no
> migrator**, and workflows and agents are now **code** shipped by extensions.
> v2 cannot open a v1 project — open old `.autosk/db` projects with the last v1
> release, [`v0.1.6`](https://github.com/wierdbytes/autosk/releases/tag/v0.1.6).

### Added
- **Claude Code agent** (`@autosk/claude-agent`) alongside the pi agent.
- **`@autosk/feature-dev`** reference workflow: `dev → review → docs → validator → accept`.
- **Claude Code and Docker variants** — `@autosk/feature-dev-cc`, `@autosk/feature-dev-docker`.
- **`@autosk/merge-to-current`** workflow — merge a task branch into the current branch.
- **Agent-owned sandboxes** (`@autosk/sandbox`): per-task git-worktree or Docker isolation.
- **Interactive chat sessions** — pick an agent and chat turn-by-turn, no task needed.
- **Editable task metadata** via `autosk metadata show/set/unset`, the TUI, and the GUI.
- **`autosk ext` extension management** — add, list, remove, and update extensions.
- **New CLI groups** `autosk session` and `autosk project`, plus `comment edit/delete`.
- **Live session updates** across the `autosk lazy` TUI and the desktop GUI.
- **Fresh installs** auto-provision the default `feature-dev` workflow on first run.
- **Native Tauri desktop GUI** at `autosk lazy` parity.
- **GUI: connect to a remote `autoskd`** over TCP with a host:port and token, from Settings.
- **GUI: streaming agent messages** — text, thinking, and tool calls render live.
- **GUI: extension browser** — find and install `autosk-extension` npm packages.
- **GUI: iPhone-friendly compact single-pane layout** on touch devices.
- **Distribution:** macOS Homebrew cask, Linux AppImage/`.deb`, and iOS TestFlight builds.

### Changed
- Tasks drop `priority` and `author`; the `list` table and `--json` lose the priority column.
- `autosk lazy`: the Jobs pane is now **Sessions**; the Workflows pane is read-only.
- `enroll` now accepts `new`/`cancel`/`human` and a `--step`; the GUI folds Resume into Enroll.
- `feature-dev`'s validator updates `CHANGELOG.md` and commits before accepting.
- `done`/`cancel` are now plain status flips (no worktree dirty-gate).

### Removed
- The `.autosk/db` database, the `--db`/`AUTOSK_DB` selector, and `migrate`.
- CLI verbs `sql`, `worktree`, `gc`, `history`, `step next`, and the `autosk daemon` group.
- `workflow create/delete/update` and `agent install/uninstall` — workflows and agents are now code.
- `autosk agent list/show` and the enroll/create `--agent` flag.
- The Homebrew formula — the macOS tap is now cask-only.
- The v1 `@wierdbytes/pi-autosk` pi extension and v1 npm-package agents.

## [0.1.6] — 2026-06-08

### Changed
- **bootstrap:** the `feature-dev-generic` workflow now ships with
  `isolation: worktree` by default.

### Fixed
- **daemon:** fix the empty lazy transcript and `HTTP 410 session_missing` from
  `daemon messages <job>`.

## [0.1.5] — 2026-05-25

### Added
- **lazy:** two-pane workflow + step picker for `enroll` / `resume` actions.
- **lazy:** redesigned Workflow Detail pane.
- **lazy:** `?` opens a sectioned, filterable cheatsheet popup; Enter runs the highlighted binding (ask-ed8035).
- **lazy:** context-aware bindings hint row pinned across the bottom of the dashboard (ask-ed8035).
- **lazy:** changelog modal on first run of a new release; `ctrl+w` to re-open the full CHANGELOG.md; `--no-changelog` to suppress the auto-popup (ask-911ea0).

### Changed
- **lazy:** status bar collapsed to a single row with ` | ` separators; legacy `?=help` hint moved into the cheatsheet popup (ask-ed8035).
- **docs:** `CHANGELOG.md` now lives at `internal/changelog/CHANGELOG.md` (embedded into the binary via `go:embed` for the new changelog modal); the repo-root `CHANGELOG.md` is a relative symlink so release tooling, GitHub's auto-renderer, and `scripts/changelog-section.sh` keep working unchanged (ask-911ea0).

### Fixed
- **lazy:** fix cursor jumping to a different row when a refresh re-sorted or re-filtered a panel.
- **lazy:** hydrate the job transcript on focus change to the Jobs panel.

### Docs
- **lazy:** document the two-pane enroll / resume picker.
- **lazy:** describe the redesigned Workflow Detail pane.
- **lazy:** document the changelog modal, `ctrl+w` re-opener, `--no-changelog` flag, and the `~/.autosk/state.json` `last_seen_changelog` field (ask-911ea0).

## [0.1.4] — 2026-05-24

### Added
- **enroll:** allow enrolling tasks that are currently in `human`, `done`,
  or `cancel` status (previously enroll was limited to `new` / `work`).
- **lazy:** rework Jobs panel columns — worktime column, colored status glyph,
  right-aligned task id.

### Changed
- **cli:** drop `autosk assign` in favour of `autosk enroll --agent`.
  `enroll --agent <pkg>` is now the single way to bind an agent to a task,
  with or without a workflow.
- **examples:** move workflow JSON examples to `docs/examples/workflows/`.

### Fixed
- **lazy:** render the full comment thread in the Tasks → Detail pane
  (drop the previous "last 5 comments" cap).
- **enroll:** address review remarks on the human / done / cancel enroll path.

### Docs
- **enroll:** refresh `docs/workflows.md` and CLI docstrings for the new
  human / done / cancel enroll behaviour.

## [0.1.3] — 2026-05-23

### Added
- **workflow:** `autosk workflow update --isolation <mode>` to change a
  workflow's worktree isolation mode in place; lazy surfaces this via the
  new `i` binding on a selected workflow.

### Fixed
- **workflow update:** per-task confirmation body, JSON-on-error output, and
  sentinel-preserving error returns (review remarks WU1–WU4).

### Docs
- Cover `workflow update --isolation` and the lazy `i` binding.
- Simplify the workflow-creation doc; layout fix for the workflow doc.

## [0.1.2] — 2026-05-23

### Docs
- Rewrite `docs/lazy.md` for the current version of the TUI.
- Add the lazy-mode dashboard screenshot (`docs/lazy-mode.png`) to the docs.
- Clean up `README.md`, add more cross-links to the docs.

## [0.1.1] — 2026-05-23

### Added
- **README:** Homebrew install option (`brew install wierdbytes/autosk/autosk`).

### Changed
- **ci/release:** automatically bump the `wierdbytes/homebrew-autosk` formula
  on every tag publish.
- **ci/release:** bump `actions/download-artifact` v6 → v7 (Node.js 24).

## [0.1.0] — 2026-05-23

Initial public release of **autosk** — a local-first task manager and
workflow engine for AI coding agents. One DB per repo, one daemon per host,
one TUI to see it all.

### Added

#### Core / task tracker
- doltlite-backed `.autosk/db` per repository.
- Task ids in the canonical `ask-XXXXXX` format (6 hex chars).
- Blockers / blocked-by graph, priorities `0..3` (`0` highest).
- Short, persisted task / run status vocabulary: `new`, `work`, `human`,
  `done`, `cancel`.
- CLI verbs: `init`, `create`, `update`, `show`, `list`, `ready`, `done`,
  `cancel`, `enroll`.
- `--step NAME` flag on `create` / `enroll` to join a workflow at a specific
  step instead of its `first_step`.
- Implicit auto-init in fresh directories on the first write verb; opt-out
  with `--skip-bootstrap`; non-interactive auto-accept via
  `AUTOSK_AUTOINIT_*` env knobs.
- Auto-bootstrap of the canonical `feature-dev-generic` workflow on
  `autosk init` (and on implicit auto-init).

#### Workflow engine
- v0.2 step engine with directed-graph workflows, transitions, agents, and
  comments.
- `autosk_step next` as the canonical agent-side "I'm done with this step"
  signal.
- Per-step `agent.params` overrides for npm-packaged agents.
- Step visit limits + target-step parking on advance failures.
- Parked / failed runs render uniformly as `[id]: name` and surface all
  fields.
- v0.3 worktree isolation per workflow; executor auto-recovers a missing
  worktree dir; `AUTOSK_DB` is isolated in tests.

#### Agents
- npm-package agent system (replaces the earlier `.autosk/agents/*.toml`).
- Reference workflow `feature-dev-generic` (dev → review → docs → validator
  → human) shipped via `go:embed` from `internal/bootstrap`.

#### Daemon
- `autosk daemon` — multi-project orchestrator over a Unix-domain socket
  (one daemon per host, project resolved per request).
- HTTP API with closure verification and SSE event stream.
- `pi`-runner reader switched to `json.Decoder`; honours the cancellation
  signal on read-loop errors; `step_signal` honoured on wait-error recovery.

#### Lazy TUI (`autosk lazy`)
- gocui-based dashboard with Tasks / Jobs / Workflows / Agents / Detail
  panes and a smoke E2E.
- Datasource layer split into offline + live + compose.
- Lazygit-style task-compose popup editor; key bindings: `c` (edit
  title / description, two-pane compose), `m` (single-pane comment compose),
  `M` (JSON-validated metadata compose), `i` (workflow isolation), `?`
  (help), `n` (new task).
- Tokyo Night theme + dedicated `theme` package; rounded panel and popup
  corners; scroll-off-margin viewport that follows the cursor; mouse-wheel
  scroll across panels and inspector.
- Tasks panel redesigned with entity colours, spinner, job marker, and
  manual-scope toggle.
- Detail pane: labeled boxes, markdown rendering with a bounded LRU cache,
  signals box stacked as a 4-column table, blockers partitioned by status,
  full comment thread.
- Job Detail redesigned — Inspector removed, sticky-tail on the transcript,
  job-input nested inside Detail and gated on `Streaming`.
- Jobs panel: age column moved leftmost.

#### Extension (`@wierdbytes/pi-autosk`)
- TypeScript SDK + agent runtime, callable from `pi --mode rpc`.
- Mega-tool split into three focused tools: `autosk_task`,
  `autosk_comment`, `autosk_step`.

#### Docs
- End-user `README.md` (install, quick start, lazy mode, CLI walk-through).
- `docs/daemon.md`, `docs/lazy.md`, `docs/workflows.md`.
- Time-rendering policy: human-facing times route through
  `internal/timeformat` in the operator's local TZ; machine wire formats
  (JSON CLI, HTTP API, `RunContextSeed`, TS types) stay on RFC3339 UTC.
- `AGENTS.md` rewritten as a repo orientation guide.

#### Build & CI
- Go 1.25 toolchain; mandatory `-tags libsqlite3` build tag routed through
  doltlite-provided libsqlite3.
- `make doctor` checks the doltlite library; every build / test target
  depends on it.
- Auto-fetch `libdoltlite`; CI runs on linux + macOS.
- Publish release binaries on every semver tag push.
- Migrations 001..007 squashed into a single release migration.

### Changed
- **statuses:** shortened persisted task / run status vocabulary
  (e.g. `in_workflow` → `work`); CLI / TUI / docs all updated.
- **tasksvc:** unified human-driven status transitions across the CLI and
  the lazy TUI.

### Fixed
- **doltlite:** validate the file inode on every connection checkout to
  close a write-loss race.
- **doltlite:** rotate the `sql.DB` connection periodically to recover from
  cross-process garbage collection.
- **runstore:** retry on `database is locked` for every write, not just
  reads.
- **workflow:** `REINDEX workflows` after `Delete` to clear phantom
  unique-index entries.
- **migrations:** rebuild `tasks` / `task_deps` in migration 007 to dodge a
  doltlite UPDATE-PK quirk.
- **compactor + lazy:** bound chunk-store WAL replay so `autosk lazy`
  doesn't peg a CPU core.
- **lazy/tui:** assorted scope, focus, and sticky-tail fixes on the Detail
  pane; widen input visibility to `!IsTerminal`; pin overlay sticky-tail
  and popup z-order; clear `TextArea` (not just `v.lines`) on
  dispatch / cursor-move / Esc.

[Unreleased]: https://github.com/wierdbytes/autosk/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/wierdbytes/autosk/compare/v0.1.6...v0.2.0
[0.1.6]: https://github.com/wierdbytes/autosk/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/wierdbytes/autosk/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/wierdbytes/autosk/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/wierdbytes/autosk/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/wierdbytes/autosk/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/wierdbytes/autosk/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/wierdbytes/autosk/releases/tag/v0.1.0
