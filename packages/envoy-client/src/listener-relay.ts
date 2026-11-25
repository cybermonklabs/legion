// packages/envoy-client/src/listener-relay.ts
//
// A session's inbound messages through the Envoy listener's relay socket, for a session that has
// no NATS connection: an agent on a machine outside the deployment's network, which reaches the
// listener over HTTPS alone (`ENVOY_INBOUND=listener`, `envoySettings`).
//
// The wire. `GET <listener>/v1/sessions/<session>/relay` upgrades to a WebSocket of subprotocol
// `envoy-relay.v1`, presenting `Authorization: DPoP <token>` and a proof of the token's key whose
// `htm` is GET and whose `htu` is that URL in its http(s) form; the token is the session's own
// broker token, and a token naming another session is refused before the upgrade. Every frame is
// a text frame of JSON, and the listener reads a socket's frames one at a time, handling each in
// full before the next. The client sends `{"op":"sub","sid":<int>,"subject":<s>}`, which the
// listener grants only for the session's own relay subject (`deliverSubject`) and otherwise answers
// `{"op":"err","sid":<int>,"code":"subject_not_granted"}`, and grants one subscription a socket,
// answering a second `{"op":"err","sid":<int>,"code":"subscription_limit"}` and keeping the socket
// open (this client takes one per socket). Either refusal ends that subscription with an error
// naming its subject and code, logged once, and the socket goes on. A granted `sub` has no
// answer, so a `sub` is held once no `err` names its sid. It sends `{"op":"unsub","sid":<int>}`,
// and `{"op":"pub","subject":<s>,"data":<base64>}`, and nothing more, which the listener takes only
// on the reply of a frame this socket forwarded in the last 10 minutes and otherwise answers
// `{"op":"err","code":"publish_not_granted"}`. A frame the listener cannot read (no JSON, an unknown
// `op`, a `sub` or `unsub` naming no sid, a `sub` of a sid already held) it answers
// `{"op":"err","code":"bad_frame"}` and closes the socket 1008 with reason `bad_frame`. So a session
// publishes nothing here but its relay receipts (`RelayReply`), and neither the connection probe
// nor the agent stream is carried: a host opens no live view over this socket. The listener sends
// `{"op":"msg","sid":…,"subject":…,"reply":…,"headers":{…},"data":<base64>}`, whose `headers` maps
// each name as NATS carries it, case kept, to the list of its values in order (Go's `nats.Header`
// as JSON, `{}` for none), and pings every 20 s.
//
// The listener checks the token again every 20 s and closes an open socket by why it stopped:
// 4401 the token lapsed, 4403 it was revoked, no longer names what the socket was opened with or
// names a person Dispatch holds, 1013 the broker or NATS cannot be asked or whether the token's
// person may still act cannot be read, 1012 the listener is stopping. The client opens again,
// taking every live subscription again on the new socket (`relayReopenDelay`): at once after 4401
// and 1012, however often they come; after 1 s doubling to a minute after 4403; after 5 s doubling
// to a minute after 1013; at once after a proxy's 1001 or a socket that ended with no close frame
// (1006), then after 1 s doubling to a minute while they repeat; and after 1 s doubling to a
// minute after any other close, 1008 `bad_frame` included. An upgrade the listener refuses names
// its HTTP status in Bun's `error` event (`Expected 101 status code: <n>`): one refused 401 or 403
// waits as after 4403, one refused 503 after a 1013 goes on with the 1013's waits, and one refused
// any other status, an upgrade not answered within 20 s, a listener that cannot be reached and a
// failed trade are tried again after 1 s doubling to a minute. Each cause doubles its own waits
// (`relayRun`), and a socket that stayed open a minute starts every one afresh. Only 4401, 4403
// and an upgrade refused 401 or 403 make the next open trade for a fresh token, since only there
// was the token the cause; every open presents its token with a fresh proof. A trade that failed is
// told to the session once per outage (`InboundUnavailable`). The socket never ends an outage: the
// report is the host's, shared with its registrations, and one flag cannot say which of them told
// it, so only a registration the listener accepted ends it. Each open is followed by one, the
// start's or the one a `reconnect` (`status`) sets off. A caller that waits for a socket to open
// and whose bound ends first is answered `RelayNotOpenedError`, which names why the last open
// failed and carries the code that failure had where it got no answer: the runtime's code for the
// open's socket (ECONNREFUSED for a listener whose port nothing took, ENOTFOUND for a name that
// did not resolve), ETIMEDOUT for an upgrade not answered within 20 s, the trade's own code for a
// trade the broker did not answer (`failureCode`: ETIMEDOUT, ECONNREFUSED, …), and ETIMEDOUT where
// no open had failed yet and one was still waiting for its token or its upgrade's answer.
//
// The connection is an `InboundConnection` (`./inbound-connection.ts`), the shape of the NATS
// connection a session plugin holds, so a plugin's delivery runs on either: its subscriptions, a
// publish, a round trip (`flush`, a WebSocket ping, whose pong the listener's reader sends only
// after it handled every frame the socket sent before it, every `sub` included), the status events
// `disconnect` and `reconnect`, a drain and a close.

import { failureCode } from "@legion/contracts/log-text";
import { withoutTrailingSlashes } from "@legion/contracts/trailing-slash";
import type { RequestCredential } from "./bearer";
import { type BrokerSessionName, sessionName } from "./broker-credential";
import type { InboundUnavailable, RelayHeaders } from "./consumer-delivery";
import { LoggableError, messageFor } from "./errors";
import type {
  InboundConnection,
  InboundMessage,
  InboundStatus,
  InboundSubscription,
} from "./inbound-connection";
import { NATS_MAX_PINGS_OUT, NATS_PING_INTERVAL_MS } from "./nats-connection";
import { FOLLOW_DEADLINE_MS, TRADE_DEADLINE_MS } from "./nats-credential";

/** The subprotocol a relay socket speaks. */
export const RELAY_SUBPROTOCOL = "envoy-relay.v1";

/** How long a socket stays open before its next close starts every run of waits afresh. */
const RELAY_STABLE_MS = 60_000;
/** The longest wait before a socket opens again. */
const RELAY_LAST_WAIT_MS = 60_000;
/** The first wait after a credential refusal (4403, an upgrade refused 401 or 403), a socket that
 *  never opened, a failed trade and a close this file has no rule of its own for. */
const RELAY_FIRST_WAIT_MS = 1_000;
/** The first wait after 1013: the listener could not ask the broker or NATS. */
const RELAY_BROKER_FIRST_WAIT_MS = 5_000;
/** How long an upgrade may go unanswered before its socket is dropped and opened again: Bun keeps
 *  one a path took and never answered connecting, with no close, for as long as the path holds it. */
const RELAY_HANDSHAKE_MS = 20_000;

/** The close codes the listener closes an open relay socket with. */
const CLOSE_EXPIRED = 4401;
const CLOSE_REFUSED = 4403;
const CLOSE_TRY_AGAIN_LATER = 1013;
const CLOSE_STOPPING = 1012;
/** The listener could not read a frame this client sent (`bad_frame`). */
const CLOSE_BAD_FRAME = 1008;
/** The largest frame a listener sends: one NATS message, at most 1 MiB (the hub's `max_payload`),
 *  as base64 inside the frame's JSON, about 1.4 MiB, with room for its headers. A larger frame is
 *  no listener's, and is dropped unread. The listener's own bound, 1 MiB, is on what it reads. */
const RELAY_MAX_FRAME_BYTES = 2 * 1024 * 1024;
/** Opened again at once however often they come: the token lapsed (4401) or the listener is
 *  stopping (1012), which the listener sends at most once a check and once a stop. */
const AT_ONCE: ReadonlySet<number> = new Set([CLOSE_EXPIRED, CLOSE_STOPPING]);
/** Opened again at once, then after a wait while they repeat: a proxy in front of the listener is
 *  stopping (1001), or the socket ended with no close frame at all (1006), as a path that froze
 *  does once this file stops waiting for its pings. Either can come as fast as a socket opens. */
const AT_ONCE_FIRST: ReadonlySet<number> = new Set([1001, 1006]);
/** The upgrade statuses that say the token was the cause, so the next open trades for another. */
const TOKEN_REFUSED: ReadonlySet<number> = new Set([401, 403]);
/** The upgrade status of a listener that cannot ask the broker, as after its 1013. */
const UPGRADE_UNAVAILABLE = 503;
/** Where Bun's `error` event names the status an upgrade was answered with. */
const UPGRADE_STATUS = /Expected 101 status code: (\d{3})\b/;
/** The `err` codes that refuse one subscription and leave the socket open: a subject the socket
 *  does not carry, and a subscription past the one a socket holds. */
const SUBSCRIPTION_REFUSALS: Readonly<Record<string, true>> = {
  subject_not_granted: true,
  subscription_limit: true,
};

/** Every `err` code the wire names (this file's header). */
const ERR_CODES: Readonly<Record<string, true>> = {
  ...SUBSCRIPTION_REFUSALS,
  publish_not_granted: true,
  bad_frame: true,
};

/** How long a drain waits for the listener to have taken its unsubscribes. */
const DRAIN_BOUND_MS = 1_000;

/** Bun's WebSocket client, which takes the upgrade's headers and sends pings. The package's lib
 *  loads the DOM's `WebSocket` type over it, which has neither, so the constructor is named here
 *  as Bun declares it (`Bun.WebSocketOptions`, `Bun.WebSocket`). */
const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: Bun.WebSocketOptions
) => Bun.WebSocket;

/** The relay socket of `session` at the listener `url`, in its http(s) form: what the proof of
 *  the upgrade names as its `htu`. */
export function relaySocketUrl(url: string, session: string): string {
  return `${withoutTrailingSlashes(url)}/v1/sessions/${encodeURIComponent(session)}/relay`;
}

/**
 * How one open of a relay socket ended: a socket that opened closed with the code `closed`; or
 * none opened, its upgrade refused with the HTTP status `refused`, undefined where none was read
 * (the listener could not be reached, the upgrade went unanswered, the trade failed), in a run of
 * such opens that a close of code `after` began, undefined where no close did.
 */
export type RelayEnd =
  | { readonly closed: number }
  | { readonly refused: number | undefined; readonly after: number | undefined };

/** The run of waits an end counts in (`relayRun`): the token's (4403, an upgrade refused 401 or
 *  403), the broker's (1013, and the upgrades refused 503 after it), every open that never opened,
 *  or each other close code's own. */
export type RelayRun = "token" | "broker" | "unopened" | `closed ${number}`;

/**
 * The run of waits `end` counts in, each doubling apart from every other: one per close code, an
 * upgrade refused 401 or 403 counted with 4403 (the token was the cause of each), one refused 503
 * after a 1013 with that 1013, and every other open that never opened together.
 */
export function relayRun(end: RelayEnd): RelayRun {
  if ("closed" in end) {
    if (end.closed === CLOSE_REFUSED) return "token";
    if (end.closed === CLOSE_TRY_AGAIN_LATER) return "broker";
    return `closed ${end.closed}`;
  }
  if (end.refused !== undefined && TOKEN_REFUSED.has(end.refused)) return "token";
  if (end.refused === UPGRADE_UNAVAILABLE && end.after === CLOSE_TRY_AGAIN_LATER) return "broker";
  return "unopened";
}

/** Whether `end` says the token was the cause, so the next open trades for a fresh one: 4401 (it
 *  lapsed), and every end of the token's run. */
function tokenCaused(end: RelayEnd): boolean {
  return ("closed" in end && end.closed === CLOSE_EXPIRED) || relayRun(end) === "token";
}

/** How long to wait before opening again after `end`, the `streak`th end of its run (`relayRun`)
 *  with no socket between that stayed open a minute. */
export function relayReopenDelay(end: RelayEnd, streak: number): number {
  const doubling = (first: number, times: number): number =>
    Math.min(first * 2 ** times, RELAY_LAST_WAIT_MS);
  if ("closed" in end && AT_ONCE.has(end.closed)) return 0;
  // At once for the first; a socket that keeps closing so right after it opened waits as a refusal
  // does, so it costs the broker and the listener one trade and one upgrade a minute at most.
  if ("closed" in end && AT_ONCE_FIRST.has(end.closed)) {
    return streak === 0 ? 0 : doubling(RELAY_FIRST_WAIT_MS, streak - 1);
  }
  if (relayRun(end) === "broker") return doubling(RELAY_BROKER_FIRST_WAIT_MS, streak);
  return doubling(RELAY_FIRST_WAIT_MS, streak);
}

/** An async queue: what is pushed is read in order, and reading ends once it is ended and empty,
 *  or rejects with the error it failed with once that is read. */
class Queue<T> implements AsyncIterable<T> {
  readonly #items: T[] = [];
  readonly #waiting: {
    readonly resolve: (result: IteratorResult<T>) => void;
    readonly reject: (error: Error) => void;
  }[] = [];
  #ended = false;
  #failed: Error | undefined;
  readonly #onReturn: () => void;

  constructor(onReturn: () => void) {
    this.#onReturn = onReturn;
  }

  push(item: T): void {
    if (this.#ended) return;
    const waiting = this.#waiting.shift();
    if (waiting === undefined) this.#items.push(item);
    else waiting.resolve({ done: false, value: item });
  }

  end(): void {
    if (this.#ended) return;
    this.#ended = true;
    for (const waiting of this.#waiting.splice(0))
      waiting.resolve({ done: true, value: undefined });
  }

  /** Ends the queue with `error`: a reader takes what was pushed before it, then the error, as a
   *  nats.js subscription a server refused ends with its permissions violation. */
  fail(error: Error): void {
    if (this.#ended) return;
    this.#ended = true;
    this.#failed = error;
    for (const waiting of this.#waiting.splice(0)) waiting.reject(error);
  }

  [Symbol.asyncIterator](): AsyncIterator<T> {
    return {
      next: () => {
        if (this.#items.length > 0) {
          return Promise.resolve({ done: false, value: this.#items.shift() as T });
        }
        if (this.#failed !== undefined) return Promise.reject(this.#failed);
        if (this.#ended) return Promise.resolve({ done: true, value: undefined });
        const { promise, resolve, reject } = Promise.withResolvers<IteratorResult<T>>();
        this.#waiting.push({ resolve, reject });
        return promise;
      },
      // A reader that stops early (`break`) ends what it read, as a nats.js iterator does.
      return: () => {
        this.#onReturn();
        this.end();
        return Promise.resolve({ done: true, value: undefined });
      },
    };
  }
}

/**
 * A relay frame's headers as the socket carries them: each name as NATS carries it, case kept,
 * matched exactly. `get` answers its first value and "" for a name the frame does not carry, as
 * nats.js's `MsgHdrs.get` does, which is what `decodeRelayFrame` reads; `values` answers every value
 * in order, and none for such a name, as its `values` does.
 */
export interface RelaySocketHeaders extends RelayHeaders {
  get(name: string): string;
  values(name: string): readonly string[];
}

/** A message on one of the relay socket's subscriptions. */
export interface RelaySocketMessage extends InboundMessage {
  readonly headers: RelaySocketHeaders;
}

/** A subscription of the relay socket. */
export interface RelaySocketSubscription extends InboundSubscription {
  [Symbol.asyncIterator](): AsyncIterator<RelaySocketMessage>;
}

/** One subscription on the relay socket, taken again on every socket the connection opens. */
class RelaySubscription implements RelaySocketSubscription {
  readonly sid: number;
  readonly subject: string;
  readonly #queue: Queue<RelaySocketMessage>;

  constructor(sid: number, subject: string, unsubscribe: () => void) {
    this.sid = sid;
    this.subject = subject;
    this.#queue = new Queue(unsubscribe);
    this.unsubscribe = unsubscribe;
  }

  readonly unsubscribe: () => void;

  push(message: RelaySocketMessage): void {
    this.#queue.push(message);
  }

  end(): void {
    this.#queue.end();
  }

  fail(error: Error): void {
    this.#queue.fail(error);
  }

  [Symbol.asyncIterator](): AsyncIterator<RelaySocketMessage> {
    return this.#queue[Symbol.asyncIterator]();
  }
}

/** A `msg` frame's `headers`, or undefined when it is not the map of names to lists of strings
 *  this file's header describes: such a frame is no relay frame, and is dropped and logged rather
 *  than read. */
function relayHeaders(raw: unknown): RelaySocketHeaders | undefined {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return undefined;
  const values = new Map<string, readonly string[]>();
  for (const [name, value] of Object.entries(raw)) {
    if (!Array.isArray(value) || !value.every((one) => typeof one === "string")) return undefined;
    values.set(name, value);
  }
  return {
    get: (name) => values.get(name)?.[0] ?? "",
    values: (name) => values.get(name) ?? [],
  };
}

/** A frame's or a pong's bytes as text, however Bun hands them over. */
function frameText(data: unknown): string {
  if (typeof data === "string") return data;
  if (data instanceof ArrayBuffer) return Buffer.from(data).toString("utf8");
  if (ArrayBuffer.isView(data)) {
    return Buffer.from(data.buffer, data.byteOffset, data.byteLength).toString("utf8");
  }
  return "";
}

/**
 * What the socket may tell the host's outage report: a refusal, and never its end. The report is
 * the host's, shared with its registrations, and only a registration the listener accepted ends
 * it, so a socket that could call `accepted` would not compile.
 */
export type RelayOutage = Pick<InboundUnavailable, "refused">;

/** What `ListenerRelay` is opened with. */
export interface ListenerRelayOptions {
  /** The listener's address (`EnabledEnvoySettings.url`); the socket is `relaySocketUrl`'s. */
  readonly url: string;
  /** The session the socket is for, read at every open: a host that moves to another session
   *  opens its socket under the new one (`ListenerRelay.follow`). */
  readonly session: BrokerSessionName;
  /** The session's broker credential (`brokerCredential`), which each upgrade presents with a
   *  fresh proof, and whose token is dropped (`stale`) where it was the cause of a close or a
   *  refused upgrade, so the next open trades. */
  readonly credential: RequestCredential;
  /** Told a failed trade once per outage: the host's own report (`inboundUnavailable`), which its
   *  registrations share and whose accepted registration alone ends the outage. */
  readonly outage: RelayOutage;
  /** Where a line about the socket goes: a close, a refusal, a socket that stopped answering. */
  readonly log: (line: string) => void;
}

/**
 * The relay socket did not open within its caller's bound (`connectListenerRelay`,
 * `ListenerRelay.opened`, `ListenerRelay.follow`). Its message names why the last open failed, and
 * `code` is that failure's code where the far end gave no answer: the code the runtime gave that
 * open's socket (`failureCode`: ECONNREFUSED, ENOTFOUND, ECONNRESET, …), ETIMEDOUT for an upgrade
 * not answered within 20 s, and the code of a trade the broker did not answer (ETIMEDOUT,
 * ECONNREFUSED, ENOTFOUND, …, as the broker client keeps it). Where no open had failed yet and one
 * was still waiting for its token or its upgrade's answer when the bound ended, it is ETIMEDOUT,
 * and the message says so. That is what tells a far end that answered nothing from one that
 * answered: `code` is undefined where the listener or the broker answered (a refused upgrade, a
 * refused trade) and where a socket that opened was closed by the listener. A `LoggableError`, so
 * it keeps no cause: the runtime's error would repeat the socket's URL, and the code is all of it
 * a caller reads.
 */
export class RelayNotOpenedError extends LoggableError {
  override readonly name = "RelayNotOpenedError";
  readonly code: string | undefined;

  constructor(message: string, code: string | undefined) {
    super(message);
    this.code = code;
  }
}

/** A pending round trip: the ping it sent, and its answer. */
interface Pong {
  readonly seq: number;
  readonly resolve: () => void;
  readonly reject: (error: Error) => void;
}

/** How a socket ended, as its close event and the events before it said. */
interface SocketEnd {
  readonly end: RelayEnd;
  /** The close's code and its reason, as a log line names them. */
  readonly why: string;
  /** Why it never opened, where this file knows better than its close code. */
  readonly refusal: string | undefined;
  /** What Bun's `error` event said of it, "" where it said nothing. */
  readonly failure: string;
  /** The code the runtime gave a socket that never opened (`failureCode`), ETIMEDOUT for an
   *  upgrade not answered in time, undefined where the listener answered or there was none. */
  readonly code: string | undefined;
}

/**
 * A session's inbound connection over the listener's relay socket (this file's header says the
 * wire and how each close is answered). Open it with `connectListenerRelay`.
 */
export class ListenerRelay implements InboundConnection {
  readonly #options: ListenerRelayOptions;
  #outage: RelayOutage;
  /** The socket opening or open, and the session and the presentation it was opened with. */
  #socket: Bun.WebSocket | undefined;
  #socketSession = "";
  #presented: Readonly<Record<string, string>> | undefined;
  /** Bumped at every open and at the close: an event of an earlier socket changes nothing. */
  #generation = 0;
  #everOpened = false;
  /** Each run's ends in a row (`relayRun`) with no socket between that stayed open
   *  `RELAY_STABLE_MS`. */
  readonly #streaks = new Map<RelayRun, number>();
  /** The code of the close that began the run of opens that have not opened since, if one did. */
  #closedWith: number | undefined;
  #stableTimer: ReturnType<typeof setTimeout> | undefined;
  #reopenTimer: ReturnType<typeof setTimeout> | undefined;
  #livenessTimer: ReturnType<typeof setInterval> | undefined;
  #pingSeq = 0;
  /** The ops of the frames sent since the listener last answered a ping, each with the ping
   *  sequence it went out after: a pong past it says the listener read it. A `bad_frame` close
   *  names these, since the listener's answer names no op. */
  readonly #sentSincePong = new Map<string, number>();
  #pongSeen = 0;
  #pongs: Pong[] = [];
  #openWaiters: { readonly resolve: () => void; readonly reject: (error: Error) => void }[] = [];
  /** Why the last open failed, which a caller that waited for one is told, and the code the
   *  runtime gave its socket, which that caller reads (`RelayNotOpenedError`). */
  #lastFailure = "";
  #lastFailureCode: string | undefined;
  /** Whether an open is under way: its trade, or its socket's upgrade, not yet answered. False
   *  once it opened or failed, and while the next open waits its turn after a close. */
  #openUnderWay = false;
  /** The failure the last line logged, so a run of the same one is logged once (`#failed`). */
  #loggedFailure = "";
  readonly #subs = new Map<number, RelaySubscription>();
  #nextSid = 0;
  readonly #statuses = new Set<Queue<InboundStatus>>();
  #closedForGood = false;
  readonly #closed = Promise.withResolvers<void>();
  /** What has been logged once: a refused subscription, by session, code and subject, and any
   *  other error code. */
  readonly #logged = new Set<string>();

  constructor(options: ListenerRelayOptions) {
    this.#options = options;
    this.#outage = options.outage;
    this.#open();
  }

  /** Has a later failed trade told `outage`, rather than the one this was opened with: a host
   *  whose own report is made only after the socket opened. */
  reportOutagesTo(outage: RelayOutage): void {
    this.#outage = outage;
  }

  subscribe(subject: string): RelaySocketSubscription {
    if (this.#closedForGood) throw new LoggableError("the listener's relay socket is closed");
    this.#nextSid += 1;
    const sid = this.#nextSid;
    const subscription = new RelaySubscription(sid, subject, () => this.#unsubscribe(sid));
    this.#subs.set(sid, subscription);
    this.#send({ op: "sub", sid, subject });
    return subscription;
  }

  /**
   * Publishes `data` on `subject`, which the listener takes only as the receipt of a frame this
   * socket forwarded. One made while no socket is open is dropped: a receipt is good only on the
   * socket that forwarded its frame, and the frame comes again once a socket is open.
   */
  publish(subject: string, data?: Uint8Array): void {
    if (this.#closedForGood) throw new LoggableError("the listener's relay socket is closed");
    this.#send({
      op: "pub",
      subject,
      data: Buffer.from(data ?? new Uint8Array()).toString("base64"),
    });
  }

  /**
   * Waits for a socket to be open, then sends a ping and waits for its pong: the listener answers
   * a ping after it has read every frame sent before it. Rejects when that socket closes first, or
   * is dropped for another (`follow`), as a NATS round trip does at a disconnect, or when the
   * connection is closed for good: only the socket a ping went out on can say what it took.
   */
  async flush(): Promise<void> {
    const socket = await this.#openSocket();
    if (socket !== this.#socket) {
      throw new LoggableError("the listener's relay socket closed before the flush was sent");
    }
    this.#pingSeq += 1;
    const seq = this.#pingSeq;
    const answered = Promise.withResolvers<void>();
    this.#pongs.push({ seq, resolve: answered.resolve, reject: answered.reject });
    socket.ping(String(seq));
    await answered.promise;
  }

  isClosed(): boolean {
    return this.#closedForGood;
  }

  /** Unsubscribes every subscription, waits a little for the listener to have taken that, and
   *  closes. */
  async drain(): Promise<void> {
    if (this.#closedForGood) return;
    for (const sid of [...this.#subs.keys()]) this.#unsubscribe(sid);
    const bound = Promise.withResolvers<void>();
    const timer = setTimeout(bound.resolve, DRAIN_BOUND_MS);
    await Promise.race([this.flush().catch(() => undefined), bound.promise]);
    clearTimeout(timer);
    await this.close();
  }

  async close(): Promise<void> {
    if (this.#closedForGood) return;
    this.#closedForGood = true;
    this.#generation += 1;
    this.#stopTimers();
    clearTimeout(this.#reopenTimer);
    const socket = this.#socket;
    this.#socket = undefined;
    socket?.close(1000, "the session closed its relay socket");
    for (const subscription of this.#subs.values()) subscription.end();
    this.#subs.clear();
    for (const status of this.#statuses) status.end();
    this.#statuses.clear();
    const closed = new LoggableError("the listener's relay socket is closed");
    for (const pong of this.#pongs.splice(0)) pong.reject(closed);
    for (const waiter of this.#openWaiters.splice(0)) waiter.reject(closed);
    this.#closed.resolve();
  }

  status(): AsyncIterable<InboundStatus> {
    const queue: Queue<InboundStatus> = new Queue(() => this.#statuses.delete(queue));
    if (this.#closedForGood) queue.end();
    else this.#statuses.add(queue);
    return queue;
  }

  closed(): Promise<void> {
    return this.#closed.promise;
  }

  /**
   * Opens the socket again under the session the name reads now, when that is not the session the
   * socket is for, and waits until a socket of it is open, for at most `boundMs`: what a host runs
   * when it moves to another session, before it takes that session's relay subject. The listener
   * grants a socket only its own session's subject, under that session's own token. A bound that
   * ends first throws `RelayNotOpenedError`.
   */
  async follow(boundMs: number = FOLLOW_DEADLINE_MS): Promise<void> {
    if (this.#closedForGood) throw new LoggableError("the listener's relay socket is closed");
    const session = sessionName(this.#options.session);
    if (session !== this.#socketSession) {
      this.#streaks.clear();
      this.#closedWith = undefined;
      this.#open();
    }
    const bound = AbortSignal.timeout(boundMs);
    await this.#openSocket(bound).catch((error: unknown) => {
      if (!bound.aborted) throw error;
      throw this.#notOpened(
        `the listener's relay socket did not open as the session ${session} within ${Math.round(boundMs / 1000)} s`
      );
    });
  }

  /** Waits for the first socket to open, for as long as `signal` lets it; once it ends, throws
   *  `RelayNotOpenedError`. */
  async opened(signal?: AbortSignal): Promise<void> {
    await this.#openSocket(signal).catch((error: unknown) => {
      if (signal?.aborted !== true) throw error;
      throw this.#notOpened(`the listener's relay socket for ${this.#socketSession} did not open`);
    });
  }

  /** What a caller whose bound ended before a socket opened is told: `said`, then why the last
   *  open failed, with that failure's code; or, where none had failed and an open was still under
   *  way, that it was still waiting, ETIMEDOUT: its trade or its upgrade got no answer in time. */
  #notOpened(said: string): RelayNotOpenedError {
    if (this.#lastFailure === "" && this.#openUnderWay) {
      return new RelayNotOpenedError(
        `${said}: its open was still waiting for its token or its upgrade's answer when the bound ended`,
        "ETIMEDOUT"
      );
    }
    return new RelayNotOpenedError(
      this.#lastFailure === "" ? said : `${said}: ${this.#lastFailure}`,
      this.#lastFailureCode
    );
  }

  /** The open socket, once there is one; rejects once the connection is closed for good, and
   *  when `signal` ends first. */
  #openSocket(signal?: AbortSignal): Promise<Bun.WebSocket> {
    const ready = (): Bun.WebSocket | undefined =>
      this.#socket?.readyState === WebSocket.OPEN &&
      this.#socketSession === sessionName(this.#options.session)
        ? this.#socket
        : undefined;
    const { promise, resolve, reject } = Promise.withResolvers<Bun.WebSocket>();
    if (this.#closedForGood) {
      reject(new LoggableError("the listener's relay socket is closed"));
      return promise;
    }
    if (signal?.aborted === true) {
      reject(signal.reason);
      return promise;
    }
    const now = ready();
    if (now !== undefined) {
      resolve(now);
      return promise;
    }
    const stop = () => {
      const index = this.#openWaiters.indexOf(waiter);
      if (index >= 0) this.#openWaiters.splice(index, 1);
      reject(signal?.reason);
    };
    const waiter = {
      resolve: () => {
        const socket = ready();
        // A socket of a session the host has since moved off: wait for the next one.
        if (socket === undefined) {
          this.#openWaiters.push(waiter);
          return;
        }
        signal?.removeEventListener("abort", stop);
        resolve(socket);
      },
      reject: (error: Error) => {
        signal?.removeEventListener("abort", stop);
        reject(error);
      },
    };
    this.#openWaiters.push(waiter);
    signal?.addEventListener("abort", stop, { once: true });
    return promise;
  }

  #send(frame: { readonly op: string } & Readonly<Record<string, unknown>>): void {
    const socket = this.#socket;
    if (socket?.readyState !== WebSocket.OPEN) return;
    socket.send(JSON.stringify(frame));
    this.#sentSincePong.set(frame.op, this.#pingSeq);
  }

  #unsubscribe(sid: number): void {
    const subscription = this.#subs.get(sid);
    if (subscription === undefined) return;
    this.#subs.delete(sid);
    subscription.end();
    this.#send({ op: "unsub", sid });
  }

  #logOnce(key: string, line: string): void {
    if (this.#logged.has(key)) return;
    this.#logged.add(key);
    this.#options.log(line);
  }

  #emit(type: InboundStatus["type"]): void {
    for (const status of this.#statuses) status.push({ type });
  }

  #stopTimers(): void {
    clearTimeout(this.#stableTimer);
    clearInterval(this.#livenessTimer);
    this.#stableTimer = undefined;
    this.#livenessTimer = undefined;
  }

  /** Opens a socket now, under the session the name reads now, dropping the socket there is and
   *  every flush waiting on it, which no later socket can answer. */
  #open(): void {
    if (this.#closedForGood) return;
    clearTimeout(this.#reopenTimer);
    this.#reopenTimer = undefined;
    this.#openUnderWay = true;
    this.#generation += 1;
    const generation = this.#generation;
    const previous = this.#socket;
    this.#socket = undefined;
    this.#stopTimers();
    previous?.close(1000, "the session opens its relay socket again");
    const dropped = new LoggableError(
      "the listener's relay socket was opened again before it answered the flush"
    );
    for (const pong of this.#pongs.splice(0)) pong.reject(dropped);
    this.#sentSincePong.clear();
    const session = sessionName(this.#options.session);
    this.#socketSession = session;
    void this.#upgradeHeaders(session).then((upgrade) => {
      // An open a later one replaced says nothing, its failure included: `follow` may have moved
      // to another session, whose report this is now.
      if (generation !== this.#generation) return;
      if ("failed" in upgrade) {
        this.#outage.refused(upgrade.error);
        const delay = this.#reopenAfter({ refused: undefined, after: this.#closedWith });
        // A trade the broker did not answer carries its code (`failureCode`), a refusal none.
        this.#failed(session, upgrade.failed, delay, failureCode(upgrade.error));
        return;
      }
      this.#connect(generation, session, upgrade.headers);
    });
  }

  /** Records why an open failed, and the code its failure had where it got no answer (its
   *  socket's, or its trade's), which a caller waiting for one is told, and logs it, with the wait
   *  before the next, when it is not what the last failure logged said: a listener or a broker
   *  away for an hour costs one line, not one per attempt. A socket that opens ends the run. */
  #failed(session: string, failure: string, delay: number, code?: string): void {
    this.#lastFailure = failure;
    this.#lastFailureCode = code;
    if (failure === this.#loggedFailure) return;
    this.#loggedFailure = failure;
    this.#options.log(
      `the listener's relay socket for ${session} could not be opened: ${failure}; it is tried again in ${delay} ms, the wait doubling to a minute while it fails`
    );
  }

  /** The upgrade's `Authorization` and proof for `session`, or why they could not be had: a trade
   *  that failed, which the open that asked tells once per outage, if it still stands. */
  async #upgradeHeaders(
    session: string
  ): Promise<
    | { readonly headers: Readonly<Record<string, string>> }
    | { readonly failed: string; readonly error: unknown }
  > {
    try {
      if (session === "") {
        throw new LoggableError("this process names no session for its relay socket");
      }
      const headers = await this.#options.credential.headers(
        "GET",
        relaySocketUrl(this.#options.url, session),
        session,
        AbortSignal.timeout(TRADE_DEADLINE_MS)
      );
      return { headers };
    } catch (error) {
      return { failed: `its token could not be had (${messageFor(error)})`, error };
    }
  }

  #connect(generation: number, session: string, headers: Readonly<Record<string, string>>): void {
    this.#presented = headers;
    const target = relaySocketUrl(this.#options.url, session).replace(/^http/, "ws");
    let socket: Bun.WebSocket;
    try {
      socket = new BunWebSocket(target, { headers, protocols: RELAY_SUBPROTOCOL });
    } catch (error) {
      const delay = this.#reopenAfter({ refused: undefined, after: this.#closedWith });
      this.#failed(session, `the socket could not be made (${messageFor(error)})`, delay);
      return;
    }
    this.#socket = socket;
    let opened = false;
    // Why this socket closed before it opened, where this file knows better than its close code:
    // an upgrade not answered in time.
    let refusal: string | undefined;
    // What Bun said of a socket that failed before it opened: the status an upgrade was refused
    // with (`UPGRADE_STATUS`), or why it could not be made, and the code its error carried.
    let failure = "";
    let code: string | undefined;
    const handshake = setTimeout(() => {
      if (generation !== this.#generation || socket.readyState !== WebSocket.CONNECTING) return;
      refusal = `the upgrade was not answered within ${RELAY_HANDSHAKE_MS / 1000} s`;
      socket.close();
    }, RELAY_HANDSHAKE_MS);
    handshake.unref?.();
    // Bun refuses an upgrade answered with no subprotocol or another one itself, before `open`
    // ("Missing client protocol", "Mismatch client protocol", then a close 1006), so a socket that
    // opens speaks `envoy-relay.v1`.
    socket.addEventListener("open", () => {
      clearTimeout(handshake);
      if (generation !== this.#generation) return;
      opened = true;
      this.#opened(generation, session, socket);
    });
    // Bun's event map names the DOM's `MessageEvent` and `ErrorEvent` values where their types are
    // meant, so `data`, `message` and `error` are read through the guard rather than the declared
    // type. Bun 1.4.4 sets the runtime's code on the event's `error` (ECONNREFUSED for a port
    // nothing took, ENOTFOUND for a name that did not resolve, measured).
    socket.addEventListener("error", (event: Event) => {
      if ("message" in event && typeof event.message === "string") failure = event.message;
      if ("error" in event) code = failureCode(event.error);
    });
    socket.addEventListener("message", (event: Event) => {
      if (generation !== this.#generation) return;
      this.#message(session, "data" in event ? event.data : undefined);
    });
    socket.addEventListener("pong", (event: Event) => {
      if (generation !== this.#generation) return;
      this.#pong(frameText("data" in event ? event.data : undefined));
    });
    socket.addEventListener("close", (event) => {
      clearTimeout(handshake);
      const status = opened ? undefined : UPGRADE_STATUS.exec(failure)?.[1];
      this.#socketClosed(generation, session, socket, {
        end: opened
          ? { closed: event.code }
          : { refused: status === undefined ? undefined : Number(status), after: this.#closedWith },
        why: event.reason === "" ? `${event.code}` : `${event.code} ${event.reason}`,
        refusal,
        failure,
        // An upgrade the listener answered with a status is its answer, whatever Bun's error
        // carried; one this file stopped waiting for got none.
        code:
          opened || status !== undefined ? undefined : refusal === undefined ? code : "ETIMEDOUT",
      });
    });
  }

  #opened(generation: number, session: string, socket: Bun.WebSocket): void {
    const reopened = this.#everOpened;
    this.#everOpened = true;
    this.#openUnderWay = false;
    this.#lastFailure = "";
    this.#lastFailureCode = undefined;
    this.#loggedFailure = "";
    this.#stableTimer = setTimeout(() => {
      if (generation === this.#generation) this.#streaks.clear();
    }, RELAY_STABLE_MS);
    this.#stableTimer.unref?.();
    this.#closedWith = undefined;
    // A path that froze answers no ping, and nothing else would say so: two unanswered in a row,
    // as a NATS connection counts them (`nats-connection.ts`), end the socket, and it opens again.
    const pingsAtStart = this.#pingSeq;
    this.#pongSeen = Math.max(this.#pongSeen, pingsAtStart);
    this.#livenessTimer = setInterval(() => {
      if (generation !== this.#generation) return;
      if (this.#pingSeq - this.#pongSeen >= NATS_MAX_PINGS_OUT) {
        this.#lastFailure = `the socket answered no ping for ${Math.round((NATS_MAX_PINGS_OUT * NATS_PING_INTERVAL_MS) / 1000)} s`;
        this.#lastFailureCode = undefined;
        this.#options.log(`the listener's relay socket for ${session}: ${this.#lastFailure}`);
        socket.terminate();
        return;
      }
      this.#pingSeq += 1;
      socket.ping(String(this.#pingSeq));
    }, NATS_PING_INTERVAL_MS);
    this.#livenessTimer.unref?.();
    for (const subscription of this.#subs.values()) {
      this.#send({ op: "sub", sid: subscription.sid, subject: subscription.subject });
    }
    for (const waiter of this.#openWaiters.splice(0)) waiter.resolve();
    if (reopened) this.#emit("reconnect");
  }

  #pong(text: string): void {
    const seq = Number(text);
    if (!Number.isInteger(seq) || seq <= 0) return;
    this.#pongSeen = Math.max(this.#pongSeen, seq);
    const answered = this.#pongs.filter((pong) => pong.seq <= seq);
    this.#pongs = this.#pongs.filter((pong) => pong.seq > seq);
    for (const pong of answered) pong.resolve();
    for (const [op, after] of this.#sentSincePong) {
      if (after < seq) this.#sentSincePong.delete(op);
    }
  }

  #message(session: string, raw: unknown): void {
    const bytes =
      typeof raw === "string"
        ? Buffer.byteLength(raw)
        : raw instanceof ArrayBuffer || ArrayBuffer.isView(raw)
          ? raw.byteLength
          : 0;
    if (bytes > RELAY_MAX_FRAME_BYTES) {
      // Left unanswered, a frame's message comes again after the session consumer's ack wait.
      this.#logOnce(
        `${session}\u0000size`,
        `the listener's relay socket for ${session} sent a frame past 2 MiB, which no relay frame is; it is dropped, and its message comes again after the consumer's ack wait`
      );
      return;
    }
    let frame: unknown;
    try {
      frame = JSON.parse(frameText(raw));
    } catch {
      this.#logOnce(
        `${session}\u0000json`,
        `the listener's relay socket for ${session} sent a frame that is no JSON`
      );
      return;
    }
    if (typeof frame !== "object" || frame === null || !("op" in frame)) return;
    if (frame.op === "msg" && "sid" in frame && "subject" in frame && "data" in frame) {
      const subscription = this.#subs.get(Number(frame.sid));
      if (subscription === undefined) return;
      const headers = "headers" in frame ? relayHeaders(frame.headers) : undefined;
      if (headers === undefined) {
        // No relay frame: the listener always sends the header map this file's header describes.
        // Left unanswered, the frame comes again after the session consumer's ack wait.
        this.#logOnce(
          `${session}\u0000headers`,
          `the listener's relay socket for ${session} sent a msg whose headers are not a map of names to lists of strings; it is dropped`
        );
        return;
      }
      const reply = "reply" in frame && typeof frame.reply === "string" ? frame.reply : undefined;
      subscription.push({
        subject: String(frame.subject),
        data: Buffer.from(String(frame.data), "base64"),
        ...(reply === undefined ? {} : { reply }),
        headers,
      });
      return;
    }
    if (frame.op !== "err" || !("code" in frame)) return;
    const code = String(frame.code);
    const subscription = "sid" in frame ? this.#subs.get(Number(frame.sid)) : undefined;
    if (subscription !== undefined && SUBSCRIPTION_REFUSALS[code] === true) {
      // A subject this socket does not carry, or a subscription past the most a socket holds:
      // that subscription ends with an error naming the subject and code, as a NATS server's
      // permissions violation ends a nats.js one, so its reader is told why; every other goes on.
      this.#subs.delete(subscription.sid);
      subscription.fail(
        new LoggableError(
          `the listener's relay socket refused the subject ${subscription.subject} (${code})`
        )
      );
      this.#logOnce(
        `${session}\u0000${code}\u0000${subscription.subject}`,
        `the listener's relay socket for ${session} refused the subject ${subscription.subject} (${code}); that subscription is closed`
      );
      return;
    }
    // Logged once per code the wire names; any other code once in all, so a listener sending code
    // after code grows the once-logged set by one key, not one each. An unknown code is the
    // listener's own text, as long as a frame, so the line holds its first 64 characters.
    const known = ERR_CODES[code] === true;
    const shown = code.length > 64 ? `${code.slice(0, 64)}…` : code;
    this.#logOnce(
      `${session}\u0000${known ? code : "another code"}`,
      known
        ? `the listener's relay socket for ${session} answered ${code}`
        : `the listener's relay socket for ${session} answered ${shown}, a code this client does not know; later unknown codes are not logged`
    );
  }

  #socketClosed(
    generation: number,
    session: string,
    socket: Bun.WebSocket,
    { end, why, refusal, failure, code }: SocketEnd
  ): void {
    if (this.#socket === socket) this.#socket = undefined;
    if (generation !== this.#generation || this.#closedForGood) return;
    this.#stopTimers();
    const dropped = new LoggableError(`the listener's relay socket closed (${why})`);
    for (const pong of this.#pongs.splice(0)) pong.reject(dropped);
    if ("closed" in end) this.#emit("disconnect");
    // Only where the token was the cause does the next open trade for a fresh one; every other
    // presents the token in hand again, with a fresh proof. A fresher token the credential already
    // holds is kept (`RequestCredential.stale`).
    if (tokenCaused(end)) this.#options.credential.stale?.(session, this.#presented);
    const delay = this.#reopenAfter(end);
    if ("closed" in end) {
      this.#closedWith = end.closed;
      const closed =
        end.closed === CLOSE_BAD_FRAME
          ? `${why}: one of the frames sent since its last pong, ${[...this.#sentSincePong.keys()].join(", ") || "none"}`
          : why;
      this.#options.log(
        `the listener's relay socket for ${session} closed (${closed}); it opens again in ${delay} ms`
      );
      return;
    }
    const said = failure.replace(/^WebSocket connection to '[^']*' failed: /, "");
    this.#failed(
      session,
      end.refused !== undefined
        ? `the listener refused the upgrade with HTTP ${end.refused}`
        : (refusal ??
            `the socket closed before it opened (${why}${said === "" ? "" : `: ${said}`})`),
      delay,
      code
    );
  }

  /** Opens again once `end`'s wait is over, counting `end` in its run (`relayRun`); answers the
   *  wait. */
  #reopenAfter(end: RelayEnd): number {
    // Every failed open and every close of an open socket waits here: none is under way now.
    this.#openUnderWay = false;
    const run = relayRun(end);
    const streak = this.#streaks.get(run) ?? 0;
    this.#streaks.set(run, streak + 1);
    const delay = relayReopenDelay(end, streak);
    clearTimeout(this.#reopenTimer);
    this.#reopenTimer = setTimeout(() => this.#open(), delay);
    return delay;
  }
}

/**
 * Opens a session's relay socket (`ListenerRelay`) and answers it once its first socket is open.
 * The connection keeps opening on its own after that, as the file's header says. `signal` bounds
 * the wait for the first: when it ends first, the connection is closed and this throws
 * `RelayNotOpenedError`, naming why the last open failed with the code its socket's failure
 * carried.
 */
export async function connectListenerRelay(
  options: ListenerRelayOptions,
  signal?: AbortSignal
): Promise<ListenerRelay> {
  const relay = new ListenerRelay(options);
  try {
    await relay.opened(signal);
  } catch (error) {
    await relay.close();
    throw error;
  }
  return relay;
}
