import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import {
  expectNoMissingHelpers,
  nameMissingHelpers,
  scriptAssignments,
  scriptFunctions,
} from "./script-functions";

// live-daemon-proof.sh's last line, printed by its EXIT trap (cleanup), taken from the lib by name.
// bash runs an EXIT trap with status 0 when a signal (TERM, INT or HUP) ends the shell, so the
// status alone reads a killed run as a pass. cleanup prints PASS only for a run that reached
// live_daemon_proof's last statement (run_ended) and exits 0; every other run reads FAIL at the
// check it stopped in.

const lib = join(import.meta.dir, "live-daemon-proof.sh");
const fn = scriptFunctions(lib);
const assignment = scriptAssignments(lib);
const helperGuard = nameMissingHelpers(lib);

/** Runs BODY in a bash whose EXIT trap is the lib's cleanup, over the state cleanup reads as a run
 * standing in its `proof` check leaves it, with no broker and no work directory, and the end mark
 * as the lib first assigns it. */
function cleanupRun(body: string) {
  const result = Bun.spawnSync([
    "bash",
    "-c",
    `set -euo pipefail
${helperGuard}
${fn("cleanup")}
${assignment("run_ended")}
proof_label='child adoption' evidence=/evidence work='' nats_pid='' check=proof started=$SECONDS
trap cleanup EXIT
${body}`,
  ]);
  const out = result.stdout.toString();
  const err = result.stderr.toString();
  expectNoMissingHelpers(out, err);
  return { code: result.exitCode, signal: result.signalCode, out, err };
}

describe("the run's last line", () => {
  test("reads FAIL at the check a SIGTERM ended the run in, though bash gives the trap status 0", () => {
    const run = cleanupRun("kill -TERM $$\nsleep 5\nrun_ended=1");
    expect(run.out).toBe("");
    expect(run.err).toMatch(
      /^child adoption e2e: FAIL at proof after \d+s: the run ended before its last check, by a signal or an exit of its own \(evidence: \/evidence\)\n$/
    );
    expect(run.code === 143 || run.signal === "SIGTERM").toBe(true);
  });

  test("reads PASS for a run that reached its end and exits 0", () => {
    const run = cleanupRun("run_ended=1\nexit 0");
    expect(run.code).toBe(0);
    expect(run.err).toBe("");
    expect(run.out).toMatch(/^child adoption e2e: PASS in \d+s \(evidence: \/evidence\)\n$/);
  });
});
