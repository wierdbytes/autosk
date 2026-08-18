/**
 * Session meta + transcript file IO and the in-memory index (plan §3.2, §3.7(5)).
 *
 * A session is `sessions/<id>.json` (meta) + `sessions/<id>.jsonl` (a pi-format
 * transcript: header on line 1, then entries). Listing a task's sessions is an
 * in-memory filter of metas by `task_id` (`byTask`); the files are the
 * persistence. "Live" = a meta in `queued` | `running` — the bit the task
 * reconciler consults to decide whether the engine owns a task's status/step/
 * workflow.
 */

import { readFile, readdir, stat } from "node:fs/promises";

import type {
  SessionActivity,
  SessionHeader,
  SessionKind,
  SessionMeta,
  SessionStatus,
  TranscriptEntry,
  TranscriptLine,
} from "@autosk/sdk";
import { appendLine, atomicWrite, fileSig, statSig } from "./atomic.ts";
import { KeyedMutex } from "./lock.ts";
import { consoleLogger, type Logger } from "./logger.ts";
import { ProjectPaths } from "./paths.ts";
import {
  parseSessionMeta,
  parseTranscript,
  serializeSessionHeader,
  serializeSessionMeta,
  serializeTranscriptLine,
} from "./records.ts";

const LIVE_STATUSES: ReadonlySet<SessionStatus> = new Set(["queued", "running"]);

export interface CreateSessionInput {
  id: string;
  /** `"task"` (default) or `"interactive"`; interactive tolerates empty task_id/workflow/step. */
  kind?: SessionKind;
  task_id: string;
  workflow: string;
  step: string;
  agent: string;
  /** Initial live turn activity (interactive sessions open `idle`); omitted for task sessions. */
  activity?: SessionActivity;
  /** The cwd recorded in the transcript header (always the project root — isolation no longer rewrites the run dir). */
  cwd: string;
  /** The header timestamp (RFC3339 UTC). */
  timestamp: string;
}

export class SessionStore {
  private readonly paths: ProjectPaths;
  private readonly locks: KeyedMutex;
  private readonly logger: Logger;

  /** Meta cache, keyed by session id. */
  private metaCache = new Map<string, { sig: string; value: SessionMeta }>();
  /** Index: task id → session ids (insertion order preserved). */
  private byTask = new Map<string, Set<string>>();

  constructor(paths: ProjectPaths, locks: KeyedMutex, logger: Logger = consoleLogger) {
    this.paths = paths;
    this.locks = locks;
    this.logger = logger;
  }

  // -- index ---------------------------------------------------------------

  /** Records the meta→task edge in the in-memory index. */
  private indexMeta(meta: SessionMeta): void {
    let set = this.byTask.get(meta.task_id);
    if (!set) {
      set = new Set();
      this.byTask.set(meta.task_id, set);
    }
    set.add(meta.id);
  }

  /**
   * Sessions for a task, NEWEST-id first (UUIDv7 ids sort by creation, so a
   * descending id sort is newest-first). This is the default order clients
   * render top-to-bottom; a client wanting oldest-first reverses it.
   */
  sessionsForTask(taskId: string): SessionMeta[] {
    const ids = this.byTask.get(taskId);
    if (!ids) return [];
    return [...ids]
      .map((id) => this.metaCache.get(id)?.value)
      .filter((m): m is SessionMeta => m !== undefined)
      .sort((a, b) => (a.id < b.id ? 1 : a.id > b.id ? -1 : 0));
  }

  /** Live (`queued`|`running`) sessions for a task. */
  liveSessionsForTask(taskId: string): SessionMeta[] {
    return this.sessionsForTask(taskId).filter((m) => LIVE_STATUSES.has(m.status));
  }

  /** Whether a task has any live session (the reconciler's ownership signal). */
  hasLiveSession(taskId: string): boolean {
    const ids = this.byTask.get(taskId);
    if (!ids) return false;
    for (const id of ids) {
      const meta = this.metaCache.get(id)?.value;
      if (meta && LIVE_STATUSES.has(meta.status)) return true;
    }
    return false;
  }

  /** Every known session meta (cache snapshot). */
  allMetas(): SessionMeta[] {
    return [...this.metaCache.values()].map((c) => c.value);
  }

  // -- reads ---------------------------------------------------------------

  /**
   * Reads a session meta, refreshing the cache when the file changed. Returns
   * `null` for an absent OR unparseable meta (logged) — a corrupt session file
   * under hybrid ownership must not throw a raw parse error out of the
   * `session.get` RPC read; the caller treats it as "not found" (mirrors the
   * defensive parse on the human-editable files).
   */
  async getMeta(id: string): Promise<SessionMeta | null> {
    const probe = await statSig(this.paths.sessionMeta(id));
    if (!probe) return null;
    const cached = this.metaCache.get(id);
    if (cached && cached.sig === probe.sig) return cached.value;
    const text = await readFile(this.paths.sessionMeta(id), "utf8");
    let meta: SessionMeta;
    try {
      meta = parseSessionMeta(text);
    } catch (e) {
      this.logger.warn(
        `session ${id}: unparseable meta, treating as not found ` +
          `(${e instanceof Error ? e.message : String(e)})`,
      );
      return null;
    }
    this.metaCache.set(id, { sig: probe.sig, value: meta });
    this.indexMeta(meta);
    return meta;
  }

  /**
   * Reads transcript lines. `fromLine` is 1-based (header is line 1); `limit`
   * caps the count. Returns the lines plus the 1-based cursor to resume tailing.
   *
   * NOTE (P5): this re-reads + re-parses the whole file on every paged call, so
   * subscribe-then-tail clients pay O(file) per page. Acceptable for P2; when P5
   * wires live tailing, replace this with a line/byte-offset index or an
   * incremental streamed read so each poll is O(new lines), not O(file).
   */
  async readTranscript(
    id: string,
    opts: { fromLine?: number; limit?: number } = {},
  ): Promise<{ lines: TranscriptLine[]; nextLine: number }> {
    const probe = await statSig(this.paths.sessionTranscript(id));
    if (!probe) return { lines: [], nextLine: 1 };
    const text = await readFile(this.paths.sessionTranscript(id), "utf8");
    const all = parseTranscript(text);
    const from = Math.max(1, opts.fromLine ?? 1);
    const start = from - 1;
    const slice = opts.limit !== undefined ? all.slice(start, start + opts.limit) : all.slice(start);
    return { lines: slice, nextLine: start + slice.length + 1 };
  }

  // -- mutations (locked, atomic) ------------------------------------------

  /**
   * Creates a session: writes the `queued` meta and the transcript header
   * (line 1). Returns the meta.
   */
  async create(input: CreateSessionInput): Promise<SessionMeta> {
    return this.locks.run(`session::${input.id}`, async () => {
      const kind: SessionKind = input.kind ?? "task";
      const meta: SessionMeta = {
        id: input.id,
        kind,
        task_id: input.task_id,
        workflow: input.workflow,
        step: input.step,
        agent: input.agent,
        status: "queued",
        started_at: null,
        ended_at: null,
      };
      if (input.activity !== undefined) meta.activity = input.activity;
      const header: SessionHeader = {
        type: "session",
        version: 1,
        id: input.id,
        kind,
        task_id: input.task_id,
        workflow: input.workflow,
        step: input.step,
        agent: input.agent,
        timestamp: input.timestamp,
        cwd: input.cwd,
      };
      await atomicWrite(this.paths.sessionTranscript(input.id), serializeSessionHeader(header) + "\n");
      const sig = await atomicWrite(this.paths.sessionMeta(input.id), serializeSessionMeta(meta));
      this.metaCache.set(input.id, { sig, value: meta });
      this.indexMeta(meta);
      return meta;
    });
  }

  /** Patches a session meta (status/activity/error/timestamps). Returns the new meta. */
  async patchMeta(
    id: string,
    patch: Partial<Pick<SessionMeta, "status" | "activity" | "error" | "started_at" | "ended_at" | "usage">>,
  ): Promise<SessionMeta> {
    return this.locks.run(`session::${id}`, async () => {
      const current = this.metaCache.get(id)?.value ?? (await this.getMeta(id));
      if (!current) throw new Error(`session not found: ${id}`);
      const next: SessionMeta = { ...current, ...patch };
      const sig = await atomicWrite(this.paths.sessionMeta(id), serializeSessionMeta(next));
      this.metaCache.set(id, { sig, value: next });
      this.indexMeta(next);
      return next;
    });
  }

  /**
   * Conditionally patches a meta only if its current status equals `expect`.
   * Returns the meta (patched when applied, current otherwise) and whether the
   * patch was applied.
   *
   * The engine uses this to make the `queued → running` transition ATOMIC against
   * a concurrent `session.abort`: if an abort already sealed the queued session
   * (`status:aborted`), a late `running` write must NOT resurrect it. Both writes
   * serialise on the per-session lock, and the status precondition is evaluated
   * under that lock, so exactly one of {start, abort} wins.
   */
  async patchMetaIf(
    id: string,
    expect: SessionStatus,
    patch: Partial<Pick<SessionMeta, "status" | "activity" | "error" | "started_at" | "ended_at" | "usage">>,
  ): Promise<{ meta: SessionMeta; applied: boolean }> {
    return this.locks.run(`session::${id}`, async () => {
      const current = this.metaCache.get(id)?.value ?? (await this.getMeta(id));
      if (!current) throw new Error(`session not found: ${id}`);
      if (current.status !== expect) return { meta: current, applied: false };
      const next: SessionMeta = { ...current, ...patch };
      const sig = await atomicWrite(this.paths.sessionMeta(id), serializeSessionMeta(next));
      this.metaCache.set(id, { sig, value: next });
      this.indexMeta(next);
      return { meta: next, applied: true };
    });
  }

  /** Appends one transcript entry (a single newline-terminated JSON line). */
  async appendEntry(id: string, entry: TranscriptEntry): Promise<void> {
    await this.locks.run(`session::${id}`, async () => {
      await appendLine(this.paths.sessionTranscript(id), serializeTranscriptLine(entry) + "\n");
    });
  }

  // -- startup scan --------------------------------------------------------

  /**
   * Scans `sessions/` into the meta cache + `byTask` index. Returns the metas
   * found. Malformed metas are skipped (logged by the caller).
   */
  async scan(): Promise<{ metas: SessionMeta[]; errors: { id: string; error: string }[] }> {
    let entries: string[];
    try {
      entries = await readdir(this.paths.sessionsDir);
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code === "ENOENT") return { metas: [], errors: [] };
      throw e;
    }
    const metas: SessionMeta[] = [];
    const errors: { id: string; error: string }[] = [];
    for (const name of entries) {
      if (!name.endsWith(".json")) continue;
      const id = name.slice(0, -".json".length);
      try {
        const path = this.paths.sessionMeta(id);
        const st = await stat(path);
        const text = await readFile(path, "utf8");
        const meta = parseSessionMeta(text);
        this.metaCache.set(id, { sig: fileSig(st), value: meta });
        this.indexMeta(meta);
        metas.push(meta);
      } catch (e) {
        errors.push({ id, error: e instanceof Error ? e.message : String(e) });
      }
    }
    return { metas, errors };
  }
}
