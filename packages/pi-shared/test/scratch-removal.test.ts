import { afterEach, describe, expect, jest, spyOn, test } from "bun:test";
import { mkdir, mkdtemp, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { REMOVAL_NOTE_MS, removeAfterEachCase, removeScratch } from "./scratch-removal";

/** Whether `target` is gone from the filesystem. */
async function gone(target: string): Promise<boolean> {
  return await stat(target).then(
    () => false,
    (error: NodeJS.ErrnoException) => {
      if (error.code === "ENOENT") return true;
      throw error;
    }
  );
}

/** One macrotask turn, on a `setImmediate` that Bun's fake clock leaves real: it comes only once
 *  every promise already chained has run as far as it goes. */
function nextTurn(): Promise<void> {
  const turn = Promise.withResolvers<void>();
  setImmediate(turn.resolve);
  return turn.promise;
}

describe("removeScratch", () => {
  const dirs: string[] = [];
  removeAfterEachCase(dirs);
  // A fake clock left behind stops the timers of every case the same worker runs after this one.
  afterEach(() => {
    jest.useRealTimers();
  });

  test("removes every directory it is given, with what they hold", async () => {
    const root = await mkdtemp(path.join(tmpdir(), "scratch-removal-"));
    dirs.push(root);
    const given = [path.join(root, "a"), path.join(root, "b")];
    for (const dir of given) {
      await mkdir(path.join(dir, "nested"), { recursive: true });
      await Bun.write(path.join(dir, "nested", "file"), "held\n");
    }
    await removeScratch(given);
    expect(await Promise.all(given.map(gone))).toEqual([true, true]);
  });

  // The fact the hook waits for is the removal, on the fake clock here, so the case holds no wait
  // of its own: a removal held past the note is named once and still awaited until it ends.
  test("names the directories once a removal has run past its note, and still waits for it", async () => {
    jest.useFakeTimers();
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    try {
      const held = Promise.withResolvers<void>();
      const named = ["/scratch/a", "/scratch/b"];
      let settled = false;
      const removal = removeScratch(named, () => held.promise).finally(() => {
        settled = true;
      });
      jest.advanceTimersByTime(REMOVAL_NOTE_MS - 1);
      expect(warn).not.toHaveBeenCalled();
      jest.advanceTimersByTime(1);
      expect(warn).toHaveBeenCalledTimes(1);
      expect(warn.mock.calls[0]?.[0]).toBe(
        `still removing /scratch/a /scratch/b after ${REMOVAL_NOTE_MS} ms; the hook waits for the removal to finish`
      );
      await nextTurn();
      expect(settled).toBe(false);
      held.resolve();
      await removal;
      expect(warn).toHaveBeenCalledTimes(1);
      expect(jest.getTimerCount()).toBe(0);
    } finally {
      warn.mockRestore();
    }
  });

  test("a removal that ends before its note names nothing and leaves no timer", async () => {
    jest.useFakeTimers();
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    try {
      await removeScratch(["/scratch/a"], async () => {});
      expect(jest.getTimerCount()).toBe(0);
      jest.advanceTimersByTime(REMOVAL_NOTE_MS);
      expect(warn).not.toHaveBeenCalled();
    } finally {
      warn.mockRestore();
    }
  });

  test("an error of the removal rejects with that error and leaves no timer", async () => {
    jest.useFakeTimers();
    const failure = new Error("EBUSY: resource busy or locked, rmdir '/scratch/a'");
    await expect(
      removeScratch(["/scratch/a"], async () => {
        throw failure;
      })
    ).rejects.toBe(failure);
    expect(jest.getTimerCount()).toBe(0);
  });
});

// Cases run in order: the second reads what the hook did after the first.
describe("removeAfterEachCase", () => {
  const dirs: string[] = [];
  removeAfterEachCase(dirs);
  let made: string | undefined;

  test("leaves a case's directories in place while the case runs", async () => {
    made = await mkdtemp(path.join(tmpdir(), "scratch-removal-case-"));
    dirs.push(made);
    await Bun.write(path.join(made, "file"), "held\n");
    expect(await gone(made)).toBe(false);
  });

  test("has removed them, and emptied its list, before the next case starts", async () => {
    if (made === undefined) throw new Error("the case before made no directory");
    expect(await gone(made)).toBe(true);
    expect(dirs).toEqual([]);
  });
});
