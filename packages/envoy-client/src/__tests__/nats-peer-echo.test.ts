// A failing test for #4140 at aeb4d42f09, found by the acceptance (AcceptE2-2): nats.js prints a
// peer's bytes, and a peer that is no nats-server can make those bytes the CONNECT the session sent
// it, its broker token included. nats.js's reader loop ends with `console.log("reader closed",
// err)` (protocol.js), and a parse error's message holds the bytes it could not parse ("parse
// error [0]: <bytes>"). On the real stack, pi-envoy's stdout carried the session's broker token
// this way. Put it in packages/envoy-client/src/__tests__/ and run
// `bun test src/__tests__/nats-peer-echo.test.ts` in packages/envoy-client.
import { afterEach, describe, expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import { connect } from "nats";
import { natsConnectionOptions } from "../nats-connection";
import type { BrokerNatsCredential } from "../nats-credential";

const printed: string[] = [];
const originals = {
  log: console.log,
  error: console.error,
  warn: console.warn,
  info: console.info,
  debug: console.debug,
};
function capture(): void {
  for (const name of Object.keys(originals) as (keyof typeof originals)[]) {
    console[name] = (...args: unknown[]) => {
      printed.push(
        args
          .map((arg) => (arg instanceof Error ? `${arg.message}\n${arg.stack ?? ""}` : String(arg)))
          .join(" ")
      );
    };
  }
}
afterEach(() => {
  Object.assign(console, originals);
  printed.length = 0;
});

describe("a peer that is no nats-server", () => {
  test("puts nothing the session sent it on the console when it answers the CONNECT with garbage", async () => {
    const token = `TOKENCANARY${randomBytes(12).toString("hex")}`;
    const received: string[] = [];
    const peer = Bun.listen({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        open(socket) {
          socket.write(
            'INFO {"server_id":"peer","version":"2.10.0","proto":1,"max_payload":1048576,"nonce":"bm9uY2U"}\r\n'
          );
        },
        data(socket, data) {
          const connect = /CONNECT (\{.*?\})\r\n/.exec(String(data))?.[1];
          if (connect === undefined) return;
          received.push(connect);
          // The CONNECT back as a line that is no protocol op.
          socket.write(`${connect}\r\n`);
        },
      },
    });
    const broker: BrokerNatsCredential = {
      ready: async () => {},
      renew: async () => {},
      expiresAt: () => new Date(Date.now() + 3_600_000),
      needsReconnect: () => false,
      drop: () => {},
      tokenGeneration: () => 0,
      tokenSession: () => undefined,
      refused: () => undefined,
      admitted: () => {},
      admissions: () => ({ count: 0, dialedAt: undefined }),
      authenticator: () => ({ auth_token: token, sig: "the-proof" }),
    };
    capture();
    try {
      await connect({
        ...natsConnectionOptions({
          servers: [`nats://127.0.0.1:${peer.port}`],
          name: "peer-echo",
          credential: { kind: "broker", broker },
        }),
        reconnect: false,
        timeout: 3_000,
      })
        .then((connection) => connection.close())
        .catch(() => undefined);
      // The dial settles (refused, or at its 3 s timeout) after the peer's reply was parsed, and
      // nats.js prints from its reader loop at that parse, so what it printed is in by now.
    } finally {
      Object.assign(console, originals);
      peer.stop(true);
    }
    // The row can fail for the leak: the peer was sent the token.
    expect(received.join("\n")).toContain(token);
    expect(printed.join("\n")).not.toContain(token);
  }, 20_000);
});
