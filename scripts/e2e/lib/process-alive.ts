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
    return !read(`/proc/${pid}/stat`)
      .replace(/^.*\) /, "")
      .startsWith("Z");
  };
}

/**
 * Whether `pid` names a running process: one that answers signal 0 and whose state in
 * `/proc/<pid>/stat` is not `Z`. A zombie still answers signal 0, and it is gone for every purpose
 * of a row.
 */
export const alive = aliveBy((path) => readFileSync(path, "utf8"));
