// packages/cipher-client/src/__tests__/device-login-lock.test.ts
//
// The device login's lock (`device-login-lock.ts`), through `withDeviceLoginLock`: a live holder
// is waited for, a stale lock is taken by one caller alone, each removal is of the very file the
// caller looked at, and a holder can tell it still holds its lock. No row sends anything to a
// broker. Each row holds a caller at one step of the protocol (`at`, `sleep`, `beats`), so no row
// is decided by a clock.

import { expect, test } from "bun:test";
import { readFile, stat, utimes, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { writeDeviceLoginFile } from "../../test/device-logins";
import { withDeviceLoginLock } from "../device-login";
import { DEVICE_LOGIN_STALE_LOCK_MS, LOCK_HEARTBEAT_MS } from "../device-login-lock";
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
  // The holder's beats, driven here one by one instead of every LOCK_HEARTBEAT_MS.
  let beat: (() => Promise<void>) | undefined;
  let stopped = false;
  await withDeviceLoginLock(
    path,
    async (held) => {
      expect(await held()).toBe(true);
      // A lock its holder last touched a while ago is touched again at its next beat, so a
      // holder that is slow is never taken for dead.
      const old = new Date(Date.now() - 12_000);
      await utimes(lock, old, old);
      await beat?.();
      expect(Date.now() - (await stat(lock)).mtimeMs).toBeLessThan(LOCK_HEARTBEAT_MS);
      // Another caller took it over.
      await writeFile(lock, "another process that took the lock over");
      expect(await held()).toBe(false);
    },
    {
      beats: (touch) => {
        beat = touch;
        return () => {
          stopped = true;
        };
      },
    }
  );
  expect(beat).toBeDefined();
  expect(stopped).toBe(true);
  // The lock is the new holder's, and stays where it is.
  expect(await readFile(lock, "utf8")).toBe("another process that took the lock over");
  await Bun.file(lock).delete();
});

test("of two callers that find one stale lock, exactly one takes it", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  await writeFile(lock, "a process that died holding it");
  const old = staleTime();
  await utimes(lock, old, old);

  // B finds the lock stale and is held there, between its look and its take-over, while A finds
  // it stale too, takes it and starts its work. Then B goes on. B must find A's lock live and
  // wait for it, never take it from A: two holders would spend one refresh token twice.
  const events: string[] = [];
  let holders = 0;
  let most = 0;
  const work = (name: string, until: Promise<void>) => async () => {
    holders += 1;
    most = Math.max(most, holders);
    events.push(`${name} starts`);
    await until;
    events.push(`${name} ends`);
    holders -= 1;
  };
  const bFound = Promise.withResolvers<void>();
  const bGoes = Promise.withResolvers<void>();
  const aWorking = Promise.withResolvers<void>();
  const aEnds = Promise.withResolvers<void>();
  // B has decided once it either waits for A's lock or runs its own work.
  const bDecided = Promise.withResolvers<void>();
  const b = withDeviceLoginLock(
    path,
    async () => {
      bDecided.resolve();
      await work("B", Promise.resolve())();
    },
    {
      at: async (step) => {
        if (step !== "lock-stale") return;
        bFound.resolve();
        await bGoes.promise;
      },
      sleep: async () => {
        bDecided.resolve();
        await aEnds.promise;
      },
    }
  );
  await bFound.promise;
  const a = withDeviceLoginLock(path, async () => {
    aWorking.resolve();
    await work("A", aEnds.promise)();
  });
  await aWorking.promise;
  bGoes.resolve();
  await bDecided.promise;
  aEnds.resolve();
  await Promise.all([a, b]);
  expect(most, events.join(", ")).toBe(1);
  expect(events).toEqual(["A starts", "A ends", "B starts", "B ends"]);
  expect(await stat(lock).catch(() => null)).toBeNull();
  expect(await stat(`${lock}.steal`).catch(() => null)).toBeNull();
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
