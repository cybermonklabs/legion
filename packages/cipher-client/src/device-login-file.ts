// packages/cipher-client/src/device-login-file.ts
//
// The file a machine keeps its device login in (`device-login.ts`): where it is, the checks that
// its place is one this user alone can replace it in and that it is a regular file, its read, and
// its save. Every message names the file by its setting, never by its path.
//
// - The file is `AGENT_SECRETS_DEVICE_LOGIN_FILE`, else `~/.config/legion/device-login.json`,
//   mode 0600 in a directory others cannot write to and this user owns.
// - Only a regular file is read or replaced, and only one of at most 64 KiB is read. A directory
//   cannot be replaced by a rename, and a read of a named pipe that no process writes to never
//   returns, so either at the file's path is refused before a code is asked for
//   (`deviceLoginPlace`). A read checks the directory first, and where the setting names a link,
//   the directory of the file it links to, then opens the path once, without waiting and as no
//   terminal, and checks and reads that one open file (`readDeviceLogin`).
// - A save writes a new temporary file beside it, syncs it and renames it over the file, under the
//   login's lock (`device-login-lock.ts`), so the file is always one whole login. A save of a new
//   login that fails once its copy is whole keeps that copy (`keptCopy`): the broker holds its
//   credential live from the approval on, and no other copy of its key and refresh token exists.

import { createPrivateKey, type KeyObject, randomUUID } from "node:crypto";
import { constants, type Stats } from "node:fs";
import { access, type FileHandle, mkdir, open, realpath, rename, rm, stat } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { holdSecret } from "@legion/contracts/held-secrets";
import {
  DEVICE_LOGIN_MOST_BYTES,
  DEVICE_LOGIN_STALE_LOCK_MS,
  removeLeftLocks,
  removeUntouched,
} from "./device-login-lock";
import { messageOf, readErrorCode } from "./error-text";
import { DEVICE_LOGIN_COMMAND } from "./token-errors";

/** Names the file a machine keeps its device login in. Unset, it is
 *  `~/.config/legion/device-login.json` ({@link deviceLoginFile}). */
export const DEVICE_LOGIN_FILE_VAR = "AGENT_SECRETS_DEVICE_LOGIN_FILE";

/** How every message names the login's file: by its setting, never by its path. The file holds
 *  the login's key and refresh token, and the setting is where a person who pasted a secret in
 *  place of a path put it, as the Go helper's `MachineFileNamed` says of its own file. */
export const DEVICE_LOGIN_FILE_NAMED = `the device login ${DEVICE_LOGIN_FILE_VAR} names (default ~/.config/legion/device-login.json)`;

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
 * no secret. Each refusal names the directory as `what` does, by the file's setting, and the fix.
 * `absent`, where it is given, is the refusal of a directory that is not there.
 */
async function checkLoginDirectory(
  dir: string,
  absent?: () => Error,
  what = `the directory of ${DEVICE_LOGIN_FILE_NAMED}`
): Promise<void> {
  let info: Stats;
  try {
    info = await stat(dir);
  } catch (error) {
    if (absent !== undefined && readErrorCode(error) === "ENOENT") throw absent();
    throw refusedBy(error, `read ${what}`);
  }
  checkPrivate(
    info,
    what,
    0o022,
    "lets others replace the login in it; run chmod 700 on it",
    "keep the login in a directory of this user's own"
  );
}

/**
 * The refusal of a login file's path where something other than a regular file is there: `there`
 * is its status, or the words for what the runtime's code says is there. A directory, which no
 * rename replaces, a named pipe, which a read waits on until a process writes to it, a socket, a
 * device or a loop of links. It names what is there, by the file's setting, and the fix.
 */
function notARegularFile(there: Stats | string): Error {
  const what =
    typeof there === "string"
      ? there
      : there.isDirectory()
        ? "a directory"
        : there.isFIFO()
          ? "a named pipe"
          : there.isSocket()
            ? "a socket"
            : there.isCharacterDevice() || there.isBlockDevice()
              ? "a device"
              : "something";
  return new Error(
    `${DEVICE_LOGIN_FILE_NAMED} is ${what}, not a regular file, so no login can be kept in its place; remove it, or set ${DEVICE_LOGIN_FILE_VAR} to a file's path`
  );
}

/**
 * The refusal of `doing`, a step on the login's file or its directory, that the runtime refused
 * with `error`. Where its code says what is in the path, that is said, as {@link notARegularFile}
 * says it: a socket or a device, which an open is refused (`ENXIO`), a loop of links (`ELOOP`), or
 * a file where a directory of the path must be (`ENOTDIR`). Else the code alone is named, never
 * the runtime's text, which repeats the path.
 */
function refusedBy(error: unknown, doing: string): Error {
  const code = readErrorCode(error);
  if (code === "ENXIO") return notARegularFile("a socket or a device");
  if (code === "ELOOP") return notARegularFile("a loop of links");
  if (code === "ENOTDIR") {
    return new Error(
      `${DEVICE_LOGIN_FILE_NAMED} has a file, not a directory, in its path, so no login can be kept there; set ${DEVICE_LOGIN_FILE_VAR} to a path in a directory`
    );
  }
  return new Error(`${doing}: ${code}`);
}

/**
 * Makes the directory of the login at `path`, 0700, where it is absent, and refuses one another
 * user could replace the login or its lock in ({@link checkLoginDirectory}). The login's lock runs
 * it before it opens anything there (`withDeviceLoginLock`), and so does {@link deviceLoginPlace}.
 */
export async function deviceLoginDirectory(path: string): Promise<void> {
  const dir = dirname(path);
  try {
    await mkdir(dir, { recursive: true, mode: 0o700 });
  } catch (error) {
    throw refusedBy(error, `make the directory of ${DEVICE_LOGIN_FILE_NAMED}`);
  }
  await checkLoginDirectory(dir);
}

/**
 * Makes the directory of the login at `path` and refuses one another user could replace the
 * login in ({@link deviceLoginDirectory}), and refuses one this user cannot enter and write to (a
 * 0555 directory, or the 0600 one a `chmod -R 600` leaves), and a `path` that is there and is not
 * a regular file, a link to one included ({@link notARegularFile}): a link to nothing is taken,
 * since a save's rename replaces the link. `dispatch login --device` runs it before it asks for a
 * code, so no person approves a login this machine could not keep there.
 */
export async function deviceLoginPlace(path: string): Promise<void> {
  await deviceLoginDirectory(path);
  const dir = dirname(path);
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
    info = await stat(path);
  } catch (error) {
    if (readErrorCode(error) === "ENOENT") return;
    throw refusedBy(error, `read ${DEVICE_LOGIN_FILE_NAMED}`);
  }
  if (!info.isFile()) throw notARegularFile(info);
}

/** How the login's file is opened to be read: read-only, without waiting (an open of a named pipe
 *  would wait for a writer), and never as this process's controlling terminal (the open of a tty
 *  can make it one). A link is followed: the setting may name one, and the directory of the file
 *  it links to is checked before the open ({@link readDeviceLogin}). */
const LOGIN_READ = constants.O_RDONLY | constants.O_NONBLOCK | constants.O_NOCTTY;

/**
 * The device login at `path`. A file that is not there is refused naming
 * {@link DEVICE_LOGIN_COMMAND}; a path that is not a regular file is refused ({@link
 * notARegularFile}); a file whose mode grants its group or anyone else any access, or that this
 * user does not own, is refused naming the fix, since it holds the login's key and refresh token,
 * and so is one in a directory another user could replace it in ({@link checkLoginDirectory}); one
 * over {@link DEVICE_LOGIN_MOST_BYTES}, or that is not a whole login (no JSON object, a missing
 * value, a key that is not a P-256 PKCS#8 key, an expiry that is no time) is refused. The
 * directory is checked before the file is opened: in one another user can write to, that user
 * picks what an open would open, and the open of a device can do something. Where the setting
 * names a link, the directory of the file it links to is checked too, before the open, for the
 * same reason: the file is in that directory. The path is then opened once ({@link LOGIN_READ}),
 * and the checks and the read are of that one open file: a look at the path and a second open
 * could open a named pipe put in its place between the two, and a read of a pipe nobody writes to
 * never returns. Each refusal names the file by its setting ({@link DEVICE_LOGIN_FILE_NAMED}) and
 * quotes none of its content. The refresh and access tokens are held (`holdSecret`), so no line
 * this process prints carries them.
 */
export async function readDeviceLogin(path: string): Promise<DeviceLogin> {
  const absent = () =>
    new Error(
      `this machine has no device login: ${DEVICE_LOGIN_FILE_NAMED} is not there; log this machine in with: ${DEVICE_LOGIN_COMMAND}`
    );
  const failed = (error: unknown) =>
    new Error(`read ${DEVICE_LOGIN_FILE_NAMED}: ${readErrorCode(error)}`);
  await checkLoginDirectory(dirname(path), absent);
  let real: string;
  try {
    real = await realpath(path);
  } catch (error) {
    throw readErrorCode(error) === "ENOENT"
      ? absent()
      : refusedBy(error, `read ${DEVICE_LOGIN_FILE_NAMED}`);
  }
  if (real !== resolve(path)) {
    await checkLoginDirectory(
      dirname(real),
      absent,
      `the directory of the file ${DEVICE_LOGIN_FILE_NAMED} links to`
    );
  }
  let handle: FileHandle;
  try {
    handle = await open(path, LOGIN_READ);
  } catch (error) {
    throw readErrorCode(error) === "ENOENT"
      ? absent()
      : refusedBy(error, `read ${DEVICE_LOGIN_FILE_NAMED}`);
  }
  let data: string;
  try {
    const info = await handle.stat().catch((error: unknown) => {
      throw failed(error);
    });
    if (!info.isFile()) throw notARegularFile(info);
    checkPrivate(
      info,
      DEVICE_LOGIN_FILE_NAMED,
      0o077,
      "lets others read the login's key; run chmod 600 on it",
      `remove it and log in again with: ${DEVICE_LOGIN_COMMAND}`
    );
    if (info.size > DEVICE_LOGIN_MOST_BYTES) {
      throw new Error(
        `${DEVICE_LOGIN_FILE_NAMED} is not a device login: it is over ${DEVICE_LOGIN_MOST_BYTES / 1024} KiB, many times a login's size; set ${DEVICE_LOGIN_FILE_VAR} to the login's own file, or log this machine in again with: ${DEVICE_LOGIN_COMMAND}`
      );
    }
    data = await handle.readFile("utf8").catch((error: unknown) => {
      throw failed(error);
    });
  } catch (error) {
    await handle.close().catch(() => {});
    throw error;
  }
  await handle.close().catch((error: unknown) => {
    throw failed(error);
  });
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

/** The refusal of a holder whose lock another caller took while it was stopped; `then` says what
 *  the holder did and did not do. */
export function lockLost(
  then = "this trade refreshes and keeps nothing, and the next one reads the login again"
): Error {
  return new Error(
    `the lock beside ${DEVICE_LOGIN_FILE_NAMED} is no longer this process's: it stopped for over ${DEVICE_LOGIN_STALE_LOCK_MS / 1000} s while it held it (a laptop asleep, a process stopped), and another process took it; ${then}`
  );
}

/** The refusal of a save of a new login whose copy, written whole, is kept beside the login's file
 *  under that file's name with `suffix` added ({@link keptCopy}). The copy holds the whole login,
 *  so moved over the file it is the login, and its credential needs no revoke. `failed` says why
 *  the save failed, in this package's own words and with no cause of its own, so its words are
 *  this error's and it is kept as no cause: that is left for a failure to let go of the lock after
 *  the save (`withLoginLock`). */
export class KeptCopyError extends Error {
  readonly suffix: string;

  constructor(suffix: string, failed: unknown) {
    super(
      `${messageOf(failed)}; a copy of the new login is kept beside ${DEVICE_LOGIN_FILE_NAMED}, under that file's name with ${suffix} added: move the copy over the file to use the login`
    );
    this.suffix = suffix;
  }
}

/**
 * Keeps `temp`, a whole copy of a new login that could not be saved to the login's file at `path`
 * (`error` says why), beside that file as `<path>.kept-<uuid>`, a name no save removes, and
 * answers {@link KeptCopyError}, which names the copy by the suffix its name has, never by its
 * path. Where even that rename fails, `error` is answered as it is, and the save removes its
 * temporary file as it removes any other.
 */
async function keptCopy(temp: string, path: string, error: unknown): Promise<unknown> {
  const suffix = `.kept-${randomUUID()}`;
  try {
    await rename(temp, `${path}${suffix}`);
  } catch {
    return error;
  }
  return new KeptCopyError(suffix, error);
}

/**
 * Writes `login` to `path`, mode 0600, creating its directory 0700 where it is absent and refusing
 * a place {@link deviceLoginPlace} refuses: through a new temporary file beside it (a fresh name,
 * created exclusively and 0600, so no file or link already there is followed), synced, then
 * renamed over `path`, so the file is always one whole login, the old or the new. A save that
 * fails, or whose lock is no longer the caller's, removes its temporary file, and its error names
 * the file by its setting; but where `keep` is set, as it is for a new login, a copy written whole
 * is kept ({@link keptCopy}), since the broker already holds its credential live and no other copy
 * of its key and refresh token exists. It first removes what a save or a take-over that died left
 * beside the file: a temporary copy of a login, which holds its key and refresh token, and a lock
 * moved aside, each untouched for the lock's stale bound, as the Go helper's
 * `RemoveLeftoverMachineTemps` removes its own; a younger one may be a live caller's. A caller
 * holds the login's lock (`withDeviceLoginLock`), so no other process's refresh writes between its
 * read and this write; `held` reads that lock again just before the rename, and a lock that is no
 * longer the caller's keeps nothing in the file. What is left is the instant between that look and
 * the rename.
 */
export async function saveDeviceLogin(
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
    // After the rename, or once the copy is kept, there is no temporary file to remove. One that
    // cannot be removed is left for a later save to remove once it is stale, and its failure,
    // whose text repeats the path, never takes the place of the save's answer.
    await rm(temp, { force: true }).catch(() => {});
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
