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

test("names the run's processes by working directory and by command line, without running a command for each /proc entry", async () => {
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
    // So the sleep is a child of the bash, which a SIGKILL to the bash alone would leave running in
    // `dir`: `setsid` makes the bash (the same pid, since a spawned child leads no group) the leader
    // of a group of its own, and the group is killed whole.
    const named = Bun.spawn(["setsid", "bash", "-c", "sleep 60; exit 0", work], {
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
    // Each must have ended, not only been sent its SIGKILL, before the afterAll removes `dir`,
    // which they work in: the three, and the sleep of `named`'s group.
    for (const child of children) child.kill("SIGKILL");
    const group = children[1]?.pid;
    if (group !== undefined) {
      try {
        process.kill(-group, "SIGKILL");
      } catch {}
    }
    await Promise.all(children.map((child) => child.exited));
    if (group !== undefined) await groupGone(group);
  }
});

/** Resolves once no process of group PGID is left, signal 0 to the group reaching none; throws
 *  after 10 s. It polls real processes, which no fake clock moves. */
async function groupGone(pgid: number): Promise<void> {
  for (let i = 0; i < 500; i++) {
    try {
      process.kill(-pgid, 0);
    } catch {
      return;
    }
    await Bun.sleep(20);
  }
  throw new Error(`process group ${pgid} outlived its SIGKILL by 10 s`);
}
