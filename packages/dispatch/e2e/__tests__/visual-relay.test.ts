import { afterEach, expect, test } from "bun:test";
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { createConnection, createServer, type Server, type Socket } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { readyLine, relayPortsToSockets, relaySocketsToPorts } from "../visual-relay";

// The visual review's browser has no network but loopback (e2e/visual-run.sh), and reaches the
// host's stack through the two halves of e2e/visual-relay.ts. A real run puts the halves in two
// network namespaces on one port; these rows run both in one, so the container half listens on a
// port of its own and names the host half's socket by the upstream's port.

const relay = join(import.meta.dir, "..", "visual-relay.ts");
const opened: Server[] = [];
const directories: string[] = [];

afterEach(() => {
  for (const server of opened.splice(0)) server.close();
  for (const directory of directories.splice(0))
    rmSync(directory, { force: true, recursive: true });
});

function directory(): string {
  const made = mkdtempSync(join(tmpdir(), "visual-relay-"));
  directories.push(made);
  return made;
}

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
  // A port that was listening and is not now: nothing answers it on 127.0.0.1.
  const gone = await upstream(() => "");
  const closed = opened.pop();
  const { promise, resolve } = Promise.withResolvers<void>();
  closed?.close(() => resolve());
  await promise;
  expect(await exchange(await bridge(gone), "anyone there")).toBe("");
});

/** Runs the CLI with `args` and answers its exit status and what it printed. */
async function cli(args: readonly string[]): Promise<{ status: number | null; out: string }> {
  const child = spawn(process.execPath, [relay, ...args], { stdio: ["ignore", "pipe", "pipe"] });
  let out = "";
  child.stdout.setEncoding("utf8");
  child.stdout.on("data", (chunk: string) => {
    out += chunk;
  });
  child.stderr.setEncoding("utf8");
  child.stderr.on("data", (chunk: string) => {
    out += chunk;
  });
  const { promise, resolve } = Promise.withResolvers<{ status: number | null; out: string }>();
  child.on("exit", (status) => resolve({ out, status }));
  return promise;
}

test("the container half runs its command once its ports listen, and exits as the command does", async () => {
  const port = await upstream(() => "");
  const listening = opened.pop();
  const { promise, resolve } = Promise.withResolvers<void>();
  listening?.close(() => resolve());
  await promise;
  // The command connects to the port the container half listens on, so it can only pass once
  // that port listens: `exit 3` is reached after a connection the relay accepted.
  const probe =
    `require("node:net").createConnection(${port}, "127.0.0.1")` +
    '.on("connect", () => process.exit(3)).on("error", () => process.exit(9))';
  const ran = await cli([
    "container",
    directory(),
    String(port),
    "--",
    process.execPath,
    "-e",
    probe,
  ]);
  expect(ran).toEqual({ out: "", status: 3 });
});

test("the host half prints its ready line once its sockets listen, and stops on SIGTERM", async () => {
  const port = await upstream(() => "");
  const child = spawn(process.execPath, [relay, "host", directory(), String(port)], {
    stdio: ["ignore", "pipe", "inherit"],
  });
  child.stdout.setEncoding("utf8");
  const first = Promise.withResolvers<string>();
  child.stdout.once("data", (chunk: string) => first.resolve(chunk));
  expect(await first.promise).toBe(`${readyLine}\n`);
  const ended = Promise.withResolvers<number | null>();
  child.on("exit", (status) => ended.resolve(status));
  child.kill("SIGTERM");
  expect(await ended.promise).toBe(0);
});

test("a word that is no port is refused by name", async () => {
  expect(await cli(["host", directory(), "80a"])).toEqual({
    out: 'e2e/visual-relay.ts: "80a" is not a port\n',
    status: 2,
  });
});
