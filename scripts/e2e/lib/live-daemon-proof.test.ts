import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  inOwnGroup,
  type KillPoint,
  liveDaemonStandIns,
  type StopSignal,
  stopped,
} from "./live-daemon-proof-fixture";

// live-daemon-proof.sh's last line, printed by its EXIT trap (cleanup) through lib/run-verdict.sh.
// bash runs an EXIT trap with status 0 when a signal (TERM, INT or HUP) ends the shell, so the
// status alone reads a killed run as a pass. cleanup prints PASS only for a run that reached
// live_daemon_proof's last statement (run_end) and exits 0; every other run reads FAIL at the
// check it stopped in. Each row drives the real live_daemon_proof, its real end mark and the real
// lib, over stand-ins of go, the nats-server, python3 and the driver
// (lib/live-daemon-proof-fixture.ts): moving run_end above the proof check, or the lib's mark
// itself, makes a killed row print PASS.

const root = join(import.meta.dir, "..", "..", "..");
const scratch = temporaryDirectories("live-daemon-proof-test");
const withoutSetsid = skipWithout("setsid");

const testName = "TestAFixtureProofLive";
// What a driver whose one test passed prints.
const passingDriver = [
  `=== RUN   ${testName}`,
  "== the-fixture-step",
  "ok the-fixture-step",
  `--- PASS: ${testName} (0.01s)`,
  "PASS",
];

/** Runs the real live_daemon_proof over stand-ins, stopped at KILL when given, in a process group
 * of its own; with HANDED, it hands the run the stand-in driver as DRIVER. The lib's transcript
 * sends stderr to stdout, so `out` holds every line. */
function fixtureProof(kill?: { at: KillPoint; signal: StopSignal }, handed = false) {
  const dir = scratch();
  const env = liveDaemonStandIns(dir, { driverOutput: passingDriver, kill });
  const evidence = join(dir, "evidence");
  const driver = handed ? ` '' ${JSON.stringify(join(dir, "driver"))}` : "";
  const run = inOwnGroup(
    [
      "bash",
      "-c",
      `set -euo pipefail
root=${JSON.stringify(root)}
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'fixture proof' fixture-proof ${testName} ${JSON.stringify(evidence)}${driver}`,
    ],
    env
  );
  return { ...run, evidence };
}

describe.skipIf(withoutSetsid)("the run's last line", () => {
  for (const signal of ["TERM", "HUP", "INT"] as const) {
    test(`reads FAIL at the proof check a group SIG${signal} ended the run in`, () => {
      const run = fixtureProof({ at: "proof", signal });
      expect(run.out).toContain("ok driver (");
      expect(run.out).not.toContain("e2e: PASS");
      expect(run.out).toMatch(/\nfixture proof e2e: FAIL at proof after \d+s \(evidence: .*\)\n$/);
      expect(run.code === 0 && run.signal === null).toBe(false);
    });
  }

  test("reads FAIL at the driver check for a run a group SIGTERM ended in its setup", () => {
    const run = fixtureProof({ at: "build", signal: "TERM" });
    expect(run.out).toContain("ok broker (");
    expect(run.out).not.toContain("e2e: PASS");
    expect(run.out).toContain(`${stopped}\n`);
    expect(run.out).toMatch(/\nfixture proof e2e: FAIL at driver after \d+s \(evidence: .*\)\n$/);
    expect(run.code === 143 || run.signal === "SIGTERM").toBe(true);
  });

  test("reads PASS for a run that reached its end and exits 0", () => {
    const run = fixtureProof();
    expect(run.code).toBe(0);
    expect(run.out).toContain("ok proof (");
    expect(run.out).not.toContain("FAIL");
    expect(run.out).toMatch(/\nfixture proof e2e: PASS in \d+s \(evidence: .*\)\n$/);
    expect(run.out).toContain(`(evidence: ${run.evidence})`);
  });
});

// A caller that built the driver before the run hands it over as DRIVER, and the driver check
// builds nothing: the stand-in build that would end the run with a group SIGTERM never runs, and
// the proof runs the binary it was handed.
describe.skipIf(withoutSetsid)("the driver check", () => {
  test("builds nothing for a run handed its driver", () => {
    const run = fixtureProof({ at: "build", signal: "TERM" }, true);
    expect(run.code).toBe(0);
    expect(run.out).toContain("; this check builds nothing\n");
    expect(run.out).not.toContain("FAIL");
    expect(run.out).toMatch(/\nfixture proof e2e: PASS in \d+s \(evidence: .*\)\n$/);
  });
});
