/**
 * ownGroup(argv) is argv, run as the leader of a process group of its own inside the test runner's
 * session, with the runner as its parent: a python3 that calls setpgid(0, 0) and then execs argv, so
 * the pid Bun.spawn returns is argv's own.
 *
 * A row that stops a process (SIGSTOP, through stop_tree, freeze_if or a sweep of
 * lib/process-tree.sh) must not stop a member of the runner's own process group. Where that group
 * is orphaned, as in a Legion pod, whose bash tool starts each command as the leader of a session
 * of its own, gVisor before release-20261012.0 sent SIGHUP and SIGCONT to an orphaned group with a
 * stopped member whenever another member exited, and `bun test` died of that SIGHUP (status 129)
 * without naming a row; gVisor's 553528ee1034 (2026-10-07) sends them only when the group becomes
 * orphaned, as Linux does. A group of its own, whose parent (the runner) is in another group of the
 * same session, is not orphaned while the runner runs, so its stops reach nothing of the runner's,
 * on either. A session of its own (setsid, or Bun.spawn's `detached`) is the wrong tool: its group
 * is orphaned from the start, and a stop in it with another member exiting hangs the row's own
 * processes up.
 *
 * Everything a harness shell started this way runs in the background (`&`, job control off) stays
 * in the shell's group, which is its own. python3 is the first on PATH, resolved once to its own
 * executable (sys.executable), as packages/pi-legion/scripts/skill-scenarios/rig-isolation.test.ts
 * resolves it: a version manager's shim there can refuse to run under a row's own HOME.
 */
let python = "";

const setpgidExec = "import os, sys; os.setpgid(0, 0); os.execvp(sys.argv[1], sys.argv[1:])";

export function ownGroup(argv: readonly string[]): string[] {
  if (python === "") {
    const resolved = Bun.spawnSync(["python3", "-I", "-c", "import sys; print(sys.executable)"]);
    const executable = resolved.stdout.toString().trim();
    if (resolved.exitCode !== 0 || !executable.startsWith("/")) {
      throw new Error(
        `python3 -I could not name its own executable (exit ${resolved.exitCode}): ${resolved.stderr.toString().trim()}`
      );
    }
    python = executable;
  }
  return [python, "-I", "-c", setpgidExec, ...argv];
}
