import { setTimeout as delay } from "node:timers/promises";
import { type ConnectionOptions, ErrorCode, type NatsConnection } from "nats";
// Not part of nats.js's public surface; nats 2.29.x has them where its connection and server list
// are. A move would fail the typecheck here, and the rows that dial real nats.js
// (nats-connection-log and nats-handshake tests) would fail on a change of what they return.
import { NatsConnectionImpl } from "nats/lib/nats-base-client/nats";
import { hostPort } from "nats/lib/nats-base-client/servers";
import { LoggableError, natsFailureReason } from "./errors";
import { natsAuthOptions } from "./nats-auth";
import type { NatsCredential } from "./nats-credential";

/**
 * How often a session pings its NATS server, and how many pings may go unanswered. nats.js 2.29.3
 * sends one ping per tick and drops the connection on the tick that finds `maxPingOut` pings still
 * unanswered, so a path that freezes (packets stop, neither side closes) is left within
 * 3 x 10 s = 30 s and redialed at once. A redial that also hangs costs the dial timeout below and
 * the 2 s reconnect wait: 30 + 10 + 2 = 42 s, under a minute. nats.js's default 120 s interval
 * left a frozen path for up to 6 minutes.
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
 * a seed's nkey user, the session's broker token, or none), reconnect attempts without limit 2 s
 * apart, the ping check above, and, for the broker token alone, never giving up over a refused
 * credential. A seed or no credential that the server refuses twice in a row still closes the
 * connection for good, as below; a broker token presented to a server that sends no nonce fails
 * the CONNECT (`noAuthService`, `./nats-credential`).
 *
 * That last one matters on a hub whose connections an auth service admits
 * (packages/envoy/internal/cipher/natscallout): while that service is away, which a restart of the
 * hub itself causes, or while the session's token lapses with the broker away, the server refuses
 * the session. nats.js 2.29.3 closes a connection for good on a repeated authorization error unless
 * `ignoreAuthErrorAbort` is set (`protocol.js:657-661`; the default is false, `options.js:50`), and
 * it clears `lastError` after a successful reconnect only for an expired authentication
 * (`protocol.js:397-399`), so a session refused once in any earlier outage would abort on its first
 * refusal in the next one. A seed or no credential keeps nats.js's default, as before the auth
 * service (R12, `__tests__/nats-connection-r12.test.ts`): no auth service decides those. */
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
    reconnectTimeWait: 2_000,
    pingInterval: NATS_PING_INTERVAL_MS,
    maxPingOut: NATS_MAX_PINGS_OUT,
    ...(input.credential.kind === "broker" ? { ignoreAuthErrorAbort: true } : {}),
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
 * connection that was up redials through nats.js's own reconnect wait instead.
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
  /** A dial failed and another starts `delayMs` from now; `attempt` counts from 1. */
  readonly onRetry?: (error: unknown, delayMs: number, attempt: number) => void;
}

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
 * `ignoreAuthErrorAbort`), whose authenticator mints a new token once the held one expires: while
 * the held token is live it is presented again on every redial, the same as any other credential,
 * which never changes between dials, so only its expiry can cure the refusal. Never cured:
 * an error with no code (a hub that runs no auth service, `noAuthService`; an INFO that does not
 * parse), a runtime's programming error (`ERR_INVALID_URL`), and nats.js's errors for its own
 * options.
 */
function redialCures(error: unknown, options: ConnectionOptions): boolean {
  if (error instanceof NatsDialBoundError) return true;
  if (!(error instanceof Error) || !("code" in error) || typeof error.code !== "string")
    return false;
  const { code } = error;
  if (AUTHORIZATION_REFUSALS[code] === true) return options.ignoreAuthErrorAbort === true;
  return NEVER_CURED[code] !== true && !code.startsWith("ERR_");
}

/**
 * Opens a NATS connection with `connect` and `options`, dialing again after a failed dial as
 * `plan` says while a redial can cure the failure (`redialCures`). It answers the first connection
 * that opens, and otherwise fails with the last dial's own error, unchanged.
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
      return nc;
    } catch (error) {
      failed = error;
      const waitEnds = elapsed() + delayMs;
      if (
        !redialCures(error, options) ||
        waitEnds + NATS_MIN_REDIAL_BOUND_MS > plan.budgetMs ||
        hooks.stopped?.() === true
      ) {
        throw error;
      }
      hooks.onRetry?.(error, delayMs, attempt);
      await delay(delayMs);
      // A wait can run long (a blocked event loop): a later dial starts only with time to answer.
      if (plan.budgetMs - elapsed() < NATS_MIN_REDIAL_BOUND_MS) throw error;
      delayMs = Math.min(delayMs * 2, plan.maxDelayMs);
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
