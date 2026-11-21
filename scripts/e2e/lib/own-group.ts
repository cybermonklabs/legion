import { readlinkSync, realpathSync } from "node:fs";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { until } from "@legion/test-fixtures/wait";

/**
 * ownGroup(argv) is argv, run as the leader of a process group of its own inside the test runner's
 * session, with the runner as its parent: a perl that calls setpgrp(0, 0) (setpgid) and then execs
 * argv, so the pid Bun.spawn returns is argv's own. perl changes nothing else argv gets: no signal
 * disposition and no environment variable. A python3 in its place ignores SIGPIPE and SIGXFSZ at
 * startup and, in a C locale, coerces LC_CTYPE into its environment, and its exec handed all three
 * to the row's program, so a row ran with SIGPIPE ignored where the rigs do not.
 *
 * A row that stops a process (SIGSTOP, through stop_tree, freeze_if or a sweep of
 * lib/process-tree.sh) must not stop a member of the runner's own process group. Where that group
 * is orphaned, as in a Legion pod, whose bash tool starts each command as the leader of a session
 * of its own, gVisor sends SIGHUP and SIGCONT to an orphaned group with a stopped member whenever
 * another member exits, and `bun test` dies of that SIGHUP (status 129) without naming a row. A
 * Legion pod (4.19.0-gvisor) still did that when measured on 2026-11-18. gVisor's commit
 * 553528ee1034 (2026-10-07) sends them only when the group becomes orphaned, as Linux does: under
 * runsc release-20260928.0 a stopped member of an orphaned group got SIGHUP when another member
 * exited, and under release-20261012.0 and release-20261116.0 it did not. A group of its own, whose
 * parent (the runner) is in another group of the same session, is not orphaned while the runner
 * runs, so its stops reach nothing of the runner's on any of them. A session of its own (setsid,
 * or Bun.spawn's `detached`) is the wrong tool: its group is orphaned from the start.
 *
 * Everything a harness shell started this way runs in the background (`&`, job control off) stays
 * in the shell's group, which is its own. A file whose rows call ownGroup skips them by name where
 * the host has no perl (skipWithout("perl")); ownGroup itself throws, naming perl, for a caller that
 * did not.
 */
let perl = "";

const setpgrpExec =
  'setpgrp(0, 0) or die "setpgrp: $!\\n"; exec { $ARGV[0] } @ARGV or die "exec $ARGV[0]: $!\\n"';

export function ownGroup(argv: readonly string[]): string[] {
  if (perl === "") {
    const found = hostTool("perl");
    if (found === null) {
      throw new Error("ownGroup needs perl on PATH to start a row's process in a group of its own");
    }
    perl = found;
  }
  return [perl, "-e", setpgrpExec, "--", ...argv];
}

/**
 * untilRunning(pid, program) resolves once pid runs program (its /proc/<pid>/exe, the kernel's
 * record). It waits through @legion/test-fixtures/wait's `until`, which takes no bound and ends at
 * the row's deadline; the failure names the pid, the program and what the pid ran instead. A
 * process ownGroup started leads its own group only once its perl has called setpgrp, and runs its
 * program only after that, so a sweep row waits on this before it sweeps: a sweep that stopped the
 * perl before its setpgrp would stop a member of the runner's own group.
 */
export async function untilRunning(pid: number, program: string): Promise<void> {
  const want = realpathSync(hostTool(program) ?? program);
  let seen = "";
  try {
    await until(`pid ${pid} to run ${want}`, () => {
      try {
        seen = readlinkSync(`/proc/${pid}/exe`);
      } catch {
        seen = "";
      }
      return seen === want;
    });
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error);
    throw new Error(`${reason} (running ${seen || "nothing"})`, { cause: error });
  }
}
