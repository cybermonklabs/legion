/** The visual review's way from a browser with no network of its own to the stack on the host.
 *
 *  `e2e/visual-run.sh` starts the Playwright container with `--network none`, so Chromium sits
 *  in a network namespace that holds a loopback device and nothing else, and no interface the
 *  host adds or removes reaches it. Chromium fails a navigation in flight with
 *  `net::ERR_NETWORK_CHANGED` when the namespace it runs in gains or loses an address, and a
 *  host that starts and stops docker networks does that all day, so on the host's own network a
 *  page could fail for no fault of its own.
 *
 *  The stack still listens on the host's 127.0.0.1, and a page must reach it at 127.0.0.1 too:
 *  that origin is a secure context, which another address is not, and the application under
 *  review needs one (`navigator.locks`, `crypto.randomUUID`). So the bridge is two halves of
 *  one relay, joined by Unix sockets in a directory both sides mount:
 *
 *    bun e2e/visual-relay.ts host <directory> <port>...
 *      on the host: a socket `<directory>/<port>` for each port, each connection to it carried
 *      to 127.0.0.1:<port>. It prints `visual-relay: ready` once every socket listens, and runs
 *      until a signal ends it.
 *
 *    node e2e/visual-relay.ts container <directory> <port>... -- <command>...
 *      in the container: 127.0.0.1:<port> for each port, each connection to it carried to the
 *      socket `<directory>/<port>`. Once every port listens it runs `<command>`, and exits as
 *      the command does.
 *
 *  Both halves pass bytes and nothing else, in both directions, and an end of one direction
 *  ends that direction alone (`allowHalfOpen`), so HTTP, a WebSocket and a request body that the
 *  client ends before the answer all pass as they would over one TCP connection.
 *
 *  It runs under bun on the host and under node in the image, which has no bun, so it imports
 *  `node:` modules alone and uses only the TypeScript syntax node strips. */
import { spawn } from "node:child_process";
import { createConnection, createServer, type Server, type Socket } from "node:net";
import { constants } from "node:os";
import { join } from "node:path";

/** The line `host` prints once every socket listens, which `e2e/visual-run.sh` reads before it
 *  starts the container. */
export const readyLine = "visual-relay: ready";

/** Joins two sockets into one stream, each way. An error or a close on either side destroys the
 *  other, so a connection the far end refused or reset ends the near one too, rather than
 *  leaving a reader that waits for bytes that cannot come. */
function pipeBoth(near: Socket, far: Socket): void {
  near.pipe(far);
  far.pipe(near);
  const end = () => {
    near.destroy();
    far.destroy();
  };
  for (const socket of [near, far]) {
    socket.on("error", end);
    socket.on("close", end);
  }
}

/** A listener that carries each connection it accepts to `to`, opened afresh per connection. */
function relayServer(to: () => Socket): Server {
  return createServer({ allowHalfOpen: true }, (near) => pipeBoth(near, to()));
}

/** Listens on `address` (a port on 127.0.0.1, or a socket path) and answers once it does. */
function listen(server: Server, address: number | string): Promise<void> {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  server.once("error", reject);
  const listening = () => {
    server.off("error", reject);
    resolve();
  };
  if (typeof address === "number") server.listen(address, "127.0.0.1", listening);
  else server.listen(address, listening);
  return promise;
}

/** The host half: a socket under `directory` for each port, carried to 127.0.0.1:<port>. */
export async function relayPortsToSockets(
  directory: string,
  ports: readonly number[]
): Promise<Server[]> {
  const servers = ports.map((port) =>
    relayServer(() => createConnection({ allowHalfOpen: true, host: "127.0.0.1", port }))
  );
  await Promise.all(
    servers.map((server, at) => listen(server, join(directory, String(ports[at]))))
  );
  return servers;
}

/** The container half: 127.0.0.1:<listenPort> for each pair, carried to the socket under
 *  `directory` named for its `socketPort`. The two are one port in a real run; a test that runs
 *  both halves in one namespace gives them two, since one port cannot listen twice there. */
export async function relaySocketsToPorts(
  directory: string,
  pairs: readonly { readonly listenPort: number; readonly socketPort: number }[]
): Promise<Server[]> {
  const servers = pairs.map(({ socketPort }) =>
    relayServer(() =>
      createConnection({ allowHalfOpen: true, path: join(directory, String(socketPort)) })
    )
  );
  await Promise.all(servers.map((server, at) => listen(server, pairs[at]?.listenPort ?? 0)));
  return servers;
}

/** The ports a command line names, each a port in canonical decimal, or a refusal naming the
 *  first that is not. */
function portsOf(words: readonly string[]): number[] {
  return words.map((word) => {
    const port = Number(word);
    if (!/^[1-9][0-9]{0,4}$/.test(word) || port > 65535) {
      throw new Error(`e2e/visual-relay.ts: ${JSON.stringify(word)} is not a port`);
    }
    return port;
  });
}

async function main(argv: readonly string[]): Promise<void> {
  const [mode, directory, ...rest] = argv;
  const usage =
    "usage: visual-relay.ts host <directory> <port>... | " +
    "visual-relay.ts container <directory> <port>... -- <command>...";
  if (directory === undefined) throw new Error(usage);
  if (mode === "host") {
    const servers = await relayPortsToSockets(directory, portsOf(rest));
    console.log(readyLine);
    const stop = () => {
      for (const server of servers) server.close();
      process.exit(0);
    };
    process.once("SIGINT", stop);
    process.once("SIGTERM", stop);
    return;
  }
  const split = rest.indexOf("--");
  const command = split === -1 ? [] : rest.slice(split + 1);
  if (mode !== "container" || command.length === 0) throw new Error(usage);
  const ports = portsOf(rest.slice(0, split));
  await relaySocketsToPorts(
    directory,
    ports.map((port) => ({ listenPort: port, socketPort: port }))
  );
  const [program, ...args] = command;
  const child = spawn(program ?? "", args, { stdio: "inherit" });
  for (const signal of ["SIGINT", "SIGTERM"] as const) {
    process.on(signal, () => child.kill(signal));
  }
  // The status a shell gives a command a signal ended, so `e2e/visual-run.sh` reads the run's
  // own outcome through this relay.
  child.on("exit", (code, signal) => {
    process.exit(code ?? 128 + (signal === null ? 0 : constants.signals[signal]));
  });
}

if (import.meta.main) {
  main(process.argv.slice(2)).catch((error: unknown) => {
    console.error(error instanceof Error ? error.message : String(error));
    process.exit(2);
  });
}
