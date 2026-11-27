/**
 * One sweep of the stall watchdog run on Bun's fake clock, for the rows of
 * `src/stall-watchdog.test.ts` whose sweep waits on the abort's bounded wait (`abortEnd`), a host
 * timer the watchdog arms itself apart from the clock it judges by.
 */
import { jest } from "bun:test";
import { turn } from "@legion/test-fixtures/fake-clock";
import type { StallWatchdog } from "../src/stall-watchdog";

/** How many turns of the event loop {@link drain} lets pass. The sweeps of
 *  `src/stall-watchdog.test.ts` need one: inside one turn every promise chain queued so far
 *  settles. Measured: at 1 every row of that file passed in three runs, and at 0 the row whose
 *  sweep waits on no abort read that sweep as still held. 25 is headroom over that one, for a
 *  sweep step that comes to wait on a turn of the loop of its own (a `setImmediate`), and no sweep
 *  here needs that many. */
const DRAIN_TURNS = 25;

/** Lets every promise chain queued so far run. A sweep reads no clock between its awaits and
 *  every dependency the rig gives it answers in this process, so once the event loop has turned
 *  the sweep has done all it does before a timer; Bun's fake clock leaves `setImmediate` real. */
async function drain(): Promise<void> {
  for (let turns = 0; turns < DRAIN_TURNS; turns += 1) await turn();
}

/** What {@link sweepOnFakeClock} hands its row while the sweep runs. */
export interface HeldSweep {
  /** Whether the sweep has handed back yet. */
  readonly handedBack: () => boolean;
  /** Moves Bun's fake clock on by `ms`, then lets the sweep run on. */
  readonly advance: (ms: number) => Promise<void>;
}

/** Runs one sweep with Bun's fake clock holding the host timer of the abort's bounded wait
 *  (`abortEnd`), and runs `look` once the sweep has done all it does before that timer. The real
 *  clock comes back however `look` ends. */
export async function sweepOnFakeClock(
  watchdog: StallWatchdog,
  look: (sweep: HeldSweep) => Promise<void> | void
): Promise<void> {
  jest.useFakeTimers();
  try {
    let handedBack = false;
    const sweeping = watchdog.sweep().then(() => {
      handedBack = true;
    });
    await drain();
    await look({
      handedBack: () => handedBack,
      advance: async (ms) => {
        jest.advanceTimersByTime(ms);
        await drain();
      },
    });
    await sweeping;
  } finally {
    jest.useRealTimers();
  }
}
