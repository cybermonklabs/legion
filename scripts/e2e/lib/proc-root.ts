import { existsSync, lstatSync, mkdtempSync, symlinkSync } from "node:fs";
import { join } from "node:path";

// A proc root of a test's own processes, for code under test that reads `${proc_root:-/proc}`
// (procs_naming and so the run's sweep, listening_holders, refuse_leftovers, and
// parked-decision-devbox.sh's stop_agent_daemons): a directory holding a link to the real
// /proc/<pid> of each process the test names, so that code reads those processes' own files and
// signals their real pids, and its work is the test's processes rather than the host's. A harness
// shell makes the same root for itself with one `ln -s` an entry.

/** A fresh proc root under `dir`: a link to /proc/<pid> for each of `pids` still running. */
export function procRootOf(dir: string, pids: readonly number[]): string {
  return linkProcesses(mkdtempSync(join(dir, "proc-")), pids);
}

/**
 * Gives the proc root `root` a link to /proc/<pid> for each of `pids` still running that it holds
 * no entry for yet, and answers `root`: how a row gives its own processes to a root a hook made
 * ahead of it, as one holding the force's entries (lib/arg-max-force.ts).
 */
export function linkProcesses(root: string, pids: readonly number[]): string {
  for (const pid of new Set(pids)) {
    const link = join(root, String(pid));
    if (existsSync(`/proc/${pid}`) && lstatSync(link, { throwIfNoEntry: false }) === undefined) {
      symlinkSync(`/proc/${pid}`, link);
    }
  }
  return root;
}
