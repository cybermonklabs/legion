import type { ConnectionOptions } from "nats";
// Not part of nats.js's public surface; nats 2.29.x has it where its server list calls it. A
// move would fail the typecheck here, and the rows that dial real nats.js (nats-connection-log
// test) would fail on a change of what it returns.
import { hostPort } from "nats/lib/nats-base-client/servers";
import { natsFailureReason } from "./errors";
import { natsAuthOptions } from "./nats-auth";
import type { BrokerNatsCredential, NatsCredential } from "./nats-credential";

/**
 * How often a session pings its NATS server, and how many pings may go unanswered. nats.js 2.29.3
 * sends one ping per tick and drops the connection on the tick that finds `maxPingOut` pings still
 * unanswered, so a path that freezes (packets stop, neither side closes) is left within
 * 3 x 10 s = 30 s and redialed at once. A redial that also hangs costs nats.js's 20 s dial timeout
 * and the 2 s reconnect wait: 30 + 20 + 2 = 52 s, under a minute. nats.js's default 120 s interval
 * left a frozen path for up to 6 minutes. A session that presents its broker token redials on
 * `brokerRedialDelays` instead, and comes back sooner still: its first redial is at once and its
 * next is due within `BROKER_REDIAL_FIRST_MS` of that one's start, which the 20 s hang has passed.
 */
export const NATS_PING_INTERVAL_MS = 10_000;
export const NATS_MAX_PINGS_OUT = 2;

/** The `connect` options every session plugin dials NATS with: its credential (`natsAuthOptions`:
 * a seed's nkey user, the session's broker token, or none), reconnect attempts without limit 2 s
 * apart, the ping check above, and, for the broker token alone, never giving up over a refused
 * credential. A seed or no credential that the server refuses twice in a row still closes the
 * connection for good, as below; a broker token presented to a server that sends no nonce fails
 * the CONNECT (`noAuthService`, `./nats-credential`).
 *
 * That last one matters on a hub whose connections an auth service admits
 * (packages/envoy/internal/broker/natscallout): while that service is away, which a restart of the
 * hub itself causes, or while the session's token lapses with the broker away, the server refuses
 * the session. nats.js 2.29.3 closes a connection for good on a repeated authorization error unless
 * `ignoreAuthErrorAbort` is set (`protocol.js:657-661`; the default is false, `options.js:50`), and
 * it clears `lastError` after a successful reconnect only for an expired authentication
 * (`protocol.js:397-399`), so a session refused once in any earlier outage would abort on its first
 * refusal in the next one. A seed or no credential keeps nats.js's default, as before the auth
 * service (R12, `__tests__/nats-connection-r12.test.ts`): no auth service decides those.
 *
 * A session that presents its broker token also redials on `brokerRedialDelays`, a backoff, where
 * one with a seed or none keeps the flat 2 s (R12): every refused redial costs the auth service a
 * decision, and a hub that refuses every session at once would otherwise be asked again by each of
 * them every 2 s for as long as it refuses. `reconnectTimeWait` is then unused (nats.js reads it
 * only in its own default handler, `options.js:95-105`). */
export function natsConnectionOptions(input: {
  readonly servers: readonly string[];
  readonly name: string;
  readonly credential: NatsCredential;
}): ConnectionOptions {
  return {
    servers: [...input.servers],
    name: input.name,
    ...natsAuthOptions(input.credential),
    reconnect: true,
    maxReconnectAttempts: -1,
    reconnectTimeWait: 2_000,
    pingInterval: NATS_PING_INTERVAL_MS,
    maxPingOut: NATS_MAX_PINGS_OUT,
    ...(input.credential.kind === "broker"
      ? {
          ignoreAuthErrorAbort: true,
          reconnectDelayHandler: brokerRedialDelays(input.credential.broker),
        }
      : {}),
  };
}

/** The highest wait before a broker-token session's first planned redial, and the most any later
 *  one waits: each doubles from the first until it reaches the cap (`brokerRedialWaitBounds`). */
export const BROKER_REDIAL_FIRST_MS = 4_000;
export const BROKER_REDIAL_CAP_MS = 20_000;

/**
 * How long nats.js leaves its redial handler uncalled, at most, while it redials: the longest wait
 * (the cap) or a failed dial that hangs on two addresses, nats.js's 20 s dial timeout for each
 * address the server's name resolves to (`protocol.js:417`; `_doDial` dials them in turn), and a
 * margin. A call later than this after the one before it finds that the connection was up in
 * between, so it starts a new outage. A dial that hangs on three or more addresses outlasts it:
 * that session's next redial is then at once and its waits start over, which costs the auth service
 * nothing, since a dial that hangs never reaches it.
 */
export const BROKER_REDIAL_OUTAGE_GAP_MS = 60_000;

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

/** What the redial handler reads of the session's broker token (`BrokerNatsCredential`): whether
 *  the connection has yet to present the token in hand, and that token's expiry, which tells one
 *  token from the next. */
export type RedialCredential = Pick<BrokerNatsCredential, "needsReconnect" | "expiresAt">;

/**
 * The `reconnectDelayHandler` of a session that presents its broker token: it redials at once on
 * losing its connection, then waits `brokerRedialWaitBounds(1)`, `(2)` and so on between the starts
 * of its redials, each drawn anew, until one is admitted. A redial to present a token the
 * connection has not presented yet (`credential`: the keeper's renewal, a session switch's
 * `followNatsCredential`, or the trade a refused CONNECT started) is also at once, and starts the
 * waits over, once for each such token. One handler serves one connection, so
 * `natsConnectionOptions` makes a new one each time.
 *
 * Sized to the auth service's measured refusal throughput: one replica at its 0.5 CPU decided
 * 1,200 refusals a second, every one inside its budget, began to shed at about 1,350, and decided
 * 1,100 that came within half a second, the last 0.42 s after it came. So 1,100 sessions dropped
 * together redial at once, as they did before; their next redials spread over 2-4 s, about 550 a
 * second; and once at the cap they redial 1,100 / 15 s, about 73 a second. Those are refusals per
 * redial of a session that dials one server at one address, as sessions dial the hub today; nats.js
 * dials each address a server's name resolves to in one redial, and keeps the wait for each server
 * it knows by that server's own last dial, so a session that reaches several pays a decision at
 * each. A session's next redial comes within 20 s of the hub admitting again.
 *
 * nats.js asks the handler for a wait on every turn of its redial loop and dials only when the
 * server's last dial plus that wait has passed (`protocol.js:519-559`): so the handler answers 0
 * to dial now, which places that server's last dial at this call, and otherwise the time from this
 * call's dial to the next one due, which the loop sleeps out before asking again. A server nats.js
 * has never dialed is dialed at once whatever the answer (`srv.lastConnect === 0`).
 *
 * nats.js calls the handler only while it redials, so a call more than
 * `BROKER_REDIAL_OUTAGE_GAP_MS` after the one before it starts a new outage, at once. A connection
 * that is admitted and lost again inside that gap keeps its place in the backoff, so a hub that
 * flaps keeps its sessions slowed. `random` and `now` are for a test.
 */
export function brokerRedialDelays(
  credential: RedialCredential,
  random: () => number = Math.random,
  now: () => number = Date.now
): () => number {
  let lastCall = Number.NEGATIVE_INFINITY;
  let dialedAt = 0;
  let dueAt = Number.NEGATIVE_INFINITY;
  let waits = 0;
  // The expiry of the last unpresented token a redial was made at once for.
  let rushed: number | undefined;
  return () => {
    const at = now();
    const unpresented = credential.needsReconnect() ? credential.expiresAt().getTime() : undefined;
    const newToken = unpresented !== undefined && unpresented !== rushed;
    if (newToken) rushed = unpresented;
    if (newToken || at - lastCall > BROKER_REDIAL_OUTAGE_GAP_MS) {
      dueAt = Number.NEGATIVE_INFINITY;
      waits = 0;
    }
    lastCall = at;
    // nats.js reads its own clock after this answer (`protocol.js:519-536`), so a dial due in
    // between would go out on a nonzero answer, which this handler would not count; a dial due
    // within `DUE_SLACK_MS` goes out now, on this answer, instead.
    if (at < dueAt - DUE_SLACK_MS) return dueAt - dialedAt;
    waits += 1;
    const { lowestMs, highestMs } = brokerRedialWaitBounds(waits);
    dialedAt = at;
    dueAt = at + lowestMs + random() * (highestMs - lowestMs);
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
