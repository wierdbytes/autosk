import { mkdtempSync, readFileSync, rmSync, writeFileSync, chmodSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createSpawnPi } from "../src/pi.ts";

const tmp = mkdtempSync(join(tmpdir(), "autosk-spawn-pi-test-"));
process.on("exit", () => rmSync(tmp, { recursive: true, force: true }));

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(message);
}

function writeFakePi(name: string, source: string): string {
  const path = join(tmp, name);
  writeFileSync(path, `#!/usr/bin/env node\n${source}`);
  chmodSync(path, 0o755);
  return path;
}

const successOut = join(tmp, "success.json");
const successPi = writeFakePi(
  "fake-pi-success.js",
  String.raw`
const fs = require("node:fs");
const chunks = [];
process.stdin.on("data", (chunk) => chunks.push(chunk));
process.stdin.on("end", () => {
  const lines = Buffer.concat(chunks).toString("utf8").trim().split("\n").filter(Boolean);
  const commands = lines.map((line) => JSON.parse(line));
  fs.writeFileSync(process.env.FAKE_PI_OUT, JSON.stringify({ args: process.argv.slice(2), commands }));
  const first = commands[0];
  process.stdout.write(JSON.stringify({ type: "response", id: first.id, command: "prompt", success: true }) + "\n");
  process.stdout.write(JSON.stringify({ type: "agent_start" }) + "\n");
  process.stdout.write(JSON.stringify({ type: "agent_end", messages: [] }) + "\n");
});
`,
);

const successResult = await createSpawnPi({
  cwd: tmp,
  env: { ...process.env, FAKE_PI_OUT: successOut },
  piBin: successPi,
})({
  firstMessage: "review this",
  model: "provider/model",
  thinking: "low",
  extensions: ["ext-a"],
  skills: ["skill-a"],
  extraArgs: ["--no-session"],
});

assert(successResult.exitCode === 0, `success exitCode=${successResult.exitCode} error=${successResult.error}`);
assert(successResult.agentEnd === true, "expected agentEnd=true");
const success = JSON.parse(readFileSync(successOut, "utf8"));
assert(JSON.stringify(success.args) === JSON.stringify([
  "--mode",
  "rpc",
  "--model",
  "provider/model",
  "--thinking",
  "low",
  "-e",
  "ext-a",
  "--skill",
  "skill-a",
  "--no-session",
]), `unexpected args: ${JSON.stringify(success.args)}`);
assert(success.commands.length === 1, `expected one command, got ${success.commands.length}`);
assert(success.commands[0].type === "prompt", `expected prompt command, got ${success.commands[0].type}`);
assert(success.commands[0].message === "review this", `unexpected prompt message: ${success.commands[0].message}`);
assert(typeof success.commands[0].id === "string" && success.commands[0].id.length > 0, "expected prompt id");

const failurePi = writeFakePi(
  "fake-pi-failure.js",
  String.raw`
process.stderr.write("fake pi failed before agent_end\n");
process.exit(42);
`,
);
const failureResult = await createSpawnPi({ cwd: tmp, env: process.env, piBin: failurePi })({
  firstMessage: "will fail",
});
assert(failureResult.exitCode === 42, `failure exitCode=${failureResult.exitCode}`);
assert(failureResult.agentEnd === false, "expected agentEnd=false");
assert(failureResult.error?.includes("before agent_end"), `missing diagnostic: ${failureResult.error}`);
assert(failureResult.stderr?.includes("fake pi failed"), "stderr was not captured");

console.log("spawnPi RPC tests passed");
