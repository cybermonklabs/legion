// packages/envoy-client/src/__tests__/nats-redial.test.ts
//
// How a session that presents its broker token redials a hub that refuses it (`brokerRedialDelays`,
// `natsConnectionOptions`): at once when an outage starts, then after waits that double from 2-4 s
// to 10-20 s, each drawn anew, and never giving up; a token it has yet to present within a second,
// mid-wait too, at most once in each wait; and, on real nats.js against a hub that refuses it twice
// and more, that it comes back by itself once the hub admits it again, where the options without
// `ignoreAuthErrorAbort` close at the second refusal and the flat 2 s redials several times as
// often, that a session switch right after it is back, or while it waits, does not wait out the
// backoff, and that a first dial the hub refuses (`connectNats`) waits the same and drops a token
// refused twice.

import { describe, expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import type { Socket } from "bun";
import { type ConnectionOptions, connect, Events, type NatsConnection, nkeys } from "nats";
import {
  BROKER_REDIAL_CAP_MS,
  BROKER_REDIAL_FIRST_MS,
  BROKER_REDIAL_OUTAGE_GAP_MS,
  BROKER_REDIAL_POLL_MS,
  brokerRedialDelays,
  brokerRedialWaitBounds,
  connectNats,
  NATS_DIAL_PLAN,
  NATS_DIAL_TIMEOUT_MS,
  natsConnectionOptions,
  type RedialCredential,
} from "../nats-connection";
import {
  type BrokerNatsCredential,
  brokerNatsCredential,
  followNatsCredential,
} from "../nats-credential";
import { natsBrokerStandIns } from "./broker-stand-ins";

// The broker's stand-in mints random tokens, and the pod's projected token is random too
// (`fixtureSecret`); its file's cleanup forgets every broker client and stops the broker.
const { stubBroker, podEnvironment } = natsBrokerStandIns("nats-redial");

/** A token the connection has not presented: its generation, which tells it from the next, and
 *  its expiry, which does not (two tokens may lapse at the same moment). */
interface Unpresented {
  readonly generation: number;
  readonly expiry: number;
}

/** A clock, a draw and the token the connection has yet to present, each set by hand. */
function handled(draw: () => number) {
  let at = 1_000_000;
  let unpresented: Unpresented | undefined;
  // The credential's expiry too, though the handler's type leaves it out: a handler that read it
  // as the token's identity would fail the row below where two tokens lapse together.
  const credential: RedialCredential & Pick<BrokerNatsCredential, "expiresAt"> = {
    needsReconnect: () => unpresented !== undefined,
    tokenGeneration: () => unpresented?.generation ?? 0,
    expiresAt: () => new Date(unpresented?.expiry ?? 0),
  };
  const handler = brokerRedialDelays(credential, draw, () => at);
  return {
    /** Moves the clock to `ms` past its start and asks the handler, as nats.js's loop does. */
    ask(ms: number) {
      at = 1_000_000 + ms;
      return handler();
    },
    /** Holds a token the connection has not presented; or none. */
    hold(token: Unpresented | undefined) {
      unpresented = token;
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
      // The loop is asked again within a second, whatever is due (a microsecond for rounding).
      expect(lastDial + wait).toBeLessThanOrEqual(at + BROKER_REDIAL_POLL_MS + 1e-3);
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

  test("an answer other than 0 sleeps nats.js's loop at most a second, and never past the next dial due", () => {
    const clock = handled(() => 0);
    expect(clock.ask(0)).toBe(0);
    // Refused 30 ms after the dial: the loop sleeps to its last dial (0) plus the answer, a second
    // past this call while the dial due at 2,000 is further off than that.
    expect(clock.ask(30)).toBe(1_030);
    expect(clock.ask(1_030)).toBe(2_000);
    expect(clock.ask(1_900)).toBe(2_000);
    // Due a little early, so a dial nats.js would make on its own clock, which it reads after the
    // answer, is one the handler made too.
    expect(clock.ask(1_950)).toBe(0);
  });

  test("a call after the outage gap starts a new outage, at once; one inside it keeps its place", () => {
    // The gap outlasts the longest the loop leaves the handler uncalled: five addresses' hung
    // dials, with a second between two calls of a wait.
    expect(BROKER_REDIAL_OUTAGE_GAP_MS).toBe(60_000);
    expect(BROKER_REDIAL_OUTAGE_GAP_MS).toBeGreaterThan(
      5 * NATS_DIAL_TIMEOUT_MS + BROKER_REDIAL_POLL_MS
    );
    const clock = handled(() => 0);
    for (const at of [0, 30, 2_000, 2_030, 6_000]) clock.ask(at);
    // The dial at 6,000 is admitted, and nats.js asks nothing until the connection is lost: lost
    // inside the gap, the redial due at 6,000 + 8,000 is past, so it goes at once, and the one
    // after waits 10 s, which the answer names once it is due within a second.
    const inside = 6_000 + BROKER_REDIAL_OUTAGE_GAP_MS;
    expect(clock.ask(inside)).toBe(0);
    expect(clock.ask(inside + 30)).toBe(1_030);
    expect(clock.ask(inside + 9_030)).toBe(10_000);
    // Lost past the gap: a new outage, redialed at once, its first wait 2 s again.
    const past = inside + 9_030 + BROKER_REDIAL_OUTAGE_GAP_MS + 1;
    expect(clock.ask(past)).toBe(0);
    expect(clock.ask(past + 30)).toBe(1_030);
    expect(clock.ask(past + 1_030)).toBe(2_000);
  });

  test("a connection lost less than a minute after the dial that opened it waits out the rest of that dial's wait", () => {
    const clock = handled(() => 0);
    // The dial that opened the connection, admitted.
    expect(clock.ask(0)).toBe(0);
    // Lost 500 ms later: its first redial is the one due 2 s after that dial, not at once.
    expect(clock.ask(500)).toBe(1_500);
    expect(clock.ask(1_500)).toBe(2_000);
    expect(clock.ask(2_000)).toBe(0);
  });

  test("a token the connection has yet to present goes out at the next ask, within a second mid-wait, once per token and once in each wait, and moves no planned redial", () => {
    const clock = handled(() => 0);
    for (const at of [0, 30, 2_000, 2_030, 6_000]) clock.ask(at);
    // Refused, with the dial due at 14,000 (8 s after the one at 6,000): the loop is asked again a
    // second on, not at 14,000.
    expect(clock.ask(6_030)).toBe(1_030);
    // Meanwhile a renewal or a session switch lands a token the connection has not presented: the
    // next ask dials it, not held to the wait.
    clock.hold({ generation: 1, expiry: 900_000 });
    expect(clock.ask(7_030)).toBe(0);
    // That dial did not get as far as presenting it (the hub was away): the same token is no
    // reason to dial again before the planned redial at 14,000.
    expect(clock.ask(7_060)).toBe(1_030);
    // A second new token in the same wait waits for the planned redial too.
    clock.hold({ generation: 2, expiry: 900_000 });
    expect(clock.ask(8_060)).toBe(2_030);
    expect(clock.ask(13_000)).toBe(6_970);
    // The planned redial is where it was, and the wait after it is the fourth, 10 s: no new token
    // started the waits over.
    expect(clock.ask(13_950)).toBe(0);
    expect(clock.ask(23_000)).toBe(10_000);
    // In the next wait, the next new token is dialed at once again, though it lapses when the one
    // before does: a token is its generation, not its expiry.
    clock.hold({ generation: 3, expiry: 900_000 });
    expect(clock.ask(23_030)).toBe(0);
    // Presented, it is no reason at all.
    clock.hold(undefined);
    expect(clock.ask(23_060)).toBe(920);
  });

  test("tokens renewed every half second add at most one dial to each wait, and the waits still reach the cap", () => {
    // A short-lived token's renewals come faster than the waits: each one a token the connection
    // has not presented. On one server that refuses each dial 30 ms after it starts, for 120 s.
    const clock = handled(() => 0);
    const dials: number[] = [];
    let at = 0;
    let lastDial = 0;
    while (at < 120_000) {
      clock.hold({ generation: Math.floor(at / 500) + 1, expiry: at + 1_000 });
      const wait = clock.ask(at);
      if (wait === 0) {
        dials.push(at);
        lastDial = at;
        at += 30;
      } else at = lastDial + wait;
    }
    // The planned redials at the lowest draws are at 0, 2, 6 and 14 s, then every 10 s to 114 s:
    // 14 of them, and one new token's dial at most in each wait.
    expect(dials.length).toBeLessThanOrEqual(2 * 14);
    // At the cap, in the last 30 s: three planned redials and three more at most.
    expect(dials.filter((one) => one >= 90_000).length).toBeLessThanOrEqual(6);
  });
});

/** A broker credential that is never traded and never holds a token the connection has not
 *  presented: these rows read only its options. */
const untraded = {
  kind: "broker",
  broker: {
    authenticator: () => undefined,
    needsReconnect: () => false,
    tokenGeneration: () => 0,
  } as unknown as BrokerNatsCredential,
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
    // Asked again at once after the dial, nothing is due for 2 s: the loop is told to ask again
    // within a second.
    expect(first).toBeGreaterThanOrEqual(BROKER_REDIAL_POLL_MS);
    expect(first).toBeLessThan(2_000);
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
    expect(one()).toBeGreaterThanOrEqual(BROKER_REDIAL_POLL_MS);
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

/** One TCP connection to {@link stubHub}: its CONNECT's name and token, and whether it was
 *  admitted. */
interface StubSocket {
  buffered: string;
  name: string;
  token: string;
  decided: boolean;
}

/** One CONNECT the stub hub answered: the client's name, when it answered, how, and which token
 *  the CONNECT presented, by the order the hub first saw each (1 for the first; 0 for none), so a
 *  failure's trace prints no token. */
interface Answered {
  readonly name: string;
  readonly at: number;
  readonly admitted: boolean;
  readonly token: number;
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
  const tokens: string[] = [];
  const decoder = new TextDecoder();
  const server = Bun.listen<StubSocket>({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        socket.data = { buffered: "", name: "", token: "", decided: false };
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

/** `options` without `key`. */
function without(options: ConnectionOptions, key: keyof ConnectionOptions): ConnectionOptions {
  const copy = { ...options };
  delete copy[key];
  return copy;
}

/** One client of the row below: its connection, its credential and the session name that
 *  credential reads at each trade, when nats.js reported it back, and how its connection closed. */
interface Dialed {
  readonly connection: NatsConnection;
  readonly credential: BrokerNatsCredential;
  readonly session: { name: string };
  readonly reconnected: number[];
  readonly closed: Promise<{ at: number; error: unknown }>;
}

describe("a broker-token session refused by its hub", () => {
  test("is refused twice and more on the backoff's schedule, comes back by itself once admitted, and moves to a switch's token within a second, mid-wait too", async () => {
    const hub = stubHub();
    const broker = stubBroker(600);
    const env = podEnvironment(broker.url);
    const connections: NatsConnection[] = [];
    // The handler's clock: moved past the outage gap at the drop, so the drop starts an outage
    // as one does after a connection that lived a while, without the row waiting a minute.
    let shift = 0;
    const clock = () => Date.now() + shift;
    const dial = async (
      name: string,
      shape: (options: ConnectionOptions, credential: BrokerNatsCredential) => ConnectionOptions
    ): Promise<Dialed> => {
      // The name is read at each trade, as a host's session switch changes it.
      const session = { name: `ses_${name}` };
      const credential = brokerNatsCredential(env, () => session.name);
      await credential.ready();
      const options = natsConnectionOptions({
        servers: [hub.url],
        name,
        credential: { kind: "broker", broker: credential },
      });
      const connection = await connect(shape(options, credential));
      connections.push(connection);
      const reconnected: number[] = [];
      void (async () => {
        for await (const status of connection.status()) {
          if (status.type === Events.Reconnect) reconnected.push(Date.now());
        }
      })();
      const closed = connection.closed().then((error) => ({ at: Date.now(), error }));
      return { connection, credential, session, reconnected, closed };
    };
    /** How long a session switch takes, and how it ended: the host switches `client` to a session
     *  of its own, and `followNatsCredential` moves the connection to that session's token. */
    const follow = async (client: Dialed): Promise<{ ms: number; error: unknown }> => {
      const started = Date.now();
      client.session.name = `${client.session.name}_next`;
      const error = await followNatsCredential(client.connection, client.credential).then(
        () => undefined,
        (failed: unknown) => failed
      );
      return { ms: Date.now() - started, error };
    };
    try {
      // This change's options, its handler on the test's clock.
      const backoff = await dial("redial-backoff", (options, credential) => ({
        ...options,
        reconnectDelayHandler: brokerRedialDelays(credential, Math.random, clock),
      }));
      // Control: the same options with nats.js's flat 2 s, as before this change.
      const flat = await dial("redial-flat", (options) =>
        without(options, "reconnectDelayHandler")
      );
      // Control: the same options without ignoreAuthErrorAbort, as main dials every session.
      const abort = await dial("redial-abort", (options, credential) => ({
        ...without(options, "ignoreAuthErrorAbort"),
        reconnectDelayHandler: brokerRedialDelays(credential, Math.random, clock),
      }));
      // A session switch right after the hub admits again, and its control, a handler that never
      // sees the switch's token: both draw the lowest waits, so both redial at once, at 2 s, 6 s
      // and 14 s, and the next redial each has due is 10 s after the one that is admitted, past
      // the 8 s `followNatsCredential` gives the connection to come back as the new session.
      const switching = await dial("redial-switch", (options, credential) => ({
        ...options,
        reconnectDelayHandler: brokerRedialDelays(credential, () => 0, clock),
      }));
      const blind = { needsReconnect: () => false, tokenGeneration: () => 0 };
      const switchingBlind = await dial("redial-switch-blind", (options) => ({
        ...options,
        reconnectDelayHandler: brokerRedialDelays(blind, () => 0, clock),
      }));
      // A session switch 200 ms after the hub admits again, while the session sleeps toward its
      // next redial, and its control, the same handler with no poll, whose loop sleeps out each
      // wait whole: both draw the highest waits, so both redial at once, at 4 s and at 12 s, and
      // the next is due at 28 s, past the 8 s the switch gives the connection to come back.
      const midWait = await dial("redial-mid-wait", (options, credential) => ({
        ...options,
        reconnectDelayHandler: brokerRedialDelays(credential, () => 1 - 1e-12, clock),
      }));
      const midWaitUnpolled = await dial("redial-mid-wait-unpolled", (options, credential) => ({
        ...options,
        reconnectDelayHandler: brokerRedialDelays(
          credential,
          () => 1 - 1e-12,
          clock,
          Number.POSITIVE_INFINITY
        ),
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
      await Bun.sleep(200);
      const midWaitSwitches = Promise.all([follow(midWait), follow(midWaitUnpolled)]);
      const deadline = admitted + BROKER_REDIAL_CAP_MS + 2_000;
      const back = (client: { reconnected: number[] }) => client.reconnected.length > 0;
      while (!(back(switching) && back(switchingBlind)) && Date.now() < deadline) {
        await Bun.sleep(50);
      }
      const [switched, switchedBlind] = await Promise.all([
        follow(switching),
        follow(switchingBlind),
      ]);
      const [switchedMidWait, switchedMidWaitUnpolled] = await midWaitSwitches;
      while (!back(backoff) && Date.now() < deadline) await Bun.sleep(100);

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

      // A session switch right after the hub admitted it again: the switch's new token is
      // presented at once, where the handler that never sees it waits out the 10 s it has due, so
      // that switch fails at the 8 s its connection is given.
      expect(switched.error, trace).toBeUndefined();
      expect(switched.ms, trace).toBeLessThan(2_000);
      expect(String(switchedBlind.error), trace).toContain(
        "the NATS connection did not come back as the new session within"
      );
      expect(switchedBlind.ms, trace).toBeGreaterThan(5_000);

      // A session switch while the session sleeps toward its redial at 28 s: the loop asks within
      // a second, so the new token goes out then (the trade and the handshakes aside), where the
      // loop with no poll sleeps on, and that switch fails at the 8 s its connection is given.
      // The poll adds no refusal: both were refused at once, at 4 s and at 12 s.
      expect(switchedMidWait.error, trace).toBeUndefined();
      expect(switchedMidWait.ms, trace).toBeLessThan(BROKER_REDIAL_POLL_MS + 2_000);
      expect(String(switchedMidWaitUnpolled.error), trace).toContain(
        "the NATS connection did not come back as the new session within"
      );
      expect(switchedMidWaitUnpolled.ms, trace).toBeGreaterThan(5_000);
      expect(refused("redial-mid-wait").length, trace).toBe(3);
      expect(refused("redial-mid-wait-unpolled").length, trace).toBe(3);
    } finally {
      for (const connection of connections) await connection.close().catch(() => {});
      hub.stop();
    }
  }, 90_000);
});

describe("a broker-token session's first dial refused by its hub", () => {
  test("waits 2-4 s, 4-8 s, 8-16 s, then 10-20 s between dials, and drops its token at the second refusal, once", async () => {
    // Real time, as the row above: what is under test is how long `connectNats` waits between
    // dials of the real nats.js. The lowest draws hold the row to the lowest waits, 24 s in all.
    const hub = stubHub();
    hub.refuseAndDrop();
    const broker = stubBroker(600);
    const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_first_dial");
    await credential.ready();
    const presenting = { kind: "broker", broker: credential } as const;
    const name = "first-dial";
    const dialStarts: number[] = [];
    const delays: number[] = [];
    try {
      const error = await connectNats(
        (options) => {
          dialStarts.push(Date.now());
          return connect(options);
        },
        natsConnectionOptions({ servers: [hub.url], name, credential: presenting }),
        // The channel server's plan: its first dial is dialed again for the server's life.
        { ...NATS_DIAL_PLAN, budgetMs: Number.POSITIVE_INFINITY },
        {
          credential: presenting,
          random: () => 0,
          // Five dials: the fifth one's refusal ends the row, with no wait after it.
          stopped: () => dialStarts.length >= 5,
          onRetry: (_error, delayMs) => delays.push(delayMs),
        }
      ).then(
        () => undefined,
        (failed: unknown) => failed
      );
      const trace = JSON.stringify(
        hub.answered.map((one) => ({ ...one, at: one.at - (dialStarts[0] ?? 0) })),
        null,
        0
      );
      expect((error as { code?: unknown } | undefined)?.code, trace).toBe(
        "AUTHORIZATION_VIOLATION"
      );
      // Each wait is the backoff's, at its lowest, where the plan's would be 250 ms doubling to
      // 4 s; between two dials' starts there is the wait and the refused dial itself.
      expect(delays, trace).toEqual([2_000, 4_000, 8_000, 10_000]);
      const dialGaps = gaps(dialStarts);
      for (const [index, wait] of delays.entries()) {
        expect(dialGaps[index], trace).toBeGreaterThanOrEqual(wait - 5);
      }
      // The first token is refused twice, dropped and traded again at once, so the third dial
      // presents the next; that one is refused twice too and kept: one drop per session.
      expect(
        hub.answered.filter((one) => one.name === name).map((one) => one.token),
        trace
      ).toEqual([1, 1, 2, 2, 2]);
      expect(broker.state.minted.length, trace).toBe(2);
    } finally {
      hub.stop();
    }
  }, 60_000);
});
