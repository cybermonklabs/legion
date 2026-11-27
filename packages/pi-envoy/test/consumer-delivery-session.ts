// The session the consumer-delivery rows boot (`../extensions/consumer-delivery*.test.ts`): an
// instance of the extension on the harness (`./extension-harness`), against a listener that answers
// every registration with a consumer, or refuses it. A listener that answers `consumer` relays the
// session's durable consumer on `notifications.envoy.deliver.<id>`: each frame carries the stored
// envelope and its stream headers, with a reply inbox the session answers `received` on at decode
// and `done` once the frame is in the transcript (`@legion/envoy-client/consumer-delivery`). This
// module registers no hook: each file that boots the session calls `useExtensionHarness()`.
import { createHash } from "node:crypto";
import { RelayHeader } from "@legion/envoy-client/consumer-delivery";
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer";
import type { SessionContext as HostSessionContext, PiApi } from "@legion/pi-shared/pi-types";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { REPLAY_NOTICE_ENTRY } from "../src/missed-messages";
import type { EnvoyModule } from "./envoy-module";
import { createPi, sessionContext, settle, until } from "./extension-harness";
import { natsState, pushNatsStatus, response } from "./listener-stand-in";
import type { SessionContext } from "./stand-in-host";

/** The host's own context, for the two members the stand-in host's type does not name. */
type HostContextMembers = Pick<HostSessionContext, "hasPendingMessages" | "runEphemeralTurn">;

/** The Dispatch token a row hands the session in DISPATCH_TOKEN. */
const DISPATCH_TOKEN = fixtureSecret("dsp");

/** The session the rows boot, its agent subject, its relay subject, and the stream's id. */
export const SESSION = "ses_delivery";
export const AGENT = `notifications.agent.${SESSION}`;
export const RELAY = `notifications.envoy.deliver.${SESSION}`;
export const STREAM = "stream-1";

/** A peer session's message to the session under test. */
export function peer(label: string): string {
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
export function removal(issue: string, topic: string): string {
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

/** One registration the listener took, as the rows read it. */
export interface Registration {
  readonly topics: readonly string[];
  readonly delivery?: {
    readonly consumer: boolean;
    readonly epoch: string;
    readonly resume?: { readonly stream_id: string; readonly seq: number };
  };
  /** Whether the relay subject was subscribed when the registration reached the listener. */
  readonly relaySubscribed: boolean;
}

/** The listener's interest entry a row's sessions register with (`bootConsumer`'s `registry`). */
export interface InterestEntry {
  readonly topics: Set<string>;
  /** The topics of every release a session asked for, landed or refused, in order. */
  readonly releases: string[][];
  /** While set, every release fails, as a listener that is down fails it, and the entry keeps
   *  the topics. */
  refusing: boolean;
}

/** An empty interest entry for a row's sessions (`bootConsumer`'s `registry`). */
export const interestEntry = (): InterestEntry => ({
  topics: new Set(),
  releases: [],
  refusing: false,
});

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
 * card handed and written and each NATS publish (a relay reply as `<inbox> <word>`). `query` tags
 * the extension's module (`../extensions/envoy.ts?<query>`): Bun evaluates a module once per
 * process, so a tag no other row of any file uses boots an instance of the extension of its own.
 */
export async function bootConsumer(
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
    readonly runEphemeralTurn?: HostContextMembers["runEphemeralTurn"];
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
    /**
     * The listener's interest entry for the session: a registration it accepts merges its
     * topics in, a release takes them out, and a read of the interests answers it. Without it a
     * read answers no topic and every release lands (`unsubscribe` answers it then).
     */
    readonly registry?: InterestEntry;
    /** The host refuses to append an entry of each custom type this answers true for, as a
     *  session file that cannot be written refuses it. */
    readonly refuseEntry?: (customType: string) => boolean;
  } = {}
) {
  process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
  process.env.DISPATCH_URL = "http://dispatch.test";
  process.env.DISPATCH_TOKEN = DISPATCH_TOKEN;
  let refusing = options.refuse;
  // While set, the listener holds each registration on the wire until the row releases it.
  let holding: { readonly promise: Promise<void>; readonly resolve: () => void } | undefined;
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
      for (const topic of body.topics) options.registry?.topics.add(topic);
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
    if (url.pathname === "/v1/interests/unsubscribe" && options.registry !== undefined) {
      const body = JSON.parse(init?.body?.toString() ?? "{}") as {
        readonly topics: readonly string[];
      };
      options.registry.releases.push([...body.topics]);
      if (options.registry.refusing) {
        return new Response(JSON.stringify({ error: "the listener is down" }), { status: 409 });
      }
      for (const topic of body.topics) options.registry.topics.delete(topic);
      return response({});
    }
    if (url.pathname === "/v1/interests/unsubscribe" && options.unsubscribe !== undefined) {
      return options.unsubscribe();
    }
    if (url.pathname.startsWith("/v1/interests/") && url.pathname !== "/v1/interests/unsubscribe") {
      return response({
        session_id: SESSION,
        machine_id: "test",
        dir: "/tmp",
        topics: [...(options.registry?.topics ?? [])],
      });
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
  const { default: envoyExtension }: EnvoyModule = await import(`../extensions/envoy.ts?${query}`);
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
  // The stand-in host is a subset of the real `PiApi`: the extension reads only what this host
  // supplies, and the cast is that same narrowing.
  await envoyExtension({
    ...fixture.pi,
    sendMessage: (message: SentCard, sendOptions: unknown) => {
      if (options.refuseCard?.(message) === true) throw new Error("the host refused the card");
      fixture.pi.sendMessage(message, sendOptions);
      events.push(`handed ${message.details?.event_id ?? message.content}`);
      if (turn === "idle") void write(message);
      else held.push(message);
    },
    appendEntry: (customType: string, data: unknown) => {
      if (options.refuseEntry?.(customType) === true) {
        throw new Error("the session file could not be written");
      }
      fixture.pi.appendEntry(customType, data);
    },
  } as unknown as PiApi);
  const prior = options.entries ?? [];
  const base = sessionContext(SESSION);
  const notifications: string[] = [];
  const context: SessionContext & HostContextMembers = {
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
    /** The person unfollows `topics` (`envoy_unsubscribe`). */
    unfollow: (topics: readonly string[]) =>
      fixture.tools
        .find((tool) => tool.name === "envoy_unsubscribe")
        ?.execute("", { topics: [...topics] }),
    /** A session event that keeps the session's id (a tree navigation): its pass registers. */
    navigate: () => fixture.handlers.get("session_tree")?.({}, context),
    stop: () => fixture.handlers.get("session_shutdown")?.({}, context),
  };
  return session;
}
