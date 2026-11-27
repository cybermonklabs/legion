// The listener and the NATS connection the extension talks to in every test file that calls
// `useExtensionHarness()` (`./extension-harness`): the fetch that records every registration, the
// NATS connection the extension dials (`natsStandInConnect`), the relay of the session's consumer,
// the row a session's work runs for, and the listener's answers a row's fetch gives. Bun evaluates
// an imported module once per process, so every such file drives this one set of state.
// `./extension-harness` hands the dial to the extension, and starts and resets this state around
// each row (`startRow`, `resetNatsStandIn`).
import { AsyncLocalStorage } from "node:async_hooks";
import { BUS_UNANSWERED_PREFIX } from "@legion/contracts";
import { deliverSubject, RelayHeader } from "@legion/envoy-client/consumer-delivery";
import { subjectsOverlap } from "@legion/envoy-client/stream-topics";
import { keyedLayer } from "@legion/envoy-client/test/keyed-layer";
import { leastAnswerFor } from "@legion/envoy-client/test/route-answers";
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer";
import { ErrorCode, NatsError, connect as realConnect } from "nats";

/** A message's headers, as nats.js's `MsgHdrs` reads them: a missing one reads `""`. */
type MessageHeaders = { readonly get: (name: string) => string };

type Subscription = {
  readonly unsubscribe: () => void;
  readonly [Symbol.asyncIterator]: () => AsyncIterator<{
    readonly subject: string;
    readonly data: Uint8Array;
    readonly reply: string;
    readonly headers?: MessageHeaders;
  }>;
};

export type SubscriptionControls = {
  /** Hands the subscription a message; `headers`, when given, are the message's NATS headers. */
  readonly push: (data: string, reply?: string, headers?: Readonly<Record<string, string>>) => void;
  readonly end: () => void;
  readonly fail: (error: Error) => void;
  readonly active: () => boolean;
};

/**
 * Every subscription this process really holds, by subject: after a registration that is its
 * relay subject and its agent stream's control subject, and nothing else — a notification topic
 * reaches the session only through the relay of its own consumer.
 */
const openSubscriptions = new Map<string, SubscriptionControls>();

/** The stream sequence the next relayed frame carries. */
let relaySeq = 0;

/**
 * The topics the listener was last told this session follows, taken from every registration this
 * process makes. The session's consumer is filtered on exactly them, so they are the subjects a
 * relay frame can arrive on, and they are what "the session follows this topic" means now that
 * the plugin holds no subscription of its own on any of them.
 */
const registeredTopics = new Set<string>();

/** Whether a registered topic reaches `subject`, under NATS wildcard rules. */
function relayCarries(subject: string): boolean {
  for (const topic of registeredTopics) {
    if (subjectsOverlap(topic, subject)) return true;
  }
  return false;
}

/**
 * Records every registration whatever `fetch` a row installs, so the followed topics are known
 * centrally, and puts the row's fetch behind Dispatch's keyed layer (`keyedLayer`): each 2xx it
 * gives a Dispatch write names the write's key back, as Dispatch's own answers do. A row's own
 * fetch is kept behind this one; reading `globalThis.fetch` answers this wrapper, so a row that
 * wraps the fetch it found takes it from `rowFetch()`, never from `globalThis.fetch`, which would
 * call the row's own wrapper forever. That mistake fails at once, by name: a call nested past
 * `MOST_NESTED_FETCHES` of this wrapper's own is refused. Made once, it grew one row's process to
 * 118 GiB before anything stopped it.
 */
let installedFetch: typeof globalThis.fetch = globalThis.fetch;
/** The real fetch, as this module found it: what a row restores (`tearDownRow` in
 *  `./extension-harness`) and what the live stack uses. */
export const originalFetch: typeof globalThis.fetch = installedFetch;
const keyedFetch = keyedLayer((input: string | URL | Request, init?: RequestInit) =>
  installedFetch(input, init)
);
/** How deep the calls of `recordingFetch` are nested in the current call's async context. */
const fetchDepth = new AsyncLocalStorage<number>();
/** A row's fetch may hand a request on through `globalThis.fetch` this many times at most. */
const MOST_NESTED_FETCHES = 8;
const recordingFetch = (async (input, init) => {
  const depth = (fetchDepth.getStore() ?? 0) + 1;
  if (depth > MOST_NESTED_FETCHES) {
    throw new Error(
      `a row's fetch handed its request on through globalThis.fetch ${MOST_NESTED_FETCHES} times: globalThis.fetch is this harness's recording wrapper, which calls the row's fetch again, so take the fetch to hand on to from rowFetch()`
    );
  }
  try {
    if (new URL(input.toString()).pathname === "/v1/interests/subscribe") {
      const body = JSON.parse(init?.body?.toString() ?? "{}") as {
        readonly topics?: readonly string[];
      };
      registeredTopics.clear();
      for (const topic of body.topics ?? []) registeredTopics.add(topic);
    }
  } catch {
    // A request that is not a registration, or a body that is not JSON, teaches nothing.
  }
  return fetchDepth.run(depth, () => keyedFetch(input, init));
}) as typeof globalThis.fetch;
/** The fetch a row installed, for a row that answers some requests itself and hands on the rest. */
export const rowFetch = (): typeof globalThis.fetch => installedFetch;
Object.defineProperty(globalThis, "fetch", {
  configurable: true,
  get: () => recordingFetch,
  set: (next: typeof globalThis.fetch) => {
    if (next !== recordingFetch) installedFetch = next;
  },
});

/**
 * The row a session's own work runs for. `bootDirectSession` (`extensions/envoy.test.ts`) starts
 * its session inside this scope, so what the session goes on to do (a delivery, a person's
 * `!command` and its answer) carries the row that booted it; `startRow` makes each row a new one.
 * Bun fails a row at its timeout and starts the next while that row's session still runs (bun
 * kills the row's dangling child, and the command it was running then answers), so what such a
 * session writes afterwards, a reply on Dispatch or a frame on NATS, is dropped rather than read
 * as the next row's.
 */
export const rowScope = new AsyncLocalStorage<symbol>();
export let currentRow = Symbol("row");
/** Whether the code running now is the work of a session whose row has ended. */
export function fromEndedRow(): boolean {
  const row = rowScope.getStore();
  return row !== undefined && row !== currentRow;
}

/** Makes the row starting now the current one (`setUpRow` in `./extension-harness`), so the work
 *  of a session an earlier row booted reads as an ended row's. */
export function startRow(): void {
  currentRow = Symbol("row");
}

/**
 * Hands the session a frame published on `subject`, as the listener's relay of the session's
 * consumer hands it over: the frame travels the relay subject with the headers that name the
 * subject it was published on. Undefined while no relay subject is subscribed, as the controls
 * of an unsubscribed subject were.
 *
 * An agent subject is one session's own, so its own relay carries it, open or closed: a row
 * running two sessions holds two relays, and a session the host moved leaves the relay of every
 * session it was before in the map. Any other subject takes the live relay, and the last one
 * closed when none is live: what a subject answers is the subscription the session holds now,
 * never one it held before.
 */
function relayControls(subject: string): SubscriptionControls | undefined {
  if (!relayCarries(subject)) return undefined;
  const AGENT_PREFIX = "notifications.agent.";
  let live: SubscriptionControls | undefined;
  let closed: SubscriptionControls | undefined;
  const own = subject.startsWith(AGENT_PREFIX)
    ? openSubscriptions.get(deliverSubject(subject.slice(AGENT_PREFIX.length)))
    : undefined;
  for (const [on, controls] of openSubscriptions) {
    if (!on.startsWith("notifications.envoy.deliver.")) continue;
    if (controls.active()) live ??= controls;
    else closed = controls;
  }
  const relay = own ?? live ?? closed;
  if (relay === undefined) return undefined;
  return {
    push: (data, reply, headers) => {
      relaySeq += 1;
      relay.push(data, reply === undefined || reply === "" ? `_INBOX.relay.${relaySeq}` : reply, {
        [RelayHeader.subject]: subject,
        [RelayHeader.streamSeq]: String(relaySeq),
        [RelayHeader.streamID]: TEST_STREAM_ID,
        [RelayHeader.delivered]: "1",
        [RelayHeader.storedAt]: new Date().toISOString(),
        ...headers,
      });
    },
    end: relay.end,
    fail: relay.fail,
    active: relay.active,
  };
}

export const natsState = {
  subscriptions: new Map<string, Subscription>(),
  controls: {
    /** The subject's own subscription, else the relay that carries it (`relayControls`). */
    get: (subject: string): SubscriptionControls | undefined =>
      openSubscriptions.get(subject) ?? relayControls(subject),
    /** Whether the session takes `subject`: its own subscription, or a topic its consumer
     *  relays (`relayCarries`). */
    has: (subject: string): boolean => openSubscriptions.has(subject) || relayCarries(subject),
    set: (subject: string, controls: SubscriptionControls): void => {
      openSubscriptions.set(subject, controls);
    },
    clear: (): void => openSubscriptions.clear(),
    keys: (): IterableIterator<string> => openSubscriptions.keys(),
    /**
     * Every subject this process holds a live NATS subscription on, sorted: after a registration
     * that is the session's relay subject and its agent-stream control subject, and nothing else.
     */
    open: (): string[] =>
      [...openSubscriptions]
        .filter(([, controls]) => controls.active())
        .map(([subject]) => subject)
        .sort(),
  },
  controlsByTopic: new Map<string, SubscriptionControls[]>(),
  published: [] as { readonly subject: string; readonly data: Uint8Array | undefined }[],
  onPublish: undefined as ((subject: string) => void) | undefined,
  failPublishes: 0,
  failSubscribes: 0,
  connectedNames: [] as string[],
  /** What each connect was given: its servers, and whether it carried an nkey authenticator. */
  connects: [] as { readonly servers: readonly string[]; readonly authenticated: boolean }[],
  /** The token each CONNECT presented (`auth_token`), where its authenticator answers one: a
   *  connect's and each reconnect's, in order. */
  presented: [] as string[],
  /** Each `subscribe` and `reconnect`, in order, for a row that holds the two to an order. */
  timeline: [] as string[],
  /** Each connection `close()` was called on, by the name its dial carried: a dial nothing holds
   *  is never drained, and with `maxReconnectAttempts: -1` it outlives every session. */
  closedNames: [] as string[],
  /** Every connection the stand-in made, in order, so a row can close one for good, as nats.js
   *  closes a connection on an authorization error it does not ignore, or on `close()`. */
  connections: [] as { readonly close: () => Promise<void> }[],
  failConnects: 0,
  /** The error a failed connect throws, as nats.js throws it: its refusal by default, a
   *  resolver's (`getaddrinfo`, its text naming the host) for a row that sees what a notice
   *  carries of it. */
  failError: (): unknown => NatsError.errorForCode(ErrorCode.ConnectionRefused),
  /** What `closed()` of a connection settles with once it is closed, as nats.js settles it with
   *  the close's own error (protocol.js:906-913); none by default. */
  closeError: undefined as Error | undefined,
  /** Every connection reports itself closed, as one NATS dropped does. */
  connectionsClosed: false,
  /** Hands each connect, once recorded, to nats.js itself, for a row that watches what it dials. */
  realConnect: false,
  /** A connect a test parks on, so a pass can be abandoned at its deadline while its connection
   *  is still being made and what its late continuation writes can be watched. */
  heldConnect: undefined as Promise<void> | undefined,
  drainHangs: false,
  drainStarted: false,
  /** Each mocked connection's `status()` readers: a push hands every one of them the status, as
   *  nats.js reports a reconnect to every reader of the connection's status iterator. */
  statusReaders: [] as ((status: { readonly type: string; readonly data: unknown }) => void)[],
  /** Every subject the connection had been asked to subscribe to when a `flush()` was called:
   *  the round trip the server answers, which is what makes a subscription live. */
  flushes: [] as string[][],
  /** A `flush()` a test parks on, so what the extension does before the server confirms a
   *  subscription can be watched. */
  heldFlush: undefined as Promise<void> | undefined,
  /** The subject whose round trip rejects: the next `flush()` taken once that subject's SUB has
   *  been written fails and clears this, as nats.js rejects every pending flush when the
   *  connection drops (`nats@2.29.3` `protocol.js`, `resetOutbound`). Named rather than counted,
   *  because a pass takes one round trip per connection and only one case's subject carries it. */
  failFlushOnSubject: undefined as string | undefined,
  /** Whether that rejection also closes its connection: a rejection on a connection that is not
   *  closed leaves the open to go on, and only a connection closed for good makes it give up. */
  failFlushClosesConnection: false,
};

/** Reports `{ type }` to every reader of a mocked connection's `status()`. */
export function pushNatsStatus(type: string): void {
  for (const read of natsState.statusReaders) read({ type, data: "" });
}

// The dial `extensions/envoy.ts` opens NATS with, handed to it through `setNatsConnectForTests`
// (`useExtensionHarness` in `./extension-harness`). The `nats` module itself stays as it is: a row
// that sets `natsState.realConnect` gets nats.js's own connect from here, and every file in this
// `bun test` process — `card-delivery-omp.test.ts` among them — reads the real module. A row that
// hands the entry a plan of redials of its own hands it this stand-in again with that plan.
export const natsStandInConnect = async (options: {
  readonly name: string;
  readonly servers: readonly string[];
  readonly authenticator?: unknown;
}) => {
  const { name, servers, authenticator } = options;
  if (natsState.failConnects > 0) {
    natsState.failConnects -= 1;
    throw natsState.failError();
  }
  natsState.connectedNames.push(name);
  natsState.connects.push({ servers, authenticated: authenticator !== undefined });
  // A CONNECT asks the authenticator for its credential over the server's nonce, as nats.js does
  // at every connect and reconnect.
  const connectWith = (nonce: string) => {
    if (typeof authenticator !== "function") return;
    const credential = (authenticator as (nonce: string) => unknown)(nonce);
    if (typeof credential === "object" && credential !== null && "auth_token" in credential) {
      natsState.presented.push(String(credential.auth_token));
    }
  };
  connectWith("stand-in-nonce");
  // Parked only where a test asks for it: every other connect answers at once.
  if (natsState.heldConnect !== undefined) await natsState.heldConnect;
  if (natsState.realConnect) {
    return realConnect(options as unknown as Parameters<typeof realConnect>[0]);
  }
  let closed = false;
  // nats.js's `closed()`: settles once the connection is closed for good, with the close's error.
  const ended = Promise.withResolvers<Error | undefined>();
  const made = {
    isClosed: () => closed || natsState.connectionsClosed,
    closed: () => ended.promise,
    close: async () => {
      closed = true;
      natsState.closedNames.push(name);
      ended.resolve(natsState.closeError);
    },
    drain: async () => {
      natsState.drainStarted = true;
      if (natsState.drainHangs) await Promise.withResolvers<never>().promise;
    },
    // nats.js's client-initiated reconnect: the connection drops and comes back with a CONNECT of
    // its own, which every reader of its status hears of once it is back.
    reconnect: async () => {
      natsState.timeline.push("reconnect");
      connectWith("stand-in-nonce-again");
      setTimeout(() => pushNatsStatus("reconnect"), 0);
    },
    // An async iterable of the connection's status events, which a test feeds through
    // `pushNatsStatus`; it ends once the connection is closed.
    status: () => {
      const queued: { readonly type: string; readonly data: unknown }[] = [];
      let wake: (() => void) | undefined;
      const reader = (status: { readonly type: string; readonly data: unknown }) => {
        queued.push(status);
        wake?.();
        wake = undefined;
      };
      natsState.statusReaders.push(reader);
      return {
        [Symbol.asyncIterator]: () => ({
          next: async (): Promise<
            IteratorResult<{ readonly type: string; readonly data: unknown }>
          > => {
            for (;;) {
              if (closed) return { done: true, value: undefined };
              const status = queued.shift();
              if (status !== undefined) return { done: false, value: status };
              const pending = Promise.withResolvers<void>();
              wake = pending.resolve;
              await pending.promise;
            }
          },
        }),
      };
    },
    // nats.js's round trip: the server has every SUB written before it when this resolves, and
    // every pending one of them rejects when the connection drops. A rejection is transient
    // unless the connection is closed with it, which is the one case the extension gives up on.
    flush: async () => {
      natsState.flushes.push([...natsState.subscriptions.keys()]);
      if (natsState.heldFlush !== undefined) await natsState.heldFlush;
      const failing = natsState.failFlushOnSubject;
      if (failing !== undefined && natsState.controls.has(failing)) {
        natsState.failFlushOnSubject = undefined;
        if (natsState.failFlushClosesConnection) closed = true;
        throw new Error(`FLUSH_FAILED ${failing}`);
      }
    },
    publish: (subject: string, data?: Uint8Array) => {
      if (natsState.failPublishes > 0) {
        natsState.failPublishes -= 1;
        throw new Error("PUBLISH_FAILED");
      }
      if (fromEndedRow()) return;
      natsState.published.push({ subject, data });
      natsState.onPublish?.(subject);
    },
    subscribe: (topic: string) => {
      natsState.timeline.push(`subscribe ${topic}`);
      if (natsState.failSubscribes > 0) {
        natsState.failSubscribes -= 1;
        throw new Error("SUBSCRIBE_FAILED");
      }
      let active = true;
      const queue: {
        readonly subject: string;
        readonly data: Uint8Array;
        readonly reply: string;
        readonly headers?: MessageHeaders;
      }[] = [];
      let wake: (() => void) | undefined;
      let ended = false;
      let failure: Error | undefined;
      const notify = () => {
        wake?.();
        wake = undefined;
      };
      const controls = {
        push: (data: string, reply = "", headers?: Readonly<Record<string, string>>) => {
          queue.push({
            subject: topic,
            data: new TextEncoder().encode(data),
            reply,
            ...(headers === undefined
              ? {}
              : { headers: { get: (name: string) => headers[name] ?? "" } }),
          });
          notify();
        },
        end: () => {
          ended = true;
          notify();
        },
        fail: (error: Error) => {
          failure = error;
          notify();
        },
        active: () => active,
      };
      natsState.controls.set(topic, controls);
      const controlsForTopic = natsState.controlsByTopic.get(topic);
      if (controlsForTopic === undefined) natsState.controlsByTopic.set(topic, [controls]);
      else controlsForTopic.push(controls);
      const subscription: Subscription = {
        unsubscribe: () => {
          active = false;
          // nats.js ends the async iterator on unsubscribe(), which is what
          // makes a deliberate close indistinguishable from a dropped
          // connection inside the pump. A mock that leaves the iterator
          // parked cannot observe that, and lets resubscribe bugs ship green.
          notify();
        },
        [Symbol.asyncIterator]: () => ({
          next: async (): Promise<
            IteratorResult<{
              readonly subject: string;
              readonly data: Uint8Array;
              readonly reply: string;
              readonly headers?: MessageHeaders;
            }>
          > => {
            for (;;) {
              if (failure) throw failure;
              if (!active || ended) return { done: true, value: undefined };
              const item = queue.shift();
              if (item) return { done: false, value: item };
              const pending = Promise.withResolvers<void>();
              wake = pending.resolve;
              await pending.promise;
            }
          },
        }),
      };
      natsState.subscriptions.set(topic, subscription);
      return subscription;
    },
  };
  natsState.connections.push(made);
  return made;
};

/** The stream id every registration answer and relay frame of these suites names. */
export const TEST_STREAM_ID = "stream-test";

/** A server's answer, typed as Dispatch and the listener type theirs: a client reads a body as
 *  JSON only where the answer says it is JSON (`DispatchClient`'s `#response`). */
export function response(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    headers: { "Content-Type": "application/json" },
    status: 200,
  });
}

/** The listener's bus-outage body under `status`: with 503 it is the answer `isBusUnanswered`
 *  reads as a bus outage, and with any other status it is not. */
export function busOutageAnswer(status: number): Response {
  return Response.json(
    { error: `${BUS_UNANSWERED_PREFIX}: bus: NATS is reconnecting` },
    { status }
  );
}

/**
 * The listener's answer to a registration, a keyed write's answer to a Dispatch write, and
 * `fallback` to anything else. A Dispatch write (a POST or PATCH under `/api/v1/`) these tests do
 * not answer themselves gets its route's least answer, which the keyed layer every row's fetch
 * passes (`recordingFetch`) names the request's key back on, so the session's client reads it as
 * Dispatch's answer.
 */
export function responseWithRegistration(
  input: string | URL | Request,
  init: RequestInit | undefined,
  fallback: unknown
): Response {
  const request = new Request(input, init);
  const path = new URL(request.url).pathname;
  const written =
    (request.method === "POST" || request.method === "PATCH") && path.startsWith("/api/v1/")
      ? leastAnswerFor(request.method, path)
      : undefined;
  if (written !== undefined) {
    return new Response(JSON.stringify(written.body), {
      headers: { "Content-Type": "application/json" },
      status: written.status,
    });
  }
  if (path !== "/v1/interests/subscribe") return response(fallback);
  const body = JSON.parse(init?.body?.toString() ?? "{}") as {
    readonly session_id: string;
    readonly dir: string;
    readonly topics: readonly string[];
  };
  return response({
    session_id: body.session_id,
    machine_id: "test",
    dir: body.dir,
    topics: body.topics,
    delivery: consumerDelivery(body.session_id, { streamID: TEST_STREAM_ID }),
  });
}

/** The listener's grant of a claim of `role` to `session`, as `/v1/roles/set` answers it. */
export function claimGranted(session: string, role: string): Response {
  return response({
    session_id: session,
    machine_id: "test",
    dir: "/tmp/envoy-omp-test",
    topics: [`notifications.role.${role}`],
  });
}

/** Puts the stand-in back as no row has touched it: what each connection recorded, every
 *  subscription and the topics the relay carries, and each failure or hold a row set
 *  (`tearDownRow` in `./extension-harness`). */
export function resetNatsStandIn(): void {
  natsState.connectedNames.length = 0;
  natsState.connects.length = 0;
  natsState.presented.length = 0;
  natsState.timeline.length = 0;
  natsState.closedNames.length = 0;
  natsState.connections.length = 0;
  // A test aborted at Bun's own timeout never runs its `finally`: a connect left parked here
  // would park every later test's connect for the rest of the process.
  natsState.heldConnect = undefined;
  natsState.published.length = 0;
  natsState.onPublish = undefined;
  natsState.failPublishes = 0;
  natsState.subscriptions.clear();
  natsState.controls.clear();
  registeredTopics.clear();
  relaySeq = 0;
  natsState.controlsByTopic.clear();
  natsState.failConnects = 0;
  natsState.failError = () => NatsError.errorForCode(ErrorCode.ConnectionRefused);
  natsState.closeError = undefined;
  natsState.connectionsClosed = false;
  natsState.failSubscribes = 0;
  natsState.realConnect = false;
  natsState.drainHangs = false;
  natsState.drainStarted = false;
  natsState.statusReaders.length = 0;
  // As for `heldConnect`: a parked flush left behind would park every later test's subscribe.
  natsState.heldFlush = undefined;
  natsState.flushes.length = 0;
  natsState.failFlushOnSubject = undefined;
  natsState.failFlushClosesConnection = false;
}
