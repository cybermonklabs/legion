import { afterEach, describe, expect, test } from "bun:test";
import { closeSync, openSync } from "node:fs";
import { createServer } from "node:net";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Subprocess } from "bun";
import {
  LISTENER_PORT_LINE,
  NATS_CLIENT_PORT_LINE,
  NATS_MONITOR_PORT_LINE,
  reportedPort,
} from "../../test/envoy-stack";

// The real-stack harness starts natsd and the listener on ports the kernel chooses (natsd's
// `-p -1 -m -1`, the listener's PORT=0) and reads each back from the line the process logs once it
// listens, rather than picking ports free and handing them over: another process on a shared
// machine can bind a port in the moment between its release and its server's bind, and CI failed
// that way (a test reading another server's 404, and a harness's listener dying with `bind:
// address already in use`). So what a stack's caller dials is the port its own process bound, and
// that process holds it from the kernel's choice on. These rows run a stand-in that binds port 0
// and logs each server's line as that server writes it, and read the port the way the stack does.

const makeDirectory = temporaryDirectories("reported-port");
const BUDGET = { timeoutMs: 30_000, tailLines: 10 };
const children: Subprocess[] = [];
afterEach(async () => {
  for (const child of children.splice(0)) {
    child.kill("SIGKILL");
    await child.exited;
  }
});

/** Runs `script` with Bun, its stdout and stderr into a log of its own, as the stack runs a server. */
function serve(script: string): { readonly child: Subprocess; readonly log: string } {
  const log = path.join(makeDirectory("server"), "server.log");
  const out = openSync(log, "a");
  const child = Bun.spawn(["bun", "-e", script], { stdout: out, stderr: out });
  closeSync(out);
  children.push(child);
  return { child, log };
}

/** A server on a port the kernel chose that answers `who`, and logs `line` with {port} in it. */
function answering(who: string, line: string): string {
  return `const server = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch: () => new Response(${JSON.stringify(who)}) });
console.log(${JSON.stringify(line)}.replace("{port}", String(server.port)));`;
}

/** "bound", or the error code a bind of 127.0.0.1:port is refused with. */
async function bindOutcome(port: number): Promise<string> {
  const server = createServer();
  const outcome = Promise.withResolvers<string>();
  server.once("error", (error: NodeJS.ErrnoException) => outcome.resolve(error.code ?? "error"));
  server.listen(port, "127.0.0.1", () => server.close(() => outcome.resolve("bound")));
  return await outcome.promise;
}

describe("the port a stack's process reports", () => {
  const lines: [string, RegExp, string][] = [
    [
      "natsd's client port",
      NATS_CLIENT_PORT_LINE,
      "[1] 2026/11/16 00:00:00.000000 [INF] Listening for client connections on 127.0.0.1:{port}",
    ],
    [
      "natsd's monitoring port",
      NATS_MONITOR_PORT_LINE,
      "[1] 2026/11/16 00:00:00.000000 [INF] Starting http monitor on 127.0.0.1:{port}",
    ],
    [
      "the listener's port",
      LISTENER_PORT_LINE,
      '{"time":"2026-11-16T00:00:00Z","level":"INFO","msg":"envoy-listener listening","addr":"127.0.0.1:{port}"}',
    ],
  ];
  for (const [what, line, written] of lines) {
    test(`is ${what} that process serves on, held by it`, async () => {
      const { child, log } = serve(answering("the stack's own server", written));

      const port = await reportedPort(what, child, log, BUDGET, line);

      expect(await (await fetch(`http://127.0.0.1:${port}/`)).text()).toBe(
        "the stack's own server"
      );
      // No other process can take it while the stack's own serves there.
      expect(await bindOutcome(port)).toBe("EADDRINUSE");
    });
  }

  test("is a failure naming the process's log when it exits before it reports one", async () => {
    const { child, log } = serve(
      'console.log("natsd: listen tcp 127.0.0.1:4222: bind: address already in use"); process.exit(1);'
    );

    await expect(
      reportedPort("natsd's client port", child, log, BUDGET, NATS_CLIENT_PORT_LINE)
    ).rejects.toThrow(
      "natsd's client port exited (1): natsd: listen tcp 127.0.0.1:4222: bind: address already in use"
    );
  });
});
