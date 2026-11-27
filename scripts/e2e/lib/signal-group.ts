/**
 * How the TypeScript of scripts/e2e signals a process group: signalGroup. It imports nothing, so
 * lib/app-logins.ts, which the stage proofs run with plain `bun`, and the test rows import it
 * alike. lib/process-tree.sh's group_signalable is the shell's form of the same check.
 *
 * RefusedGroup and signalGroup are the same code as packages/dispatch/e2e/signal-group.ts, the
 * copy the Dispatch package's e2e code imports. There are two because this copy imports nothing,
 * the Dispatch one included, so the stage proofs run lib/app-logins.ts with plain `bun` from
 * scripts/e2e/lib alone. A change to either copy is made to the other.
 */

/**
 * The refusal signalGroup throws. A caller whose catch reads every other error as a group that has
 * already ended (ESRCH) throws this one on, so a refusal is never read as that.
 */
export class RefusedGroup extends Error {}

/**
 * signalGroup(leader, signal) sends signal to every process of the group leader leads:
 * process.kill(-leader, signal). A leader that is not an integer above 1 is refused with a
 * RefusedGroup naming it, and gets no signal. kill reads group 0 as the group of the process that
 * calls it, which under a test runner holds the runner too, and group 1 as -1, every process this
 * one may signal; a negative leader would name the one process of its absolute value. On gVisor the
 * group of a process that had just exited once read as 0. The errors process.kill throws, ESRCH
 * among them, are the caller's.
 */
export function signalGroup(leader: number, signal: NodeJS.Signals | number): void {
  if (!Number.isInteger(leader) || leader <= 1) {
    throw new RefusedGroup(
      `refusing to signal process group '${leader}': a group is an integer above 1, since kill ` +
        "reads group 0 as the group it is called from and group 1 as -1, every process it may signal"
    );
  }
  process.kill(-leader, signal);
}
