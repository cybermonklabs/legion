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
//    keeps the login in its file: the key, the launcher credential's id and expiry, the refresh
//    token, the broker, and the access token the poll answered with its expiry.
//
// A trade, every time a session needs a broker token: the login's access token is the subject of
// the RFC 8693 exchange, under RFC 8693's own `access_token` type, with no actor, and every proof
// is signed with the login's key, since the broker binds a trade of a machine login's token to
// that token's key. An access token about to lapse is first refreshed, which rotates the refresh
// token and moves the credential's expiry.
//
// Why a lock, and why it covers the trade too: a refresh revokes every access token the login
// answered before (`oauthstore.Store.RotateRefreshToken`), and a refresh token presented again
// after its five-minute grace revokes the whole login. So two processes of one machine must never
// spend one refresh token, and none may refresh while another is still trading the access token
// that refresh would end. Each trade therefore runs under the login's lock
// (`withDeviceLoginLock`, whose protocol `device-login-lock.ts` holds). Under it a process reads
// the file, refreshes where the access token kept there has under a minute left or the broker
// refused it (a revoke ends a login before its token lapses), keeps what the refresh answered,
// and trades; a process whose lock is no longer its own refreshes and keeps nothing. The access
// token is kept in the file for the same reason: every `dispatch` command is a process of its
// own, and a refresh for each would rotate the login's refresh token at every command.

import { createPrivateKey, generateKeyPairSync, type KeyObject, randomUUID } from "node:crypto";
import { constants, type Stats } from "node:fs";
import { access, lstat, mkdir, open, readFile, rename, rm, stat } from "node:fs/promises";
import { dirname, join } from "node:path";
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
  ResponseBodyError,
  type TokenEndpointResponse,
} from "openid-client";
import {
  DEVICE_LOGIN_STALE_LOCK_MS,
  type DeviceLoginLockOptions,
  removeLeftLocks,
  removeUntouched,
  withLoginLock,
} from "./device-login-lock";
import { type DpopKey, dpopKeyOf } from "./dpop";
import { messageOf, readErrorCode } from "./error-text";
import type { SubjectToken, TokenSource } from "./source";
import {
  BROKER_URL_SETTING,
  brokerRefusal,
  type DpopNonce,
  requestInit,
  sendWithProof,
  sleep,
  statusOf,
  TRADE_ATTEMPT_MS,
} from "./token-client";
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

/** Names the file a machine keeps its device login in. Unset, it is
 *  `~/.config/legion/device-login.json` ({@link deviceLoginFile}). */
export const DEVICE_LOGIN_FILE_VAR = "AGENT_SECRETS_DEVICE_LOGIN_FILE";

/** How every message names the login's file: by its setting, never by its path. The file holds
 *  the login's key and refresh token, and the setting is where a person who pasted a secret in
 *  place of a path put it, as the Go helper's `MachineFileNamed` says of its own file. */
export const DEVICE_LOGIN_FILE_NAMED = `the device login ${DEVICE_LOGIN_FILE_VAR} names (default ~/.config/legion/device-login.json)`;

/** The variable that names the broker, as `environment.ts` holds it (`BROKER_URL_VAR`); that
 *  module builds on this one, so naming it here keeps the import one way. */
const BROKER_SETTING = "AGENT_SECRETS_URL";

/** The least life an access token kept in the file must have left to be traded without a
 *  refresh: two trade attempts' worth, so it cannot lapse at the broker while the trade that
 *  presents it is still under way. */
const ACCESS_MARGIN_MS = 2 * TRADE_ATTEMPT_MS;

/** The most one request of a login (the device authorization, a poll, a refresh) waits for the
 *  broker's answer before it is one the broker did not answer: half a trade's attempt, so a
 *  refresh that got no answer can be sent once more within one. */
const BROKER_REQUEST_MS = TRADE_ATTEMPT_MS / 2;

/** How many seconds apart a code is polled where the broker names no interval (RFC 8628 Section
 *  3.2), and the least and the most it is polled apart whatever interval the broker names: a wait
 *  past a timer's range would fire at once, and no broker means a code to wait minutes between
 *  polls. */
const DEFAULT_POLL_SECONDS = 5;
const LEAST_POLL_SECONDS = 1;
const MOST_POLL_SECONDS = 60;

/** The most a code or an access token the broker answers is taken to live, whatever lifetime it
 *  names: an hour, four times the broker's own access tokens' 900 s. A longer one would keep a
 *  poll, or a token the broker no longer honours, past any use. */
const MOST_LIFETIME_SECONDS = 3600;

/** A host an http broker may be on: this machine's loopback, as a dev stack's broker is. */
const LOOPBACK_HOST = /^(localhost|127(\.\d{1,3}){3}|\[::1\])$/i;

/** A control or format character: a terminal may run the one as an escape sequence rather than
 *  print it, and the other (a right-to-left override) changes what it shows. */
const UNPRINTABLE = /[\p{Cc}\p{Cf}]/u;

/** Whether `name` is a temporary copy of a login a save left when it died (`saveDeviceLogin`). */
function isTemporaryCopy(name: string): boolean {
  return /^\.device-login-[0-9a-f-]{36}\.json\.tmp$/.test(name);
}

/** The file a machine keeps its device login in: {@link DEVICE_LOGIN_FILE_VAR}, else
 *  `$HOME/.config/legion/device-login.json`. */
export function deviceLoginFile(env: Readonly<Record<string, string | undefined>>): string {
  const named = (env[DEVICE_LOGIN_FILE_VAR] ?? "").trim();
  if (named !== "") return named;
  const home = (env["HOME"] ?? "").trim();
  if (home === "") {
    throw new Error(
      `${DEVICE_LOGIN_FILE_VAR} names no file and HOME names no home directory, so this machine has nowhere to keep its device login`
    );
  }
  return join(home, ".config", "legion", "device-login.json");
}

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

/** A machine's device login as its file keeps it. */
export interface DeviceLogin {
  /** The key the login is bound to, which signs every request of it. */
  readonly key: KeyObject;
  /** The launcher credential the approval minted. */
  readonly credentialId: string;
  /** The token the next refresh spends. */
  readonly refreshToken: string;
  /** When the credential lapses unless a refresh moves it. */
  readonly expiresAt: Date;
  /** The broker the login was made at, as `AGENT_SECRETS_URL` named it, less a trailing slash. */
  readonly broker: string;
  /** The access token the last grant answered, "" where the file keeps none. */
  readonly accessToken: string;
  /** When that access token lapses; null where the file keeps none. */
  readonly accessExpiresAt: Date | null;
}

/** A member of the file's JSON as a string, "" where it is absent or not one. */
function member(json: Record<string, unknown>, name: string): string {
  const value = json[name];
  return typeof value === "string" ? value : "";
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
 * Refuses `what`, the login's file or its directory, whose status is `info`, where its mode has a
 * bit of `forbidden` (`opens` says what that lets others do, and the fix), or where this user does
 * not own it (`notOwned` says the fix).
 */
function checkPrivate(
  info: Stats,
  what: string,
  forbidden: number,
  opens: string,
  notOwned: string
): void {
  const mode = info.mode & 0o777;
  if ((mode & forbidden) !== 0) {
    throw new Error(`${what} is mode ${mode.toString(8).padStart(4, "0")}, which ${opens}`);
  }
  const uid = process.getuid?.();
  if (uid !== undefined && info.uid !== uid) {
    throw new Error(`${what} is owned by uid ${info.uid}, not this user (uid ${uid}); ${notOwned}`);
  }
}

/**
 * Refuses `dir`, the login's directory, where another user could replace the login or its lock
 * in it: a directory whose mode lets its group or anyone else write to it, or that this user does
 * not own. A directory others may only read and enter is taken, as `mkdir -p` leaves the
 * `~/.config/legion` that envoy.json lives in: the login's file is 0600 itself, and its name is
 * no secret. Each refusal names the directory by the file's setting and the fix.
 */
async function checkLoginDirectory(dir: string): Promise<void> {
  let info: Stats;
  try {
    info = await stat(dir);
  } catch (error) {
    throw new Error(`read the directory of ${DEVICE_LOGIN_FILE_NAMED}: ${readErrorCode(error)}`);
  }
  checkPrivate(
    info,
    `the directory of ${DEVICE_LOGIN_FILE_NAMED}`,
    0o022,
    "lets others replace the login in it; run chmod 700 on it",
    "keep the login in a directory of this user's own"
  );
}

/**
 * Makes the directory of the login at `path`, 0700, where it is absent, and refuses one another
 * user could replace the login in ({@link checkLoginDirectory}), one this user cannot enter and
 * write to (a 0555 directory, or the 0600 one a `chmod -R 600` leaves), and a `path` that is a
 * directory, which no save can replace with the login. `dispatch login --device` runs it before it
 * asks for a code, so no person approves a login this machine could not keep there.
 */
export async function deviceLoginPlace(path: string): Promise<void> {
  const dir = dirname(path);
  try {
    await mkdir(dir, { recursive: true, mode: 0o700 });
  } catch (error) {
    throw new Error(`make the directory of ${DEVICE_LOGIN_FILE_NAMED}: ${readErrorCode(error)}`);
  }
  await checkLoginDirectory(dir);
  try {
    await access(dir, constants.W_OK | constants.X_OK);
  } catch (error) {
    const code = readErrorCode(error);
    // Only a mode this user can change has chmod for a cure; a read-only mount, say, has not.
    throw new Error(
      `this user cannot enter and write to the directory of ${DEVICE_LOGIN_FILE_NAMED}: ${code}${code === "EACCES" ? "; run chmod 700 on it" : ""}`
    );
  }
  let info: Stats;
  try {
    info = await lstat(path);
  } catch (error) {
    if (readErrorCode(error) === "ENOENT") return;
    throw new Error(`read ${DEVICE_LOGIN_FILE_NAMED}: ${readErrorCode(error)}`);
  }
  if (info.isDirectory()) {
    throw new Error(
      `${DEVICE_LOGIN_FILE_NAMED} is a directory, so no login can be kept in its place; remove it, or set ${DEVICE_LOGIN_FILE_VAR} to a file's path`
    );
  }
}

/**
 * The device login at `path`. A file that is not there is refused naming
 * {@link DEVICE_LOGIN_COMMAND}; one whose mode grants its group or anyone else any access, or that
 * this user does not own, is refused naming the fix, since it holds the login's key and refresh
 * token, and so is one in a directory another user could replace it in
 * ({@link checkLoginDirectory}); one that is not a whole login (no JSON object, a missing value, a
 * key that is not a P-256 PKCS#8 key, an expiry that is no time) is refused. Each names the file
 * by its setting ({@link DEVICE_LOGIN_FILE_NAMED}) and quotes none of its content. The refresh and
 * access tokens are held (`holdSecret`), so no line this process prints carries them.
 */
export async function readDeviceLogin(path: string): Promise<DeviceLogin> {
  let data: string;
  try {
    data = await readFile(path, "utf8");
  } catch (error) {
    const code = readErrorCode(error);
    throw new Error(
      code === "ENOENT"
        ? `this machine has no device login: ${DEVICE_LOGIN_FILE_NAMED} is not there; log this machine in with: ${DEVICE_LOGIN_COMMAND}`
        : `read ${DEVICE_LOGIN_FILE_NAMED}: ${code}`
    );
  }
  checkPrivate(
    await stat(path),
    DEVICE_LOGIN_FILE_NAMED,
    0o077,
    "lets others read the login's key; run chmod 600 on it",
    `remove it and log in again with: ${DEVICE_LOGIN_COMMAND}`
  );
  await checkLoginDirectory(dirname(path));
  let json: Record<string, unknown>;
  try {
    const parsed: unknown = JSON.parse(data);
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) throw new Error();
    json = parsed as Record<string, unknown>;
  } catch {
    throw new Error(
      `${DEVICE_LOGIN_FILE_NAMED} is not a device login: it does not read as one JSON object of the login's fields`
    );
  }
  const credentialId = member(json, "credential_id");
  const refreshToken = member(json, "refresh_token");
  const broker = member(json, "broker");
  const expiresAt = new Date(member(json, "expires_at"));
  if (
    credentialId === "" ||
    refreshToken === "" ||
    broker === "" ||
    Number.isNaN(expiresAt.getTime())
  ) {
    throw new Error(
      `${DEVICE_LOGIN_FILE_NAMED}: credential_id, refresh_token, broker and an RFC 3339 expires_at are all required`
    );
  }
  let key: KeyObject;
  try {
    key = createPrivateKey({
      key: Buffer.from(member(json, "key"), "base64"),
      format: "der",
      type: "pkcs8",
    });
  } catch {
    throw new Error(`${DEVICE_LOGIN_FILE_NAMED}: key is not a base64 PKCS#8 key`);
  }
  if (key.asymmetricKeyDetails?.namedCurve !== "prime256v1") {
    throw new Error(`${DEVICE_LOGIN_FILE_NAMED}: key is not a P-256 key`);
  }
  const accessToken = member(json, "access_token");
  const accessText = member(json, "access_expires_at");
  const accessExpiresAt = accessToken === "" || accessText === "" ? null : new Date(accessText);
  const accessUsable = accessExpiresAt !== null && !Number.isNaN(accessExpiresAt.getTime());
  return {
    key,
    credentialId,
    refreshToken: holdSecret(refreshToken, expiresAt),
    expiresAt,
    broker,
    accessToken: accessUsable ? holdSecret(accessToken, accessExpiresAt) : "",
    accessExpiresAt: accessUsable ? accessExpiresAt : null,
  };
}

/**
 * Keeps `temp`, a whole copy of a new login that could not be saved to the login's file at `path`
 * (`error` says why), beside that file as `<path>.kept-<uuid>`, a name no save removes, and
 * answers the error naming the copy by the suffix its name has, never by its path: the person
 * moves it into place, or revokes its credential. Where even that rename fails, `error` is
 * answered as it is, and the save removes its temporary file as it removes any other.
 */
async function keptCopy(temp: string, path: string, error: unknown): Promise<unknown> {
  const suffix = `.kept-${randomUUID()}`;
  try {
    await rename(temp, `${path}${suffix}`);
  } catch {
    return error;
  }
  return new Error(
    `${messageOf(error)}; a copy of the new login is kept beside ${DEVICE_LOGIN_FILE_NAMED}, under that file's name with ${suffix} added: move the copy over the file to use the login`,
    { cause: error }
  );
}

/**
 * Writes `login` to `path`, mode 0600, creating its directory 0700 where it is absent and refusing
 * one another user could replace the login in ({@link checkLoginDirectory}): through a new
 * temporary file beside it (a fresh name, created exclusively and 0600, so no file or link already
 * there is followed), synced, then renamed over `path`, so the file is always one whole login, the
 * old or the new. A save that fails, or whose lock is no longer the caller's, removes its
 * temporary file, and its error names the file by its setting; but where `keep` is set, as it is
 * for a new login, a copy written whole is kept ({@link keptCopy}), since the broker already holds
 * its credential live and no other copy of its key and refresh token exists. It first removes what
 * a save or a take-over that died left beside the file: a temporary copy of a login, which holds
 * its key and refresh token, and a lock moved aside, each untouched for the lock's stale bound, as
 * the Go helper's `RemoveLeftoverMachineTemps` removes its own; a younger one may be a live
 * caller's. A caller holds the login's lock ({@link withDeviceLoginLock}), so no other process's
 * refresh writes between its read and this write; `held` reads that lock again just before the
 * rename, and a lock that is no longer the caller's keeps nothing in the file. What is left is the
 * instant between that look and the rename.
 */
async function saveDeviceLogin(
  path: string,
  login: DeviceLogin,
  held: () => Promise<boolean>,
  keep = false
): Promise<void> {
  const dir = dirname(path);
  const data = JSON.stringify({
    key: login.key.export({ format: "der", type: "pkcs8" }).toString("base64"),
    credential_id: login.credentialId,
    refresh_token: login.refreshToken,
    expires_at: login.expiresAt.toISOString(),
    broker: login.broker,
    ...(login.accessToken === "" || login.accessExpiresAt === null
      ? {}
      : {
          access_token: login.accessToken,
          access_expires_at: login.accessExpiresAt.toISOString(),
        }),
  });
  await deviceLoginPlace(path);
  await removeUntouched(dir, isTemporaryCopy);
  await removeLeftLocks(path);
  const temp = join(dir, `.device-login-${randomUUID()}.json.tmp`);
  const saveFailed = (error: unknown): Error =>
    new Error(`save ${DEVICE_LOGIN_FILE_NAMED}: ${readErrorCode(error)}`);
  // Whether the temporary file holds the whole login, synced: only such a copy is worth keeping.
  let whole = false;
  try {
    try {
      const handle = await open(temp, "wx", 0o600);
      try {
        await handle.writeFile(data);
        await handle.sync();
      } finally {
        await handle.close();
      }
    } catch (error) {
      throw saveFailed(error);
    }
    whole = true;
    if (!(await held()))
      throw keep ? lockLost("the new login was not kept in its place") : lockLost();
    await rename(temp, path).catch((error: unknown) => {
      throw saveFailed(error);
    });
  } catch (error) {
    throw keep && whole ? await keptCopy(temp, path, error) : error;
  } finally {
    // After the rename, or once the copy is kept, there is no temporary file to remove.
    await rm(temp, { force: true });
  }
  // The rename is what readers see; syncing the directory is what keeps it across a power cut.
  // A directory this platform cannot sync still holds the whole login, so that failure is no
  // failure of the save.
  try {
    const handle = await open(dir, "r");
    try {
      await handle.sync();
    } finally {
      await handle.close();
    }
  } catch {}
}

/**
 * Runs `work` holding the device login's lock, `<path>.lock` (`device-login-lock.ts`, which
 * holds its protocol), and answers what it answers. `work` is handed `held`, which reads the lock
 * again and says whether it is still this call's: false once another caller took it from a holder
 * that was stopped. `signal` ends the wait for a lock another process holds, as `LockHeldError`.
 */
export async function withDeviceLoginLock<T>(
  path: string,
  work: (held: () => Promise<boolean>) => Promise<T>,
  options: DeviceLoginLockOptions = {}
): Promise<T> {
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
 * and expiry the broker's token hooks add, and the access token with when it lapses. An answer
 * that lacks any of them, or whose token is not DPoP-bound, is no login to keep, and nor is one
 * whose credential id holds a control character, since a login names that id to a person.
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
  const seconds = Math.min(answered.expires_in ?? 0, MOST_LIFETIME_SECONDS);
  if (
    credentialId === "" ||
    Number.isNaN(expiresAt.getTime()) ||
    refreshToken === "" ||
    !(seconds > 0) ||
    answered.token_type.toLowerCase() !== TOKEN_TYPE_DPOP.toLowerCase()
  ) {
    throw new BrokerTradeFailedError(
      `the secrets broker at ${urlForText(endpoint, BROKER_URL_SETTING)} answered the device login without a credential_id, an RFC 3339 credential_expires_at, a refresh_token, a positive expires_in or a DPoP-bound token`
    );
  }
  const accessExpiresAt = new Date(now + seconds * 1000);
  return {
    key,
    credentialId: printable(credentialId, "credential_id", endpoint),
    refreshToken: holdSecret(refreshToken, expiresAt),
    expiresAt,
    broker,
    accessToken: holdSecret(answered.access_token, accessExpiresAt),
    accessExpiresAt,
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
 * no http or https URL; the interval it polls at is the broker's, from {@link LEAST_POLL_SECONDS}
 * to {@link MOST_POLL_SECONDS}, and {@link DEFAULT_POLL_SECONDS} where the broker names none, and
 * the code lives as long as the broker says, {@link MOST_LIFETIME_SECONDS} at most. A refusal is
 * worded as every broker refusal is (`brokerRefusal`).
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
  const expiresAt = new Date(
    Date.now() + Math.min(answered.expires_in, MOST_LIFETIME_SECONDS) * 1000
  );
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
    intervalSeconds: Math.min(
      Math.max(answered.interval ?? DEFAULT_POLL_SECONDS, LEAST_POLL_SECONDS),
      MOST_POLL_SECONDS
    ),
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
 * Polls the device code `started` answered until the person decides, and keeps the login an
 * approval issues at `path` ({@link saveDeviceLogin}, under {@link withDeviceLoginLock}), answering
 * it with the credential it replaced ({@link KeptDeviceLogin}), which is read under the same lock
 * as the save, so a login another `dispatch login` kept there while this one waited for its
 * approval is the one named. Each poll waits the interval first (RFC 8628 Section 3.5), five
 * seconds longer after each `slow_down`, and proves the login's key. `authorization_pending` and
 * `slow_down` poll again, and so does a poll the broker did not answer, its 429 and a 5xx, as the
 * Go helper's `pollLogin` does; `access_denied` is a denial, and `expired_token`, `invalid_grant`
 * (a code the broker no longer knows) or the code's own lifetime passing is an expiry. Any other
 * answer of a 4xx, with an OAuth error or without one (a proxy's HTML page, a bare
 * `WWW-Authenticate` challenge), is one no later poll answers differently, so it stops the poll at
 * once, naming the status, rather than polling until the code lapses and saying it expired. Each
 * refusal names {@link DEVICE_LOGIN_COMMAND}, and none writes a file. An approved login that
 * cannot be kept is refused naming its credential, which the broker holds live, and where to
 * revoke it, and a copy of it written whole is kept beside the file ({@link keptCopy}). One kept
 * whose lock is then not let go is answered as kept: the machine uses that login, so it is not one
 * to revoke.
 */
export async function finishDeviceLogin(
  started: DeviceAuthorization,
  options: {
    readonly path: string;
    readonly fetch?: typeof fetch | undefined;
    readonly sleep?: ((ms: number) => Promise<void>) | undefined;
  }
): Promise<KeptDeviceLogin> {
  const wait = options.sleep ?? ((ms: number) => sleep(ms, undefined, true));
  const config = brokerConfiguration(started.brokerUrl, options.fetch ?? fetch, {
    key: loginDpopKey(started.key),
    nonce: { value: "" },
  });
  const endpoint = started.brokerUrl + TOKEN_PATH;
  const expired = new Error(
    `the code ${started.userCode} expired before ${started.person} approved this machine; ask again with: ${DEVICE_LOGIN_COMMAND}`
  );
  let interval = started.intervalSeconds;
  while (true) {
    if (Date.now() >= started.expiresAt.getTime()) throw expired;
    await wait(interval * 1000);
    let answered: TokenEndpointResponse;
    try {
      answered = await genericGrantRequest(config, GRANT_TYPE_DEVICE_CODE, {
        device_code: started.deviceCode,
      });
    } catch (error) {
      const code = error instanceof ResponseBodyError ? error.error : "";
      if (code === "access_denied") {
        throw new Error(
          `this machine's device login was denied, not approved as ${started.person}; ask again with: ${DEVICE_LOGIN_COMMAND}`
        );
      }
      if (code === "expired_token" || code === "invalid_grant") throw expired;
      if (code === "slow_down") interval += 5;
      const status = statusOf(error);
      const pollAgain =
        code === "authorization_pending" ||
        code === "slow_down" ||
        status === 0 ||
        status === 429 ||
        status >= 500;
      if (!pollAgain) {
        const refusal = brokerRefusal(
          "poll the code of",
          "this machine's device login",
          endpoint,
          error
        );
        throw new Error(`${refusal.message}; ask again with: ${DEVICE_LOGIN_COMMAND}`);
      }
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
      throw new Error(
        `${started.person} approved this machine's launcher credential ${login.credentialId}, but this machine could not keep it: ${messageOf(error)}. The credential stays live until it lapses or is revoked: where no copy of it is kept, revoke it in Machine logins on ${started.verificationUri} and log in again with: ${DEVICE_LOGIN_COMMAND}`,
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
  /** The login's file ({@link deviceLoginFile}). */
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
 * where the access token kept in the file has under a minute left.
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
   * An access token with under a minute left is refreshed first ({@link refreshDeviceLogin}), and
   * the login it answers is kept before `trade` runs. A kept access token the broker refuses as one
   * it no longer honours (`BrokerRefusedError.subjectNotHonoured`) is refreshed at once and traded
   * once more: a revoke ends a login before its token's expiry, and only the refresh tells that, as
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
  async spend<T>(trade: (subject: SubjectToken) => Promise<T>, signal?: AbortSignal): Promise<T> {
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
        let refreshed = login.accessToken === "" || expires - Date.now() < ACCESS_MARGIN_MS;
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

/** The refusal of a holder whose lock another caller took while it was stopped; `then` says what
 *  the holder did and did not do. */
function lockLost(
  then = "this trade refreshes and keeps nothing, and the next one reads the login again"
): Error {
  return new Error(
    `the lock beside ${DEVICE_LOGIN_FILE_NAMED} is no longer this process's: it stopped for over ${DEVICE_LOGIN_STALE_LOCK_MS / 1000} s while it held it (a laptop asleep, a process stopped), and another process took it; ${then}`
  );
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
 * `invalid_grant` — the credential revoked or run out, or the refresh token already rotated away —
 * is {@link DeviceLoginEndedError}, final; any other refusal is worded as every broker refusal is
 * (`brokerRefusal`).
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
      if (error instanceof ResponseBodyError && error.error === "invalid_grant") {
        throw new DeviceLoginEndedError(options.describe);
      }
      if (sent === 0 && statusOf(error) === 0) continue;
      throw brokerRefusal("refresh", options.describe, endpoint, error);
    }
  }
  const next = loginOf(login.key, login.broker, answered, Date.now());
  await saveDeviceLogin(path, next, options.held);
  return next;
}
