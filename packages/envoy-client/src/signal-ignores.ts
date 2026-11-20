// The signals this process was started with ignored. A listener for a signal undoes an inherited
// ignore for good: Bun, as Node does, takes the signal over while a listener is armed, and puts
// back the default action, not the ignore, when the last one is taken off. So a process started
// under `nohup`, which ignores SIGHUP, or as a non-interactive shell's `&` job, which ignores SIGINT
// and SIGQUIT, would end on that signal from its first such listener on: the one thing it was
// started to avoid. Each listener of this repository's commands that stops work on a signal is
// therefore armed only for a signal `ignoredAtStart` does not name, as the Go commands arm none for
// a SIGHUP that `signal.Ignored` names (packages/daemon/cmd/legion/signal_status.go,
// hangupOrQuitCancels).
//
// The ignores are read once, from the `SigIgn` mask of /proc/self/status, when this module is first
// evaluated. A module that imports it is evaluated after it, so the read comes before any listener
// that module arms. Where there is no /proc, as on macOS, no signal is named, and each listener is
// armed as it was before.

import { readFileSync } from "node:fs";
import { constants } from "node:os";

/** The `SigIgn` mask of this process at the read: bit N-1 set for each signal N it ignores. */
function readIgnored(): bigint {
  try {
    const mask = /^SigIgn:\s*([0-9a-f]+)\s*$/m.exec(readFileSync("/proc/self/status", "utf8"))?.[1];
    return mask === undefined ? 0n : BigInt(`0x${mask}`);
  } catch {
    return 0n;
  }
}

const ignored = readIgnored();

/** Whether this process was started with `signal` ignored, as this module read it at its load. */
export function ignoredAtStart(signal: NodeJS.Signals): boolean {
  const number = constants.signals[signal];
  return number !== undefined && ((ignored >> BigInt(number - 1)) & 1n) === 1n;
}
