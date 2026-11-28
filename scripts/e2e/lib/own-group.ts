import { readFileSync, readlinkSync, realpathSync } from "node:fs";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { untilTrue } from "./row-waits";

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
 * in the shell's group, which is its own. A file whose rows call ownGroup gates them on perl
 * (hostGate: each skipped naming perl outside CI, each failed by name under GITHUB_ACTIONS);
 * ownGroup itself throws, naming perl, for a caller that did not.
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
 * untilRunning(pid, program, end) resolves once pid runs program (its /proc/<pid>/exe, the kernel's
 * record), and fails at END (lib/row-waits.ts's RowWaits.end, 20 s before the row's bound) naming
 * the pid, the program and what the pid ran instead: how soon a process the runner started runs
 * its program is the kernel's, and a loaded host's. A process ownGroup started leads its own group
 * only once its perl has called setpgrp, and runs its program only after that, so a sweep row waits
 * on this, or on untilExecDone where the sweep must also find the process by its environment,
 * before it sweeps: a sweep that stopped the perl before its setpgrp would stop a member of the
 * runner's own group.
 */
export async function untilRunning(pid: number, program: string, end: number): Promise<void> {
  const want = realpathSync(hostTool(program) ?? program);
  await untilTrue(`pid ${pid} to run ${want}`, end, () => {
    let seen = "";
    try {
      seen = readlinkSync(`/proc/${pid}/exe`);
    } catch {}
    return seen === want || `it runs ${seen || "nothing"}`;
  });
}

/**
 * untilExecDone(pid, program, end) resolves once pid has finished its exec of program as far as a
 * walk of the processes' environments reads it (procs_naming, lib/process-tree.sh): its exe link
 * (/proc/<pid>/exe) names program and its /proc/<pid>/environ reads non-empty. Inside an exec Linux
 * gives the process its new memory, and the exe link with it, before it sets where that memory's
 * arguments and environment end (create_elf_tables in fs/binfmt_elf.c): first the arguments' end,
 * then a placeholder end of the environment, which lets /proc/<pid>/cmdline read in full
 * (get_mm_cmdline in fs/proc/base.c reads once that end is not 0), and only after it has counted
 * each variable the environment's real end. Until then environ reads empty and a walk names
 * nothing, so a wait on the exe link (untilRunning) or on the command line can end inside the exec,
 * and a sweep made then stops nothing. So a row that walks or sweeps a process it started waits on
 * this rather than on untilRunning. A process started with an empty environment never passes it.
 * Nothing signals the end of another process's exec, so it reads both every 50 ms (untilTrue), and
 * fails at END (lib/row-waits.ts's RowWaits.end) as untilRunning does, naming the pid, what its exe
 * link read and how many bytes its environ held.
 */
export async function untilExecDone(pid: number, program: string, end: number): Promise<void> {
  const want = realpathSync(hostTool(program) ?? program);
  await untilTrue(`pid ${pid} to finish its exec of ${want}`, end, () => {
    let exe = "";
    let environ = 0;
    try {
      exe = readlinkSync(`/proc/${pid}/exe`);
      environ = exe === want ? readFileSync(`/proc/${pid}/environ`).length : 0;
    } catch {}
    return (
      (exe === want && environ > 0) ||
      `its exe link read ${exe || "nothing"} and its environ ${environ} bytes`
    );
  });
}
