import { readFileSync } from "node:fs";

// Whether a process a row started is still running, for a row that waits for it to end or holds
// that it has not.

/** The read of `/proc/<pid>/stat` that `alive` makes; a test passes its own to force the race. */
export type StatRead = (path: string) => string;

/** `alive`, with `read` in place of its read of `/proc/<pid>/stat`. */
export function aliveBy(read: StatRead): (pid: number) => boolean {
  return (pid) => {
    try {
      process.kill(pid, 0);
    } catch {
      return false;
    }
    const stat = statOf(pid, read);
    return stat !== undefined && !stat.replace(/^.*\) /, "").startsWith("Z");
  };
}

/** `pid`'s stat line, or undefined where the process ended before the read. */
function statOf(pid: number, read: StatRead): string | undefined {
  try {
    return read(`/proc/${pid}/stat`);
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ENOENT" || code === "ESRCH") return undefined;
    throw error;
  }
}

/**
 * Whether `pid` names a running process: one that answers signal 0 and whose state in
 * `/proc/<pid>/stat` is not `Z`. A zombie still answers signal 0, and it is gone for every purpose
 * of a row. A process that ends between the signal and the read leaves no stat file, and the read
 * fails with `ENOENT` or `ESRCH`: that process is gone too. Any other failure of the read, such as
 * a `/proc` this process may not read, says nothing about the process, so it throws.
 */
export const alive = aliveBy((path) => readFileSync(path, "utf8"));
