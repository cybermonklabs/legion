/** How the e2e code and its rows signal a process group: `signalGroup`, which `signalUnlessGone`
 *  calls for a row's process or group that may have ended. It imports nothing, so
 *  `e2e/local-instance-support.ts` and `e2e/tools/unit-repeat.ts` both import it without a
 *  cycle.
 *
 *  `RefusedGroup` and `signalGroup` are the same code as `scripts/e2e/lib/signal-group.ts`, the
 *  copy the TypeScript of scripts/e2e imports. There are two because that copy imports nothing
 *  either and stays beside its importers, so the stage proofs run
 *  `scripts/e2e/lib/app-logins.ts`, which imports it, with plain `bun` and nothing of this
 *  package. A change to either copy is made to the other. */

/** The refusal `signalGroup` throws. A caller whose catch reads every other error as a group
 *  that has already ended (ESRCH) throws this one on, so a refusal is never read as that. */
export class RefusedGroup extends Error {}

/** Sends `signal` to every process of the group `leader` leads: `process.kill(-leader, signal)`.
 *  A `leader` that is not an integer above 1 is refused with a RefusedGroup naming it, and gets
 *  no signal. kill reads group 0 as the group of the process that calls it, which under a test
 *  runner holds the runner too, and group 1 as -1, every process this one may signal; a negative
 *  `leader` would name the one process of its absolute value. On gVisor the group of a process
 *  that had just exited once read as 0. The errors `process.kill` throws, ESRCH among them, are
 *  the caller's. */
export function signalGroup(leader: number, signal: NodeJS.Signals | number): void {
  if (!Number.isInteger(leader) || leader <= 1) {
    throw new RefusedGroup(
      `refusing to signal process group '${leader}': a group is an integer above 1, since kill ` +
        "reads group 0 as the group it is called from and group 1 as -1, every process it may signal"
    );
  }
  process.kill(-leader, signal);
}

/** Sends `signal` to the process `target`, or to every process of the group `target.group` leads
 *  (`signalGroup`, which refuses a group that kill would read as no group a row started). A
 *  process that ended before this kill answers ESRCH, which is no failure of the row, so it does
 *  not replace the assertion the row is making; any other error, a refused group among them, is
 *  thrown on. */
export function signalUnlessGone(
  target: number | { readonly group: number },
  signal: NodeJS.Signals
): void {
  try {
    if (typeof target === "number") process.kill(target, signal);
    else signalGroup(target.group, signal);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error;
  }
}
