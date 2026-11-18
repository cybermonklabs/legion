import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { existsSync, readdirSync, readFileSync, readlinkSync, realpathSync } from "node:fs";
import { type AddressInfo, connect, createServer, type Server, type Socket } from "node:net";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { settled, timeLeft, until } from "@legion/test-fixtures/wait";

// held-port.py's real-process tests: the helper hands its socket to the real envoy-listener of
// this branch, which serves on it through ENVOY_LISTEN_FD. They read /proc, so they run on Linux,
// and they build the listener with go; a host that is neither skips them, naming what it lacks.

const helper = join(import.meta.dir, "held-port.py");
const envoy = join(import.meta.dir, "..", "..", "..", "packages", "envoy");
// A short directory of its own: the keeper's unix socket lives here (docs/testing.md).
const work = temporaryDirectory("held-port");
const listener = join(work, "envoy-listener");
const O_CLOEXEC = 0o2000000;

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

const onLinux = process.platform === "linux";
if (!onLinux) {
  console.warn(
    `Skipped the tests in ${import.meta.path}. They read /proc, which this host's ` +
      `${process.platform} does not have; they run on linux.`
  );
}
// Both gates are read, so a host that has neither says so about both.
const withoutGo = skipWithout("go");
const skip = !onLinux || withoutGo;

beforeAll(async () => {
  if (skip) return;
  const build = Bun.spawnSync(["go", "build", "-o", listener, "./cmd/listener"], {
    cwd: envoy,
    stdout: "pipe",
    stderr: "pipe",
  });
  if (build.exitCode !== 0) {
    throw new Error(`build envoy-listener: ${build.stdout.toString()}${build.stderr.toString()}`);
  }
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
});

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
    "python3",
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

// readPort waits for path to hold a port, failing at once when running exits first. The writer is
// a real process with no event to await, so this and requireAnswers poll through the shared wait,
// and a wait that fails says what the process printed.
async function readPort(path: string, running: Running): Promise<number> {
  try {
    return await until(`${running.name} to write a port to ${path}`, () => {
      if (existsSync(path)) {
        const text = readFileSync(path, "utf8");
        if (!/^[1-9][0-9]*\n$/.test(text)) throw new Error(`${path} holds ${JSON.stringify(text)}`);
        return Number(text.trim());
      }
      if (running.exited()) throw new Error(`exited before it wrote ${path}`);
      return undefined;
    });
  } catch (error) {
    throw new Error(`${(error as Error).message}: ${describeRun(running)}`, { cause: error });
  }
}

// requireAnswers waits for /healthz on port to answer at all, failing at once when running exits
// first. Each request may take what is left of the row's deadline.
async function requireAnswers(port: number, running: Running): Promise<void> {
  const url = `http://127.0.0.1:${port}/healthz`;
  let last: unknown = "no request had ended";
  try {
    await until(`/healthz to answer on ${port}`, async () => {
      try {
        const signal = AbortSignal.timeout(timeLeft());
        const response = await fetch(url, { keepalive: false, signal });
        await response.arrayBuffer();
        return true;
      } catch (error) {
        last = error;
      }
      if (running.exited()) throw new Error(`exited before /healthz answered on ${port}`);
      return false;
    });
  } catch (error) {
    throw new Error(
      `${(error as Error).message}; the last request: ${last}\n${describeRun(running)}`,
      { cause: error }
    );
  }
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

// queuedRequest connects to port and sends a /healthz request on the connection: connected
// resolves once the kernel has accepted it, answer with the first bytes that come back, and
// answered says whether any have. A connection error, or a close before an answer, rejects both.
// It closes the connection itself once the answer comes, and the caller destroys the socket on
// any other way out.
function queuedRequest(port: number) {
  const connected = Promise.withResolvers<void>();
  const answer = Promise.withResolvers<string>();
  // Handled here as well, so a rejection no row has awaited yet does not end the run.
  void answer.promise.catch(() => {});
  let answered = false;
  const socket = connect(port, "127.0.0.1");
  socket.once("connect", () => {
    socket.write(`GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:${port}\r\n\r\n`);
    connected.resolve();
  });
  socket.once("data", (data: Buffer) => {
    answered = true;
    answer.resolve(data.toString("latin1"));
    // This end closes first, so the connection leaves no TIME_WAIT on the listener's end of the
    // kept port, which would refuse the row's last bind of that port.
    socket.destroy();
  });
  socket.once("error", (error) => {
    connected.reject(error);
    answer.reject(error);
  });
  socket.once("close", () => answer.reject(new Error("the connection closed with no answer")));
  return { socket, connected: connected.promise, answer: answer.promise, answered: () => answered };
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

// holdersOf returns, in pid order, every process that holds a descriptor linked to target.
function holdersOf(target: string): number[] {
  return readdirSync("/proc")
    .filter((entry) => /^[0-9]+$/.test(entry))
    .map(Number)
    .filter((pid) => {
      try {
        return descriptorsOf(pid, target).length > 0;
      } catch {
        // A process that has ended, or one this user may not read.
        return false;
      }
    })
    .sort((left, right) => left - right);
}

// closeOnExec reads the octal flags line of /proc/<pid>/fdinfo/<fd>.
function closeOnExec(pid: number, fd: number): boolean {
  const info = readFileSync(`/proc/${pid}/fdinfo/${fd}`, "utf8");
  const flags = /^flags:\s*([0-7]+)$/m.exec(info);
  if (!flags?.[1]) throw new Error(`fdinfo of descriptor ${fd} of process ${pid}:\n${info}`);
  return (Number.parseInt(flags[1], 8) & O_CLOEXEC) !== 0;
}

describe.skipIf(skip)("held-port.py exec", () => {
  test("the listener serves the handed socket on the port in the port file, in the helper's own process", async () => {
    const portFile = join(work, "serves.port");
    const running = exec("exec of the listener", portFile, [listener]);
    const port = await readPort(portFile, running);
    await requireAnswers(port, running);
    // The listener logs its line after it starts serving, so /healthz can answer before the line
    // is read: await the line itself.
    await settled(
      "the listener's line that it listens",
      running.logged(
        new RegExp(`"msg":"envoy-listener listening".*"addr":"127\\.0\\.0\\.1:${port}"`)
      )
    );
    // The PID Bun.spawn returned, a shell's $!, is the listener's: the helper executed it in place.
    expect(readlinkSync(`/proc/${running.process.pid}/exe`)).toBe(realpathSync(listener));
  });

  test("from the moment the port file names it until the server runs, the port is held", async () => {
    const portFile = join(work, "held.port");
    const running = start(
      "exec of a shell that waits, then the listener",
      [
        "python3",
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
    const code = await settled("the missing command's exit", running.process.exited);
    expect(code, describeRun(running)).toBe(127);
    expect(running.output()).toContain(`held-port: execute ${missing}`);
    const port = await readPort(portFile, running);
    expect(listeningInode(port)).toBeUndefined();
    expect(await bindOutcome(port)).toBe("bound");
  });
});

describe.skipIf(skip)("held-port.py keep", () => {
  test("two listeners started from one keeper serve its port, which stays held between them", async () => {
    const portFile = join(work, "kept.port");
    const socket = join(work, "keeper.sock");
    const keeper = start("the keeper", [
      "python3",
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

    // Between servers the kept socket still listens, and nothing serves it: the keeper alone holds
    // it, and the kernel accepts a connection into its queue, where the request waits unanswered
    // until the next listener takes the connection and answers it (held-port.py, keep).
    const inode = listeningInode(port);
    if (!inode) throw new Error(`nothing listens on 127.0.0.1:${port} between servers`);
    expect(holdersOf(`socket:[${inode}]`)).toEqual([keeper.process.pid]);
    const queued = queuedRequest(port);
    try {
      await settled("the kernel to accept a connection to the kept port", queued.connected);
      expect(await bindOutcome(port)).toBe("EADDRINUSE");
      expect(queued.answered()).toBe(false);

      const second = exec("the second listener", join(work, "second.port"), [listener], socket);
      expect(await readPort(join(work, "second.port"), second)).toBe(port);
      const answer = await settled(
        "the second listener's answer to the request queued between servers",
        queued.answer
      );
      expect(answer).toStartWith("HTTP/1.1 ");
      await requireAnswers(port, second);
      second.process.kill("SIGKILL");
      await second.process.exited;
    } finally {
      queued.socket.destroy();
    }

    keeper.process.kill("SIGTERM");
    expect(
      await settled("the keeper's exit on SIGTERM", keeper.process.exited),
      describeRun(keeper)
    ).toBe(0);
    expect(existsSync(socket)).toBe(false);
    expect(await bindOutcome(port)).toBe("bound");
  });
});
