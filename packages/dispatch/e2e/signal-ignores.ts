// The signals this process was started with ignored, read for e2e/harness-up.ts and
// e2e/harness-pool.ts, each of which arms listeners that stop the stack on a signal.
//
// A listener for a signal undoes an inherited ignore for good: Bun, as Node does, takes the signal
// over while a listener is armed, and puts back the default action, not the ignore, when the last
// one is taken off. So a process started under `nohup`, which ignores SIGHUP, or as a
// non-interactive shell's `&` job, which ignores SIGINT and SIGQUIT, would end on that signal from
// its first such listener on: the one thing it was started to avoid. Those two modules therefore
// arm a stop listener only where `armsStopListener` says so, as the Go commands arm none for a
// SIGHUP that `signal.Ignored` names (packages/daemon/cmd/legion/signal_status.go, hangupCancels).
//
// The ignores are read once, from the `SigIgn` mask of /proc/self/status, when this module is first
// evaluated. A module that imports it is evaluated after it, so the read comes before any listener
// that module arms. Where the mask cannot be read - there is no /proc, as on macOS, or no `SigIgn`
// line in it, as under gVisor - whether a signal was ignored is unknown, and the harness arms what
// it armed before it read the mask at all: SIGINT and SIGTERM, and no SIGHUP or SIGQUIT listener.

import { readFileSync } from "node:fs";
import { constants } from "node:os";

/** The `SigIgn` mask that `status`, the text of a `/proc/<pid>/status`, holds: bit N-1 set for each
 *  signal N the process ignores. Undefined where there is no text or no such line. Exported for
 *  e2e/__tests__/signal-ignores.test.ts. */
export function ignoreMask(status: string | undefined): bigint | undefined {
  const mask = status === undefined ? undefined : /^SigIgn:\s*([0-9a-f]+)\s*$/m.exec(status)?.[1];
  return mask === undefined ? undefined : BigInt(`0x${mask}`);
}

/** The text of /proc/self/status, or undefined where it cannot be read. */
function ownStatus(): string | undefined {
  try {
    return readFileSync("/proc/self/status", "utf8");
  } catch {
    return undefined;
  }
}

const ignored = ignoreMask(ownStatus());

/** Whether this process was started with `signal` ignored, as this module read it at its load:
 *  true or false, or undefined where the mask could not be read. */
export function ignoredAtStart(signal: NodeJS.Signals): boolean | undefined {
  if (ignored === undefined) return undefined;
  const number = constants.signals[signal];
  return number !== undefined && ((ignored >> BigInt(number - 1)) & 1n) === 1n;
}

/** Whether the harness arms a stop listener for `signal`: never where this process was started
 *  with it ignored, always where it was not, and, where that is unknown, for SIGINT and SIGTERM
 *  alone, which the harness armed before it read the mask, and never for SIGHUP or SIGQUIT, which
 *  it did not: a listener for either could undo a `nohup` it cannot see. */
export function armsStopListener(signal: NodeJS.Signals): boolean {
  const ignoredThen = ignoredAtStart(signal);
  if (ignoredThen === undefined) return signal === "SIGINT" || signal === "SIGTERM";
  return !ignoredThen;
}
