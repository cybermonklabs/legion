// packages/envoy-client/src/__tests__/listener-relay.test.ts
//
// A session's inbound through the listener's relay socket (`../listener-relay.ts`), against a
// stand-in that serves the socket's wire and the broker's token exchange on one origin
// (`../../test/relay-socket-stand-in.ts`): a frame the host answers `done` goes back on the frame's
// reply; each close code and each refused upgrade is opened again as the wire says, each cause
// counting its own run of waits, with a fresh trade only where the token was the cause; an upgrade
// never answered is dropped and tried again; a subject the socket does not carry, and a fifth
// subscription, close that subscription alone and are logged once; a flush is answered only by
// its own socket; a topic the edge refuses for the session's projects is dropped; a refused trade
// is told as an inbound outage is; and which settings choose the socket.
//
// The reopen rows read the real clock: what they hold is how long the client waits on it, which
// is the platform's timers against a real socket, so no fake clock stands in for it.

import { afterEach, describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import * as path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  type RelaySocketStandIn,
  type StandInFrame,
  type StandInUpgrade,
  standInDeliverSubject,
  startRelaySocketStandIn,
} from "../../test/relay-socket-stand-in";
import { consumerDelivery } from "../../test/subscribe-answer";
import { brokerCredential, forgetBrokerClients } from "../broker-credential";
import {
  createConsumerDelivery,
  inboundUnavailable,
  registerWithRelay,
} from "../consumer-delivery";
import { readEnvoyConfig } from "../envoy-config";
import { type EnvoyEnvironment, envoySettings } from "../envoy-settings";
import { connectListenerRelay, ListenerRelay } from "../listener-relay";
import { sessionSettings } from "../session-settings";
import { createEnvoyClient, expandSubscriptionTopics } from "../transport";

const tempDir = temporaryDirectories("listener-relay");
/** What each row started, stopped after it in the reverse order. */
const started: (() => unknown)[] = [];
afterEach(async () => {
  for (const stop of started.splice(0).reverse()) await stop();
});
afterEach(forgetBrokerClients);

const encoder = new TextEncoder();

function standIn(): RelaySocketStandIn {
  const server = startRelaySocketStandIn();
  started.push(() => server.stop());
  return server;
}

/** A pod's environment whose broker and listener are both `server`. */
function podEnvironment(server: RelaySocketStandIn): EnvoyEnvironment {
  const dir = tempDir();
  const tokenFile = path.join(dir, "token");
  writeFileSync(tokenFile, `${fixtureSecret("proj")}\n`);
  return {
    HOME: dir,
    ENVOY_URL: server.url,
    AGENT_SECRETS_URL: server.url,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
  };
}

/** A relay socket for `session` at `server`, open once the first socket opens or `boundMs` pass;
 *  closed after the row. `heard.asks` counts each time the relay asks the credential for the
 *  upgrade's headers. */
async function opened(
  server: RelaySocketStandIn,
  session: string | (() => string),
  heard: {
    readonly logs?: string[];
    readonly told?: string[];
    readonly asks?: { count: number };
    readonly boundMs?: number;
  } = {}
): Promise<ListenerRelay> {
  const credential = brokerCredential(podEnvironment(server), session);
  const relay = await connectListenerRelay(
    {
      url: server.url,
      session,
      credential: {
        ...credential,
        headers: (...args) => {
          if (heard.asks !== undefined) heard.asks.count += 1;
          return credential.headers(...args);
        },
      },
      outage: inboundUnavailable((why) => heard.told?.push(why)),
      log: (line) => heard.logs?.push(line),
    },
    AbortSignal.timeout(heard.boundMs ?? 20_000)
  );
  started.push(() => relay.close());
  return relay;
}

/** Waits until `check` holds, failing with `what()` once `ms` pass. */
async function until(check: () => boolean, ms: number, what: () => string): Promise<void> {
  const ends = Date.now() + ms;
  while (!check()) {
    if (Date.now() > ends) throw new Error(what());
    await Bun.sleep(5);
  }
}

/** A relay frame on `session`'s own agent subject, stored now, as the listener relays one: each
 *  header a list of its values, as the wire carries a `nats.Header` (`../listener-relay.ts`). */
function frame(
  session: string,
  seq: number,
  extra: Readonly<Record<string, readonly string[]>> = {}
): StandInFrame {
  return {
    headers: {
      "Envoy-Subject": [`notifications.agent.${session}`],
      "Envoy-Stream-Seq": [String(seq)],
      "Envoy-Stream-Id": ["stream-1"],
      "Envoy-Delivered": ["1"],
      "Envoy-Stored-At": [new Date().toISOString()],
      ...extra,
    },
    data: JSON.stringify({ event_id: `evt-${seq}`, source: "agent" }),
  };
}

/** The upgrades `server` opened, in order. */
function opens(server: RelaySocketStandIn): StandInUpgrade[] {
  return server.upgrades.filter((upgrade) => upgrade.status === 101);
}

describe("a relay socket", () => {
  test("hands the host a frame, and the host's done goes back on the frame's reply", async () => {
    const server = standIn();
    const relay = await opened(server, "ses_a");
    const delivery = createConsumerDelivery({
      sessionID: "ses_a",
      host: { deliver: (handed) => handed.done(), gap: () => undefined, log: () => undefined },
      publish: (reply, body) => relay.publish(reply, encoder.encode(body)),
      ledger: { delivered: () => false, liveFailed: async () => true },
      persist: () => true,
    });
    const subscription = relay.subscribe(delivery.subject);
    void (async () => {
      for await (const message of subscription) delivery.receive(message);
    })();
    await relay.flush();
    const reply = server.deliver("ses_a", frame("ses_a", 7));
    if (reply === undefined) throw new Error("the stand-in holds no relay subscription of ses_a");
    await until(
      () => server.receipts.some((receipt) => receipt.reply === reply && receipt.body === "done"),
      5_000,
      () =>
        `frame 7 of stream-1 on notifications.agent.ses_a was never answered done on its reply; the stand-in read ${JSON.stringify(server.receipts)} and refused ${JSON.stringify(server.refusedPublishes)}`
    );
    expect(server.receipts.filter((receipt) => receipt.reply === reply).map((r) => r.body)).toEqual(
      ["received", "done"]
    );
    expect(server.refusedPublishes).toEqual([]);
    // Each `sub` and `pub` carried the wire's members and nothing more.
    expect(server.malformed).toEqual([]);
  });

  test("closed 4401 reopens within 1 s with a fresh token, and takes its subject again", async () => {
    const server = standIn();
    const relay = await opened(server, "ses_a");
    relay.subscribe(standInDeliverSubject("ses_a"));
    await relay.flush();
    const closedAt = Date.now();
    server.close(4401, "credential_expired");
    await until(
      () => opens(server).length === 2,
      1_000,
      () => `no reopen within 1 s of a 4401 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    const [before, after] = opens(server);
    expect(after?.at ?? Number.POSITIVE_INFINITY).toBeLessThan(closedAt + 1_000);
    expect(after?.token).not.toBe(before?.token);
    expect(server.minted.map((one) => one.session)).toEqual(["ses_a", "ses_a"]);
    await until(
      () => server.subs.length === 2,
      1_000,
      () => `the reopened socket took no subject again; it took ${JSON.stringify(server.subs)}`
    );
  });

  test("closed 4403 reopens with a fresh token after 1 s, then 2 s", async () => {
    const server = standIn();
    await opened(server, "ses_a");
    const closes: number[] = [];
    for (const nth of [1, 2]) {
      closes.push(Date.now());
      server.close(4403, "credential_refused");
      await until(
        () => opens(server).length === nth + 1,
        10_000,
        () => `no reopen after 4403 close ${nth}; upgrades ${JSON.stringify(server.upgrades)}`
      );
    }
    const waited = opens(server)
      .slice(1)
      .map((upgrade, index) => upgrade.at - (closes[index] ?? 0));
    // The stand-in's count of reopens sooner than the backoff: none.
    const early = waited.filter((ms, index) => ms < 1_000 * 2 ** index);
    if (early.length > 0) {
      throw new Error(
        `the stand-in counted ${early.length} reopens sooner than 1 s doubling after a 4403 close: ${waited.join(" ms, ")} ms after each`
      );
    }
    // Each wait is the backoff's and no longer than it by more than the trade and the upgrade.
    expect(waited[0]).toBeLessThan(1_900);
    expect(waited[1]).toBeLessThan(2_900);
    const tokens = opens(server).map((upgrade) => upgrade.token);
    expect(new Set(tokens).size).toBe(3);
  });

  test("closed 1013 reopens after 5 s, then 10 s, on the token it holds", async () => {
    const server = standIn();
    await opened(server, "ses_a");
    const closes: number[] = [];
    for (const nth of [1, 2]) {
      closes.push(Date.now());
      server.close(1013, "broker_unavailable");
      await until(
        () => opens(server).length === nth + 1,
        15_000,
        () => `no reopen after 1013 close ${nth}; upgrades ${JSON.stringify(server.upgrades)}`
      );
    }
    const waited = opens(server)
      .slice(1)
      .map((upgrade, index) => upgrade.at - (closes[index] ?? 0));
    const early = waited.filter((ms, index) => ms < 5_000 * 2 ** index);
    if (early.length > 0) {
      throw new Error(
        `the stand-in counted ${early.length} reopens sooner than 5 s doubling after a 1013 close: ${waited.join(" ms, ")} ms after each`
      );
    }
    expect(waited[0]).toBeLessThan(5_900);
    expect(waited[1]).toBeLessThan(10_900);
    expect(new Set(opens(server).map((upgrade) => upgrade.token)).size).toBe(1);
  }, 40_000);

  test("closed 1012 reopens at once", async () => {
    const server = standIn();
    await opened(server, "ses_a");
    const closedAt = Date.now();
    server.close(1012, "service_restart");
    await until(
      () => opens(server).length === 2,
      1_000,
      () => `no reopen within 1 s of a 1012 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    expect(opens(server)[1]?.at ?? Number.POSITIVE_INFINITY).toBeLessThan(closedAt + 1_000);
  });

  test("4401 and 1012 reopen at once however often they come", async () => {
    // The listener sends 4401 at most once a check and 1012 once a stop, so neither runs up a
    // wait, however close together they come, and neither waits out a wait the other ran up.
    const server = standIn();
    await opened(server, "ses_a");
    const waited: string[] = [];
    const closes = [
      [4401, "credential_expired"],
      [1012, "listener_stopping"],
      [4401, "credential_expired"],
      [1012, "listener_stopping"],
    ] as const;
    for (const [index, [code, reason]] of closes.entries()) {
      const closedAt = Date.now();
      server.close(code, reason);
      await until(
        () => opens(server).length === index + 2,
        10_000,
        () =>
          `no reopen after close ${index + 1} (${code}); upgrades ${JSON.stringify(server.upgrades)}`
      );
      waited.push(`${code} after ${(opens(server)[index + 1]?.at ?? 0) - closedAt} ms`);
    }
    const late = waited.filter((one) => Number(/after (\d+) ms/.exec(one)?.[1]) >= 1_000);
    if (late.length > 0) {
      throw new Error(
        `the stand-in counted ${late.length} reopens a second or more after their close: ${waited.join(", ")}`
      );
    }
  }, 30_000);

  test("a 1013 after a 4403 waits 5 s, not 10 s: each close counts its own run of waits", async () => {
    const server = standIn();
    await opened(server, "ses_a");
    server.close(4403, "credential_refused");
    await until(
      () => opens(server).length === 2,
      10_000,
      () => `no reopen after the 4403 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    const closedAt = Date.now();
    server.close(1013, "broker_unavailable");
    await until(
      () => opens(server).length === 3,
      20_000,
      () => `no reopen after the 1013 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    const waited = (opens(server)[2]?.at ?? 0) - closedAt;
    if (waited < 5_000 || waited >= 5_900) {
      throw new Error(
        `the reopen after a 1013 that came after a 4403 came ${waited} ms after it, where a first 1013 waits 5 s`
      );
    }
  }, 40_000);

  test("1001 reopens at once, then after 1 s while it repeats", async () => {
    // A proxy's 1001 and a socket that ended with no close frame (1006) can repeat as fast as a
    // socket opens, so a repeat within a minute waits 1 s doubling to a minute.
    const server = standIn();
    await opened(server, "ses_a");
    const waited: number[] = [];
    for (const nth of [1, 2]) {
      const closedAt = Date.now();
      server.close(1001, "going_away");
      await until(
        () => opens(server).length === nth + 1,
        10_000,
        () => `no reopen after 1001 close ${nth}; upgrades ${JSON.stringify(server.upgrades)}`
      );
      waited.push((opens(server)[nth]?.at ?? 0) - closedAt);
    }
    if ((waited[1] ?? 0) < 1_000) {
      throw new Error(
        `a second 1001 within a minute reopened ${waited[1]} ms after it, where a repeated 1001 waits 1 s`
      );
    }
    expect(waited[0]).toBeLessThan(1_000);
    expect(waited[1]).toBeLessThan(1_900);
  }, 30_000);

  test("closed 1008 bad_frame reopens after 1 s, then 2 s, on the token it holds, the close logged", async () => {
    // The listener answers a frame it cannot read `bad_frame` and closes 1008: this client's
    // fault, so it is told in the log and waited on as any other close is.
    const server = standIn();
    const logs: string[] = [];
    await opened(server, "ses_a", { logs });
    const closes: number[] = [];
    for (const nth of [1, 2]) {
      closes.push(Date.now());
      server.refuseFrame("ses_a");
      await until(
        () => opens(server).length === nth + 1,
        10_000,
        () => `no reopen after 1008 close ${nth}; upgrades ${JSON.stringify(server.upgrades)}`
      );
    }
    const waited = opens(server)
      .slice(1)
      .map((upgrade, index) => upgrade.at - (closes[index] ?? 0));
    const early = waited.filter((ms, index) => ms < 1_000 * 2 ** index);
    if (early.length > 0) {
      throw new Error(
        `the stand-in counted ${early.length} reopens sooner than 1 s doubling after a 1008 bad_frame close: ${waited.join(" ms, ")} ms after each`
      );
    }
    expect(waited[0]).toBeLessThan(1_900);
    expect(waited[1]).toBeLessThan(2_900);
    expect(new Set(opens(server).map((upgrade) => upgrade.token)).size).toBe(1);
    expect(logs).toContain(
      "the listener's relay socket for ses_a closed (1008 bad_frame); it opens again in 1000 ms"
    );
    expect(logs.filter((line) => line.includes("answered bad_frame"))).toEqual([
      "the listener's relay socket for ses_a answered bad_frame",
    ]);
  }, 30_000);

  test("after a 1013, upgrades refused 503 wait 5 s doubling on the token it holds, the status logged", async () => {
    // While the listener cannot ask the broker it refuses every upgrade 503. Bun names the status
    // in its error event (`Expected 101 status code: 503`), so the wait goes on doubling from the
    // 1013's 5 s and the token in hand is presented again: the broker's absence is no fault of it.
    const server = standIn();
    const logs: string[] = [];
    await opened(server, "ses_a", { logs });
    const trades = server.tradesAsked;
    server.refuseUpgrades = 503;
    const closedAt = Date.now();
    server.close(1013, "broker_unavailable");
    const refused = () => server.upgrades.filter((upgrade) => upgrade.status === 503);
    await until(
      () => refused().length >= 2,
      25_000,
      () =>
        `the stand-in refused ${refused().length} upgrades 503; upgrades ${JSON.stringify(server.upgrades)}`
    );
    const [first, second] = refused().map((upgrade) => upgrade.at);
    const waited = [(first ?? 0) - closedAt, (second ?? 0) - (first ?? 0)];
    const early = waited.filter((ms, index) => ms < 5_000 * 2 ** index);
    if (early.length > 0) {
      throw new Error(
        `the stand-in counted ${early.length} upgrades sooner than 5 s doubling after a 1013 close, each refused 503: ${waited.join(" ms, ")} ms after the one before`
      );
    }
    expect(waited[0]).toBeLessThan(5_900);
    expect(waited[1]).toBeLessThan(10_900);
    expect(server.tradesAsked - trades).toBe(0);
    expect(new Set(server.upgrades.map((upgrade) => upgrade.token)).size).toBe(1);
    expect(logs.filter((line) => line.includes("503"))).toEqual([
      "the listener's relay socket for ses_a could not be opened: the listener refused the upgrade with HTTP 503; it is tried again in 10000 ms, the wait doubling to a minute while it fails",
    ]);
  }, 40_000);

  test("an upgrade refused 401 trades a fresh token; one refused 429 opens again on the token it holds", async () => {
    // 401 and 403 say the token was the cause, so the next open trades for a fresh one and the
    // waits run as after 4403; any other status says nothing against the token.
    const server = standIn();
    await opened(server, "ses_a");
    const trades = server.tradesAsked;
    const refusedWith = (status: number) =>
      server.upgrades.filter((upgrade) => upgrade.status === status);
    server.refuseUpgrades = 429;
    server.close(1012, "listener_stopping");
    await until(
      () => refusedWith(429).length >= 2,
      10_000,
      () =>
        `the stand-in refused ${refusedWith(429).length} upgrades 429; upgrades ${JSON.stringify(server.upgrades)}`
    );
    server.refuseUpgrades = 401;
    await until(
      () => refusedWith(401).length >= 2,
      10_000,
      () =>
        `the stand-in refused ${refusedWith(401).length} upgrades 401; upgrades ${JSON.stringify(server.upgrades)}`
    );
    server.refuseUpgrades = undefined;
    await until(
      () => opens(server).length === 2,
      10_000,
      () =>
        `no socket opened once the stand-in took upgrades again; upgrades ${JSON.stringify(server.upgrades)}`
    );
    const held = opens(server)[0]?.token;
    // Refused 429: the token it held, both times; the first 401 is still presented that token.
    expect(
      refusedWith(429).map((upgrade) => (upgrade.token === held ? "the token it held" : "another"))
    ).toEqual(["the token it held", "the token it held"]);
    const [after429, again] = refusedWith(401);
    expect(after429?.token).toBe(held);
    // Each open after a 401 trades a fresh token: the second 401's, and the open's after it.
    const fresh = [again?.token, opens(server)[1]?.token];
    expect(new Set([held, ...fresh]).size).toBe(3);
    expect(server.tradesAsked - trades).toBe(2);
  }, 40_000);

  test("an upgrade the listener never answers is dropped after 20 s, logged, and tried again", async () => {
    // A path that takes the connection and never answers the upgrade holds the socket connecting
    // with no close: unbounded, the session would receive nothing and say nothing until restarted.
    const connections: number[] = [];
    const silent = Bun.listen({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        open: () => {
          connections.push(Date.now());
        },
        data: () => undefined,
      },
    });
    started.push(() => silent.stop(true));
    const logs: string[] = [];
    // Never traded or checked: the listener here answers nothing, so any token will do.
    const token = fixtureSecret("relay");
    const relay = new ListenerRelay({
      url: `http://127.0.0.1:${silent.port}`,
      session: "ses_a",
      credential: {
        scheme: "DPoP",
        headers: async () => ({ Authorization: `DPoP ${token}`, DPoP: "a-proof" }),
        secret: () => token,
      },
      outage: inboundUnavailable(
        () => undefined,
        () => undefined
      ),
      log: (line) => logs.push(line),
    });
    started.push(() => relay.close());
    await until(
      () => connections.length >= 2,
      30_000,
      () =>
        `the relay made ${connections.length} connection(s) in 30 s to a listener that never answers its upgrade, and logged ${JSON.stringify(logs)}`
    );
    const gap = (connections[1] ?? 0) - (connections[0] ?? 0);
    expect(gap).toBeGreaterThanOrEqual(20_000);
    expect(gap).toBeLessThan(23_000);
    expect(logs).toEqual([
      "the listener's relay socket for ses_a could not be opened: the upgrade was not answered within 20 s; it is tried again in 1000 ms, the wait doubling to a minute while it fails",
    ]);
  }, 45_000);

  test("a subject it does not carry closes that subscription alone, logged once", async () => {
    const server = standIn();
    const logs: string[] = [];
    const relay = await opened(server, "ses_a", { logs });
    const relayed = relay.subscribe(standInDeliverSubject("ses_a"));
    const refused = [
      relay.subscribe("agentstream.ses_a.control"),
      relay.subscribe("agentstream.ses_a.control"),
    ];
    // Each refused subscription ends, as a closed one does.
    for (const subscription of refused) {
      for await (const _ of subscription) throw new Error("a refused subscription yielded");
    }
    expect(server.refusedSubs).toEqual([
      { session: "ses_a", subject: "agentstream.ses_a.control", code: "subject_not_granted" },
      { session: "ses_a", subject: "agentstream.ses_a.control", code: "subject_not_granted" },
    ]);
    expect(logs.filter((line) => line.includes("agentstream.ses_a.control"))).toEqual([
      "the listener's relay socket for ses_a refused the subject agentstream.ses_a.control (subject_not_granted); that subscription is closed",
    ]);
    // The relay subscription goes on.
    server.deliver("ses_a", frame("ses_a", 1));
    const iterator = relayed[Symbol.asyncIterator]();
    const next = await iterator.next();
    expect(next.done).toBe(false);
    expect(next.value?.headers?.get("Envoy-Stream-Seq")).toBe("1");
  });

  test("a fifth subscription the socket refuses (subscription_limit) ends alone, logged once", async () => {
    // A socket holds at most four subscriptions; the listener answers a fifth `subscription_limit`
    // and keeps the socket open, so the four go on receiving.
    const server = standIn();
    const logs: string[] = [];
    const relay = await opened(server, "ses_a", { logs });
    const subject = standInDeliverSubject("ses_a");
    const held = [1, 2, 3, 4].map(() => relay.subscribe(subject));
    const fifth = relay.subscribe(subject);
    await relay.flush();
    const fifthNow = await Promise.race([
      fifth[Symbol.asyncIterator]()
        .next()
        .then((next) => (next.done === true ? "ended" : "yielded a frame")),
      // The listener answered the fifth `sub` before the pong above: a second round trip is past
      // anything still to come about it.
      relay.flush().then(() => "still open a round trip after the listener refused it"),
    ]);
    expect(fifthNow).toBe("ended");
    expect(server.refusedSubs).toEqual([{ session: "ses_a", subject, code: "subscription_limit" }]);
    expect(logs.filter((line) => line.includes("subscription_limit"))).toEqual([
      `the listener's relay socket for ses_a refused the subject ${subject} (subscription_limit); that subscription is closed`,
    ]);
    // The four others each still receive.
    expect(server.deliver("ses_a", frame("ses_a", 1))).toBeDefined();
    const seqs = await Promise.all(
      held.map(
        async (subscription) =>
          (await subscription[Symbol.asyncIterator]().next()).value?.headers.get(
            "Envoy-Stream-Seq"
          ) ?? "nothing"
      )
    );
    expect(seqs).toEqual(["1", "1", "1", "1"]);
  });

  test("a flush resolves once the listener has handled every sub sent before it", async () => {
    // The listener reads a socket's frames one at a time and handles a `sub` in full before the
    // next frame; a successful `sub` has no answer of its own. So the pong to a ping sent after
    // it is what says it is held.
    const server = standIn();
    const relay = await opened(server, "ses_a");
    const subject = standInDeliverSubject("ses_a");
    relay.subscribe(subject);
    await relay.flush();
    // Read at once, before the stand-in's event loop could run again.
    const handled = server.handled.filter((one) => one.session === "ses_a").map((one) => one.frame);
    if (!handled.includes(`sub 1 ${subject}`)) {
      throw new Error(
        `flush resolved before the listener handled sub 1 of ${subject}; it had handled ${JSON.stringify(handled)}`
      );
    }
    expect(handled).toEqual([`sub 1 ${subject}`, "ping 1"]);
    expect(server.deliver("ses_a", frame("ses_a", 1))).toBeDefined();
  });

  test("a repeated header's values reach the host in order, each name's case kept", async () => {
    const server = standIn();
    const relay = await opened(server, "ses_a");
    const subscription = relay.subscribe(standInDeliverSubject("ses_a"));
    await relay.flush();
    server.deliver(
      "ses_a",
      frame("ses_a", 3, { "X-Envoy-Trace": ["one", "two"], "x-envoy-trace": ["lower"] })
    );
    const next = await subscription[Symbol.asyncIterator]().next();
    if (next.done === true) throw new Error("the relay subscription ended with no frame");
    expect(next.value.headers.values("X-Envoy-Trace")).toEqual(["one", "two"]);
    expect(next.value.headers.get("X-Envoy-Trace")).toBe("one");
    expect(next.value.headers.values("x-envoy-trace")).toEqual(["lower"]);
    expect(next.value.headers.get("Envoy-Stream-Seq")).toBe("3");
    expect(next.value.headers.values("Envoy-Gap")).toEqual([]);
  });

  test("a refused trade is told once, and the socket opens once the broker trades", async () => {
    const server = standIn();
    server.trades = "refuse";
    const told: string[] = [];
    const asks = { count: 0 };
    // The token client answers a refusal again with no trade until its own retry, 15 s or so
    // later (`RETRY_FIRST_WAIT_MS`, @legion/broker-client/token-client), and the relay asks it at
    // 1 s, 3 s, 7 s, 15 s and 31 s: the open comes on one of the last two.
    const opening = opened(server, "ses_a", { told, asks, boundMs: 50_000 });
    await until(
      () => told.length > 0,
      5_000,
      () => "a refused trade was never told"
    );
    // Asked again after 1 s, then 2 s: still refused, and told no second time.
    await until(
      () => asks.count >= 3,
      5_000,
      () => `the relay asked its credential ${asks.count} times`
    );
    expect(told).toHaveLength(1);
    expect(told[0]).toContain(
      "the secrets broker refused the projected service-account token AGENT_SECRETS_MACHINE_TOKEN_FILE names: HTTP 400 invalid_grant"
    );
    server.trades = "mint";
    await opening;
    expect(opens(server)).toHaveLength(1);
  }, 60_000);

  test("follows the session its name reads now, with that session's own token", async () => {
    const server = standIn();
    let session = "ses_a";
    const relay = await opened(server, () => session);
    session = "ses_b";
    await relay.follow();
    expect(opens(server).map((upgrade) => upgrade.session)).toEqual(["ses_a", "ses_b"]);
    expect(server.minted.map((one) => one.session)).toEqual(["ses_a", "ses_b"]);
  });

  test("a flush pending when follow() opens another session's socket rejects, and no pong of the new socket answers it", async () => {
    // A flush is a round trip on one socket: one whose socket is dropped is never answered there,
    // and the next socket's pong says nothing of what the dropped one took.
    const server = standIn();
    let session = "ses_a";
    const relay = await opened(server, () => session);
    // Its ping goes out on ses_a's socket before the follow below drops that socket.
    const pinged = relay.flush();
    for (let turn = 0; turn < 10; turn += 1) await Promise.resolve();
    // Its socket is taken now, and its ping would go out only after the follow below.
    const waiting = relay.flush();
    const outcomes = [pinged, waiting].map((flush) =>
      flush.then(
        () => "resolved",
        (error: unknown) => `rejected: ${error instanceof Error ? error.message : String(error)}`
      )
    );
    session = "ses_b";
    await relay.follow();
    await relay.flush();
    const settled = await Promise.all(
      outcomes.map((outcome) =>
        Promise.race([outcome, relay.flush().then(() => "pending after two round trips")])
      )
    );
    expect(settled).toEqual([
      "rejected: the listener's relay socket was opened again before it answered the flush",
      "rejected: the listener's relay socket closed before the flush was sent",
    ]);
    expect(server.handled).toContainEqual({ session: "ses_a", frame: "ping 1" });
  });

  test("the stand-in asks a test's own routes first, and the trade and 404 take what they leave", async () => {
    // A driver registers and sends at the listener whose socket it opens, and serves its device
    // login's grants at the broker that trades its session's token, so its routes answer on the
    // one origin, the token endpoint included. Each is handed a copy of the request: one it reads
    // and leaves still reaches the trade whole. The relay upgrade is never handed to them.
    const asked: string[] = [];
    // Random, as a broker's token is: the client holds every token it is handed.
    const deviceToken = fixtureSecret("device");
    const server = startRelaySocketStandIn({
      routes: async (request) => {
        const { pathname } = new URL(request.url);
        const grant = new URLSearchParams(await request.text()).get("grant_type") ?? "";
        asked.push(`${request.method} ${pathname}${grant === "" ? "" : ` ${grant}`}`);
        if (pathname === "/v1/subscribe") return Response.json({ registered: true });
        if (grant === "urn:ietf:params:oauth:grant-type:device_code") {
          return Response.json({ access_token: deviceToken });
        }
        return undefined;
      },
    });
    started.push(() => server.stop());
    const registered = await fetch(`${server.url}/v1/subscribe`, { method: "POST" });
    expect(await registered.json()).toEqual({ registered: true });
    const device = await fetch(`${server.url}/v1/oauth/token`, {
      method: "POST",
      body: new URLSearchParams({ grant_type: "urn:ietf:params:oauth:grant-type:device_code" }),
    });
    expect(await device.json()).toEqual({ access_token: deviceToken });
    expect((await fetch(`${server.url}/v1/messages/send`, { method: "POST" })).status).toBe(404);
    // The session's own trade goes through the routes and on to the stand-in's trade.
    await opened(server, "ses_a");
    expect(asked).toEqual([
      "POST /v1/subscribe",
      "POST /v1/oauth/token urn:ietf:params:oauth:grant-type:device_code",
      "POST /v1/messages/send",
      "POST /v1/oauth/token urn:ietf:params:oauth:grant-type:token-exchange",
    ]);
    expect(server.minted.map((token) => token.session)).toEqual(["ses_a"]);
    expect(opens(server).map((upgrade) => upgrade.session)).toEqual(["ses_a"]);
  });

  test("a topic the edge refuses for the session's projects is dropped, and the rest go on being delivered", async () => {
    // An edge listener refuses a registration naming a topic of no project of the token's
    // (`repository_not_in_project`, `topic_not_in_project`) and names each refused topic as the
    // registration carried it. The session drops exactly those, says so once, and registers the
    // rest at once and at every heartbeat after; a frame on a topic it kept still reaches it.
    const kept = "notifications.dispatch.issue.PRIV-1.>";
    const refusedRoot = "notifications.github.acme.priv";
    const registered: (readonly string[])[] = [];
    const server = startRelaySocketStandIn({
      routes: async (request) => {
        if (new URL(request.url).pathname !== "/v1/interests/subscribe") return undefined;
        const body = (await request.json()) as {
          readonly session_id: string;
          readonly dir: string;
          readonly topics: readonly string[];
        };
        registered.push(body.topics);
        const refused = body.topics.filter((topic) => topic.startsWith(refusedRoot));
        if (refused.length > 0) {
          return Response.json(
            {
              error: "the edge serves the GitHub events of its projects' repositories alone",
              reason: "repository_not_in_project",
              refused,
            },
            { status: 403 }
          );
        }
        return Response.json({
          session_id: body.session_id,
          machine_id: "test",
          dir: body.dir,
          topics: body.topics,
          delivery: consumerDelivery(body.session_id),
        });
      },
    });
    started.push(() => server.stop());
    const relay = await opened(server, "ses_a");
    const handed: string[] = [];
    const delivery = createConsumerDelivery({
      sessionID: "ses_a",
      host: {
        deliver: (frame) => {
          handed.push(frame.subject);
          frame.done();
        },
        gap: () => undefined,
        log: () => undefined,
      },
      publish: (reply, body) => relay.publish(reply, encoder.encode(body)),
      ledger: { delivered: () => false, liveFailed: async () => true },
      persist: () => true,
    });
    const subscription = relay.subscribe(delivery.subject);
    void (async () => {
      for await (const message of subscription) delivery.receive(message);
    })();
    await relay.flush();
    const client = createEnvoyClient({
      baseUrl: server.url,
      fetch,
      credential: brokerCredential(podEnvironment(server), "ses_a"),
    });
    const followed = new Set(expandSubscriptionTopics([kept, `${refusedRoot}.>`]));
    const logs: string[] = [];
    const told: string[] = [];
    const outage = inboundUnavailable(
      (why) => told.push(why),
      () => told.push("restored")
    );
    const heartbeat = () =>
      registerWithRelay({
        relay: delivery,
        ownAgentSubject: "notifications.agent.ses_a",
        followed,
        outage,
        send: (topics, request) =>
          client.subscribe({
            sessionID: "ses_a",
            directory: "/work",
            topics,
            port: 0,
            title: "",
            driving: false,
            selfSubscribed: true,
            plugin: { name: "test", version: "0.0.0" },
            delivery: request,
          }),
        answered: () => undefined,
        drop: (topics) => {
          for (const topic of topics) followed.delete(topic);
        },
        log: (line) => logs.push(line),
      });

    await heartbeat();
    await heartbeat();

    const carriedRefused = registered
      .slice(1)
      .filter((topics) => topics.some((topic) => topic.startsWith(refusedRoot)));
    if (carriedRefused.length > 0) {
      throw new Error(
        `the stand-in saw a refused topic again after it refused it: ${JSON.stringify(carriedRefused)}`
      );
    }
    const keptTopics = [...expandSubscriptionTopics([kept])];
    expect(registered).toEqual([
      [
        "notifications.agent.ses_a",
        ...keptTopics,
        ...expandSubscriptionTopics([`${refusedRoot}.>`]),
      ],
      ["notifications.agent.ses_a", ...keptTopics],
      ["notifications.agent.ses_a", ...keptTopics],
    ]);
    expect([...followed]).toEqual(keptTopics);
    expect(logs).toEqual([
      `the listener refused ${expandSubscriptionTopics([`${refusedRoot}.>`]).join(", ")} for this session's projects (repository_not_in_project); the session no longer follows them and registers the rest`,
    ]);
    expect(told).toEqual([]);

    // A frame on the topic the session kept still reaches it, through its own consumer.
    const reply = server.deliver(
      "ses_a",
      frame("ses_a", 3, { "Envoy-Subject": ["notifications.dispatch.issue.PRIV-1.comment"] })
    );
    if (reply === undefined) throw new Error("the stand-in holds no relay subscription of ses_a");
    await until(
      () => server.receipts.some((receipt) => receipt.reply === reply && receipt.body === "done"),
      5_000,
      () =>
        `the frame on the kept topic was never answered done; the host was handed ${JSON.stringify(handed)}`
    );
    expect(handed).toEqual(["notifications.dispatch.issue.PRIV-1.comment"]);
  });
});

/** The settings `env` and an envoy.json holding `file` resolve, as a session of `ses_a` reads
 *  them, or, with `{}`, a caller that names no session. */
function settings(
  env: EnvoyEnvironment,
  file?: unknown,
  caller: { readonly session?: string } = { session: "ses_a" }
) {
  const config = path.join(tempDir(), "envoy.json");
  if (file !== undefined) writeFileSync(config, JSON.stringify(file));
  const resolved = envoySettings(env, readEnvoyConfig(config, env, { cwd: tempDir() }), caller);
  if (!resolved.enabled) throw new Error(`Envoy is off: ${resolved.error ?? resolved.notice}`);
  return resolved;
}

describe("which inbound a session takes", () => {
  const pod = (): EnvoyEnvironment => {
    const dir = tempDir();
    const tokenFile = path.join(dir, "token");
    writeFileSync(tokenFile, `${fixtureSecret("proj")}\n`);
    return {
      HOME: dir,
      ENVOY_URL: "https://envoy.example",
      AGENT_SECRETS_URL: "https://secrets.example",
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
    };
  };

  test("ENVOY_INBOUND=listener takes it through the relay socket on the broker credential", () => {
    const { inbound } = settings({ ...pod(), ENVOY_INBOUND: "listener" });
    if (!("relaySetting" in inbound))
      throw new Error(`no relay socket: ${JSON.stringify(inbound)}`);
    expect(inbound.relaySetting).toBe("ENVOY_INBOUND");
    expect(inbound.relayCredential.scheme).toBe("DPoP");
  });

  test("envoy.inbound listener in the file does too, and ENVOY_INBOUND=nats overrides it", () => {
    const fromFile = settings(pod(), { envoy: { inbound: "listener" } }).inbound;
    expect("relaySetting" in fromFile && fromFile.relaySetting).toMatch(
      /^envoy\.inbound in \/\S+\/envoy\.json$/
    );
    expect(
      settings({ ...pod(), ENVOY_INBOUND: "nats" }, { envoy: { inbound: "listener" } }).inbound
    ).toEqual({
      off: "Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)",
    });
  });

  test("ENVOY_INBOUND=listener beside ENVOY_NATS_URL is refused naming both", () => {
    const env = {
      ...pod(),
      ENVOY_INBOUND: "listener",
      ENVOY_NATS_URL: "nats://nats.internal.example:4222",
    };
    expect(settings(env).inbound).toEqual({
      off: "ENVOY_INBOUND is listener, and ENVOY_NATS_URL names NATS servers: set one of the two",
    });
    const fromFile = settings(pod(), {
      envoy: { inbound: "listener", natsUrls: ["nats://nats.internal.example:4222"] },
    }).inbound;
    expect("off" in fromFile && fromFile.off).toMatch(
      /^envoy\.inbound in (\/\S+\/envoy\.json) is listener, and envoy\.natsUrls in \1 names NATS servers: set one of the two$/
    );
  });

  test("ENVOY_INBOUND=listener with a bearer or no broker route is refused naming why", () => {
    expect(
      settings({ ...pod(), ENVOY_INBOUND: "listener", ENVOY_TOKEN: "bearer-1" }).inbound
    ).toEqual({
      off: "ENVOY_INBOUND is listener, but the listener's credential is a bearer (ENVOY_TOKEN, ENVOY_TOKEN_FILE or envoy.token), and the relay socket admits only the session's broker token",
    });
    expect(
      settings({ ...pod(), ENVOY_INBOUND: "listener", AGENT_SECRETS_URL: "" }).inbound
    ).toEqual({
      off: "ENVOY_INBOUND is listener, but AGENT_SECRETS_URL names no secrets broker to trade it at",
    });
  });

  test("ENVOY_INBOUND=listener for a caller that names no session is refused naming it", () => {
    expect(settings({ ...pod(), ENVOY_INBOUND: "listener" }, undefined, {}).inbound).toEqual({
      off: "ENVOY_INBOUND is listener, but this process names no session for its relay socket's token",
      noSessionNamed: true,
    });
  });

  test("another value is refused naming the setting and its two values, not the value", () => {
    expect(settings({ ...pod(), ENVOY_INBOUND: "Listener" }).inbound).toEqual({
      off: "ENVOY_INBOUND is neither nats nor listener",
    });
  });

  test("ENVOY_INBOUND=nats is what an unset one is", () => {
    const env = { ...pod(), ENVOY_NATS_URL: "nats://nats.internal.example:4222" };
    expect(settings({ ...env, ENVOY_INBOUND: "nats" }).inbound).toEqual(settings(env).inbound);
    expect(settings({ ...env, ENVOY_INBOUND: "" }).inbound).toEqual(settings(env).inbound);
  });

  test("the start notice says the relay socket carries no live view", () => {
    const env = { ...pod(), ENVOY_INBOUND: "listener" };
    const file = path.join(tempDir(), "envoy.json");
    const { notice } = sessionSettings({ path: file, error: null }, env, {
      cwd: tempDir(),
      session: "ses_a",
    });
    expect(notice).toContain(
      "Envoy's inbound messages come through the listener's relay socket (ENVOY_INBOUND is listener), which carries no live view: the Agents page shows nothing of this session"
    );
  });
});
