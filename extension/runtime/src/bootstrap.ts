#!/usr/bin/env node
/**
 * @autosk/agent-runtime — Node bootstrapper for custom-runner agents.
 *
 * Spawned by the autosk daemon's executor. CLI:
 *
 *   node --import tsx <this-script> --pkg <npm-name> --runner <abs-path>
 *
 * Reads a single JSON RunContextSeed from stdin, imports the runner
 * module, constructs a RunContext (with cli/stepNext/spawnPi helpers),
 * and awaits the runner's default export.
 *
 * Exits 0 on clean completion; non-zero on any error (the autosk
 * executor surfaces stderr to its transcript).
 *
 * The runner is expected to call `ctx.stepNext(...)` exactly once
 * before the function resolves. The bootstrapper does NOT validate
 * that — the autosk executor checks step_signals after the process
 * exits and fails the run if no signal was emitted.
 */

import { spawn } from "node:child_process";
import { readFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import type {
  CliResult,
  RunAgent,
  RunContext,
} from "@autosk/agent-sdk";
import { createSpawnPi } from "./pi.js";

/** Bumped on breaking changes to the seed JSON shape. Matches
 * SeedSchemaVersion on the Go side. */
const SUPPORTED_SCHEMA_VERSION = 1;

interface RunContextSeed {
  schema_version: number;
  task: RunContext["task"];
  step: RunContext["step"];
  workflow: RunContext["workflow"];
  comments: RunContext["comments"];
  transitions: RunContext["transitions"];
  project_root: string;
  job_id: string;
  agent_name: string;
  agent_version: string;
  agent_install: string;
}

function parseArgs(argv: string[]): { pkg: string; runner: string } {
  let pkg = "";
  let runner = "";
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--pkg") pkg = argv[++i] ?? "";
    else if (a === "--runner") runner = argv[++i] ?? "";
  }
  if (!pkg || !runner) {
    throw new Error("usage: autosk-agent-runtime --pkg <name> --runner <path>");
  }
  return { pkg, runner };
}

async function readStdinJSON<T>(): Promise<T> {
  const chunks: Buffer[] = [];
  for await (const chunk of process.stdin) {
    chunks.push(typeof chunk === "string" ? Buffer.from(chunk) : chunk);
  }
  const body = Buffer.concat(chunks).toString("utf8").trim();
  if (!body) throw new Error("empty stdin: expected RunContextSeed JSON");
  try {
    return JSON.parse(body) as T;
  } catch (e) {
    throw new Error(`stdin is not valid JSON: ${(e as Error).message}`);
  }
}

function buildContext(seed: RunContextSeed): RunContext {
  const cwd = seed.project_root || process.cwd();
  const autoskBin = process.env.AUTOSK_BIN ?? "autosk";
  const piBin = process.env.PI_BIN ?? "pi";
  const env = { ...process.env, AUTOSK_AGENT: seed.agent_name };

  const cli = (args: string[]): Promise<CliResult> =>
    new Promise((resolve, reject) => {
      const child = spawn(autoskBin, args, { cwd, env });
      let stdout = "";
      let stderr = "";
      child.stdout.on("data", (d) => (stdout += d.toString()));
      child.stderr.on("data", (d) => (stderr += d.toString()));
      child.on("error", reject);
      child.on("close", (code) => resolve({ stdout, stderr, code: code ?? -1 }));
    });

  const stepNext = async (to: string): Promise<void> => {
    const { code, stderr } = await cli([
      "step",
      "next",
      seed.task.id,
      "--to",
      to,
    ]);
    if (code !== 0) {
      throw new Error(`autosk step next failed (code ${code}): ${stderr.trim()}`);
    }
  };

  const spawnPi = createSpawnPi({ cwd, env, piBin });

  return {
    task: seed.task,
    step: seed.step,
    workflow: seed.workflow,
    comments: seed.comments,
    transitions: seed.transitions,
    projectRoot: cwd,
    jobId: seed.job_id,
    agentName: seed.agent_name,
    agentVersion: seed.agent_version,
    agentInstall: seed.agent_install,
    cli,
    stepNext,
    spawnPi,
  };
}

async function loadRunner(runnerPath: string): Promise<RunAgent> {
  const url = pathToFileURL(runnerPath).href;
  const mod = (await import(url)) as { default?: RunAgent };
  if (typeof mod.default !== "function") {
    throw new Error(
      `runner at ${runnerPath} does not have a function as default export`,
    );
  }
  return mod.default;
}

async function main(): Promise<void> {
  const { pkg, runner } = parseArgs(process.argv.slice(2));
  // Sanity-check the runner exists before reading stdin.
  await readFile(runner).catch((err) => {
    throw new Error(`runner not readable at ${runner}: ${err.message}`);
  });

  const seed = await readStdinJSON<RunContextSeed>();
  if (seed.schema_version !== SUPPORTED_SCHEMA_VERSION) {
    throw new Error(
      `seed schema_version=${seed.schema_version} but this runtime expects ${SUPPORTED_SCHEMA_VERSION}`,
    );
  }
  if (seed.agent_name !== pkg) {
    throw new Error(
      `seed.agent_name=${seed.agent_name} does not match --pkg=${pkg}`,
    );
  }
  const runAgent = await loadRunner(runner);
  const ctx = buildContext(seed);
  await runAgent(ctx);
}

main().catch((err) => {
  // eslint-disable-next-line no-console
  console.error(
    err instanceof Error ? `${err.message}\n${err.stack ?? ""}` : String(err),
  );
  process.exit(1);
});
