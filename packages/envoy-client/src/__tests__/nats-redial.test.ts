// packages/envoy-client/src/__tests__/nats-redial.test.ts
//
// How a session that presents its broker token redials a hub that refuses it (`brokerRedialDelays`,
// `natsConnectionOptions`): at once when an outage starts, then after waits that double from 2-4 s
// to 10-20 s, each drawn anew, and never giving up; and, on real nats.js against a hub that refuses
// it twice and more, that it comes back by itself once the hub admits it again, where the options
// without `ignoreAuthErrorAbort` close at the second refusal and the flat 2 s redials several times
// as often.

import { afterEach, describe, expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import { writeFileSync } from "node:fs";
import * as path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Socket } from "bun";
import { type ConnectionOptions, connect, Events, type NatsConnection, nkeys } from "nats";
import { forgetBrokerClients } from "../broker-credential";
import type { EnvoyEnvironment } from "../envoy-settings";
import {
  BROKER_REDIAL_CAP_MS,
  BROKER_REDIAL_FIRST_MS,
  BROKER_REDIAL_OUTAGE_GAP_MS,
  brokerRedialDelays,
  brokerRedialWaitBounds,
  natsConnectionOptions,
} from "../nats-connection";
import { type BrokerNatsCredential, brokerNatsCredential } from "../nats-credential";

afterEach(forgetBrokerClients);

const tempDir = temporaryDirectories("nats-redial");

/** A clock and a draw a test sets by hand. */
function handled(draw: () => number) {
  let at = 1_000_000;
  const handler = brokerRedialDelays(draw, () => at);
  return {
    /** Moves the clock to `ms` past its start and asks the handler, as nats.js's loop does. */
    ask(ms: number) {
      at = 1_000_000 + ms;
      return handler();
    },
  };
}

/** The handler's planned dials, as nats.js's loop makes them on one server that refuses each one
 *  `refusedAfterMs` after it starts: the loop asks, dials when the answer is 0 (its last dial
 *  plus 0 has passed), and otherwise sleeps until its last dial plus the answer and asks again. */
function plannedDials(draw: () => number, count: number, refusedAfterMs = 30): number[] {
  const clock = handled(draw);
  const dials: number[] = [];
  let at = 0;
  let lastDial = 0;
  while (dials.length < count) {
    const wait = clock.ask(at);
    if (wait === 0) {
      dials.push(at);
      lastDial = at;
      at += refusedAfterMs;
    } else {
      expect(lastDial + wait).toBeGreaterThan(at);
      at = lastDial + wait;
    }
  }
  return dials;
}

const gaps = (dials: number[]) => dials.slice(1).map((at, index) => at - (dials[index] ?? 0));

describe("brokerRedialDelays", () => {
  test("the waits double from the first to the cap: 2-4 s, 4-8 s, 8-16 s, then 10-20 s", () => {
    expect(BROKER_REDIAL_FIRST_MS).toBe(4_000);
    expect(BROKER_REDIAL_CAP_MS).toBe(20_000);
    expect([1, 2, 3, 4, 5, 40, 2_000].map(brokerRedialWaitBounds)).toEqual([
      { lowestMs: 2_000, highestMs: 4_000 },
      { lowestMs: 4_000, highestMs: 8_000 },
      { lowestMs: 8_000, highestMs: 16_000 },
      { lowestMs: 10_000, highestMs: 20_000 },
      { lowestMs: 10_000, highestMs: 20_000 },
      { lowestMs: 10_000, highestMs: 20_000 },
      { lowestMs: 10_000, highestMs: 20_000 },
    ]);
  });

  test("an outage's first redial is at once, and each later one waits its lowest at the lowest draw", () => {
    const dials = plannedDials(() => 0, 7);
    expect(dials[0]).toBe(0);
    expect(gaps(dials)).toEqual([2_000, 4_000, 8_000, 10_000, 10_000, 10_000]);
  });

  test("and its highest at the highest draw", () => {
    const dials = plannedDials(() => 1 - 1e-12, 6);
    expect(gaps(dials).map(Math.round)).toEqual([4_000, 8_000, 16_000, 20_000, 20_000]);
  });

  test("each wait is drawn anew across its whole range", () => {
    // 400 outages of 6 dials each on Math.random: every gap inside its bounds, and the draws reach
    // within 5% of both ends of each range, so the jitter spreads clients dropped together.
    const seen = new Map<number, number[]>();
    for (let outage = 0; outage < 400; outage++) {
      gaps(plannedDials(Math.random, 6)).forEach((gap, index) => {
        seen.set(index + 1, [...(seen.get(index + 1) ?? []), gap]);
      });
    }
    for (const [wait, drawn] of seen) {
      const { lowestMs, highestMs } = brokerRedialWaitBounds(wait);
      const spread = highestMs - lowestMs;
      expect(Math.min(...drawn)).toBeGreaterThanOrEqual(lowestMs);
      expect(Math.max(...drawn)).toBeLessThan(highestMs);
      expect(Math.min(...drawn)).toBeLessThan(lowestMs + spread * 0.05);
      expect(Math.max(...drawn)).toBeGreaterThan(highestMs - spread * 0.05);
    }
  });

  test("an answer other than 0 sleeps nats.js's loop until the next dial is due, not past it", () => {
    const clock = handled(() => 0);
    expect(clock.ask(0)).toBe(0);
    // Refused 30 ms after the dial: the loop sleeps to its last dial (0) plus the answer.
    expect(clock.ask(30)).toBe(2_000);
    expect(clock.ask(1_999)).toBe(2_000);
    expect(clock.ask(2_000)).toBe(0);
  });

  test("a call after the outage gap starts a new outage, at once; one inside it keeps its place", () => {
    const clock = handled(() => 0);
    for (const at of [0, 30, 2_000, 2_030, 6_000]) clock.ask(at);
    // The dial at 6,000 is admitted, and nats.js asks nothing until the connection is lost: lost
    // inside the gap, the next redial is due at 6,000 + 8,000 and the one after waits 10 s.
    const inside = 6_000 + BROKER_REDIAL_OUTAGE_GAP_MS;
    expect(clock.ask(inside)).toBe(0);
    expect(clock.ask(inside + 30)).toBe(10_000);
    // Lost past the gap: a new outage, redialed at once, its first wait 2 s again.
    const past = inside + 30 + BROKER_REDIAL_OUTAGE_GAP_MS + 1;
    expect(clock.ask(past)).toBe(0);
    expect(clock.ask(past + 30)).toBe(2_000);
  });

  test("the gap outlasts the longest wait and two hung dials, so no outage ends inside one", () => {
    expect(BROKER_REDIAL_OUTAGE_GAP_MS).toBeGreaterThanOrEqual(
      Math.max(BROKER_REDIAL_CAP_MS, 2 * 20_000) + 10_000
    );
  });
});

/** A broker credential that is never traded: these rows read only its options. */
const untraded = {
  kind: "broker",
  broker: { authenticator: () => undefined } as unknown as BrokerNatsCredential,
} as const;

describe("natsConnectionOptions' redials", () => {
  test("a broker token redials on its own backoff, never giving up over a refusal", () => {
    const options = natsConnectionOptions({
      servers: ["nats://127.0.0.1:4222"],
      name: "test",
      credential: untraded,
    });
    expect(options.ignoreAuthErrorAbort).toBe(true);
    const redial = options.reconnectDelayHandler as () => number;
    expect(redial()).toBe(0);
    const first = redial();
    expect(first).toBeGreaterThanOrEqual(2_000);
    expect(first).toBeLessThan(4_000);
  });

  test("each call makes a handler of its own, which one connection's outage does not move", () => {
    const options = () =>
      natsConnectionOptions({
        servers: ["nats://127.0.0.1:4222"],
        name: "test",
        credential: untraded,
      }).reconnectDelayHandler as () => number;
    const one = options();
    const two = options();
    expect(one).not.toBe(two);
    expect(one()).toBe(0);
    expect(one()).toBeGreaterThanOrEqual(2_000);
    expect(two()).toBe(0);
  });

  test("a seed or no credential keeps the flat 2 s and nats.js's own handler (R12)", () => {
    const user = nkeys.createUser();
    const seed = { value: new TextDecoder().decode(user.getSeed()), source: "test" };
    for (const credential of [{ kind: "seed", seed } as const, { kind: "none" } as const]) {
      const options = natsConnectionOptions({
        servers: ["nats://127.0.0.1:4222"],
        name: "test",
        credential,
      });
      expect(options.reconnectDelayHandler).toBeUndefined();
      expect(options.reconnectTimeWait).toBe(2_000);
      expect(options.ignoreAuthErrorAbort).not.toBe(true);
    }
  });
});

/** One TCP connection to {@link stubHub}: its CONNECT's name, and whether it was admitted. */
interface StubSocket {
  buffered: string;
  name: string;
  decided: boolean;
}

/** One CONNECT the stub hub answered: the client's name, when it answered, and how. */
interface Answered {
  readonly name: string;
  readonly at: number;
  readonly admitted: boolean;
}

/**
 * A NATS server as far as a client's CONNECT: it sends INFO with a nonce of its own for each
 * connection, as nats-server does where an auth service runs, and answers the CONNECT's PING with
 * PONG while it admits, and otherwise with nats-server's own refusal, `-ERR 'Authorization
 * Violation'`, and closes. It answers each later PING with PONG, and reads nothing else.
 */
function stubHub() {
  let admitting = true;
  const answered: Answered[] = [];
  const admitted = new Set<Socket<StubSocket>>();
  const decoder = new TextDecoder();
  const server = Bun.listen<StubSocket>({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        socket.data = { buffered: "", name: "", decided: false };
        const info = {
          server_id: "STUBHUB",
          server_name: "stub",
          version: "2.10.0",
          proto: 1,
          headers: true,
          max_payload: 1_048_576,
          nonce: randomBytes(12).toString("base64url"),
        };
        socket.write(`INFO ${JSON.stringify(info)}\r\n`);
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
          } else if (line === "PING") {
            if (state.decided) {
              socket.write("PONG\r\n");
              continue;
            }
            state.decided = true;
            answered.push({ name: state.name, at: Date.now(), admitted: admitting });
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
    /** Refuses every CONNECT from now on, and drops every connection it had admitted. */
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

/** A secrets broker's token exchange that mints a token good for ten minutes for every trade. */
function stubBroker() {
  let minted = 0;
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () =>
      Response.json({
        access_token: `session-access-${++minted}`,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
      }),
  });
  return { url: `http://127.0.0.1:${server.port}`, stop: () => server.stop(true) };
}

/** The environment of a pod that trades its identity at `brokerUrl` for the session's token. */
function podEnvironment(brokerUrl: string): EnvoyEnvironment {
  const tokenFile = path.join(tempDir(), "token");
  writeFileSync(tokenFile, "projected.service.account.token\n");
  return {
    HOME: tempDir(),
    ENVOY_NATS_URL: "nats://nats.internal.example:4222",
    AGENT_SECRETS_URL: brokerUrl,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
  };
}

/** `options` without `key`. */
function without(options: ConnectionOptions, key: keyof ConnectionOptions): ConnectionOptions {
  const copy = { ...options };
  delete copy[key];
  return copy;
}

describe("a broker-token session refused by its hub", () => {
  test("is refused twice and more on the backoff's schedule, then comes back by itself once admitted", async () => {
    const hub = stubHub();
    const broker = stubBroker();
    const env = podEnvironment(broker.url);
    const connections: NatsConnection[] = [];
    // The handler's clock: moved past the outage gap at the drop, so the drop starts an outage
    // as one does after a connection that lived a while, without the row waiting a minute.
    let shift = 0;
    const clock = () => Date.now() + shift;
    const dial = async (name: string, shape: (options: ConnectionOptions) => ConnectionOptions) => {
      const credential = brokerNatsCredential(env, `ses_${name}`);
      await credential.ready();
      const options = natsConnectionOptions({
        servers: [hub.url],
        name,
        credential: { kind: "broker", broker: credential },
      });
      const connection = await connect(shape(options));
      connections.push(connection);
      const reconnected: number[] = [];
      void (async () => {
        for await (const status of connection.status()) {
          if (status.type === Events.Reconnect) reconnected.push(Date.now());
        }
      })();
      const closed = connection.closed().then((error) => ({ at: Date.now(), error }));
      return { connection, reconnected, closed };
    };
    try {
      // This change's options, its handler on the test's clock.
      const backoff = await dial("redial-backoff", (options) => ({
        ...options,
        reconnectDelayHandler: brokerRedialDelays(Math.random, clock),
      }));
      // Control: the same options with nats.js's flat 2 s, as before this change.
      const flat = await dial("redial-flat", (options) =>
        without(options, "reconnectDelayHandler")
      );
      // Control: the same options without ignoreAuthErrorAbort, as main dials every session.
      const abort = await dial("redial-abort", (options) => ({
        ...without(options, "ignoreAuthErrorAbort"),
        reconnectDelayHandler: brokerRedialDelays(Math.random, clock),
      }));

      shift = BROKER_REDIAL_OUTAGE_GAP_MS + 1;
      const dropped = Date.now();
      hub.refuseAndDrop();
      // Real time, not a fake clock: what is under test is nats.js's own redial loop, which sleeps
      // on the platform's timers between the handler's answers. The hub refuses for 13 s, long
      // enough for the handler's first three redials (at once, 2-4 s, 4-8 s more).
      await Bun.sleep(13_000);
      const admitted = Date.now();
      hub.admit();
      const deadline = admitted + BROKER_REDIAL_CAP_MS + 2_000;
      while (backoff.reconnected.length === 0 && Date.now() < deadline) await Bun.sleep(100);

      const refused = (name: string) =>
        hub.answered.filter((one) => one.name === name && !one.admitted && one.at >= dropped);
      const backoffRefused = refused("redial-backoff").map((one) => one.at);
      const trace = JSON.stringify(
        hub.answered.map((one) => ({ ...one, at: one.at - dropped })),
        null,
        0
      );

      // Refused at once, then 2-4 s later, then 4-8 s after that (the dial's own few
      // milliseconds aside), and nothing in between; never closed.
      expect(backoffRefused.length, trace).toBeGreaterThanOrEqual(3);
      expect(backoffRefused.length, trace).toBeLessThanOrEqual(4);
      expect((backoffRefused[0] ?? Number.POSITIVE_INFINITY) - dropped, trace).toBeLessThan(1_000);
      const [first, second] = gaps(backoffRefused);
      expect(first, trace).toBeGreaterThanOrEqual(2_000 - 250);
      expect(first, trace).toBeLessThan(4_000 + 250);
      expect(second, trace).toBeGreaterThanOrEqual(4_000 - 250);
      expect(second, trace).toBeLessThan(8_000 + 250);
      // Back by itself, on its next planned redial, inside the cap of the hub admitting again.
      // nats.js reports one `reconnect` for the redial loop and one more for each refused dial,
      // whose transport's close joins the same loop (protocol.js:352-358, 386-392, 497-507), all
      // as the loop ends; so the first one is the time it came back.
      expect(backoff.reconnected.length, trace).toBeGreaterThanOrEqual(1);
      expect((backoff.reconnected[0] ?? Number.POSITIVE_INFINITY) - admitted, trace).toBeLessThan(
        BROKER_REDIAL_CAP_MS
      );
      expect(backoff.connection.isClosed()).toBe(false);
      expect(await backoff.connection.rtt()).toBeGreaterThanOrEqual(0);

      // The flat 2 s asked the refusing hub about every 2 s over the same 13 s.
      expect(refused("redial-flat").length, trace).toBeGreaterThanOrEqual(5);
      expect(refused("redial-flat").length, trace).toBeGreaterThan(backoffRefused.length);

      // Without ignoreAuthErrorAbort nats.js gives up at the second refusal, and nothing brings
      // the connection back once the hub admits again.
      expect(refused("redial-abort").length, trace).toBe(2);
      expect(abort.connection.isClosed()).toBe(true);
      const ended = await abort.closed;
      expect(String(ended.error)).toContain("Authorization Violation");
      expect(abort.reconnected).toEqual([]);
      expect(flat.connection.isClosed()).toBe(false);
    } finally {
      for (const connection of connections) await connection.close().catch(() => {});
      hub.stop();
      broker.stop();
    }
  }, 60_000);
});
