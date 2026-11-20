import type { ConnectionOptions } from "nats";
// Not part of nats.js's public surface; nats 2.29.x has it where its server list calls it. A
// move would fail the typecheck here, and the rows that dial real nats.js (nats-connection-log
// test) would fail on a change of what it returns.
import { hostPort } from "nats/lib/nats-base-client/servers";
import { natsFailureReason } from "./errors";
import { natsAuthOptions } from "./nats-auth";
import type { NatsCredential } from "./nats-credential";

/**
 * How often a session pings its NATS server, and how many pings may go unanswered. nats.js 2.29.3
 * sends one ping per tick and drops the connection on the tick that finds `maxPingOut` pings still
 * unanswered, so a path that freezes (packets stop, neither side closes) is left within
 * 3 x 10 s = 30 s and redialed at once. A redial that also hangs costs nats.js's 20 s dial timeout
 * and the 2 s reconnect wait: 30 + 20 + 2 = 52 s, under a minute. nats.js's default 120 s interval
 * left a frozen path for up to 6 minutes.
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
