import type { Socket } from "bun";

/**
 * What a fake NATS server does with one connection:
 * - `refuse`: sends INFO, then nats-server's authentication timeout (`-ERR 'Authentication
 *   Timeout'`), then closes, in one write, as the hub does to a client whose CONNECT came late;
 * - `refuse-authorization`: the same with `-ERR 'Authorization Violation'`;
 * - `close`: sends INFO, then closes with no `-ERR`;
 * - `silent`: accepts the connection and sends nothing;
 * - `hold`: sends INFO and nothing more, so the client's handshake waits;
 * - `serve`: sends INFO and answers each PING with a PONG, which is all a dial and a reconnect need;
 * - `late-serve`: as `serve`, with its INFO sent 600 ms after the connection opened, which a dial
 *   with a shorter timeout does not wait for.
 */
export type FakeNatsBehaviour =
  | "refuse"
  | "refuse-authorization"
  | "close"
  | "silent"
  | "hold"
  | "serve"
  | "late-serve";

/** How long `late-serve` waits before its INFO, unless `startFakeNats` is given another delay. */
export const LATE_INFO_MS = 600;

export interface FakeNatsServer {
  readonly url: string;
  /** How many connections reached the server so far. */
  connections(): number;
  /** Closes every connection that is open, from the server's side. */
  dropAll(): void;
  stop(): void;
}

interface ConnectionState {
  behaviour: FakeNatsBehaviour;
  buffered: string;
}

/**
 * A local TCP server that speaks only as much NATS as a dial needs. `behaviour(n)` decides what
 * the n-th connection (from 0) meets; `lateInfoMs` is how long `late-serve` waits before its INFO.
 */
export function startFakeNats(
  behaviour: (n: number) => FakeNatsBehaviour,
  lateInfoMs: number = LATE_INFO_MS
): FakeNatsServer {
  let count = 0;
  const open = new Set<Socket<ConnectionState>>();
  const info = (port: number) =>
    `INFO ${JSON.stringify({ server_id: "FAKE", server_name: "fake", version: "2.10.0", proto: 1, go: "go1", host: "127.0.0.1", port, headers: true, max_payload: 1048576 })}\r\n`;
  const server = Bun.listen<ConnectionState>({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        const chosen = behaviour(count);
        count += 1;
        socket.data = { behaviour: chosen, buffered: "" };
        open.add(socket);
        const hello = info(server.port);
        switch (chosen) {
          case "refuse":
            socket.write(`${hello}-ERR 'Authentication Timeout'\r\n`);
            socket.end();
            break;
          case "refuse-authorization":
            socket.write(`${hello}-ERR 'Authorization Violation'\r\n`);
            socket.end();
            break;
          case "close":
            socket.write(hello);
            socket.end();
            break;
          case "silent":
            break;
          case "hold":
          case "serve":
            socket.write(hello);
            break;
          case "late-serve":
            setTimeout(() => {
              if (open.has(socket)) socket.write(hello);
            }, lateInfoMs);
            break;
        }
      },
      data(socket, data) {
        if (socket.data.behaviour !== "serve" && socket.data.behaviour !== "late-serve") return;
        socket.data.buffered += data.toString();
        for (
          let end = socket.data.buffered.indexOf("\r\n");
          end !== -1;
          end = socket.data.buffered.indexOf("\r\n")
        ) {
          const line = socket.data.buffered.slice(0, end);
          socket.data.buffered = socket.data.buffered.slice(end + 2);
          if (line === "PING") socket.write("PONG\r\n");
        }
      },
      close(socket) {
        open.delete(socket);
      },
    },
  });
  return {
    url: `nats://127.0.0.1:${server.port}`,
    connections: () => count,
    dropAll: () => {
      for (const socket of open) socket.end();
    },
    stop: () => {
      for (const socket of open) socket.end();
      server.stop(true);
    },
  };
}
