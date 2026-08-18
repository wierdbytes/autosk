/**
 * SessionStore IO: meta lifecycle, transcript append + paged read, index.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { appendFile, mkdir, writeFile } from "node:fs/promises";

import { CapturingLogger, KeyedMutex, ProjectPaths, SessionStore } from "../src/index.ts";
import type { CustomEntry } from "@autosk/sdk";
import { tempDir } from "./helpers.ts";

describe("SessionStore", () => {
  let dir: ReturnType<typeof tempDir>;
  let sessions: SessionStore;
  let paths: ProjectPaths;

  beforeEach(() => {
    dir = tempDir();
    paths = new ProjectPaths(dir.path);
    sessions = new SessionStore(paths, new KeyedMutex());
  });
  afterEach(() => dir.cleanup());

  const base = {
    task_id: "ask-aaaaaa",
    workflow: "wf",
    step: "dev",
    agent: "ag",
    cwd: "/repo",
    timestamp: "2026-01-01T00:00:00Z",
  };

  test("create writes a queued meta + a header-only transcript", async () => {
    const id = "0190a1b2-0000-7000-8000-000000000001";
    const meta = await sessions.create({ id, ...base });
    expect(meta.status).toBe("queued");
    expect(meta.started_at).toBeNull();
    expect(meta.ended_at).toBeNull();

    const { lines } = await sessions.readTranscript(id);
    expect(lines).toHaveLength(1);
    expect((lines[0] as { type: string }).type).toBe("session");
  });

  test("patchMeta moves the lifecycle forward and persists", async () => {
    const id = "0190a1b2-0000-7000-8000-000000000002";
    await sessions.create({ id, ...base });
    await sessions.patchMeta(id, { status: "running", started_at: "2026-01-01T00:00:01Z" });
    const running = await sessions.getMeta(id);
    expect(running?.status).toBe("running");
    expect(running?.started_at).toBe("2026-01-01T00:00:01Z");

    await sessions.patchMeta(id, {
      status: "failed",
      error: "boom",
      ended_at: "2026-01-01T00:00:02Z",
      usage: {
        input: 3,
        output: 1,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 4,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
      },
    });
    const failed = await sessions.getMeta(id);
    expect(failed?.status).toBe("failed");
    expect(failed?.error).toBe("boom");
    expect(failed?.ended_at).toBe("2026-01-01T00:00:02Z");
    expect(failed?.usage?.totalTokens).toBe(4);
  });

  test("appendEntry + paged readTranscript (fromLine / limit / cursor)", async () => {
    const id = "0190a1b2-0000-7000-8000-000000000003";
    await sessions.create({ id, ...base });
    for (let i = 0; i < 5; i++) {
      const entry: CustomEntry<{ i: number }> = {
        type: "custom",
        id: `e${i}`.padEnd(8, "0"),
        timestamp: "2026-01-01T00:00:00Z",
        customType: "test:tick",
        data: { i },
      };
      await sessions.appendEntry(id, entry);
    }

    // Whole file: 1 header + 5 entries = 6 lines.
    const all = await sessions.readTranscript(id);
    expect(all.lines).toHaveLength(6);
    expect(all.nextLine).toBe(7);

    // Tail from line 2 (skip header), limit 2.
    const page = await sessions.readTranscript(id, { fromLine: 2, limit: 2 });
    expect(page.lines).toHaveLength(2);
    expect(page.nextLine).toBe(4);
    expect((page.lines[0] as CustomEntry<{ i: number }>).data?.i).toBe(0);

    // Resume from the returned cursor.
    const rest = await sessions.readTranscript(id, { fromLine: page.nextLine });
    expect(rest.lines).toHaveLength(3);
    expect(rest.nextLine).toBe(7);
  });

  test("a missing transcript reads as empty at line 1", async () => {
    const res = await sessions.readTranscript("0190a1b2-0000-7000-8000-00000000ffff");
    expect(res.lines).toHaveLength(0);
    expect(res.nextLine).toBe(1);
  });

  // M3: sessions are engine-owned, but the same hybrid-ownership filesystem lets
  // a human corrupt a session file, and both reads back RPCs (session.transcript
  // / session.get) — a single bad byte must not throw the whole read.
  test("readTranscript skips a malformed line instead of throwing", async () => {
    const id = "0190a1b2-0000-7000-8000-000000000010";
    await sessions.create({ id, ...base });
    const entry: CustomEntry<Record<string, never>> = {
      type: "custom",
      id: "aaaaaaaa",
      timestamp: "2026-01-01T00:00:00Z",
      customType: "test:tick",
      data: {},
    };
    await sessions.appendEntry(id, entry);
    await appendFile(paths.sessionTranscript(id), "{ broken line\n"); // a human fat-fingers a line

    const { lines } = await sessions.readTranscript(id);
    expect(lines).toHaveLength(2); // header + the one valid entry; bad line skipped
  });

  test("getMeta returns null + warns on a corrupt meta (no throw out of the read)", async () => {
    const log = new CapturingLogger();
    const lenient = new SessionStore(paths, new KeyedMutex(), log);
    const id = "0190a1b2-0000-7000-8000-000000000011";
    await mkdir(paths.sessionsDir, { recursive: true });
    await writeFile(paths.sessionMeta(id), "{ not json");

    expect(await lenient.getMeta(id)).toBeNull();
    expect(log.warns.some((w) => w.includes(id) && w.includes("unparseable"))).toBe(true);
  });
});
