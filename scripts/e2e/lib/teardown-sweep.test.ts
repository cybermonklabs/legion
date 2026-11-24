import { describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptFunctions } from "./script-functions";

// Every teardown that sweeps the run's processes takes the sweep's failure as a failed teardown: a
// sweep that could not read the machine, or that left a process of the run it could not check
// running, fails with 2 (stop_marked_processes, lib/process-tree.sh; the skill-scenario rig's
// stop_env_processes), and a teardown that read that 2 as clean ended a run that passed with 0
// while a process of the run may still run. Each row runs one teardown as its script installs it,
// the EXIT trap of a shell whose run ended 0 or 1, with the sweep made to exit 2 or 0; every other
// command the teardown runs succeeds and does nothing (the tools are functions that return 0, and
// a helper of the script's own that is not taken here is answered by command_not_found_handle).
// The sweep that passes is the control: the same teardown then ends with the run's own status.
const e2e = join(import.meta.dir, "..");
const root = join(e2e, "..", "..");
const dir = temporaryDirectory("teardown-sweep-test");

type Ran = { code: number | null; out: string };

// The eleven proofs that sweep in their EXIT trap's cleanup, and the Legion section's cast recorder,
// which sweeps the same way.
const proofs = [
  "claude-backend-sandbox",
  "claude-backend-tmux",
  "clean-session-stop-tmux",
  "controller-start-tmux",
  "dispatch-user-turns",
  "envoy-session-states",
  "parked-decision-devbox",
  "stage2-tmux-supervision",
  "stage3-4b13b-acceptance",
  "stage3-devbox-workflow",
  "stage4b-sandbox-tree",
].map((name) => join("scripts/e2e", `${name}.sh`));
proofs.push("docs/site/media/walkthroughs/legion-operator/record-casts.sh");

// runCleanup runs PROOF's cleanup as its EXIT trap after a run that ended IN, its sweep exiting
// SWEEP. All the shell writes, on either stream and on the descriptor 7 two cleanups write to, goes
// to one pipe in order. ROUTE is the run's model_route: under box-login the two proofs that sweep
// twice (Stage 2 and Stage 3's devbox workflow) first give the panes a grace (`stop_marked_processes
// DIR GRACE`), then sweep again; under gateway only the second sweep runs. With FIRST_ONLY only the
// sweep with a grace fails, so each of the two sweeps is held to its own handling. Every harness
// loads lib/run-verdict.sh by its path (`run_verdict`, PASS only for a run that reached `run_end`),
// whichever library the proof gets it from, so no proof's `run_verdict` is answered by
// command_not_found_handle; a run that ended 0 has reached its end (`run_end`) and set the mark a
// proof of its own verdict reads (`ok`).
function runCleanup(
  proof: string,
  sweep: number,
  runStatus: number,
  route = "box-login",
  firstOnly = false
): Ran {
  const path = join(root, proof);
  const script = readFileSync(path, "utf8");
  const trap = script.includes(`trap 'cleanup "$?"' EXIT`)
    ? `trap 'cleanup "$?"' EXIT`
    : "trap cleanup EXIT";
  const work = mkdtempSync(join(dir, "cleanup-"));
  const shell = `exec 2>&1 7>&1
sweep=$1 in=$2 work=$3 model_route=$4 first_only=$5
set -e
command_not_found_handle() { return 0; }
stop_marked_processes() { [ "$first_only" != 1 ] || [ "$#" -ge 2 ] || return 0; return "$sweep"; }
for c in docker tmux psql op gh git kubectl curl bash cp python3; do eval "$c() { return 0; }"; done
evidence=$work check=x ran=1 passed=1 ok=
. ${JSON.stringify(join(e2e, "lib", "run-verdict.sh"))}
[ "$in" != 0 ] || { ok=1; run_end; }
declare -A latencies=()
${scriptFunctions(path)("cleanup")}
${trap}
exit "$in"`;
  const r = Bun.spawnSync(
    [
      "bash",
      "-c",
      shell,
      "-",
      String(sweep),
      String(runStatus),
      work,
      route,
      firstOnly ? "1" : "0",
    ],
    {
      env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "" },
      stdout: "pipe",
      stderr: "pipe",
      timeout: 30_000,
    }
  );
  return { code: r.exitCode, out: r.stdout.toString() + r.stderr.toString() };
}
const failLine = (ran: Ran) => ({ code: ran.code, failLine: /FAIL.*process-sweep/.test(ran.out) });

describe("a proof's teardown", () => {
  for (const proof of proofs) {
    test(`${proof}: a sweep that failed after a passing run ends it 1 on a FAIL line naming process-sweep, and the run's own status otherwise, a signal's included`, () => {
      expect(failLine(runCleanup(proof, 0, 0))).toEqual({ code: 0, failLine: false });
      expect(runCleanup(proof, 0, 1).code).toBe(1);
      expect(failLine(runCleanup(proof, 2, 0))).toEqual({ code: 1, failLine: true });
      expect(runCleanup(proof, 2, 1).code).toBe(1);
      // A run an interrupt stopped (130) keeps its status, whatever the sweep did, except under
      // Stage 4b, which ends 1 on any failed teardown check, as on its others.
      expect(runCleanup(proof, 0, 130).code).toBe(130);
      expect(runCleanup(proof, 2, 130).code).toBe(
        proof.endsWith("stage4b-sandbox-tree.sh") ? 1 : 130
      );
    }, 60_000); // A guard on a hang only: six short shells.
  }
  for (const proof of [
    "scripts/e2e/stage2-tmux-supervision.sh",
    "scripts/e2e/stage3-devbox-workflow.sh",
  ]) {
    test(`${proof}: each of its two sweeps that fails alone ends a passing run 1 on a FAIL line naming process-sweep`, () => {
      expect(failLine(runCleanup(proof, 0, 0, "gateway"))).toEqual({ code: 0, failLine: false });
      expect(failLine(runCleanup(proof, 2, 0, "gateway"))).toEqual({ code: 1, failLine: true });
      expect(failLine(runCleanup(proof, 2, 0, "box-login", true))).toEqual({
        code: 1,
        failLine: true,
      });
    }, 60_000); // A guard on a hang only: three short shells.
  }
});

// runRigExit runs the skill-scenario rig's exit HANDLER (stop_run or stop_batch) with its
// stop_env_processes, as the EXIT trap of a shell under the rig's own `set -euo pipefail` whose
// command ended IN, where the sweep's walk (run_env_pids) exits WALK (0, naming nothing, or 2, the
// machine's environments not read).
function runRigExit(handler: string, walk: number, runStatus: number): Ran {
  const fn = scriptFunctions(join(root, "packages/pi-legion/scripts/skill-scenarios/rig.sh"));
  const work = mkdtempSync(join(dir, `rig-${handler}-`));
  const shell = `exec 2>&1
walk=$1 in=$2 work=$3
set -euo pipefail
run_env_pids() { [ "$walk" = 0 ] || echo "procs_naming: no environment could be read" >&2; return "$walk"; }
kill_held() { cat >/dev/null; }
stop_tree() { :; }
stop_panes() { :; }
services_down() { :; }
tmux() { :; }
daemon_pid='' dispatch_pid='' session=s R=$work runs_pid='' tag=t batch_scenario=b
${fn("stop_env_processes")}
${fn("stop_run")}
${fn("stop_batch")}
trap ${handler} EXIT
exit "$in"`;
  const r = Bun.spawnSync(["bash", "-c", shell, "-", String(walk), String(runStatus), work], {
    env: { PATH: process.env.PATH ?? "" },
    stdout: "pipe",
    stderr: "pipe",
    timeout: 30_000,
  });
  return { code: r.exitCode, out: r.stdout.toString() + r.stderr.toString() };
}

describe("the skill-scenario rig's exit", () => {
  for (const handler of ["stop_run", "stop_batch"]) {
    test(`${handler}: a sweep whose walk could not read ends the command 1, and the command's own status otherwise`, () => {
      expect(runRigExit(handler, 0, 0)).toEqual({ code: 0, out: "" });
      expect(runRigExit(handler, 0, 1).code).toBe(1);
      const swept = runRigExit(handler, 2, 0);
      expect(swept.code).toBe(1);
      expect(swept.out).toContain("stop_env_processes: stopped no process of the run under");
      expect(runRigExit(handler, 2, 1).code).toBe(1);
    }, 60_000); // A guard on a hang only: four short shells.
  }
});
