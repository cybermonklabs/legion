// The removal of a test's scratch directories after each case, which waits for the removal itself.
// A removal is the disk's work: on a loaded machine with a full disk it passed every bound a hook
// stated for it (bun's 5 s, then 30 s), so a bound on it failed tests whose behaviour held. The hook
// here has no bound of its own. A removal that never ends is ended by the run's own deadline (the
// CI job's timeout, or whatever runs `bun test`), and by then the run's output names it.
import { afterEach } from "bun:test";
import { rm } from "node:fs/promises";

/**
 * bun's bound on the hook `removeAfterEachCase` registers: none. bun 1.4.4 gives a hook whose bound
 * is infinite no deadline of its own, under `bun test --timeout` too (a hook that waited 40 s passed
 * under `--timeout 1000`).
 */
const NO_BOUND = Number.POSITIVE_INFINITY;

/** How long a removal runs before `removeScratch` names it in the run's output, in milliseconds.
 *  The note decides nothing; it names the directories a run that its deadline ends was removing. */
export const REMOVAL_NOTE_MS = 10_000;

/**
 * Removes every directory of `dirs`, and waits for the removal itself, with no bound of its own; an
 * error of the removal rejects. Once the removal has run REMOVAL_NOTE_MS, it names the directories
 * on stderr and goes on waiting. `remove` is the removal of one directory: `rm -rf`, unless a test
 * of this module holds it.
 */
export async function removeScratch(
  dirs: readonly string[],
  remove: (dir: string) => Promise<void> = (dir) => rm(dir, { recursive: true, force: true })
): Promise<void> {
  if (dirs.length === 0) return;
  // A real timer: the removal is the filesystem's work, which bun's fake timers cannot advance,
  // and the note only names it.
  const note = setTimeout(() => {
    console.warn(
      `still removing ${dirs.join(" ")} after ${REMOVAL_NOTE_MS} ms; the hook waits for the removal to finish`
    );
  }, REMOVAL_NOTE_MS);
  try {
    await Promise.all(dirs.map(remove));
  } finally {
    clearTimeout(note);
  }
}

/**
 * Registers an afterEach, in the calling `describe` or file, that removes the directories `dirs`
 * holds when each case ends and empties it (`removeScratch`), with no bound on the hook.
 */
export function removeAfterEachCase(dirs: string[]): void {
  afterEach(() => removeScratch(dirs.splice(0)), NO_BOUND);
}
