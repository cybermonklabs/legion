// The reads a test makes of work a command holds running, for the TypeScript tests that signal a
// command and then require what it started gone: the check of a pid against the stand-in's command
// line (sleepRuns), the wait for a state another process reaches (until), and the read of what a
// process ignores (ignoresSignal). It is the one body of these reads for every such test, as
// packages/envoy/heldprocess is for the Go tests, so a fix to how a test proves a process gone
// reaches each test that proves it. A test's stand-in records its pid and becomes
// `sleep <seconds>` under it, with a number of seconds no other test of this repository uses, held
// far past any bound of the test.
import { readFileSync } from "node:fs";
import { constants } from "node:os";

/** Whether `pid` is still the stand-in: a process whose command line is `sleep <seconds>`, read
 *  from `/proc/<pid>/cmdline`, so a pid the kernel has given to another process since does not
 *  count. A pid `/proc` does not name, 0 among them, answers false, as does a machine without
 *  `/proc`. */
export function sleepRuns(pid: number, seconds: string): boolean {
  try {
    return (
      readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0").filter(Boolean).join(" ") ===
      `sleep ${seconds}`
    );
  } catch {
    return false;
  }
}

/** Whether the process `pid` ignores `signal` now, as the `SigIgn` mask of `/proc/<pid>/status`
 *  says: the kernel's own record, which a listener for the signal clears. A test reads it to see
 *  that a command left in place an ignore it was started with. A pid `/proc` does not name answers
 *  false. */
export function ignoresSignal(pid: number, signal: NodeJS.Signals): boolean {
  try {
    const mask = /^SigIgn:\s*([0-9a-f]+)\s*$/m.exec(
      readFileSync(`/proc/${pid}/status`, "utf8")
    )?.[1];
    const number = constants.signals[signal];
    return (
      mask !== undefined &&
      number !== undefined &&
      ((BigInt(`0x${mask}`) >> BigInt(number - 1)) & 1n) === 1n
    );
  } catch {
    return false;
  }
}

/** Reads `holds` every 50 ms until it answers true, and answers true; answers false once `ms`
 *  have passed with it still false. It is for a state another process reaches, which raises no
 *  event a test can await. */
export async function until(holds: () => boolean, ms: number): Promise<boolean> {
  for (const deadline = performance.now() + ms; !holds(); await Bun.sleep(50)) {
    if (performance.now() > deadline) return false;
  }
  return true;
}
