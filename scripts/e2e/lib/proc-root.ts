import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  symlinkSync,
} from "node:fs";
import { join } from "node:path";

// A proc root of a test's own processes, for code under test that reads `${proc_root:-/proc}`
// (run_processes in lib/rig.sh, stop_agent_daemons in parked-decision-devbox.sh, role_kill_sh in
// role-namespaces.sh): a directory holding a link to the real /proc/<pid> of each process the test
// names, so that code reads those processes' own files and signals their real pids, and its work is
// the test's processes rather than the host's. stage4b-role-kill.test.ts's own_proc is the same in
// bash, for a root a harness shell makes for itself.

/** The pids the kernel lists as children of `pid`'s threads (/proc/<pid>/task/<tid>/children), and
 *  their children in turn, to the bottom, when `recursive`. A process or a thread that ended since
 *  the listing has no children to list. */
export function descendantsOf(pid: number, recursive: boolean, seen = new Set<number>()): number[] {
  if (seen.has(pid)) return [];
  seen.add(pid);
  let tasks: string[];
  try {
    tasks = readdirSync(`/proc/${pid}/task`);
  } catch {
    return [];
  }
  const found: number[] = [];
  for (const tid of tasks) {
    let children: string[];
    try {
      children = readFileSync(`/proc/${pid}/task/${tid}/children`, "utf8").split(/\s+/);
    } catch {
      continue;
    }
    for (const child of children) {
      if (child === "") continue;
      found.push(Number(child));
      if (recursive) found.push(...descendantsOf(Number(child), true, seen));
    }
  }
  return found;
}

/** A fresh proc root under `dir`: a link to /proc/<pid> for each of `pids` still running. */
export function procRootOf(dir: string, pids: readonly number[]): string {
  const root = mkdtempSync(join(dir, "proc-"));
  for (const pid of new Set(pids)) {
    if (existsSync(`/proc/${pid}`)) symlinkSync(`/proc/${pid}`, join(root, String(pid)));
  }
  return root;
}

/** The processes `named` ([what, pid]) that `root` holds no live entry for, each as "<what> <pid>".
 *  A test reads the code's verdict on a process as a pass only with that process in view, so a
 *  root without one fails the test, naming it. */
export function notInRoot(root: string, named: readonly (readonly [string, number])[]): string[] {
  return named
    .filter(([, pid]) => !existsSync(join(root, String(pid), "stat")))
    .map(([what, pid]) => `${what} ${pid}`);
}

/** A pid no /proc can hold, the kernel's pid_max plus one, made an empty entry of `root` for the
 *  test to fill: code under test that reaches it read the root it was given, and a walk that went
 *  back to the host's /proc fails the test rather than only slowing it. */
export function unrealPid(root: string): string {
  const unreal = String(Number(readFileSync("/proc/sys/kernel/pid_max", "utf8")) + 1);
  mkdirSync(join(root, unreal));
  return unreal;
}
