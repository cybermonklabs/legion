// packages/envoy-client/src/__tests__/dispatch-proxy-process.test.ts
//
// `dispatch proxy --socket <path> -- <command>` (dispatch://INFRA-4799/spec, R1, R4 and R6) as
// the shell runs it: the real entry, `bin/dispatch.ts`, as its own process under the same Bun as
// this test, with standard input, output and error given as files of a temporary directory, as
// `dispatch-cli.test.ts`'s "the dispatch entry as its own process" runs it. What a call inside
// this test's process cannot hold is held here: the streams its command inherits, a signal sent
// to the proxy, the sockets the process holds, its exit status and the exit line it ends on, and
// every byte it writes. The child's environment is written rather than inherited, a pod's under
// `DISPATCH_TOKEN_BROKER=1`, so no Dispatch or broker of the developer's is read.
//
// Each wait is bounded and says what it waited for, and the hook below ends every command and
// every proxy a row started, so a proxy that never makes its socket fails its row in a few
// seconds and leaves no process behind.

import { afterEach, beforeEach, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import {
  chmodSync,
  existsSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  realpathSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { connect } from "node:net";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { brokerStandIns } from "./broker-stand-ins";

/** One proxy process a row started, and the files of its standard streams. */
interface Spawned {
  readonly child: Bun.Subprocess;
  readonly socket: string;
  /** The file whose making ends the run's `DONE_LOOP` command, and the signal row's script. */
  readonly done: string;
  readonly stdout: string;
  readonly stderr: string;
}

/** Every proxy a row started, ended by the hook below whatever the row's outcome. */
const spawned = new Set<Spawned>();

// Registered before the directories' removal and the stand-ins' cleanup, so it runs first: each
// command ends while the file it waits for can still be made, and a proxy that does not exit
// after that is killed.
afterEach(async () => {
  const runs = [...spawned];
  spawned.clear();
  for (const run of runs) writeFileSync(run.done, "");
  await within(
    Promise.allSettled(runs.map((run) => run.child.exited)),
    3_000,
    "every proxy this row started exited"
  ).catch(() => undefined);
  for (const run of runs) {
    if (run.child.exitCode === null && run.child.signalCode === null) run.child.kill("SIGKILL");
  }
  await Promise.allSettled(runs.map((run) => run.child.exited));
});

/** Each directory is `mkdtemp` under `os.tmpdir()`: a socket at `<it>/d.sock` is short of the
 *  107 bytes a unix socket's path may hold. */
const makeDirectory = temporaryDirectories("dpp");

const { podEnvironment, servers, stubBroker, stubDispatch, projectedToken } =
  brokerStandIns("dispatch-proxy-process");

const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");

/** The session every row's proxy acts as (`DISPATCH_SESSION_ID`). */
const SESSION = "ses-1";

/** The command a row's proxy runs: it waits for the file its first argument names. */
const DONE_LOOP = 'while [ ! -e "$0" ]; do sleep 0.02; done';

/** The broker's final refusal, a text `KNOWN_BROKER_REFUSALS` lists verbatim. */
const BROKER_REFUSAL = "no entry lists the account this token names";

/** The proxy's exit line, in its fixed form (R4). */
function exitLine(requests: number, trades: number, renewed: number): string {
  return `dispatch proxy: ${requests} requests, ${trades} trades, ${renewed} after BROKER_TOKEN_NOT_ACTIVE`;
}

function lastLine(text: string): string {
  return text.trimEnd().split("\n").at(-1) ?? "";
}

let stateDir = "";
beforeEach(() => {
  stateDir = makeDirectory("state");
});

type Environment = Record<string, string | undefined>;

/** A pod's environment under `DISPATCH_TOKEN_BROKER=1`, written whole for the child. */
function podProxyEnvironment(brokerUrl: string, dispatchUrl: string): Environment {
  return {
    PATH: process.env.PATH,
    TMPDIR: process.env.TMPDIR,
    ...podEnvironment(brokerUrl, dispatchUrl),
    DISPATCH_HOST: "omp",
    DISPATCH_SESSION_ID: SESSION,
    DISPATCH_TOKEN_BROKER: "1",
    DISPATCH_STATE_DIR: stateDir,
  };
}

/** `promise`, or a failure naming `what` once `ms` have passed. */
async function within<T>(promise: Promise<T>, ms: number, what: string): Promise<T> {
  const late = Promise.withResolvers<never>();
  const timer = setTimeout(() => late.reject(new Error(`not done within ${ms} ms: ${what}`)), ms);
  try {
    return await Promise.race([promise, late.promise]);
  } finally {
    clearTimeout(timer);
  }
}

/** A fresh socket path: `d.sock` in a new directory. */
function socketPath(): string {
  return join(makeDirectory(), "d.sock");
}

/**
 * `dispatch proxy --socket <socket> -- <command>` as its own process, its standard input the
 * text `input` in a file and its standard output and error files of their own. `command` is
 * given the run's done file and returns the words after `--`; the `DONE_LOOP` by default.
 */
function spawnProxy(
  env: Environment,
  command: (done: string) => readonly string[] = (done) => ["bash", "-c", DONE_LOOP, done],
  options: { readonly socket?: string; readonly input?: string } = {}
): Spawned & { readonly stdin: string } {
  const dir = makeDirectory("io");
  const socket = options.socket ?? socketPath();
  const done = join(dir, "done");
  const files = {
    stdin: join(dir, "stdin"),
    stdout: join(dir, "stdout"),
    stderr: join(dir, "stderr"),
  };
  writeFileSync(files.stdin, options.input ?? "");
  const childEnv: Record<string, string> = {};
  for (const [name, value] of Object.entries(env)) {
    if (value !== undefined) childEnv[name] = value;
  }
  const child = Bun.spawn(
    [process.execPath, ENTRY, "proxy", "--socket", socket, "--", ...command(done)],
    {
      cwd: dir,
      env: childEnv,
      stdin: Bun.file(files.stdin),
      stdout: Bun.file(files.stdout),
      stderr: Bun.file(files.stderr),
    }
  );
  const run = { child, socket, done, ...files };
  spawned.add(run);
  return run;
}

/** What a run wrote on its standard output and error so far. */
function written(run: Spawned): { stdout: string; stderr: string } {
  const read = (file: string) => (existsSync(file) ? readFileSync(file, "utf-8") : "");
  return { stdout: read(run.stdout), stderr: read(run.stderr) };
}

/** Whether something accepts a connection at the unix socket `path` now. */
function accepts(path: string): Promise<boolean> {
  const { promise, resolve } = Promise.withResolvers<boolean>();
  const probe = connect(path);
  probe.once("connect", () => {
    probe.destroy();
    resolve(true);
  });
  probe.once("error", () => {
    probe.destroy();
    resolve(false);
  });
  return promise;
}

/** Waits, at most `ms`, until the run's socket accepts a connection; fails at once, naming what
 *  it wrote, where the process exits first. */
async function untilAccepting(run: Spawned, ms = 10_000): Promise<void> {
  const deadline = Date.now() + ms;
  while (true) {
    if (run.child.exitCode !== null || run.child.signalCode !== null) {
      const { stdout, stderr } = written(run);
      throw new Error(
        `dispatch proxy exited ${run.child.exitCode ?? run.child.signalCode} before its socket ${run.socket} accepted a connection; standard output: ${JSON.stringify(stdout)}; standard error: ${JSON.stringify(stderr)}`
      );
    }
    if (await accepts(run.socket)) return;
    if (Date.now() > deadline) {
      throw new Error(`the proxy's socket ${run.socket} accepted no connection within ${ms} ms`);
    }
    await Bun.sleep(20);
  }
}

/** Waits, at most `ms`, until `path` exists: a file a row's command makes once it is ready. */
async function untilMade(path: string, run: Spawned, ms = 10_000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!existsSync(path)) {
    if (run.child.exitCode !== null) {
      throw new Error(`dispatch proxy exited ${run.child.exitCode} before ${path} was made`);
    }
    if (Date.now() > deadline) throw new Error(`${path} was not made within ${ms} ms`);
    await Bun.sleep(20);
  }
}

/** The run's exit status, once it exits, and what it wrote. */
async function exitOf(run: Spawned): Promise<{ code: number; stdout: string; stderr: string }> {
  const code = await within(run.child.exited, 15_000, "dispatch proxy exited");
  return { code, ...written(run) };
}

/** Ends the run's command, and answers the proxy's exit status and what it wrote. */
function finish(run: Spawned): Promise<{ code: number; stdout: string; stderr: string }> {
  writeFileSync(run.done, "");
  return exitOf(run);
}

/** A request through the proxy's socket, as a tool's `fetch` sends it. */
async function viaSocket(socket: string, path: string, init: RequestInit = {}): Promise<number> {
  const response = await fetch(`http://dispatch${path}`, {
    ...init,
    unix: socket,
    redirect: "manual",
    signal: AbortSignal.timeout(10_000),
  });
  await response.arrayBuffer();
  return response.status;
}

/** A secrets broker that refuses every trade for good, in the words of "Fixtures from the real
 *  system", and counts what it was asked. */
function refusingBroker(): { url: string; asked: number[] } {
  const asked: number[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => {
      asked.push(Date.now());
      return Response.json(
        { error: "invalid_grant", error_description: BROKER_REFUSAL },
        { status: 400 }
      );
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, asked };
}

/** The inode of each socket process `pid` holds open, from its `/proc/<pid>/fd` links. */
function socketInodes(pid: number): Set<string> {
  const inodes = new Set<string>();
  for (const fd of readdirSync(`/proc/${pid}/fd`)) {
    let target = "";
    try {
      target = readlinkSync(`/proc/${pid}/fd/${fd}`);
    } catch {
      continue;
    }
    const inode = /^socket:\[(\d+)\]$/.exec(target)?.[1];
    if (inode !== undefined) inodes.add(inode);
  }
  return inodes;
}

/** The inode of each TCP socket in state `0A` (listen) that `/proc/net/tcp` and `tcp6` list, and
 *  how many of the two tables could be read. */
function listeningTcpInodes(): { readonly inodes: Set<string>; readonly tables: number } {
  const inodes = new Set<string>();
  let tables = 0;
  for (const table of ["/proc/net/tcp", "/proc/net/tcp6"]) {
    let text: string;
    try {
      text = readFileSync(table, "utf-8");
    } catch {
      continue;
    }
    tables += 1;
    for (const line of text.split("\n").slice(1)) {
      const fields = line.trim().split(/\s+/);
      if (fields[3] === "0A" && fields[9] !== undefined) inodes.add(fields[9]);
    }
  }
  return { inodes, tables };
}

/** `__SO_ACCEPTCON`, the flag `/proc/net/unix` shows on a listening socket. A connection the
 *  listener accepted is listed under the listener's path too, and one already closed may linger
 *  there, so the listener is told apart by this flag. */
const LISTENING_FLAGS = "00010000";

/** The inode `/proc/net/unix` lists for the socket listening at `path`, if it lists one. */
function unixSocketInode(path: string): string | undefined {
  for (const line of readFileSync("/proc/net/unix", "utf-8").split("\n").slice(1)) {
    const fields = line.trim().split(/\s+/);
    if (fields[7] === path && fields[3] === LISTENING_FLAGS) return fields[6];
  }
  return undefined;
}

/** Each file under `dir`, at any depth. */
function filesUnder(dir: string): string[] {
  return readdirSync(dir, { recursive: true })
    .map((entry) => join(dir, String(entry)))
    .filter((path) => statSync(path).isFile());
}

/**
 * The command of the signal row, as a script: it takes `signal`, writes the marker, makes one
 * more request over the proxy's socket, writes that answer's status and exits 7. It says it is
 * ready once its handler is in place, and exits on its own once the done file is made or 20 s
 * have passed, so a proxy that dies leaves it running no longer.
 */
const SIGNAL_SCRIPT = [
  'import { existsSync, writeFileSync } from "node:fs";',
  "const [signal, ready, marker, answered, done] = process.argv.slice(2);",
  "process.on(signal, async () => {",
  '  writeFileSync(marker, "");',
  '  let status = "";',
  "  try {",
  '    const answer = await fetch("http://dispatch/api/v1/issues", {',
  "      unix: process.env.DISPATCH_PROXY_SOCKET,",
  "    });",
  "    status = String(answer.status);",
  "  } catch (error) {",
  '    status = "no answer: " + String(error);',
  "  }",
  "  writeFileSync(answered, status);",
  "  process.exit(7);",
  "});",
  'writeFileSync(ready, "");',
  "const until = Date.now() + 20000;",
  "setInterval(() => {",
  "  if (existsSync(done) || Date.now() > until) process.exit(9);",
  "}, 20);",
  "",
].join("\n");

/**
 * What stops a run before its command, each a row of its own: the environment a `dispatch`
 * command resolves its server and session from, the broker's first trade, and the socket path.
 * Each run's command is `touch <marker>`; `taken` is a path that exists.
 */
function startFailures(
  pod: Environment,
  refusedUrl: string,
  dispatchUrl: string,
  tokenFile: string,
  taken: string
): readonly { readonly what: string; readonly env: Environment; readonly socket?: string }[] {
  return [
    { what: "no DISPATCH_TOKEN_BROKER", env: { ...pod, DISPATCH_TOKEN_BROKER: undefined } },
    { what: "DISPATCH_TOKEN_BROKER empty", env: { ...pod, DISPATCH_TOKEN_BROKER: "" } },
    {
      what: "a DISPATCH_TOKEN_FILE and no DISPATCH_TOKEN_BROKER",
      env: { ...pod, DISPATCH_TOKEN_BROKER: undefined, DISPATCH_TOKEN_FILE: tokenFile },
    },
    { what: "no DISPATCH_HOST", env: { ...pod, DISPATCH_HOST: undefined } },
    { what: "no DISPATCH_SESSION_ID", env: { ...pod, DISPATCH_SESSION_ID: undefined } },
    { what: "no AGENT_SECRETS_URL", env: { ...pod, AGENT_SECRETS_URL: undefined } },
    {
      what: "a first trade the broker refuses for good",
      env: podProxyEnvironment(refusedUrl, dispatchUrl),
    },
    { what: "a socket path that exists", env: pod, socket: taken },
  ];
}

/** A file holding a Dispatch bearer, as a pod outside the setting is handed one. */
function bearerFile(): { readonly path: string; readonly bearer: string } {
  const bearer = fixtureSecret("dsp");
  const path = join(makeDirectory(), "dispatch-token");
  writeFileSync(path, `${bearer}\n`, { mode: 0o600 });
  return { path, bearer };
}

// The hang guards below are limits on a row's waits, not budgets: each row runs the entry as a
// child process, and bun's unwritten 5,000 ms per-test default is a limit on that work rather
// than on a hang. Each row fails at once where the proxy exits before it makes its socket.

test("opens no TCP listener", async () => {
  const broker = stubBroker(3600);
  const dispatch = stubDispatch();
  const run = spawnProxy(podProxyEnvironment(broker.url, dispatch.url));
  await untilAccepting(run);

  const held = socketInodes(run.child.pid);
  const { inodes: listening, tables } = listeningTcpInodes();
  expect(tables).toBeGreaterThan(0);
  expect([...held].filter((inode) => listening.has(inode))).toEqual([]);
  const bound = unixSocketInode(run.socket);
  expect(bound).toBeDefined();
  expect(held.has(bound ?? "")).toBe(true);

  const { code } = await finish(run);
  expect(code).toBe(0);
  expect(existsSync(run.socket)).toBe(false);
}, 30_000);

test("gives its command its own standard input, output and error", async () => {
  const broker = stubBroker(3600);
  const dispatch = stubDispatch();
  const line = `a line from the test ${randomUUID()}`;
  const run = spawnProxy(
    podProxyEnvironment(broker.url, dispatch.url),
    () => [
      "bash",
      "-c",
      String.raw`read -r line; printf "out:%s\n" "$line"; printf "err-line\n" >&2; for n in 0 1 2; do printf "fd%s=%s\n" "$n" "$(readlink /proc/$$/fd/$n)" >&2; done`,
    ],
    { input: `${line}\n` }
  );

  const { code, stdout, stderr } = await exitOf(run);

  expect({ code, stderr }).toMatchObject({ code: 0 });
  expect(stdout).toBe(`out:${line}\n`);
  const lines = stderr.split("\n");
  expect(lines).toContain("err-line");
  // The command's own descriptors 0, 1 and 2 (`/proc/self/fd` of the command) are the files this
  // test gave the proxy.
  expect(lines).toContain(`fd0=${realpathSync(run.stdin)}`);
  expect(lines).toContain(`fd1=${realpathSync(run.stdout)}`);
  expect(lines).toContain(`fd2=${realpathSync(run.stderr)}`);
  expect(lastLine(stderr)).toBe(exitLine(0, 1, 0));
  expect(existsSync(run.socket)).toBe(false);
}, 30_000);

test("exits with its command's status", async () => {
  const broker = stubBroker(3600);
  const dispatch = stubDispatch();
  const env = podProxyEnvironment(broker.url, dispatch.url);

  for (const status of [0, 3]) {
    const run = spawnProxy(env, () => ["bash", "-c", `exit ${status}`]);
    const { code, stdout, stderr } = await exitOf(run);
    expect({ status, code, stdout }).toEqual({ status, code: status, stdout: "" });
    expect(lastLine(stderr), `exit ${status}`).toBe(exitLine(0, 1, 0));
    expect(existsSync(run.socket), `exit ${status}`).toBe(false);
  }
}, 30_000);

test("answers 127 for a command that does not exist and 126 for one that is not executable", async () => {
  const broker = stubBroker(3600);
  const dispatch = stubDispatch();
  const env = podProxyEnvironment(broker.url, dispatch.url);
  const notExecutable = join(makeDirectory(), "not-executable");
  writeFileSync(notExecutable, "#!/bin/sh\nexit 0\n");
  chmodSync(notExecutable, 0o644);

  for (const [what, command, status] of [
    ["a command that does not exist", "dispatch-proxy-test-no-such-command", 127],
    ["a command that is not executable", notExecutable, 126],
  ] as const) {
    const run = spawnProxy(env, () => [command]);
    const { code, stdout } = await exitOf(run);
    expect({ what, code, stdout }).toEqual({ what, code: status, stdout: "" });
    expect(existsSync(run.socket), what).toBe(false);
  }
}, 30_000);

test("sends SIGTERM and SIGINT to its command, keeps serving until the command ends, and exits with its status", async () => {
  const broker = stubBroker(3600);
  const dispatch = stubDispatch();
  const env = podProxyEnvironment(broker.url, dispatch.url);
  const script = join(makeDirectory("script"), "signalled.mjs");
  writeFileSync(script, SIGNAL_SCRIPT);

  for (const signal of ["SIGTERM", "SIGINT"] as const) {
    const dir = makeDirectory(signal);
    const ready = join(dir, "ready");
    const marker = join(dir, "marker");
    const answered = join(dir, "answered");
    dispatch.asked.length = 0;
    const run = spawnProxy(env, (done) => [
      process.execPath,
      script,
      signal,
      ready,
      marker,
      answered,
      done,
    ]);
    await untilAccepting(run);
    await untilMade(ready, run);

    run.child.kill(signal);
    const { code, stderr } = await exitOf(run);

    expect({ signal, code }).toEqual({ signal, code: 7 });
    expect(existsSync(marker), signal).toBe(true);
    // The command's request after the signal was served.
    expect(readFileSync(answered, "utf-8"), signal).toBe("200");
    expect(dispatch.asked, signal).toHaveLength(1);
    expect(lastLine(stderr), signal).toBe(exitLine(1, 1, 0));
    expect(existsSync(run.socket), signal).toBe(false);
  }
}, 60_000);

// A hangup reaches a wrapper whenever its terminal goes: a tmux pane or an ssh session that
// closes. R4 and R6 hold on every exit: the proxy exits when its command ends, with the command's
// status, removes its socket, and ends with its exit line. The command records the status it
// exits with: 7 where the hangup reached it, else 9 once the done file is made. Either way the
// proxy's status is that one.
test("exits with its command's status, removes its socket and ends with its exit line when it is sent SIGHUP", async () => {
  const broker = stubBroker(3600);
  const dispatch = stubDispatch();
  const dir = makeDirectory("hangup");
  const ready = join(dir, "ready");
  const status = join(dir, "status");
  const run = spawnProxy(podProxyEnvironment(broker.url, dispatch.url), (done) => [
    "bash",
    "-c",
    `trap 'echo 7 > "$2"; exit 7' HUP; touch "$1"; ${DONE_LOOP}; echo 9 > "$2"; exit 9`,
    done,
    ready,
    status,
  ]);
  await untilAccepting(run);
  await untilMade(ready, run);

  run.child.kill("SIGHUP");
  // Real processes and a real signal, so no fake clock reaches them. A proxy that sends the
  // hangup on exits once its command does, within the race below; one that keeps the hangup to
  // itself still runs its command after it, so the done file ends that command.
  await Promise.race([run.child.exited, Bun.sleep(500)]);
  writeFileSync(run.done, "");
  const { code, stderr } = await exitOf(run);
  // A proxy that died of the hangup left its command running, which records its status once it
  // sees the done file: wait for that record, bounded.
  const deadline = Date.now() + 5_000;
  while (!existsSync(status) && Date.now() < deadline) await Bun.sleep(20);
  const recorded = existsSync(status) ? readFileSync(status, "utf-8").trim() : "nothing";

  expect({ code: String(code), last: lastLine(stderr) }).toEqual({
    code: recorded,
    last: exitLine(0, 1, 0),
  });
  expect(existsSync(run.socket)).toBe(false);
}, 30_000);

test("prints the exit line on every exit of a run: the command's end, a signal, a command that cannot run, and each start failure", async () => {
  const broker = stubBroker(3600);
  const dispatch = stubDispatch();
  const refusing = refusingBroker();
  const pod = podProxyEnvironment(broker.url, dispatch.url);

  // The command's end.
  const ending = spawnProxy(pod, () => ["bash", "-c", "exit 0"]);
  const ended = await exitOf(ending);
  expect({ code: ended.code, last: lastLine(ended.stderr) }).toEqual({
    code: 0,
    last: exitLine(0, 1, 0),
  });
  expect(existsSync(ending.socket)).toBe(false);

  // A signal that ends the command: bash keeps no trap for SIGTERM, so it dies of it.
  const ready = join(makeDirectory("ready"), "ready");
  const signalled = spawnProxy(pod, (done) => [
    "bash",
    "-c",
    `touch "$1"; ${DONE_LOOP}`,
    done,
    ready,
  ]);
  await untilAccepting(signalled);
  await untilMade(ready, signalled);
  signalled.child.kill("SIGTERM");
  const killed = await exitOf(signalled);
  expect({ code: killed.code, last: lastLine(killed.stderr) }).toEqual({
    code: 128 + 15,
    last: exitLine(0, 1, 0),
  });
  expect(existsSync(signalled.socket)).toBe(false);

  // A command that cannot run.
  const cannot = spawnProxy(pod, () => ["dispatch-proxy-test-no-such-command"]);
  const missing = await exitOf(cannot);
  expect({ code: missing.code, last: lastLine(missing.stderr) }).toEqual({
    code: 127,
    last: exitLine(0, 1, 0),
  });
  expect(existsSync(cannot.socket)).toBe(false);

  // Each start failure: nothing was sent, and no trade gave a token.
  const taken = socketPath();
  writeFileSync(taken, "kept\n");
  const failures = startFailures(pod, refusing.url, dispatch.url, bearerFile().path, taken);
  for (const { what, env, socket } of failures) {
    const marker = join(makeDirectory(), "ran");
    const run = spawnProxy(env, () => ["touch", marker], socket === undefined ? {} : { socket });
    const { code, stderr } = await exitOf(run);
    expect({ what, code, last: lastLine(stderr) }).toEqual({
      what,
      code: 2,
      last: exitLine(0, 0, 0),
    });
    expect(existsSync(marker), what).toBe(false);
    if (socket === undefined) expect(existsSync(run.socket), what).toBe(false);
  }
  expect(readFileSync(taken, "utf-8")).toBe("kept\n");
}, 90_000);

test("prints no credential", async () => {
  const broker = stubBroker(3600);
  const dispatch = stubDispatch();
  const refusing = refusingBroker();
  const pod = podProxyEnvironment(broker.url, dispatch.url);
  const handed = bearerFile();

  // A run that trades and serves reads, a comment POST and a refusal of its own.
  const serving = spawnProxy(pod);
  await untilAccepting(serving);
  expect(await viaSocket(serving.socket, "/api/v1/issues")).toBe(200);
  expect(await viaSocket(serving.socket, "/api/v1/issues/DSP-1/events")).toBe(200);
  expect(
    await viaSocket(serving.socket, "/api/v1/issues/DSP-1/comments", {
      method: "POST",
      headers: { "Content-Type": "application/json", "Idempotency-Key": randomUUID() },
      body: JSON.stringify({ body: "Shipped." }),
    })
  ).toBe(200);
  expect(await viaSocket(serving.socket, "/api/v1/issues/DSP-1", { method: "DELETE" })).toBe(405);
  expect((await finish(serving)).code).toBe(0);
  expect(existsSync(serving.socket)).toBe(false);

  // A run whose first trade the broker refuses, and one handed a bearer outside the setting.
  const refused = spawnProxy(podProxyEnvironment(refusing.url, dispatch.url), () => ["true"]);
  expect((await exitOf(refused)).code).toBe(2);
  const bearer = spawnProxy(
    { ...pod, DISPATCH_TOKEN_BROKER: undefined, DISPATCH_TOKEN_FILE: handed.path },
    () => ["true"]
  );
  expect((await exitOf(bearer)).code).toBe(2);

  const credentials = [...broker.trades.map((trade) => trade.token), projectedToken, handed.bearer];
  expect(broker.trades.length).toBeGreaterThan(0);
  const outputs = [serving, refused, bearer].flatMap((run) => [run.stdout, run.stderr]);
  for (const file of [...outputs, ...filesUnder(stateDir)]) {
    const bytes = readFileSync(file);
    for (const credential of credentials) {
      expect(bytes.includes(credential), `${file} holds a credential`).toBe(false);
    }
  }
}, 60_000);
