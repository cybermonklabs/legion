// The extension's consumer-delivery rows on the outage report: a registration the listener
// refuses, a relay subject that opens late at the start or not at all, a GitHub outage of an edge
// listener, the bus-outage 503, and a stop while a registration is on the wire. The relay's own
// rows are in `consumer-delivery.test.ts`, which says why the three files are apart. Each has the
// `describe` "consumer delivery", and they boot one session (`../test/consumer-delivery-session`)
// on one harness (`../test/extension-harness`).
import { describe, expect, setSystemTime, test } from "bun:test";
import { BUS_UNANSWERED_PREFIX } from "@legion/contracts";
import { INBOUND_FIRST_OPEN_BOUND_MS } from "@legion/envoy-client/inbound-outage";
import { logger } from "@oh-my-pi/pi-utils";
import {
  bootConsumer,
  peer,
  RELAY,
  type Registration,
  SESSION,
} from "../test/consumer-delivery-session";
import { settle, until, useExtensionHarness } from "../test/extension-harness";
import { natsState } from "../test/listener-stand-in";

useExtensionHarness();

/** What a restored card says of a session that received nothing before it. Row (e) of the pane
 *  and the log finds it in no card after a bus-outage 503; the row of a relay open that still
 *  fails past the bound finds it in the card after that outage, which left the session nothing. */
const RECEIVED_NOTHING = /receive[sd]? nothing/;

describe("consumer delivery", () => {
  // A refusal is the whole of a session's inbound: nothing else reaches it, so the session must
  // say so and keep asking rather than run on quietly deaf.
  for (const refusal of [
    { status: 503, reason: "no_grant" },
    { status: 409, reason: "not_requested" },
    // A consumer filter past the server's max payload, which the listener answers 413: the
    // session is told the same way.
    { status: 413, reason: "too_large" },
  ] as const) {
    test(`a registration refused ${refusal.status} ${refusal.reason} leaves the session running with a notice naming it, the next heartbeat asks again, and the accepted registration is told once`, async () => {
      const session = await bootConsumer(`consumer-refused-${refusal.reason}`, {
        refuse: refusal,
      });

      const told = session.fixture.deliveries.filter((delivery) =>
        delivery.content.includes("Envoy inbound unavailable")
      );
      expect(told).toHaveLength(1);
      expect(told[0]?.content).toContain(refusal.reason);
      // The session runs on: its tools are registered and its relay subject stays open, so the
      // consumer a later registration is given has somewhere to land.
      expect(session.fixture.tools.map((tool) => tool.name)).toContain("envoy_send");
      expect(session.subscriptions()).toEqual([`agentstream.${SESSION}.control`, RELAY]);

      session.accept();
      await session.tick();
      // The accepted registration tells the session once that it receives again.
      expect(session.cards("Envoy inbound restored")).toHaveLength(1);
      const inbox = session.relay(3, peer(`after-${refusal.reason}`));
      await until(() => session.replies(inbox).includes("done"), "the frame's done");

      expect(session.cards(`evt-after-${refusal.reason}`)).toHaveLength(1);
      await session.tick();
      expect(session.cards("Envoy inbound restored")).toHaveLength(1);
    });
  }

  // At a pod's start the first NATS dial can fail, and a later try opens the relay (R24 of
  // dispatch://INFRA-3589). Until the session's first accepted registration, an outage with no
  // relay subject is held for the start race's bound (`INBOUND_FIRST_OPEN_BOUND_MS`, two
  // minutes): a session whose relay opens inside it is told nothing, and one whose relay is still
  // closed past it is told once, then once that it receives again. A refusal is told at once.
  test("a session whose NATS connection opens a few seconds after its start shows no inbound-unavailable notice and receives a message sent to it", async () => {
    natsState.failConnects = 99;
    const session = await bootConsumer("consumer-start-race", { withoutRelay: true });
    const start = Date.now();
    try {
      // The background retry 15 s after the start finds NATS still closed.
      setSystemTime(new Date(start + 15_000));
      await session.tickWithoutNats();
      // NATS opens before the retry 30 s after the start.
      natsState.failConnects = 0;
      setSystemTime(new Date(start + 30_000));
      await session.tick();
      const inbox = session.relay(1, peer("start-race"));
      await until(() => session.replies(inbox).includes("done"), "the frame's done");

      expect(session.cards("evt-start-race")).toHaveLength(1);
      expect(session.cards("Envoy inbound unavailable")).toEqual([]);
      expect(
        session.notifications.filter((message) =>
          message.includes("inbound messages are unavailable")
        )
      ).toEqual([]);
      // The session was told nothing, so it is told nothing about receiving again either.
      expect(session.cards("Envoy inbound restored")).toEqual([]);
    } finally {
      setSystemTime();
      await session.stop();
    }
  });

  test("a session whose relay open still fails past the bound is told once, then told once that its inbound is back, and the message stored meanwhile arrives", async () => {
    natsState.failConnects = 99;
    const session = await bootConsumer("consumer-start-outage", { withoutRelay: true });
    const start = Date.now();
    const pastBound = start + INBOUND_FIRST_OPEN_BOUND_MS + 1_000;
    try {
      setSystemTime(new Date(pastBound));
      await session.tickWithoutNats();
      expect(session.cards("Envoy inbound unavailable")).toHaveLength(1);
      // A later tick of the same outage tells nothing more.
      setSystemTime(new Date(pastBound + 15_000));
      await session.tickWithoutNats();
      expect(session.cards("Envoy inbound unavailable")).toHaveLength(1);
      expect(
        session.notifications.filter((message) =>
          message.includes("inbound messages are unavailable")
        )
      ).toHaveLength(1);

      // NATS opens, and the next tick's registration is accepted.
      natsState.failConnects = 0;
      setSystemTime(new Date(pastBound + 30_000));
      await session.tick();
      const restored = session.cards("Envoy inbound restored");
      expect(restored).toHaveLength(1);
      // The control of row (e)'s absence below: this outage left the session nothing, so its
      // restored card says that the session received nothing.
      expect(restored[0]?.content).toMatch(RECEIVED_NOTHING);
      expect(
        session.notifications.filter((message) => message.includes("inbound messages are back"))
      ).toHaveLength(1);
      // What the stream stored while the session received nothing arrives.
      const inbox = session.relay(4, peer("stored-meanwhile"), {
        storedAt: new Date(pastBound + 5_000),
      });
      await until(() => session.replies(inbox).includes("done"), "the frame's done");
      expect(session.cards("evt-stored-meanwhile")).toHaveLength(1);

      // A later accepted registration tells nothing more.
      await session.tick();
      expect(session.cards("Envoy inbound restored")).toHaveLength(1);
      expect(session.cards("Envoy inbound unavailable")).toHaveLength(1);
    } finally {
      setSystemTime();
      await session.stop();
    }
  });

  // The log does not wait for the start race: the first registration that found no relay subject
  // is logged at once, at warn (`held`), so the log of a start whose relay never opens says so
  // long before the bound. The line says what that registration found, and why, and makes no
  // claim about what the session receives. It is written once per outage, not at every try. The
  // notice, its card, its pane note and the warning that the session receives nothing wait for
  // the bound.
  test("the first registration that finds no relay subject is logged at once at warn, a later one of the same outage logs nothing more, and the notice, its card and its pane note wait for the bound", async () => {
    const logged: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.message.startsWith("envoy: ")) logged.push(`${entry.level} ${entry.message}`);
    });
    natsState.failConnects = 99;
    const session = await bootConsumer("consumer-start-race-logged", { withoutRelay: true });
    const start = Date.now();
    const found = () => logged.filter((line) => line.includes("holds no relay subject"));
    const told = () => ({
      cards: session.cards("Envoy inbound unavailable").length,
      notes: session.notifications.filter((message) =>
        message.includes("inbound messages are unavailable")
      ).length,
    });
    try {
      // The start's pass found NATS closed, so its registration found no relay subject.
      await until(() => found().length > 0, "the log line");
      expect(found()).toEqual([
        "warn envoy: a registration found that this session holds no relay subject, because its relay subject could not be opened: CONNECTION_REFUSED",
      ]);
      expect(logged.filter((line) => RECEIVED_NOTHING.test(line))).toEqual([]);
      expect(told()).toEqual({ cards: 0, notes: 0 });

      // A later registration of the same outage, inside the bound, logs nothing more.
      setSystemTime(new Date(start + 15_000));
      await session.tickWithoutNats();
      expect(found()).toHaveLength(1);
      expect(told()).toEqual({ cards: 0, notes: 0 });

      // Past the bound the session is told once: the notice's card, its pane note, and its own
      // log line.
      setSystemTime(new Date(start + INBOUND_FIRST_OPEN_BOUND_MS + 1_000));
      await session.tickWithoutNats();
      expect(told()).toEqual({ cards: 1, notes: 1 });
      expect(
        logged.filter((line) => line.startsWith("warn envoy: this session receives nothing: "))
      ).toHaveLength(1);
      expect(
        logged.filter((line) => line.startsWith("warn envoy: a registration found that "))
      ).toHaveLength(1);
    } finally {
      setSystemTime();
      await session.stop();
      stopSink();
    }
  });

  test("a registration the listener refuses is told at once", async () => {
    const session = await bootConsumer("consumer-refused-at-once", {
      refuse: { status: 503, reason: "no_grant" },
    });

    // The relay was open, so the start race's bound holds nothing back: no clock moves here.
    expect(session.registrations[0]?.relaySubscribed).toBe(true);
    const told = session.cards("Envoy inbound unavailable");
    expect(told).toHaveLength(1);
    expect(told[0]?.content).toContain("no_grant");
    // The control of row (e)'s absences below: a refusal leaves the session no consumer, and the
    // card says so.
    expect(told[0]?.content).toContain("no durable consumer");
    expect(told[0]?.content).toContain("receives none");
    expect(
      session.notifications.filter((message) =>
        message.includes("inbound messages are unavailable (")
      )
    ).toHaveLength(1);
  });

  // R24 of dispatch://INFRA-3589, after main's #4810: an edge listener that could not read the
  // repositories of the session's projects refuses the registration 503 `repositories_unknown`,
  // and it withholds the GitHub messages. Where it already held the session's registration, it
  // kept that registration and the consumer; where it held none, the session receives nothing.
  // The client cannot tell which, so the card, the pane note and the log line say both, at once,
  // and none says outright that the session receives nothing. The accepted registration after it
  // says that the listener follows the GitHub topics again, and none says that the session
  // receives again or that its inbound messages are back.
  test("a 503 repositories_unknown is told at once as GitHub topics withheld in the card, the pane and the log, each with what the session receives where the listener held its registration and where it held none, and the accepted registration after it as GitHub topics followed again", async () => {
    const logged: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.message.startsWith("envoy: ")) logged.push(`${entry.level} ${entry.message}`);
    });
    const error =
      "the repositories of PRIV's issues could not be read, so no GitHub topic is followed now; register again shortly";
    const why = `${error} (reason: repositories_unknown)`;
    const session = await bootConsumer("consumer-repositories-unknown", {
      refuse: { status: 503, reason: "repositories_unknown", error },
    });
    const claims = /receive[sd]? (nothing|again)|inbound messages are/;
    try {
      const told = session.cards("Envoy GitHub topics withheld");
      expect(told).toHaveLength(1);
      expect(told[0]?.content).toContain(why);
      expect(told[0]?.content).toContain(
        "If the listener already held a registration of this session, it kept that registration and the session's consumer, so the session receives the messages of every other topic the listener follows for it"
      );
      expect(told[0]?.content).toContain(
        "If the listener held no registration of this session, the session receives nothing until a registration is accepted."
      );
      expect(session.cards("Envoy inbound unavailable")).toEqual([]);
      expect(session.notifications).toContain(
        `envoy: GitHub messages are withheld (${why}); other messages still arrive if the listener already held this session's registration, and none arrive until a registration is accepted if it held none; registering again every heartbeat`
      );
      expect(logged).toContain(
        `warn envoy: the listener follows none of this session's GitHub topics now; the session's other messages still reach it if the listener already held its registration, and none reach it until a registration is accepted if it held none: ${why}`
      );

      session.accept();
      await session.tick();
      const restored = session.cards("Envoy GitHub topics followed again");
      expect(restored).toHaveLength(1);
      expect(restored[0]?.content).toContain("it follows this session's GitHub topics again");
      expect(session.cards("Envoy inbound restored")).toEqual([]);
      expect(session.notifications).toContain(
        "envoy: the listener follows this session's GitHub topics again"
      );
      expect(logged).toContain(
        "info envoy: the listener follows this session's GitHub topics again: a registration was accepted"
      );
      expect(session.notifications.filter((message) => claims.test(message))).toEqual([]);
      expect(logged.filter((line) => claims.test(line))).toEqual([]);
    } finally {
      await session.stop();
      stopSink();
    }
  });

  // Row (e) of check l23, for this plugin (R24 of dispatch://INFRA-3589, against R26): the
  // listener answers its bus-outage 503 to a registration whose known-session record did not
  // land, after it made the session's consumer, so what the stream stores still reaches the
  // session. Such a refusal is held as the start race is: no notice inside the bound, and a
  // registration accepted inside it clears it with none. Past the bound the session is told once
  // that its registration is not recorded for certain, and never that it has no consumer.
  const UNRECORDED = `${BUS_UNANSWERED_PREFIX}: write the known-session record of ${SESSION}: context deadline exceeded`;

  /** One heartbeat tick whose registration the listener refuses: envoy-client's transport sends
   *  a 5xx once more, so the tick is done once both attempts reached the listener. */
  async function refusedTick(session: {
    readonly registrations: readonly Registration[];
    readonly tick: () => Promise<void>;
  }): Promise<void> {
    const before = session.registrations.length;
    await session.tick();
    await until(() => session.registrations.length >= before + 2, "the tick's second attempt");
    await settle();
  }

  test("(e) a registration answered with the bus-outage 503 shows no notice inside the bound, the session still receives, and an accepted registration clears it with none", async () => {
    const session = await bootConsumer("consumer-unrecorded-in-bound", {
      refuse: { status: 503, error: UNRECORDED },
    });
    const start = Date.now();
    try {
      await until(() => session.registrations.length >= 2, "the first registration's retry");
      await settle();
      expect(session.cards("Envoy inbound unavailable")).toEqual([]);
      // The listener made the consumer before the record's write, so a stored message arrives.
      const inbox = session.relay(1, peer("unrecorded"));
      await until(() => session.replies(inbox).includes("done"), "the frame's done");
      expect(session.cards("evt-unrecorded")).toHaveLength(1);

      setSystemTime(new Date(start + 15_000));
      await refusedTick(session);
      expect(session.cards("Envoy inbound unavailable")).toEqual([]);

      session.accept();
      setSystemTime(new Date(start + 30_000));
      await session.tick();
      expect(session.cards("Envoy inbound unavailable")).toEqual([]);
      expect(session.cards("Envoy inbound restored")).toEqual([]);
      expect(
        session.notifications.filter((message) => message.includes("inbound messages are"))
      ).toEqual([]);
    } finally {
      setSystemTime();
      await session.stop();
    }
  });

  test("(e) past the bound a bus-outage 503 is told once, as a registration not recorded for certain, and the accepted registration after it says inbound is back", async () => {
    const session = await bootConsumer("consumer-unrecorded-past-bound", {
      refuse: { status: 503, error: UNRECORDED },
    });
    const start = Date.now();
    try {
      await until(() => session.registrations.length >= 2, "the first registration's retry");
      await settle();
      setSystemTime(new Date(start + INBOUND_FIRST_OPEN_BOUND_MS + 1_000));
      await refusedTick(session);
      setSystemTime(new Date(start + INBOUND_FIRST_OPEN_BOUND_MS + 16_000));
      await refusedTick(session);
      const told = session.cards("Envoy inbound unavailable");
      expect(told).toHaveLength(1);
      expect(told[0]?.content).toContain("write the known-session record");
      expect(told[0]?.content).toMatch(/not record/i);
      expect(told[0]?.content).not.toContain("no durable consumer");
      expect(told[0]?.content).not.toContain("receives none");

      session.accept();
      setSystemTime(new Date(start + INBOUND_FIRST_OPEN_BOUND_MS + 31_000));
      await session.tick();
      expect(session.cards("Envoy inbound restored")).toHaveLength(1);
    } finally {
      setSystemTime();
      await session.stop();
    }
  });

  // The pane note and the log line of that episode say what the card says: the registration is
  // not recorded for certain, and then that the listener recorded one. None says that the session
  // receives nothing, that its inbound messages are unavailable, or that they are back.
  test("(e) the pane and the log tell a bus-outage 503 past the bound as a registration not recorded for certain, and the accepted registration after it as one the listener recorded", async () => {
    const logged: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.message.startsWith("envoy: ")) logged.push(`${entry.level} ${entry.message}`);
    });
    const session = await bootConsumer("consumer-unrecorded-lines", {
      refuse: { status: 503, error: UNRECORDED },
    });
    const start = Date.now();
    const claims = /receive[sd]? (nothing|again)|inbound messages are/;
    try {
      await until(() => session.registrations.length >= 2, "the first registration's retry");
      await settle();
      setSystemTime(new Date(start + INBOUND_FIRST_OPEN_BOUND_MS + 1_000));
      await refusedTick(session);
      expect(session.notifications).toContain(
        `envoy: this session's registration is not recorded for certain (${UNRECORDED}), and the session registers again at every heartbeat`
      );
      expect(logged).toContain(
        `warn envoy: this session's registration is not recorded for certain: ${UNRECORDED}`
      );

      session.accept();
      setSystemTime(new Date(start + INBOUND_FIRST_OPEN_BOUND_MS + 16_000));
      await session.tick();
      const restored = session.cards("Envoy inbound restored");
      expect(restored).toHaveLength(1);
      expect(restored[0]?.content).toContain(
        "The listener recorded a registration of this session"
      );
      expect(restored[0]?.content).not.toMatch(RECEIVED_NOTHING);
      expect(session.notifications).toContain(
        "envoy: the listener recorded a registration of this session"
      );
      expect(logged).toContain("info envoy: the listener recorded a registration of this session");
      expect(session.notifications.filter((message) => claims.test(message))).toEqual([]);
      expect(logged.filter((line) => claims.test(line))).toEqual([]);
    } finally {
      setSystemTime();
      await session.stop();
      stopSink();
    }
  });

  /**
   * A session whose listener holds a session event's registration on the wire (`release` lets it
   * go), with a heartbeat's pass queued behind it. `navigating` settles once the session event's
   * pass ends.
   */
  async function heartbeatBehindHeldRegistration(query: string) {
    const session = await bootConsumer(query);
    session.hold();
    const asked = session.registrations.length;
    const navigating = session.navigate();
    await until(() => session.registrations.length > asked, "the held registration");
    session.beat();
    await settle();
    return { session, navigating };
  }

  // Row (d) of check l23, for this plugin (R24 of dispatch://INFRA-3589): a session that stops
  // while a heartbeat's pass waits behind a registration on the wire is told nothing. Its stop
  // closes the relay on purpose and deletes the record next, so no registration runs once the
  // stop began (`sendRegistration`), and the pass that waited decides nothing and says nothing.
  // The control row after it is the same wait with no stop begun.
  test("(d) a session that stops while a heartbeat's registration waits behind one on the wire sends no registration after the stop began and is told nothing", async () => {
    const { session, navigating } = await heartbeatBehindHeldRegistration(
      "consumer-stop-behind-registration"
    );
    const notified = session.notifications.length;

    // The session stops: the stop closes the relay, then waits out the registration on the wire,
    // which the listener answers meanwhile.
    const stopping = session.stop();
    const sent = session.registrations.length;
    session.release();
    await stopping;
    await navigating;
    // The heartbeat's pass runs once the held one has ended.
    await settle();

    expect(session.registrations).toHaveLength(sent);
    for (const notice of ["Envoy inbound unavailable", "Envoy inbound restored"]) {
      expect(session.cards(notice)).toEqual([]);
      expect(session.heldNotices(notice)).toEqual([]);
    }
    expect(session.notifications.slice(notified)).toEqual([]);
  });

  test("control: with no stop begun, a heartbeat's pass that waits behind a registration on the wire registers once that one is answered", async () => {
    const { session, navigating } = await heartbeatBehindHeldRegistration(
      "consumer-beat-behind-registration"
    );
    try {
      const sent = session.registrations.length;
      session.release();
      await navigating;
      await until(() => session.registrations.length > sent, "the heartbeat's registration");
      await settle();

      expect(session.registrations).toHaveLength(sent + 1);
    } finally {
      await session.stop();
    }
  });

  // The outage report's own rule says that a stopping session is told nothing
  // (`inboundUnavailable`'s `stopping`), since the refusal of every registration once the stop
  // began does not reach this case: a registration already on the wire when the stop began is
  // answered during it, and its refusal says nothing about what a session whose record is deleted
  // next receives. A stopping host is never settled, so a notice raised then would be held
  // (`heldNotices`): the control row after it shows a notice held.
  test("a registration on the wire when the stop began and refused during it tells the session nothing", async () => {
    const session = await bootConsumer("consumer-stop-refused-on-wire");
    session.hold();
    const asked = session.registrations.length;
    const navigating = session.navigate();
    await until(() => session.registrations.length > asked, "the held registration");
    const notified = session.notifications.length;

    const stopping = session.stop();
    session.refuse({ status: 409, reason: "not_requested" });
    session.release();
    await stopping;
    await navigating;
    await settle();

    expect(session.cards("Envoy inbound unavailable")).toEqual([]);
    expect(session.heldNotices("Envoy inbound unavailable")).toEqual([]);
    expect(session.notifications.slice(notified)).toEqual([]);
  });

  // The control of the held notices the two rows above find none of: the same refusal, answered
  // while the host streams a turn and so is not settled, with no stop begun. The notice is held,
  // and recorded in the transcript, rather than handed as a card.
  test("control: a refusal told while the host is not settled holds its notice", async () => {
    const session = await bootConsumer("consumer-refused-unsettled", {
      refuse: { status: 409, reason: "not_requested" },
      turn: "streaming",
    });
    try {
      expect(session.heldNotices("Envoy inbound unavailable")).toHaveLength(1);
      expect(session.cards("Envoy inbound unavailable")).toEqual([]);
    } finally {
      await session.stop();
    }
  });
});
