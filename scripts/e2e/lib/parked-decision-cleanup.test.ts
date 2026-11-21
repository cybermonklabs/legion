import { describe, expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptFunctions } from "./script-functions";

// wait_for_run_processes, the cleanup-is-complete checkpoint's wait for every process of the run to
// end (parked-decision-devbox.sh). Each case takes the function from the script by name and runs it
// under the script's own `set` line, with stubs for the helpers it calls: run_processes, which
// counts its calls in a file, and note, fail and since. proc_describe, which names each survivor
// in the failure, is lib/process-tree.sh's own, which the script sources through lib/rig.sh.
const scriptPath = join(import.meta.dir, "..", "parked-decision-devbox.sh");
const fn = scriptFunctions(scriptPath);
const tree = scriptFunctions(join(import.meta.dir, "process-tree.sh"));
const set = readFileSync(scriptPath, "utf8").match(/^set -.*$/m)?.[0];
if (set === undefined) throw new Error(`${scriptPath} has no top-level set line`);
const dir = temporaryDirectory("parked-decision-cleanup-test");

let runs = 0;
// wait runs `wait_for_run_processes ARGS` with RUN_PROCESSES as run_processes's body; the body sees
// the call it is in as $call, counting from 1. BIN, when given, comes first on PATH.
function wait(runProcesses: string, args: string, bin = "") {
  const calls = join(dir, `calls-${++runs}`);
  writeFileSync(calls, "");
  const script = `${set}
note() { printf '   %s\\n' "$*"; }
fail() { printf 'FAIL: %s\\n' "$*" >&2; exit 1; }
since() { printf '%ss' "$((SECONDS - $1))"; }
run_processes() {
  echo call >>"$CALLS"
  local call
  call=$(wc -l <"$CALLS")
  ${runProcesses}
}
${tree("proc_describe")}
${fn("wait_for_run_processes")}
wait_for_run_processes ${args}`;
  const result = Bun.spawnSync(["bash", "-c", script], {
    env: { PATH: `${bin}${bin ? ":" : ""}${process.env.PATH ?? ""}`, CALLS: calls },
  });
  return {
    pid: result.pid,
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
    calls: readFileSync(calls, "utf8").split("\n").filter(Boolean).length,
  };
}

describe("wait_for_run_processes", () => {
  test("a poll that costs more than the whole deadline does not end the wait", () => {
    // The first walk outlasts the deadline and still finds a process; the next finds none. The
    // stub's status is that of the last thing it ran, 1 here: the real walk (run_processes) exits 0,
    // or 2 when it cannot read the processes, and the wait fails only on 2.
    const r = wait('if ((call == 1)); then sleep 1.1; echo 4242; fi; [ "$call" = 1 ]', "0 2");
    expect({ code: r.code, stderr: r.stderr }).toEqual({ code: 0, stderr: "" });
    expect(r.calls).toBeGreaterThan(1);
  });

  test("a process of the run that never ends fails the wait once the poll floor is reached, naming each survivor that runs and starting no ps, pgrep or pkill", () => {
    // Three survivors: the harness's own shell ($$) and this test process ($PPID), which run, and
    // a process that has ended. The failure names all three, and its note names each one that
    // runs, its parent, group, session, start and command line included, and none that has ended.
    // A ps, a pgrep and a pkill first on PATH log each call and run nothing: each reads every
    // process of the machine, and the note reads only the /proc entries of the survivors.
    const bin = join(dir, `bin-${++runs}`);
    mkdirSync(bin);
    const logged = join(bin, "calls");
    writeFileSync(logged, "");
    for (const name of ["ps", "pgrep", "pkill"]) {
      writeFileSync(join(bin, name), `#!/bin/sh\necho "$0 $*" >>'${logged}'\nexit 1\n`, {
        mode: 0o755,
      });
    }
    const ended = Bun.spawnSync(["true"]).pid;
    const r = wait(`printf '%s\\n' "$$" "$PPID" ${ended}`, "0 3", bin);
    expect(r.code).not.toBe(0);
    expect(r.calls).toBe(3);
    expect(r.stderr).toMatch(
      new RegExp(
        `^FAIL: timed out after \\d+s \\(3 polls\\) waiting for every process of the run to end; still running: ${r.pid} ${process.pid} ${ended}$`,
        "m"
      )
    );
    expect(r.stdout).toMatch(
      new RegExp(`^ +${r.pid} ppid ${process.pid} pgid \\d+ sid \\d+ started .+: bash -c set `, "m")
    );
    expect(r.stdout).toMatch(new RegExp(`^ +${process.pid} ppid \\d+ .*: \\S*bun`, "m"));
    expect(r.stdout).not.toMatch(new RegExp(`^ +${ended} `, "m"));
    expect(readFileSync(logged, "utf8")).toBe("");
  });

  // Catches a wait that gives up on the poll floor alone, the wall-clock half dropped: it would
  // abandon a cleanup after one poll on a machine where processes merely linger.
  test("a process of the run that never ends holds the wait until the deadline passes", () => {
    const r = wait("printf '%s\\n' 7777", "5 1");
    expect(r.code).not.toBe(0);
    expect(r.calls).toBeGreaterThan(1);
    expect(r.stderr).toContain("7777");
  });

  test("a call with no arguments waits the production bounds, 600 s and 10 polls", () => {
    // The walk finds nothing at once and exits 1 from its last test, which the wait, failing only
    // on the real walk's 2, reads as no failure.
    const r = wait("false", "");
    expect({ code: r.code, stderr: r.stderr }).toEqual({ code: 0, stderr: "" });
    const announced = r.stdout.split("\n")[0].match(/\d+/g)?.map(Number);
    expect(announced).toEqual([600, 10]);
  });
});
