// packages/cipher-client/src/__tests__/device-login-lock.test.ts
//
// The device login's lock (`device-login-lock.ts`), through `withDeviceLoginLock`: a live holder
// is waited for, a stale lock is taken by one caller alone, each removal is of the very file the
// caller looked at, a holder that lets go removes only its own lock, and a holder can tell it
// still holds its lock. No row sends anything to a broker. Each row holds a caller at one step of
// the protocol (`at`, `sleep`, or the read of its look, {@link holdTheRead}), or runs the holder's
// timer callback by hand, so no row is decided by a clock.

import { expect, spyOn, test } from "bun:test";
import * as fs from "node:fs/promises";
import { readdir, readFile, stat, utimes, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { writeDeviceLoginFile } from "../../test/device-logins";
import { withDeviceLoginLock } from "../device-login";
import { DEVICE_LOGIN_STALE_LOCK_MS, LOCK_HEARTBEAT_MS } from "../device-login-lock";
import { TRADE_ATTEMPT_MS } from "../token-client";
import { LockHeldError } from "../token-errors";

const makeDirectory = temporaryDirectories("device-login-lock");

/** A device login file's path in a directory of this test's own. */
function loginPath(): string {
  return join(makeDirectory(), "legion", "device-login.json");
}

/** A modification time past the lock's stale bound: a holder that is gone. */
function staleTime(): Date {
  return new Date(Date.now() - DEVICE_LOGIN_STALE_LOCK_MS - 1_000);
}

/**
 * Holds the first read of a file, once `arm` is called, that `match` takes by its path and what
 * it read, until `go` settles: the caller has read the file for a look, and does what it does
 * with that look only after. Any read of the file holds, by whatever name the caller reads it,
 * so the hold is between a look and what follows it however the protocol takes its look.
 * `read` answers what the held read read; `restore` ends the hold for every later row.
 */
function holdTheRead(match: (path: string, read: string) => boolean, go: Promise<void>) {
  const reached = Promise.withResolvers<string>();
  const real = fs.readFile;
  let armed = false;
  let held = false;
  const spy = spyOn(fs, "readFile").mockImplementation((async (
    ...args: Parameters<typeof fs.readFile>
  ) => {
    const read = await real(...args);
    if (armed && !held && typeof read === "string" && match(String(args[0]), read)) {
      held = true;
      reached.resolve(read);
      await go;
    }
    return read;
  }) as typeof fs.readFile);
  return {
    read: reached.promise,
    arm: () => {
      armed = true;
    },
    restore: () => spy.mockRestore(),
  };
}

test("a dead holder's lock costs a trade at most half its attempt", () => {
  // A trade waits out a lock its holder touched just before it died for the whole stale bound;
  // the refresh and the exchange must still fit in what is left of the trade's one attempt.
  expect(DEVICE_LOGIN_STALE_LOCK_MS * 2).toBeLessThanOrEqual(TRADE_ATTEMPT_MS);
});

test("the holder's beat is a timer every LOCK_HEARTBEAT_MS that touches its lock", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  // The real beat: the timer the lock sets is read, and its callback run here instead of after
  // LOCK_HEARTBEAT_MS.
  const timers = spyOn(globalThis, "setInterval");
  try {
    await withDeviceLoginLock(path, async () => {
      expect(timers.mock.calls).toHaveLength(1);
      const [beat, every] = timers.mock.calls[0] as unknown as [() => Promise<void>, number];
      expect(every).toBe(LOCK_HEARTBEAT_MS);
      const old = staleTime();
      await utimes(lock, old, old);
      await beat();
      expect((await stat(lock)).mtimeMs).toBeGreaterThan(old.getTime() + 1_000);
    });
  } finally {
    timers.mockRestore();
  }
});

test("the login's lock waits for a live holder and takes a stale one", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;

  // A live holder: the work runs only once the holder lets go, which it does at the third wait.
  await writeFile(lock, "another process");
  let waits = 0;
  let ranAfter = -1;
  await withDeviceLoginLock(
    path,
    async () => {
      ranAfter = waits;
    },
    {
      sleep: async () => {
        waits += 1;
        if (waits === 3) await Bun.file(lock).delete();
      },
    }
  );
  expect(ranAfter).toBe(3);
  expect(await stat(lock).catch(() => null)).toBeNull();

  // A holder gone for longer than the stale bound: its lock is taken with no wait at all.
  await writeFile(lock, "a process that died holding it");
  const old = staleTime();
  await utimes(lock, old, old);
  let staleWaits = 0;
  await withDeviceLoginLock(path, async () => {}, {
    sleep: async () => {
      staleWaits += 1;
    },
  });
  expect(staleWaits).toBe(0);

  // A caller whose signal ends stops waiting, and never runs the work.
  await writeFile(lock, "another process");
  const caller = new AbortController();
  let ran = false;
  const stopped = await withDeviceLoginLock(
    path,
    async () => {
      ran = true;
    },
    {
      signal: caller.signal,
      sleep: async () => {
        caller.abort(new Error("the caller's deadline"));
      },
    }
  ).then(
    () => null,
    (error: unknown) => error as Error
  );
  // The trade that waited names the lock, not the broker, and carries the caller's reason.
  expect(stopped).toBeInstanceOf(LockHeldError);
  expect(stopped?.message).toContain("lock beside");
  expect((stopped?.cause as Error | undefined)?.message).toBe("the caller's deadline");
  expect(ran).toBe(false);
  // The holder's lock is the holder's: a caller that gave up leaves it where it was.
  expect(await readFile(lock, "utf8")).toBe("another process");
  await Bun.file(lock).delete();
});

test("the lock tells its holder whether it still holds it, and its holder keeps it fresh", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  // The holder's timer, read here, and its beat run by hand instead of every LOCK_HEARTBEAT_MS.
  const timers = spyOn(globalThis, "setInterval");
  const cleared = spyOn(globalThis, "clearInterval");
  try {
    await withDeviceLoginLock(path, async (held) => {
      expect(await held()).toBe(true);
      // A lock its holder last touched a while ago is touched again at its next beat, so a
      // holder that is slow is never taken for dead.
      const old = new Date(Date.now() - 12_000);
      await utimes(lock, old, old);
      const [beat] = timers.mock.calls[0] as unknown as [() => Promise<void>];
      await beat();
      expect(Date.now() - (await stat(lock)).mtimeMs).toBeLessThan(LOCK_HEARTBEAT_MS);
      // Another caller took it over.
      await writeFile(lock, "another process that took the lock over");
      expect(await held()).toBe(false);
    });
    // The holder stopped its timer when its work settled.
    const timer = timers.mock.results[0]?.value as ReturnType<typeof setInterval>;
    expect(cleared.mock.calls.map(([stopped]) => stopped)).toContain(timer);
  } finally {
    timers.mockRestore();
    cleared.mockRestore();
  }
  // The lock is the new holder's, and stays where it is.
  expect(await readFile(lock, "utf8")).toBe("another process that took the lock over");
  await Bun.file(lock).delete();
});

test("a take-over waits while another caller takes over, and not for one that died doing so", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  const steal = `${lock}.steal`;
  const old = staleTime();
  await writeFile(lock, "a process that died holding it");
  await utimes(lock, old, old);

  // Another caller is taking the stale lock over: this one waits, and runs once that caller's
  // take-over lock is gone, at its third wait.
  await writeFile(steal, "another caller taking over");
  let waits = 0;
  let ranAfter = -1;
  await withDeviceLoginLock(
    path,
    async () => {
      ranAfter = waits;
    },
    {
      sleep: async () => {
        waits += 1;
        if (waits === 3) await Bun.file(steal).delete();
      },
    }
  );
  expect(ranAfter).toBe(3);

  // A take-over lock 6 s old is one whose caller died inside its take-over: it is removed, and
  // the stale lock taken, after one wait.
  await writeFile(lock, "a process that died holding it");
  await utimes(lock, old, old);
  await writeFile(steal, "a caller that died taking over");
  const stealOld = new Date(Date.now() - 6_000);
  await utimes(steal, stealOld, stealOld);
  let deadWaits = 0;
  await withDeviceLoginLock(path, async () => {}, {
    sleep: async () => {
      deadWaits += 1;
    },
  });
  expect(deadWaits).toBe(1);
  expect(await stat(steal).catch(() => null)).toBeNull();
});

// A file another caller makes in place of one this caller found stale can carry the very inode
// number of the one it replaced: XFS hands a new file the number freed a moment before (5 of 5
// tries on a review box). These rows write the other caller's file into the stale file's own
// inode, which is exactly what such a filesystem gives the check to see, so they hold on every
// filesystem: the same device and inode, and a fresh modification time and holder value.

test("of two callers inside one take-over, the one that comes second puts the lock back", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  await writeFile(lock, "a process that died holding it");
  const old = staleTime();
  await utimes(lock, old, old);

  // A is under its own take-over lock, has found the lock still stale, and is held before it
  // moves the lock aside: it stopped there for over 5 s, so B took A's take-over lock as dead,
  // took the stale lock over and now holds a fresh one. A then goes on. It must find the lock it
  // moves is B's, put it back and wait for B, never take it from B.
  const aTaking = Promise.withResolvers<void>();
  const aGoes = Promise.withResolvers<void>();
  let bHolds = false;
  let heldAtFirstWait = "";
  let ranWhileBHeld: boolean | undefined;
  const a = withDeviceLoginLock(
    path,
    async () => {
      ranWhileBHeld = bHolds;
    },
    {
      at: async (step) => {
        if (step !== "taking") return;
        aTaking.resolve();
        await aGoes.promise;
      },
      sleep: async () => {
        if (heldAtFirstWait === "") {
          heldAtFirstWait = await readFile(lock, "utf8").catch(() => "(no lock)");
        }
        // B's work ends, and B lets go.
        bHolds = false;
        await Bun.file(lock).delete();
      },
    }
  );
  await aTaking.promise;
  await writeFile(lock, "B holds it");
  bHolds = true;
  aGoes.resolve();
  await a;
  expect(heldAtFirstWait).toBe("B holds it");
  expect(ranWhileBHeld).toBe(false);
  expect(await stat(lock).catch(() => null)).toBeNull();
});

test("a dead take-over lock is removed only while it is still the dead one", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  const steal = `${lock}.steal`;
  const old = staleTime();
  await writeFile(lock, "a process that died holding it");
  await utimes(lock, old, old);
  await writeFile(steal, "a caller that died taking over");
  const stealOld = new Date(Date.now() - 6_000);
  await utimes(steal, stealOld, stealOld);

  // B finds the take-over lock dead and is held before it removes it, while C removes that dead
  // one and starts a take-over of its own. B then goes on: it must leave C's take-over lock in
  // place, and wait for C.
  const bFound = Promise.withResolvers<void>();
  const bGoes = Promise.withResolvers<void>();
  let stealAtFirstWait = "";
  const b = withDeviceLoginLock(path, async () => {}, {
    at: async (step) => {
      if (step !== "steal-stale" || stealAtFirstWait !== "") return;
      bFound.resolve();
      await bGoes.promise;
    },
    sleep: async () => {
      if (stealAtFirstWait === "") {
        stealAtFirstWait = await readFile(steal, "utf8").catch(() => "(no take-over lock)");
      }
      // C's take-over ends.
      await Bun.file(steal).delete();
    },
  });
  await bFound.promise;
  await writeFile(steal, "C takes over");
  bGoes.resolve();
  await b;
  expect(stealAtFirstWait).toBe("C takes over");
  expect(await stat(steal).catch(() => null)).toBeNull();
});

test("a take-over that finds the holder back just before it moves the lock puts the lock back", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  await writeFile(lock, "a process that stopped holding it");
  const old = staleTime();
  await utimes(lock, old, old);

  // A has found the lock still stale under its take-over lock, and is held just before it moves
  // the lock aside. The holder comes back then and touches its lock: the same file, holding the
  // same value, touched a moment ago. A goes on: it must put the holder's lock back and wait for
  // it, never take it from a holder that is back.
  const aTaking = Promise.withResolvers<void>();
  const aGoes = Promise.withResolvers<void>();
  let takingOnce = false;
  let lockAtFirstWait = "";
  let ran = false;
  const a = withDeviceLoginLock(
    path,
    async () => {
      ran = true;
    },
    {
      at: async (step) => {
        if (step !== "taking" || takingOnce) return;
        takingOnce = true;
        aTaking.resolve();
        await aGoes.promise;
      },
      sleep: async () => {
        if (lockAtFirstWait === "") {
          lockAtFirstWait = await readFile(lock, "utf8").catch(() => "(no lock)");
        }
        // The holder's work ends, and it lets go.
        await Bun.file(lock).delete();
      },
    }
  );
  await Promise.race([aTaking.promise, a]);
  const now = new Date();
  await utimes(lock, now, now).catch(() => {});
  aGoes.resolve();
  await a;
  expect(lockAtFirstWait).toBe("a process that stopped holding it");
  expect(ran).toBe(true);
  expect(await readdir(dirname(path))).toEqual(["device-login.json"]);
});

// A holder lets go of its lock, and a caller of its take-over lock, in two steps: a look that
// reads the file and finds its own value there, and then what it does with it. It can be stopped
// for longer than the stale bound between the two; its timer is stopped by then, so another caller
// takes the lock over as stale and starts work under it. A removal by name in the second step
// would remove that caller's lock, and a third caller could then take the lock beside it: two
// holders, two refreshes of one refresh token. These rows hold the caller just after the read of
// its look ({@link holdTheRead}).

test("a holder that lets go leaves the lock another caller took from it while it was stopped", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  let aValue = "";
  const aGoes = Promise.withResolvers<void>();
  const look = holdTheRead((_, read) => read === aValue, aGoes.promise);
  const bWorking = Promise.withResolvers<() => Promise<boolean>>();
  const bEnds = Promise.withResolvers<void>();
  let b: Promise<void> | undefined;
  try {
    // A's work ends, and A lets go: it is held just after it read its own value back.
    const a = withDeviceLoginLock(path, async () => {
      aValue = await readFile(lock, "utf8");
      look.arm();
    });
    await look.read;
    // A was stopped there for over 15 s, so B finds A's lock stale, takes it over and works.
    const old = staleTime();
    await utimes(lock, old, old);
    b = withDeviceLoginLock(path, async (held) => {
      bWorking.resolve(held);
      await bEnds.promise;
    });
    const bHeld = await bWorking.promise;
    const bValue = await readFile(lock, "utf8");
    expect(bValue).not.toBe(aValue);
    expect(await bHeld()).toBe(true);
    // A goes on: B's lock is where it was, and B still holds it.
    aGoes.resolve();
    await a;
    expect(await readFile(lock, "utf8").catch(() => "(no lock)")).toBe(bValue);
    expect(await bHeld()).toBe(true);
  } finally {
    aGoes.resolve();
    bEnds.resolve();
    await b;
    look.restore();
  }
  // B let go too, and nothing either caller made is left beside the login.
  expect(await readdir(dirname(path))).toEqual(["device-login.json"]);
});

test("a caller that lets go of its take-over lock leaves the one another caller took from it", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  const steal = `${lock}.steal`;
  const old = staleTime();
  await writeFile(lock, "a process that stopped holding it");
  await utimes(lock, old, old);

  // A finds the lock stale and is held before it takes its take-over lock, while the lock's
  // holder comes back and touches it. A goes on: under its take-over lock it finds the lock live,
  // and lets go of the take-over lock. It is held just after it read that lock's value back.
  const aFound = Promise.withResolvers<void>();
  const aGoesOn = Promise.withResolvers<void>();
  const aLetsGo = Promise.withResolvers<void>();
  const aWaits = Promise.withResolvers<void>();
  const dDone = Promise.withResolvers<void>();
  const dTaking = Promise.withResolvers<void>();
  const dGoes = Promise.withResolvers<void>();
  const look = holdTheRead((file) => file.startsWith(steal), aLetsGo.promise);
  let aFoundOnce = false;
  let d: Promise<void> | undefined;
  let a: Promise<void> | undefined;
  try {
    a = withDeviceLoginLock(path, async () => {}, {
      at: async (step) => {
        if (step !== "lock-stale" || aFoundOnce) return;
        aFoundOnce = true;
        aFound.resolve();
        await aGoesOn.promise;
      },
      sleep: async () => {
        aWaits.resolve();
        await dDone.promise;
      },
    });
    await aFound.promise;
    const now = new Date();
    await utimes(lock, now, now);
    look.arm();
    aGoesOn.resolve();
    const aSteal = await look.read;

    // A was stopped there for over 5 s, and the holder has died for good: D finds the lock stale
    // and A's take-over lock dead, removes that, and is under a take-over lock of its own.
    const stealOld = new Date(Date.now() - 6_000);
    await utimes(steal, stealOld, stealOld);
    await utimes(lock, old, old);
    let dTakingOnce = false;
    d = withDeviceLoginLock(path, async () => {}, {
      at: async (step) => {
        if (step !== "taking" || dTakingOnce) return;
        dTakingOnce = true;
        dTaking.resolve();
        await dGoes.promise;
      },
      sleep: async () => {},
    });
    await Promise.race([dTaking.promise, d]);
    const dSteal = await readFile(steal, "utf8").catch(() => "(no take-over lock)");
    expect(dSteal).not.toBe(aSteal);
    expect(dSteal).not.toBe("(no take-over lock)");

    // A goes on: D's take-over lock is where it was, so no third caller can take over beside D.
    aLetsGo.resolve();
    await aWaits.promise;
    expect(await readFile(steal, "utf8").catch(() => "(no take-over lock)")).toBe(dSteal);

    dGoes.resolve();
    await d;
  } finally {
    aGoesOn.resolve();
    aLetsGo.resolve();
    dGoes.resolve();
    dDone.resolve();
    await d;
    await a;
    look.restore();
  }
  // D took the dead holder's lock and let go, then A did: nothing is left beside the login.
  expect(await readdir(dirname(path))).toEqual(["device-login.json"]);
});
