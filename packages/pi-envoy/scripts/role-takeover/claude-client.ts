#!/usr/bin/env bun
/**
 * claude-client.ts — the `claude` process of one Claude Code session, for
 * `scripts/smoke-role-takeover.sh` and `scripts/e2e/nats-callout-sessions.sh`.
 *
 * It spawns the real channel server (`packages/claude-envoy/bin/envoy-channel.ts`) as its child,
 * exactly as Claude Code spawns its stdio MCP server, and speaks MCP to it over that pipe. Being
 * the server's parent is the point: the server watches
 * `${CLAUDE_PLUGIN_DATA}/sessions/<its parent's pid>/session-id` for the id Claude Code mints on
 * `/clear` (`sessionHandoffFile`), and this process is that parent, so `{"op":"clear"}` writes
 * that file the way the SessionStart hook does and the server adopts the handoff on its own.
 *
 * Nothing of the server is faked: it registers, heartbeats, claims and deregisters over HTTP to
 * the smoke's recording proxy, and over NATS to the smoke's natsd. One process is one client
 * instance, which is what the listener's rule turns on (R5, R6 of dispatch://INFRA-404/spec).
 *
 * Usage:
 *   bun claude-client.ts --server <bin/envoy-channel.ts> --session <id> --state-dir <dir>
 *                        --config-dir <dir> --heartbeat-ms <n> [--stderr <file>]
 *                        [--env-file <file>] [--inbox <socket path>] [--withhold-inbox-answers true]
 *
 * `--env-file` names a file of NAME=VALUE lines added to the server's environment, which is
 * otherwise only the variables below: a session that trades for a broker token is given the
 * variables the trade reads that way (AGENT_SECRETS_URL, the machine identity, NATS_AUTH), and
 * nothing of this process's own environment reaches the server. `--inbox` plays the inbox of
 * `legion claude-bridge` (LEGION_CLAUDE_INBOX): this process serves that unix socket, takes each
 * delivery the server writes there (`{id, content}`) and answers it taken (`{id, ok: true}`), as
 * the bridge does once Claude Code took it, so the server writes every delivery there.
 * `--withhold-inbox-answers true` takes each delivery and never answers it, so the server never
 * tells the listener the delivery was taken and the listener acks nothing.
 *
 * Its input ending is Claude Code going away: it closes the channel server's stdin, as Claude Code
 * does, waits up to 10 s for the server to deregister and exit, and exits.
 *
 * Protocol: one JSON object per line on stdin, one answer per line on stdout, matched by `id`.
 *
 *   {"id":"1","op":"tool","name":"envoy_role_set","args":{"role":"reviewer"}}
 *   {"id":"2","op":"clear","session":"<the id Claude Code minted>"}
 *   {"id":"3","op":"kill"}          a crash: the registration outlives the process
 *   {"id":"4","op":"deliveries"}    every delivery the inbox took, its content, in order
 */
import { mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { join } from "node:path";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { settled } from "@legion/test-fixtures/wait";
import type { Subprocess } from "bun";

interface Options {
  readonly server: string;
  readonly session: string;
  readonly stateDirectory: string;
  readonly configDirectory: string;
  readonly heartbeatMs: string;
  readonly stderr: string | undefined;
  readonly envFile: string | undefined;
  readonly inbox: string | undefined;
  readonly withholdInboxAnswers: boolean;
}

interface Command {
  readonly id?: string;
  readonly op?: string;
  readonly name?: string;
  readonly args?: Record<string, unknown>;
  readonly session?: string;
}

function parseOptions(argv: readonly string[]): Options {
  const values = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (name === undefined || !name.startsWith("--") || value === undefined) {
      throw new Error(`claude-client: expected --name value pairs, got ${argv.join(" ")}`);
    }
    values.set(name.slice(2), value);
  }
  const server = values.get("server");
  const session = values.get("session");
  const stateDirectory = values.get("state-dir");
  const configDirectory = values.get("config-dir");
  const heartbeatMs = values.get("heartbeat-ms");
  if (server === undefined) throw new Error("claude-client: --server is required");
  if (session === undefined) throw new Error("claude-client: --session is required");
  if (stateDirectory === undefined) throw new Error("claude-client: --state-dir is required");
  if (configDirectory === undefined) throw new Error("claude-client: --config-dir is required");
  if (heartbeatMs === undefined) throw new Error("claude-client: --heartbeat-ms is required");
  return {
    server,
    session,
    stateDirectory,
    configDirectory,
    heartbeatMs,
    stderr: values.get("stderr"),
    envFile: values.get("env-file"),
    inbox: values.get("inbox"),
    withholdInboxAnswers: values.get("withhold-inbox-answers") === "true",
  };
}

/** The NAME=VALUE lines of `file`, blank lines and `#` comments skipped. */
function readEnvFile(file: string | undefined): Record<string, string> {
  if (file === undefined) return {};
  const variables: Record<string, string> = {};
  for (const line of readFileSync(file, "utf8").split("\n")) {
    const text = line.trim();
    if (text === "" || text.startsWith("#")) continue;
    const equals = text.indexOf("=");
    if (equals <= 0) throw new Error(`claude-client: ${file} holds a line with no NAME=: ${text}`);
    variables[text.slice(0, equals)] = text.slice(equals + 1);
  }
  return variables;
}

/** Every delivery the inbox took, its content, in the order the server wrote them. */
const delivered: string[] = [];

/**
 * Serves `path` as the bridge's inbox does: one JSON line `{id, content}` per delivery, each
 * answered `{id, ok: true}` on the connection it came on.
 */
function serveInbox(path: string): Promise<void> {
  const inbox = createServer((connection) => {
    let buffered = "";
    connection.on("data", (chunk) => {
      buffered += chunk.toString("utf8");
      for (let newline = buffered.indexOf("\n"); newline !== -1; newline = buffered.indexOf("\n")) {
        const line = JSON.parse(buffered.slice(0, newline)) as {
          readonly id: string;
          readonly content: string;
        };
        buffered = buffered.slice(newline + 1);
        delivered.push(line.content);
        // Withheld, the server is never told the delivery was taken: it sends no `done`, and
        // the listener acks nothing (nats-callout-sessions.sh's withhold-ack control).
        if (!options.withholdInboxAnswers) {
          connection.write(`${JSON.stringify({ id: line.id, ok: true })}\n`);
        }
      }
    });
  });
  const listening = Promise.withResolvers<void>();
  inbox.once("error", listening.reject);
  inbox.listen(path, listening.resolve);
  return listening.promise;
}

const options = parseOptions(process.argv.slice(2));
const { PATH } = process.env;
if (options.inbox !== undefined) await serveInbox(options.inbox);

const child: Subprocess<"pipe", "pipe", "pipe"> = Bun.spawn(["bun", options.server], {
  cwd: options.stateDirectory,
  env: {
    ...bunCacheOff,
    ...readEnvFile(options.envFile),
    PATH: PATH ?? "",
    HOME: options.stateDirectory,
    CLAUDE_CONFIG_DIR: options.configDirectory,
    CLAUDE_CODE_SESSION_ID: options.session,
    CLAUDE_PLUGIN_DATA: options.stateDirectory,
    ENVOY_HEARTBEAT_MS: options.heartbeatMs,
    ...(options.inbox === undefined ? {} : { LEGION_CLAUDE_INBOX: options.inbox }),
  },
  stdin: "pipe",
  stdout: "pipe",
  stderr: "pipe",
});

const answers = new Map<number, unknown>();
const waiters = new Map<number, () => void>();
let serverStderr = "";

void (async () => {
  let buffered = "";
  for await (const chunk of child.stdout.pipeThrough(new TextDecoderStream())) {
    buffered += chunk;
    for (let newline = buffered.indexOf("\n"); newline !== -1; newline = buffered.indexOf("\n")) {
      const message = JSON.parse(buffered.slice(0, newline)) as { readonly id?: unknown };
      buffered = buffered.slice(newline + 1);
      const { id } = message;
      if (typeof id !== "number") continue;
      answers.set(id, message);
      waiters.get(id)?.();
    }
  }
})();

void (async () => {
  for await (const chunk of child.stderr.pipeThrough(new TextDecoderStream())) {
    serverStderr += chunk;
    if (options.stderr !== undefined) await Bun.write(options.stderr, serverStderr);
  }
})();

let nextRequestID = 0;

/** Sends one MCP request and resolves with the server's answer to it. The wait for the answer is
 *  @legion/test-fixtures/wait's, which ends at the smoke's deadline (LEGION_PROOF_DEADLINE_MS,
 *  which smoke-role-takeover.sh's proof_guard sets). */
async function request(method: string, params: unknown): Promise<unknown> {
  nextRequestID += 1;
  const id = nextRequestID;
  const answered = Promise.withResolvers<void>();
  waiters.set(id, answered.resolve);
  child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", id, method, params })}\n`);
  child.stdin.flush();
  try {
    await settled(`the channel server to answer ${method}`, answered.promise);
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error);
    throw new Error(`${reason}: ${serverStderr}`, { cause: error });
  } finally {
    waiters.delete(id);
  }
  return answers.get(id);
}

/** The handoff file Claude Code's SessionStart hook writes for this process's children. */
function writeHandoff(sessionID: string): string {
  const directory = join(options.stateDirectory, "sessions", String(process.pid));
  mkdirSync(directory, { recursive: true });
  const file = join(directory, "session-id");
  const temporary = `${file}.tmp`;
  writeFileSync(temporary, `${sessionID}\n`);
  renameSync(temporary, file);
  return file;
}

async function run(command: Command): Promise<Record<string, unknown>> {
  switch (command.op) {
    case "tool":
      return {
        result: await request("tools/call", { name: command.name, arguments: command.args ?? {} }),
      };
    case "clear": {
      if (command.session === undefined) throw new Error("clear needs the id Claude Code minted");
      return { handoff: writeHandoff(command.session) };
    }
    case "kill": {
      // A crash: SIGKILL leaves the server no chance to deregister, so its registration outlives
      // the process it was made by.
      child.kill("SIGKILL");
      return { exit: await child.exited };
    }
    case "deliveries":
      return { deliveries: [...delivered] };
    default:
      throw new Error(`unknown op ${command.op}`);
  }
}

function answer(line: Record<string, unknown>): void {
  process.stdout.write(`${JSON.stringify(line)}\n`);
}

await request("initialize", {
  protocolVersion: "2025-06-18",
  capabilities: {},
  clientInfo: { name: "smoke-role-takeover", version: "0" },
});
child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" })}\n`);
child.stdin.flush();
answer({ ready: true, pid: process.pid, serverPid: child.pid, session: options.session });

for await (const line of console) {
  const text = line.trim();
  if (text === "") continue;
  const command = JSON.parse(text) as Command;
  const id = command.id ?? "";
  try {
    answer({ id, ok: true, ...(await run(command)) });
  } catch (error) {
    answer({ id, ok: false, error: error instanceof Error ? error.message : String(error) });
  }
}
child.stdin.end();
// The channel server exits once its stdin ends (`envoy-channel.ts`); a server that does not fails
// the wait at the proof's deadline, naming it.
await settled("the channel server to exit once its stdin ended", child.exited);
process.exit(0);
