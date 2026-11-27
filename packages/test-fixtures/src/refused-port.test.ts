import { afterAll, afterEach, describe, expect, test } from "bun:test";
import { fstatSync, readdirSync } from "node:fs";
import { connect, createServer } from "node:net";
import { holdRefusedPort, type RefusedPort, refusingOrigin } from "./refused-port";

const held: RefusedPort[] = [];
afterEach(() => {
  for (const port of held.splice(0)) port.release();
});

function hold(): RefusedPort {
  const port = holdRefusedPort();
  held.push(port);
  return port;
}

/** Connects to 127.0.0.1:`port` and answers "connected" or the connection's error code. */
async function dial(port: number): Promise<string> {
  const outcome = Promise.withResolvers<string>();
  const client = connect(port, "127.0.0.1");
  client.once("connect", () => outcome.resolve("connected"));
  client.once("error", (error: NodeJS.ErrnoException) =>
    outcome.resolve(error.code ?? String(error))
  );
  try {
    return await outcome.promise;
  } finally {
    client.destroy();
  }
}

/** Listens on 127.0.0.1:`port` and closes again: "listened", or the error code of the refusal. */
async function listenOutcome(port: number): Promise<string> {
  const server = createServer();
  const outcome = Promise.withResolvers<string>();
  server.once("error", (error: NodeJS.ErrnoException) =>
    outcome.resolve(error.code ?? String(error))
  );
  server.listen(port, "127.0.0.1", () => server.close(() => outcome.resolve("listened")));
  return await outcome.promise;
}

/** Runs `code` in another Bun process and answers what it printed. */
function inChild(code: string): string {
  const child = Bun.spawnSync([process.execPath, "-e", code], { stdout: "pipe", stderr: "pipe" });
  expect(child.stderr.toString()).toBe("");
  return child.stdout.toString();
}

/**
 * Every socket this process holds, each as `<descriptor>:<device>:<inode>`: the descriptors
 * `/dev/fd` lists (Linux links it to `/proc/self/fd`, macOS mounts its own), kept when `fstat`
 * says socket. The listing's own directory is in what it lists and closed once it is read, so its
 * descriptor answers `EBADF` and is passed over.
 */
function socketDescriptors(): string[] {
  const sockets: string[] = [];
  for (const entry of readdirSync("/dev/fd")) {
    const descriptor = Number(entry);
    try {
      const stats = fstatSync(descriptor);
      if (stats.isSocket()) sockets.push(`${descriptor}:${stats.dev}:${stats.ino}`);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EBADF") throw error;
    }
  }
  return sockets;
}

test("a connection to the held port is refused, and its origin names it", async () => {
  const refused = hold();
  expect(refused.port).toBeGreaterThan(0);
  expect(refused.origin).toBe(`http://127.0.0.1:${refused.port}`);
  expect(await dial(refused.port)).toBe("ECONNREFUSED");
});

test("a connection from another process is refused too", () => {
  // A test's code under test often dials from a child: the hold is the socket's, not the process's.
  const refused = hold();
  expect(
    inChild(`const s = require("node:net").connect(${refused.port}, "127.0.0.1");
s.once("connect", () => { console.log("connected"); s.destroy(); });
s.once("error", (e) => console.log(e.code));`)
  ).toBe("ECONNREFUSED\n");
});

test("no other socket can take the held port while it is held", async () => {
  const refused = hold();
  expect(await listenOutcome(refused.port)).toBe("EADDRINUSE");
  expect(await dial(refused.port)).toBe("ECONNREFUSED");
});

test("no other process can take the held port while it is held", () => {
  const refused = hold();
  expect(
    inChild(`const s = require("node:net").createServer();
s.once("error", (e) => console.log(e.code));
s.listen(${refused.port}, "127.0.0.1", () => { console.log("listened"); s.close(); });`)
  ).toBe("EADDRINUSE\n");
});

describe("refusingOrigin", () => {
  /** The held socket, as socketDescriptors names it; empty until the row below runs. */
  let socket = "";
  afterAll(() => {
    // The row has finished, so the fixture has closed its socket: no descriptor of this process
    // is that socket any more. A bind of the port would not say so, since any other process on the
    // machine may bind a port once it is free.
    expect(socket).not.toBe("");
    expect(socketDescriptors()).not.toContain(socket);
  });

  test("holds a refused port for the row that calls it, and closes its socket once the row is done", async () => {
    const before = new Set(socketDescriptors());
    const origin = new URL(refusingOrigin());
    // The call opens one socket and nothing else, and no other code runs while it does.
    const opened = socketDescriptors().filter((descriptor) => !before.has(descriptor));
    expect(opened).toHaveLength(1);
    socket = opened[0] ?? "";
    const port = Number(origin.port);
    expect(origin.origin).toBe(`http://127.0.0.1:${port}`);
    expect(await dial(port)).toBe("ECONNREFUSED");
    expect(await listenOutcome(port)).toBe("EADDRINUSE");
  });
});
