import { afterEach, expect, test } from "bun:test";
import { connect, createServer } from "node:net";
import { holdRefusedPort, type RefusedPort } from "./refused-port";

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

test("a connection to the held port is refused, and its origin names it", async () => {
  const refused = hold();
  expect(refused.port).toBeGreaterThan(0);
  expect(refused.origin).toBe(`http://127.0.0.1:${refused.port}`);
  expect(await dial(refused.port)).toBe("ECONNREFUSED");
});

test("a connection from another process is refused too", async () => {
  // A test's code under test often dials from a child: the hold is the socket's, not the process's.
  const refused = hold();
  const child = Bun.spawnSync(
    [
      process.execPath,
      "-e",
      `const s = require("node:net").connect(${refused.port}, "127.0.0.1");
s.once("connect", () => { console.log("connected"); s.destroy(); });
s.once("error", (e) => console.log(e.code));`,
    ],
    { stdout: "pipe", stderr: "pipe" }
  );
  expect(child.stderr.toString()).toBe("");
  expect(child.stdout.toString()).toBe("ECONNREFUSED\n");
});

test("no other socket can take the held port while it is held", async () => {
  const refused = hold();
  expect(await listenOutcome(refused.port)).toBe("EADDRINUSE");
  expect(await dial(refused.port)).toBe("ECONNREFUSED");
});
