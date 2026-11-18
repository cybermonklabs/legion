import { beforeAll, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// run_processes (lib/rig.sh) names the processes a run still has alive, and a teardown SIGKILLs
// each one. It reads every /proc/<pid> on the devbox, thousands of them, so it does its reading in
// one pass and runs nothing for each entry: a `readlink` and a `grep` for each cost a teardown, and
// each of a checkpoint's sweeps, seconds. The run below puts a `readlink` and a `grep` on PATH that
// record every call and answer nothing, so a run that still calls one both fails to find the run's
// processes and says which command it called.
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
      { env: { PATH: `${bin}:${process.env.PATH ?? ""}` }, stdout: "pipe", stderr: "pipe" }
    );
    expect(ran.stderr.toString()).toBe("");
    expect(ran.exitCode).toBe(0);
    const pids = ran.stdout.toString().split("\n").filter(Boolean);
    expect(pids).toContain(String(inside.pid));
    expect(pids).toContain(String(named.pid));
    expect(pids).not.toContain(String(other.pid));
    expect(existsSync(calls) ? readFileSync(calls, "utf8") : "").toBe("");
  } finally {
    for (const child of children) child.kill("SIGKILL");
  }
});

// harness runs BODY in a bash with lib/rig.sh sourced for the run directory WORK, under the
// drivers' `set -Eeuo pipefail` and an ERR trap that prints, and answers its exit code and output.
// WORK comes in through the environment, so the harness's own command line does not name it and
// run_processes does not name the harness.
function harness(work: string, body: string) {
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
root=${JSON.stringify(join(import.meta.dir, "..", "..", ".."))}
work=$WORK
. ${JSON.stringify(rig)}
${body}`,
    ],
    { env: { PATH: process.env.PATH ?? "", WORK: work }, stdout: "pipe", stderr: "pipe" }
  );
  return { code: ran.exitCode, out: ran.stdout.toString(), err: ran.stderr.toString() };
}

test("names a process of the run that left every tree the run started, double-forked to pid 1 or a subreaper", () => {
  // run_processes is the run's search for its leftovers: a daemon that forked twice and whose
  // parent ended belongs to no tree the run recorded, and only a walk of every process finds it.
  const work = join(dir, "escaped");
  mkdirSync(work);
  const pidFile = join(dir, "escaped.pid");
  let pid = 0;
  try {
    const forked = Bun.spawnSync(
      [
        "bash",
        "-c",
        `( (cd "$1" && exec sleep 60 </dev/null >/dev/null 2>&1) & echo $! >"$2" ) </dev/null >/dev/null 2>&1`,
        "fork",
        work,
        pidFile,
      ],
      { stdout: "ignore", stderr: "pipe" }
    );
    expect(forked.exitCode).toBe(0);
    pid = Number(readFileSync(pidFile, "utf8").trim());
    // Its parent, the subshell, has ended, so nothing the test started is its parent any more.
    const parent = readFileSync(`/proc/${pid}/stat`, "utf8")
      .replace(/^.*\) /, "")
      .split(" ")[1];
    expect(parent).not.toBe(String(process.pid));
    const r = harness(work, "run_processes");
    expect(r.err).toBe("");
    expect(r.code).toBe(0);
    expect(r.out.split("\n").filter(Boolean)).toEqual([String(pid)]);
  } finally {
    if (pid > 0) {
      try {
        process.kill(pid, "SIGKILL");
      } catch {}
    }
  }
});

test("names every process of the run where argv and the environment past ARG_MAX would stop a program handed every /proc entry", () => {
  // The force: a 512 KiB stack makes ARG_MAX 128 KiB, and a variable fills the environment to 1 KiB
  // short of the largest that still lets a program of no arguments start, measured here. An exec
  // handed every /proc/<pid>/cwd as an argument then fails with E2BIG, as `find /proc/[0-9]*/cwd`
  // does unforced at about 80,000 processes, while an exec of a few arguments still starts. The
  // walk must still name the run's process, or fail naming why; an empty answer is the defect.
  const work = join(dir, "e2big");
  mkdirSync(work);
  const inside = Bun.spawn(["sleep", "60"], { cwd: work, stdout: "ignore", stderr: "ignore" });
  try {
    const r = harness(
      work,
      `ulimit -s 512
[ "$(getconf ARG_MAX)" = 131072 ] || { echo "ARG_MAX is $(getconf ARG_MAX), not 131072, under a 512 KiB stack" >&2; exit 1; }
true_bin=$(type -P true)
lo=0 hi=131072
while [ $((hi - lo)) -gt 64 ]; do
  mid=$(((lo + hi) / 2))
  if PADDING=$(printf "%\${mid}s" "") "$true_bin" 2>/dev/null; then lo=$mid; else hi=$mid; fi
done
[ "$lo" -gt 4096 ] || { echo "no room for the padding: a program starts with only $lo bytes of it" >&2; exit 1; }
export PADDING
PADDING=$(printf "%\${lo}s" "")
PADDING=\${PADDING:1024}
# The control the force must pass: the form this replaced cannot start, and a program of no
# arguments still does.
"$true_bin" || { echo "a program of no arguments does not start under the force" >&2; exit 1; }
if find /proc/[0-9]*/cwd -maxdepth 0 -printf '%h %l\\0' >/dev/null 2>"$work/old.err"; then echo "the old form started: the force is not in force" >&2; exit 1; fi
{ IFS= read -r line <"$work/old.err"; } || line=
[[ $line == *"Argument list too long"* ]] || { echo "the old form failed otherwise: $line" >&2; exit 1; }
run_processes`
    );
    expect(r.err).toBe("");
    expect(r.code).toBe(0);
    expect(r.out.split("\n").filter(Boolean)).toContain(String(inside.pid));
  } finally {
    inside.kill("SIGKILL");
  }
});
