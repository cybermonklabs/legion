import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { existsSync, readdirSync, readFileSync, readlinkSync, realpathSync } from "node:fs";
import { type AddressInfo, connect, createServer, type Server, type Socket } from "node:net";
import { isAbsolute, join } from "node:path";
import { goOutput } from "@legion/test-fixtures/go-command";
import { hostGate, requireHostTools } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { rowWaits, untilTrue } from "./row-waits";

// held-port.py's real-process tests: the helper hands its socket to the real envoy-listener of
// this branch, which serves on it through ENVOY_LISTEN_FD. They read /proc, so they run on Linux,
// and they build the listener with go; a host that is neither skips them outside CI, naming what
// it lacks, and fails them by name under GITHUB_ACTIONS (hostGate).

const helper = join(import.meta.dir, "held-port.py");
const envoy = join(import.meta.dir, "..", "..", "..", "packages", "envoy");
// A short directory of its own: the keeper's unix socket lives here (docs/testing.md).
const work = temporaryDirectory("held-port");
const listener = join(work, "envoy-listener");
const O_CLOEXEC = 0o2000000;
// Each wait of a row on the helper and the listener (a port file written, /healthz answering, a
// connection made) ends on its event, 20 s before the row's bound at the latest
// (lib/row-waits.ts): they are real processes, and how soon they get there is a loaded host's.
const waits = rowWaits(120_000);

// The interpreter python3 reports as sys.executable, resolved once with the caller's environment
// and run directly, as claude-backend-guards.test.ts's unreadable-environment row runs it: the
// first python3 on PATH can be a version manager's shim (mise's), which runs the helper with a
// PATH of its own, so the --pass row read the shim's PATH back from env(1), not the one it gave,
// and passed or failed by which python3 the machine put first. Without python3 a row fails naming
// it (requireHostTools).
let resolvedPython: string | undefined;
function python(): string {
  if (resolvedPython !== undefined) return resolvedPython;
  requireHostTools("python3");
  const resolved = Bun.spawnSync(["python3", "-I", "-c", "import sys; print(sys.executable)"]);
  const path = resolved.stdout.toString().trim();
  if (resolved.exitCode !== 0 || !isAbsolute(path)) {
    throw new Error(
      `could not resolve the python3 interpreter (exit ${resolved.exitCode}): ${resolved.stderr.toString() || "no absolute executable path"}`
    );
  }
  resolvedPython = path;
  return path;
}

type Running = {
  name: string;
  process: Bun.Subprocess<"ignore" | "pipe", "pipe", "pipe">;
  output: () => string;
  exited: () => boolean;
  // logged resolves once the output matches pattern, and rejects when the process ended first.
  logged: (pattern: RegExp) => Promise<void>;
};

const started: Running[] = [];
let nats: Server | undefined;
const natsClients: Socket[] = [];
let natsUrl = "";

// Both gates are read, so a host that has neither says so about both.
const host = hostGate({
  programs: ["go"],
  reason:
    process.platform === "linux"
      ? ""
      : `They read /proc, which this host's ${process.platform} does not have; they run on linux.`,
});
const gated = host.describe;

beforeAll(async () => {
  if (host.missing) return;
  await goOutput(["build", "-o", listener, "./cmd/listener"], { cwd: envoy });
  // A NATS address that accepts and never answers: the listener serves before it connects, so its
  // /healthz answers "starting" while its connect waits out its attempts (about a minute).
  const silent = createServer((client) => {
    natsClients.push(client);
    client.on("error", () => {});
  });
  nats = silent;
  const listening = Promise.withResolvers<void>();
  silent.once("error", listening.reject);
  silent.listen(0, "127.0.0.1", listening.resolve);
  await listening.promise;
  natsUrl = `nats://127.0.0.1:${(silent.address() as AddressInfo).port}`;
  // Bun ends a hook at 5 s unless the hook names its own bound (Bun 1.4.4: a 7 s hook with no bound
  // fails "hook timed out" at 5.0 s, and passes with this second argument). The build above is the
  // slow part: from empty module and build caches, as the test job's cold setup-go starts, it took
  // 36.3 s with GOMAXPROCS=4 and 25.0 s with 16 on a dev box at load average 1,300-1,700, fetching
  // 22 modules; warm, 0.9 s. Ten minutes leaves that cold build more than ten times its measure.
}, 600_000);

afterEach(async () => {
  const running = started.splice(0);
  for (const { process } of running) process.kill("SIGKILL");
  await Promise.all(running.map(({ process }) => process.exited));
});

afterAll(async () => {
  for (const client of natsClients) client.destroy();
  const closed = Promise.withResolvers<void>();
  if (nats) nats.close(() => closed.resolve());
  else closed.resolve();
  await closed.promise;
});

// start runs argv with the environment the listener needs and collects its output; afterEach
// kills it.
function start(name: string, argv: string[], stdin: "ignore" | "pipe" = "ignore"): Running {
  const process = Bun.spawn(argv, {
    env: {
      PATH: Bun.env.PATH ?? "",
      ENVOY_MACHINE_ID: "held-port-test",
      NATS_URLS: natsUrl,
    },
    stdin,
    stdout: "pipe",
    stderr: "pipe",
  });
  let output = "";
  let done = false;
  let drained = false;
  const waiting: { pattern: RegExp; resolve: () => void; reject: (error: Error) => void }[] = [];
  const settle = () => {
    for (const waiter of waiting.splice(0)) {
      if (waiter.pattern.test(output)) waiter.resolve();
      else if (drained)
        waiter.reject(new Error(`ended before it logged ${waiter.pattern}:\n${output}`));
      else waiting.push(waiter);
    }
  };
  const decoder = new TextDecoder();
  const streams = [process.stdout, process.stderr].map(async (stream) => {
    for await (const chunk of stream) {
      output += decoder.decode(chunk, { stream: true });
      settle();
    }
  });
  void process.exited.then(() => {
    done = true;
  });
  void Promise.all([process.exited, ...streams]).then(() => {
    drained = true;
    settle();
  });
  const logged = (pattern: RegExp) => {
    const outcome = Promise.withResolvers<void>();
    waiting.push({ pattern, resolve: outcome.resolve, reject: outcome.reject });
    settle();
    return outcome.promise;
  };
  const running = { name, process, output: () => output, exited: () => done, logged };
  started.push(running);
  return running;
}

function exec(name: string, portFile: string, command: string[], from?: string): Running {
  return start(name, [
    python(),
    helper,
    "exec",
    ...(from ? ["--from", from] : []),
    "--port-file",
    portFile,
    "--fd-env",
    "ENVOY_LISTEN_FD",
    "--env",
    "PORT={port}",
    "--",
    ...command,
  ]);
}

function describeRun(running: Running): string {
  return `${running.name} (pid ${running.process.pid}, exit ${running.process.exitCode ?? running.process.signalCode ?? "none yet"}):\n${running.output()}`;
}

// readPort waits for path to hold a port, failing when running exits first. The writer is a real
// process with no event to await, so this and requireAnswers poll on the real clock.
async function readPort(path: string, running: Running): Promise<number> {
  let port = 0;
  await untilTrue(`${running.name} to write a port to ${path}`, waits.end(), () => {
    if (existsSync(path)) {
      const text = readFileSync(path, "utf8");
      if (!/^[1-9][0-9]*\n$/.test(text)) throw new Error(`${path} holds ${JSON.stringify(text)}`);
      port = Number(text.trim());
      return true;
    }
    if (running.exited())
      throw new Error(`exited before it wrote ${path}: ${describeRun(running)}`);
    return `nothing written yet: ${describeRun(running)}`;
  });
  return port;
}

// requireAnswers waits for /healthz on port to answer at all, failing when running exits first. A
// request is bounded by the end of the row's waits alone, so a slow answer is still read.
async function requireAnswers(port: number, running: Running): Promise<void> {
  const url = `http://127.0.0.1:${port}/healthz`;
  const end = waits.end();
  await untilTrue(`/healthz to answer on ${port}`, end, async () => {
    let failed: unknown;
    try {
      const response = await fetch(url, {
        keepalive: false,
        signal: AbortSignal.timeout(Math.max(1, Math.ceil(end - performance.now()))),
      });
      await response.arrayBuffer();
      return true;
    } catch (error) {
      failed = error;
    }
    if (running.exited()) {
      throw new Error(`exited before /healthz answered on ${port}: ${describeRun(running)}`);
    }
    return `${failed}\n${describeRun(running)}`;
  });
}

// bindOutcome binds 127.0.0.1:port and closes it again: "bound", or the error code of the refusal.
async function bindOutcome(port: number): Promise<string> {
  const server = createServer();
  const outcome = Promise.withResolvers<string>();
  server.once("error", (error: NodeJS.ErrnoException) =>
    outcome.resolve(error.code ?? String(error))
  );
  server.listen(port, "127.0.0.1", () => server.close(() => outcome.resolve("bound")));
  return await outcome.promise;
}

// answerWithin connects to port, sends a request and resolves what came back within milliseconds
// of the connection: "no answer" on an accepted connection that stayed silent that long,
// "answered", the connection's error, or "never connected" when no connection was made by the end
// of the row's waits. The silence it measures is real time, so it waits on the real clock; the
// connect is the kernel's and the event loop's, so the silence is timed from it, never from the
// call.
async function answerWithin(port: number, milliseconds: number): Promise<string> {
  const outcome = Promise.withResolvers<string>();
  const client = connect(port, "127.0.0.1");
  let silence: Timer | undefined;
  const connecting = setTimeout(
    () => outcome.resolve("never connected"),
    Math.max(0, waits.end() - performance.now())
  );
  client.once("connect", () => {
    clearTimeout(connecting);
    silence = setTimeout(() => outcome.resolve("no answer"), milliseconds);
    client.write(`GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:${port}\r\nConnection: close\r\n\r\n`);
  });
  client.once("data", () => outcome.resolve("answered"));
  client.once("error", (error: NodeJS.ErrnoException) =>
    outcome.resolve(error.code ?? String(error))
  );
  try {
    return await outcome.promise;
  } finally {
    clearTimeout(connecting);
    clearTimeout(silence);
    client.destroy();
  }
}

// listeningInode returns the inode of the socket listening on 127.0.0.1:port from /proc/net/tcp,
// or undefined when none listens. local_address is the IPv4 address in the host's byte order
// (127.0.0.1 is 0100007F on the little-endian machines the tests run on) and the port, in
// hexadecimal; st 0A is LISTEN, and the tenth field is the inode.
function listeningInode(port: number): string | undefined {
  const local = `0100007F:${port.toString(16).toUpperCase().padStart(4, "0")}`;
  const inodes = readFileSync("/proc/net/tcp", "utf8")
    .split("\n")
    .map((line) => line.trim().split(/\s+/))
    .filter((fields) => fields.length > 9 && fields[1] === local && fields[3] === "0A")
    .map((fields) => fields[9]);
  if (inodes.length > 1) throw new Error(`${inodes.length} sockets listen on 127.0.0.1:${port}`);
  return inodes[0];
}

// descriptorsOf returns the descriptors of process pid that link to target.
function descriptorsOf(pid: number, target: string): number[] {
  const dir = `/proc/${pid}/fd`;
  return readdirSync(dir)
    .filter((entry) => {
      try {
        return readlinkSync(join(dir, entry)) === target;
      } catch {
        return false;
      }
    })
    .map(Number);
}

// closeOnExec reads the octal flags line of /proc/<pid>/fdinfo/<fd>.
function closeOnExec(pid: number, fd: number): boolean {
  const info = readFileSync(`/proc/${pid}/fdinfo/${fd}`, "utf8");
  const flags = /^flags:\s*([0-7]+)$/m.exec(info);
  if (!flags?.[1]) throw new Error(`fdinfo of descriptor ${fd} of process ${pid}:\n${info}`);
  return (Number.parseInt(flags[1], 8) & O_CLOEXEC) !== 0;
}

gated("held-port.py exec", () => {
  test("the listener serves the handed socket on the port in the port file, in the helper's own process", async () => {
    const portFile = join(work, "serves.port");
    const running = exec("exec of the listener", portFile, [listener]);
    const port = await readPort(portFile, running);
    await requireAnswers(port, running);
    // The listener logs its line after it starts serving, so /healthz can answer before the line
    // is read: await the line itself.
    await running.logged(
      new RegExp(`"msg":"envoy-listener listening".*"addr":"127\\.0\\.0\\.1:${port}"`)
    );
    // The PID Bun.spawn returned, a shell's $!, is the listener's: the helper executed it in place.
    expect(readlinkSync(`/proc/${running.process.pid}/exe`)).toBe(realpathSync(listener));
  });

  test("from the moment the port file names it until the server runs, the port is held", async () => {
    const portFile = join(work, "held.port");
    const running = start(
      "exec of a shell that waits, then the listener",
      [
        python(),
        helper,
        "exec",
        "--port-file",
        portFile,
        "--fd-env",
        "ENVOY_LISTEN_FD",
        "--env",
        "PORT={port}",
        "--",
        "sh",
        "-c",
        'read go && exec "$0" "$@"',
        listener,
      ],
      "pipe"
    );
    const port = await readPort(portFile, running);
    expect(await bindOutcome(port)).toBe("EADDRINUSE");
    expect(running.exited()).toBe(false);
    const stdin = running.process.stdin;
    if (!stdin) throw new Error("the shell has no stdin pipe");
    stdin.write("go\n");
    await stdin.flush();
    await requireAnswers(port, running);
  });

  test("the listener holds the socket only on its own close-on-exec copy, not on descriptor 3", async () => {
    const portFile = join(work, "descendant.port");
    const running = exec("exec of the listener", portFile, [listener]);
    const port = await readPort(portFile, running);
    await requireAnswers(port, running);
    const inode = listeningInode(port);
    if (!inode) throw new Error(`nothing listens on 127.0.0.1:${port}`);
    const pid = running.process.pid;
    const holders = descriptorsOf(pid, `socket:[${inode}]`);
    // Exactly one: the listener adopted descriptor 3 and closed it, so nothing it starts inherits
    // the port. The listener may open descriptor 3 again for something else, so the check is on
    // the socket's holders, not on descriptor 3 being closed.
    expect(holders).toHaveLength(1);
    expect(holders[0]).not.toBe(3);
    expect(closeOnExec(pid, holders[0] as number)).toBe(true);
  });

  test("a command that cannot run exits non-zero, and its port binds again", async () => {
    const portFile = join(work, "missing.port");
    const missing = join(work, "no-such-command");
    const running = exec("exec of a missing command", portFile, [missing]);
    const code = await running.process.exited;
    expect(code, describeRun(running)).toBe(127);
    expect(running.output()).toContain(`held-port: execute ${missing}`);
    const port = await readPort(portFile, running);
    expect(listeningInode(port)).toBeUndefined();
    expect(await bindOutcome(port)).toBe("bound");
  });
});

gated("held-port.py keep", () => {
  test("two listeners started from one keeper serve its port, which stays held between them", async () => {
    const portFile = join(work, "kept.port");
    const socket = join(work, "keeper.sock");
    const keeper = start("the keeper", [
      python(),
      helper,
      "keep",
      "--port-file",
      portFile,
      "--socket",
      socket,
    ]);
    const port = await readPort(portFile, keeper);

    const first = exec("the first listener", join(work, "first.port"), [listener], socket);
    expect(await readPort(join(work, "first.port"), first)).toBe(port);
    await requireAnswers(port, first);
    first.process.kill("SIGKILL");
    await first.process.exited;

    // Between servers the kept socket still listens: the kernel accepts, and nothing answers.
    expect(await answerWithin(port, 2_000)).toBe("no answer");
    expect(await bindOutcome(port)).toBe("EADDRINUSE");

    const second = exec("the second listener", join(work, "second.port"), [listener], socket);
    expect(await readPort(join(work, "second.port"), second)).toBe(port);
    await requireAnswers(port, second);
    second.process.kill("SIGKILL");
    await second.process.exited;

    keeper.process.kill("SIGTERM");
    expect(await keeper.process.exited, describeRun(keeper)).toBe(0);
    expect(existsSync(socket)).toBe(false);
    expect(await bindOutcome(port)).toBe("bound");
  });
});

// --pass needs neither /proc nor the listener: the command is env(1), which prints the environment
// it was executed with and exits.
describe("held-port.py exec --pass", () => {
  // passExec runs held-port.py exec with environment as its own, each name in names given to
  // --pass, executing env(1), and answers env's exit code, its lines sorted, the helper's stderr
  // and the port file's path.
  function passExec(label: string, names: string[], environment: Record<string, string>) {
    const portFile = join(work, `${label}.port`);
    const run = Bun.spawnSync(
      [
        python(),
        helper,
        "exec",
        "--port-file",
        portFile,
        "--fd-env",
        "LISTEN_FD",
        "--env",
        "PORT={port}",
        ...names.flatMap((name) => ["--pass", name]),
        "--",
        "env",
      ],
      { env: environment, stdout: "pipe", stderr: "pipe" }
    );
    const lines = run.stdout.toString().split("\n").filter(Boolean).sort();
    return { code: run.exitCode, lines, stderr: run.stderr.toString(), portFile };
  }

  test("the command's environment is exactly the variables --pass names, the --env pairs and the descriptor's name", () => {
    const path = Bun.env.PATH ?? "";
    const url = "postgres://tester:not-a-secret@127.0.0.1:5999/dispatch";
    const { code, lines, stderr, portFile } = passExec("pass", ["PATH", "DATABASE_URL"], {
      PATH: path,
      DATABASE_URL: url,
      NOT_PASSED: "left behind",
    });
    expect(code, stderr).toBe(0);
    const port = readFileSync(portFile, "utf8").trim();
    expect(lines).toEqual(
      [`DATABASE_URL=${url}`, "LISTEN_FD=3", `PATH=${path}`, `PORT=${port}`].sort()
    );
  });

  test("a name --pass gives that the environment lacks is refused before the command runs", () => {
    const { code, lines, stderr, portFile } = passExec("pass-missing", ["PATH", "NOT_SET"], {
      PATH: Bun.env.PATH ?? "",
    });
    expect(code).toBe(1);
    expect(stderr).toContain("--pass names variables this process's environment lacks: NOT_SET");
    expect(lines).toEqual([]);
    expect(existsSync(portFile)).toBe(false);
  });
});
