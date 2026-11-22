// The read a test makes of what a process it started ignores, for the tests that require a command
// to have left in place an ignore it was started with (`ignoresSignal`), and the skip of those
// tests on a machine where that cannot be read (`skipWithoutIgnoreMasks`).
import { readFileSync } from "node:fs";
import { constants } from "node:os";

/** Whether the process `pid` ignores `signal` now, as the `SigIgn` mask of `/proc/<pid>/status`
 *  says: the kernel's own record, which a listener for the signal clears. A test reads it to see
 *  that a command left in place an ignore it was started with. A pid `/proc` does not name answers
 *  false, as does a machine whose `/proc` holds no such mask (`skipWithoutIgnoreMasks`). */
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

/** Whether the rows of `file` that read a process's ignored signals must skip on this machine,
 *  because its `/proc/self/status` holds no `SigIgn` line, as under gVisor, or there is no `/proc`,
 *  as on macOS; where they must, it prints one line saying so, naming `file`. Called once, at a test
 *  file's module scope, its answer handed to each `test.skipIf` of those rows, as `skipWithout` is
 *  for a missing program (`./host-tools`). Code that reads the mask finds it unknown there, and a
 *  row that reads it can tell nothing. */
export function skipWithoutIgnoreMasks(file: string): boolean {
  let readable = false;
  try {
    readable = /^SigIgn:/m.test(readFileSync("/proc/self/status", "utf8"));
  } catch {
    readable = false;
  }
  if (!readable) {
    console.error(
      `${file}: the rows that read which signals a process ignores are skipped, since this ` +
        "machine's /proc/self/status names none (no SigIgn line, or no /proc)."
    );
  }
  return !readable;
}
