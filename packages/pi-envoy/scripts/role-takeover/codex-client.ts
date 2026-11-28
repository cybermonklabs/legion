#!/usr/bin/env bun
/**
 * codex-client.ts — the `codex` process of one Codex CLI session, for
 * `scripts/e2e/nats-callout-sessions.sh`, which compiles it into a binary named `codex`
 * (`bun build --compile`), so the kernel names the process `codex` and its `/proc/<pid>/exe` is
 * this binary, as for the real Codex CLI.
 *
 * It does for the Codex plugin (`@sjawhar/codex-envoy`, packages/claude-envoy) what Codex CLI
 * 0.154.0 does, with no model:
 *
 * - It starts the plugin's MCP server from the installed plugin's own manifest
 *   (`.codex-plugin/plugin.json`, `mcpServers.envoy`: its command, arguments and directory) as its
 *   child, and speaks MCP to it over that pipe, as Codex does. The server gets only what Codex hands
 *   a plugin's stdio MCP server: the manifest's `env_vars` that this process has, and Codex's own
 *   short list (`CODEX_DEFAULT_ENV` below, measured on 0.154.0). So a variable the manifest does not
 *   name never reaches the server, which is what the script's manifest-drops-nats-auth control
 *   turns on.
 * - It runs the plugin's hooks from its `hooks/hooks.json` as Codex does: each command through
 *   `sh -c`, as this process's child, with `PLUGIN_ROOT` and `PLUGIN_DATA` (the plugin's state
 *   directory, `<CODEX_HOME>/plugins/data/<plugin>-<marketplace>`) beside this process's
 *   environment, the event's JSON on stdin. `start` and `new` run `SessionStart`, which writes the
 *   record the MCP server waits for before it registers the session; the end of its input runs
 *   `SessionEnd`.
 * - It keeps the session's transcript where Codex does
 *   (`<CODEX_HOME>/sessions/<yyyy>/<mm>/<dd>/rollout-<time>-<id>.jsonl`), the file the plugin's live
 *   view and turn tracking read, in the lines Codex 0.154.0 writes
 *   (packages/claude-envoy/tests/fixtures/codex/rollout-0.154.0.jsonl).
 * - It answers `codex queue --thread <id> --message=<text>`, the command the plugin puts a message
 *   into an idle session with (`queueMessage`, src/codex/codex-cli.ts). The plugin runs it with the
 *   `/proc/<pid>/exe` of its `codex` process, which is this binary; that call hands the message to
 *   this process through `<CODEX_HOME>/ncs-standin/queue/` and exits 0 once this process took it,
 *   as Codex's own command exits once Codex stored the message. This process then starts a turn
 *   holding it, as the Codex terminal does a few seconds later: the turn's user message goes into
 *   the transcript and the turn ends, with no model reply. `--withhold-queue-answers true` takes
 *   each message and never answers, so `codex queue` does not exit while the session runs, the
 *   plugin never answers the Envoy frame (`done`), and the listener acks nothing (the script's
 *   withhold-ack-codex control). A `codex queue` whose session process has gone exits 1, as one
 *   with no session to queue for, so none outlives the run.
 *
 * One MCP server, as for a session's first thread: Codex starts one more per `/new` thread, which
 * stands by under the plugin's owner rule (src/codex/codex-owner.ts) while the first one runs, so
 * it would serve no Envoy traffic here.
 *
 * Usage:
 *   codex --plugin <installed plugin dir> --cwd <dir> --stderr <file> --hook-log <file>
 *         [--withhold-queue-answers true]
 *   codex queue --thread <id> --message=<text>
 *
 * The first reads CODEX_HOME, as Codex does, and every other variable from its own environment.
 *
 * Protocol: one JSON object per line on stdin, one answer per line on stdout, matched by `id`.
 *
 *   {"id":"1","op":"start","session":"<uuid>"}   the session's first prompt: SessionStart
 *   {"id":"2","op":"new","session":"<uuid>"}     /new to that id: SessionStart again
 *   {"id":"3","op":"deliveries"}                 every message `codex queue` handed it, in order
 *
 * Its input ending is the person quitting Codex: `SessionEnd` runs, the MCP server's stdin closes,
 * and it waits up to 10 s for the server to deregister and exit.
 */
import { randomBytes, randomUUID } from "node:crypto";
import {
  appendFileSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { basename, dirname, join, resolve } from "node:path";
import type { Subprocess } from "bun";

/** What Codex CLI 0.154.0 hands every stdio MCP server whatever its `env_vars` say (measured: a
 *  server configured with `env_vars = ["X_PASSED"]` got exactly these and X_PASSED). */
const CODEX_DEFAULT_ENV = [
  "HOME",
  "LANG",
  "LC_ALL",
  "LOGNAME",
  "PATH",
  "SHELL",
  "TERM",
  "TMPDIR",
  "TZ",
  "USER",
] as const;

/** How often a waiting `codex queue`, and this process for a new one, look at the queue. */
const QUEUE_POLL_MS = 50;

interface Command {
  readonly id?: string;
  readonly op?: string;
  readonly session?: string;
}

interface Hook {
  readonly type?: string;
  readonly command?: string;
  readonly timeout?: number;
}

const codexHome = process.env["CODEX_HOME"] ?? "";
if (codexHome === "") throw new Error("codex: CODEX_HOME is required");
const queueDirectory = join(codexHome, "ncs-standin", "queue");
/** Which process takes the queue: the session's `codex` process, by its pid and its start, so a
 *  `codex queue` whose session has gone stops waiting rather than outliving the run. */
const takerFile = join(codexHome, "ncs-standin", "taker.json");

/** When process `pid` started (field 22 of `/proc/<pid>/stat`), or undefined when it is gone. */
function startOf(pid: number): string | undefined {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
    return stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19];
  } catch {
    return undefined;
  }
}

/** Writes `text` to `file` whole: a temporary file renamed into place. */
function writeWhole(file: string, text: string): void {
  const temporary = `${file}.${process.pid}.tmp`;
  writeFileSync(temporary, text, { mode: 0o600 });
  renameSync(temporary, file);
}

/** `codex queue --thread <id> --message=<text>`: hands the message to the running session and
 *  exits 0 once it took it, 1 when it refused it or no session process takes the queue. */
async function queue(args: readonly string[]): Promise<never> {
  let thread: string | undefined;
  let message: string | undefined;
  for (let index = 0; index < args.length; index++) {
    const arg = args[index] ?? "";
    if (arg === "--thread") thread = args[++index];
    else if (arg.startsWith("--message=")) message = arg.slice("--message=".length);
    else if (arg === "--message") message = args[++index];
  }
  if (thread === undefined || message === undefined) {
    process.stderr.write("codex queue: --thread and --message are required\n");
    process.exit(2);
  }
  mkdirSync(queueDirectory, { recursive: true, mode: 0o700 });
  const name = `${Date.now()}-${randomBytes(4).toString("hex")}`;
  writeWhole(join(queueDirectory, `${name}.json`), JSON.stringify({ thread, message }));
  const answerFile = join(queueDirectory, `${name}.answer`);
  for (;;) {
    if (existsSync(answerFile)) {
      const said = JSON.parse(readFileSync(answerFile, "utf8")) as {
        readonly ok: boolean;
        readonly error?: string;
      };
      if (said.ok) {
        process.stdout.write(`Queued message for thread ${thread}.\n`);
        process.exit(0);
      }
      process.stderr.write(`codex queue: ${said.error ?? "refused"}\n`);
      process.exit(1);
    }
    // Read each pass: the session writes it as it starts, and it names the process that answers.
    let taker: { readonly pid?: unknown; readonly started?: unknown } = {};
    try {
      taker = JSON.parse(readFileSync(takerFile, "utf8"));
    } catch {
      // No session has started.
    }
    if (typeof taker.pid !== "number" || startOf(taker.pid) !== taker.started) {
      process.stderr.write(`codex queue: no codex session is running for thread ${thread}\n`);
      process.exit(1);
    }
    await Bun.sleep(QUEUE_POLL_MS);
  }
}

const argv = process.argv.slice(2);
if (argv[0] === "queue") await queue(argv.slice(1));

// ---- the session's `codex` process -------------------------------------------------------------

const options = new Map<string, string>();
for (let index = 0; index < argv.length; index += 2) {
  const name = argv[index];
  const value = argv[index + 1];
  if (name === undefined || !name.startsWith("--") || value === undefined) {
    throw new Error(`codex: expected --name value pairs, got ${argv.join(" ")}`);
  }
  options.set(name.slice(2), value);
}
const required = (name: string): string => {
  const value = options.get(name);
  if (value === undefined) throw new Error(`codex: --${name} is required`);
  return value;
};
const pluginRoot = resolve(required("plugin"));
const cwd = resolve(required("cwd"));
const serverStderr = required("stderr");
const hookLog = required("hook-log");
const withhold = options.get("withhold-queue-answers") === "true";

// The installed plugin: `<CODEX_HOME>/plugins/cache/<marketplace>/<plugin>/<version>`, whose state
// directory Codex hands its hooks as PLUGIN_DATA.
const marketplace = basename(dirname(dirname(pluginRoot)));
const pluginName = basename(dirname(pluginRoot));
const pluginData = join(codexHome, "plugins", "data", `${pluginName}-${marketplace}`);
mkdirSync(pluginData, { recursive: true, mode: 0o700 });

const manifest = JSON.parse(
  readFileSync(join(pluginRoot, ".codex-plugin", "plugin.json"), "utf8")
) as {
  readonly mcpServers?: Record<
    string,
    {
      readonly command: string;
      readonly args?: readonly string[];
      readonly cwd?: string;
      readonly env_vars?: readonly string[];
    }
  >;
};
const declared = manifest.mcpServers?.["envoy"];
if (declared === undefined) {
  throw new Error(`codex: ${pluginRoot}'s manifest declares no envoy server`);
}

/** The MCP server's environment: Codex's own list and the manifest's `env_vars`, each where this
 *  process has it, and nothing else of this process's. */
const serverEnv: Record<string, string> = {};
for (const name of [...CODEX_DEFAULT_ENV, ...(declared.env_vars ?? [])]) {
  const value = process.env[name];
  if (value !== undefined) serverEnv[name] = value;
}
const serverCommand = Bun.which(declared.command, { PATH: serverEnv["PATH"] ?? "" });
if (serverCommand === null) throw new Error(`codex: ${declared.command} is not on PATH`);

const child: Subprocess<"pipe", "pipe", "pipe"> = Bun.spawn(
  [serverCommand, ...(declared.args ?? [])],
  {
    cwd: resolve(pluginRoot, declared.cwd ?? "."),
    env: serverEnv,
    stdin: "pipe",
    stdout: "pipe",
    stderr: "pipe",
  }
);

const answers = new Map<number, unknown>();
const waiters = new Map<number, () => void>();
let serverErr = "";

void (async () => {
  let buffered = "";
  for await (const chunk of child.stdout.pipeThrough(new TextDecoderStream())) {
    buffered += chunk;
    for (let newline = buffered.indexOf("\n"); newline !== -1; newline = buffered.indexOf("\n")) {
      const message = JSON.parse(buffered.slice(0, newline)) as { readonly id?: unknown };
      buffered = buffered.slice(newline + 1);
      if (typeof message.id !== "number") continue;
      answers.set(message.id, message);
      waiters.get(message.id)?.();
    }
  }
})();

void (async () => {
  for await (const chunk of child.stderr.pipeThrough(new TextDecoderStream())) {
    serverErr += chunk;
    await Bun.write(serverStderr, serverErr);
  }
})();

let nextRequestID = 0;

/** Sends one MCP request and resolves with the server's answer to it. */
async function request(method: string, params: unknown): Promise<unknown> {
  nextRequestID += 1;
  const id = nextRequestID;
  const settled = Promise.withResolvers<void>();
  waiters.set(id, settled.resolve);
  child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", id, method, params })}\n`);
  child.stdin.flush();
  const deadline = setTimeout(() => {
    settled.reject(new Error(`the MCP server did not answer ${method} within 60s: ${serverErr}`));
  }, 60_000);
  try {
    await settled.promise;
  } finally {
    clearTimeout(deadline);
    waiters.delete(id);
  }
  return answers.get(id);
}

// ---- the hooks ---------------------------------------------------------------------------------

const hooksFile = join(pluginRoot, "hooks", "hooks.json");
const hooks = existsSync(hooksFile)
  ? (
      JSON.parse(readFileSync(hooksFile, "utf8")) as {
        readonly hooks?: Record<string, readonly { readonly hooks?: readonly Hook[] }[]>;
      }
    ).hooks
  : undefined;

/** Runs every command hook the plugin declares for `event`, one after another, each as Codex runs
 *  it, and logs what each printed. */
async function runHooks(event: string, input: Record<string, unknown>): Promise<void> {
  for (const group of hooks?.[event] ?? []) {
    for (const hook of group.hooks ?? []) {
      if (hook.type !== "command" || hook.command === undefined) continue;
      const ran = Bun.spawn(["sh", "-c", hook.command], {
        cwd,
        env: { ...process.env, PLUGIN_ROOT: pluginRoot, PLUGIN_DATA: pluginData },
        stdin: new Blob([JSON.stringify({ ...input, hook_event_name: event })]),
        stdout: "pipe",
        stderr: "pipe",
      });
      const timer = setTimeout(() => ran.kill("SIGKILL"), (hook.timeout ?? 60) * 1000);
      const [stdout, stderr, code] = await Promise.all([
        new Response(ran.stdout).text(),
        new Response(ran.stderr).text(),
        ran.exited,
      ]);
      clearTimeout(timer);
      appendFileSync(
        hookLog,
        `${JSON.stringify({ event, command: hook.command, code, stdout, stderr })}\n`
      );
    }
  }
}

// ---- the transcript ----------------------------------------------------------------------------

let session = "";
let transcript = "";
let ordinal = 0;

function line(type: string, payload: Record<string, unknown>): void {
  appendFileSync(
    transcript,
    `${JSON.stringify({ timestamp: new Date().toISOString(), ordinal: ordinal++, type, payload })}\n`
  );
}

/** A new thread's transcript, where Codex keeps it, opened with its `session_meta` line. */
function openTranscript(id: string): void {
  const now = new Date();
  const stamp = now.toISOString().slice(0, 19).replaceAll(":", "-");
  const directory = join(
    codexHome,
    "sessions",
    stamp.slice(0, 4),
    stamp.slice(5, 7),
    stamp.slice(8, 10)
  );
  mkdirSync(directory, { recursive: true });
  session = id;
  transcript = join(directory, `rollout-${stamp}-${id}.jsonl`);
  ordinal = 0;
  line("session_meta", {
    session_id: id,
    id,
    timestamp: now.toISOString(),
    cwd,
    originator: "codex-tui",
    cli_version: "ncs-standin",
    source: "cli",
    thread_source: "user",
    model_provider: "standin",
  });
}

/** One turn holding a person's message and no model reply, as Codex writes a queued message's. */
function turn(text: string): void {
  const turnId = randomUUID();
  const at = Date.now();
  line("turn_context", { turn_id: turnId, cwd, model: "standin-model" });
  line("event_msg", { type: "task_started", turn_id: turnId, started_at: Math.floor(at / 1000) });
  line("event_msg", {
    type: "item_completed",
    thread_id: session,
    turn_id: turnId,
    item: {
      type: "UserMessage",
      id: randomUUID(),
      content: [{ type: "text", text, text_elements: [] }],
    },
    started_at_ms: at,
    completed_at_ms: at,
  });
  line("event_msg", { type: "task_complete", turn_id: turnId, last_agent_message: null });
}

// ---- `codex queue`, as the running session takes it -------------------------------------------

/** Every message `codex queue` handed this session, in order. */
const delivered: string[] = [];

async function takeQueued(): Promise<void> {
  mkdirSync(queueDirectory, { recursive: true, mode: 0o700 });
  const taken = new Set<string>();
  for (;;) {
    for (const name of readdirSync(queueDirectory).sort()) {
      if (!name.endsWith(".json") || taken.has(name)) continue;
      taken.add(name);
      const asked = JSON.parse(readFileSync(join(queueDirectory, name), "utf8")) as {
        readonly thread: string;
        readonly message: string;
      };
      const answerFile = join(queueDirectory, name.replace(/\.json$/, ".answer"));
      if (asked.thread !== session) {
        writeWhole(
          answerFile,
          JSON.stringify({ ok: false, error: `no thread ${asked.thread} is loaded` })
        );
        continue;
      }
      delivered.push(asked.message);
      // Withheld, the plugin's `codex queue` never returns: it never answers the Envoy frame, and
      // the listener acks nothing (nats-callout-sessions.sh's withhold-ack-codex control).
      if (withhold) continue;
      writeWhole(answerFile, JSON.stringify({ ok: true }));
      turn(asked.message);
    }
    await Bun.sleep(QUEUE_POLL_MS);
  }
}

// ---- the driver's commands ---------------------------------------------------------------------

async function run(command: Command): Promise<Record<string, unknown>> {
  switch (command.op) {
    case "start":
    case "new": {
      if (command.session === undefined) throw new Error(`${command.op} needs a session id`);
      // The thread's transcript, then SessionStart, which records it for the MCP server.
      openTranscript(command.session);
      await runHooks("SessionStart", {
        session_id: command.session,
        transcript_path: transcript,
        cwd,
        source: command.op === "start" ? "startup" : "clear",
        model: "standin-model",
      });
      return { session, transcript };
    }
    case "deliveries":
      return { deliveries: [...delivered] };
    default:
      throw new Error(`unknown op ${command.op}`);
  }
}

function answer(reply: Record<string, unknown>): void {
  process.stdout.write(`${JSON.stringify(reply)}\n`);
}

rmSync(queueDirectory, { recursive: true, force: true });
mkdirSync(queueDirectory, { recursive: true, mode: 0o700 });
writeWhole(takerFile, JSON.stringify({ pid: process.pid, started: startOf(process.pid) }));
void takeQueued();
await request("initialize", {
  protocolVersion: "2025-06-18",
  capabilities: { elicitation: {} },
  clientInfo: { name: "codex-mcp-client", title: "Codex", version: "0.154.0" },
});
child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" })}\n`);
child.stdin.flush();
await request("tools/list", {});
answer({ ready: true, pid: process.pid, serverPid: child.pid });

for await (const text of console) {
  const trimmed = text.trim();
  if (trimmed === "") continue;
  const command = JSON.parse(trimmed) as Command;
  const id = command.id ?? "";
  try {
    answer({ id, ok: true, ...(await run(command)) });
  } catch (error) {
    answer({ id, ok: false, error: error instanceof Error ? error.message : String(error) });
  }
}
if (session !== "") await runHooks("SessionEnd", { session_id: session, cwd });
child.stdin.end();
await Promise.race([child.exited, Bun.sleep(10_000)]);
process.exit(0);
