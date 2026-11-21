// The extension's consumer-delivery rows. They have a file of their own so that `envoy.test.ts`
// stays below the size Biome reads a file up to (1 MiB by default). Both files share one harness
// (`../test/extension-harness`).
import { describe, expect, setSystemTime, test } from "bun:test";
import { createHash } from "node:crypto";
import { BUS_UNANSWERED_PREFIX, legacyDispatchIssueSubject } from "@legion/contracts";
import * as consumerDeliveryModule from "@legion/envoy-client/consumer-delivery";
import { deliverSubject, RelayHeader } from "@legion/envoy-client/consumer-delivery";
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { logger } from "@oh-my-pi/pi-utils";
import {
  REPLAY_DELIVERED_ENTRY,
  REPLAY_NOTICE_ENTRY,
  REPLAY_STATE_ENTRY,
} from "../src/missed-messages";
import {
  busOutageAnswer,
  createPi,
  ledgerSeam,
  natsState,
  pushNatsStatus,
  response,
  responseWithRegistration,
  type SubscriptionControls,
  sessionContext,
  settle,
  targetedDispatchEnvelope,
  until,
  useExtensionHarness,
} from "../test/extension-harness";
import type { SessionContext } from "../test/stand-in-host";

useExtensionHarness();

/** The Dispatch token a row hands the session in DISPATCH_TOKEN. */
const DISPATCH_TOKEN = fixtureSecret("dsp");

// A listener that answers `consumer` relays the session's durable consumer on
// `notifications.envoy.deliver.<id>`: each frame carries the stored envelope and its stream
// headers, with a reply inbox the session answers `received` on at decode and `done` once the
// frame is in the transcript (`@legion/envoy-client/consumer-delivery`).
describe("consumer delivery", () => {
  const SESSION = "ses_delivery";
  const AGENT = `notifications.agent.${SESSION}`;
  const RELAY = `notifications.envoy.deliver.${SESSION}`;
  const STREAM = "stream-1";

  /** A peer session's message to the session under test. */
  function peer(label: string): string {
    return JSON.stringify({
      event_id: `evt-${label}`,
      source: "agent",
      source_event_id: `source-${label}`,
      source_session: "ses_peer",
      topic: AGENT,
      dedupe_key: `agent.ses_peer.${createHash("sha256").update(label).digest("hex").slice(0, 32)}`,
      issued_at: 1,
      payload_summary: `peer message ${label}`,
      trace_id: `trace-${label}`,
    });
  }

  /** A person's removal of `topic` from this session's subscriptions, through Dispatch `issue`. */
  function removal(issue: string, topic: string): string {
    const by = { kind: "user", id: "alice" };
    return JSON.stringify({
      event_id: `dispatch-${issue}`,
      source: "dispatch",
      source_event_id: issue,
      topic: AGENT,
      dedupe_key: `dispatch-${issue}`,
      issued_at: 1,
      payload_summary: `unsubscribed from ${issue}`,
      trace_id: `trace-dispatch-${issue}`,
      payload: JSON.stringify({
        issue_key: issue,
        type: "subscription.removed",
        actor: by,
        notify: true,
        payload: { session_id: SESSION, by, topics: [topic] },
      }),
    });
  }

  interface Registration {
    readonly topics: readonly string[];
    readonly delivery?: {
      readonly consumer: boolean;
      readonly epoch: string;
      readonly resume?: { readonly stream_id: string; readonly seq: number };
    };
    /** Whether the relay subject was subscribed when the registration reached the listener. */
    readonly relaySubscribed: boolean;
  }

  interface SentCard {
    readonly content: string;
    readonly customType?: string;
    readonly display?: boolean;
    readonly details?: { readonly event_id?: string };
  }

  /**
   * Boots the session against a listener that answers every registration with a consumer on
   * `RELAY`, the one path a message reaches it by, or refuses it (`refuse`) the way a listener
   * that can give no consumer does. The host is `idle` (writes each card it is handed, then ends
   * its message) or `streaming` (holds every card until `take`). `events` lists, in order, each
   * card handed and written and each NATS publish (a relay reply as `<inbox> <word>`).
   */
  async function bootConsumer(
    query: string,
    options: {
      /** The first consumer answer reports a gap: messages aged out before the consumer's start. */
      readonly gap?: boolean;
      /**
       * The listener refuses every registration with this status and reason until `accept()`,
       * with `error` as its message (`this session was given no durable consumer` without it).
       */
      readonly refuse?: {
        readonly status: number;
        readonly reason?: string;
        readonly error?: string;
      };
      readonly entries?: readonly unknown[];
      readonly turn?: "idle" | "streaming";
      readonly runEphemeralTurn?: SessionContext["runEphemeralTurn"];
      /** The listener's clock, sent as every reply's `Date`; replies carry none without it. */
      readonly listenerClock?: () => number;
      /**
       * The relay subject's own open fails, so the first pass sends no registration at all
       * (`registerWithRelay` refuses one that could carry no consumer request) and boot waits
       * for none.
       */
      readonly withoutRelay?: boolean;
      /** Answers a Dispatch request the fixture has no answer of its own for: the read of a
       *  message an event carried in part (`wholeCarriedBody`). A row whose host moves inside
       *  that read answers it from an async callback, as Dispatch's own answer takes time. */
      readonly dispatch?: (url: URL) => Response | undefined | Promise<Response | undefined>;
      /** The session the host has open, read at each call: a row moves the host by changing what
       *  it answers, with no session event fired for the move. */
      readonly hostSessionID?: () => string;
      /** The host refuses every card this answers true for, as a host that cannot take one
       *  does: the frame fails, and its third delivery is given up on, which is when the
       *  session is told what it was never shown (`@legion/envoy-client/consumer-delivery`). */
      readonly refuseCard?: (message: SentCard) => boolean;
      /** Answers `POST /v1/interests/unsubscribe` (`{}` without it); a throw is no answer. */
      readonly unsubscribe?: () => Response;
    } = {}
  ) {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = DISPATCH_TOKEN;
    let refusing = options.refuse;
    // While set, the listener holds each registration on the wire until the row releases it.
    let holding: PromiseWithResolvers<void> | undefined;
    const registrations: Registration[] = [];
    const dispatchReplies: string[] = [];
    const listener = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(input.toString());
      if (url.pathname.endsWith("/reply")) {
        dispatchReplies.push(url.pathname);
        return response({});
      }
      if (url.pathname === "/v1/interests/subscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly dir: string;
          readonly topics: readonly string[];
          readonly delivery?: Registration["delivery"];
        };
        registrations.push({
          topics: body.topics,
          ...(body.delivery === undefined ? {} : { delivery: body.delivery }),
          relaySubscribed: natsState.controls.get(RELAY)?.active() === true,
        });
        const held = holding;
        if (held !== undefined) await held.promise;
        if (refusing !== undefined) {
          return new Response(
            JSON.stringify({
              error: refusing.error ?? "this session was given no durable consumer",
              reason: refusing.reason,
            }),
            { status: refusing.status }
          );
        }
        return response({
          session_id: SESSION,
          machine_id: "test",
          dir: body.dir,
          topics: body.topics,
          delivery: consumerDelivery(SESSION, {
            streamID: STREAM,
            gap: options.gap === true && registrations.length === 1,
          }),
        });
      }
      if (url.pathname === "/v1/interests/unsubscribe" && options.unsubscribe !== undefined) {
        return options.unsubscribe();
      }
      if (
        url.pathname.startsWith("/v1/interests/") &&
        url.pathname !== "/v1/interests/unsubscribe"
      ) {
        return response({ session_id: SESSION, machine_id: "test", dir: "/tmp", topics: [] });
      }
      const read = await options.dispatch?.(url);
      if (read !== undefined) return read;
      return response({});
    }) as typeof fetch;
    const listenerClock = options.listenerClock;
    globalThis.fetch =
      listenerClock === undefined
        ? listener
        : ((async (input: string | URL | Request, init?: RequestInit) => {
            const answer = await listener(input, init);
            const headers = new Headers(answer.headers);
            headers.set("date", new Date(listenerClock()).toUTCString());
            return new Response(answer.body, { status: answer.status, headers });
          }) as typeof fetch);
    const events: string[] = [];
    natsState.onPublish = (subject) => {
      const data = natsState.published.at(-1)?.data;
      events.push(data === undefined ? subject : `${subject} ${new TextDecoder().decode(data)}`);
    };
    const { default: envoyExtension } = await import(`./envoy.ts?${query}`);
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const written: unknown[] = [];
    const held: SentCard[] = [];
    let turn = options.turn ?? "idle";
    const write = async (message: SentCard) => {
      // The host starts the turn the card triggers after `sendMessage` returns.
      await Promise.resolve();
      const { content, customType, display, details } = message;
      written.push({ type: "custom_message", customType, content, display, details });
      events.push(`written ${details?.event_id ?? content}`);
      await fixture.handlers.get("message_end")?.(
        {
          message: { role: "custom", customType, content, display, details, timestamp: Date.now() },
        },
        context
      );
    };
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message: SentCard, sendOptions: unknown) => {
        if (options.refuseCard?.(message) === true) throw new Error("the host refused the card");
        fixture.pi.sendMessage(message, sendOptions);
        events.push(`handed ${message.details?.event_id ?? message.content}`);
        if (turn === "idle") void write(message);
        else held.push(message);
      },
    });
    const prior = options.entries ?? [];
    const base = sessionContext(SESSION);
    const notifications: string[] = [];
    const context: SessionContext = {
      ...base,
      sessionManager: {
        ...base.sessionManager,
        getBranch: () => [...prior, ...fixture.entries, ...written],
        getEntries: () => [...prior, ...fixture.entries, ...written],
        ...(options.hostSessionID !== undefined && { getSessionId: options.hostSessionID }),
      },
      setInterval: (callback) => {
        intervals.push(callback);
      },
      isIdle: () => turn === "idle",
      hasPendingMessages: () => held.length > 0,
      ui: { ...base.ui, notify: (message) => notifications.push(message) },
      ...(options.runEphemeralTurn !== undefined && { runEphemeralTurn: options.runEphemeralTurn }),
    };
    await fixture.handlers.get("session_start")?.({}, context);
    if (options.withoutRelay !== true) {
      await until(() => registrations.length > 0, "the first registration");
    }
    await settle();
    let inboxes = 0;
    const session = {
      fixture,
      events,
      registrations,
      dispatchReplies,
      /** Every warning and note the session showed in its UI, in order. */
      notifications,
      /** Every subject this process holds a live NATS subscription on, sorted. */
      subscriptions: () => natsState.controls.open(),
      /** The listener stops refusing: the next registration is answered a consumer. */
      accept: () => {
        refusing = undefined;
      },
      /** The listener refuses every registration from here on, as `options.refuse` does. */
      refuse: (refusal: {
        readonly status: number;
        readonly reason?: string;
        readonly error?: string;
      }) => {
        refusing = refusal;
      },
      /** The listener holds each registration from here on on the wire, until `release`. */
      hold: () => {
        holding = Promise.withResolvers<void>();
      },
      /** The listener answers every registration it holds, and holds no more. */
      release: () => {
        holding?.resolve();
        holding = undefined;
      },
      entries: () => [...prior, ...fixture.entries, ...written],
      /** The `envoy-message` cards handed whose text holds `text`: a frame's event id, or the
       *  name of a notice the session tells itself ("Envoy inbound unavailable"). */
      cards: (text: string) =>
        fixture.deliveries.filter(
          (delivery) => delivery.customType === "envoy-message" && delivery.content.includes(text)
        ),
      /**
       * The notices whose text holds `text` that the session holds for a settled host. A stopping
       * host is never settled, so a notice raised during the stop is never handed: it is held,
       * and recorded in the transcript, which the session's next process shows it from.
       */
      heldNotices: (text: string) =>
        fixture.entries.filter(
          (entry) =>
            entry.customType === REPLAY_NOTICE_ENTRY && JSON.stringify(entry.data).includes(text)
        ),
      /** The listener relays stored message `seq`; answers the reply inbox it carried. */
      relay: (
        seq: number,
        data: string,
        frame: {
          readonly subject?: string;
          readonly delivered?: number;
          readonly storedAt?: Date;
        } = {}
      ): string => {
        const controls = natsState.controls.get(RELAY);
        if (controls === undefined) throw new Error("the relay subject was never subscribed");
        inboxes += 1;
        const inbox = `_INBOX.relay.${seq}.${inboxes}`;
        controls.push(data, inbox, {
          [RelayHeader.subject]: frame.subject ?? AGENT,
          [RelayHeader.streamSeq]: String(seq),
          [RelayHeader.streamID]: STREAM,
          [RelayHeader.delivered]: String(frame.delivered ?? 1),
          [RelayHeader.storedAt]: (frame.storedAt ?? new Date()).toISOString(),
        });
        return inbox;
      },
      /** The words the session answered on a relay frame's inbox, in order. */
      replies: (inbox: string) =>
        natsState.published
          .filter((published) => published.subject === inbox)
          .map((published) => new TextDecoder().decode(published.data)),
      /** One heartbeat tick, run until its registration reached the listener. */
      tick: async () => {
        const before = registrations.length;
        for (const interval of intervals) interval();
        await until(() => registrations.length > before, "the heartbeat's registration");
        await settle();
      },
      /** One heartbeat tick, not waited on: its pass may queue behind another. */
      beat: () => {
        for (const interval of intervals) interval();
      },
      /**
       * One heartbeat tick while NATS refuses every dial, run until its pass dialled and settled:
       * the session holds no relay subject, so no registration reaches the listener.
       */
      tickWithoutNats: async () => {
        const dials = natsState.failConnects;
        for (const interval of intervals) interval();
        await until(() => natsState.failConnects < dials, "the tick's NATS dial");
        await settle();
      },
      /** nats.js reports a reconnect. */
      reconnect: async () => {
        const before = registrations.length;
        pushNatsStatus("reconnect");
        await until(() => registrations.length > before, "the reconnect's registration");
        await settle();
      },
      /** The streaming turn takes every card it holds: the host writes each, in order. */
      take: async () => {
        for (const message of held.splice(0)) await write(message);
        await settle();
      },
      /**
       * The person presses Escape while the turn streams: Oh My Pi's `clearQueue({ forInterrupt:
       * true })` drops the cards the turn held, writing none, the run ends aborted, and the host is
       * idle from then on, writing each card it is handed.
       */
      interrupt: async () => {
        held.splice(0);
        turn = "idle";
        await fixture.handlers.get("agent_end")?.(
          { messages: [{ role: "assistant", stopReason: "aborted", content: [] }] },
          context
        );
        await settle();
      },
      follow: (topic: string) =>
        fixture.tools
          .find((tool) => tool.name === "envoy_subscribe")
          ?.execute("", { topics: [topic] }),
      /** A session event that keeps the session's id (a tree navigation): its pass registers. */
      navigate: () => fixture.handlers.get("session_tree")?.({}, context),
      stop: () => fixture.handlers.get("session_shutdown")?.({}, context),
    };
    return session;
  }

  test("subscribes the relay subject before it registers, and every registration asks for consumer delivery", async () => {
    const session = await bootConsumer("consumer-registration");

    const [first] = session.registrations;
    expect(first?.relaySubscribed).toBe(true);
    expect(first?.delivery?.consumer).toBe(true);
    expect(first?.delivery?.epoch).toEqual(expect.any(String));
    expect(first?.delivery?.resume).toBeUndefined();

    const inbox = session.relay(7, peer("registration"));
    await until(() => session.replies(inbox).includes("done"), "the frame's done");
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

    const inboxes = [1, 2, 3].map((seq) => session.relay(seq, peer(`order-${seq}`)));
    await until(() => session.cards("evt-order-3").length > 0, "the third card");

    for (const [index, inbox] of inboxes.entries()) {
      expect(session.replies(inbox)).toEqual(["received"]);
      expect(session.events.indexOf(`${inbox} received`)).toBeLessThan(
        session.events.indexOf(`handed evt-order-${index + 1}`)
      );
    }
    expect(
      session.events.filter((event) => event.startsWith("handed ")).map((event) => event.slice(7))
    ).toEqual(["evt-order-1", "evt-order-2", "evt-order-3"]);

    await session.take();

    for (const [index, inbox] of inboxes.entries()) {
      expect(session.replies(inbox)).toEqual(["received", "done"]);
      expect(session.events.indexOf(`written evt-order-${index + 1}`)).toBeLessThan(
        session.events.indexOf(`${inbox} done`)
      );
    }
  });

  test("a frame written before a restart and relayed again after it is answered done and not shown again", async () => {
    const first = await bootConsumer("consumer-restart-a");
    const frame = peer("restart");
    const before = first.relay(4, frame);
    await until(() => first.replies(before).includes("done"), "the first copy's done");
    await first.stop();

    const second = await bootConsumer("consumer-restart-b", { entries: first.entries() });
    expect(second.registrations[0]?.delivery?.resume).toEqual({ stream_id: STREAM, seq: 4 });
    const again = second.relay(4, frame, { delivered: 2 });
    await until(() => second.replies(again).includes("done"), "the second copy's done");

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
    await until(() => first.replies(inbox).includes("done"), "the frame's done");

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
    const { default: envoyExtension } = await import("./envoy.ts?connection-probe");
    const fixture = createPi();
    envoyExtension(fixture.pi);
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

    await fixture.handlers.get("session_start")?.({}, firstContext);
    await until(
      () => natsState.controls.get(deliverSubject(first))?.active() === true,
      "the first session's relay subject"
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
      () => natsState.controls.get(deliverSubject(next))?.active() === true,
      "the new session's relay subject"
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
      () => natsState.controls.get(deliverSubject(next)) !== nextRelay,
      "the relay subject subscribed again"
    );
    const reopened = natsState.controls.get(deliverSubject(next));
    expect(await probe(reopened)).toEqual(["done"]);

    // The session shuts down: its relay subject closes with its connection.
    await fixture.handlers.get("session_shutdown")?.({}, sessionContext(next));
    expect(reopened?.active()).toBe(false);
    expect(cards()).toHaveLength(shown);
  });

  // A refusal is the whole of a session's inbound: nothing else reaches it, so the session must
  // say so and keep asking rather than run on quietly deaf.
  for (const refusal of [
    { status: 503, reason: "no_grant" },
    { status: 409, reason: "not_requested" },
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
    // A namespace read, so this file still loads at a commit whose module lacks the export.
    const FIRST_OPEN_BOUND_MS = consumerDeliveryModule.INBOUND_FIRST_OPEN_BOUND_MS ?? 120_000;
    const pastBound = start + FIRST_OPEN_BOUND_MS + 1_000;
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
      expect(session.cards("Envoy inbound restored")).toHaveLength(1);
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
    const FIRST_OPEN_BOUND_MS = consumerDeliveryModule.INBOUND_FIRST_OPEN_BOUND_MS ?? 120_000;
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
      expect(logged.filter((line) => /receive[sd]? nothing/.test(line))).toEqual([]);
      expect(told()).toEqual({ cards: 0, notes: 0 });
      expect(session.heldNotices("Envoy inbound unavailable")).toEqual([]);

      // A later registration of the same outage, inside the bound, logs nothing more.
      setSystemTime(new Date(start + 15_000));
      await session.tickWithoutNats();
      expect(found()).toHaveLength(1);
      expect(told()).toEqual({ cards: 0, notes: 0 });
      expect(session.heldNotices("Envoy inbound unavailable")).toEqual([]);

      // Past the bound the session is told once: the notice's card, its pane note, and its own
      // log line.
      setSystemTime(new Date(start + FIRST_OPEN_BOUND_MS + 1_000));
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
    expect(
      session.notifications.filter((message) =>
        message.includes("inbound messages are unavailable (")
      )
    ).toHaveLength(1);
  });

  // Row (e) of check l23, for this plugin (R24 of dispatch://INFRA-3589, against R26): the
  // listener answers its bus-outage 503 to a registration whose known-session record did not
  // land, after it made the session's consumer, so what the stream stores still reaches the
  // session. Such a refusal is held as the start race is: no notice inside the bound, and a
  // registration accepted inside it clears it with none. Past the bound the session is told once
  // that the listener did not record its registration, and never that it has no consumer.
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

  test("(e) past the bound a bus-outage 503 is told once, as a registration the listener did not record, and the accepted registration after it says inbound is back", async () => {
    const session = await bootConsumer("consumer-unrecorded-past-bound", {
      refuse: { status: 503, error: UNRECORDED },
    });
    const start = Date.now();
    // A namespace read, so this file still loads at a commit whose module lacks the export.
    const FIRST_OPEN_BOUND_MS = consumerDeliveryModule.INBOUND_FIRST_OPEN_BOUND_MS ?? 120_000;
    try {
      await until(() => session.registrations.length >= 2, "the first registration's retry");
      await settle();
      setSystemTime(new Date(start + FIRST_OPEN_BOUND_MS + 1_000));
      await refusedTick(session);
      setSystemTime(new Date(start + FIRST_OPEN_BOUND_MS + 16_000));
      await refusedTick(session);
      const told = session.cards("Envoy inbound unavailable");
      expect(told).toHaveLength(1);
      expect(told[0]?.content).toContain("write the known-session record");
      expect(told[0]?.content).toMatch(/not record/i);
      expect(told[0]?.content).not.toContain("no durable consumer");
      expect(told[0]?.content).not.toContain("receives none");

      session.accept();
      setSystemTime(new Date(start + FIRST_OPEN_BOUND_MS + 31_000));
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
    const FIRST_OPEN_BOUND_MS = consumerDeliveryModule.INBOUND_FIRST_OPEN_BOUND_MS ?? 120_000;
    const claims = /receive[sd]? (nothing|again)|inbound messages are/;
    try {
      await until(() => session.registrations.length >= 2, "the first registration's retry");
      await settle();
      setSystemTime(new Date(start + FIRST_OPEN_BOUND_MS + 1_000));
      await refusedTick(session);
      expect(session.notifications).toContain(
        `envoy: this session's registration is not recorded for certain (${UNRECORDED}), and the session registers again at every heartbeat`
      );
      expect(logged).toContain(
        `warn envoy: this session's registration is not recorded for certain: ${UNRECORDED}`
      );

      session.accept();
      setSystemTime(new Date(start + FIRST_OPEN_BOUND_MS + 16_000));
      await session.tick();
      const restored = session.cards("Envoy inbound restored");
      expect(restored).toHaveLength(1);
      expect(restored[0]?.content).toContain(
        "The listener recorded a registration of this session"
      );
      expect(restored[0]?.content).not.toMatch(/receive[sd]? nothing/);
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

  // Row (d) of check l23, for this plugin (R24 of dispatch://INFRA-3589): a session that stops
  // while a heartbeat's pass waits behind a registration on the wire is told nothing. Its stop
  // closes the relay on purpose and deletes the record next, so no registration runs once the
  // stop began (`sendRegistration`), and the pass that waited decides nothing and says nothing.
  test("(d) a session that stops while a heartbeat's registration waits behind one on the wire sends no registration after the stop began and is told nothing", async () => {
    const session = await bootConsumer("consumer-stop-behind-registration");
    // A session event's pass registers, and the listener holds that registration on the wire.
    session.hold();
    const asked = session.registrations.length;
    const navigating = session.navigate();
    await until(() => session.registrations.length > asked, "the held registration");
    // The heartbeat's pass queues behind it.
    session.beat();
    await settle();
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

  // The outage report's own rule says that a stopping session is told nothing
  // (`inboundUnavailable`'s `stopping`), since the refusal of every registration once the stop
  // began does not reach this case: a registration already on the wire when the stop began is
  // answered during it, and its refusal says nothing about what a session whose record is deleted
  // next receives.
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

  test("a late BTW is shown as a card that starts no turn, and runs no side turn", async () => {
    let sideTurns = 0;
    const session = await bootConsumer("consumer-late-btw", {
      runEphemeralTurn: async () => {
        sideTurns += 1;
        return { replyText: "too late" };
      },
    });

    const inbox = session.relay(9, targetedDispatchEnvelope("btw", "late-btw"), { delivered: 2 });
    await until(() => session.replies(inbox).includes("done"), "the late BTW's done");

    const cards = session.cards("dispatch-late-btw");
    expect(cards).toHaveLength(1);
    expect(cards[0]?.content).toContain("runs no side turn");
    expect(cards[0]?.options).toEqual({ deliverAs: "steer", triggerTurn: false });
    // The inert path stamps the hand-over state too, so a later reading of the stored transcripts
    // says which state held the waiting card.
    expect(cards[0]?.details).toMatchObject({ handOver: "inert" });
    expect(session.events.indexOf("written dispatch-late-btw")).toBeLessThan(
      session.events.indexOf(`${inbox} done`)
    );
    expect(sideTurns).toBe(0);
    expect(session.dispatchReplies).toHaveLength(0);
  });

  // The one window before a late BTW's card: the read of the body whole. The card is the
  // session the frame names, and `sendCard` would hand it to the conversation the host moved to
  // inside that read, under the name of the session the person wrote to. Nothing is handed: the
  // claim goes back, so the frame is naked and the listener sends it again to the session it
  // names and to no other.
  test("shows no late BTW card when the host left inside the read of its body, and naks the frame", async () => {
    const warnings: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push(entry.message);
    });
    const move: { fire?: () => Promise<void> } = {};
    let moved = false;
    try {
      const session = await bootConsumer("consumer-late-btw-moved", {
        dispatch: async (url) => {
          if (!url.pathname.includes("/messages/")) return undefined;
          if (!moved) {
            moved = true;
            await move.fire?.();
          }
          return response({
            message: { body: "Can this ship?", id: "11111111-1111-4111-8111-111111111111" },
            replies: [],
          });
        },
      });
      // The conversation the host moves to is idle and holds no message, so an inert card is
      // handed to it at once rather than waiting for a settled moment: this row reads whether
      // the card went there, not whether the host was ready for it.
      move.fire = async () => {
        await session.fixture.handlers.get("session_switch")?.(
          { reason: "new" },
          {
            ...sessionContext("ses_after_btw"),
            hasPendingMessages: () => false,
            isIdle: () => true,
          }
        );
      };

      const inbox = session.relay(
        9,
        targetedDispatchEnvelope("btw", "late-btw-moved", { bodyTruncated: true }),
        { delivered: 2 }
      );
      await until(() => session.replies(inbox).includes("nak"), "the late BTW's nak");

      expect(moved).toBe(true);
      expect(session.cards("dispatch-late-btw-moved")).toHaveLength(0);
      expect(warnings).toContain(
        "envoy: a card was not shown here; the session it names is closed"
      );
      expect(session.replies(inbox)).toEqual(["received", "nak"]);
    } finally {
      stopSink();
    }
  });

  // The notice naming a frame the session was never shown is that session's: the ledger hands
  // the host the session the frame was relayed to (`DeliveryLedgerHost.showFailureNotice`), and
  // the host shows it there and nowhere else. The host moves while the notice is in flight -
  // the one window where the session the notice names and the conversation the host has open
  // part company, the ledger having bound the give-up to that session before it called - and the
  // notice is still held to the session it names: it is handed to nobody, the conversation the
  // host moved to is shown nothing of the frame, and the frame goes back to the stream for
  // whoever takes ses_delivery next (dispatch://INFRA-771).
  test("holds a give-up's notice to the session its frame was relayed to, showing the conversation the host moved to nothing of it", async () => {
    const moved = "ses_moved_notice";
    const warnings: logger.LogEvent[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push(entry);
    });
    try {
      const session = await bootConsumer("consumer-notice-addressed", {
        // Every card of this row's frames is refused, so each of their deliveries fails; the
        // notice naming a frame is no card of it, and is taken. The host's own messages - the
        // card wake's among them - are left alone, since refusing those tests nothing here and
        // throws where nothing is waiting to catch it.
        refuseCard: (message) =>
          String(message.details?.event_id ?? "").startsWith("evt-notice-") &&
          !message.content.includes("message not shown"),
      });
      // One frame's three deliveries: the third is given up on, and what comes back is what the
      // session answered the listener on that delivery's inbox.
      const givenUp = async (seq: number, label: string): Promise<string[]> => {
        let inbox = "";
        for (const delivered of [1, 2, 3]) {
          inbox = session.relay(seq, peer(label), { delivered });
          await until(
            () => session.replies(inbox).length > 1,
            `the answer to delivery ${delivered} of ${label}`
          );
        }
        return session.replies(inbox);
      };

      // The host stays on the session the frame was relayed to: the notice is carded there, and
      // the frame is settled on it.
      expect(await givenUp(9, "notice-shown")).toEqual(["received", "done"]);
      const shown = session.cards("evt-notice-shown");
      expect(shown).toHaveLength(1);
      expect(shown[0]?.content).toContain("notice: message not shown");

      // The host moves to another conversation while the next frame's notice is in flight.
      ledgerSeam.beforeFailureNotice = async () => {
        ledgerSeam.beforeFailureNotice = undefined;
        await session.fixture.handlers.get("session_switch")?.(
          { reason: "new" },
          { ...sessionContext(moved), hasPendingMessages: () => false, isIdle: () => true }
        );
      };
      expect(await givenUp(10, "notice-moved")).toEqual(["received", "nak"]);

      // The conversation the host moved to was shown nothing of that frame: no card of it, and
      // no notice naming it.
      expect(session.cards("evt-notice-moved")).toHaveLength(0);
      expect(
        session.fixture.deliveries.filter((delivery) =>
          JSON.stringify(delivery).includes("evt-notice-moved")
        )
      ).toEqual([]);
      // Which is because the notice was addressed to the session the frame was relayed to, with
      // another conversation open here: the one card not shown is that notice.
      expect(
        warnings
          .filter(
            (entry) =>
              entry.message === "envoy: a card was not shown here; the session it names is closed"
          )
          .map((entry) => entry.context)
      ).toEqual([{ addressed: SESSION, eventID: "evt-notice-moved", open: moved }]);
      // The first frame's notice is still there, so the row above reads a session that is shown
      // notices, not one that is shown nothing at all.
      expect(session.cards("evt-notice-shown")).toHaveLength(1);
    } finally {
      ledgerSeam.beforeFailureNotice = undefined;
      stopSink();
    }
  });

  test("a BTW whose side turn is still running delays neither the next frame's received nor its card", async () => {
    const answering = Promise.withResolvers<{ replyText: string }>();
    let sideTurns = 0;
    const session = await bootConsumer("consumer-btw-off-worker", {
      runEphemeralTurn: () => {
        sideTurns += 1;
        return answering.promise;
      },
    });

    const btw = session.relay(1, targetedDispatchEnvelope("btw", "btw-running"));
    await until(() => sideTurns === 1, "the side turn");
    const next = session.relay(2, peer("after-btw"));
    await until(() => session.replies(next).includes("done"), "the next frame's done");

    expect(session.replies(next)).toEqual(["received", "done"]);
    expect(session.cards("evt-after-btw")).toHaveLength(1);
    expect(session.replies(btw)).toEqual(["received"]);

    answering.resolve({ replyText: "the answer" });
    await until(() => session.replies(btw).includes("done"), "the BTW's done");
    expect(session.dispatchReplies).toHaveLength(1);
    expect(session.cards("dispatch-btw-running")).toHaveLength(0);
  });

  test("a card Escape cleared from the queue between received and done is shown once afterwards, and done only after that write", async () => {
    const session = await bootConsumer("consumer-queue-cleared", { turn: "streaming" });

    const inbox = session.relay(5, peer("cleared"));
    await until(() => session.cards("evt-cleared").length > 0, "the card");
    expect(session.replies(inbox)).toEqual(["received"]);

    await session.interrupt();
    expect(session.replies(inbox)).toEqual(["received"]);
    expect(
      session.entries().filter((entry) => JSON.stringify(entry).includes("evt-cleared"))
    ).toEqual([]);

    await session.tick();
    await until(() => session.replies(inbox).includes("done"), "the frame's done");

    expect(session.events.filter((event) => event === "written evt-cleared")).toHaveLength(1);
    expect(session.events.indexOf("written evt-cleared")).toBeLessThan(
      session.events.indexOf(`${inbox} done`)
    );
    expect(session.replies(inbox)).toEqual(["received", "done"]);
    await session.tick();
    expect(session.events.filter((event) => event === "written evt-cleared")).toHaveLength(1);
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
    await until(() => session.registrations.length > followed, "the registration without it");
    expect(session.registrations.at(-1)?.topics).not.toContain(issueTopic);

    const late = JSON.stringify({ ...JSON.parse(peer("after-removal")), topic: issueSubject });
    const inbox = session.relay(2, late, { subject: issueSubject });
    await until(() => session.replies(inbox).includes("done"), "the unfollowed frame's done");
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
        await until(() => warnings.length > index, `the warning about ${issue}'s removal`);
        expect(warnings[index]).toBe(row.warned);
      }
    } finally {
      stopSink();
    }
  });

  // A session behind on its inbox read a card stamped five hours earlier, took the stamp for the
  // present time, and told other sessions that the machines' clocks were slow. Each model request
  // now shows when the card reached the session beside when it was sent (`../src/card-age`).
  describe("the time a card reached the session", () => {
    test("a card the host writes hours after its send shows every model request when it was delivered and how long it waited", async () => {
      const session = await bootConsumer("card-age-late", { turn: "streaming" });
      const sentAt = Date.now() - (5 * 3_600_000 + 30_000);
      const frame = JSON.stringify({ ...JSON.parse(peer("card-age-late")), issued_at: sentAt });

      // The turn streams, so the host holds the card until the turn takes it.
      session.relay(1, frame);
      await until(() => session.cards("evt-card-age-late").length > 0, "the relayed card");
      await session.take();
      const [card] = session.cards("evt-card-age-late");
      if (card === undefined) throw new Error("the card was not handed");
      const written = {
        role: "custom",
        customType: card.customType,
        content: card.content,
        display: true,
        details: card.details,
        timestamp: sentAt,
      };
      const context = sessionContext(SESSION);
      const contextHandler = session.fixture.handlers.get("context");
      if (contextHandler === undefined) throw new Error("no context handler was registered");
      const first = JSON.stringify(await contextHandler({ messages: [written] }, context));
      const second = JSON.stringify(await contextHandler({ messages: [written] }, context));

      expect(first).toMatch(/\\n {2}delivered: \\"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\\"\\n/);
      expect(first).toContain(
        "waited: \\\"5 h 0 min between at and delivered; delivered is this machine's clock and at the sending server's, and at is when it was sent, not the present time\\\""
      );
      // The same bytes in every request, so the provider's prompt cache reads them again.
      expect(second).toBe(first);
      // The card the host keeps is the one it was handed.
      expect(card.content).not.toContain("delivered:");
    });

    // The mirror of the case above: a machine whose clock runs 4 h 45 min ahead of the listener's
    // read a card that waited a second exactly as if it had waited 4 h 45 min. The listener's
    // replies carry its clock, so the card says how the clocks stand and states no wait.
    test("a card from a listener whose clock runs hours behind this machine's says so, and states no wait", async () => {
      const behind = 4 * 3_600_000 + 45 * 60_000;
      const session = await bootConsumer("card-age-clocks", {
        turn: "streaming",
        listenerClock: () => Date.now() - behind,
      });
      // The listener stamps the send by its own clock, a second before the card reaches us.
      const sentAt = Date.now() - behind - 1_000;
      const frame = JSON.stringify({ ...JSON.parse(peer("card-age-clocks")), issued_at: sentAt });

      session.relay(1, frame);
      await until(() => session.cards("evt-card-age-clocks").length > 0, "the relayed card");
      await session.take();
      const [card] = session.cards("evt-card-age-clocks");
      if (card === undefined) throw new Error("the card was not handed");
      const written = {
        role: "custom",
        customType: card.customType,
        content: card.content,
        display: true,
        details: card.details,
        timestamp: Date.now(),
      };
      const contextHandler = session.fixture.handlers.get("context");
      if (contextHandler === undefined) throw new Error("no context handler was registered");
      const request = JSON.stringify(
        await contextHandler({ messages: [written] }, sessionContext(SESSION))
      );

      expect(request).not.toContain("waited:");
      expect(request).toContain(
        "clocks: \\\"this machine's clock runs 4 h 45 min ahead of the Envoy listener's, read from the listener's replies; delivered is this machine's clock and at the sending server's\\\""
      );
    });

    // One process serves one session at a time, and a session the host moves to is shown the
    // same event by its own consumer. The request that session makes must state when the event
    // reached it, from its own ledger record, and never when the session before it was shown it.
    test("an event in the session the host moved to reads that session's own delivery time", async () => {
      const session = await bootConsumer("card-age-moved", { turn: "streaming" });
      session.relay(1, peer("card-age-moved"));
      await until(() => session.cards("evt-card-age-moved").length > 0, "the relayed card");
      await session.take();

      // The host resumes a session it left, whose file holds no record of this event: what that
      // session was shown is its own file's, and this process's write for the session before it
      // is not in it.
      const mark = session.fixture.entries.length;
      const movedFile = () => session.fixture.entries.slice(mark);
      const resumed = sessionContext("ses_moved");
      const movedContext = {
        ...resumed,
        sessionManager: { ...resumed.sessionManager, getBranch: movedFile, getEntries: movedFile },
      };
      await session.fixture.handlers.get("session_switch")?.({ reason: "resume" }, movedContext);
      await settle();

      const [card] = session.cards("evt-card-age-moved");
      if (card === undefined) throw new Error("the card was not handed");
      // Handed before this process started, as a card held across a restart is: the request time
      // is no delivery time for it, so with nothing in this session's file it stays as it is.
      const written = {
        role: "custom",
        customType: card.customType,
        content: card.content,
        display: true,
        details: card.details,
        timestamp: 1,
      };
      const contextHandler = session.fixture.handlers.get("context");
      if (contextHandler === undefined) throw new Error("no context handler was registered");
      const movedWithoutRecord =
        JSON.stringify(await contextHandler({ messages: [written] }, movedContext)) ?? "";

      // The write this session's host made, a minute and a half before this request.
      const movedAt = Math.floor((Date.now() - 90_000) / 1_000) * 1_000;
      const movedIso = `${new Date(movedAt).toISOString().slice(0, 19)}Z`;
      const recorded = {
        ...movedContext,
        sessionManager: {
          ...movedContext.sessionManager,
          getBranch: () => [
            ...movedFile(),
            {
              type: "custom",
              customType: REPLAY_DELIVERED_ENTRY,
              data: { event_id: "evt-card-age-moved", at: movedAt },
            },
          ],
        },
      };
      const movedWithRecord = JSON.stringify(
        await contextHandler({ messages: [written] }, recorded)
      );
      const live = JSON.stringify(
        await contextHandler({ messages: [written] }, sessionContext(SESSION))
      );
      const liveAgain = JSON.stringify(
        await contextHandler({ messages: [written] }, sessionContext(SESSION))
      );

      // Never the time the session shown it first was shown it: with nothing in this session's
      // file, its request states no delivery time at all.
      expect(movedWithoutRecord).not.toContain("delivered:");
      expect(movedWithRecord).toContain(`delivered: \\"${movedIso}\\"`);
      // The session shown it first reads its own write, which its `message_end` fixed.
      expect(live).toMatch(/\\n {2}delivered: \\"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\\"\\n/);
      expect(live).not.toContain(movedIso);
      // Returning to it reads the same bytes, so the provider's prompt cache reads them again.
      expect(liveAgain).toBe(live);
    });

    // The delivery a session first gives a card goes into its file once, at the card's
    // `message_end` (`envoy-card-delivered`, a literal so the case loads against a source without
    // the export), and the requests after it record nothing (`../src/card-age`).
    test("a card's write records its delivery in the session once", async () => {
      const session = await bootConsumer("card-age-recorded", { turn: "streaming" });
      session.relay(1, peer("card-age-recorded"));
      await until(() => session.cards("evt-card-age-recorded").length > 0, "the relayed card");
      await session.take();
      const [card] = session.cards("evt-card-age-recorded");
      if (card === undefined) throw new Error("the card was not handed");
      const { customType, content, details } = card;
      const written = { role: "custom", customType, content, display: true, details, timestamp: 1 };
      const contextHandler = session.fixture.handlers.get("context");
      if (contextHandler === undefined) throw new Error("no context handler was registered");
      await contextHandler({ messages: [written] }, sessionContext(SESSION));
      await contextHandler({ messages: [written] }, sessionContext(SESSION));

      const recorded = session.fixture.entries.filter(
        (entry) => entry.customType === "envoy-card-delivered"
      );
      expect(recorded.map((entry) => entry.data)).toEqual([
        { event_id: "evt-card-age-recorded", at: expect.any(Number) },
      ]);
    });
  });

  // A comment, message or broadcast body is bounded block by block, so one event carries its
  // first part alone and says so with `body_truncated`; an event the bus refused whole reaches
  // the session as a stand-in with no body at all (`payload_omitted`). A host that prompted its
  // agent with either would hand it less than the comment says, so the session reads the whole
  // body through its own Dispatch before it prompts; with no Dispatch, or a read that fails, it
  // keeps what the event carried and the note that says so.
  describe("a body an event carried only in part", () => {
    const CUT_COMMENT = "44444444-4444-4444-8444-444444444444";
    const CARRIED = "The first paragraph.";
    const WHOLE = `${CARRIED}\n\nThe second paragraph, past where the event stopped.`;
    /** The topic the session follows, and the subject the comment event is published on: the
     *  frame reaches the session as its own consumer's relay of that subject. */
    const CUT_TOPIC = legacyDispatchIssueSubject("INFRA-370", ">");
    const CUT_SUBJECT = legacyDispatchIssueSubject("INFRA-370", "comment.created");

    /** The payload the event carries: the comment row with its body cut (`body_truncated`), or,
     *  for `omitted`, the stand-in a bus refusal leaves behind - the row's short top-level fields
     *  and `payload_omitted`, with no body at all. */
    function commentPayload(omitted: boolean): Record<string, unknown> {
      const named = {
        id: CUT_COMMENT,
        issue_key: "INFRA-370",
        author: { kind: "user", id: "alice" },
        created_at: "2026-11-02T00:00:00Z",
      };
      return omitted
        ? { ...named, payload_omitted: true }
        : {
            ...named,
            artifact_id: null,
            body: CARRIED,
            body_truncated: true,
            anchor: null,
            reply_to: null,
            ask_id: null,
            turn: null,
            resolved: false,
            resolved_by: null,
            resolved_at: null,
            edited_at: null,
            suggestion: null,
            mentions: [],
            deliveries: [],
            artifact_name: "",
          };
    }

    function cutCommentEnvelope(topic: string, omitted: boolean): string {
      return JSON.stringify({
        event_id: "dispatch-cut-1",
        source: "dispatch",
        source_event_id: "1",
        topic,
        dedupe_key: "dispatch-cut-1",
        issued_at: 1,
        payload_summary: "INFRA-370 comment created",
        payload: JSON.stringify({
          id: 1,
          seq: 1,
          issue_key: "INFRA-370",
          type: "comment.created",
          actor: { kind: "user", id: "alice" },
          notify: true,
          quiet: false,
          created_at: "2026-11-02T00:00:00Z",
          payload: commentPayload(omitted),
        }),
        trace_id: "dispatch-cut-1",
      });
    }

    /** Boots a session following INFRA-370's topic, relays the comment event - cut, or the
     *  stand-in when `omitted` - on the session's own consumer, the one path a frame reaches it
     *  by, and answers the comment read with `read`. Returns the card its agent was prompted
     *  with. */
    async function cardForCutComment(
      name: string,
      read: (path: string) => Response | undefined,
      omitted = false
    ): Promise<string> {
      const session = await bootConsumer(name);
      await session.follow(CUT_TOPIC);
      await settle();
      // The listener's own answers go on being answered; the comment read is the test's.
      const listener = globalThis.fetch;
      globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) =>
        read(new URL(input.toString()).pathname) ?? (await listener(input, init))) as typeof fetch;

      const inbox = session.relay(1, cutCommentEnvelope(CUT_SUBJECT, omitted), {
        subject: CUT_SUBJECT,
      });
      await until(() => session.replies(inbox).includes("done"), "the cut comment's done");
      const card = session.cards("dispatch-cut-1")[0]?.content;
      if (card === undefined) throw new Error("no card was delivered for the cut comment");
      return card;
    }

    test("is prompted with the body the comment route answers", async () => {
      const reads: string[] = [];
      const card = await cardForCutComment("cut-comment-read", (path) => {
        if (path !== `/api/v1/comments/${CUT_COMMENT}`) return undefined;
        reads.push(path);
        return response({
          comment: { id: CUT_COMMENT, issue_key: "INFRA-370", body: WHOLE },
          replies: [],
        });
      });

      expect(reads).toEqual([`/api/v1/comments/${CUT_COMMENT}`]);
      // Byte for byte: the body the route answered, written as the card writes a text holding
      // blank lines (one quoted scalar), and no note, because nothing is missing.
      expect(card).toContain(`body: ${JSON.stringify(WHOLE)}`);
      expect(card).not.toContain("first part only");
    });

    test("keeps the part it carried, and the note, when that read fails", async () => {
      const card = await cardForCutComment("cut-comment-read-fails", (path) =>
        path === `/api/v1/comments/${CUT_COMMENT}`
          ? new Response(JSON.stringify({ code: "INTERNAL", error: "down" }), { status: 500 })
          : undefined
      );

      expect(card).toContain(CARRIED);
      expect(card).not.toContain("The second paragraph");
      expect(card).toContain(
        `the body is its first part only; read it whole with GET /api/v1/comments/${CUT_COMMENT}`
      );
    });

    test("is prompted with that body for a stand-in the bus left with no body at all", async () => {
      const reads: string[] = [];
      const card = await cardForCutComment(
        "omitted-comment-read",
        (path) => {
          if (path !== `/api/v1/comments/${CUT_COMMENT}`) return undefined;
          reads.push(path);
          return response({
            comment: { id: CUT_COMMENT, issue_key: "INFRA-370", body: WHOLE },
            replies: [],
          });
        },
        true
      );

      expect(reads).toEqual([`/api/v1/comments/${CUT_COMMENT}`]);
      expect(card).toContain(`body: ${JSON.stringify(WHOLE)}`);
      expect(card).not.toContain("too large for the bus");
    });

    test("gives the note for a stand-in whose read fails, having no body to keep", async () => {
      const card = await cardForCutComment(
        "omitted-comment-read-fails",
        (path) =>
          path === `/api/v1/comments/${CUT_COMMENT}`
            ? new Response(JSON.stringify({ code: "INTERNAL", error: "down" }), { status: 500 })
            : undefined,
        true
      );

      expect(card).not.toContain("The second paragraph");
      expect(card).toContain(
        `this event carries no body: it was too large for the bus; read it whole with GET /api/v1/comments/${CUT_COMMENT}`
      );
    });
  });
});
