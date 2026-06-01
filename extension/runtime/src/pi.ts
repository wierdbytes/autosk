import { spawn } from "node:child_process";
import { StringDecoder } from "node:string_decoder";
import type { PiResult, PiSpawnOpts } from "@autosk/agent-sdk";

const DEFAULT_CAPTURE_LIMIT = 64 * 1024;
const PROMPT_ID = "autosk-spawn-pi-prompt-1";

interface SpawnPiRuntimeOpts {
  cwd: string;
  env: NodeJS.ProcessEnv;
  piBin?: string;
  captureLimit?: number;
}

class TailCapture {
  private value = "";
  private readonly limit: number;

  constructor(limit: number) {
    this.limit = limit;
  }

  append(chunk: string): void {
    if (this.limit <= 0 || chunk.length === 0) return;
    this.value += chunk;
    if (this.value.length > this.limit) {
      this.value = this.value.slice(this.value.length - this.limit);
    }
  }

  snapshot(): string {
    return this.value;
  }
}

function buildPiArgs(opts: PiSpawnOpts): string[] {
  const args: string[] = ["--mode", "rpc"];
  if (opts.model) args.push("--model", opts.model);
  if (opts.thinking) args.push("--thinking", opts.thinking);
  for (const e of opts.extensions ?? []) args.push("-e", e);
  for (const s of opts.skills ?? []) args.push("--skill", s);
  if (opts.extraArgs) args.push(...opts.extraArgs);
  return args;
}

function stripRecordTerminator(line: string): string {
  return line.endsWith("\r") ? line.slice(0, -1) : line;
}

function attachJsonlReader(
  stream: NodeJS.ReadableStream,
  onLine: (line: string) => void,
): void {
  const decoder = new StringDecoder("utf8");
  let buffer = "";

  stream.on("data", (chunk: string | Buffer) => {
    buffer += typeof chunk === "string" ? chunk : decoder.write(chunk);
    for (;;) {
      const newlineIndex = buffer.indexOf("\n");
      if (newlineIndex === -1) break;
      const line = stripRecordTerminator(buffer.slice(0, newlineIndex));
      buffer = buffer.slice(newlineIndex + 1);
      onLine(line);
    }
  });

  stream.on("end", () => {
    buffer += decoder.end();
    if (buffer.length > 0) {
      onLine(stripRecordTerminator(buffer));
      buffer = "";
    }
  });
}

function isFireAndForgetExtensionRequest(method: unknown): boolean {
  return (
    method === "notify" ||
    method === "setStatus" ||
    method === "setWidget" ||
    method === "setTitle" ||
    method === "set_editor_text"
  );
}

function appendDiagnostic(base: string, stderr: string): string {
  const trimmed = stderr.trim();
  if (!trimmed) return base;
  return `${base} (stderr: ${trimmed})`;
}

export function createSpawnPi(runtime: SpawnPiRuntimeOpts) {
  const piBin = runtime.piBin || "pi";
  const captureLimit = runtime.captureLimit ?? DEFAULT_CAPTURE_LIMIT;

  return (opts: PiSpawnOpts): Promise<PiResult> =>
    new Promise((resolve) => {
      const stdout = new TailCapture(captureLimit);
      const stderr = new TailCapture(captureLimit);
      const args = buildPiArgs(opts);
      const child = spawn(piBin, args, {
        cwd: runtime.cwd,
        env: runtime.env,
        stdio: ["pipe", "pipe", "pipe"],
      });

      let settled = false;
      let startError: Error | undefined;
      let promptAccepted = !opts.firstMessage;
      let promptRejected = "";
      let sawAgentEnd = false;
      let protocolError = "";
      let stdinClosed = false;

      const closeStdin = (): void => {
        if (stdinClosed) return;
        stdinClosed = true;
        child.stdin.end();
      };

      const writeJson = (value: unknown): void => {
        if (stdinClosed || child.stdin.destroyed) return;
        child.stdin.write(`${JSON.stringify(value)}\n`, (err) => {
          if (err && !protocolError) {
            protocolError = `write pi stdin failed: ${err.message}`;
          }
        });
      };

      attachJsonlReader(child.stdout, (line) => {
        stdout.append(line + "\n");
        if (line.trim() === "") return;

        let msg: Record<string, unknown>;
        try {
          msg = JSON.parse(line) as Record<string, unknown>;
        } catch (err) {
          if (!protocolError) {
            protocolError = `pi stdout was not valid JSONL: ${(err as Error).message}`;
          }
          return;
        }

        if (msg.type === "response" && msg.id === PROMPT_ID) {
          promptAccepted = msg.success === true;
          if (!promptAccepted) {
            promptRejected = typeof msg.error === "string" ? msg.error : "prompt rejected";
            closeStdin();
          }
          return;
        }

        if (msg.type === "agent_end") {
          sawAgentEnd = true;
          closeStdin();
          return;
        }

        if (msg.type === "extension_ui_request") {
          const id = msg.id;
          if (typeof id !== "string" || isFireAndForgetExtensionRequest(msg.method)) return;
          writeJson({ type: "extension_ui_response", id, cancelled: true });
        }
      });

      child.stderr.on("data", (chunk: string | Buffer) => {
        stderr.append(chunk.toString());
      });

      child.stdin.on("error", (err) => {
        if (!protocolError) {
          protocolError = `pi stdin error: ${err.message}`;
        }
      });

      child.on("error", (err) => {
        startError = err;
      });

      child.on("close", (code) => {
        if (settled) return;
        settled = true;

        const processExitCode = code ?? -1;
        let exitCode = processExitCode;
        let error: string | undefined;

        if (startError) {
          exitCode = -1;
          error = `failed to start ${piBin}: ${startError.message}`;
        } else if (promptRejected) {
          exitCode = processExitCode === 0 ? 1 : processExitCode;
          error = `pi prompt rejected: ${promptRejected}`;
        } else if (opts.firstMessage && !promptAccepted) {
          exitCode = processExitCode === 0 ? 1 : processExitCode;
          error = "pi exited before acknowledging the prompt";
        } else if (opts.firstMessage && !sawAgentEnd) {
          exitCode = processExitCode === 0 ? 1 : processExitCode;
          error = `pi exited before agent_end (code ${processExitCode})`;
        } else if (processExitCode !== 0) {
          error = `pi exited with code ${processExitCode}`;
        } else if (protocolError) {
          exitCode = 1;
          error = protocolError;
        }

        if (error) {
          error = appendDiagnostic(error, stderr.snapshot());
        }

        resolve({
          exitCode,
          stdout: stdout.snapshot(),
          stderr: stderr.snapshot(),
          error,
          agentEnd: sawAgentEnd,
        });
      });

      if (opts.firstMessage) {
        writeJson({ id: PROMPT_ID, type: "prompt", message: opts.firstMessage });
      } else {
        closeStdin();
      }
    });
}
