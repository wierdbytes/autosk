/**
 * Golden tests pinning the on-disk byte formats (plan §3.1, §3.2).
 *
 * These files are the **new public contract**: any byte-level change to
 * `task.json`, `comments.jsonl`, a session meta, or a transcript line MUST fail
 * here. The expected strings are written out in full (not regenerated from the
 * serialiser) so the test catches accidental reordering / spacing / newline
 * drift, not just a serialiser refactor.
 */

import { describe, expect, test } from "bun:test";
import { readFile } from "node:fs/promises";

import {
  SessionStore,
  TaskStore,
  ProjectPaths,
  parseCommentsLenient,
  parseTask,
  parseTranscriptLenient,
  serializeComments,
  serializeSessionHeader,
  serializeSessionMeta,
  serializeTask,
  serializeTranscriptLine,
  type StoredTask,
} from "../src/index.ts";
import type {
  Comment,
  MessageEntry,
  SessionHeader,
  SessionMeta,
  TransitEntry,
} from "@autosk/sdk";
import { KeyedMutex } from "../src/index.ts";
import { tempDir } from "./helpers.ts";

// ---------------------------------------------------------------------------
// task.json
// ---------------------------------------------------------------------------

const GOLDEN_TASK: StoredTask = {
  id: "ask-3f9b2c",
  title: "Implement auth module",
  description: "Wire up the login flow.",
  status: "work",
  workflow: "feature-dev",
  step: "review",
  blocked_by: ["ask-aa11bb"],
  metadata: {},
  created_at: "2026-06-12T09:00:00Z",
  updated_at: "2026-06-12T09:30:00Z",
};

const GOLDEN_TASK_JSON = `{
  "id": "ask-3f9b2c",
  "title": "Implement auth module",
  "description": "Wire up the login flow.",
  "status": "work",
  "workflow": "feature-dev",
  "step": "review",
  "blocked_by": [
    "ask-aa11bb"
  ],
  "created_at": "2026-06-12T09:00:00Z",
  "updated_at": "2026-06-12T09:30:00Z"
}
`;

describe("task.json golden", () => {
  test("serialises with fixed key order + 2-space indent + trailing newline", () => {
    expect(serializeTask(GOLDEN_TASK)).toBe(GOLDEN_TASK_JSON);
  });

  test("a fresh unenrolled task: null workflow/step, empty blocked_by", () => {
    const fresh: StoredTask = {
      id: "ask-000001",
      title: "New",
      description: "",
      status: "new",
      workflow: null,
      step: null,
      blocked_by: [],
      metadata: {},
      created_at: "2026-06-12T09:00:00Z",
      updated_at: "2026-06-12T09:00:00Z",
    };
    expect(serializeTask(fresh)).toBe(`{
  "id": "ask-000001",
  "title": "New",
  "description": "",
  "status": "new",
  "workflow": null,
  "step": null,
  "blocked_by": [],
  "created_at": "2026-06-12T09:00:00Z",
  "updated_at": "2026-06-12T09:00:00Z"
}
`);
  });

  test("key order is independent of how the object was built", () => {
    const scrambled = {
      updated_at: GOLDEN_TASK.updated_at,
      blocked_by: GOLDEN_TASK.blocked_by,
      id: GOLDEN_TASK.id,
      status: GOLDEN_TASK.status,
      title: GOLDEN_TASK.title,
      step: GOLDEN_TASK.step,
      created_at: GOLDEN_TASK.created_at,
      description: GOLDEN_TASK.description,
      workflow: GOLDEN_TASK.workflow,
      metadata: GOLDEN_TASK.metadata,
    } as StoredTask;
    expect(serializeTask(scrambled)).toBe(GOLDEN_TASK_JSON);
  });

  test("non-empty metadata serialises in the fixed slot (after blocked_by, before created_at)", () => {
    const withMeta: StoredTask = {
      ...GOLDEN_TASK,
      metadata: { step_visits: { dev: 2, review: 1 }, note: "keep" },
    };
    expect(serializeTask(withMeta)).toBe(`{
  "id": "ask-3f9b2c",
  "title": "Implement auth module",
  "description": "Wire up the login flow.",
  "status": "work",
  "workflow": "feature-dev",
  "step": "review",
  "blocked_by": [
    "ask-aa11bb"
  ],
  "metadata": {
    "step_visits": {
      "dev": 2,
      "review": 1
    },
    "note": "keep"
  },
  "created_at": "2026-06-12T09:00:00Z",
  "updated_at": "2026-06-12T09:30:00Z"
}
`);
  });

  test("empty metadata omits the key (pre-existing files round-trip unchanged)", () => {
    expect(serializeTask(GOLDEN_TASK)).not.toContain("metadata");
    // A task.json written before metadata existed (no key) parses to {} and
    // re-serialises to the identical bytes.
    const reparsed = parseTask(GOLDEN_TASK_JSON);
    expect(reparsed.metadata).toEqual({});
    expect(serializeTask(reparsed)).toBe(GOLDEN_TASK_JSON);
  });

  test("parseTask is defensive: missing/corrupt/non-object metadata becomes {}", () => {
    const base = JSON.parse(GOLDEN_TASK_JSON) as Record<string, unknown>;
    expect(parseTask(JSON.stringify({ ...base, metadata: undefined })).metadata).toEqual({});
    expect(parseTask(JSON.stringify({ ...base, metadata: null })).metadata).toEqual({});
    expect(parseTask(JSON.stringify({ ...base, metadata: [1, 2] })).metadata).toEqual({});
    expect(parseTask(JSON.stringify({ ...base, metadata: "oops" })).metadata).toEqual({});
    expect(parseTask(JSON.stringify({ ...base, metadata: { a: 1 } })).metadata).toEqual({ a: 1 });
  });

  test("TaskStore writes the golden bytes to disk", async () => {
    const dir = tempDir();
    try {
      const store = new TaskStore(new ProjectPaths(dir.path));
      await store.writeTask(GOLDEN_TASK);
      const bytes = await readFile(new ProjectPaths(dir.path).taskJson("ask-3f9b2c"), "utf8");
      expect(bytes).toBe(GOLDEN_TASK_JSON);
    } finally {
      dir.cleanup();
    }
  });
});

// ---------------------------------------------------------------------------
// comments.jsonl
// ---------------------------------------------------------------------------

const GOLDEN_COMMENTS: Comment[] = [
  {
    id: "cm-111111",
    author: "alice",
    text: "first",
    created_at: "2026-06-12T09:00:00Z",
    updated_at: "2026-06-12T09:00:00Z",
  },
  {
    id: "cm-222222",
    author: "@autosk/pi-agent/dev",
    text: "second",
    created_at: "2026-06-12T09:05:00Z",
    updated_at: "2026-06-12T09:06:00Z",
  },
];

const GOLDEN_COMMENTS_JSONL =
  '{"id":"cm-111111","author":"alice","text":"first","created_at":"2026-06-12T09:00:00Z","updated_at":"2026-06-12T09:00:00Z"}\n' +
  '{"id":"cm-222222","author":"@autosk/pi-agent/dev","text":"second","created_at":"2026-06-12T09:05:00Z","updated_at":"2026-06-12T09:06:00Z"}\n';

describe("comments.jsonl golden", () => {
  test("one compact object per line, each newline-terminated", () => {
    expect(serializeComments(GOLDEN_COMMENTS)).toBe(GOLDEN_COMMENTS_JSONL);
  });

  test("an empty comment list serialises to the empty file", () => {
    expect(serializeComments([])).toBe("");
  });

  test("the golden bytes round-trip through the lenient parser", () => {
    const { comments, skipped } = parseCommentsLenient(GOLDEN_COMMENTS_JSONL);
    expect(comments).toEqual(GOLDEN_COMMENTS);
    expect(skipped).toBe(0);
  });

  test("a malformed line is skipped and counted, valid lines survive", () => {
    const withGarbage = GOLDEN_COMMENTS_JSONL.replace(
      '{"id":"cm-222222"',
      '{ not json\n{"id":"cm-222222"',
    );
    const { comments, skipped } = parseCommentsLenient(withGarbage);
    expect(comments.map((c) => c.id)).toEqual(["cm-111111", "cm-222222"]);
    expect(skipped).toBe(1);
  });
});

// ---------------------------------------------------------------------------
// session meta (sessions/<id>.json)
// ---------------------------------------------------------------------------

const SESSION_ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b";

const GOLDEN_SESSION_QUEUED: SessionMeta = {
  id: SESSION_ID,
  kind: "task",
  task_id: "ask-3f9b2c",
  workflow: "feature-dev",
  step: "dev",
  agent: "@autosk/pi-agent/dev",
  status: "queued",
  started_at: null,
  ended_at: null,
};

const GOLDEN_SESSION_QUEUED_JSON = `{
  "id": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
  "kind": "task",
  "task_id": "ask-3f9b2c",
  "workflow": "feature-dev",
  "step": "dev",
  "agent": "@autosk/pi-agent/dev",
  "status": "queued",
  "started_at": null,
  "ended_at": null
}
`;

const GOLDEN_SESSION_FAILED_JSON = `{
  "id": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
  "kind": "task",
  "task_id": "ask-3f9b2c",
  "workflow": "feature-dev",
  "step": "dev",
  "agent": "@autosk/pi-agent/dev",
  "status": "failed",
  "error": "daemon_restart",
  "started_at": "2026-06-12T09:00:00Z",
  "ended_at": "2026-06-12T09:01:00Z"
}
`;

const GOLDEN_SESSION_USAGE_JSON = `{
  "id": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
  "kind": "task",
  "task_id": "ask-3f9b2c",
  "workflow": "feature-dev",
  "step": "dev",
  "agent": "@autosk/pi-agent/dev",
  "status": "done",
  "started_at": "2026-06-12T09:00:00Z",
  "ended_at": "2026-06-12T09:01:00Z",
  "usage": {
    "input": 10,
    "output": 5,
    "cacheRead": 2,
    "cacheWrite": 1,
    "totalTokens": 15,
    "cost": {
      "input": 0.1,
      "output": 0.2,
      "cacheRead": 0,
      "cacheWrite": 0,
      "total": 0.3
    }
  }
}
`;

describe("session meta golden", () => {
  test("queued meta: no error key, null timestamps", () => {
    expect(serializeSessionMeta(GOLDEN_SESSION_QUEUED)).toBe(GOLDEN_SESSION_QUEUED_JSON);
  });

  test("failed meta: error key present, between status and started_at", () => {
    const failed: SessionMeta = {
      ...GOLDEN_SESSION_QUEUED,
      status: "failed",
      error: "daemon_restart",
      started_at: "2026-06-12T09:00:00Z",
      ended_at: "2026-06-12T09:01:00Z",
    };
    expect(serializeSessionMeta(failed)).toBe(GOLDEN_SESSION_FAILED_JSON);
  });

  test("terminal meta: canonical aggregate follows timestamps", () => {
    const done: SessionMeta = {
      ...GOLDEN_SESSION_QUEUED,
      status: "done",
      started_at: "2026-06-12T09:00:00Z",
      ended_at: "2026-06-12T09:01:00Z",
      usage: {
        input: 10,
        output: 5,
        cacheRead: 2,
        cacheWrite: 1,
        totalTokens: 15,
        cost: { input: 0.1, output: 0.2, cacheRead: 0, cacheWrite: 0, total: 0.3 },
      },
    };
    expect(serializeSessionMeta(done)).toBe(GOLDEN_SESSION_USAGE_JSON);
  });
});

// ---------------------------------------------------------------------------
// transcript (sessions/<id>.jsonl): header line + entries
// ---------------------------------------------------------------------------

const GOLDEN_HEADER: SessionHeader = {
  type: "session",
  version: 1,
  id: SESSION_ID,
  kind: "task",
  task_id: "ask-3f9b2c",
  workflow: "feature-dev",
  step: "dev",
  agent: "@autosk/pi-agent/dev",
  timestamp: "2026-06-12T09:00:00Z",
  cwd: "/repo",
};

const GOLDEN_HEADER_LINE =
  '{"type":"session","version":1,"id":"0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",' +
  '"kind":"task","task_id":"ask-3f9b2c","workflow":"feature-dev","step":"dev","agent":"@autosk/pi-agent/dev",' +
  '"timestamp":"2026-06-12T09:00:00Z","cwd":"/repo"}';

const GOLDEN_TRANSIT: TransitEntry = {
  type: "custom",
  id: "11111111",
  timestamp: "2026-06-12T09:01:00Z",
  customType: "autosk:transit",
  data: { to: { step: "review" }, from: { workflow: "feature-dev", step: "dev" } },
};

const GOLDEN_TRANSIT_LINE =
  '{"type":"custom","id":"11111111","timestamp":"2026-06-12T09:01:00Z",' +
  '"customType":"autosk:transit","data":{"to":{"step":"review"},"from":{"workflow":"feature-dev","step":"dev"}}}';

const GOLDEN_MESSAGE: MessageEntry = {
  type: "message",
  id: "a1b2c3d4",
  timestamp: "2026-06-12T09:00:30Z",
  message: { role: "user", content: "Hello", timestamp: 1_760_000_000_000 },
};

const GOLDEN_MESSAGE_LINE =
  '{"type":"message","id":"a1b2c3d4","timestamp":"2026-06-12T09:00:30Z",' +
  '"message":{"role":"user","content":"Hello","timestamp":1760000000000}}';

describe("transcript golden", () => {
  test("header line: canonical key order, compact, no trailing newline", () => {
    expect(serializeSessionHeader(GOLDEN_HEADER)).toBe(GOLDEN_HEADER_LINE);
    expect(serializeTranscriptLine(GOLDEN_HEADER)).toBe(GOLDEN_HEADER_LINE);
  });

  test("engine structural entry (autosk:transit) line", () => {
    expect(serializeTranscriptLine(GOLDEN_TRANSIT)).toBe(GOLDEN_TRANSIT_LINE);
  });

  test("pi message entry line", () => {
    expect(serializeTranscriptLine(GOLDEN_MESSAGE)).toBe(GOLDEN_MESSAGE_LINE);
  });

  test("parseTranscriptLenient round-trips lines and counts a skipped one", () => {
    const text = `${GOLDEN_HEADER_LINE}\n${GOLDEN_MESSAGE_LINE}\n{ broken line\n${GOLDEN_TRANSIT_LINE}\n`;
    const { lines, skipped } = parseTranscriptLenient(text);
    expect(lines).toHaveLength(3); // header + message + transit; broken line dropped
    expect(skipped).toBe(1);
  });

  test("SessionStore.create writes header line 1; appendEntry adds entries", async () => {
    const dir = tempDir();
    try {
      const paths = new ProjectPaths(dir.path);
      const sessions = new SessionStore(paths, new KeyedMutex());
      await sessions.create({
        id: SESSION_ID,
        task_id: "ask-3f9b2c",
        workflow: "feature-dev",
        step: "dev",
        agent: "@autosk/pi-agent/dev",
        cwd: "/repo",
        timestamp: "2026-06-12T09:00:00Z",
      });
      await sessions.appendEntry(SESSION_ID, GOLDEN_MESSAGE);
      await sessions.appendEntry(SESSION_ID, GOLDEN_TRANSIT);

      const meta = await readFile(paths.sessionMeta(SESSION_ID), "utf8");
      expect(meta).toBe(GOLDEN_SESSION_QUEUED_JSON);

      const transcript = await readFile(paths.sessionTranscript(SESSION_ID), "utf8");
      expect(transcript).toBe(
        `${GOLDEN_HEADER_LINE}\n${GOLDEN_MESSAGE_LINE}\n${GOLDEN_TRANSIT_LINE}\n`,
      );
    } finally {
      dir.cleanup();
    }
  });
});
