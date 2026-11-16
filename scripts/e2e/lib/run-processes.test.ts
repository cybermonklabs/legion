import { beforeAll, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { descendantsOf, notInRoot, procRootOf, unrealPid } from "./proc-root";

// run_processes (lib/rig.sh) names the processes a run still has alive, and a teardown SIGKILLs
// each one. It reads every /proc/<pid> on the devbox, thousands of them, so it does its reading in
// one pass and runs nothing for each entry: a `readlink` and a `grep` for each cost a teardown, and
// each of a checkpoint's sweeps, seconds. The run below puts a `readlink` and a `grep` on PATH that
// record every call and answer nothing, so a run that still calls one both fails to find the run's
// processes and says which command it called.
// It reads `${proc_root:-/proc}`, and the row hands it a proc root of the test's own children
// (lib/proc-root.ts), the three processes it starts among them, each a link to its real /proc
// entry: over the host's /proc the row's work, under Bun's 5 s default, was the host's process
// count, which no test owns. A command run for each entry is still one call per process of the
// root, which the stand-ins record.
const rig = join(import.meta.dir, "rig.sh");
const dir = temporaryDirectory("run-processes-test");
const bin = join(dir, "bin");
const calls = join(dir, "calls");
beforeAll(() => {
  mkdirSync(bin);
  for (const name of ["readlink", "grep"]) {
    writeFileSync(
      join(bin, name),
      `#!/bin/sh\nprintf '${name} %s\\n' "$*" >>"${calls}"\nexit 1\n`,
      {
        mode: 0o755,
      }
    );
  }
});

test("names the run's processes by working directory and by command line, without running a command for each /proc entry", () => {
  const work = join(dir, "work");
  mkdirSync(work);
  // Three processes: one working inside the run's directory, one naming it in its command line
  // from a working directory of its own, and one that is no part of the run. Each is started
  // inside the `try`, and registered as it is started, so a throw between two spawns leaves none
  // of them running.
  const children: Bun.Subprocess<"ignore", "ignore", "ignore">[] = [];
  try {
    const inside = Bun.spawn(["sleep", "60"], { cwd: work, stdout: "ignore", stderr: "ignore" });
    children.push(inside);
    // The `exit 0` keeps bash from replacing itself with sleep, which would take its argv with it.
    const named = Bun.spawn(["bash", "-c", "sleep 60; exit 0", work], {
      cwd: dir,
      stdout: "ignore",
      stderr: "ignore",
    });
    children.push(named);
    const other = Bun.spawn(["sleep", "60"], { cwd: dir, stdout: "ignore", stderr: "ignore" });
    children.push(other);
    // The proc root: a link to the real /proc entry of every child the kernel lists for this
    // test's process, the three among them. A verdict on a process is read as a pass only with
    // that process in view, so a root without one of the three fails the row, naming it.
    const proc = procRootOf(dir, descendantsOf(process.pid, false));
    expect(
      notInRoot(proc, [
        ["the process inside", inside.pid],
        ["the process naming the run", named.pid],
        ["the process of neither kind", other.pid],
      ])
    ).toEqual([]);
    // And one entry no /proc can hold, a pid above the kernel's pid_max whose working directory is
    // the run's: run_processes names it only when it reads the root it is given, so a walk that went
    // back to the host's /proc fails the row rather than only slowing it.
    const unreal = unrealPid(proc);
    symlinkSync(work, join(proc, unreal, "cwd"));
    const ran = Bun.spawnSync(
      [
        "bash",
        "-c",
        // The drivers call it under `set -E` with an ERR trap that prints, and in the bare word
        // split a stage's sweeps use (`for p in $(run_processes)`), where a status of its own
        // would reach that trap: the trap's line would then be read as a check's failure.
        `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
root=${JSON.stringify(join(import.meta.dir, "..", "..", ".."))}
work=${JSON.stringify(work)}
. ${JSON.stringify(rig)}
for p in $(run_processes); do echo "$p"; done`,
      ],
      {
        env: { PATH: `${bin}:${process.env.PATH ?? ""}`, proc_root: proc },
        stdout: "pipe",
        stderr: "pipe",
      }
    );
    expect(ran.stderr.toString()).toBe("");
    expect(ran.exitCode).toBe(0);
    const pids = ran.stdout.toString().split("\n").filter(Boolean);
    expect(pids).toContain(String(inside.pid));
    expect(pids).toContain(String(named.pid));
    expect(pids).toContain(unreal);
    expect(pids).not.toContain(String(other.pid));
    expect(existsSync(calls) ? readFileSync(calls, "utf8") : "").toBe("");
  } finally {
    for (const child of children) child.kill("SIGKILL");
  }
});
