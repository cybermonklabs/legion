import { afterAll, describe, expect, test } from "bun:test";
import { readdirSync, readFileSync, readlinkSync } from "node:fs";
import type { Subprocess } from "bun";
import { connect, StringCodec } from "nats";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
  useDispatchStackDirectory,
} from "../../test/dispatch-stack";
import {
  type EnvoyStack,
  envoyStackEnabled,
  type PortChosen,
  type StackServer,
  startEnvoyStack,
  useEnvoyStackDirectory,
} from "../../test/envoy-stack";

// A port a stack starts one of its servers on is the server's from the moment the stack chooses
// it: another program that binds the port in between must be refused, and the server must serve.
// Each row makes that race certain. At every port the stack chooses, before the server it is for
// starts (the stacks' `portChosen`), a second program tries to bind the port and keeps it when it
// can. A stack that chose a number and let it go loses the port there, and its server exits with
// `bind: address already in use`; a stack that holds the socket it chose, and starts its server
// on it, does not. A server that takes no socket from the stack (natsd) binds ports the kernel
// chooses for it, and the stack reads them from its log, so the stack chooses no port for it that
// another program could take.
//
// These rows read /proc, so they need Linux, as the stacks' own CI job is.

const codec = StringCodec();

/** A second program that tried to bind a port the stack chose. */
interface Rival {
  readonly server: StackServer;
  readonly port: number;
  /** `bound` when it took the port, else the code its bind failed with (`EADDRINUSE`). */
  readonly outcome: string;
  readonly process: Subprocess;
}

// The second program: it binds 127.0.0.1 at RIVAL_PORT, prints `bound` or the error's code, and
// keeps a port it bound until it is killed.
const RIVAL_SOURCE = [
  'const server = require("node:net").createServer();',
  'server.once("error", (error) => { console.log(error.code); process.exit(0); });',
  'server.listen(Number(process.env.RIVAL_PORT), "127.0.0.1", () => console.log("bound"));',
].join("\n");

/** Starts a second program on `port` and resolves once it has said whether it took the port. */
async function startRival(server: StackServer, port: number): Promise<Rival> {
  const child = Bun.spawn([process.execPath, "-e", RIVAL_SOURCE], {
    env: { RIVAL_PORT: String(port) },
    stdout: "pipe",
    stderr: "inherit",
  });
  const decoder = new TextDecoder();
  let said = "";
  for await (const chunk of child.stdout) {
    said += decoder.decode(chunk, { stream: true });
    if (said.includes("\n")) break;
  }
  return { server, port, outcome: said.trim() || "said nothing", process: child };
}

/** Every second program the file starts, killed once the file is done. */
const everyRival: Rival[] = [];

/** One row's second programs: the row's stack is handed `portChosen`, and `started` lists them. */
interface Rivals {
  readonly portChosen: PortChosen;
  readonly started: readonly Rival[];
}

/**
 * Rivals that send a second program at each port the stack chooses for `only`, or at every port it
 * chooses when `only` is undefined.
 */
function rivalsFor(only?: StackServer): Rivals {
  const started: Rival[] = [];
  return {
    started,
    portChosen: async (server, port) => {
      if (only !== undefined && server !== only) return;
      const rival = await startRival(server, port);
      everyRival.push(rival);
      started.push(rival);
    },
  };
}

/** What each of `rivals`' second programs got, as `<server> <outcome>`. */
function outcomes(rivals: Rivals): string[] {
  return rivals.started.map(({ server, outcome }) => `${server} ${outcome}`);
}

/** The port of `url`, as a number. */
function portOf(url: string): number {
  return Number(new URL(url).port);
}

/**
 * The inode of the socket listening on 127.0.0.1:`port`, from /proc/net/tcp, or undefined when
 * none listens there. The local address is 127.0.0.1 in the host's byte order (0100007F on the
 * little-endian machines the rows run on) and the port, in hexadecimal; state 0A is LISTEN, and
 * the tenth field is the inode.
 */
function listeningInode(port: number): string | undefined {
  const local = `0100007F:${port.toString(16).toUpperCase().padStart(4, "0")}`;
  for (const line of readFileSync("/proc/net/tcp", "utf8").split("\n").slice(1)) {
    const fields = line.trim().split(/\s+/);
    if (fields[1] === local && fields[3] === "0A") return fields[9];
  }
  return undefined;
}

/** The descriptors of this test process that are the socket with `inode`. */
function ownDescriptorsOf(inode: string): string[] {
  return readdirSync("/proc/self/fd").filter((fd) => {
    try {
      return readlinkSync(`/proc/self/fd/${fd}`) === `socket:[${inode}]`;
    } catch {
      return false;
    }
  });
}

/** The pids of the processes that hold the socket with `inode`, from their /proc fd links. */
function holdersOf(inode: string): number[] {
  const holders: number[] = [];
  for (const pid of readdirSync("/proc").filter((entry) => /^\d+$/.test(entry))) {
    try {
      if (
        readdirSync(`/proc/${pid}/fd`).some(
          (fd) => readlinkSync(`/proc/${pid}/fd/${fd}`) === `socket:[${inode}]`
        )
      ) {
        holders.push(Number(pid));
      }
    } catch {
      // Another user's process, or one that exited during the walk: not a holder of this test's.
    }
  }
  return holders;
}

/** The server serves on `port`, and the stack, which is this process, keeps no copy of it. */
function expectServedByTheServerAlone(port: number): void {
  const inode = listeningInode(port);
  expect(inode).toBeDefined();
  expect(ownDescriptorsOf(inode ?? "")).toEqual([]);
}

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped the processes writing into it.
const dispatchDirectory = useDispatchStackDirectory();
const listenerDirectory = useEnvoyStackDirectory();
const natsDirectory = useEnvoyStackDirectory();

afterAll(async () => {
  for (const rival of everyRival) rival.process.kill();
  await Promise.all(everyRival.map((rival) => rival.process.exited));
});

describe.skipIf(!dispatchStackEnabled)("the Dispatch stack's port", () => {
  let stack: DispatchStack | undefined;
  afterAll(async () => {
    await stack?.stop();
  }, 60_000);

  test("Dispatch serves on the port the stack chose, though a second program binds that port before Dispatch starts", async () => {
    const rivals = rivalsFor("dispatch");
    stack = await startDispatchStack({ work: dispatchDirectory, portChosen: rivals.portChosen });
    expect(outcomes(rivals)).toEqual(["dispatch EADDRINUSE"]);
    expect(portOf(stack.url)).toBe(rivals.started[0]?.port ?? 0);
    const response = await fetch(`${stack.url}/api/v1`, { signal: AbortSignal.timeout(10_000) });
    expect(response.status).toBe(200);
    expectServedByTheServerAlone(portOf(stack.url));
  }, 600_000);
});

describe.skipIf(!envoyStackEnabled)("the Envoy stack's ports", () => {
  const stacks: EnvoyStack[] = [];
  afterAll(async () => {
    for (const stack of stacks) await stack.stop();
  }, 60_000);

  test("the listener serves on the port the stack chose, though a second program binds that port before the listener starts", async () => {
    const rivals = rivalsFor("listener");
    const stack = await startEnvoyStack({ work: listenerDirectory, portChosen: rivals.portChosen });
    stacks.push(stack);
    expect(outcomes(rivals)).toEqual(["listener EADDRINUSE"]);
    expect(portOf(stack.envoyUrl)).toBe(rivals.started[0]?.port ?? 0);
    const health = await fetch(`${stack.envoyUrl}/healthz`, {
      signal: AbortSignal.timeout(10_000),
    });
    expect(await health.json()).toMatchObject({ status: "healthy" });
    expectServedByTheServerAlone(portOf(stack.envoyUrl));
  }, 600_000);

  test("NATS serves, and the stack's clients reach it, though a second program takes every port the stack chooses", async () => {
    const rivals = rivalsFor();
    const stack = await startEnvoyStack({ work: natsDirectory, portChosen: rivals.portChosen });
    stacks.push(stack);
    // The listener's port is the one port the stack chooses, and it holds it: natsd's two ports
    // are the kernel's choice, made as natsd binds them.
    expect(outcomes(rivals)).toEqual(["listener EADDRINUSE"]);
    // natsd chose its client port itself: the one process that holds the socket listening on it
    // was started with -p -1 -m -1, nats-server's RANDOM_PORT, and with no port number of the
    // stack's. A stack that chose a number and handed it over would fail here, rival or not.
    const natsInode = listeningInode(portOf(stack.natsUrl.replace(/^nats:/, "http:")));
    expect(natsInode).toBeDefined();
    const natsHolders = holdersOf(natsInode ?? "");
    expect(natsHolders).toHaveLength(1);
    const natsArgv = readFileSync(`/proc/${natsHolders[0]}/cmdline`, "utf8").split("\0");
    expect(natsArgv.slice(1, 5)).toEqual(["-p", "-1", "-m", "-1"]);
    const monitor = await fetch(`${stack.monitorUrl}/healthz`, {
      signal: AbortSignal.timeout(10_000),
    });
    expect(await monitor.json()).toMatchObject({ status: "ok" });
    // The stack's clients reach it: the listener, whose /healthz is healthy only with its NATS
    // connection up, and a client of this row, which gets back what it sent.
    const health = await fetch(`${stack.envoyUrl}/healthz`, {
      signal: AbortSignal.timeout(10_000),
    });
    expect(await health.json()).toMatchObject({ status: "healthy" });
    const client = await connect({ servers: stack.natsUrl, timeout: 10_000 });
    try {
      client.subscribe("stack-ports.echo", {
        callback: (_error, message) => message.respond(message.data),
      });
      const reply = await client.request("stack-ports.echo", codec.encode("reached"), {
        timeout: 10_000,
      });
      expect(codec.decode(reply.data)).toBe("reached");
    } finally {
      await client.close();
    }
  }, 600_000);
});
