import { existsSync, readFileSync } from "node:fs";

// Whether a process is still running, and the wait for it to end, for a process the test did not
// spawn itself (a command's child, a helper a script left running), which has no `exited` to
// await. Nothing announces such a process's end to the test's process, so it is read off the
// system: a pid `kill(pid, 0)` no longer answers, a zombie, which has ended and waits only for
// whatever adopted it to reap it (under load that reap has taken over a second), or a pid a new
// process has taken since, which /proc tells by its start time.

/** Whether this host has /proc, where a zombie and a reused pid can be told from the process. */
const PROC = existsSync("/proc/self/stat");

/** The state (`S`, `R`, `Z`...) and the start time, in clock ticks since boot, of the process with
 *  this pid, from /proc/<pid>/stat; undefined once it is gone. */
function procStat(pid: number): { readonly state: string; readonly start: string } | undefined {
  let stat: string;
  try {
    stat = readFileSync(`/proc/${pid}/stat`, "utf-8");
  } catch {
    return undefined;
  }
  // "pid (comm) state ppid ...": comm can hold spaces and parentheses, so the fields are read after
  // its last ")". The start time is field 22, the 20th after comm.
  const fields = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
  return { state: fields[0] ?? "", start: fields[19] ?? "" };
}

/** Throws unless `pid` names one process: a positive integer. `kill(0, 0)` and `kill(-n, 0)`
 *  name a process group, and a pid read from an empty or garbled file is 0 or NaN, which would
 *  read as a process already ended. */
function oneProcess(pid: number): void {
  if (!Number.isInteger(pid) || pid <= 0) {
    throw new Error(`${pid} is no process id: a process id is a positive integer`);
  }
}

/** Whether the process with this pid, started at `start` where that is known, is running. */
function running(pid: number, start: string | undefined): boolean {
  try {
    process.kill(pid, 0);
  } catch {
    return false;
  }
  // Without /proc (macOS), a zombie reads as running until it is reaped.
  if (!PROC) return true;
  const stat = procStat(pid);
  return stat !== undefined && stat.state !== "Z" && (start === undefined || stat.start === start);
}

/** Whether the process with this pid is running: it exists and, where /proc says so, is not a
 *  zombie. Throws for a pid that names no process. */
export function processRunning(pid: number): boolean {
  oneProcess(pid);
  return running(pid, undefined);
}

/**
 * Resolves once the process with this pid has ended: it is gone, a zombie, or its pid names a
 * process started since this call. It sets no bound of its own: the test's own timeout is the only
 * hang guard, so the wait says the process ended, however long a loaded machine takes to schedule
 * its end and its reap, and a process that never ends fails the test at that timeout. Rejects for a
 * pid that names no process.
 */
export async function processEnded(pid: number): Promise<void> {
  oneProcess(pid);
  const start = PROC ? procStat(pid)?.start : undefined;
  // The pace of the poll, not a bound: nothing announces the end of a process this one did not
  // spawn.
  while (running(pid, start)) await Bun.sleep(20);
}
