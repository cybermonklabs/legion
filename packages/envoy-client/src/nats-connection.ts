import { setTimeout as delay } from "node:timers/promises";
import { type ConnectionOptions, ErrorCode, type NatsConnection } from "nats";
// Not part of nats.js's public surface; nats 2.29.x has them where its connection and server list
// are. A move would fail the typecheck here, and the rows that dial real nats.js
// (nats-connection-log and nats-handshake tests) would fail on a change of what they return.
import { NatsConnectionImpl } from "nats/lib/nats-base-client/nats";
import { hostPort } from "nats/lib/nats-base-client/servers";
import { LoggableError, natsFailureReason } from "./errors";
import { natsAuthOptions } from "./nats-auth";
import {
  type BrokerNatsCredential,
  type BrokerTokenRefusal,
  brokerTokenRefusals,
  type NatsCredential,
  natsAdmissions,
  noteNatsAdmission,
  TRADE_DEADLINE_MS,
} from "./nats-credential";

/**
 * How often a session pings its NATS server, and how many pings may go unanswered. nats.js 2.29.3
 * sends one ping per tick and drops the connection on the tick that finds `maxPingOut` pings still
 * unanswered, so a path that freezes (packets stop, neither side closes) is left within
 * 3 x 10 s = 30 s and redialed at once. A redial that also hangs costs the dial timeout below and
 * the 2 s reconnect wait: 30 + 10 + 2 = 42 s, under a minute. nats.js's default 120 s interval
 * left a frozen path for up to 6 minutes. A session that presents its broker token redials on
 * `brokerRedialDelays` instead: at once where its connection lived a minute or more past its last
 * dial, as a connection that froze mostly has, and its next is then due within
 * `BROKER_REDIAL_FIRST_MS` of that one's start, which the 10 s hang has passed; a connection lost
 * sooner keeps its place in the backoff, so its redial can come up to 20 s on: 30 + 20 = 50 s.
 */
export const NATS_PING_INTERVAL_MS = 10_000;
export const NATS_MAX_PINGS_OUT = 2;

/**
 * The longest one dial of a session plugin waits for its server, in `natsConnectionOptions`'
 * `timeout` and, around the whole of a first dial's `connect()`, in `connectNats`. A server that
 * closes the dial during its handshake fails it at once (patches/nats@2.29.3.patch), so this
 * bounds a server that says nothing. nats.js's own default is 20 s. Measured in a scratch pod on
 * the worker image: a healthy dial through the in-cluster bridge to the hub took at most 134 ms
 * (300 dials, 2026-11-17), and Oh My Pi's start blocked the event loop for up to 3.7 s (8 starts,
 * 2026-11-18). A block past the bound fails the dial at the bound once the loop runs again, and
 * `connectNats` dials again.
 */
export const NATS_DIAL_TIMEOUT_MS = 10_000;

/** The `connect` options every session plugin dials NATS with: its credential (`natsAuthOptions`:
 * a seed's nkey user, the session's broker token, or none), reconnect attempts without limit, the
 * ping check above, and, for the broker token alone, never giving up over a refused credential. A
 * seed or no credential reconnects 2 s apart (`reconnectTimeWait`), and a server that refuses it
 * twice in a row still closes the connection for good, as below; a broker token presented to a
 * server that sends no nonce fails the CONNECT (`noAuthService`, `./nats-credential`).
 *
 * That last one matters on a hub whose connections an auth service admits
 * (packages/envoy/internal/cipher/natscallout): while that service is away, which a restart of the
 * hub itself causes, or while the session's token lapses with the broker away, the server refuses
 * the session. nats.js 2.29.3 closes a connection for good on a repeated authorization error unless
 * `ignoreAuthErrorAbort` is set (`protocol.js:657-661`; the default is false, `options.js:50`), and
 * it clears `lastError` after a successful reconnect only for an expired authentication
 * (`protocol.js:397-399`), so a session refused once in any earlier outage would abort on its first
 * refusal in the next one. A seed or no credential keeps nats.js's default, as before the auth
 * service (R12, `__tests__/nats-connection-r12.test.ts`): no auth service decides those.
 *
 * A session that presents its broker token redials on `brokerRedialDelays`, a backoff, in place of
 * the 2 s (nats.js reads `reconnectTimeWait` only in its own default handler,
 * `options.js:95-105`, so a broker session sets none): every refused redial costs the auth service
 * a decision, and a hub that refuses every session at once would otherwise be asked again by each
 * of them every 2 s for as long as it refuses. */
export function natsConnectionOptions(input: {
  readonly servers: readonly string[];
  readonly name: string;
  readonly credential: NatsCredential;
}): ConnectionOptions {
  return {
    servers: [...input.servers],
    name: input.name,
    ...natsAuthOptions(input.credential),
    timeout: NATS_DIAL_TIMEOUT_MS,
    reconnect: true,
    maxReconnectAttempts: -1,
    pingInterval: NATS_PING_INTERVAL_MS,
    maxPingOut: NATS_MAX_PINGS_OUT,
    ...(input.credential.kind === "broker"
      ? {
          ignoreAuthErrorAbort: true,
          reconnectDelayHandler: brokerRedialDelays(input.credential.broker),
        }
      : { reconnectTimeWait: 2_000 }),
  };
}

/** The highest wait before a broker-token session's first planned redial, and the most any later
 *  one waits: each doubles from the first until it reaches the cap (`brokerRedialWaitBounds`). */
export const BROKER_REDIAL_FIRST_MS = 4_000;
export const BROKER_REDIAL_CAP_MS = 20_000;

/**
 * How long nats.js leaves its redial handler uncalled, at most, while it redials: a failed dial
 * that hangs on up to five addresses, `NATS_DIAL_TIMEOUT_MS` for each address the server's name
 * resolves to (`protocol.js:417`; `_doDial` dials them in turn), and a margin; between dials it
 * asks at least every `BROKER_REDIAL_POLL_MS`. A call later than this after the one before it
 * finds that the connection was up in between, so it starts a new outage. A dial that hangs on six
 * or more addresses outlasts it: that session's next redial is then at once and its waits start
 * over, which costs the auth service nothing, since a dial that hangs never reaches it.
 */
export const BROKER_REDIAL_OUTAGE_GAP_MS = 60_000;

/**
 * The longest a broker-token session's redial loop sleeps before it asks its handler again: an
 * answer other than 0 reaches at most this far past the call that gave it. nats.js reads a token
 * the connection has yet to present only through the handler, and its `reconnect()` does nothing
 * while the connection redials (`protocol.js:368-377`), so a session switch made while the loop
 * sleeps out a wait of up to 20 s would otherwise wait for its end, past the 8 s the switch gives
 * the connection to come back (`followNatsCredential`).
 */
export const BROKER_REDIAL_POLL_MS = 1_000;

/** How early a redial may go out before it is due: more than nats.js takes between the handler's
 *  answer and its own read of the clock, and too little to change any wait. */
const DUE_SLACK_MS = 50;

/** The range the wait before the `wait`-th planned redial of an outage is drawn from, uniformly:
 *  from half its highest to its highest, the highest being `BROKER_REDIAL_FIRST_MS` doubled
 *  `wait - 1` times, at most `BROKER_REDIAL_CAP_MS`. So 2-4 s, 4-8 s, 8-16 s, then 10-20 s on. */
export function brokerRedialWaitBounds(wait: number): { lowestMs: number; highestMs: number } {
  const highestMs = Math.min(
    BROKER_REDIAL_CAP_MS,
    BROKER_REDIAL_FIRST_MS * 2 ** Math.min(wait - 1, 30)
  );
  return { lowestMs: highestMs / 2, highestMs };
}

/** The wait before the `wait`-th planned redial: `random()`, uniform in [0, 1), placed across
 *  `brokerRedialWaitBounds(wait)`, to the millisecond. The open connection's redials
 *  (`brokerRedialDelays`) and a refused first dial's (`connectNats`) both draw it here. */
export function brokerRedialWaitMs(wait: number, random: () => number): number {
  const { lowestMs, highestMs } = brokerRedialWaitBounds(wait);
  return Math.round(lowestMs + random() * (highestMs - lowestMs));
}

/** What the redial handler reads of the session's broker token (`BrokerNatsCredential`): whether
 *  the connection has yet to present the token in hand, that token's generation, which tells one
 *  token from the next as the keeper's refusal count does, and the session it was traded for,
 *  which tells a session switch from a renewal. It reads the credential's admissions too
 *  (`natsAdmissions`). */
export type RedialCredential = Pick<
  BrokerNatsCredential,
  "needsReconnect" | "tokenGeneration" | "tokenSession"
>;

/**
 * The `reconnectDelayHandler` of a session that presents its broker token: it redials at once on
 * losing its connection, then waits `brokerRedialWaitBounds(1)`, `(2)` and so on between the starts
 * of its planned redials, each drawn anew, until one is admitted. A token the connection has not
 * presented yet (`credential`: the keeper's renewal, a session switch's `followNatsCredential`, or
 * the trade a refused CONNECT started) is dialed the next time nats.js asks, within
 * `BROKER_REDIAL_POLL_MS` (at once after a failed dial, and within about a second in the middle of
 * a wait), once for each such token, between the planned redials, whose schedule it does not move.
 * A token of the session the last dial presented (a renewal, a trade after a refusal) goes out so
 * once between two planned redials unless a connection is admitted in between
 * (`noteNatsAdmission`): tokens that come faster than the waits while the hub refuses, as a
 * short-lived token's renewals do, add at most one dial to each wait and leave the waits growing.
 * A token of another session (a switch) goes out so whatever the wait: each switch's follow has 8 s
 * for the connection to come back as the new session, and switches come from the host, one at a
 * time. So a second switch, or a switch after a renewal, goes out at once too, and so does a
 * renewal once the connection is back. One handler serves one connection, so
 * `natsConnectionOptions` makes a new one each time; the dials `connectNats` makes to open that
 * connection share it, and nats.js dials a server it has never dialed at once whatever the answer,
 * so the handler times its waits from the dial that opened the connection where `connectNats`
 * notes it.
 *
 * Sized to the auth service's measured refusal throughput: one replica at its 0.5 CPU decided
 * 1,200 refusals a second, every one inside its budget, began to shed at about 1,350, and decided
 * 1,100 that came within half a second, the last 0.42 s after it came. So 1,100 sessions dropped
 * together redial at once, as they did before; their next redials spread over 2-4 s, about 550 a
 * second; and once at the cap they redial 1,100 / 15 s, about 73 a second. Those are refusals per
 * redial of a session that dials one server at one address, as sessions dial the hub today; nats.js
 * dials each address a server's name resolves to in one redial, and takes the next server it knows
 * at each call of the handler, dialing it once its own last dial plus the answer has passed, so a
 * session that reaches several pays a decision at each in every round, each within about a second
 * of the round's first. A session's next redial comes within 20 s of the hub admitting again.
 *
 * nats.js asks the handler for a wait on every turn of its redial loop and dials only when the
 * server's last dial plus that wait has passed (`protocol.js:519-559`): so the handler answers 0
 * to dial now, which places that server's last dial at this call, and otherwise the time from this
 * call's dial to the next one due, or to `BROKER_REDIAL_POLL_MS` past this call where that comes
 * first, which the loop sleeps out before asking again. A server nats.js has never dialed is
 * dialed at once whatever the answer (`srv.lastConnect === 0`).
 *
 * nats.js calls the handler only while it redials, so a call more than
 * `BROKER_REDIAL_OUTAGE_GAP_MS` after the one before it starts a new outage, at once. A connection
 * lost again inside that gap of the handler's last call, which is its last dial for a connection
 * that opened then, keeps its place in the backoff: its first redial waits out what is left of the
 * wait drawn at that dial, and the waits go on from there, so a hub that flaps keeps its sessions
 * slowed. `random`, `now` and `pollMs` are for a test.
 */
export function brokerRedialDelays(
  credential: RedialCredential,
  random: () => number = Math.random,
  now: () => number = Date.now,
  pollMs: number = BROKER_REDIAL_POLL_MS
): () => number {
  let lastCall = Number.NEGATIVE_INFINITY;
  let dialedAt = 0;
  let dueAt = Number.NEGATIVE_INFINITY;
  let waits = 0;
  // The generation of the last unpresented token a dial went out for, the session of the token
  // the last dial presented, whether a token was dialed since the last planned redial or
  // admission, and the last admission seen.
  let rushed: number | undefined;
  let dialedSession: string | undefined;
  let rushedInWait = false;
  let admissions = natsAdmissions(credential).count;
  return () => {
    const at = now();
    if (at - lastCall > BROKER_REDIAL_OUTAGE_GAP_MS) {
      dueAt = Number.NEGATIVE_INFINITY;
      waits = 0;
    }
    lastCall = at;
    const admitted = natsAdmissions(credential);
    if (admitted.count !== admissions) {
      admissions = admitted.count;
      rushedInWait = false;
      // A dial this handler did not answer 0 for opened the connection (`connectNats`'): nats.js
      // times the next sleep from that dial.
      if (admitted.dialedAt !== undefined && admitted.dialedAt > dialedAt) {
        dialedAt = admitted.dialedAt;
      }
    }
    const unpresented = credential.needsReconnect() ? credential.tokenGeneration() : undefined;
    const session = credential.tokenSession();
    // nats.js reads its own clock after this answer (`protocol.js:519-536`), so a dial due in
    // between would go out on a nonzero answer, which this handler would not count; a dial due
    // within `DUE_SLACK_MS` goes out now, on this answer, instead.
    if (at < dueAt - DUE_SLACK_MS) {
      const renewal = session === dialedSession;
      if (unpresented === undefined || unpresented === rushed || (rushedInWait && renewal)) {
        return Math.min(dueAt, at + pollMs) - dialedAt;
      }
      rushed = unpresented;
      rushedInWait = true;
      dialedSession = session;
      dialedAt = at;
      return 0;
    }
    if (unpresented !== undefined) rushed = unpresented;
    dialedSession = session;
    rushedInWait = false;
    waits += 1;
    dialedAt = at;
    dueAt = at + brokerRedialWaitMs(waits, random);
    return 0;
  };
}

/**
 * The host and port nats.js dials for `server`, as nats.js itself finds them: its own `hostPort`
 * (servers.js), the function its server list calls for every entry. It takes what follows the
 * last `://`, takes the port from the first `:` followed by digits anywhere in it (a password's
 * included), and reads the rest as an http: URL, which ends the host at `\`, takes the last of a
 * comma list written in one entry, percent-decodes, writes a Unicode name in punycode and
 * lowercases. So the address a URL parse names can differ from the one dialed; a caller that
 * checks which host a server reaches (the e2e bridge's upstream) checks this one. Undefined where
 * nats.js cannot read the server either, which it refuses before it dials. Never for a log: no
 * line names any part of a server (`natsServersForLog`).
 */
export function natsDialedAddress(server: string): { host: string; port: number } | undefined {
  try {
    const { hostname, port } = hostPort(server);
    return { host: hostname.toLowerCase(), port };
  } catch {
    return undefined;
  }
}

/**
 * The NATS servers a setting names, as a log names them: by the setting alone, never by any part
 * of what it holds. A server's text can be a credential (`nats://token@host`, a seed or token
 * pasted into the wrong setting), and every rule that tried to tell a host from a secret by its
 * spelling missed one (special schemes, case, digits read as a port, `\`, commas, escapes); so
 * the reader finds the servers in the setting the line names. `setting` is that setting's name,
 * as the operator writes it (`ENVOY_NATS_URL`, `envoy.natsUrls in <file>`, `SMOKE_RIG_NATS`), and
 * `count` how many servers it named.
 */
export function natsServersForLog(setting: string, count: number): string {
  return count === 1 ? `the server ${setting} names` : `the ${count} servers ${setting} names`;
}

/**
 * The line a failed dial is logged with: `could not connect to NATS at <named>[ presenting
 * <credential>]: <reason>`, `named` being the servers as a log names them (`natsServersForLog`)
 * and the reason `natsFailureReason`'s.
 */
export function natsDialFailure(named: string, error: unknown, presenting?: string): string {
  const credential = presenting === undefined ? "" : ` presenting ${presenting}`;
  return `could not connect to NATS at ${named}${credential}: ${natsFailureReason(error)}`;
}

/** How `connectNats` dials again after a dial that failed. */
export interface NatsDialPlan {
  /** The wait before the second dial; each later wait is twice the one before. */
  readonly firstDelayMs: number;
  /** The longest wait between two dials. */
  readonly maxDelayMs: number;
  /**
   * How long after the first dial began a later dial may still run: one starts only when it can
   * start before this, and its bound is the part of it left. 0 dials once; Infinity dials until
   * the caller stops.
   */
  readonly budgetMs: number;
}

/**
 * The redials every session plugin's NATS dial makes. A hub that closes a dial, as nats-server
 * does when no CONNECT came within its 2 s authentication timeout because the plugin's event loop
 * was busy, answers the next dial in about 0.1 s, so the first wait is short. The cap keeps a hub
 * that refuses at once in an outage to about 10 dials in the budget. The budget is the most a dial
 * may take when its caller can give it more; a session pass gives it less (`natsDialBudget`).
 * There is no jitter: sessions start at different times and each waits from its own failure, and a
 * connection that was up redials through nats.js's own reconnect wait instead. The hub's refusal of
 * a broker token waits on the broker-token backoff instead (`connectNats`, `hooks.credential`).
 */
export const NATS_DIAL_PLAN: NatsDialPlan = {
  firstDelayMs: 250,
  maxDelayMs: 4_000,
  budgetMs: 25_000,
};

/**
 * How long a dial may redial for when it must leave its caller `afterDialMs` of the `leftMs` it
 * has: the rest of what `leftMs` allows, at most `plan.budgetMs`, and 0 (one dial) when nothing is
 * left. A session pass's dial is followed by steps with bounds of their own (a subscription's
 * confirmation, the registration), and a dial that used the pass's whole deadline would leave them
 * none, so the pass would be abandoned with nothing registered.
 */
export function natsDialBudget(leftMs: number, afterDialMs: number, plan: NatsDialPlan): number {
  return Math.max(0, Math.min(plan.budgetMs, leftMs - afterDialMs));
}

/** What `connectNats` reads of its caller, and what it tells it. */
export interface NatsDialHooks {
  /** The bound of the first dial, below `NATS_DIAL_TIMEOUT_MS` when the caller has less time. */
  readonly firstBoundMs?: number;
  /** True once the caller stops: no further dial starts. Read before each dial and after each
   *  wait. */
  readonly stopped?: () => boolean;
  /** A dial failed and another starts `delayMs` from now; `attempt` counts from 1. `refusal` is
   *  what the hub's refusal of the broker token did (`brokerTokenRefusals`), where it refused it. */
  readonly onRetry?: (
    error: unknown,
    delayMs: number,
    attempt: number,
    refusal?: BrokerTokenRefusal
  ) => void;
  /** The credential `options` present (`natsConnectionOptions`'). Under a broker token, it alone
   *  decides how a refusal is answered (`brokerDialRefusals`): the wait drawn from the
   *  broker-token backoff, and the drop of a token refused twice. */
  readonly credential?: NatsCredential;
  /** The draw of a refused broker token's wait, uniform in [0, 1): `Math.random`, or a test's. */
  readonly random?: () => number;
  /** How a wait between two dials is waited: `node:timers/promises`' `setTimeout`, or a test's. */
  readonly delay?: (ms: number) => Promise<unknown>;
}

/** What a host's line for a failed dial adds where that dial's refusal dropped the session's
 *  broker token (`NatsDialHooks.onRetry`'s `refusal.dropped`), in the keeper's words for its own
 *  drop: pi-envoy's and the channel server's. */
export const NATS_DIAL_TOKEN_DROPPED =
  "the hub refused the session's broker token twice, so the token was dropped and another trade started";

/**
 * The shortest bound a later dial starts with. A healthy dial through the in-cluster bridge took
 * at most 134 ms (300 dials, 2026-11-17), so a dial with less than this left would fail at its
 * bound and report that rather than the failure it followed.
 */
export const NATS_MIN_REDIAL_BOUND_MS = 500;

/** The error of a dial that ran past its bound: this module's own words, which a log prints whole,
 *  and a failure a later dial can cure. */
export class NatsDialBoundError extends LoggableError {}

/** nats.js's codes for an authorization refusal. */
const AUTHORIZATION_REFUSALS: Record<string, true> = {
  [ErrorCode.AuthorizationViolation]: true,
  [ErrorCode.AuthenticationExpired]: true,
  [ErrorCode.AccountExpired]: true,
};

/** nats.js's codes for a dial its own options or credential make fail every time. */
const NEVER_CURED: Record<string, true> = {
  [ErrorCode.BadAuthentication]: true,
  [ErrorCode.BadCreds]: true,
  [ErrorCode.InvalidOption]: true,
  [ErrorCode.NotFunction]: true,
  [ErrorCode.ServerOptionNotAvailable]: true,
  [ErrorCode.WssRequired]: true,
  [ErrorCode.Tls]: true,
};

/**
 * Whether dialing again can cure `error`: a dial past its bound, a socket's or a resolver's
 * failure (`ECONNRESET`, `EAI_AGAIN`), or nats.js's for the server's close or refusal. An
 * authorization refusal is dialed again only under a broker token (which sets
 * `ignoreAuthErrorAbort`), whose authenticator mints a new token once the held one expires, and
 * which `connectNats` drops and trades again at its second refusal: until either, the held token
 * is presented again on every redial, the same as any other credential, which never changes
 * between dials. Never cured: an error with no code (a hub that runs no auth service,
 * `noAuthService`; an INFO that does not parse), a runtime's programming error
 * (`ERR_INVALID_URL`), and nats.js's errors for its own options.
 */
function redialCures(error: unknown, options: ConnectionOptions): boolean {
  if (error instanceof NatsDialBoundError) return true;
  const code = errorCode(error);
  if (code === undefined) return false;
  if (AUTHORIZATION_REFUSALS[code] === true) return options.ignoreAuthErrorAbort === true;
  return NEVER_CURED[code] !== true && !code.startsWith("ERR_");
}

/** The code of an error nats.js or the runtime gave, where it carries one. */
function errorCode(error: unknown): string | undefined {
  return error instanceof Error && "code" in error && typeof error.code === "string"
    ? error.code
    : undefined;
}

/** How `connectNats` answers a failed dial of a session that presents its broker token. */
interface BrokerDialRefusals {
  /** For a dial that failed with `code`: undefined unless the hub refused it
   *  (`AUTHORIZATION_REFUSALS`); else the wait before the next dial, the `n`-th refusal's drawn
   *  from `brokerRedialWaitMs(n)`, and what the refusal did to the token in hand. */
  readonly refused: (
    code: string | undefined
  ) => { readonly waitMs: number; readonly token: BrokerTokenRefusal | undefined } | undefined;
  /** A dial that began at `dialedAt` (`Date.now()`) was admitted (`noteNatsAdmission`). */
  readonly admitted: (dialedAt: number) => void;
}

/**
 * `connectNats`' answers to the hub's refusals of a broker token, or undefined for any other
 * credential. Every authorization refusal waits on the broker-token backoff, the account's lapse
 * (`ACCOUNT_EXPIRED`) included, since the hub repeats it as it repeats the rest; only a refusal of
 * the token itself counts toward its drop (`brokerTokenRefusals`, the session's one count, which
 * its keeper shares), and a drop starts the next trade at once.
 */
function brokerDialRefusals(
  credential: NatsCredential | undefined,
  random: () => number
): BrokerDialRefusals | undefined {
  if (credential?.kind !== "broker") return undefined;
  const { broker } = credential;
  const count = brokerTokenRefusals(broker);
  let refusals = 0;
  return {
    refused: (code) => {
      if (code === undefined || AUTHORIZATION_REFUSALS[code] !== true) return undefined;
      refusals += 1;
      const token = count.refused(code);
      // Started, not awaited: the dial after a drop waits at least 4 s, and a trade that fails by
      // then leaves that dial's authenticator to start another.
      if (token?.dropped) void broker.renew(AbortSignal.timeout(TRADE_DEADLINE_MS)).catch(() => {});
      return { waitMs: brokerRedialWaitMs(refusals, random), token };
    },
    admitted: (dialedAt) => noteNatsAdmission(broker, dialedAt),
  };
}

/**
 * Opens a NATS connection with `connect` and `options`, dialing again after a failed dial as
 * `plan` says while a redial can cure the failure (`redialCures`). It answers the first connection
 * that opens, and otherwise fails with the last dial's own error, unchanged.
 *
 * Under a broker token (`hooks.credential`), the hub's refusal waits as the open connection's own
 * redials do rather than as `plan` says: the wait after the `n`-th refusal is drawn anew,
 * uniformly, from `brokerRedialWaitBounds(n)`, 2-4 s, 4-8 s, 8-16 s, then 10-20 s, and the plan's
 * own wait stays where it was for the next failure of another kind. A hub that refuses every
 * session refuses each one's first dial too, and the plan's 4 s cap would ask its auth service
 * every 4 s, in step, for as long as the caller dials, which for the channel server is its life.
 * A token the hub refuses twice is dropped and another trade started at once, once for the session
 * until a dial is admitted, across every call that dials with that credential
 * (`brokerTokenRefusals`), so a session refused pass after pass trades one token for the outage;
 * `onRetry` is told what each refusal did to the token.
 *
 * Each dial has one bound around its whole `connect()`: nats.js resolves the server's name outside
 * its own timer and gives each resolved address and each configured server a `timeout` of its own,
 * so its `timeout` alone bounds none of them together. A dial past its bound fails with
 * `NatsDialBoundError`, and a connection it still opens is closed. nats.js's `timeout` is the
 * bound's share for each configured server, at least `NATS_MIN_REDIAL_BOUND_MS` and at most the
 * bound, so a silent first server leaves the next one time to answer, and each socket the dial
 * still opens closes within its server's share. The connection that opens
 * gets back the whole timeout of `options` for its reconnects (`keepWholeTimeout`). nats.js itself
 * gives up on a first dial that fails (it drops the server unless `waitOnFirstConnect` is set, and
 * with that set it would dial forever, past any deadline of its caller's).
 */
export async function connectNats(
  connect: (options: ConnectionOptions) => Promise<NatsConnection>,
  options: ConnectionOptions,
  plan: NatsDialPlan,
  hooks: NatsDialHooks = {}
): Promise<NatsConnection> {
  const started = performance.now();
  const elapsed = () => performance.now() - started;
  const timeout = options.timeout ?? NATS_DIAL_TIMEOUT_MS;
  const servers = Array.isArray(options.servers) ? Math.max(1, options.servers.length) : 1;
  const brokerRefusals = brokerDialRefusals(hooks.credential, hooks.random ?? Math.random);
  const wait = hooks.delay ?? delay;
  let delayMs = plan.firstDelayMs;
  let failed: unknown;
  for (let attempt = 1; ; attempt += 1) {
    if (hooks.stopped?.() === true) {
      throw failed ?? new LoggableError("the NATS dial was stopped before it began");
    }
    const bound = Math.max(
      1,
      Math.round(
        Math.min(
          timeout,
          attempt === 1 ? (hooks.firstBoundMs ?? timeout) : plan.budgetMs - elapsed()
        )
      )
    );
    const dialedAt = Date.now();
    try {
      const nc = await boundedDial(
        connect,
        {
          ...options,
          timeout: Math.min(bound, Math.max(NATS_MIN_REDIAL_BOUND_MS, Math.floor(bound / servers))),
        },
        bound
      );
      keepWholeTimeout(nc, timeout);
      brokerRefusals?.admitted(dialedAt);
      return nc;
    } catch (error) {
      failed = error;
      const refusal = brokerRefusals?.refused(errorCode(error));
      const waitMs = refusal?.waitMs ?? delayMs;
      if (
        !redialCures(error, options) ||
        elapsed() + waitMs + NATS_MIN_REDIAL_BOUND_MS > plan.budgetMs ||
        hooks.stopped?.() === true
      ) {
        throw error;
      }
      hooks.onRetry?.(error, waitMs, attempt, refusal?.token);
      await wait(waitMs);
      // A wait can run long (a blocked event loop): a later dial starts only with time to answer.
      if (plan.budgetMs - elapsed() < NATS_MIN_REDIAL_BOUND_MS) throw error;
      if (refusal === undefined) delayMs = Math.min(delayMs * 2, plan.maxDelayMs);
    }
  }
}

/**
 * Gives a connection back the whole dial timeout for its reconnects. nats.js keeps the options a
 * connection opened with and reads their `timeout` again at every reconnect, so a connection that
 * a late dial opened would otherwise redial with that dial's short bound for its whole life. A
 * connection a test's stand-in made is no `NatsConnectionImpl` and reconnects through nothing.
 */
function keepWholeTimeout(nc: NatsConnection, timeout: number): void {
  if (!(nc instanceof NatsConnectionImpl)) return;
  const opened: ConnectionOptions & AbandonOption = nc.options;
  opened.timeout = timeout;
  delete opened.abandonSignal;
}

/**
 * The option patches/nats@2.29.3.patch reads in `ProtocolHandler._doDial`: once the signal is
 * aborted, the dial tries no further address and no further server. nats.js copies the options it
 * is given one level deep, so the connection's handler holds this signal itself.
 */
interface AbandonOption {
  abandonSignal?: AbortSignal;
}

async function boundedDial(
  connect: (options: ConnectionOptions) => Promise<NatsConnection>,
  options: ConnectionOptions,
  boundMs: number
): Promise<NatsConnection> {
  const abandon = new AbortController();
  const withAbandon: ConnectionOptions & AbandonOption = {
    ...options,
    abandonSignal: abandon.signal,
  };
  const dialing = connect(withAbandon);
  const expired = Promise.withResolvers<never>();
  const timer = setTimeout(() => {
    abandon.abort();
    expired.reject(new NatsDialBoundError(`the NATS dial did not complete within ${boundMs} ms`));
  }, boundMs);
  try {
    return await Promise.race([dialing, expired.promise]);
  } catch (error) {
    dialing.then((late) => late.close()).catch(() => undefined);
    throw error;
  } finally {
    clearTimeout(timer);
  }
}
