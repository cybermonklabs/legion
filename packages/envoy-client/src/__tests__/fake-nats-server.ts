import { randomBytes } from "node:crypto";
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
 * - `gated-serve`: as `serve`, with its INFO sent only once the row calls `answer()`, so the row
 *   orders the server's answer after whatever it reads first.
 */
export type FakeNatsBehaviour =
  | "refuse"
  | "refuse-authorization"
  | "close"
  | "silent"
  | "hold"
  | "serve"
  | "gated-serve";

export interface FakeNatsServer {
  readonly url: string;
  /** How many connections reached the server so far. */
  connections(): number;
  /** Settles once `count` connections have reached the server, in the turn the last one opened,
   *  so a row that awaits it acts before any timer runs. */
  opened(count: number): Promise<void>;
  /** Sends its INFO to each `gated-serve` connection open and not answered yet. */
  answer(): void;
  /** Closes every connection that is open, from the server's side. */
  dropAll(): void;
  stop(): void;
}

interface ConnectionState {
  behaviour: FakeNatsBehaviour;
  buffered: string;
}

/** The INFO line every fake server here sends, with `extra` fields beside the common ones. */
function infoLine(port: number, extra: Record<string, unknown> = {}): string {
  return `INFO ${JSON.stringify({ server_id: "FAKE", server_name: "fake", version: "2.10.0", proto: 1, go: "go1", host: "127.0.0.1", port, headers: true, max_payload: 1048576, ...extra })}\r\n`;
}

/**
 * A local TCP server that speaks only as much NATS as a dial needs. `behaviour(n)` decides what
 * the n-th connection (from 0) meets.
 */
export function startFakeNats(behaviour: (n: number) => FakeNatsBehaviour): FakeNatsServer {
  let count = 0;
  const open = new Set<Socket<ConnectionState>>();
  const gated = new Set<Socket<ConnectionState>>();
  const waiting: { readonly count: number; readonly reached: () => void }[] = [];
  const server = Bun.listen<ConnectionState>({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        const chosen = behaviour(count);
        count += 1;
        socket.data = { behaviour: chosen, buffered: "" };
        open.add(socket);
        const hello = infoLine(server.port);
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
          case "gated-serve":
            gated.add(socket);
            break;
        }
        for (const waiter of waiting.filter((one) => one.count <= count)) {
          waiting.splice(waiting.indexOf(waiter), 1);
          waiter.reached();
        }
      },
      data(socket, data) {
        if (socket.data.behaviour !== "serve" && socket.data.behaviour !== "gated-serve") return;
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
        gated.delete(socket);
      },
    },
  });
  return {
    url: `nats://127.0.0.1:${server.port}`,
    connections: () => count,
    opened: (wanted) => {
      if (count >= wanted) return Promise.resolve();
      const { promise, resolve } = Promise.withResolvers<void>();
      waiting.push({ count: wanted, reached: resolve });
      return promise;
    },
    answer: () => {
      for (const socket of gated) socket.write(infoLine(server.port));
      gated.clear();
    },
    dropAll: () => {
      for (const socket of open) socket.end();
    },
    stop: () => {
      for (const socket of open) socket.end();
      server.stop(true);
    },
  };
}

/** One CONNECT a {@link startAuthHub} answered: the client's name, when the hub answered, how,
 *  and which token the CONNECT presented, by the order the hub first saw each (1 for the first;
 *  0 for none), so a failure's trace prints no token. */
export interface AuthHubAnswer {
  readonly name: string;
  readonly at: number;
  readonly admitted: boolean;
  readonly token: number;
}

/** A {@link startAuthHub}. */
export interface AuthHub {
  readonly url: string;
  /** Every CONNECT the hub answered, in order. */
  readonly answered: readonly AuthHubAnswer[];
  /** Refuses every CONNECT from now on, and drops every connection it had admitted. */
  refuseAndDrop(): void;
  /** Admits every CONNECT from now on. */
  admit(): void;
  stop(): void;
}

interface AuthHubSocket {
  buffered: string;
  name: string;
  token: string;
  decided: boolean;
}

/**
 * A NATS server as far as a client's CONNECT on a hub whose auth service decides each session: it
 * sends INFO with a nonce of its own for each connection, as nats-server does where an auth service
 * runs, and answers the CONNECT's PING with PONG while it admits, and otherwise with nats-server's
 * own refusal, `-ERR 'Authorization Violation'`, and closes. It answers each later PING with PONG,
 * reads nothing else, and records each CONNECT it answered (`answered`).
 */
export function startAuthHub(): AuthHub {
  let admitting = true;
  const answered: AuthHubAnswer[] = [];
  const admitted = new Set<Socket<AuthHubSocket>>();
  const tokens: string[] = [];
  const decoder = new TextDecoder();
  const server = Bun.listen<AuthHubSocket>({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        socket.data = { buffered: "", name: "", token: "", decided: false };
        socket.write(infoLine(server.port, { nonce: randomBytes(12).toString("base64url") }));
      },
      data(socket, chunk) {
        const state = socket.data;
        state.buffered += decoder.decode(chunk);
        let end = state.buffered.indexOf("\r\n");
        while (end >= 0) {
          const line = state.buffered.slice(0, end);
          state.buffered = state.buffered.slice(end + 2);
          end = state.buffered.indexOf("\r\n");
          if (line.startsWith("CONNECT ")) {
            const fields: unknown = JSON.parse(line.slice("CONNECT ".length));
            if (fields !== null && typeof fields === "object" && "name" in fields) {
              state.name = String(fields.name);
            }
            if (fields !== null && typeof fields === "object" && "auth_token" in fields) {
              state.token = String(fields.auth_token);
            }
          } else if (line === "PING") {
            if (state.decided) {
              socket.write("PONG\r\n");
              continue;
            }
            state.decided = true;
            if (state.token !== "" && !tokens.includes(state.token)) tokens.push(state.token);
            answered.push({
              name: state.name,
              at: Date.now(),
              admitted: admitting,
              token: tokens.indexOf(state.token) + 1,
            });
            if (admitting) {
              admitted.add(socket);
              socket.write("PONG\r\n");
            } else {
              socket.write("-ERR 'Authorization Violation'\r\n");
              socket.end();
            }
          }
        }
      },
      close(socket) {
        admitted.delete(socket);
      },
    },
  });
  return {
    url: `nats://127.0.0.1:${server.port}`,
    answered,
    refuseAndDrop() {
      admitting = false;
      for (const socket of admitted) socket.end();
    },
    admit() {
      admitting = true;
    },
    stop() {
      server.stop(true);
    },
  };
}
