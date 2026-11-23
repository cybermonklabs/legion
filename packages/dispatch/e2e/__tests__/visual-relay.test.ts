import { afterEach, expect, test } from "bun:test";
import { spawn } from "node:child_process";
import { createConnection, createServer, type Server, type Socket } from "node:net";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { readyLine, relayPortsToSockets, relaySocketsToPorts } from "../visual-relay";
import { rowLimit, runBounded } from "./bounded-subprocess";

// The visual review's browser has no network but loopback (e2e/visual-run.sh), and reaches the
// host's stack through the two halves of e2e/visual-relay.ts. A real run puts the halves in two
// network namespaces on one port; these rows run both in one, so the container half listens on a
// port of its own and names the host half's socket by the upstream's port. Every listener a row
// reads is held from the moment it binds until the row ends, and no row names a port it freed:
// on a shared machine another program can take a freed port.

const relay = join(import.meta.dir, "..", "visual-relay.ts");
const opened: Server[] = [];
const directory = temporaryDirectories("visual-relay");

/** A loopback port nothing listens on, so every connection to it is refused: below 1024, where
 *  no unprivileged program can bind, and port 1 (tcpmux) is served by nothing on these machines. */
const refusedPort = 1;

afterEach(() => {
  for (const server of opened.splice(0)) server.close();
});

function portOf(server: Server): number {
  const address = server.address();
  if (address === null || typeof address === "string") throw new Error("no TCP port");
  return address.port;
}

/** An upstream on 127.0.0.1 that answers each connection with `answer` of what it read, once
 *  the client has ended its half, and then ends its own: a request whose body ends before its
 *  answer, as HTTP's is. */
async function upstream(answer: (read: string) => string): Promise<number> {
  const server = createServer({ allowHalfOpen: true }, (socket) => {
    let read = "";
    socket.setEncoding("utf8");
    socket.on("data", (chunk: string) => {
      read += chunk;
    });
    socket.on("end", () => socket.end(answer(read)));
  });
  opened.push(server);
  const { promise, resolve } = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", resolve);
  await promise;
  return portOf(server);
}

/** Both halves for `port`: the host half's socket for it, and the container half on a port the
 *  OS picks, which is returned. */
async function bridge(port: number): Promise<number> {
  const sockets = directory();
  opened.push(...(await relayPortsToSockets(sockets, [port])));
  const [near] = await relaySocketsToPorts(sockets, [{ listenPort: 0, socketPort: port }]);
  if (near === undefined) throw new Error("no container half");
  opened.push(near);
  return portOf(near);
}

/** Sends `body` on a fresh connection to `port`, ends the send, and answers everything read
 *  before the far end closed. */
async function exchange(port: number, body: string): Promise<string> {
  const socket: Socket = createConnection({ allowHalfOpen: true, host: "127.0.0.1", port });
  socket.setEncoding("utf8");
  let read = "";
  socket.on("data", (chunk: string) => {
    read += chunk;
  });
  const { promise, resolve, reject } = Promise.withResolvers<string>();
  socket.on("error", reject);
  socket.on("close", () => resolve(read));
  socket.end(body);
  return promise;
}

test("a request crosses both halves and its answer comes back after the request's end", async () => {
  const port = await upstream((read) => `answered ${read.length} bytes: ${read}`);
  expect(await exchange(await bridge(port), "GET / HTTP/1.1")).toBe(
    "answered 14 bytes: GET / HTTP/1.1"
  );
});

test("connections at once keep their own bytes", async () => {
  const port = await upstream((read) => read.toUpperCase());
  const near = await bridge(port);
  const bodies = Array.from({ length: 20 }, (_, at) => `body ${at} `.repeat(2_000));
  expect(await Promise.all(bodies.map((body) => exchange(near, body)))).toEqual(
    bodies.map((body) => body.toUpperCase())
  );
});

test("a port nothing listens on closes the connection rather than holding it open", async () => {
  expect(await exchange(await bridge(refusedPort), "anyone there")).toBe("");
});

/** Runs the CLI with `args` as a bounded child of this row, and answers its exit status and what
 *  it printed on stdout and on stderr. */
async function cli(
  args: readonly string[]
): Promise<{ status: number; stdout: string; stderr: string }> {
  const ran = await runBounded({
    argv: [process.execPath, relay, ...args],
    bound: "short",
    from: import.meta.url,
  });
  return { status: ran.code, stderr: ran.stderr, stdout: ran.stdout };
}

test(
  "the container half runs its command once its ports listen, and exits as the command does",
  async () => {
    // The port is `0`, so the container half takes one the kernel chooses and names it to the
    // command; the command connects to it, so it can only pass once that port listens: `exit 3`
    // is reached after a connection the relay accepted.
    const probe =
      'require("node:net").createConnection(Number(process.env.VISUAL_RELAY_PORTS), "127.0.0.1")' +
      '.on("connect", () => process.exit(3)).on("error", () => process.exit(9))';
    const ran = await cli(["container", directory(), "0", "--", process.execPath, "-e", probe]);
    expect(ran).toEqual({ status: 3, stderr: "", stdout: "" });
  },
  rowLimit("short")
);

// The host half runs until a signal ends it, so this row drives it rather than waits for it: it
// reads the ready line, sends SIGTERM, and kills it outright in the end if it is still running.
test("the host half prints its ready line once its sockets listen, and stops on SIGTERM", async () => {
  const port = await upstream(() => "");
  const child = spawn(process.execPath, [relay, "host", directory(), String(port)], {
    stdio: ["ignore", "pipe", "inherit"],
  });
  try {
    child.stdout.setEncoding("utf8");
    const first = Promise.withResolvers<string>();
    child.stdout.once("data", (chunk: string) => first.resolve(chunk));
    expect(await first.promise).toBe(`${readyLine}\n`);
    const ended = Promise.withResolvers<number | null>();
    child.on("exit", (status) => ended.resolve(status));
    child.kill("SIGTERM");
    expect(await ended.promise).toBe(0);
  } finally {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
  }
});

test(
  "a word that is no port is refused by name",
  async () => {
    expect(await cli(["host", directory(), "80a"])).toEqual({
      status: 2,
      stderr: 'e2e/visual-relay.ts: "80a" is not a port\n',
      stdout: "",
    });
    expect((await cli(["host", directory(), "0"])).stderr).toBe(
      'e2e/visual-relay.ts: "0" is not a port\n'
    );
  },
  rowLimit("short", "short")
);
