import { beforeEach, setDefaultTimeout } from "bun:test";
import { readFileSync } from "node:fs";

/**
 * The waits a row of this directory makes from TypeScript on what the kernel does: a process to run
 * a program, to exec, to end. No fake clock drives the kernel, and a loaded host took more than 10 s
 * to deliver a zombie (2026-11-20, main d36289cb84, 8 busy loops on two CPUs), so a wait ends on the
 * event it waits for, never on a count of polls, and its one bound is the row's own. Bun ends a row
 * at its bound and names no wait, so each wait gives up rowWaitMarginMs before that bound, naming
 * what it waited for and what it saw instead. A row's shell takes the same end for lib/rig.sh's
 * until_true and its own polls (until_true_deadline, set from RowWaits.secondsLeft() as it starts).
 */
export const rowWaitMarginMs = 20_000;

/**
 * The bound of a row of this directory that names no other, and the one package.json's
 * test:e2e-lib gives every row: about twice the slowest row of the directory measured with 8 busy
 * loops on the two CPUs it ran on (98.5 s, 2026-11-20).
 */
export const libRowBoundMs = 200_000;

export interface RowWaits {
  /** The end of the running row's waits, in performance.now() time: the row's start, plus the
   * row's bound, less rowWaitMarginMs. */
  end(): number;
  /** The whole seconds from now to end(), never less than 0: what a shell the row starts now sets
   * until_true_deadline to, from its own $SECONDS, so every shell of a row shares the row's end. */
  secondsLeft(): number;
}

/**
 * rowWaits(rowBoundMs) makes ROWBOUNDMS the bound of every row of the calling file
 * (setDefaultTimeout), whatever the runner's command line, and marks each row's start (beforeEach),
 * so that every wait of a row ends rowWaitMarginMs before the row's bound. A row of the file that
 * names a bound of its own must name this one.
 */
export function rowWaits(rowBoundMs: number): RowWaits {
  setDefaultTimeout(rowBoundMs);
  let start: number | undefined;
  beforeEach(() => {
    start = performance.now();
  });
  const end = () => {
    if (start === undefined) {
      throw new Error("rowWaits: no row has started, so its beforeEach has not run");
    }
    return start + rowBoundMs - rowWaitMarginMs;
  };
  return {
    end,
    secondsLeft: () => Math.max(0, Math.floor((end() - performance.now()) / 1000)),
  };
}

/**
 * untilTrue(what, end, look) polls LOOK every 50 ms until it answers true, and at END (RowWaits.end)
 * throws, naming WHAT and LOOK's last answer, what it saw instead. It looks once more at END itself,
 * so a condition that holds then, or an END that had passed before the wait began, still succeeds
 * rather than failing unread, as until_true does. A LOOK that throws ends the wait with its error.
 */
export async function untilTrue(
  what: string,
  end: number,
  look: () => true | string | Promise<true | string>
): Promise<void> {
  const began = performance.now();
  for (;;) {
    const seen = await look();
    if (seen === true) return;
    const now = performance.now();
    if (now >= end) {
      const waited = ((now - began) / 1000).toFixed(1);
      throw new Error(`waited ${waited} s, to the end of the row's waits, for ${what}: ${seen}`);
    }
    await Bun.sleep(Math.min(50, end - now));
  }
}

/** stateOf(pid) is PID's state letter (/proc/<pid>/stat field 3, read past the last `) `), or
 * "gone" once no process holds PID. */
export function stateOf(pid: number): string {
  try {
    return readFileSync(`/proc/${pid}/stat`, "utf8")
      .replace(/^[\s\S]*\) /, "")
      .slice(0, 1);
  } catch {
    return "gone";
  }
}

/**
 * untilGone(pid, what, end) resolves once PID, the process WHAT names, has ended, a zombie counting
 * as ended, and otherwise fails at END naming both and the state PID was last in. A SIGKILL's
 * delivery and the end of the process it hits are the kernel's, so it polls the process itself. A
 * PID that is no pid at all fails at once: no process holds it, and it would read as gone.
 */
export async function untilGone(pid: number, what: string, end: number): Promise<void> {
  if (!Number.isSafeInteger(pid) || pid <= 0) throw new Error(`${what}: ${pid} is no pid`);
  await untilTrue(`pid ${pid}, ${what}, to end`, end, () => {
    const state = stateOf(pid);
    return state === "gone" || state === "Z" || state === "X" ? true : `its state is ${state}`;
  });
}
