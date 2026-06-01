# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **init:** `autosk init` now syncs enabled global workflows into the project after bootstrap (same behavior as `autosk workflow sync`). Add `--skip-global-workflows` to opt out.
- **init:** `AUTOSK_AUTOINIT_SKIP_GLOBAL_WORKFLOWS` environment variable suppresses global-workflow sync during write-command auto-init (mirrors the explicit `--skip-global-workflows` flag).
- **workflow:** `autosk workflow install <package>` installs workflow bundles declared in npm-style packages via `package.json` `autosk.workflows`, with `--workflow`, `--version`, `--no-install`, and `--json` support.
- **workflow:** add the global workflow registry foundation: canonical workflow definition hashes, `$AUTOSK_WORKFLOWS`/XDG workflow-prefix resolution, registry definition storage, and project-level workflow provenance rows.

### Changed
- **init:** write-command auto-init now runs the same three post-migrate steps as explicit `autosk init`: ensure packages prefix, bootstrap `feature-dev-generic`, and sync enabled global workflows. `--skip-bootstrap` and `AUTOSK_AUTOINIT_SKIP_BOOTSTRAP` remain scoped to the built-in seed only; use `--skip-global-workflows` / `AUTOSK_AUTOINIT_SKIP_GLOBAL_WORKFLOWS` to skip global workflow sync.
- **bootstrap:** `feature-dev-generic` workflow now ships with `isolation: worktree` by default.
- **bootstrap:** `autosk init` now reconciles managed `feature-dev-generic` revisions by canonical hash while preserving unmanaged same-name workflows as no-ops.
- **workflow:** `autosk workflow show` / `--json` now include `superseded_by` / `superseded_by_id` for superseded workflow revisions, and workflow names containing `@rev-` are reserved for autosk internals.

### Fixed
- **daemon:** fix empty lazy transcript and `HTTP 410 session_missing` from `daemon messages <job>`

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

[Unreleased]: https://github.com/wierdbytes/autosk/compare/v0.1.4...HEAD
[0.1.4]: https://github.com/wierdbytes/autosk/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/wierdbytes/autosk/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/wierdbytes/autosk/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/wierdbytes/autosk/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/wierdbytes/autosk/releases/tag/v0.1.0
