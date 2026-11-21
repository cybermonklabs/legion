// packages/envoy-client/src/__tests__/nats-redial.test.ts
//
// How a session that presents its broker token redials a hub that refuses it. The redial handler
// (`brokerRedialDelays`) on an injected clock: at once when an outage starts, then after waits that
// double from 2-4 s to 10-20 s, each read from a draw of its own; a token it has yet to present
// within a second, mid-wait too, a renewal's at most once in each wait until a connection is
// admitted, a switch's whatever the wait. Then,
// on real nats.js against a hub that refuses its sessions and then admits them, what each client
// did, read from the hub's record and the clients' own events, never from a duration. And, on real
// nats.js, a first dial the hub refuses (`connectNats`): the token it drops at the second refusal,
// once for the session across passes.

import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { type ConnectionOptions, connect, Events, type NatsConnection } from "nats";
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
  noteNatsAdmission,
} from "../nats-credential";
import { natsBrokerStandIns } from "./broker-stand-ins";
import { type AuthHub, startAuthHub } from "./fake-nats-server";

// The broker's stand-in mints random tokens, and the pod's projected token is random too
// (`fixtureSecret`); its file's cleanup forgets every broker client and stops the broker.
const { stubBroker, podEnvironment } = natsBrokerStandIns("nats-redial");

/** A clock, a draw and the token in hand, each set by hand. */
function handled(draw: () => number) {
  let at = 1_000_000;
  let generation = 0;
  let session = "ses_a";
  let presented = true;
  const credential: RedialCredential = {
    needsReconnect: () => !presented,
    tokenGeneration: () => generation,
    tokenSession: () => session,
  };
  const handler = brokerRedialDelays(credential, draw, () => at);
  return {
    /** Moves the clock to `ms` past its start and asks the handler, as nats.js's loop does. */
    ask(ms: number) {
      at = 1_000_000 + ms;
      return handler();
    },
    /** Holds a token of `next` generation, traded for `forSession` (`ses_a` unless named), which
     *  the connection has not presented; given none, the one in hand was. */
    hold(next: number | undefined, forSession = "ses_a") {
      if (next === undefined) {
        presented = true;
        return;
      }
      generation = next;
      session = forSession;
      presented = false;
    },
    /** A connection presenting the token is admitted (`noteNatsAdmission`): by the keeper at a
     *  reconnect, or by `connectNats` at the dial `dialedMs` in. */
    admit(dialedMs?: number) {
      noteNatsAdmission(credential, dialedMs === undefined ? undefined : 1_000_000 + dialedMs);
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
      // The loop is asked again within a second, whatever is due.
      expect(lastDial + wait).toBeLessThanOrEqual(at + BROKER_REDIAL_POLL_MS);
      at = lastDial + wait;
    }
  }
  return dials;
}

const gaps = (dials: number[]) => dials.slice(1).map((at, index) => at - (dials[index] ?? 0));

/** A draw that answers `draws` in turn, one for each call. */
function scripted(draws: readonly number[]): () => number {
  let next = 0;
  return () => {
    const draw = draws[next];
    next += 1;
    if (draw === undefined) throw new Error(`draw ${next} was not scripted`);
    return draw;
  };
}

describe("brokerRedialDelays", () => {
  test("the waits double from 4 s to a 20 s cap, and stay there", () => {
    expect(BROKER_REDIAL_FIRST_MS).toBe(4_000);
    expect(BROKER_REDIAL_CAP_MS).toBe(20_000);
    expect([40, 2_000].map(brokerRedialWaitBounds)).toEqual([
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
    expect(gaps(plannedDials(() => 1 - 1e-12, 6))).toEqual([4_000, 8_000, 16_000, 20_000, 20_000]);
  });

  test("each wait reads a draw of its own, placed across its own range", () => {
    // 2 s + 0 of 2 s, 4 s + half of 4 s, 8 s + a quarter of 8 s, 10 s + three quarters of 10 s,
    // and 10 s + all of 10 s; the sixth dial draws its next wait too.
    const draws = scripted([0, 0.5, 0.25, 0.75, 1 - 1e-12, 0]);
    expect(gaps(plannedDials(draws, 6))).toEqual([2_000, 6_000, 10_000, 17_500, 20_000]);
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

  test("a renewal goes out at the next ask, within a second mid-wait, once per token and once in each wait, and moves no planned redial", () => {
    const clock = handled(() => 0);
    for (const at of [0, 30, 2_000, 2_030, 6_000]) clock.ask(at);
    // Refused, with the dial due at 14,000 (8 s after the one at 6,000): the loop is asked again a
    // second on, not at 14,000.
    expect(clock.ask(6_030)).toBe(1_030);
    // Meanwhile the keeper's renewal lands a token the connection has not presented: the next ask
    // dials it, not held to the wait.
    clock.hold(1);
    expect(clock.ask(7_030)).toBe(0);
    // That dial did not get as far as presenting it (the hub was away): the same token is no
    // reason to dial again before the planned redial at 14,000.
    expect(clock.ask(7_060)).toBe(1_030);
    // A second renewal in the same wait waits for the planned redial too.
    clock.hold(2);
    expect(clock.ask(8_060)).toBe(2_030);
    expect(clock.ask(13_000)).toBe(6_970);
    // The planned redial is where it was, and the wait after it is the fourth, 10 s: no token
    // started the waits over.
    expect(clock.ask(13_950)).toBe(0);
    expect(clock.ask(23_000)).toBe(10_000);
    // In the next wait, the next renewal is dialed at once again.
    clock.hold(3);
    expect(clock.ask(23_030)).toBe(0);
    // Presented, it is no reason at all.
    clock.hold(undefined);
    expect(clock.ask(23_060)).toBe(920);
  });

  test("once a renewal's dial is admitted, the next renewal in the same wait goes out at once too; one refused still holds the next to the planned redial", () => {
    const clock = handled(() => 0);
    for (const at of [0, 30, 2_000, 2_030, 6_000]) clock.ask(at);
    // A renewal's token, dialed at once and admitted: the connection is back (the keeper notes it).
    clock.hold(1);
    expect(clock.ask(6_030)).toBe(0);
    clock.hold(undefined);
    clock.admit();
    // The next renewal in the same wait: the reconnect that presents it asks, and the token goes
    // out at once.
    clock.hold(2);
    expect(clock.ask(7_000)).toBe(0);
    // That dial is refused (the hub refuses again): the next renewal in the wait waits for the
    // planned redial, which is where it was.
    clock.hold(3);
    expect(clock.ask(7_030)).toBe(1_030);
    expect(clock.ask(13_950)).toBe(0);
  });

  test("a session switch goes out at the next ask though a renewal went out in the same wait and was refused, and so does a second switch", () => {
    const clock = handled(() => 0);
    for (const at of [0, 30, 2_000, 2_030, 6_000]) clock.ask(at);
    // A renewal, dialed at once and refused: the wait to the redial due at 14,000 has had its one
    // renewal.
    clock.hold(1);
    expect(clock.ask(6_030)).toBe(0);
    expect(clock.ask(6_060)).toBe(1_030);
    // A switch in the same wait: another session's token goes out at the next ask.
    clock.hold(2, "ses_b");
    expect(clock.ask(7_060)).toBe(0);
    // Refused too, and then a second switch: another session again, at once again.
    clock.hold(3, "ses_c");
    expect(clock.ask(7_090)).toBe(0);
    // A renewal of that session in the same wait waits for the planned redial, which is where it
    // was.
    clock.hold(4, "ses_c");
    expect(clock.ask(7_120)).toBe(1_030);
    expect(clock.ask(13_950)).toBe(0);
  });

  test("a connection a dial of connectNats opened times the next waits from that dial", () => {
    const clock = handled(() => 0);
    // The first dial: the next is due 2 s on.
    expect(clock.ask(0)).toBe(0);
    // connectNats dials again a second on: a new nats.js connection, which dials whatever the
    // answer, and that dial is admitted (`noteNatsAdmission`, with the dial's time).
    expect(clock.ask(1_000)).toBe(2_000);
    clock.admit(1_000);
    // Lost half a second later: the answer counts from that dial, as nats.js does from its own
    // last dial, so the loop wakes when the redial is due, 2 s after the first dial, not 1 s late.
    expect(clock.ask(1_500)).toBe(1_000);
    expect(clock.ask(2_000)).toBe(0);
  });

  test("tokens renewed every half second add at most one dial to each wait, and the waits still reach the cap", () => {
    // A short-lived token's renewals come faster than the waits: each one a token the connection
    // has not presented. On one server that refuses each dial 30 ms after it starts, for 120 s.
    const clock = handled(() => 0);
    const dials: number[] = [];
    let at = 0;
    let lastDial = 0;
    while (at < 120_000) {
      clock.hold(Math.floor(at / 500) + 1);
      const wait = clock.ask(at);
      if (wait === 0) {
        dials.push(at);
        lastDial = at;
        at += 30;
      } else at = lastDial + wait;
    }
    // The planned redials at the lowest draws are at 0, 2, 6 and 14 s, then every 10 s to 114 s:
    // 14 of them, and one renewal's dial at most in each wait.
    expect(dials.length).toBeLessThanOrEqual(2 * 14);
    // At the cap, in the last 30 s: three planned redials and three more at most.
    expect(dials.filter((one) => one >= 90_000).length).toBeLessThanOrEqual(6);
  });
});

/** A broker credential that is never traded and never holds a token the connection has not
 *  presented: this row reads only its options. */
const untraded = {
  kind: "broker",
  broker: {
    authenticator: () => undefined,
    needsReconnect: () => false,
    tokenGeneration: () => 0,
    tokenSession: () => "ses_a",
  } as unknown as BrokerNatsCredential,
} as const;

describe("natsConnectionOptions' redials", () => {
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
});

/** `options` without `key`. */
function without(options: ConnectionOptions, key: keyof ConnectionOptions): ConnectionOptions {
  const copy = { ...options };
  delete copy[key];
  return copy;
}

/** One client of the outage below: its connection and credential, the session name that
 *  credential reads at each trade, when nats.js reported it back, how its connection closed, and
 *  when its handler answered 0. */
interface Client {
  readonly name: string;
  readonly connection: NatsConnection;
  readonly credential: BrokerNatsCredential;
  readonly session: { name: string };
  readonly reconnected: number[];
  readonly closed: Promise<{ at: number; error: unknown }>;
  readonly dialsAnswered: number[];
}

/** One session switch: the token the client's CONNECTs presented before it (by the hub's count),
 *  when the switch began and ended, and how it ended. */
interface Switched {
  readonly before: number;
  readonly startedAt: number;
  readonly endedAt: number;
  readonly error: unknown;
}

/** What the outage below left for its rows to read. */
interface Outage {
  readonly hub: AuthHub;
  readonly dropped: number;
  readonly clients: Readonly<Record<string, Client>>;
  readonly switches: Readonly<Record<string, Switched>>;
}

/** The hub's answers to `client`'s CONNECTs, from the drop on. */
function answersTo(outage: Outage, client: Client) {
  return outage.hub.answered.filter((one) => one.name === client.name && one.at >= outage.dropped);
}

/** The hub's answers, from the switch's start on, to `client`'s CONNECTs that presented another
 *  token than `switched.before`: the switch's. */
function switchedDials(outage: Outage, client: Client, switched: Switched) {
  return outage.hub.answered.filter(
    (one) =>
      one.name === client.name &&
      one.at >= switched.startedAt &&
      one.token !== 0 &&
      one.token !== switched.before
  );
}

/** What the outage below opened, each held as soon as it opens, so its teardown closes it however
 *  far the setup got. */
interface OutageFixture {
  hub: AuthHub | undefined;
  readonly connections: NatsConnection[];
}

/**
 * The outage every row of the describe below reads, run once: nine clients on one hub, each
 * presenting its own broker token, are refused for 13 s and then admitted, and six of them switch
 * sessions. Real time, on purpose: what is under test is nats.js's own redial loop, which sleeps
 * on the platform's timers between the handler's answers. Every draw is fixed. The handler's
 * clock is moved past the outage gap at the drop, so the drop starts an outage as one does after a
 * connection that lived a while, without the run waiting a minute. The hub and each connection go
 * into `fixture` as each opens, for the teardown, which owns them.
 */
async function runOutage(fixture: OutageFixture): Promise<Outage> {
  const hub = startAuthHub();
  fixture.hub = hub;
  const broker = stubBroker(600);
  const env = podEnvironment(broker.url);
  let shift = 0;
  const clock = () => Date.now() + shift;
  const clients: Record<string, Client> = {};
  const dial = async (
    name: string,
    shape: (
      options: ConnectionOptions,
      credential: BrokerNatsCredential,
      counted: (handler: () => number) => () => number
    ) => ConnectionOptions
  ): Promise<Client> => {
    // The name is read at each trade, as a host's session switch changes it.
    const session = { name: `ses_${name}` };
    const credential = brokerNatsCredential(env, () => session.name);
    await credential.ready();
    const dialsAnswered: number[] = [];
    const counted = (handler: () => number) => () => {
      const wait = handler();
      if (wait === 0) dialsAnswered.push(Date.now());
      return wait;
    };
    const options = natsConnectionOptions({
      servers: [hub.url],
      name,
      credential: { kind: "broker", broker: credential },
    });
    const connection = await connect(shape(options, credential, counted));
    fixture.connections.push(connection);
    const reconnected: number[] = [];
    void (async () => {
      for await (const status of connection.status()) {
        if (status.type !== Events.Reconnect) continue;
        reconnected.push(Date.now());
        // As the keeper does at each reconnect; the keeper itself is not run, since its drop of a
        // token refused twice would move each client's token while the hub refuses.
        noteNatsAdmission(credential);
      }
    })();
    const closed = connection.closed().then((error) => ({ at: Date.now(), error }));
    const client = { name, connection, credential, session, reconnected, closed, dialsAnswered };
    clients[name] = client;
    return client;
  };
  /** The host switches `client` to a session of its own, and `followNatsCredential` moves the
   *  connection to that session's token. */
  const follow = async (client: Client): Promise<Switched> => {
    const startedAt = Date.now();
    const before = hub.answered.filter((one) => one.name === client.name).at(-1)?.token ?? 0;
    client.session.name = `${client.session.name}_next`;
    const error = await followNatsCredential(client.connection, client.credential).then(
      () => undefined,
      (failed: unknown) => failed
    );
    return { before, startedAt, endedAt: Date.now(), error };
  };
  const backoff = await dial("redial-backoff", (options, credential, counted) => ({
    ...options,
    reconnectDelayHandler: counted(brokerRedialDelays(credential, () => 0, clock)),
  }));
  // Control: the same options with nats.js's flat 2 s, as before this change.
  await dial("redial-flat", (options) => without(options, "reconnectDelayHandler"));
  // Control: the same options without ignoreAuthErrorAbort, as main dialed every session.
  await dial("redial-abort", (options, credential) => ({
    ...without(options, "ignoreAuthErrorAbort"),
    reconnectDelayHandler: brokerRedialDelays(credential, () => 0, clock),
  }));
  // A switch once the session is back, and its control, a handler that never sees the switch's
  // token: at the lowest waits both redial at once, at 2 s, 6 s and 14 s, the last admitted, and
  // the next is due 10 s after it, past the 8 s the switch gives the connection to come back.
  const switching = await dial("redial-switch", (options, credential) => ({
    ...options,
    reconnectDelayHandler: brokerRedialDelays(credential, () => 0, clock),
  }));
  const blind = { needsReconnect: () => false, tokenGeneration: () => 0, tokenSession: () => "" };
  const switchingBlind = await dial("redial-switch-blind", (options) => ({
    ...options,
    reconnectDelayHandler: brokerRedialDelays(blind, () => 0, clock),
  }));
  // Switches while the session waits: at the highest waits each redials at once, at 4 s and at
  // 12 s, and the next is due at 28 s. R switches 200 ms after the hub admits again, and once
  // that switch is done, again; Q switches for the first time with R's second. R's control does
  // as R does with a handler that neither learns its connection was admitted nor tells a switch
  // from a renewal, as at 34dc6bbefd; the other control switches with R's first, its loop with no
  // poll sleeping out each wait whole.
  const highest = () => 1 - 1e-12;
  const r = await dial("redial-mid-wait", (options, credential) => ({
    ...options,
    reconnectDelayHandler: brokerRedialDelays(credential, highest, clock),
  }));
  const q = await dial("redial-mid-wait-once", (options, credential) => ({
    ...options,
    reconnectDelayHandler: brokerRedialDelays(credential, highest, clock),
  }));
  const rUnseen = await dial("redial-mid-wait-unseen", (options, credential) => ({
    ...options,
    // Another object than the credential the admissions are noted for, and one session for every
    // token.
    reconnectDelayHandler: brokerRedialDelays(
      {
        needsReconnect: () => credential.needsReconnect(),
        tokenGeneration: () => credential.tokenGeneration(),
        tokenSession: () => "",
      },
      highest,
      clock
    ),
  }));
  const unpolled = await dial("redial-mid-wait-unpolled", (options, credential) => ({
    ...options,
    reconnectDelayHandler: brokerRedialDelays(credential, highest, clock, Number.POSITIVE_INFINITY),
  }));

  shift = BROKER_REDIAL_OUTAGE_GAP_MS + 1;
  const dropped = Date.now();
  hub.refuseAndDrop();
  // 13 s: the lowest schedule's first three redials (at once, 2 s, 6 s) and the highest's (at
  // once, 4 s, 12 s) all come while the hub refuses.
  await Bun.sleep(13_000);
  const admitted = Date.now();
  hub.admit();
  await Bun.sleep(200);
  const rFirst = follow(r);
  const rUnseenFirst = follow(rUnseen);
  const unpolledSwitch = follow(unpolled);
  const rSecondAndQ = rFirst.then(() => Promise.all([follow(r), follow(q)]));
  const rUnseenSecond = rUnseenFirst.then(() => follow(rUnseen));
  const deadline = admitted + BROKER_REDIAL_CAP_MS + 2_000;
  const back = (client: Client) => client.reconnected.length > 0;
  while (!(back(switching) && back(switchingBlind)) && Date.now() < deadline) {
    await Bun.sleep(50);
  }
  const [switched, switchedBlind] = await Promise.all([follow(switching), follow(switchingBlind)]);
  const [rSecond, qFirst] = await rSecondAndQ;
  const unseenSecond = await rUnseenSecond;
  while (!back(backoff) && Date.now() < deadline) await Bun.sleep(100);
  return {
    hub,
    dropped,
    clients,
    switches: {
      switched,
      switchedBlind,
      rFirst: await rFirst,
      rSecond,
      qFirst,
      rUnseenFirst: await rUnseenFirst,
      rUnseenSecond: unseenSecond,
      unpolled: await unpolledSwitch,
    },
  };
}

describe("broker-token sessions a hub refuses for 13 s and then admits, on real nats.js", () => {
  const fixture: OutageFixture = { hub: undefined, connections: [] };
  let outage: Outage | undefined;
  beforeAll(async () => {
    outage = await runOutage(fixture);
  }, 90_000);
  afterAll(async () => {
    for (const connection of fixture.connections) {
      await connection.close().catch(() => {});
    }
    fixture.hub?.stop();
  });
  /** The outage, which `beforeAll` ran. */
  const ran = (): Outage => {
    if (outage === undefined) throw new Error("the outage did not run");
    return outage;
  };
  const client = (name: string): Client => {
    const found = ran().clients[name];
    if (found === undefined) throw new Error(`no client ${name}`);
    return found;
  };
  const switched = (name: string): Switched => {
    const found = ran().switches[name];
    if (found === undefined) throw new Error(`no switch ${name}`);
    return found;
  };
  const trace = () =>
    JSON.stringify(ran().hub.answered.map((one) => ({ ...one, at: one.at - ran().dropped })));

  test("the backoff dials when its handler answers 0 and at no other time, is refused twice and more, and comes back by itself", async () => {
    const backoff = client("redial-backoff");
    const answers = answersTo(ran(), backoff);
    expect(backoff.dialsAnswered.filter((at) => at >= ran().dropped).length, trace()).toBe(
      answers.length
    );
    expect(answers.filter((one) => !one.admitted).length, trace()).toBeGreaterThanOrEqual(2);
    expect(answers.at(-1)?.admitted, trace()).toBe(true);
    expect(backoff.reconnected.length, trace()).toBeGreaterThanOrEqual(1);
    expect(backoff.connection.isClosed()).toBe(false);
    expect(await backoff.connection.rtt()).toBeGreaterThanOrEqual(0);
  });

  test("without ignoreAuthErrorAbort the connection closes at the second refusal and stays closed", async () => {
    const abort = client("redial-abort");
    expect(answersTo(ran(), abort).filter((one) => !one.admitted).length, trace()).toBe(2);
    expect(abort.connection.isClosed()).toBe(true);
    expect(String((await abort.closed).error)).toContain("Authorization Violation");
    expect(abort.reconnected).toEqual([]);
  });

  test("the flat 2 s asks the refusing hub more often than the backoff", () => {
    const refused = (name: string) =>
      answersTo(ran(), client(name)).filter((one) => !one.admitted).length;
    expect(refused("redial-flat"), trace()).toBeGreaterThan(refused("redial-backoff"));
    expect(client("redial-flat").connection.isClosed()).toBe(false);
  });

  test("a switch once the session is back presents its token before the switch ends; a handler that never sees it does not", () => {
    const done = switched("switched");
    expect(done.error, trace()).toBeUndefined();
    expect(
      switchedDials(ran(), client("redial-switch"), done).some(
        (one) => one.admitted && one.at <= done.endedAt
      ),
      trace()
    ).toBe(true);
    const blind = switched("switchedBlind");
    expect(String(blind.error), trace()).toContain(
      "the NATS connection did not come back as the new session within"
    );
    expect(
      switchedDials(ran(), client("redial-switch-blind"), blind).filter(
        (one) => one.at <= blind.endedAt
      ),
      trace()
    ).toEqual([]);
  });

  test("a switch while the session waits presents its token before the switch ends; a loop with no poll does not", () => {
    const done = switched("rFirst");
    expect(done.error, trace()).toBeUndefined();
    expect(
      switchedDials(ran(), client("redial-mid-wait"), done).some(
        (one) => one.admitted && one.at <= done.endedAt
      ),
      trace()
    ).toBe(true);
    const unpolled = switched("unpolled");
    expect(String(unpolled.error), trace()).toContain(
      "the NATS connection did not come back as the new session within"
    );
    expect(
      switchedDials(ran(), client("redial-mid-wait-unpolled"), unpolled).filter(
        (one) => one.at <= unpolled.endedAt
      ),
      trace()
    ).toEqual([]);
  });

  test("a second switch in the wait its first went out in presents its token before it ends; a handler as at 34dc6bbefd does not", () => {
    // R's second switch follows a dial the first made in the same wait, which was admitted; Q's
    // first, made with it, follows none.
    const second = switched("rSecond");
    expect(second.error, trace()).toBeUndefined();
    expect(
      switchedDials(ran(), client("redial-mid-wait"), second).some(
        (one) => one.admitted && one.at <= second.endedAt
      ),
      trace()
    ).toBe(true);
    expect(switched("qFirst").error, trace()).toBeUndefined();
    // The control's first switch goes out as R's did; its second waits for the planned redial.
    expect(switched("rUnseenFirst").error, trace()).toBeUndefined();
    const unseen = switched("rUnseenSecond");
    expect(String(unseen.error), trace()).toContain(
      "the NATS connection did not come back as the new session within"
    );
    expect(
      switchedDials(ran(), client("redial-mid-wait-unseen"), unseen).filter(
        (one) => one.at <= unseen.endedAt
      ),
      trace()
    ).toEqual([]);
  });
});

describe("a broker-token session's first dials refused by its hub", () => {
  test("drop the token at its second refusal and present the next one, once for the session across passes", async () => {
    const hub = startAuthHub();
    hub.refuseAndDrop();
    const broker = stubBroker(600);
    const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_first_dial");
    await credential.ready();
    const presenting = { kind: "broker", broker: credential } as const;
    /** One pass's `connectNats`, on the channel server's plan (no budget), ended by its caller once
     *  `dials` dials began. Its waits are recorded, not slept: the next dial waits only for a live
     *  token, which the trade a drop started brings. */
    const pass = async (dials: number) => {
      let began = 0;
      const delays: number[] = [];
      const error = await connectNats(
        (options) => {
          began += 1;
          return connect(options);
        },
        natsConnectionOptions({ servers: [hub.url], name: "first-dial", credential: presenting }),
        { ...NATS_DIAL_PLAN, budgetMs: Number.POSITIVE_INFINITY },
        {
          credential: presenting,
          random: () => 0,
          delay: () => credential.ready(),
          stopped: () => began >= dials,
          onRetry: (_error, delayMs) => delays.push(delayMs),
        }
      ).then(
        () => undefined,
        (failed: unknown) => failed
      );
      return { code: (error as { code?: unknown } | undefined)?.code, delays };
    };
    try {
      const first = await pass(5);
      const second = await pass(3);
      const trace = JSON.stringify(hub.answered);
      expect(first, trace).toEqual({
        code: "AUTHORIZATION_VIOLATION",
        delays: [2_000, 4_000, 8_000, 10_000],
      });
      expect(second, trace).toEqual({ code: "AUTHORIZATION_VIOLATION", delays: [2_000, 4_000] });
      // The first token is refused twice, dropped and traded again at once, so the third dial
      // presents the next; that one is refused twice in the first pass and three times in the
      // second, and kept: one drop for the session until a dial is admitted.
      expect(
        hub.answered.map((one) => one.token),
        trace
      ).toEqual([1, 1, 2, 2, 2, 2, 2, 2]);
      expect(broker.state.minted.length, trace).toBe(2);
    } finally {
      hub.stop();
    }
  }, 30_000);
});
