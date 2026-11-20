// packages/broker-client/src/device-login.ts
//
// A machine's device login: the identity of a machine that has no platform identity of its own,
// such as a person's laptop (`AGENT_SECRETS_MACHINE_IDENTITY=device-login`). It is the login the
// Go helper makes (`internal/broker/helper`, `Broker.Login`), made here so a machine with no Go
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
// that refresh would end. Each trade therefore runs under the lock file beside the login
// (`withDeviceLoginLock`): exclusive create, its holder touching it while it holds it, and a lock
// untouched for 30 s taken as one whose holder died, by one caller alone (`takeStale`, under a
// take-over lock of its own). Under it a process reads the file, refreshes
// only where the access token kept there has under a minute left, keeps what the refresh answered
// before it trades, and trades. The access token is kept in the file for the same reason: every
// `dispatch` command is a process of its own, and a refresh for each would rotate the login's
// refresh token at every command.

import { createPrivateKey, generateKeyPairSync, type KeyObject, randomUUID } from "node:crypto";
import type { Stats } from "node:fs";
import {
  type FileHandle,
  link,
  mkdir,
  open,
  readFile,
  rename,
  rm,
  stat,
  utimes,
} from "node:fs/promises";
import { dirname, join } from "node:path";
import { holdSecret, holdUrlUserinfo, scrub } from "@legion/contracts/held-secrets";
import { errorClass, isHttpUrl, urlForText } from "@legion/contracts/log-text";
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
import { type DpopKey, dpopKeyOf } from "./dpop";
import { readErrorCode } from "./error-text";
import type { SubjectToken, TokenSource } from "./source";
import { type DpopNonce, requestInit, sendWithProof, statusOf } from "./token-client";
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

/** A lock its holder has not touched for this long is one whose holder died, and is taken. */
export const DEVICE_LOGIN_STALE_LOCK_MS = 30_000;
/** How often a lock's holder touches it, well inside {@link DEVICE_LOGIN_STALE_LOCK_MS}. */
const LOCK_HEARTBEAT_MS = 10_000;
/** How long a caller waits between two tries of a lock another process holds. */
const LOCK_RETRY_MS = 100;

/** The least life an access token kept in the file must have left to be traded without a
 *  refresh: two trade attempts' worth (`TRADE_ATTEMPT_MS`), so it cannot lapse at the broker
 *  while the trade that presents it is still under way. */
const ACCESS_MARGIN_MS = 60_000;

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

/** A file's error code, "" where it has none. */
function codeOf(error: unknown): string {
  const { code } = (error ?? {}) as { code?: unknown };
  return typeof code === "string" ? code : "";
}

/** A member of the file's JSON as a string, "" where it is absent or not one. */
function member(json: Record<string, unknown>, name: string): string {
  const value = json[name];
  return typeof value === "string" ? value : "";
}

/**
 * The device login at `path`. A file that is not there is refused naming
 * {@link DEVICE_LOGIN_COMMAND}; one whose mode grants its group or anyone else any access, or that
 * this user does not own, is refused naming the fix, since it holds the login's key and refresh
 * token; one that is not a whole login (no JSON object, a missing value, a key that is not a P-256
 * PKCS#8 key, an expiry that is no time) is refused. Each names the file by its setting
 * ({@link DEVICE_LOGIN_FILE_NAMED}) and quotes none of its content. The refresh and access tokens
 * are held (`holdSecret`), so no line this process prints carries them.
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
  const info = await stat(path);
  const mode = info.mode & 0o777;
  if ((mode & 0o077) !== 0) {
    throw new Error(
      `${DEVICE_LOGIN_FILE_NAMED} is mode ${mode.toString(8).padStart(4, "0")}, which lets others read the login's key; run chmod 600 on it`
    );
  }
  const uid = process.getuid?.();
  if (uid !== undefined && info.uid !== uid) {
    throw new Error(
      `${DEVICE_LOGIN_FILE_NAMED} is owned by uid ${info.uid}, not this user (uid ${uid}); remove it and log in again with: ${DEVICE_LOGIN_COMMAND}`
    );
  }
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
 * Writes `login` to `path`, mode 0600, creating its directory 0700 where it is absent: through a
 * new temporary file beside it (a fresh name, created exclusively and 0600, so no file or link
 * already there is followed), synced, then renamed over `path`, so the file is always one whole
 * login, the old or the new. A save that fails removes its temporary file, and its error names the
 * file by its setting. A caller holds the login's lock ({@link withDeviceLoginLock}), so no other
 * process's refresh writes between its read and this write.
 */
export async function saveDeviceLogin(path: string, login: DeviceLogin): Promise<void> {
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
  const temp = join(dir, `.device-login-${randomUUID()}.json.tmp`);
  try {
    await mkdir(dir, { recursive: true, mode: 0o700 });
    const handle = await open(temp, "wx", 0o600);
    try {
      await handle.writeFile(data);
      await handle.sync();
    } finally {
      await handle.close();
    }
    await rename(temp, path);
  } catch (error) {
    await rm(temp, { force: true });
    throw new Error(`save ${DEVICE_LOGIN_FILE_NAMED}: ${readErrorCode(error)}`);
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

/** Waits `ms`, or until `signal` aborts. Its timer holds the process open: a caller waiting on
 *  the lock is waiting for that, and a process that exited would print nothing of why. */
function pause(ms: number, signal?: AbortSignal): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  if (signal?.aborted) {
    resolve();
    return promise;
  }
  const done = (): void => {
    clearTimeout(timer);
    signal?.removeEventListener("abort", done);
    resolve();
  };
  const timer = setTimeout(done, ms);
  signal?.addEventListener("abort", done, { once: true });
  return promise;
}

/** How {@link withDeviceLoginLock} waits. */
export interface DeviceLoginLockOptions {
  /** Ends the wait for a lock another process holds, with the signal's reason. */
  readonly signal?: AbortSignal | undefined;
  /** Waits between two tries of a held lock; a timer unless a test replaces it. */
  readonly sleep?: ((ms: number, signal?: AbortSignal) => Promise<void>) | undefined;
  /** Runs after this caller finds the lock stale and before it takes it over: the window in
   *  which another caller can take the same stale lock. Only a test holds a caller there. */
  readonly foundStale?: (() => Promise<void>) | undefined;
}

/** How long a take-over's own lock (`<lock>.steal`) may stand before it is one whose taker
 *  died: a take-over is a look, a rename and a removal, so this is far longer than one takes. */
const STEAL_STALE_MS = 5_000;

/** `path`'s status, or null where nothing is there. */
async function statOrNone(path: string): Promise<Stats | null> {
  try {
    return await stat(path);
  } catch (error) {
    if (codeOf(error) === "ENOENT") return null;
    throw new Error(`read the lock beside ${DEVICE_LOGIN_FILE_NAMED}: ${readErrorCode(error)}`);
  }
}

/** Creates `path` exclusively, 0600, holding `value`, and says whether it did: false where a
 *  file is already there. */
async function createOwn(path: string, value: string): Promise<boolean> {
  let handle: FileHandle;
  try {
    handle = await open(path, "wx", 0o600);
  } catch (error) {
    if (codeOf(error) === "EEXIST") return false;
    throw new Error(`take the lock beside ${DEVICE_LOGIN_FILE_NAMED}: ${readErrorCode(error)}`);
  }
  try {
    await handle.writeFile(value);
  } finally {
    await handle.close();
  }
  return true;
}

/** Removes `path` where it still holds `value`, the one its creator wrote, and leaves it where
 *  another caller's file has taken its place. */
async function removeOwn(path: string, value: string): Promise<void> {
  const held = await readFile(path, "utf8").catch(() => "");
  if (held === value) await rm(path, { force: true });
}

/**
 * Takes the lock at `lock` where its holder has not touched it for
 * {@link DEVICE_LOGIN_STALE_LOCK_MS}, and says whether the caller should try to create it again
 * at once: true where it took it or it was gone. A look and a take-over are two steps, so two
 * callers that both find one lock stale could otherwise both take it: the second would take the
 * first's fresh lock in the gap. So only the caller that creates the take-over's own lock,
 * `<lock>.steal`, takes over, and it looks at the lock again under it first: a caller that comes
 * second finds the first's fresh lock and waits for it. A steal lock left by a caller that died
 * taking over is removed once it is {@link STEAL_STALE_MS} old. Where two callers share that
 * window too, the lock's inode says whether what one moved is the stale lock it looked at; where
 * it is not, the moved lock is linked back in place. Only a third caller creating a lock in the
 * instant between that move and that link, after a caller died inside a take-over, can still
 * leave two holders.
 */
async function takeStale(
  lock: string,
  mine: string,
  options: DeviceLoginLockOptions
): Promise<boolean> {
  const found = await statOrNone(lock);
  if (found === null) return true;
  if (Date.now() - found.mtimeMs <= DEVICE_LOGIN_STALE_LOCK_MS) return false;
  await options.foundStale?.();
  const steal = `${lock}.steal`;
  if (!(await createOwn(steal, mine))) {
    const taking = await statOrNone(steal);
    if (taking !== null && Date.now() - taking.mtimeMs > STEAL_STALE_MS) {
      await rm(steal, { force: true });
    }
    return false;
  }
  try {
    const again = await statOrNone(lock);
    if (again === null) return true;
    if (Date.now() - again.mtimeMs <= DEVICE_LOGIN_STALE_LOCK_MS) return false;
    const moved = `${lock}.stale-${randomUUID()}`;
    try {
      await rename(lock, moved);
    } catch (error) {
      if (codeOf(error) === "ENOENT") return true;
      throw new Error(
        `take the stale lock beside ${DEVICE_LOGIN_FILE_NAMED}: ${readErrorCode(error)}`
      );
    }
    const took = await stat(moved);
    if (took.ino !== again.ino || took.dev !== again.dev) {
      // Another caller's fresh lock, moved in the gap: put back, unless a lock is there again.
      await link(moved, lock).catch(() => {});
      await rm(moved, { force: true });
      return false;
    }
    await rm(moved, { force: true });
    return true;
  } finally {
    await removeOwn(steal, mine);
  }
}

/**
 * Runs `work` holding the device login's lock, `<path>.lock`, and answers what it answers. The
 * lock is taken by creating the file exclusively, naming this process and a value of this call's
 * own; a lock another process holds is tried again every 100 ms until it is free, `signal` ends
 * (rejecting with its reason, `work` never run), or it is stale: untouched for
 * {@link DEVICE_LOGIN_STALE_LOCK_MS}, its holder gone, and taken by one caller alone (`takeStale`).
 * The holder touches the lock every 10 s while `work` runs, so a holder that is slow is never
 * taken for dead, and removes it when `work` settles, only while it still holds its own value.
 */
export async function withDeviceLoginLock<T>(
  path: string,
  work: () => Promise<T>,
  options: DeviceLoginLockOptions = {}
): Promise<T> {
  const lock = `${path}.lock`;
  const mine = `${process.pid} ${randomUUID()}\n`;
  const sleep = options.sleep ?? pause;
  try {
    await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  } catch (error) {
    throw new Error(`make the directory of ${DEVICE_LOGIN_FILE_NAMED}: ${readErrorCode(error)}`);
  }
  while (true) {
    options.signal?.throwIfAborted();
    if (await createOwn(lock, mine)) break;
    if (await takeStale(lock, mine, options)) continue;
    await sleep(LOCK_RETRY_MS, options.signal);
  }
  const beat = setInterval(() => {
    const now = new Date();
    // A touch that fails leaves the lock as it was; the holder is still the one working.
    utimes(lock, now, now).catch(() => {});
  }, LOCK_HEARTBEAT_MS);
  beat.unref();
  try {
    return await work();
  } finally {
    clearInterval(beat);
    await removeOwn(lock, mine);
  }
}

/** The OAuth client of the broker's machine-login routes at `brokerUrl`, sending through
 *  `fetchImpl`: with a proof of `proved`'s key on every request where it is given (the device code
 *  poll and the refresh), and with none where it is not (the device authorization). */
function brokerConfiguration(
  brokerUrl: string,
  fetchImpl: typeof fetch,
  signal: AbortSignal | undefined,
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
  // The broker is wherever the deployment put it, which on a dev stack is an http origin.
  allowInsecureRequests(config);
  config[customFetch] = (url, options) =>
    proved === undefined
      ? fetchImpl(url, requestInit(options, signal, options.headers))
      : sendWithProof(
          { key: proved.key, fetch: fetchImpl, nonce: proved.nonce },
          url,
          options,
          signal
        );
  return config;
}

/** `brokerUrl` as every request and record of a login names it: trimmed, less its trailing
 *  slashes, its user info held; refused, unquoted, where it is no http or https URL. */
function brokerOf(brokerUrl: string): string {
  const trimmed = withoutTrailingSlashes(brokerUrl.trim());
  if (!isHttpUrl(trimmed)) throw new Error(`${BROKER_SETTING} is not an http or https URL`);
  return holdUrlUserinfo(trimmed);
}

/** What a failed call to the broker's `endpoint` was, as `doing` names it: the OAuth error a 4xx
 *  answered, by its code and, scrubbed, its description; else its status and class. */
function brokerFailure(doing: string, endpoint: string, error: unknown): Error {
  const at = urlForText(endpoint, BROKER_SETTING);
  if (error instanceof ResponseBodyError) {
    const description: unknown = error.error_description;
    const said =
      typeof description === "string" && description !== "" ? `: ${scrub(description)}` : "";
    return new Error(
      `${doing} at ${at}: the broker answered HTTP ${error.status} ${error.error}${said}`
    );
  }
  const status = statusOf(error);
  return new Error(
    `${doing} at ${at}: ${status === 0 ? errorClass(error) : `HTTP ${status}: ${errorClass(error)}`}`
  );
}

/**
 * The login a grant's answer issues for `key` at `broker`: the refresh token, the credential's id
 * and expiry the broker's token hooks add, and the access token with when it lapses. An answer
 * that lacks any of them, or whose token is not DPoP-bound, is no login to keep.
 */
function loginOf(
  key: KeyObject,
  broker: string,
  answered: TokenEndpointResponse,
  now: number
): DeviceLogin {
  const credentialId =
    typeof answered["credential_id"] === "string" ? answered["credential_id"] : "";
  const expiresAt = new Date(
    typeof answered["credential_expires_at"] === "string" ? answered["credential_expires_at"] : ""
  );
  const refreshToken = answered.refresh_token ?? "";
  const seconds = answered.expires_in ?? 0;
  if (
    credentialId === "" ||
    Number.isNaN(expiresAt.getTime()) ||
    refreshToken === "" ||
    !(seconds > 0) ||
    answered.token_type.toLowerCase() !== TOKEN_TYPE_DPOP.toLowerCase()
  ) {
    throw new BrokerTradeFailedError(
      `the secrets broker at ${urlForText(broker + TOKEN_PATH, BROKER_SETTING)} answered the device login without a credential_id, an RFC 3339 credential_expires_at, a refresh_token, a positive expires_in or a DPoP-bound token`
    );
  }
  const accessExpiresAt = new Date(now + seconds * 1000);
  return {
    key,
    credentialId,
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
 * Starts a device login of this machine, `host`, at the broker `brokerUrl`, for `person` to
 * approve: a fresh P-256 key signs the request object `machine.Service.verifyPersonLogin`
 * verifies — `typ` `agent-secrets-request+jwt`, ES256 with the public key embedded, `iss` its
 * thumbprint, `aud` the broker, a `jti` of its own, `iat` now and `exp` 600 s on, one
 * `launcher_credential` detail naming `host`, and `login_hint` naming `person` — and posts it to
 * the broker's device authorization endpoint with the key's thumbprint as `dpop_jkt`, as the
 * machine-login client and its one scope. It answers what the person types and where. A refusal
 * names the broker as `AGENT_SECRETS_URL` names it and what the broker said.
 */
export async function startDeviceLogin(options: {
  readonly brokerUrl: string;
  readonly person: string;
  readonly host: string;
  readonly fetch?: typeof fetch | undefined;
}): Promise<DeviceAuthorization> {
  const brokerUrl = brokerOf(options.brokerUrl);
  const person = options.person.trim();
  const { privateKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
  // The public half as every proof of the login embeds it: `kty`, `crv`, `x` and `y` alone.
  const jwk = dpopKeyOf(privateKey, "this machine's device login key").publicJwk;
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
  const config = brokerConfiguration(brokerUrl, options.fetch ?? fetch, undefined);
  let answered: DeviceAuthorizationResponse;
  try {
    answered = await initiateDeviceAuthorization(config, {
      scope: MACHINE_LOGIN_SCOPE,
      dpop_jkt: thumbprint,
      request,
    });
  } catch (error) {
    throw brokerFailure(
      "ask the secrets broker for a device login code",
      brokerUrl + DEVICE_AUTHORIZATION_PATH,
      error
    );
  }
  const expiresAt = new Date(Date.now() + answered.expires_in * 1000);
  return {
    brokerUrl,
    person,
    userCode: answered.user_code,
    verificationUri: answered.verification_uri,
    verificationUriComplete: answered.verification_uri_complete,
    expiresAt,
    intervalSeconds: answered.interval ?? 5,
    deviceCode: holdSecret(answered.device_code, expiresAt),
    key: privateKey,
  };
}

/**
 * Polls the device code `started` answered until the person decides, and keeps the login an
 * approval issues at `path` ({@link saveDeviceLogin}, under {@link withDeviceLoginLock}), answering
 * it. Each poll waits the broker's interval first (RFC 8628 Section 3.5), five seconds longer after
 * each `slow_down`, and proves the login's key. `authorization_pending`, a poll that reached no
 * broker, the broker's 429 and a 5xx poll again, as the Go helper's `pollLogin` does;
 * `access_denied` is a denial, and `expired_token`, `invalid_grant` (a code the broker no longer
 * knows) or the code's own lifetime passing is an expiry. Any other OAuth refusal of a 4xx is
 * one no later poll answers differently, so it stops the poll at once, naming the broker's code,
 * rather than polling until the code lapses and saying it expired. Each refusal names
 * {@link DEVICE_LOGIN_COMMAND}, and none writes a file. `signal` ends the wait.
 */
export async function finishDeviceLogin(
  started: DeviceAuthorization,
  options: {
    readonly path: string;
    readonly fetch?: typeof fetch | undefined;
    readonly signal?: AbortSignal | undefined;
    readonly sleep?: ((ms: number, signal?: AbortSignal) => Promise<void>) | undefined;
  }
): Promise<DeviceLogin> {
  const sleep = options.sleep ?? pause;
  const config = brokerConfiguration(started.brokerUrl, options.fetch ?? fetch, options.signal, {
    key: dpopKeyOf(started.key, "this machine's device login key"),
    nonce: { value: "" },
  });
  const expired = new Error(
    `the code ${started.userCode} expired before ${started.person} approved this machine; ask again with: ${DEVICE_LOGIN_COMMAND}`
  );
  let interval = started.intervalSeconds;
  while (true) {
    options.signal?.throwIfAborted();
    if (Date.now() >= started.expiresAt.getTime()) throw expired;
    await sleep(interval * 1000, options.signal);
    options.signal?.throwIfAborted();
    let answered: TokenEndpointResponse;
    try {
      answered = await genericGrantRequest(config, GRANT_TYPE_DEVICE_CODE, {
        device_code: started.deviceCode,
      });
    } catch (error) {
      if (error instanceof ResponseBodyError) {
        if (error.error === "slow_down") interval += 5;
        if (error.error === "access_denied") {
          throw new Error(
            `this machine's device login was denied, not approved as ${started.person}; ask again with: ${DEVICE_LOGIN_COMMAND}`
          );
        }
        if (error.error === "expired_token" || error.error === "invalid_grant") throw expired;
        const pending = error.error === "authorization_pending" || error.error === "slow_down";
        if (!pending && error.status !== 429 && error.status < 500) {
          const failure = brokerFailure(
            "poll the secrets broker for this machine's device login",
            started.brokerUrl + TOKEN_PATH,
            error
          );
          throw new Error(`${failure.message}; ask again with: ${DEVICE_LOGIN_COMMAND}`);
        }
      }
      continue;
    }
    const login = loginOf(started.key, started.brokerUrl, answered, Date.now());
    await withDeviceLoginLock(options.path, () => saveDeviceLogin(options.path, login), {
      signal: options.signal,
    });
    return login;
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
  /** The broker, as `AGENT_SECRETS_URL` names it: the one the login must have been made at. */
  readonly brokerUrl: string;
  /** The fetch a refresh goes through; the global one by default. */
  readonly fetch?: typeof fetch | undefined;
  /** The lock every trade runs under: {@link withDeviceLoginLock} unless a caller replaces it,
   *  which only the suite's control does, to show the lock is what keeps one refresh token from
   *  being spent twice. */
  readonly lock?:
    | (<T>(path: string, work: () => Promise<T>, options: DeviceLoginLockOptions) => Promise<T>)
    | undefined;
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
  readonly #fetch: typeof fetch;
  readonly #lock: NonNullable<DeviceLoginIdentityOptions["lock"]>;
  /** The login's key as this identity first read it, made once; a read that failed is not kept,
   *  so the next one reads the file again. */
  #key: Promise<LoginKey> | undefined;

  constructor(options: DeviceLoginIdentityOptions) {
    this.#path = options.path;
    this.#brokerUrl = brokerOf(options.brokerUrl);
    this.#fetch = options.fetch ?? fetch;
    this.#lock = options.lock ?? withDeviceLoginLock;
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
        const dpop = dpopKeyOf(key, "this machine's device login key");
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
   * proves (a `dispatch login --device` since it started), each naming what cures it. An access
   * token with under a minute left is refreshed first ({@link refreshDeviceLogin}), and the login
   * it answers is kept before `trade` runs. `signal` ends the wait for the lock and the refresh.
   */
  async spend<T>(trade: (subject: SubjectToken) => Promise<T>, signal?: AbortSignal): Promise<T> {
    const { thumbprint } = await this.#loaded();
    return await this.#lock(
      this.#path,
      async () => {
        let login = await readDeviceLogin(this.#path);
        if (withoutTrailingSlashes(login.broker) !== this.#brokerUrl) {
          throw new Error(
            `${DEVICE_LOGIN_FILE_NAMED} was made at another secrets broker than ${BROKER_SETTING} names; log this machine in at this one with: ${DEVICE_LOGIN_COMMAND}`
          );
        }
        const now = dpopKeyOf(login.key, "this machine's device login key").publicJwk;
        if ((await calculateJwkThumbprint(now)) !== thumbprint) {
          throw new Error(
            `${DEVICE_LOGIN_FILE_NAMED} holds another login than the one this process started with, since ${DEVICE_LOGIN_COMMAND} ran again; start this session again to trade the new one`
          );
        }
        const expires = login.accessExpiresAt?.getTime() ?? 0;
        if (login.accessToken === "" || expires - Date.now() < ACCESS_MARGIN_MS) {
          login = await refreshDeviceLogin(this.#path, login, {
            describe: this.describe(),
            fetch: this.#fetch,
            signal,
          });
        }
        return await trade({
          token: login.accessToken,
          tokenType: SUBJECT_TYPE_ACCESS,
        });
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
 * answers at `path` and answering it. A caller holds the login's lock. `invalid_grant` — the
 * credential revoked or run out, or the refresh token already rotated away — is
 * {@link DeviceLoginEndedError}, final; any other 4xx is the broker's refusal
 * (`BrokerRefusedError`, which a later trade may cure where it is a 429 or a DPoP refusal); a
 * refresh that got no answer, a 5xx or an answer that is no login is `BrokerTradeFailedError`. A
 * login the broker rotated but this process could not save is an error the next trade cures: the
 * file still holds the refresh token this one spent, which the broker redeems again within its
 * five-minute grace.
 */
export async function refreshDeviceLogin(
  path: string,
  login: DeviceLogin,
  options: {
    readonly describe: string;
    readonly fetch?: typeof fetch | undefined;
    readonly signal?: AbortSignal | undefined;
  }
): Promise<DeviceLogin> {
  const config = brokerConfiguration(login.broker, options.fetch ?? fetch, options.signal, {
    key: dpopKeyOf(login.key, "this machine's device login key"),
    nonce: { value: "" },
  });
  const endpoint = login.broker + TOKEN_PATH;
  let answered: TokenEndpointResponse;
  try {
    answered = await genericGrantRequest(config, GRANT_TYPE_REFRESH, {
      refresh_token: login.refreshToken,
    });
  } catch (error) {
    if (error instanceof ResponseBodyError && error.status >= 400 && error.status < 500) {
      if (error.error === "invalid_grant") throw new DeviceLoginEndedError(options.describe);
      const description: unknown = error.error_description;
      throw new BrokerRefusedError(
        error.status,
        error.error,
        typeof description === "string" ? description : "",
        options.describe
      );
    }
    const status = statusOf(error);
    throw new BrokerTradeFailedError(
      `refresh ${options.describe} at ${urlForText(endpoint, BROKER_SETTING)}: ${status === 0 ? errorClass(error) : `HTTP ${status}: ${errorClass(error)}`}`
    );
  }
  const next = loginOf(login.key, login.broker, answered, Date.now());
  await saveDeviceLogin(path, next);
  return next;
}
