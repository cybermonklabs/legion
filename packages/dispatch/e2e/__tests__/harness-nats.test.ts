import { expect, test } from "bun:test";
import { chmodSync, readFileSync, writeFileSync } from "node:fs";
import { connect } from "node:net";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

import { natsBinVariable, natsStoreDirectory, stackNats } from "../harness-nats";

// What the pool's NATS provider (e2e/harness-nats.ts) starts for each stack, read off a stand-in
// for nats-server: a Bun script that prints the argv it was given, listens on a port of 127.0.0.1
// the system picks, writes the two lines nats-server writes as its start ends, and answers each
// connection with one INFO line naming the store it was started with, as NATS answers a client
// with its INFO first. So a row reads which store, and so which stack, the URL handed to that
// stack reaches. Two stacks on one server were why the Legion intake of the second never read its
// daemon: that stack's server found the durable bound to the first one's subject and stood by.
const scratch = temporaryDirectory("harness-nats-");

/** Writes `body` as an executable file of the scratch directory, and returns its path. */
function executable(name: string, body: string): string {
  const path = join(scratch, name);
  writeFileSync(path, body);
  chmodSync(path, 0o755);
  return path;
}

const standIn = `#!/usr/bin/env bun
const args = process.argv.slice(2);
const store = args[args.indexOf("-sd") + 1];
const server = Bun.listen({
  hostname: "127.0.0.1",
  port: 0,
  socket: {
    data() {},
    open(socket) {
      socket.write(\`INFO \${JSON.stringify({ store })}\\r\\n\`);
    },
  },
});
console.log(\`stand-in argv \${JSON.stringify(args)}\`);
console.error(\`[\${process.pid}] 2026/11/28 00:00:00.000000 [INF] Starting nats-server\`);
console.error(\`[\${process.pid}] 2026/11/28 00:00:00.000001 [INF] Listening for client connections on 127.0.0.1:\${server.port}\`);
console.error(\`[\${process.pid}] 2026/11/28 00:00:00.000002 [INF] Server is ready\`);
`;

/** The INFO line the server at `url` answers a new connection with, or the error of a connection
 *  it refuses. */
async function firstLine(url: string): Promise<string> {
  const { hostname, port } = new URL(url);
  const answered = Promise.withResolvers<string>();
  const socket = connect({ host: hostname, port: Number(port) });
  let read = "";
  socket.on("data", (chunk) => {
    read += chunk.toString();
    const end = read.indexOf("\r\n");
    if (end >= 0) answered.resolve(read.slice(0, end));
  });
  socket.on("error", (error) => answered.reject(error));
  try {
    return await answered.promise;
  } finally {
    socket.destroy();
  }
}

test("no binary named starts no NATS server", () => {
  expect(stackNats({ dir: scratch, env: {} })).toBeUndefined();
  expect(stackNats({ dir: scratch, env: { [natsBinVariable]: " " } })).toBeUndefined();
});

test("a binary that is no file is refused by name before anything starts", () => {
  const missing = join(scratch, "no-such-nats-server");
  expect(() => stackNats({ dir: scratch, env: { [natsBinVariable]: missing } })).toThrow(
    `${natsBinVariable}=${missing} names no file`
  );
});

test("each stack gets a NATS server of its own, with its own store, port and log", async () => {
  const bin = executable("stand-in-nats-server", standIn);
  const dir = join(scratch, "run");
  const servers = stackNats({ dir, env: { [natsBinVariable]: bin } });
  if (servers === undefined) throw new Error("a binary that exists started no provider");
  const logDirs = [0, 1].map((index) => join(dir, `worker-${index}`, "log"));
  const started = await Promise.all(logDirs.map((logDir, index) => servers.start(index, logDir)));
  try {
    expect(started[0].url).not.toBe(started[1].url);
    for (const [index, stack] of started.entries()) {
      expect(stack.url).toMatch(/^nats:\/\/127\.0\.0\.1:\d+$/);
      const store = join(dir, `worker-${index}`, natsStoreDirectory);
      // The URL handed to stack `index` reaches the server started with that stack's store.
      expect(await firstLine(stack.url)).toBe(`INFO ${JSON.stringify({ store })}`);
      // JetStream on, on loopback, at a port the server picks itself, in the stack's own store.
      const log = readFileSync(join(logDirs[index], "nats.log"), "utf8");
      expect(log).toContain(
        `stand-in argv ${JSON.stringify(["-js", "-a", "127.0.0.1", "-p", "-1", "-sd", store])}`
      );
    }
  } finally {
    await Promise.all(started.map((stack) => stack.stop()));
  }
  // A stopped stack's server answers nobody any more.
  for (const stack of started) await expect(firstLine(stack.url)).rejects.toThrow();
});

test("a server that exits before its start ends is refused with its last lines", async () => {
  const bin = executable(
    "failing-nats-server",
    '#!/bin/sh\necho "[1] 2026/11/28 00:00:00.000000 [FTL] Error listening on port" >&2\nexit 1\n'
  );
  const dir = join(scratch, "failing");
  const servers = stackNats({ dir, env: { [natsBinVariable]: bin } });
  if (servers === undefined) throw new Error("a binary that exists started no provider");
  const logDir = join(dir, "worker-0", "log");
  await expect(servers.start(0, logDir)).rejects.toThrow(
    /stack 0's NATS server exited 1 before its start ended \(.*nats\.log\)\. Its last lines:\n.*\[FTL\] Error listening on port/
  );
});
