// The extension's consumer-delivery rows on the relay itself: the relay subject the session opens
// before it registers, each frame's answers and their order, the resume point and the follows the
// session file carries, the session's only subscriptions and its connection probe, a reconnect's
// new epoch, a gap, and a relayed subscription removal. The rows on the outage report are in
// `consumer-delivery-outage.test.ts`, and the rows on the card a frame becomes are in
// `consumer-delivery-cards.test.ts`. Each of the three files has the `describe` "consumer
// delivery". They are apart from `envoy.test.ts` and from each other so that each file holds the
// rows of one subject and stays under 1,000 lines.
// They boot one session (`../test/consumer-delivery-session`) on one harness
// (`../test/extension-harness`).
import { describe, expect } from "bun:test";
import { legacyDispatchIssueSubject } from "@legion/contracts";
import { deliverSubject, RelayHeader } from "@legion/envoy-client/consumer-delivery";
import { until } from "@legion/test-fixtures/wait";
import { logger } from "@oh-my-pi/pi-utils";
import { REPLAY_STATE_ENTRY } from "../src/missed-messages";
import {
  bootConsumer,
  peer,
  RELAY,
  removal,
  SESSION,
  STREAM,
} from "../test/consumer-delivery-session";
import type { EnvoyModule } from "../test/envoy-module";
import {
  createPi,
  onPassClock,
  rowProbe,
  rowSessionShutdown,
  sessionContext,
  settle,
  useExtensionHarness,
} from "../test/extension-harness";
import {
  busOutageAnswer,
  natsState,
  response,
  responseWithRegistration,
  type SubscriptionControls,
  test,
} from "../test/listener-stand-in";

useExtensionHarness();

describe("consumer delivery", () => {
  test("subscribes the relay subject before it registers, and every registration asks for consumer delivery", async () => {
    const session = await bootConsumer("consumer-registration");

    const [first] = session.registrations;
    expect(first?.relaySubscribed).toBe(true);
    expect(first?.delivery?.consumer).toBe(true);
    expect(first?.delivery?.epoch).toEqual(expect.any(String));
    expect(first?.delivery?.resume).toBeUndefined();

    const inbox = session.relay(7, peer("registration"));
    await until(
      "the frame's done",
      rowProbe(() => session.replies(inbox).includes("done"))
    );
    await session.tick();

    const renewed = session.registrations.at(-1);
    expect(renewed?.delivery?.epoch).toBe(first?.delivery?.epoch ?? "");
    expect(renewed?.delivery?.resume).toEqual({ stream_id: STREAM, seq: 7 });
  });

  test("a relay subject whose connection closes under its round trip is closed, and the next tick opens it", async () => {
    // `retryRelay` bails while `owner.subscription` is set and `pumpRelay` starts no retry for a
    // subscription whose owner no longer names it, so an open that gave up and left the field set
    // would have neither the reconcile pass nor `retryRelay` reopen the relay: it would stay open,
    // unregistered and unrelayed until the pump's own end path cleared the field. Only a
    // connection closed for good makes the open give up; a rejection on a connection that is not
    // closed leaves it to go on.
    natsState.failFlushClosesConnection = true;
    natsState.failFlushOnSubject = RELAY;
    const session = await bootConsumer("relay-flush-fails", { withoutRelay: true });
    natsState.failFlushClosesConnection = false;
    try {
      expect(natsState.controls.get(RELAY)?.active() ?? false).toBe(false);
      // With no relay subject there is no consumer to ask for, so the pass registers nothing at
      // all rather than registering a session the listener could deliver nothing to.
      expect(session.registrations).toEqual([]);

      await session.tick();
      expect(natsState.controls.get(RELAY)?.active()).toBe(true);
      expect(session.registrations.at(-1)?.relaySubscribed).toBe(true);
      expect(session.registrations.at(-1)?.delivery?.consumer).toBe(true);
    } finally {
      // A pump retry or a background confirmation left running would wake inside a later row and
      // write to the shared mock.
      await session.stop();
    }
  });

  test("answers received before any host work and done only once each card is written, in stream order", async () => {
    const session = await bootConsumer("consumer-order", { turn: "streaming" });

    // The row reads each frame's replies whole, and a frame the session holds is sent `progress`
    // every 20 s (`RELAY_PROGRESS_INTERVAL_MS`) until it is answered, on an interval of this
    // process that `createConsumerDelivery` arms as it receives the frame. On the real clock the
    // three frames, held from their relay to the take, raced that interval with the row's work,
    // and a host slow enough put a `progress` in a list the row reads. So the frames are relayed
    // on the test's clock (`onPassClock`), as in the BTW row below, where each interval is armed
    // and ends only once the clock moves, which it never does here. Nothing the row waits for
    // needs it to: the relay subject stays open, so no redial or resubscribe delay comes into it,
    // the host writes each card after a microtask, and the stand-in listener answers each call at
    // once. Everything between runs in process, on real turns, so each wait holds the clock for
    // its count of turns (`hold`, `run`). The boot runs before, on the real clock: it holds no
    // frame, so it arms no interval. Most consumer rows boot so; the refused-registration rows
    // boot on the test's clock instead.
    await onPassClock(async (clock) => {
      const inboxes = [1, 2, 3].map((seq) => session.relay(seq, peer(`order-${seq}`)));
      await clock.hold("the third frame's card", () => session.cards("evt-order-3").length > 0);

      for (const [index, inbox] of inboxes.entries()) {
        expect(session.replies(inbox)).toEqual(["received"]);
        expect(session.events.indexOf(`${inbox} received`)).toBeLessThan(
          session.events.indexOf(`handed evt-order-${index + 1}`)
        );
      }
      expect(
        session.events.filter((event) => event.startsWith("handed ")).map((event) => event.slice(7))
      ).toEqual(["evt-order-1", "evt-order-2", "evt-order-3"]);

      await clock.run("the take", session.take());

      for (const [index, inbox] of inboxes.entries()) {
        expect(session.replies(inbox)).toEqual(["received", "done"]);
        expect(session.events.indexOf(`written evt-order-${index + 1}`)).toBeLessThan(
          session.events.indexOf(`${inbox} done`)
        );
      }
    });
    // What can hang is the boot's wait for its first registration (`bootConsumer`, on the real
    // clock), which ends at the row's deadline; every wait on the test's clock ends by its own
    // count of turns (`onPassClock`).
  });

  test("a frame written before a restart and relayed again after it is answered done and not shown again", async () => {
    const first = await bootConsumer("consumer-restart-a");
    const frame = peer("restart");
    const before = first.relay(4, frame);
    await until(
      "the first copy's done",
      rowProbe(() => first.replies(before).includes("done"))
    );
    await first.stop();

    const second = await bootConsumer("consumer-restart-b", { entries: first.entries() });
    expect(second.registrations[0]?.delivery?.resume).toEqual({ stream_id: STREAM, seq: 4 });
    const again = second.relay(4, frame, { delivered: 2 });
    await until(
      "the second copy's done",
      rowProbe(() => second.replies(again).includes("done"))
    );

    expect(second.replies(again)).toEqual(["received", "done"]);
    expect(first.cards("evt-restart")).toHaveLength(1);
    expect(second.cards("evt-restart")).toHaveLength(0);
  });

  test("the session file carries the follows map only when it changes, and either shape restores", async () => {
    const issueTopic = legacyDispatchIssueSubject("INFRA-7", ">");

    const first = await bootConsumer("consumer-follows-a");
    await first.follow(issueTopic);
    await settle();
    const inbox = first.relay(4, peer("follows"));
    await until(
      "the frame's done",
      rowProbe(() => first.replies(inbox).includes("done"))
    );

    const states = first.fixture.entries
      .filter((entry) => entry.customType === "envoy-consumer-state")
      .map((entry) => entry.data as Record<string, unknown>);
    // The follow wrote the map; the written frame wrote its resume point alone beside it.
    expect(states.filter((state) => "follows" in state)).toHaveLength(1);
    expect(states.at(-1)).toEqual({ session_id: SESSION, resume: { stream_id: STREAM, seq: 4 } });
    await first.stop();

    // A session file written before the entry was split carries the whole snapshot in one entry.
    const whole = {
      type: "custom",
      customType: "envoy-consumer-state",
      data: { session_id: SESSION, resume: { stream_id: STREAM, seq: 9 }, follows: {} },
    };
    const second = await bootConsumer("consumer-follows-b", { entries: [whole] });
    expect(second.registrations[0]?.delivery?.resume).toEqual({ stream_id: STREAM, seq: 9 });
    await second.stop();

    // The split entries restore the same snapshot: the newest resume point, and the follow times
    // of the newest entry that carried them, so following the topic again writes nothing.
    const third = await bootConsumer("consumer-follows-c", { entries: first.entries() });
    expect(third.registrations[0]?.delivery?.resume).toEqual({ stream_id: STREAM, seq: 4 });
    await third.follow(issueTopic);
    await settle();
    expect(
      third.fixture.entries
        .filter((entry) => entry.customType === "envoy-consumer-state")
        .map((entry) => entry.data as Record<string, unknown>)
        .filter((state) => "follows" in state)
    ).toEqual([]);
  });

  // What main's INFRA-812 row proved of the replay engine's state, carried onto the ledger that
  // replaced it: a state entry carries the whole of this session's state - the topics it follows
  // - and a session restores the newest state its file holds as its own, whatever session wrote
  // it. So an entry appended once the host has moved would make the conversation the host moved
  // to follow this session's topics. The guard is the ledger's own bind (`transcriptSessionID`,
  // which answers nothing once the host's session is not this one), and the first follow below
  // is the control: it shows the ledger does write its state here when the host is on this
  // session (dispatch://INFRA-812).
  test("appends no ledger state of this session to the conversation the host moved to", async () => {
    const kept = legacyDispatchIssueSubject("INFRA-812", ">");
    let live = SESSION;
    const session = await bootConsumer("consumer-state-not-carried", {
      hostSessionID: () => live,
    });
    const ledgerStates = () =>
      session.fixture.entries.filter((entry) => entry.customType === REPLAY_STATE_ENTRY);

    await session.follow(kept);
    await settle();
    expect(JSON.stringify(ledgerStates())).toContain(kept);

    // The host is on another conversation from here on, with no handler fired for the move, so
    // every entry written now lands in that conversation's file.
    const mark = session.fixture.entries.length;
    live = "ses_after_state";
    await session.follow("notifications.team.moved-to");
    await settle();

    // The follow itself was accepted, so the absence below is the move's doing rather than a
    // topic the listener refused.
    expect(session.registrations.at(-1)?.topics).toContain("notifications.team.moved-to");
    const appended = session.fixture.entries.slice(mark);
    expect(appended.filter((entry) => entry.customType === REPLAY_STATE_ENTRY)).toEqual([]);
    expect(JSON.stringify(appended)).not.toContain(kept);
    expect(JSON.stringify(appended)).not.toContain("notifications.team.moved-to");
  });

  // The acceptance line of dispatch://INFRA-771: no Envoy session receives from a plain NATS
  // subscription. The whole set is asserted, never the presence of the two, so a core
  // subscription added back on any followed topic fails here.
  test("holds exactly its relay subject and its agent-stream control subject, and no other", async () => {
    const session = await bootConsumer("consumer-subscription-set");
    await session.follow(legacyDispatchIssueSubject("INFRA-771", ">"));
    await session.follow("notifications.team.alpha");
    await settle();
    await session.tick();

    expect(session.registrations.at(-1)?.topics).toContain("notifications.team.alpha");
    expect(session.subscriptions()).toEqual([`agentstream.${SESSION}.control`, RELAY]);
  });

  // The listener's connection check (INFRA-626) is a core request on the session's own relay
  // subject that carries `Envoy-Probe: 1` and nothing else: an answer, or a second of silence,
  // reads connected, and the no-responders a server gives a subject nothing subscribes reads
  // stopped. The probe is no frame, so it reaches no card and moves no resume point, and the
  // session holds no subscription for it beyond the relay subject the set above pins.
  test("a connection probe on the relay subject is answered, follows a session-id change, and the relay subscription closes when the session moves or shuts down", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?connection-probe");
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    let inboxes = 0;
    /** Probes `controls` as the listener does; answers what the session replied on the inbox. */
    const probe = async (
      controls: SubscriptionControls | undefined
    ): Promise<readonly string[]> => {
      if (controls === undefined) throw new Error("that relay subject was never subscribed");
      inboxes += 1;
      const inbox = `_INBOX.probe.${inboxes}`;
      controls.push("", inbox, { [RelayHeader.probe]: "1" });
      await settle();
      return natsState.published
        .filter((published) => published.subject === inbox)
        .map((published) => new TextDecoder().decode(published.data));
    };
    const consumerStates = () =>
      JSON.stringify(
        fixture.entries.filter((entry) => entry.customType === "envoy-consumer-state")
      );
    const cards = () =>
      fixture.deliveries.filter((delivery) => delivery.customType === "envoy-message");
    const first = "ses_probe_first";
    const next = "ses_probe_next";
    const firstContext = sessionContext(first);
    const stop = rowSessionShutdown(fixture, sessionContext(next));

    await fixture.handlers.get("session_start")?.({}, firstContext);
    await until(
      "the first session's relay subject",
      rowProbe(() => natsState.controls.get(deliverSubject(first))?.active() === true)
    );
    const firstRelay = natsState.controls.get(deliverSubject(first));
    const states = consumerStates();
    expect(await probe(firstRelay)).toEqual(["done"]);
    expect(cards()).toEqual([]);
    expect(consumerStates()).toBe(states);

    // A switch moves the relay subject to the new id: a probe of the id left behind meets no
    // subscriber, which a server answers no-responders, and the new id is answered.
    await fixture.handlers.get("session_switch")?.({}, sessionContext(next));
    await until(
      "the new session's relay subject",
      rowProbe(() => natsState.controls.get(deliverSubject(next))?.active() === true)
    );
    // The switch tells the session its id changed; that notice is the one card it is shown.
    const shown = cards().length;
    expect(firstRelay?.active()).toBe(false);
    const nextRelay = natsState.controls.get(deliverSubject(next));
    expect(await probe(nextRelay)).toEqual(["done"]);

    // The connection drops: nats.js ends the subscription.
    nextRelay?.end();
    await settle();
    // The session subscribes its relay subject again, and is answered again.
    await until(
      "the relay subject subscribed again",
      rowProbe(() => natsState.controls.get(deliverSubject(next)) !== nextRelay)
    );
    const reopened = natsState.controls.get(deliverSubject(next));
    expect(await probe(reopened)).toEqual(["done"]);

    // The session shuts down: its relay subject closes with its connection.
    await stop();
    expect(reopened?.active()).toBe(false);
    expect(cards()).toHaveLength(shown);
  });

  test("a NATS reconnect begins a new epoch and registers it at once", async () => {
    const session = await bootConsumer("consumer-reconnect");
    const before = session.registrations.at(-1)?.delivery?.epoch;

    await session.reconnect();

    const after = session.registrations.at(-1)?.delivery?.epoch;
    expect(after).toEqual(expect.any(String));
    expect(after).not.toBe(before);
  });

  test("a gap the registration answers is a notice held until the host is settled, held again after a restart", async () => {
    const gapNotices = (deliveries: readonly { readonly content: string }[]) =>
      deliveries.filter((delivery) => delivery.content.includes("no longer holds some messages"));
    const first = await bootConsumer("consumer-gap-a", { gap: true, turn: "streaming" });
    expect(gapNotices(first.fixture.deliveries)).toHaveLength(0);
    await first.stop();

    const second = await bootConsumer("consumer-gap-b", { entries: first.entries() });
    await second.tick();

    expect(gapNotices(second.fixture.deliveries)).toHaveLength(1);
    await second.tick();
    expect(gapNotices(second.fixture.deliveries)).toHaveLength(1);
  });

  test("a relayed subscription removal registers the session again without the topic, and stops taking its frames", async () => {
    const issueTopic = legacyDispatchIssueSubject("INFRA-9", ">");
    const issueSubject = legacyDispatchIssueSubject("INFRA-9", "issue.updated");
    const session = await bootConsumer("consumer-removal");
    await session.follow(issueTopic);
    await settle();
    expect(session.registrations.at(-1)?.topics).toContain(issueTopic);
    const followed = session.registrations.length;

    session.relay(1, removal("INFRA-9", issueTopic));
    await until(
      "the registration without it",
      rowProbe(() => session.registrations.length > followed)
    );
    expect(session.registrations.at(-1)?.topics).not.toContain(issueTopic);

    const late = JSON.stringify({ ...JSON.parse(peer("after-removal")), topic: issueSubject });
    const inbox = session.relay(2, late, { subject: issueSubject });
    await until(
      "the unfollowed frame's done",
      rowProbe(() => session.replies(inbox).includes("done"))
    );
    expect(session.cards("evt-after-removal")).toHaveLength(0);
  });

  // The listener answers its bus-outage 503 also after it wrote the record, when only the filter
  // update met a NATS reconnect.
  test("a relayed removal that met the bus-outage 503 is warned as that outage, and any other failure as not taken out", async () => {
    const notTakenOut = "envoy: a subscription removal was not taken out of the registry";
    const rows = [
      {
        answer: () => busOutageAnswer(503),
        warned:
          "envoy: the listener's message bus did not answer a request to take the topics of a subscription removal out of the registry. The registry may already be without them. The session's next registration sets its consumer's filter to the topics the registry holds.",
      },
      { answer: () => busOutageAnswer(500), warned: notTakenOut },
      {
        answer: (): Response => {
          throw new TypeError("fetch failed");
        },
        warned: notTakenOut,
      },
    ];
    let answer = () => response({});
    const warnings: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn" && entry.message.includes("subscription removal")) {
        warnings.push(entry.message);
      }
    });
    try {
      const session = await bootConsumer("consumer-removal-unanswered", {
        unsubscribe: () => answer(),
      });
      for (const [index, row] of rows.entries()) {
        const issue = `INFRA-3${index}`;
        const topic = legacyDispatchIssueSubject(issue, ">");
        await session.follow(topic);
        await settle();
        answer = row.answer;
        session.relay(index + 1, removal(issue, topic));
        await until(
          `the warning about ${issue}'s removal`,
          rowProbe(() => warnings.length > index)
        );
        expect(warnings[index]).toBe(row.warned);
      }
    } finally {
      stopSink();
    }
  });
});
