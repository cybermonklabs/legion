// packages/envoy-client/src/__tests__/listener-relay.test.ts
//
// A session's inbound through the listener's relay socket (`../listener-relay.ts`), against a
// stand-in that serves the socket's wire and the broker's token exchange on one origin
// (`../../test/relay-socket-stand-in.ts`, with the fixtures of `../../test/relay-socket-fixtures.ts`):
// a frame the host answers `done` goes back on the frame's reply; each close code and each refused
// upgrade is opened again as the wire says, each cause counting its own run of waits, with a fresh
// trade only where the token was the cause; an upgrade never answered is dropped and tried again;
// a subject the socket does not carry, and a second subscription, close that subscription alone
// and are logged once; a flush is answered only by its own socket; and a refused trade is told as
// an inbound outage is. The socket with a host's registrations is `./relay-registration.test.ts`,
// and which settings choose the socket is `./envoy-settings.test.ts`.
//
// The reopen rows read the real clock against a real socket, but what they hold is the wait each
// close logs, never how long a reopen took beyond it; the waits themselves, each cause and streak,
// are the table at the end (`relayReopenDelay`), which reads no clock.

import { describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import {
  frame,
  opens,
  registerOnce,
  relaySocketFixtures,
  until,
} from "../../test/relay-socket-fixtures";
import { standInDeliverSubject } from "../../test/relay-socket-stand-in";
import { createConsumerDelivery, inboundUnavailable } from "../consumer-delivery";
import { ListenerRelay, type RelayEnd, relayReopenDelay, relayRun } from "../listener-relay";

const { started, standIn, opened } = relaySocketFixtures("listener-relay");

const encoder = new TextEncoder();

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

  test("closed 4401 reopens at once with a fresh token, and takes its subject again", async () => {
    const server = standIn();
    const logs: string[] = [];
    const relay = await opened(server, "ses_a", { logs });
    relay.subscribe(standInDeliverSubject("ses_a"));
    await relay.flush();
    server.close(4401, "credential_expired");
    await until(
      () => opens(server).length === 2,
      10_000,
      () => `no reopen after a 4401 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    expect(logs.filter((line) => line.includes(" closed ("))).toEqual([
      "the listener's relay socket for ses_a closed (4401 credential_expired); it opens again in 0 ms",
    ]);
    const [before, after] = opens(server);
    expect(after?.token).not.toBe(before?.token);
    expect(server.minted.map((one) => one.session)).toEqual(["ses_a", "ses_a"]);
    await until(
      () => server.subs.length === 2,
      10_000,
      () => `the reopened socket took no subject again; it took ${JSON.stringify(server.subs)}`
    );
  });

  test("closed 4403 reopens with a fresh token after 1 s, then 2 s", async () => {
    const server = standIn();
    const logs: string[] = [];
    await opened(server, "ses_a", { logs });
    const closedAt = Date.now();
    server.close(4403, "credential_refused");
    await until(
      () => opens(server).length === 2,
      10_000,
      () => `no reopen after the 4403 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    server.close(4403, "credential_refused");
    await until(
      () => logs.filter((line) => line.includes("4403")).length === 2,
      5_000,
      () => `the second 4403 close was not logged; the log holds ${JSON.stringify(logs)}`
    );
    // The waits are the logged ones (`relayReopenDelay`), and the first was waited out.
    expect(logs.filter((line) => line.includes("4403"))).toEqual([
      "the listener's relay socket for ses_a closed (4403 credential_refused); it opens again in 1000 ms",
      "the listener's relay socket for ses_a closed (4403 credential_refused); it opens again in 2000 ms",
    ]);
    expect((opens(server)[1]?.at ?? 0) - closedAt).toBeGreaterThanOrEqual(1_000);
    const [first, second] = opens(server);
    expect(second?.token).not.toBe(first?.token);
  });

  test("closed 1013 reopens after 5 s, then 10 s, on the token it holds", async () => {
    const server = standIn();
    const logs: string[] = [];
    await opened(server, "ses_a", { logs });
    const closedAt = Date.now();
    server.close(1013, "broker_unavailable");
    await until(
      () => opens(server).length === 2,
      15_000,
      () => `no reopen after the 1013 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    server.close(1013, "broker_unavailable");
    await until(
      () => logs.filter((line) => line.includes("1013")).length === 2,
      5_000,
      () => `the second 1013 close was not logged; the log holds ${JSON.stringify(logs)}`
    );
    expect(logs.filter((line) => line.includes("1013"))).toEqual([
      "the listener's relay socket for ses_a closed (1013 broker_unavailable); it opens again in 5000 ms",
      "the listener's relay socket for ses_a closed (1013 broker_unavailable); it opens again in 10000 ms",
    ]);
    expect((opens(server)[1]?.at ?? 0) - closedAt).toBeGreaterThanOrEqual(5_000);
    expect(new Set(opens(server).map((upgrade) => upgrade.token)).size).toBe(1);
  }, 30_000);

  test("4401 and 1012 reopen at once however often they come", async () => {
    // The listener sends 4401 at most once a check and 1012 once a stop, so neither runs up a
    // wait, however close together they come, and neither waits out a wait the other ran up.
    const server = standIn();
    const logs: string[] = [];
    await opened(server, "ses_a", { logs });
    const closes = [
      [4401, "credential_expired"],
      [1012, "listener_stopping"],
      [4401, "credential_expired"],
      [1012, "listener_stopping"],
    ] as const;
    for (const [index, [code, reason]] of closes.entries()) {
      server.close(code, reason);
      await until(
        () => opens(server).length === index + 2,
        10_000,
        () =>
          `no reopen after close ${index + 1} (${code}); upgrades ${JSON.stringify(server.upgrades)}`
      );
    }
    expect(logs.filter((line) => line.includes(" closed ("))).toEqual(
      closes.map(
        ([code, reason]) =>
          `the listener's relay socket for ses_a closed (${code} ${reason}); it opens again in 0 ms`
      )
    );
  });

  test("a 1013 after a 4403 waits 5 s, not 10 s: each close counts its own run of waits", async () => {
    const server = standIn();
    const logs: string[] = [];
    await opened(server, "ses_a", { logs });
    server.close(4403, "credential_refused");
    await until(
      () => opens(server).length === 2,
      10_000,
      () => `no reopen after the 4403 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    server.close(1013, "broker_unavailable");
    await until(
      () => logs.some((line) => line.includes("1013")),
      5_000,
      () => `the 1013 close was not logged; the log holds ${JSON.stringify(logs)}`
    );
    expect(logs.filter((line) => line.includes("1013"))).toEqual([
      "the listener's relay socket for ses_a closed (1013 broker_unavailable); it opens again in 5000 ms",
    ]);
  });

  test("1001 reopens at once, then after 1 s while it repeats", async () => {
    // A proxy's 1001 and a socket that ended with no close frame (1006) can repeat as fast as a
    // socket opens, so a repeat within a minute waits 1 s doubling to a minute.
    const server = standIn();
    const logs: string[] = [];
    await opened(server, "ses_a", { logs });
    server.close(1001, "going_away");
    await until(
      () => opens(server).length === 2,
      5_000,
      () => `no reopen after the first 1001 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    server.close(1001, "going_away");
    await until(
      () => logs.filter((line) => line.includes("1001")).length === 2,
      5_000,
      () => `the second 1001 close was not logged; the log holds ${JSON.stringify(logs)}`
    );
    expect(logs.filter((line) => line.includes("1001"))).toEqual([
      "the listener's relay socket for ses_a closed (1001 going_away); it opens again in 0 ms",
      "the listener's relay socket for ses_a closed (1001 going_away); it opens again in 1000 ms",
    ]);
  });

  test("closed 1008 bad_frame reopens after 1 s, then 2 s, on the token it holds, the close logged", async () => {
    // The listener answers a frame it cannot read `bad_frame` and closes 1008: this client's
    // fault, so it is told in the log and waited on as any other close is.
    const server = standIn();
    const logs: string[] = [];
    await opened(server, "ses_a", { logs });
    const closedAt = Date.now();
    server.refuseFrame("ses_a");
    await until(
      () => opens(server).length === 2,
      10_000,
      () => `no reopen after the 1008 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    server.refuseFrame("ses_a");
    await until(
      () => logs.filter((line) => line.includes("1008")).length === 2,
      5_000,
      () => `the second 1008 close was not logged; the log holds ${JSON.stringify(logs)}`
    );
    expect(logs.filter((line) => line.includes("1008"))).toEqual([
      "the listener's relay socket for ses_a closed (1008 bad_frame: one of the frames sent since its last pong, none); it opens again in 1000 ms",
      "the listener's relay socket for ses_a closed (1008 bad_frame: one of the frames sent since its last pong, none); it opens again in 2000 ms",
    ]);
    expect((opens(server)[1]?.at ?? 0) - closedAt).toBeGreaterThanOrEqual(1_000);
    expect(new Set(opens(server).map((upgrade) => upgrade.token)).size).toBe(1);
    expect(logs.filter((line) => line.includes("answered bad_frame"))).toEqual([
      "the listener's relay socket for ses_a answered bad_frame",
    ]);
  });

  test("a 1008 bad_frame close names the ops of the frames sent since the listener last answered a ping", async () => {
    // The listener's `bad_frame` names no op: the frame it could not read is one of those this
    // client sent since its last pong, which the listener had answered only after every earlier one.
    const server = standIn();
    const logs: string[] = [];
    const relay = await opened(server, "ses_a", { logs });
    // A `sub` and an `unsub` the pong below answers, so neither is named for them.
    relay.subscribe(standInDeliverSubject("ses_a")).unsubscribe();
    await relay.flush();
    // Both go out after the pong above, and no ping follows them.
    relay.subscribe(standInDeliverSubject("ses_a"));
    relay.publish("notifications.envoy.relayreply.unknown", new Uint8Array());
    server.refuseFrame("ses_a");
    await until(
      () => logs.some((line) => line.includes("1008 bad_frame")),
      5_000,
      () => `no 1008 close was logged; the log holds ${JSON.stringify(logs)}`
    );
    expect(logs.filter((line) => line.includes("1008 bad_frame"))).toEqual([
      "the listener's relay socket for ses_a closed (1008 bad_frame: one of the frames sent since its last pong, sub, pub); it opens again in 1000 ms",
    ]);
  });

  test("a frame carrying a 1 MiB body is handed over, and one past 2 MiB is dropped and logged once", async () => {
    // A relay frame carries one NATS message, at most 1 MiB, as base64 inside its JSON: about
    // 1.4 MiB. A frame past 2 MiB is no listener's, and is dropped unread; its message comes again
    // after the session consumer's ack wait, as any frame left unanswered does.
    const server = standIn();
    const logs: string[] = [];
    const relay = await opened(server, "ses_a", { logs });
    const subscription = relay.subscribe(standInDeliverSubject("ses_a"));
    await relay.flush();
    const iterator = subscription[Symbol.asyncIterator]();
    server.deliver("ses_a", { headers: frame("ses_a", 1).headers, data: "x".repeat(1 << 20) });
    const first = await iterator.next();
    expect(first.value?.data.byteLength).toBe(1 << 20);

    for (const seq of [2, 3]) {
      server.deliver("ses_a", { headers: frame("ses_a", seq).headers, data: "y".repeat(2 << 20) });
    }
    server.deliver("ses_a", frame("ses_a", 4));
    const next = await iterator.next();
    expect(next.value?.headers.get("Envoy-Stream-Seq")).toBe("4");
    expect(logs.filter((line) => line.includes("past"))).toEqual([
      "the listener's relay socket for ses_a sent a frame past 2 MiB, which no relay frame is; it is dropped, and its message comes again after the consumer's ack wait",
    ]);
  });

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
      () => refused().length >= 1 && logs.some((line) => line.includes("HTTP 503")),
      15_000,
      () =>
        `the stand-in refused ${refused().length} upgrades 503; upgrades ${JSON.stringify(server.upgrades)}`
    );
    expect((refused()[0]?.at ?? 0) - closedAt).toBeGreaterThanOrEqual(5_000);
    expect(server.tradesAsked - trades).toBe(0);
    expect(new Set(server.upgrades.map((upgrade) => upgrade.token)).size).toBe(1);
    expect(logs.filter((line) => line.includes("1013") || line.includes("503"))).toEqual([
      "the listener's relay socket for ses_a closed (1013 broker_unavailable); it opens again in 5000 ms",
      "the listener's relay socket for ses_a could not be opened: the listener refused the upgrade with HTTP 503; it is tried again in 10000 ms, the wait doubling to a minute while it fails",
    ]);
  }, 30_000);

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

  test("an upgrade refused 403 trades a fresh token, and waits 1 s, then 2 s, as after 4403", async () => {
    const server = standIn();
    await opened(server, "ses_a");
    const trades = server.tradesAsked;
    const refused = () => server.upgrades.filter((upgrade) => upgrade.status === 403);
    server.refuseUpgrades = 403;
    server.close(1012, "listener_stopping");
    await until(
      () => refused().length >= 2,
      10_000,
      () =>
        `the stand-in refused ${refused().length} upgrades 403; upgrades ${JSON.stringify(server.upgrades)}`
    );
    server.refuseUpgrades = undefined;
    await until(
      () => opens(server).length === 2,
      10_000,
      () =>
        `no socket opened once the stand-in took upgrades again; upgrades ${JSON.stringify(server.upgrades)}`
    );
    const held = opens(server)[0]?.token;
    const [first, second] = refused();
    // The 1012 says nothing against the token, so the first upgrade after it presents it.
    expect(first?.token).toBe(held);
    // Each open after a 403 trades a fresh token: the second 403's, and the open's after it.
    expect(new Set([held, second?.token, opens(server)[1]?.token]).size).toBe(3);
    expect(server.tradesAsked - trades).toBe(2);
    // The stand-in took upgrades again once it had refused two, so the open is the next attempt.
    expect({
      afterFirst: (second?.at ?? 0) - (first?.at ?? 0) >= 1_000,
      afterSecond: (opens(server)[1]?.at ?? 0) - (second?.at ?? 0) >= 2_000,
    }).toEqual({ afterFirst: true, afterSecond: true });
  }, 30_000);

  test("every open presents a proof of its own, the token it holds or a fresh one", async () => {
    // A reopen after 1012 presents the token in hand, and one after 4401 a fresh one: either way
    // its proof is new, since a proof names the one request it was signed for.
    const server = standIn();
    await opened(server, "ses_a");
    server.close(1012, "listener_stopping");
    await until(
      () => opens(server).length === 2,
      5_000,
      () => `no reopen after the 1012 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    server.close(4401, "credential_expired");
    await until(
      () => opens(server).length === 3,
      5_000,
      () => `no reopen after the 4401 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    const [first, second, third] = opens(server);
    expect([second?.token === first?.token, third?.token === first?.token]).toEqual([true, false]);
    const proofs = opens(server).map((upgrade) => upgrade.proof);
    expect(proofs.every((proof) => proof !== "")).toBe(true);
    expect(new Set(proofs).size).toBe(3);
  });

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
      outage: { refused: () => undefined },
      log: (line) => logs.push(line),
    });
    started.push(() => relay.close());
    await until(
      () => connections.length >= 2,
      30_000,
      () =>
        `the relay made ${connections.length} connection(s) in 30 s to a listener that never answers its upgrade, and logged ${JSON.stringify(logs)}`
    );
    // The 20 s deadline is the logged line below; the gap only shows it was waited out.
    expect((connections[1] ?? 0) - (connections[0] ?? 0)).toBeGreaterThanOrEqual(20_000);
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
    // Each refused subscription ends with an error naming the subject and code, as a nats.js
    // subscription a server refused does, so its reader is told why.
    const ended = await Promise.all(
      refused.map((subscription) =>
        subscription[Symbol.asyncIterator]()
          .next()
          .then(
            (next) => (next.done === true ? "ended with no error" : "yielded a frame"),
            (error: unknown) => (error instanceof Error ? error.message : String(error))
          )
      )
    );
    expect(ended).toEqual([
      "the listener's relay socket refused the subject agentstream.ses_a.control (subject_not_granted)",
      "the listener's relay socket refused the subject agentstream.ses_a.control (subject_not_granted)",
    ]);
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

  test("a second subscription the socket refuses (subscription_limit) ends alone, logged once", async () => {
    // A socket holds one subscription; the listener answers a second `subscription_limit` and keeps
    // the socket open, so the first goes on receiving.
    const server = standIn();
    const logs: string[] = [];
    const relay = await opened(server, "ses_a", { logs });
    const subject = standInDeliverSubject("ses_a");
    const first = relay.subscribe(subject);
    const second = relay.subscribe(subject);
    await relay.flush();
    const secondNow = await Promise.race([
      second[Symbol.asyncIterator]()
        .next()
        .then(
          (next) => (next.done === true ? "ended with no error" : "yielded a frame"),
          (error: unknown) => (error instanceof Error ? error.message : String(error))
        ),
      // The listener answered the second `sub` before the pong above: a second round trip is past
      // anything still to come about it.
      relay.flush().then(() => "still open a round trip after the listener refused it"),
    ]);
    expect(secondNow).toBe(
      `the listener's relay socket refused the subject ${subject} (subscription_limit)`
    );
    expect(server.refusedSubs).toEqual([{ session: "ses_a", subject, code: "subscription_limit" }]);
    expect(logs.filter((line) => line.includes("subscription_limit"))).toEqual([
      `the listener's relay socket for ses_a refused the subject ${subject} (subscription_limit); that subscription is closed`,
    ]);
    // The first still receives.
    expect(server.deliver("ses_a", frame("ses_a", 1))).toBeDefined();
    const next = await first[Symbol.asyncIterator]().next();
    expect(next.value?.headers.get("Envoy-Stream-Seq")).toBe("1");
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
    const restored = { count: 0 };
    const outage = inboundUnavailable(
      (why) => told.push(why),
      () => {
        restored.count += 1;
      }
    );
    // The token client answers a refusal again with no trade until its own retry, 15 s or so
    // later (`RETRY_FIRST_WAIT_MS`, @legion/cipher-client/token-client), and the relay asks it at
    // 1 s, 3 s, 7 s, 15 s and 31 s: the open comes on one of the last two.
    const opening = opened(server, "ses_a", { asks, outage, boundMs: 50_000 });
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
    // The socket's open ends no outage; the registration the open lets the host make is accepted,
    // and that ends the outage the failed trade told, once.
    await registerOnce(outage, () => false);
    expect(restored.count).toBe(1);
  }, 60_000);

  test("a trade that fails for a session the relay has since followed away from tells no outage", async () => {
    // `follow` opens for the session the host moved to while the old session's trade still waits;
    // when that trade fails, its open no longer stands, and the outage report is the new
    // session's, so it is told nothing.
    const listener = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      fetch: (request, server) =>
        server.upgrade(request, { headers: { "Sec-WebSocket-Protocol": "envoy-relay.v1" } })
          ? undefined
          : new Response("not an upgrade", { status: 400 }),
      websocket: { message: () => undefined },
    });
    started.push(() => listener.stop(true));
    let session = "ses_a";
    const oldTrade = Promise.withResolvers<Readonly<Record<string, string>>>();
    const token = fixtureSecret("relay");
    const told: string[] = [];
    const relay = new ListenerRelay({
      url: `http://127.0.0.1:${listener.port}`,
      session: () => session,
      credential: {
        scheme: "DPoP",
        headers: async (_method, _url, forSession) =>
          forSession === "ses_a"
            ? oldTrade.promise
            : { Authorization: `DPoP ${token}`, DPoP: "a-proof" },
        secret: () => token,
      },
      outage: { refused: (why) => told.push(String(why)) },
      log: () => undefined,
    });
    started.push(() => relay.close());
    session = "ses_b";
    await relay.follow(5_000);
    oldTrade.reject(new Error("the trade for ses_a failed"));
    // A round trip on ses_b's socket: anything the old trade's failure set off has run by now.
    await relay.flush();
    expect(told).toEqual([]);
  });

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

  test("a reopen is a disconnect and then a reconnect on status(), from which a host begins its next epoch", async () => {
    // Both hosts register a new epoch at `reconnect`, so the listener restarts the session's relay
    // at its ack floor and nothing published while the socket was away is lost.
    const server = standIn();
    const relay = await opened(server, "ses_a");
    const statuses = relay.status()[Symbol.asyncIterator]();
    server.close(1012, "listener_stopping");
    const next = () =>
      Promise.race([
        statuses.next().then((read) => (read.done === true ? "ended" : read.value.type)),
        Bun.sleep(5_000).then(() => "nothing within 5 s"),
      ]);
    const seen = [await next(), await next()];
    expect(seen).toEqual(["disconnect", "reconnect"]);
  });

  test("a socket the path drops with no close frame (1006) reopens at once and takes its subject again", async () => {
    const server = standIn();
    const logs: string[] = [];
    const relay = await opened(server, "ses_a", { logs });
    relay.subscribe(standInDeliverSubject("ses_a"));
    await relay.flush();
    server.drop("ses_a");
    await until(
      () => opens(server).length === 2 && server.subs.length === 2,
      5_000,
      () =>
        `no reopen and no subject taken again after the drop; upgrades ${JSON.stringify(server.upgrades)}, subs ${JSON.stringify(server.subs)}`
    );
    expect(logs.filter((line) => line.includes(" closed ("))).toEqual([
      "the listener's relay socket for ses_a closed (1006); it opens again in 0 ms",
    ]);
  });

  test("a socket that answers no ping for 20 s is ended, logged, and opened again", async () => {
    // A path that froze keeps the socket open with nothing on it. The client pings every 10 s,
    // and a socket that answered neither of two is ended (1006) and opened again at once. The
    // listener here completes the upgrade and then reads nothing, so it answers no ping.
    const upgrades: number[] = [];
    const deaf = Bun.listen({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        data: (socket, data) => {
          const text = Buffer.from(data).toString("latin1");
          const key = /Sec-WebSocket-Key: (.+)\r\n/i.exec(text)?.[1]?.trim();
          if (key === undefined) return;
          upgrades.push(Date.now());
          const accept = createHash("sha1")
            .update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`)
            .digest("base64");
          socket.write(
            `HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\nSec-WebSocket-Protocol: envoy-relay.v1\r\n\r\n`
          );
        },
      },
    });
    started.push(() => deaf.stop(true));
    const logs: string[] = [];
    // Never traded or checked: this listener reads nothing, so any token will do.
    const token = fixtureSecret("relay");
    const relay = new ListenerRelay({
      url: `http://127.0.0.1:${deaf.port}`,
      session: "ses_a",
      credential: {
        scheme: "DPoP",
        headers: async () => ({ Authorization: `DPoP ${token}`, DPoP: "a-proof" }),
        secret: () => token,
      },
      outage: { refused: () => undefined },
      log: (line) => logs.push(line),
    });
    started.push(() => relay.close());
    await until(
      () => upgrades.length >= 2,
      45_000,
      () =>
        `the deaf socket was not opened again within 45 s; upgrades at ${JSON.stringify(upgrades)}, log ${JSON.stringify(logs)}`
    );
    expect((upgrades[1] ?? 0) - (upgrades[0] ?? 0)).toBeGreaterThanOrEqual(20_000);
    expect(logs.slice(0, 2)).toEqual([
      "the listener's relay socket for ses_a: the socket answered no ping for 20 s",
      "the listener's relay socket for ses_a closed (1006); it opens again in 0 ms",
    ]);
  }, 60_000);

  test("a frame that is no JSON, and a msg whose headers are no map of lists, are dropped and logged once; later frames go on", async () => {
    const server = standIn();
    const logs: string[] = [];
    const relay = await opened(server, "ses_a", { logs });
    const subscription = relay.subscribe(standInDeliverSubject("ses_a"));
    await relay.flush();
    const sid = Number(
      /^sub (\d+) /.exec(
        server.handled.find((one) => one.frame.startsWith("sub "))?.frame ?? ""
      )?.[1]
    );
    const malformed = JSON.stringify({
      op: "msg",
      sid,
      subject: standInDeliverSubject("ses_a"),
      reply: "notifications.envoy.relayreply.x",
      headers: { "Envoy-Subject": "notifications.agent.ses_a" },
      data: Buffer.from("{}").toString("base64"),
    });
    for (const text of ["no json", "no json", malformed, malformed]) server.sendRaw("ses_a", text);
    server.deliver("ses_a", frame("ses_a", 9));
    const next = await subscription[Symbol.asyncIterator]().next();
    expect(next.value?.headers.get("Envoy-Stream-Seq")).toBe("9");
    expect(logs).toEqual([
      "the listener's relay socket for ses_a sent a frame that is no JSON",
      "the listener's relay socket for ses_a sent a msg whose headers are not a map of names to lists of strings; it is dropped",
    ]);
  });

  test("an err code the wire does not name is logged once in all, cut short, and a named one once", async () => {
    // The code is the listener's text: a listener sending code after code, or one code of any
    // length, grows the log by one line of bounded length.
    const server = standIn();
    const logs: string[] = [];
    await opened(server, "ses_a", { logs });
    const long = `x${"y".repeat(10_000)}`;
    for (const code of [long, "another_unknown", "publish_not_granted", "publish_not_granted"]) {
      server.sendRaw("ses_a", JSON.stringify({ op: "err", code }));
    }
    // A round trip after them: the listener's frames reach the client in order.
    server.sendRaw("ses_a", JSON.stringify({ op: "err", code: "bad_frame" }));
    await until(
      () => logs.some((line) => line.includes("answered bad_frame")),
      5_000,
      () => `the last err was never logged; the log holds ${JSON.stringify(logs)}`
    );
    expect(logs).toEqual([
      `the listener's relay socket for ses_a answered ${long.slice(0, 64)}…, a code this client does not know; later unknown codes are not logged`,
      "the listener's relay socket for ses_a answered publish_not_granted",
      "the listener's relay socket for ses_a answered bad_frame",
    ]);
  });

  test("drain unsubscribes every subscription, waits for the listener to take that, and closes", async () => {
    const server = standIn();
    const relay = await opened(server, "ses_a");
    const subject = standInDeliverSubject("ses_a");
    const subscription = relay.subscribe(subject);
    const statuses = relay.status()[Symbol.asyncIterator]();
    await relay.flush();
    await relay.drain();
    expect(relay.isClosed()).toBe(true);
    expect(server.handled.filter((one) => one.frame.startsWith("unsub "))).toHaveLength(1);
    expect((await subscription[Symbol.asyncIterator]().next()).done).toBe(true);
    expect((await statuses.next()).done).toBe(true);
  });
});

// The wait before each open again (`relayReopenDelay`), by how the last open ended and how many
// of its run (`relayRun`) came before it with no socket open a minute between: no clock read.
describe("how long a relay socket waits before it opens again", () => {
  const cases: readonly (readonly [string, RelayEnd, readonly number[]])[] = [
    ["closed 4401", { closed: 4401 }, [0, 0, 0]],
    ["closed 1012", { closed: 1012 }, [0, 0, 0]],
    ["closed 4403", { closed: 4403 }, [1_000, 2_000, 4_000]],
    ["closed 1013", { closed: 1013 }, [5_000, 10_000, 20_000]],
    ["closed 1001", { closed: 1001 }, [0, 1_000, 2_000]],
    ["closed 1006", { closed: 1006 }, [0, 1_000, 2_000]],
    ["closed 1008", { closed: 1008 }, [1_000, 2_000, 4_000]],
    ["refused 401", { refused: 401, after: undefined }, [1_000, 2_000, 4_000]],
    ["refused 403", { refused: 403, after: 4401 }, [1_000, 2_000, 4_000]],
    ["refused 503 after a 1013", { refused: 503, after: 1013 }, [5_000, 10_000, 20_000]],
    ["refused 503 after a 1012", { refused: 503, after: 1012 }, [1_000, 2_000, 4_000]],
    ["refused 429", { refused: 429, after: undefined }, [1_000, 2_000, 4_000]],
    ["never answered", { refused: undefined, after: 1012 }, [1_000, 2_000, 4_000]],
  ];

  test("each cause waits as its run says, doubling with each end of that run", () => {
    expect(
      cases.map(([name, end]) => [name, [0, 1, 2].map((streak) => relayReopenDelay(end, streak))])
    ).toEqual(cases.map(([name, , waits]) => [name, [...waits]]));
  });

  test("no wait passes a minute", () => {
    expect(cases.map(([name, end]) => [name, relayReopenDelay(end, 20)])).toEqual(
      cases.map(([name, , waits]) => [name, waits[2] === 0 ? 0 : 60_000])
    );
  });

  test("the token's causes share one run, the broker's another, and each other close its own", () => {
    expect(cases.map(([name, end]) => [name, relayRun(end)])).toEqual([
      ["closed 4401", "closed 4401"],
      ["closed 1012", "closed 1012"],
      ["closed 4403", "token"],
      ["closed 1013", "broker"],
      ["closed 1001", "closed 1001"],
      ["closed 1006", "closed 1006"],
      ["closed 1008", "closed 1008"],
      ["refused 401", "token"],
      ["refused 403", "token"],
      ["refused 503 after a 1013", "broker"],
      ["refused 503 after a 1012", "unopened"],
      ["refused 429", "unopened"],
      ["never answered", "unopened"],
    ]);
  });
});
