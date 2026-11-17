import { describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  expectNoMissingHelpers,
  nameMissingHelpers,
  scriptAssignments,
  scriptFunctions,
} from "./script-functions";

// approved-version-news.sh's two controls and its last line, taken from the script by name and
// run over driver output written as each driver prints it. Part one's negative control
// (control_verdict) bites only when A2's one-approval check fails in its own words after the
// step's five setup checks: never on the line the driver prints when its harness fails, never on a
// killed run. Part two's count of each arm (part_two_step) counts only runs that reached the
// step's assertion: a run that stopped at its setup is named and not counted, so it never makes
// the control bite, and a head run that stopped there is no pass. The last line (finish, the
// script's EXIT trap) reads PASS or the control's verdict only for a run that reached one of the
// script's ends: a signal ends a shell with its EXIT trap reading status 0, so a killed run must
// read FAIL.

const script = join(import.meta.dir, "..", "approved-version-news.sh");
const assignment = scriptAssignments(script);
const fn = scriptFunctions(script);
const note = scriptFunctions(join(import.meta.dir, "live-daemon-proof.sh"))("note");
const helperGuard = nameMissingHelpers(script);
const dir = temporaryDirectory("approved-version-news-verdicts-test");

let files = 0;
/** A driver output file holding LINES, one a line; null writes no file at all. */
function proofLog(lines: string[] | null): string {
  const path = join(dir, `proof-${++files}.log`);
  if (lines !== null) writeFileSync(path, lines.map((line) => `${line}\n`).join(""));
  return path;
}

/** Runs BODY after the script's control settings, its two control functions and the lib's note. */
function controls(body: string) {
  const result = Bun.spawnSync([
    "bash",
    "-c",
    `set -euo pipefail
${helperGuard}
${note}
${assignment("control_step")}
${assignment("control_setup_checks")}
${assignment("control_failure")}
${assignment("part_two_runs")}
${fn("control_verdict")}
${fn("part_two_step")}
${body}`,
  ]);
  const out = result.stdout.toString();
  const err = result.stderr.toString();
  expectNoMissingHelpers(out, err);
  return { code: result.exitCode, out, err };
}

const a2 = "a2-the-reviewer-is-told-the-changed-sections";
const driverTest = "TestAnApprovedVersionReachesItsHolderLive";
// The base run's output up to A2's one-approval check: the driver's notes, then the step's five
// setup checks, each printed as it held.
const setup = (checks = 5) => [
  `=== RUN   ${driverTest}`,
  "   Dispatch http://127.0.0.1:41871 built from /tmp/base, its events on nats://127.0.0.1:40111",
  `== ${a2}`,
  ...[
    "the setup approval opened the gate of AVQ1-1 at version 1 of spec",
    "the setup approval opened the gate of AVQ1-2 at version 1 of spec",
    "version 2 of AVQ1-1's spec, changing its Design section, closed the gate",
    "the reviewer of AVQ1-1 is in a turn",
    "the person's approval of version 2 opened the gate again",
  ]
    .slice(0, checks)
    .map((check) => `   assert ${a2}: ${check}`),
];
// The one-approval check's own failure on a daemon from before the change, as awaitQueued writes it.
const ownFailure = `FAIL ${a2}: waited 2m0s for a task queued for the reviewer of AVQ1-1 behind its open turn; last seen: its claim holds no task`;
const goFailure = [
  `    approved_version_news_live_test.go:558: ${a2}: waited 2m0s for a task queued for the reviewer of AVQ1-1 behind its open turn`,
  `--- FAIL: ${driverTest} (191.40s)`,
  "FAIL",
];

/** control_verdict over PROOF and STATUS: whether it bit, what it counted and what it said. */
function controlVerdict(proof: string, status: number) {
  const run =
    controls(`if control_verdict ${JSON.stringify(proof)} ${status}; then echo bites; else echo "no control"; fi
printf 'counted=%s\\nsaid=%s\\n' "$counted" "$control_said"`);
  expect(run.err).toBe("");
  const [verdict, counted, said] = run.out.trimEnd().split("\n");
  return { verdict, counted, said: said.replace(/^said=/, "") };
}

describe("part one's negative control", () => {
  test("bites on the one-approval check's own failure after the five setup checks", () => {
    const control = controlVerdict(proofLog([...setup(), ownFailure, ...goFailure]), 1);
    expect(control).toEqual({ verdict: "bites", counted: "counted=5", said: ownFailure });
  });

  test("does not bite on the line the driver prints when its harness fails at that check", () => {
    // The base daemon died during the wait: the harness's read of the claim failed the test, and
    // the driver's run named the step in words of no check.
    const harness = `FAIL ${a2}: the step stopped on the failure the test log above names`;
    const control = controlVerdict(
      proofLog([
        ...setup(),
        "    daemon_test.go:961: GET /legion/v1/operator/claims: dial tcp 127.0.0.1:41234: connect: connection refused",
        harness,
        `--- FAIL: ${driverTest} (12.20s)`,
        "FAIL",
      ]),
      1
    );
    expect(control.verdict).toBe("no control");
    expect(control.counted).toBe("counted=5");
    expect(control.said).toStartWith(
      `no control: the base run stopped at ${a2}'s first one-approval check, but not on that check's own failure`
    );
    expect(control.said).toEndWith(harness);
  });

  test("does not bite on a killed run that printed no FAIL line", () => {
    const control = controlVerdict(proofLog(setup()), 137);
    expect(control.verdict).toBe("no control");
    expect(control.said).toBe(
      "no control: the base run's driver ended (status 137) with no FAIL line naming a step, a killed or timed-out run"
    );
  });

  test("does not bite on a run whose driver never ran", () => {
    const control = controlVerdict(proofLog(null), 1);
    expect(control).toEqual({
      verdict: "no control",
      counted: "counted=0",
      said: "no control: the base run failed (status 1) before its driver ran",
    });
  });

  test("does not bite on a pass", () => {
    const control = controlVerdict(
      proofLog([...setup(), `ok ${a2} (40s)`, `--- PASS: ${driverTest} (300.00s)`]),
      0
    );
    expect(control.verdict).toBe("no control");
    expect(control.said).toBe("no control: part one passed against the base");
  });

  test("does not bite on a failure before the step", () => {
    const setupFailure =
      'FAIL setup: Dispatch created Approved version news a2, member 0 with key "" and spec ""';
    const control = controlVerdict(
      proofLog([`=== RUN   ${driverTest}`, setupFailure, ...goFailure.slice(1)]),
      1
    );
    expect(control.verdict).toBe("no control");
    expect(control.said).toBe(
      `no control: the base run failed somewhere other than ${a2}: ${setupFailure}`
    );
  });

  test("does not bite on a failure inside the step's setup checks", () => {
    const early = `FAIL ${a2}: waited 2m0s for the reviewer of AVQ1-1 in a turn; last seen: state ready`;
    const control = controlVerdict(proofLog([...setup(3), early, ...goFailure.slice(1)]), 1);
    expect(control.verdict).toBe("no control");
    expect(control.counted).toBe("counted=3");
    expect(control.said).toStartWith(
      `no control: the base run failed at ${a2} after 3 of its checks`
    );
  });
});

const a4 = "a4-the-architect-tells-the-child-reviewer";
// One run's line as part two's driver prints it at the run's end.
const runLine = (arm: string, n: number, outcome: string) =>
  `run ${a4} ${arm} ${n}: ${outcome} in 140s, 31 model calls`;
const passed = "PASS";
const failed =
  "FAIL: the reviewer of AS1-2 was sent no Envoy message naming version 2 and the change";
const setupFailed =
  "SETUP: the reviewer of AS1-2 did not end its start turn within the run's bound";
const harnessFailed = "HARNESS: the harness failed; its log is above";

/** part_two_step over the A4 lines LINES: the step's part of the last line, the head runs that
 * missed, and the notes it printed. */
function partTwoStep(lines: string[]) {
  const proof = proofLog([`=== RUN   TestRealSessionsTellAndAnswerANewVersionLive`, ...lines]);
  const run = controls(`part_two_step ${JSON.stringify(proof)} ${a4} a4
printf 'said=%s\\nmissed=%s\\n' "$step_said" "$head_missed"`);
  expect(run.err).toBe("");
  const said = /^said=(.*)$/m.exec(run.out)?.[1];
  const missed = /^missed=(.*)$/m.exec(run.out)?.[1];
  return { said, missed, notes: run.out };
}

const headPassed = [1, 2, 3].map((n) => runLine("head", n, passed));

describe("part two's count of each arm", () => {
  test("does not count a base run that stopped at its setup, and does not bite on it", () => {
    const step = partTwoStep([
      ...headPassed,
      runLine("base", 1, passed),
      runLine("base", 2, passed),
      runLine("base", 3, setupFailed),
    ]);
    expect(step.said).toBe(
      "a4 head 3/3, base 2/2 with 1 not counted (no control: the base arm passed every run that reached its assertion)"
    );
    expect(step.missed).toBe("");
    expect(step.notes).toContain(`   not counted: ${runLine("base", 3, setupFailed)}\n`);
  });

  test("bites on a base run that failed at its assertion, beside one not counted", () => {
    const step = partTwoStep([
      ...headPassed,
      runLine("base", 1, passed),
      runLine("base", 2, failed),
      runLine("base", 3, setupFailed),
    ]);
    expect(step.said).toBe("a4 head 3/3, base 1/2 with 1 not counted (control bites)");
    expect(step.missed).toBe("");
  });

  test("is no control when every base run stopped before its assertion", () => {
    const step = partTwoStep([
      ...headPassed,
      runLine("base", 1, setupFailed),
      runLine("base", 2, setupFailed),
      runLine("base", 3, harnessFailed),
    ]);
    expect(step.said).toBe(
      "a4 head 3/3, base 0/0 with 3 not counted (no control: no base run reached its assertion)"
    );
  });

  test("names a head run that stopped at its setup, which is no pass", () => {
    // The driver stops at the first head run that does not pass, so the base arm never runs.
    const step = partTwoStep([runLine("head", 1, passed), runLine("head", 2, setupFailed)]);
    expect(step.said).toBe("a4 head 1/1 with 1 not counted, base 0/0 (base did not run)");
    expect(step.missed).toBe(`; a4: 2 of 3 head runs ran; ${runLine("head", 2, setupFailed)}`);
  });
});

/** Runs BODY in a bash whose EXIT trap is the script's finish, over the state finish reads as a
 * run of part one alone leaves it standing in its `driver` check with 3 of its 52 checks counted,
 * and the end mark as the script first assigns it; VERDICT is the control's, "" for none. */
function finishRun(body: string, verdict = "") {
  const result = Bun.spawnSync([
    "bash",
    "-c",
    `set -euo pipefail
${helperGuard}
${fn("finish")}
${assignment("version_news_ended")}
parts=one base_tree='' prompts_tree='' counted=3 expected=52 part_two_said=''
verdict=${JSON.stringify(verdict)} version_news_evidence=/evidence version_news_started=$SECONDS check=driver
trap finish EXIT
${body}`,
  ]);
  const out = result.stdout.toString();
  const err = result.stderr.toString();
  expectNoMissingHelpers(out, err);
  return { code: result.exitCode, signal: result.signalCode, out, err };
}

// The line finish prints for a run that ended before either of the script's ends with status 0.
const interrupted =
  /^approved version news e2e: FAIL at driver after \d+s: the run ended before its last check, by a signal or an exit of its own \(assertions: 3\/52; part one alone, part two did not run, evidence: \/evidence\)\n$/;

describe("the last line", () => {
  test("reads FAIL at the check a SIGTERM ended the run in, though bash gives the trap status 0", () => {
    const run = finishRun("kill -TERM $$\nsleep 5\nversion_news_ended=1");
    expect(run.out).toBe("");
    expect(run.err).toMatch(interrupted);
    expect(run.code === 143 || run.signal === "SIGTERM").toBe(true);
  });

  test("prints no control verdict for a control run a SIGTERM ended before its exit", () => {
    const run = finishRun("kill -TERM $$\nsleep 5", "control bites at a2 against a97cd593");
    expect(run.out).toBe("");
    expect(run.err).toMatch(interrupted);
  });

  test("reads FAIL for an exit 0 the script makes before either of its ends", () => {
    const run = finishRun("exit 0");
    expect(run.code).toBe(0);
    expect(run.out).toBe("");
    expect(run.err).toMatch(interrupted);
  });

  test("reads PASS for a run that reached its end and exits 0", () => {
    const run = finishRun("version_news_ended=1\nexit 0");
    expect(run.code).toBe(0);
    expect(run.err).toBe("");
    expect(run.out).toMatch(
      /^approved version news e2e: PASS in \d+s \(assertions: 3\/52; part one alone, part two did not run\)\n$/
    );
  });
});
