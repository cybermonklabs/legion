// packages/cipher-client/src/device-login.ts
//
// A machine's device login: the identity of a machine that has no platform identity of its own,
// such as a person's laptop (`AGENT_SECRETS_MACHINE_IDENTITY=device-login`). It is the login the
// Go helper makes (`internal/cipher/helper`, `Broker.Login`), made here so a machine with no Go
// binary and no systemd can make one, and traded as no Go client trades one.
//
// The login, once per machine (`dispatch login --device`):
//
// 1. A fresh P-256 key signs a credential-request object (`record.Sign`): one
//    `launcher_credential` detail naming this host, and `login_hint` naming the person who
//    approves it, which the broker's `machine.Service.verifyPersonLogin` verifies. The key's
//    thumbprint goes beside it as `dpop_jkt`, so every token of the login is bound to this key.
// 2. The broker answers an RFC 8628 user code and the page to type it into; the person types it
//    there, signed in to Dispatch.
// 3. The machine polls the device code, proving the key, until the person decides. Approved, it
//    keeps the login in its file (`device-login-file.ts`, which holds the file's place, its read
//    and its save): the key, the launcher credential's id and expiry, the refresh token, the
//    broker, and the access token the poll answered with its expiry and its life.
//
// A trade, every time a session needs a broker token: the login's access token is the subject of
// the RFC 8693 exchange, under RFC 8693's own `access_token` type, with no actor, and its proof is
// signed with the login's key, since the broker binds a trade of a machine login's token to that
// token's key; a read trade binds its token to a holder key of its own instead (`environment.ts`).
// An access token about to lapse is first refreshed, which rotates the refresh token and moves
// the credential's expiry.
//
// Why a lock, and why it covers the trade too: a refresh revokes every access token the login
// answered before (`oauthstore.Store.RotateRefreshToken`), and a refresh token presented again
// after its five-minute grace revokes the whole login. So two processes of one machine must never
// spend one refresh token, and none may refresh while another is still trading the access token
// that refresh would end. Each trade therefore runs under the login's lock
// (`withDeviceLoginLock`, whose protocol `device-login-lock.ts` holds). Under it a process reads
// the file, refreshes where the access token kept there has under a minute left (a read trade
// asks for more, `DeviceLoginIdentity.spend`) or the broker refused it (a revoke ends a login
// before its token lapses), keeps what the refresh answered, and trades; a process whose lock is
// no longer its own refreshes and keeps nothing. The access token is kept in the file for the
// same reason: every `dispatch` command is a process of its own, and a refresh for each would
// rotate the login's refresh token at every command.

import { generateKeyPairSync, type KeyObject, randomUUID } from "node:crypto";
import { holdSecret, holdUrlUserinfo } from "@legion/contracts/held-secrets";
import { isHttpUrl, urlForText } from "@legion/contracts/log-text";
import { withoutTrailingSlashes } from "@legion/contracts/trailing-slash";
import { CompactSign, calculateJwkThumbprint } from "jose";
import {
  allowInsecureRequests,
  Configuration,
  customFetch,
  type DeviceAuthorizationResponse,
  genericGrantRequest,
  initiateDeviceAuthorization,
  None,
  type TokenEndpointResponse,
} from "openid-client";
import {
  BROKER_URL_SETTING,
  brokerRefusal,
  type DpopNonce,
  requestInit,
  responseOf,
  sendWithProof,
  statusOf,
} from "./broker-request";
import {
  DEVICE_LOGIN_FILE_NAMED,
  type DeviceLogin,
  deviceLoginDirectory,
  KeptCopyError,
  lockLost,
  readDeviceLogin,
  saveDeviceLogin,
} from "./device-login-file";
import {
  DEVICE_LOGIN_STALE_LOCK_MS,
  type DeviceLoginLockOptions,
  withLoginLock,
} from "./device-login-lock";
import {
  codeLeft,
  DEFAULT_POLL_SECONDS,
  LEAST_POLL_SECONDS,
  MOST_LIFETIME_SECONDS,
  pastTheMillisecond,
  seconds,
  unpollableAnswer,
  waitMillisecondsOf,
  wholeMillisecondsOf,
} from "./device-login-reach";
import { type DpopKey, dpopKeyOf } from "./dpop";
import { messageOf } from "./error-text";
import { endsGrant, outage, refusalCode } from "./oauth-refusal";
import type { SubjectToken, TokenSource } from "./source";
import { sleep, TRADE_ATTEMPT_MS } from "./token-client";
import {
  BrokerRefusedError,
  BrokerTradeFailedError,
  DEVICE_LOGIN_COMMAND,
  DeviceLoginEndedError,
} from "./token-errors";
import {
  DEVICE_AUTHORIZATION_PATH,
  GRANT_TYPE_DEVICE_CODE,
  GRANT_TYPE_REFRESH,
  LAUNCHER_CREDENTIAL_DETAIL,
  MACHINE_LOGIN_CLIENT_ID,
  MACHINE_LOGIN_SCOPE,
  REQUEST_OBJECT_LIFETIME_SECONDS,
  REQUEST_OBJECT_TYPE,
  SUBJECT_TYPE_ACCESS,
  TOKEN_PATH,
  TOKEN_TYPE_DPOP,
} from "./wire";

/** The variable that names the broker, as `environment.ts` holds it (`BROKER_URL_VAR`); that
 *  module builds on this one, so naming it here keeps the import one way. */
const BROKER_SETTING = "AGENT_SECRETS_URL";

/** The least life an access token kept in the file must have left to be traded without a
 *  refresh: two trade attempts' worth, so it cannot lapse at the broker while the trade that
 *  presents it is still under way. */
const ACCESS_MARGIN_MS = 2 * TRADE_ATTEMPT_MS;

/**
 * The least life the access token kept in the file must have left for a trade to spend it as it
 * is ({@link DeviceLoginIdentity.spend} refreshes one with less, strictly less): the trade's
 * `leastLifeMs`, capped at half of `lifeSeconds`, the life the last refresh gave the access
 * token, and never under {@link ACCESS_MARGIN_MS}. A file that keeps no life (an older build
 * wrote it) and a trade that asks for none take that margin alone. The cap and the margin are
 * `spend`'s, which says why.
 */
export function leastAccessLifeMs(
  leastLifeMs: number | undefined,
  lifeSeconds: number | null
): number {
  if (leastLifeMs === undefined || lifeSeconds === null) return ACCESS_MARGIN_MS;
  return Math.max(ACCESS_MARGIN_MS, Math.min(leastLifeMs, (lifeSeconds * 1000) / 2));
}

/** The most one request of a login (the device authorization, a poll, a refresh) waits for the
 *  broker's answer before it is one the broker did not answer: half a trade's attempt, so a
 *  refresh that got no answer can be sent once more within one. */
const BROKER_REQUEST_MS = TRADE_ATTEMPT_MS / 2;

/** A host an http broker may be on: this machine's loopback, as a dev stack's broker is. */
const LOOPBACK_HOST = /^(localhost|127(\.\d{1,3}){3}|\[::1\])$/i;

/** A control or format character: a terminal may run the one as an escape sequence rather than
 *  print it, and the other (a right-to-left override) changes what it shows. */
const UNPRINTABLE = /[\p{Cc}\p{Cf}]/u;

/** The session name a device login trades under for a caller that names none:
 *  `device-login-` and the first 12 characters of the login key's thumbprint, the same on every
 *  trade of that login and different for each machine's. */
export function deviceLoginSessionName(thumbprint: string): string {
  return `device-login-${thumbprint.slice(0, 12)}`;
}

/**
 * `name` in the form the broker takes a launcher credential's host in (`record.hostnamePattern`):
 * lower-cased, each run of other characters a `-`, each label trimmed of its outer `-` and kept to
 * 63 characters, and "device" where nothing is left. A laptop's own name is often none of that
 * form (`Adas-MacBook-Pro.local`).
 */
export function deviceLoginHost(name: string): string {
  const labels = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9.-]+/g, "-")
    .split(".")
    .map((label) => label.replace(/^-+/, "").slice(0, 63).replace(/-+$/, ""))
    .filter((label) => label !== "");
  const host = labels
    .join(".")
    .slice(0, 253)
    .replace(/[.-]+$/, "");
  return host === "" ? "device" : host;
}

/**
 * `brokerUrl` as every request and record of a login names it: trimmed, less its trailing
 * slashes, its user info held. Refused, unquoted, where it is no http or https URL, or an http URL
 * of a host other than this machine's loopback: the login's refresh token and access tokens cross
 * this connection, so a broker off this machine is https, and a dev stack's on the loopback is
 * http.
 */
export function deviceLoginBroker(brokerUrl: string): string {
  const trimmed = withoutTrailingSlashes(brokerUrl.trim());
  if (!isHttpUrl(trimmed)) throw new Error(`${BROKER_SETTING} is not an http or https URL`);
  const { protocol, hostname } = new URL(trimmed);
  if (protocol === "http:" && !LOOPBACK_HOST.test(hostname)) {
    throw new Error(
      `${BROKER_SETTING} is an http URL of a host off this machine's loopback: a device login sends its refresh token and access tokens to the broker, so it takes https, or http to 127.0.0.1, ::1 or localhost`
    );
  }
  return holdUrlUserinfo(trimmed);
}

/** `key` as the DPoP key every request of the login proves; a key that is not P-256 is refused
 *  naming it as this machine's device login key. */
function loginDpopKey(key: KeyObject): DpopKey {
  return dpopKeyOf(key, "this machine's device login key");
}

/**
 * `value`, a field of the broker's answer at `endpoint` that this machine prints, refused where it
 * holds a control or format character ({@link UNPRINTABLE}): a terminal runs an escape sequence it
 * is sent (an OSC 8 link, an OSC 52 clipboard write) rather than print it, and a right-to-left
 * override turns round what it shows. `what` names the field.
 */
function printable(value: string, what: string, endpoint: string): string {
  if (UNPRINTABLE.test(value)) {
    throw new BrokerTradeFailedError(
      `the secrets broker at ${urlForText(endpoint, BROKER_URL_SETTING)} answered a ${what} holding a control or format character, which this machine does not print`
    );
  }
  return value;
}

/** `uri`, a page of the broker's answer at `endpoint` that a person is told to open, refused
 *  where it is no http or https URL or holds a control character ({@link printable}). */
function page(uri: string, endpoint: string): string {
  if (!isHttpUrl(uri)) {
    throw new BrokerTradeFailedError(
      `the secrets broker at ${urlForText(endpoint, BROKER_URL_SETTING)} answered a page to type the code in that is no http or https URL`
    );
  }
  return printable(uri, "page to type the code in", endpoint);
}

/**
 * Runs `work` holding the device login's lock, `<path>.lock` (`device-login-lock.ts`, which
 * holds its protocol), and answers what it answers. It first makes the login's directory and
 * refuses one another user could write to ({@link deviceLoginDirectory}), so no other user picks
 * what the lock opens. `work` is handed `held`, which reads the lock again and says whether it is
 * still this call's: false once another caller took it from a holder that was stopped. `signal`
 * ends the wait for a lock another process holds, as `LockHeldError`.
 */
export async function withDeviceLoginLock<T>(
  path: string,
  work: (held: () => Promise<boolean>) => Promise<T>,
  options: DeviceLoginLockOptions = {}
): Promise<T> {
  await deviceLoginDirectory(path);
  return await withLoginLock(path, DEVICE_LOGIN_FILE_NAMED, work, options);
}

/**
 * The OAuth client of the broker's machine-login routes at `brokerUrl`, sending through
 * `fetchImpl`: with a proof of `proved`'s key on every request where it is given (the device code
 * poll and the refresh), and with none where it is not (the device authorization). Each request
 * waits {@link BROKER_REQUEST_MS} at most for its answer: one the broker never answers is one it
 * did not answer.
 */
function brokerConfiguration(
  brokerUrl: string,
  fetchImpl: typeof fetch,
  proved?: { readonly key: DpopKey; readonly nonce: DpopNonce }
): Configuration {
  const config = new Configuration(
    {
      issuer: brokerUrl,
      token_endpoint: brokerUrl + TOKEN_PATH,
      device_authorization_endpoint: brokerUrl + DEVICE_AUTHORIZATION_PATH,
    },
    MACHINE_LOGIN_CLIENT_ID,
    {},
    None()
  );
  // An http broker is one on this machine's loopback, as a dev stack's is (`deviceLoginBroker`).
  if (new URL(brokerUrl).protocol === "http:") allowInsecureRequests(config);
  config[customFetch] = (url, options) => {
    const deadline = AbortSignal.timeout(BROKER_REQUEST_MS);
    return proved === undefined
      ? fetchImpl(url, requestInit(options, deadline, options.headers))
      : sendWithProof(
          { key: proved.key, fetch: fetchImpl, nonce: proved.nonce },
          url,
          options,
          deadline
        );
  };
  return config;
}

/**
 * The login a grant's answer issues for `key` at `broker`: the refresh token, the credential's id
 * and expiry the broker's token hooks add, and the access token with when it lapses and the life
 * it was given. An answer that lacks any of them, or whose token is not DPoP-bound, is no login
 * to keep, and nor is one whose credential id holds a control character, since a login names that
 * id to a person.
 */
function loginOf(
  key: KeyObject,
  broker: string,
  answered: TokenEndpointResponse,
  now: number
): DeviceLogin {
  const endpoint = broker + TOKEN_PATH;
  const credentialId =
    typeof answered["credential_id"] === "string" ? answered["credential_id"] : "";
  const expiresAt = new Date(
    typeof answered["credential_expires_at"] === "string" ? answered["credential_expires_at"] : ""
  );
  const refreshToken = answered.refresh_token ?? "";
  const accessSeconds = Math.min(answered.expires_in ?? 0, MOST_LIFETIME_SECONDS);
  if (
    credentialId === "" ||
    Number.isNaN(expiresAt.getTime()) ||
    refreshToken === "" ||
    !(accessSeconds > 0) ||
    answered.token_type.toLowerCase() !== TOKEN_TYPE_DPOP.toLowerCase()
  ) {
    throw new BrokerTradeFailedError(
      `the secrets broker at ${urlForText(endpoint, BROKER_URL_SETTING)} answered the device login without a credential_id, an RFC 3339 credential_expires_at, a refresh_token, a positive expires_in or a DPoP-bound token`
    );
  }
  const accessExpiresAt = new Date(now + accessSeconds * 1000);
  return {
    key,
    credentialId: printable(credentialId, "credential_id", endpoint),
    refreshToken: holdSecret(refreshToken, expiresAt),
    expiresAt,
    broker,
    accessToken: holdSecret(answered.access_token, accessExpiresAt),
    accessExpiresAt,
    accessLifeSeconds: accessSeconds,
  };
}

/** A device authorization the broker answered: what the person types and where, and what the
 *  machine polls with. */
export interface DeviceAuthorization {
  /** The broker, as {@link startDeviceLogin} was told it, less its trailing slash. */
  readonly brokerUrl: string;
  /** The person the login names as its approver. */
  readonly person: string;
  /** The code the person types in. */
  readonly userCode: string;
  /** Where the person types it: the broker's machine-login page, Dispatch's. */
  readonly verificationUri: string;
  /** That page with the code in it, where the broker sends one; the broker omits it today, since
   *  a person always types the code. */
  readonly verificationUriComplete: string | undefined;
  /** When the code lapses undecided. */
  readonly expiresAt: Date;
  /** How many seconds apart the machine polls, until the broker says to slow down. */
  readonly intervalSeconds: number;
  /** What the machine polls with. */
  readonly deviceCode: string;
  /** The key the login is bound to. */
  readonly key: KeyObject;
}

/**
 * Starts a device login of this machine, `host`, at the broker `brokerUrl`
 * ({@link deviceLoginBroker}), for `person` to approve: a fresh P-256 key signs the request object
 * `machine.Service.verifyPersonLogin` verifies — `typ` `agent-secrets-request+jwt`, ES256 with the
 * public key embedded, `iss` its thumbprint, `aud` the broker, a `jti` of its own, `iat` now and
 * `exp` 600 s on, one `launcher_credential` detail naming `host`, and `login_hint` naming
 * `person` — and posts it to the broker's device authorization endpoint with the key's thumbprint
 * as `dpop_jkt`, as the machine-login client and its one scope. It answers what the person types
 * and where, each refused where it holds a control or format character, and the page where it is
 * no http or https URL; the interval it polls at is the broker's, {@link LEAST_POLL_SECONDS} at
 * least, and {@link DEFAULT_POLL_SECONDS} where the broker names none, and the code lives as long
 * as the broker says, {@link MOST_LIFETIME_SECONDS} at most. A poll waits the interval first (RFC
 * 8628 Section 3.5), so a code that lapses no later than its first poll could never be polled:
 * it is refused here, before its user code is shown, by the test {@link finishDeviceLogin}
 * applies too ({@link codeLeft}), since an approval of it would mint a launcher credential bound
 * to a key no process holds. The Go helper's `Login` refuses such a code too; where the two read
 * an answer apart, `pollLogin`'s doc lists it. The refusal names the broker's interval and
 * lifetime, or the hour this machine polls a code at most where that ends the code first, and what
 * must change at the broker ({@link unpollableAnswer}): asking again would meet the same answer. A
 * refusal of the device authorization is worded as every broker refusal is (`brokerRefusal`). A
 * refusal of an answer this machine will not use names the broker as `urlForText` does, as
 * {@link printable}'s does, and prints of the answer at most its interval and lifetime, numbers
 * oauth4webapi checked finite, in digits.
 */
export async function startDeviceLogin(options: {
  readonly brokerUrl: string;
  readonly person: string;
  readonly host: string;
  readonly fetch?: typeof fetch | undefined;
}): Promise<DeviceAuthorization> {
  const brokerUrl = deviceLoginBroker(options.brokerUrl);
  const endpoint = brokerUrl + DEVICE_AUTHORIZATION_PATH;
  const person = options.person.trim();
  const { privateKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
  // The public half as every proof of the login embeds it: `kty`, `crv`, `x` and `y` alone.
  const jwk = loginDpopKey(privateKey).publicJwk;
  const thumbprint = await calculateJwkThumbprint(jwk);
  const iat = Math.floor(Date.now() / 1000);
  const claims = {
    iss: thumbprint,
    aud: brokerUrl,
    jti: randomUUID(),
    iat,
    exp: iat + REQUEST_OBJECT_LIFETIME_SECONDS,
    authorization_details: [{ type: LAUNCHER_CREDENTIAL_DETAIL, identifier: options.host }],
    login_hint: person,
  };
  const request = await new CompactSign(new TextEncoder().encode(JSON.stringify(claims)))
    .setProtectedHeader({ alg: "ES256", typ: REQUEST_OBJECT_TYPE, jwk })
    .sign(privateKey);
  const config = brokerConfiguration(brokerUrl, options.fetch ?? fetch);
  let answered: DeviceAuthorizationResponse;
  try {
    answered = await initiateDeviceAuthorization(config, {
      scope: MACHINE_LOGIN_SCOPE,
      dpop_jkt: thumbprint,
      request,
    });
  } catch (error) {
    throw brokerRefusal("ask for a code for", "this machine's device login", endpoint, error);
  }
  const intervalSeconds = Math.max(answered.interval ?? DEFAULT_POLL_SECONDS, LEAST_POLL_SECONDS);
  const lifetimeSeconds = Math.min(answered.expires_in, MOST_LIFETIME_SECONDS);
  // The lifetime's whole milliseconds as the broker's digits name them (wholeMillisecondsOf;
  // oauth4webapi takes none below 0), on one reading of the clock, so the test sees the whole
  // milliseconds the end holds.
  const lifetimeMs = wholeMillisecondsOf(lifetimeSeconds);
  const now = Date.now();
  const expiresAt = new Date(now + lifetimeMs);
  const code = codeLeft(expiresAt, intervalSeconds, now);
  if (!code.reached) {
    throw new BrokerTradeFailedError(
      `the secrets broker at ${urlForText(endpoint, BROKER_URL_SETTING)} answered ${unpollableAnswer(answered, intervalSeconds, lifetimeSeconds, code.left)}`
    );
  }
  return {
    brokerUrl,
    person,
    userCode: printable(answered.user_code, "user code", endpoint),
    verificationUri: page(answered.verification_uri, endpoint),
    verificationUriComplete:
      answered.verification_uri_complete === undefined
        ? undefined
        : page(answered.verification_uri_complete, endpoint),
    expiresAt,
    intervalSeconds,
    deviceCode: holdSecret(answered.device_code, expiresAt),
    key: privateKey,
  };
}

/** A login {@link finishDeviceLogin} kept, with `replaced`, the launcher credential of the login
 *  its file held just before, which this one replaced, or "" where the file held none that reads
 *  as a login: that credential stays live at the broker until it lapses or is revoked. `lockLeft`
 *  says why this process could not let go of the login's lock once the login was kept, or is ""
 *  where it let go: the login is kept either way, and another process takes such a lock over once
 *  it is stale. */
export interface KeptDeviceLogin extends DeviceLogin {
  readonly replaced: string;
  readonly lockLeft: string;
}

/**
 * How long the answer behind `error` ({@link responseOf}) asks its client to wait before it asks
 * again, in milliseconds: its `Retry-After` (RFC 9110 Section 10.2.3), a number of seconds or an
 * HTTP date, the time until which is the wait. It is 0 where the answer names none, names a time
 * already past, or names neither form, and where there was no answer.
 */
function retryAfterMs(error: unknown): number {
  const value = responseOf(error)?.headers.get("retry-after")?.trim() ?? "";
  if (/^\d+$/.test(value)) return Number(value) * 1000;
  const at = Date.parse(value);
  return Number.isFinite(at) ? Math.max(0, at - Date.now()) : 0;
}

/**
 * Polls the device code `started` answered until the person decides, and keeps the login an
 * approval issues at `path` ({@link saveDeviceLogin}, under {@link withDeviceLoginLock}), answering
 * it with the credential it replaced ({@link KeptDeviceLogin}), which is read under the same lock
 * as the save, so a login another `dispatch login` kept there while this one waited for its
 * approval is the one named. Each poll waits the interval first (RFC 8628 Section 3.5) and proves
 * the login's key. The status decides first, as the Go helper's `pollLogin` decides it
 * (`oauthrefusal.Refusal`): a poll the broker did not answer, or answered with an {@link outage}
 * (a 408, its 429, a 5xx), polls again whatever OAuth error it names, since no person decided and
 * a later poll may be answered. Only in any other 4xx does a deciding OAuth error count
 * ({@link refusalCode}): `authorization_pending` and `slow_down` poll again, `access_denied` is a
 * denial (the broker answers it 403), and `expired_token` or `invalid_grant` (a code the broker no
 * longer knows) is an expiry, as is the code's own lifetime passing, which names how many polls
 * in a row before it the broker did not answer or answered with an outage. Each `slow_down` and
 * each poll the broker did not answer or answered with an outage polls five seconds further apart
 * from then on, once for a poll that is both: the library reads an OAuth error only in a 4xx, so a
 * `slow_down` is a refusal's code or comes in a 408 or a 429, an outage, and adds its 5 s either
 * way. RFC 8628 Section 3.5 grows the interval on a `slow_down` and slows the polling on a
 * connection timeout, and `pollLogin` grows it the same way. An outage whose answer names a
 * `Retry-After`, in seconds or as an HTTP date, is waited at least that long before the next
 * poll, though never past the code's lifetime, as `pollLogin` waits it. The code's end is read
 * once from its {@link DeviceAuthorization.expiresAt}, {@link MOST_LIFETIME_SECONDS} at most, and
 * kept on the monotonic clock (`performance.now()`, or `clock`), as `pollLogin` keeps it, so a step
 * of the wall clock neither ends the poll early nor makes it go on past the code. That clock, on
 * Linux, does not count a time the machine is suspended, so a machine that sleeps past the code's
 * end polls on after it resumes, and the broker answers `expired_token`, an expiry too. The device
 * code is held for the scrub (`@legion/contracts/held-secrets`) until an hour past that end, read
 * again on the wall clock at each poll. A code that lapses no later than its first poll, the
 * interval on, could never be polled: {@link startDeviceLogin} refuses such an answer before its
 * code is shown, and this poll, by the same test ({@link codeLeft}), refuses one it is handed too
 * (a `started` a caller made, or one the time since then, or a step of the wall clock, has brought
 * nearer its end), naming the code, which has lapsed or will before its first poll, rather than
 * waiting it out and saying it expired before the person approved it; asking again starts a new
 * code, which {@link startDeviceLogin} checks. A `started` whose `expiresAt` is no date, or whose
 * interval is not a number of seconds, 1 or more, is refused with a `TypeError`: it would poll the
 * broker more than once a second, or as fast as the broker answers. Any other answer of a 4xx,
 * with another OAuth error or without one (a proxy's HTML page, a bare `WWW-Authenticate`
 * challenge), stops the poll at once, naming the status, rather than polling until the code lapses
 * and saying it expired. That is the one place this poll and `pollLogin` differ by choice: the
 * helper polls on there until the code lapses, as it records the login's state in the background,
 * where a person waits on this poll. They also read apart some answers no conforming broker gives,
 * where their libraries or their own checks differ; the one list of them is `pollLogin`'s doc
 * (`packages/envoy/internal/cipher/helper/broker.go`). Each refusal of the code or of a poll's
 * answer before an approval names {@link DEVICE_LOGIN_COMMAND}, and none writes a file; a
 * `started` a caller made is also refused with no such name: by that `TypeError`, by
 * `loginDpopKey` where its key is not P-256, and by `new URL` where its `brokerUrl` is no URL. An
 * approved login that cannot be kept is refused naming its credential, which the broker holds
 * live, and where to revoke it, and a copy of it written whole is kept beside the file
 * ({@link KeptCopyError}), which the refusal names whatever fails after it, letting go of the lock
 * included. One kept whose lock is then not let go is answered as kept: the machine uses that
 * login, so it is not one to revoke.
 */
export async function finishDeviceLogin(
  started: DeviceAuthorization,
  options: {
    readonly path: string;
    readonly fetch?: typeof fetch | undefined;
    readonly sleep?: ((ms: number) => Promise<void>) | undefined;
    /** The monotonic clock, in milliseconds, the code's end is kept on; `performance.now()`
     *  unless a test replaces it. */
    readonly clock?: (() => number) | undefined;
  }
): Promise<KeptDeviceLogin> {
  if (
    !Number.isFinite(started.expiresAt.getTime()) ||
    !Number.isFinite(started.intervalSeconds) ||
    started.intervalSeconds < LEAST_POLL_SECONDS
  ) {
    throw new TypeError(
      `finishDeviceLogin was handed a device authorization whose expiresAt is no date, or whose intervalSeconds is not a number of seconds, ${LEAST_POLL_SECONDS} or more`
    );
  }
  const wait = options.sleep ?? ((ms: number) => sleep(ms, undefined, true));
  const clock = options.clock ?? (() => performance.now());
  const config = brokerConfiguration(started.brokerUrl, options.fetch ?? fetch, {
    key: loginDpopKey(started.key),
    nonce: { value: "" },
  });
  const endpoint = started.brokerUrl + TOKEN_PATH;
  // The code's lapse, naming the polls in a row just before it that the broker did not answer or
  // answered with an outage: why nobody's approval reached this machine, where there were any.
  const expired = (outages: number): Error => {
    const unreached =
      outages === 0
        ? ""
        : `; the broker was unreachable or busy for the last ${outages === 1 ? "poll" : `${outages} polls`}`;
    return new Error(
      `the code ${started.userCode} expired before ${started.person} approved this machine${unreached}; ask again with: ${DEVICE_LOGIN_COMMAND}`
    );
  };
  let interval = started.intervalSeconds;
  // The polls in a row the broker did not answer or answered with an outage.
  let outages = 0;
  // The wait before the next poll: the interval rounded up to a whole millisecond, never shorter
  // than the broker asked (waitMillisecondsOf, the count the code's test reads too), or an
  // outage's longer `Retry-After`.
  let waitMs = waitMillisecondsOf(interval);
  // What is left of the code, read once from the wall clock, and the hour at most, which a step
  // back of that clock since `startDeviceLogin`, or a `started` a caller made, could otherwise
  // pass, and with it a timer's range; its end is kept on the monotonic clock.
  const { named, left: leftAtStart, reached } = codeLeft(started.expiresAt, interval, Date.now());
  if (!reached) {
    // The wait the test decided on: an interval with digits past the millisecond waits them
    // counted up (4.9997 s waits 5 s); any other waits its own digits, printed from the interval,
    // since a huge one's milliseconds can pass what a number holds.
    const first = `its first poll, ${seconds(pastTheMillisecond(interval) ? waitMs / 1000 : interval)} s on`;
    const lapse =
      leftAtStart <= 0
        ? `had lapsed before ${first}`
        : named > leftAtStart
          ? `would lapse here before ${first}, as this machine polls a device code for an hour at most`
          : `lapses in ${seconds(leftAtStart / 1000)} s, no later than ${first}`;
    throw new Error(
      `the code ${started.userCode} ${lapse}, so no poll could reach it; ask again with: ${DEVICE_LOGIN_COMMAND}`
    );
  }
  const endsAt = clock() + leftAtStart;
  while (true) {
    const left = endsAt - clock();
    if (left <= 0) throw expired(outages);
    // No wait passes the code's lifetime, and a code that lapses in it is not polled.
    await wait(Math.min(waitMs, left));
    const lasts = endsAt - clock();
    if (lasts <= 0) throw expired(outages);
    // Held for the scrub until an hour past the poll's end, as the wall clock reads it now, so a
    // step of that clock does not let the registry forget the code while it is still polled.
    holdSecret(started.deviceCode, Date.now() + lasts);
    let answered: TokenEndpointResponse;
    try {
      answered = await genericGrantRequest(config, GRANT_TYPE_DEVICE_CODE, {
        device_code: started.deviceCode,
      });
    } catch (error) {
      // A deciding code counts only in a refusal: an answer that is an outage decides nothing.
      const code = refusalCode(error);
      if (code === "access_denied") {
        throw new Error(
          `this machine's device login was denied, not approved as ${started.person}; ask again with: ${DEVICE_LOGIN_COMMAND}`
        );
      }
      if (code === "expired_token" || code === "invalid_grant") throw expired(0);
      const status = statusOf(error);
      const unanswered = status === 0 || outage(status);
      outages = unanswered ? outages + 1 : 0;
      if (code === "slow_down" || unanswered) interval += 5;
      const pollAgain = code === "authorization_pending" || code === "slow_down" || unanswered;
      if (!pollAgain) {
        const refusal = brokerRefusal(
          "poll the code of",
          "this machine's device login",
          endpoint,
          error
        );
        throw new Error(`${refusal.message}; ask again with: ${DEVICE_LOGIN_COMMAND}`);
      }
      const asked = unanswered ? retryAfterMs(error) : 0;
      // The wait before the next poll is capped at the code's lifetime where it is waited.
      waitMs = Math.max(waitMillisecondsOf(interval), asked);
      continue;
    }
    const login = loginOf(started.key, started.brokerUrl, answered, Date.now());
    let replaced = "";
    // Whether the save returned: an error after it is one of letting go of the lock.
    let saved = false;
    try {
      await withDeviceLoginLock(options.path, async (held) => {
        replaced = await readDeviceLogin(options.path).then(
          (kept) => kept.credentialId,
          () => ""
        );
        await saveDeviceLogin(options.path, login, held, true);
        saved = true;
      });
    } catch (error) {
      if (saved) {
        const lockLeft = `this process could not let go of the lock beside ${DEVICE_LOGIN_FILE_NAMED} (${messageOf(error)}); another process takes it over once it is ${DEVICE_LOGIN_STALE_LOCK_MS / 1000} s old`;
        return { ...login, replaced, lockLeft };
      }
      const revoke = `Machine logins on ${started.verificationUri}`;
      throw new Error(
        error instanceof KeptCopyError
          ? `${started.person} approved this machine's launcher credential ${login.credentialId}, but this machine could not keep it in its place: ${messageOf(error)}, which then needs no revoke. Where the copy is not used, delete it, since it holds the login's key and refresh token, and revoke the credential in ${revoke}: it stays live until it lapses or is revoked`
          : `${started.person} approved this machine's launcher credential ${login.credentialId}, but this machine could not keep it: ${messageOf(error)}. The credential stays live until it lapses or is revoked: revoke it in ${revoke}, and log in again with: ${DEVICE_LOGIN_COMMAND}`,
        { cause: error }
      );
    }
    return { ...login, replaced, lockLeft: "" };
  }
}

/** The DPoP key and thumbprint of a login's key. */
interface LoginKey {
  readonly key: DpopKey;
  readonly thumbprint: string;
}

/** How a {@link DeviceLoginIdentity} is made. */
export interface DeviceLoginIdentityOptions {
  /** The login's file (`device-login-file.ts`'s `deviceLoginFile`). */
  readonly path: string;
  /** The broker, as `AGENT_SECRETS_URL` names it: the one the login must have been made at
   *  ({@link deviceLoginBroker}). */
  readonly brokerUrl: string;
}

/**
 * A machine's device login as the subject of the broker's exchange
 * (`AGENT_SECRETS_MACHINE_IDENTITY=device-login`): the access token the login holds, presented
 * under RFC 8693's `access_token` type with no actor, by a client that proves the login's own key
 * ({@link key}). Every trade spends it under the login's lock ({@link spend}), refreshing first
 * where the access token kept in the file has under a minute left, or less than a read trade
 * asks, up to half the life the last refresh gave it.
 */
export class DeviceLoginIdentity implements TokenSource {
  readonly #path: string;
  readonly #brokerUrl: string;
  /** The login's key as this identity first read it, made once; a read that failed is not kept,
   *  so the next one reads the file again. */
  #key: Promise<LoginKey> | undefined;

  constructor(options: DeviceLoginIdentityOptions) {
    this.#path = options.path;
    this.#brokerUrl = deviceLoginBroker(options.brokerUrl);
  }

  describe(): string {
    return `this machine's device login (${DEVICE_LOGIN_FILE_NAMED})`;
  }

  /** The login's key as this identity first read it, which every proof of a trade of it is
   *  signed with: the broker binds a trade of a machine login's token to that token's key. */
  async key(): Promise<DpopKey> {
    return (await this.#loaded()).key;
  }

  /** The RFC 7638 thumbprint of that key, which names the login at the broker. */
  async thumbprint(): Promise<string> {
    return (await this.#loaded()).thumbprint;
  }

  #loaded(): Promise<LoginKey> {
    if (this.#key === undefined) {
      const loading = readDeviceLogin(this.#path).then(async ({ key }) => {
        const dpop = loginDpopKey(key);
        return { key: dpop, thumbprint: await calculateJwkThumbprint(dpop.publicJwk) };
      });
      this.#key = loading;
      loading.catch(() => {
        if (this.#key === loading) this.#key = undefined;
      });
    }
    return this.#key;
  }

  /**
   * Runs `trade` with the login's access token, holding the login's lock until it settles, so no
   * other process of this machine refreshes the login — which would revoke that access token, or
   * spend the refresh token this one is about to — while `trade` presents it. Under the lock it
   * reads the file again, since another process may have refreshed since, and refuses a login made
   * at another broker than this identity's, or a login of another key than the one this process
   * proves (a `dispatch login --device` since it started), each naming what cures it.
   *
   * An access token with less left than {@link leastAccessLifeMs} answers is refreshed first
   * ({@link refreshDeviceLogin}), and the login it answers is kept before `trade` runs. That is a
   * minute ({@link ACCESS_MARGIN_MS}), or `leastLifeMs` where a trade asks for more, as a read
   * trade does (`BrokerTokenClient`'s `#trade` says why), capped at half the life the last
   * refresh gave the access token. The cap is what keeps an access token that lives less than the
   * ask from being refreshed at every trade, each refresh ending the read tokens other processes
   * traded from the token before: a fresh access token has about that whole life left, so it
   * meets the cap, and the next refresh comes no sooner than half that life later, wherever that
   * half is over the minute. Under two minutes of life the minute is the larger, and an access
   * token that lives under a minute is refreshed at every trade, plain or read, by the minute
   * alone, so a read trade's ask adds no refresh there. A file that keeps no life, which an older
   * build wrote, takes the minute alone until the next refresh keeps one: the ask uncapped would
   * refresh at every read trade wherever the life is under it.
   *
   * A kept access token the broker refuses as one it no longer honours
   * (`BrokerRefusedError.subjectNotHonoured`) is refreshed at once and traded once more: a revoke
   * ends a login before its token's expiry, and only the refresh tells that, as
   * `DeviceLoginEndedError`, which names {@link DEVICE_LOGIN_COMMAND}. A refresh is made only while
   * the lock is still this process's (`refreshDeviceLogin`'s `held`).
   *
   * A trade made under a lock another caller took from this one while it was stopped may be
   * refused: the new holder's refresh revoked its token. That refusal, at the first trade or at
   * the one made again after a refresh, is the retryable {@link lockLost}, which a later trade
   * cures by reading the login the new holder kept; a refusal under a lock still held after a
   * refresh just made is the broker's, and final.
   *
   * `signal` ends the wait for the lock, as `LockHeldError`, which names the lock, since nothing
   * was sent. A refresh does not run on it: it waits on deadlines of its own.
   */
  async spend<T>(
    trade: (subject: SubjectToken) => Promise<T>,
    signal?: AbortSignal,
    leastLifeMs?: number
  ): Promise<T> {
    const { thumbprint } = await this.#loaded();
    return await withDeviceLoginLock(
      this.#path,
      async (held) => {
        let login = await readDeviceLogin(this.#path);
        if (withoutTrailingSlashes(login.broker) !== this.#brokerUrl) {
          throw new Error(
            `${DEVICE_LOGIN_FILE_NAMED} was made at another secrets broker than ${BROKER_SETTING} names; log this machine in at this one with: ${DEVICE_LOGIN_COMMAND}`
          );
        }
        if ((await calculateJwkThumbprint(loginDpopKey(login.key).publicJwk)) !== thumbprint) {
          throw new Error(
            `${DEVICE_LOGIN_FILE_NAMED} holds another login than the one this process started with, since ${DEVICE_LOGIN_COMMAND} ran again; start this session again to trade the new one`
          );
        }
        const refresh = (): Promise<DeviceLogin> =>
          refreshDeviceLogin(this.#path, login, { describe: this.describe(), held });
        const expires = login.accessExpiresAt?.getTime() ?? 0;
        const least = leastAccessLifeMs(leastLifeMs, login.accessLifeSeconds);
        let refreshed = login.accessToken === "" || expires - Date.now() < least;
        if (refreshed) login = await refresh();
        // At most two trades and one refresh: a refused token is refreshed once, unless it was
        // just refreshed. Each refusal is read the same way: after a lost lock it is retryable.
        while (true) {
          try {
            return await trade({ token: login.accessToken, tokenType: SUBJECT_TYPE_ACCESS });
          } catch (error) {
            if (!(error instanceof BrokerRefusedError) || !error.subjectNotHonoured) throw error;
            if (!(await held())) throw lockLost();
            if (refreshed) throw error;
            login = await refresh();
            refreshed = true;
          }
        }
      },
      { signal }
    );
  }

  /** The login's access token, read under its lock; {@link spend} is what a trade uses, since
   *  the token this answers is one another process's refresh can end once it is answered. */
  async read(signal?: AbortSignal): Promise<SubjectToken> {
    return await this.spend(async (subject) => subject, signal);
  }
}

/**
 * Refreshes `login`, kept at `path`, proving its key: the refresh grant as the machine-login
 * client, which rotates the refresh token and moves the credential's expiry, keeping the login it
 * answers at `path` and answering it. A caller holds the login's lock, and `held` says whether it
 * still does: it is read before each send of the refresh and again just before its answer is kept
 * ({@link saveDeviceLogin}), and a lock that is no longer the caller's refreshes or keeps nothing,
 * since another process may be spending the same refresh token.
 *
 * Each request waits for its answer on a deadline of its own, apart from the trade's attempt, so
 * an answer that rotated the refresh token is never cut off for the attempt's sake. A refresh the
 * broker did not answer is sent once more at once, under the same lock, with the same refresh
 * token: the broker may have rotated it, and it redeems the spent one again within its five-minute
 * grace. So a refresh token lost to an answer that never arrived is cured only within that grace;
 * past it, its redeem is a reuse, which ends the login.
 *
 * A 400 or 401 `invalid_grant` ({@link endsGrant}, the rule of Go's `oauthrefusal.EndsGrant`) —
 * the credential revoked or run out, or the refresh token already rotated away — is
 * {@link DeviceLoginEndedError}, final. Any other refusal, an `invalid_grant` at a 403 or a 429
 * among them, is worded as every broker refusal is (`brokerRefusal`).
 */
export async function refreshDeviceLogin(
  path: string,
  login: DeviceLogin,
  options: { readonly describe: string; readonly held: () => Promise<boolean> }
): Promise<DeviceLogin> {
  const config = brokerConfiguration(login.broker, fetch, {
    key: loginDpopKey(login.key),
    nonce: { value: "" },
  });
  const endpoint = login.broker + TOKEN_PATH;
  let answered: TokenEndpointResponse | undefined;
  for (let sent = 0; answered === undefined; sent += 1) {
    if (!(await options.held())) throw lockLost();
    try {
      answered = await genericGrantRequest(config, GRANT_TYPE_REFRESH, {
        refresh_token: login.refreshToken,
      });
    } catch (error) {
      if (endsGrant(error)) throw new DeviceLoginEndedError(options.describe);
      if (sent === 0 && statusOf(error) === 0) continue;
      throw brokerRefusal("refresh", options.describe, endpoint, error);
    }
  }
  const next = loginOf(login.key, login.broker, answered, Date.now());
  await saveDeviceLogin(path, next, options.held);
  return next;
}
