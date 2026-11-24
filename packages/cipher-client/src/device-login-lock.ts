// packages/cipher-client/src/device-login-lock.ts
//
// The lock every process of one machine takes before it reads, refreshes or trades the machine's
// device login (`device-login.ts`, `withDeviceLoginLock`): `<file>.lock`, beside the login's file,
// in a directory the caller made and checked is one only this user can write to.
//
// - It is taken by creating the file exclusively, holding this process's pid and a value of this
//   call's own, and its holder touches it every 5 s while it holds it. A lock is the node at its
//   path, never what a link there names, since no caller makes one as a link. It is looked at
//   through one open of it, without waiting and as no terminal, and that one open file gives both
//   its status and its holder value, which is read only where it is a regular file of at most
//   64 KiB: a named pipe left at its path holds no caller, and neither does a huge file. Such a
//   node, or one no open reads (a link, a socket), holds no holder's value, so it is taken over
//   once stale, by its own status.
// - A lock untouched for 15 s is taken as one whose holder died. Only one caller takes it over:
//   the one that creates the take-over's own lock, `<file>.lock.steal`, and finds the lock still
//   stale under it. A `.lock.steal` left for 5 s by a caller that died or stopped is removed.
// - Every removal is of the very file the caller means: a stale lock it takes over, a dead
//   `.lock.steal` it clears, and its own lock or `.lock.steal` as it lets go. The caller looks at
//   the file first, and leaves one that is not the one untouched. A look and a removal by name are
//   still two steps, and another caller can put a file of its own in the place between them, so
//   the caller never removes by name: it moves the file aside to `<name>.stale-<uuid>`, looks at
//   what it moved, removes that only where it is the one, and otherwise links it back. A
//   take-over's file is the one whose device, inode, modification time and holder value are all
//   the ones the caller saw: the inode alone tells nothing, since a filesystem can give a new file
//   the number of one removed a moment before (XFS does at once). A caller's own file is the one
//   that holds the caller's value, which no other caller writes.
// - Two windows can still leave two holders. One is a file made in the instant between a move
//   aside and its link back, which needs the file to change in the instant between the caller's
//   look and its move: a holder back from a stop of over 15 s, or another caller's take-over after
//   this caller stopped there, for over 5 s in a take-over or for over 15 s as it let go. The
//   other needs no race: a holder alive but stopped for over 15 s (a laptop asleep, a process
//   stopped) cannot touch its lock, so another caller takes it while the first still works.
//   `work` is handed `held`, which reads the lock again, so such a holder can stop before it does
//   harm.

import { randomUUID } from "node:crypto";
import { closeSync, constants, fstatSync, openSync, readSync, type Stats } from "node:fs";
import {
  type FileHandle,
  link,
  lstat,
  open,
  readdir,
  rename,
  rm,
  stat,
  utimes,
} from "node:fs/promises";
import { basename, dirname, join } from "node:path";
import { readErrorCode } from "./error-text";
import { sleep, TRADE_ATTEMPT_MS } from "./token-client";
import { LockHeldError } from "./token-errors";

/**
 * A lock its holder has not touched for this long is taken as one whose holder died: half of a
 * trade's attempt (`TRADE_ATTEMPT_MS`), so a trade that waited out a dead holder's lock from the
 * moment it was touched still has half its attempt for the refresh and the exchange.
 */
export const DEVICE_LOGIN_STALE_LOCK_MS = TRADE_ATTEMPT_MS / 2;

/** How often a lock's holder touches it: three times in each {@link DEVICE_LOGIN_STALE_LOCK_MS}. */
export const LOCK_HEARTBEAT_MS = DEVICE_LOGIN_STALE_LOCK_MS / 3;

/** The most a device login or its lock is read of: 64 KiB, many times a login's size, so a file
 *  far larger (a log, a disk image named by mistake) is refused, not read whole into memory. */
export const DEVICE_LOGIN_MOST_BYTES = 64 * 1024;

/** How a lock is opened to be looked at: read-only, without waiting (an open of a named pipe
 *  would wait for a writer), never as this process's controlling terminal (the open of a tty can
 *  make it one), and not through a link. */
const LOCK_READ =
  constants.O_RDONLY | constants.O_NONBLOCK | constants.O_NOCTTY | constants.O_NOFOLLOW;

/** How long a caller waits between two tries of a lock another process holds. */
const LOCK_RETRY_MS = 100;

/** How long a take-over's own lock (`<lock>.steal`) may stand before it is one whose taker
 *  died: a take-over is a look, a rename and a removal, so this is far longer than one takes. */
const STEAL_STALE_MS = 5_000;

/** How {@link withLoginLock} waits. */
export interface DeviceLoginLockOptions {
  /** Ends the wait for a lock another process holds, as `LockHeldError`. */
  readonly signal?: AbortSignal | undefined;
  /** Waits between two tries of a held lock; a timer that holds the process open unless a test
   *  replaces it. */
  readonly sleep?: ((ms: number, signal?: AbortSignal) => Promise<void>) | undefined;
  /** Runs at each step of a take-over where another caller can act between this caller's look
   *  and what it does next ({@link TakeOverStep}). Only a test holds a caller there. */
  readonly at?: ((step: TakeOverStep) => Promise<void>) | undefined;
}

/**
 * The windows of a take-over in which another caller can act: `lock-stale`, after this caller
 * finds the lock stale and before it tries `<lock>.steal`; `steal-stale`, after it finds another
 * caller's `<lock>.steal` stale and before it removes it; `taking`, under its own
 * `<lock>.steal`, after it finds the lock still stale and before it moves it aside.
 */
export type TakeOverStep = "lock-stale" | "steal-stale" | "taking";

/** `path`'s own status, a link's and not what it names, or null where nothing is there. `what`
 *  names the file in an error. */
async function statOrNone(path: string, what: string): Promise<Stats | null> {
  try {
    return await lstat(path);
  } catch (error) {
    if (readErrorCode(error) === "ENOENT") return null;
    throw new Error(`read ${what}: ${readErrorCode(error)}`);
  }
}

/** Creates `path` exclusively, 0600, holding `value`, and says whether it did: false where a
 *  file is already there. `named` names the login in an error. */
async function createOwn(path: string, value: string, named: string): Promise<boolean> {
  let handle: FileHandle;
  try {
    handle = await open(path, "wx", 0o600);
  } catch (error) {
    if (readErrorCode(error) === "EEXIST") return false;
    throw new Error(`take the lock beside ${named}: ${readErrorCode(error)}`);
  }
  try {
    await handle.writeFile(value);
  } finally {
    await handle.close();
  }
  return true;
}

/** A lock file as a caller looked at it: its status and the holder value written in it. */
interface Seen {
  readonly stats: Stats;
  readonly value: string;
}

/**
 * `path` as it is now ({@link Seen}), or null where nothing is there. Its status and its holder
 * value are of one open of it ({@link LOCK_READ}), so they are of one file. The value is "" where
 * that file is no regular one (a plain read of a named pipe would wait until a process writes to
 * it, and so would every caller of the lock), is over {@link DEVICE_LOGIN_MOST_BYTES}, or cannot
 * be read. Where the path cannot be opened at all (a link, a socket), its own status is taken
 * ({@link statOrNone}), with "" for its value. `what` names the file in an error.
 */
async function look(path: string, what: string): Promise<Seen | null> {
  let handle: FileHandle;
  try {
    handle = await open(path, LOCK_READ);
  } catch (error) {
    if (readErrorCode(error) === "ENOENT") return null;
    const stats = await statOrNone(path, what);
    return stats === null ? null : { stats, value: "" };
  }
  let seen: Seen;
  try {
    const stats = await handle.stat();
    const readable = stats.isFile() && stats.size <= DEVICE_LOGIN_MOST_BYTES;
    seen = { stats, value: readable ? await handle.readFile("utf8").catch(() => "") : "" };
  } catch (error) {
    await handle.close().catch(() => {});
    throw new Error(`read ${what}: ${readErrorCode(error)}`);
  }
  // The file is closed once on each path: a second close after one that failed could close a
  // descriptor another open was handed since.
  await handle.close().catch((error: unknown) => {
    throw new Error(`read ${what}: ${readErrorCode(error)}`);
  });
  return seen;
}

/** Whether a file as a caller looks at it is the very file `was` names: the same device, inode,
 *  modification time and holder value. */
function sameFile(was: Seen): (seen: Seen) => boolean {
  return (seen) =>
    seen.stats.dev === was.stats.dev &&
    seen.stats.ino === was.stats.ino &&
    seen.stats.mtimeMs === was.stats.mtimeMs &&
    seen.value === was.value;
}

/**
 * Removes `path` where it is still the file `isIt` takes it for, and says whether the caller may
 * go on: true where it removed that file or nothing was there. It moves the file aside first, and
 * then looks at what it moved: a file `isIt` does not take is linked back in place, unless a file
 * is there again, and the answer is false. `what` names the file in an error.
 */
async function removeIfStill(
  path: string,
  isIt: (seen: Seen) => boolean,
  what: string
): Promise<boolean> {
  const moved = `${path}.stale-${randomUUID()}`;
  try {
    await rename(path, moved);
  } catch (error) {
    if (readErrorCode(error) === "ENOENT") return true;
    throw new Error(`move ${what} aside: ${readErrorCode(error)}`);
  }
  const took = await look(moved, what);
  const same = took !== null && isIt(took);
  if (!same) await link(moved, path).catch(() => {});
  await rm(moved, { force: true }).catch((error: unknown) => {
    throw new Error(`remove ${what}, moved aside: ${readErrorCode(error)}`);
  });
  return same;
}

/**
 * Lets go of `path`, a lock or a take-over lock this caller made holding `value`. It looks at the
 * file first and leaves one holding another caller's value untouched: another caller took it over
 * while this one was stopped. It removes its own only through {@link removeIfStill}, never by
 * name, since another caller can take it over between that look and the removal too, where this
 * caller stops there for longer than the file's stale bound. `what` names the file in an error.
 */
async function removeOwn(path: string, value: string, what: string): Promise<void> {
  const seen = await look(path, what);
  if (seen === null || seen.value !== value) return;
  await removeIfStill(path, (took) => took.value === value, what);
}

/**
 * Takes the lock at `lock` where its holder has not touched it for
 * {@link DEVICE_LOGIN_STALE_LOCK_MS}, and says whether the caller should try to create it again
 * at once: true where it took it or it was gone. A look and a take-over are two steps, so two
 * callers that both find one lock stale could otherwise both take it: the second would take the
 * first's fresh lock in the gap. So only the caller that creates `<lock>.steal` takes over, and
 * it looks at the lock again under it first: a caller that comes second finds the first's fresh
 * lock and waits for it. A take-over lock left {@link STEAL_STALE_MS} is removed, so two callers
 * can then be inside one take-over; each removal is of the very file the caller looked at
 * ({@link removeIfStill}), so a caller that comes second puts the other's file back and waits.
 */
async function takeStale(
  lock: string,
  mine: string,
  named: string,
  options: DeviceLoginLockOptions
): Promise<boolean> {
  const locked = `the lock beside ${named}`;
  const found = await statOrNone(lock, locked);
  if (found === null) return true;
  if (Date.now() - found.mtimeMs <= DEVICE_LOGIN_STALE_LOCK_MS) return false;
  await options.at?.("lock-stale");
  const steal = `${lock}.steal`;
  const stealing = `the take-over lock beside ${named}`;
  if (!(await createOwn(steal, mine, named))) {
    const taking = await look(steal, stealing);
    if (taking !== null && Date.now() - taking.stats.mtimeMs > STEAL_STALE_MS) {
      await options.at?.("steal-stale");
      await removeIfStill(steal, sameFile(taking), `the dead take-over lock beside ${named}`);
    }
    return false;
  }
  try {
    const again = await look(lock, locked);
    if (again === null) return true;
    if (Date.now() - again.stats.mtimeMs <= DEVICE_LOGIN_STALE_LOCK_MS) return false;
    await options.at?.("taking");
    return await removeIfStill(lock, sameFile(again), `the stale lock beside ${named}`);
  } finally {
    await removeOwn(steal, mine, stealing);
  }
}

/**
 * Keeps `released`, the failure to let go of a lock after `work` failed with `failed`, on `failed`
 * itself, so `work`'s caller is answered the very error `work` threw, of its own type: as its
 * `cause` where it has none, and else as its `releaseFailure`, so the cause `work` gave it stays.
 * An error that takes no field (a frozen one) is left as it is: a write to it throws, and that
 * throw would take the place of `work`'s error.
 */
function keepReleaseFailure(failed: unknown, released: unknown): void {
  if (!(failed instanceof Error)) return;
  Reflect.defineProperty(failed, failed.cause === undefined ? "cause" : "releaseFailure", {
    value: released,
    writable: true,
    enumerable: true,
    configurable: true,
  });
}

/**
 * Runs `work` holding the lock of the login at `path`, `<path>.lock`, and answers what it
 * answers. The caller has made the login's directory and checked that no other user can write to
 * it, so no other user picks what is at the lock's path. A lock another process holds is tried
 * again every 100 ms until it is free, `signal` ends (rejecting with `LockHeldError`, which names
 * the lock, with the signal's reason as its cause, and `work` never run), or it is stale and this
 * caller alone takes it ({@link takeStale}). The holder touches the lock every
 * {@link LOCK_HEARTBEAT_MS} while `work` runs, on a timer that holds no process open, so a holder
 * that is slow is never taken for dead, and lets go of it when `work` settles, removing it only
 * while it still holds its own value ({@link removeOwn}). Where `work` rejects and letting go then
 * fails too, `work`'s error is the one rejected with, since it says what `work` did and did not
 * do, and the release's is kept on it ({@link keepReleaseFailure}). `work` is handed `held`, which
 * reads the lock again and says whether it still holds this call's value: false once another
 * caller took it from a holder that was stopped. `named` names the login in an error.
 */
export async function withLoginLock<T>(
  path: string,
  named: string,
  work: (held: () => Promise<boolean>) => Promise<T>,
  options: DeviceLoginLockOptions = {}
): Promise<T> {
  const lock = `${path}.lock`;
  const locked = `the lock beside ${named}`;
  const mine = `${process.pid} ${randomUUID()}\n`;
  const wait = options.sleep ?? ((ms: number, signal?: AbortSignal) => sleep(ms, signal, true));
  while (true) {
    if (options.signal?.aborted === true) {
      throw new LockHeldError(locked, { cause: options.signal.reason });
    }
    if (await createOwn(lock, mine, named)) break;
    if (await takeStale(lock, mine, named, options)) continue;
    await wait(LOCK_RETRY_MS, options.signal);
  }
  // Each beat is the touch itself, so a caller of the timer's callback can await the touch.
  const beat = setInterval(async () => {
    const now = new Date();
    // A touch that fails leaves the lock as it was; the holder is still the one working.
    await utimes(lock, now, now).catch(() => {});
  }, LOCK_HEARTBEAT_MS);
  beat.unref();
  let answer: T;
  try {
    answer = await work(async () => (await look(lock, locked))?.value === mine);
  } catch (error) {
    clearInterval(beat);
    await removeOwn(lock, mine, locked).catch((released: unknown) =>
      keepReleaseFailure(error, released)
    );
    throw error;
  }
  clearInterval(beat);
  await removeOwn(lock, mine, locked);
  return answer;
}

/**
 * Whether process `pid` holds the lock of the device login at `path` (`<path>.lock`, whose first
 * word is its holder's pid, {@link withLoginLock}), read synchronously and opened as {@link look}
 * opens a lock: never waiting on a named pipe, never following a link, a regular file only, and
 * at most {@link DEVICE_LOGIN_MOST_BYTES} read. Synchronous for a caller that must act on the
 * answer before any other task of its process runs: claude-envoy's open-asks hook asks it and
 * exits in one step, so its own trade cannot take the lock, and start a refresh the exit would
 * cut, between the two. No lock, and one that cannot be read, is held by no process; no error
 * leaves it.
 */
export function loginLockHeldBy(path: string, pid: number): boolean {
  let fd: number;
  try {
    fd = openSync(`${path}.lock`, LOCK_READ);
  } catch {
    return false;
  }
  try {
    if (!fstatSync(fd).isFile()) return false;
    const bytes = Buffer.alloc(DEVICE_LOGIN_MOST_BYTES);
    const read = readSync(fd, bytes, 0, bytes.length, 0);
    return bytes.toString("utf8", 0, read).startsWith(`${pid} `);
  } catch {
    return false;
  } finally {
    // A close that fails changes nothing read, and must not throw out of the caller's exit.
    try {
      closeSync(fd);
    } catch {}
  }
}

/** Whether `name` is `prefix` and a UUID: a file a caller named with a fresh value of its own. */
function namedWithUuid(name: string, prefix: string): boolean {
  return name.startsWith(prefix) && /^[0-9a-f-]{36}$/.test(name.slice(prefix.length));
}

/**
 * Removes each file in `dir` that `left` takes by its name and that is untouched for
 * {@link DEVICE_LOGIN_STALE_LOCK_MS}: a file another caller left when it died. A younger one may be
 * a live caller's, mid-step, and is left alone, and so is one that cannot be read or removed: a
 * later caller removes it, and a failure here, whose text repeats the path, stops no caller.
 */
export async function removeUntouched(dir: string, left: (name: string) => boolean): Promise<void> {
  const names = await readdir(dir).catch(() => [] as string[]);
  for (const name of names.filter(left)) {
    const path = join(dir, name);
    const info = await stat(path).catch(() => null);
    if (info !== null && Date.now() - info.mtimeMs > DEVICE_LOGIN_STALE_LOCK_MS) {
      await rm(path, { force: true }).catch(() => {});
    }
  }
}

/**
 * Removes what a caller that died mid take-over or mid release left beside the login at `path`: a
 * lock or a take-over lock it moved aside (`<path>.lock.stale-<uuid>`,
 * `<path>.lock.steal.stale-<uuid>`) and never removed or linked back ({@link removeUntouched}). A
 * move aside keeps the file's modification time, so a lock moved aside by a live caller can read
 * as old too; removing it only makes that caller find nothing to link back and try again, or, as
 * it lets go of its own, nothing left to remove. A caller holds the lock.
 */
export async function removeLeftLocks(path: string): Promise<void> {
  const name = basename(path);
  await removeUntouched(
    dirname(path),
    (one) =>
      namedWithUuid(one, `${name}.lock.stale-`) || namedWithUuid(one, `${name}.lock.steal.stale-`)
  );
}
