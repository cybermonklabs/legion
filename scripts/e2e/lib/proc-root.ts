import { existsSync, mkdtempSync, symlinkSync } from "node:fs";
import { join } from "node:path";

// A proc root of a test's own processes, for code under test that reads `${proc_root:-/proc}`
// (procs_naming and so the run's sweep, listening_holders, refuse_leftovers, and
// parked-decision-devbox.sh's stop_agent_daemons): a directory holding a link to the real
// /proc/<pid> of each process the test names, so that code reads those processes' own files and
// signals their real pids, and its work is the test's processes rather than the host's. A harness
// shell makes the same root for itself with one `ln -s` an entry.

/** A fresh proc root under `dir`: a link to /proc/<pid> for each of `pids` still running. */
export function procRootOf(dir: string, pids: readonly number[]): string {
  const root = mkdtempSync(join(dir, "proc-"));
  for (const pid of new Set(pids)) {
    if (existsSync(`/proc/${pid}`)) symlinkSync(`/proc/${pid}`, join(root, String(pid)));
  }
  return root;
}
