import { describe, expect, test } from "bun:test";
import { chmodSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import {
  temporaryDirectories,
  temporaryDirectory,
} from "@legion/test-fixtures/temporary-directories";
import {
  inOwnGroup,
  type KillPoint,
  liveDaemonStandIns,
  type StopSignal,
  stopped,
} from "./live-daemon-proof-fixture";
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
// script's EXIT trap, through lib/run-verdict.sh) reads PASS or the control's verdict only for a
// run that reached one of the script's ends: a signal ends a shell with its EXIT trap reading
// status 0, so a killed run must read FAIL. The script's run of part one is driven whole, so its
// own end mark is the one under test.

const script = join(import.meta.dir, "..", "approved-version-news.sh");
const assignment = scriptAssignments(script);
const fn = scriptFunctions(script);
const note = scriptFunctions(join(import.meta.dir, "live-daemon-proof.sh"))("note");
const helperGuard = nameMissingHelpers(script);
const dir = temporaryDirectory("approved-version-news-verdicts-test");
const scratch = temporaryDirectories("approved-version-news-run-test");
const withoutSetsid = skipWithout("setsid");

let files = 0;
/** A driver output file holding LINES, one a line; null writes no file at all. */
function proofLog(lines: string[] | null): string {
  const path = join(dir, `proof-${++files}.log`);
  if (lines !== null) writeFileSync(path, lines.map((line) => `${line}\n`).join(""));
  return path;
}

/** Runs BODY after the script's control settings, its control and part-two functions and the
 * lib's note. */
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
${assignment("part_two_steps")}
${assignment("part_two_arm_pick")}
${assignment("part_two_runs")}
${assignment("part_two_minutes")}
${assignment("part_two_setup_minutes")}
${assignment("head_only")}
${fn("head_only_why")}
${fn("control_verdict")}
${fn("step_arms")}
${fn("part_two_deadline")}
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
const off = "off-the-architect-tells-the-holders";
const answers = "answers-a-stale-reviewer-moves-back-a-current-one-comments";
// One run's line as part two's driver prints it at the run's end.
const runLine = (step: string, arm: string, n: number, outcome: string) =>
  `run ${step} ${arm} ${n}: ${outcome} in 140s, 31 model calls`;
const passed = "PASS";
const failed =
  "FAIL: the reviewer of AS1-2 was sent no Envoy message naming version 2 and the change";
const setupFailed =
  "SETUP: the reviewer of AS1-2 did not end its start turn within the run's bound";
const harnessFailed = "HARNESS: the harness failed; its log is above";
// Each step whose base arm is retired, its word, and what the last line says in its place, the
// measurement that retired it.
const retired = [
  {
    step: a4,
    word: "a4",
    headOnly: "(head only: the base arm passed 10 of 10 at 578c6abb, main meets R4 since #4203)",
  },
  {
    step: answers,
    word: "answers",
    headOnly:
      "(head only: the base arm passed 10 of 10 at 578c6abb, the daemon's news task states the rule in both arms)",
  },
];

/** part_two_step over STEP's lines LINES, named WORD on the last line, after PICK, the settings
 * the arguments check makes of a pick ("" for none): the step's part of the last line, the head
 * runs that missed, and the notes it printed. */
function partTwoStep(step: string, word: string, lines: string[], pick = "") {
  const proof = proofLog([`=== RUN   TestRealSessionsTellAndAnswerANewVersionLive`, ...lines]);
  const run = controls(`${pick}
part_two_step ${JSON.stringify(proof)} ${step} ${word}
printf 'said=%s\\nmissed=%s\\n' "$step_said" "$head_missed"`);
  expect(run.err).toBe("");
  const said = /^said=(.*)$/m.exec(run.out)?.[1];
  const missed = /^missed=(.*)$/m.exec(run.out)?.[1];
  return { said, missed, notes: run.out };
}

const headPassed = (step: string) => [1, 2, 3].map((n) => runLine(step, "head", n, passed));

describe("part two's count of each arm", () => {
  test("does not count a base run that stopped at its setup, and does not bite on it", () => {
    const step = partTwoStep(off, "off", [
      ...headPassed(off),
      runLine(off, "base", 1, passed),
      runLine(off, "base", 2, passed),
      runLine(off, "base", 3, setupFailed),
    ]);
    expect(step.said).toBe(
      "off head 3/3, base 2/2 with 1 not counted (no control: the base arm passed every run that reached its assertion)"
    );
    expect(step.missed).toBe("");
    expect(step.notes).toContain(`   not counted: ${runLine(off, "base", 3, setupFailed)}\n`);
  });

  test("bites on a base run that failed at its assertion, beside one not counted", () => {
    const step = partTwoStep(off, "off", [
      ...headPassed(off),
      runLine(off, "base", 1, passed),
      runLine(off, "base", 2, failed),
      runLine(off, "base", 3, setupFailed),
    ]);
    expect(step.said).toBe("off head 3/3, base 1/2 with 1 not counted (control bites)");
    expect(step.missed).toBe("");
  });

  test("is no control when every base run stopped before its assertion", () => {
    const step = partTwoStep(off, "off", [
      ...headPassed(off),
      runLine(off, "base", 1, setupFailed),
      runLine(off, "base", 2, setupFailed),
      runLine(off, "base", 3, harnessFailed),
    ]);
    expect(step.said).toBe(
      "off head 3/3, base 0/0 with 3 not counted (no control: no base run reached its assertion)"
    );
  });

  test("names a head run that stopped at its setup, which is no pass", () => {
    // The driver stops at the first head run that does not pass, so the base arm never runs.
    const step = partTwoStep(off, "off", [
      runLine(off, "head", 1, passed),
      runLine(off, "head", 2, setupFailed),
    ]);
    expect(step.said).toBe("off head 1/1 with 1 not counted, base 0/0 (base did not run)");
    expect(step.missed).toBe(
      `; off: 2 of 3 head runs ran; ${runLine(off, "head", 2, setupFailed)}`
    );
  });

  for (const { step: retiredStep, word, headOnly } of retired) {
    // A run with no arm picked makes the step in the head arm alone: the last line names the
    // measurement that retired its base arm, and a base line the driver printed for it is not
    // counted, so a base FAIL there would not make a control bite that the full run no longer has.
    test(`counts the head arm of ${word} alone in a full run, and names why its base arm is retired`, () => {
      const step = partTwoStep(retiredStep, word, [
        ...headPassed(retiredStep),
        runLine(retiredStep, "base", 1, failed),
      ]);
      expect(step.said).toBe(`${word} head 3/3 ${headOnly}`);
      expect(step.missed).toBe("");
      expect(step.notes).not.toContain(`${retiredStep} base:`);
    });

    test(`still owes every head run of ${word} in a full run`, () => {
      const step = partTwoStep(retiredStep, word, [
        runLine(retiredStep, "head", 1, passed),
        runLine(retiredStep, "head", 2, setupFailed),
      ]);
      expect(step.said).toBe(`${word} head 1/1 with 1 not counted ${headOnly}`);
      expect(step.missed).toBe(
        `; ${word}: 2 of 3 head runs ran; ${runLine(retiredStep, "head", 2, setupFailed)}`
      );
    });

    // A pick of the retired base arm alone, the command that repeats the measurement: the arm is
    // counted against the pick's own count of runs, no head run is owed, and a base FAIL still
    // bites.
    test(`counts a pick of the base arm of ${word} alone against its own count of runs`, () => {
      const outcomes = [passed, failed, failed, setupFailed, passed];
      const runs = [...outcomes, failed, failed, passed, failed, failed];
      const step = partTwoStep(
        retiredStep,
        word,
        runs.map((outcome, i) => runLine(retiredStep, "base", i + 1, outcome)),
        "part_two_arm_pick=base part_two_runs=10"
      );
      expect(step.said).toBe(`${word} base 3/9 with 1 not counted (control bites)`);
      expect(step.missed).toBe("");
      expect(step.notes).toContain(
        `   ${retiredStep} base: 3 of 9 runs that reached the assertion passed; 1 not counted (SETUP or HARNESS)\n`
      );
    });

    // "head only" is the step in a run that picks no arm, where its base arm's retirement is the
    // reason no base ran. A pick of the head arm leaves out its base arm as it does every step's,
    // and says so in the same words.
    test(`is no control for ${word} too when the pick names the head arm`, () => {
      const step = partTwoStep(
        retiredStep,
        word,
        headPassed(retiredStep),
        "part_two_arm_pick=head"
      );
      expect(step.said).toBe(`${word} head 3/3 (no control: the pick leaves the base arm out)`);
      expect(step.missed).toBe("");
    });
  }

  test("is no control when the pick leaves the base arm out", () => {
    const step = partTwoStep(off, "off", headPassed(off), "part_two_arm_pick=head");
    expect(step.said).toBe("off head 3/3 (no control: the pick leaves the base arm out)");
    expect(step.missed).toBe("");
  });
});

/** part_two_deadline after PICK, the settings the arguments check makes of a pick ("" for none). */
function partTwoDeadline(pick = "") {
  const run = controls(`${pick}
part_two_deadline`);
  expect(run.err).toBe("");
  return run.out.trim();
}

// The driver's deadline counts the runs it makes: nine minutes a run and twenty more. A full run
// makes a4 and answers three times each in the head arm and off three times in each arm, 12 runs;
// one that counted a retired base arm too would allow 15 or 18. A pick of a retired base arm
// counts that arm alone.
describe("part two's deadline", () => {
  test("counts no base run of a4 or answers in a full run", () => {
    expect(partTwoDeadline()).toBe(`${12 * 9 + 20}m`);
  });

  for (const [at, word] of [
    [0, "a4"],
    [2, "answers"],
  ] as const) {
    test(`counts the runs of a pick of the base arm of ${word}`, () => {
      expect(
        partTwoDeadline(
          `part_two_steps=("\${part_two_steps[${at}]}") part_two_arm_pick=base part_two_runs=10`
        )
      ).toBe(`${10 * 9 + 20}m`);
    });
  }
});

const fail = scriptFunctions(join(import.meta.dir, "live-daemon-proof.sh"))("fail");

/** A scratch directory holding a jj that prints SAID for `jj --version`. */
function fakeJJ(said: string) {
  const bin = scratch();
  writeFileSync(join(bin, "jj"), `#!/bin/sh\nprintf '%s\\n' ${JSON.stringify(said)}\n`);
  chmodSync(join(bin, "jj"), 0o755);
  return bin;
}

/** check_jj with PATH holding a jj that prints SAID for `jj --version`, or no jj at all for null,
 * and LEGION_JJ_PATH naming a jj that prints OVERRIDE where it is given: its exit status and what
 * it printed on stderr. */
function checkJJ(said: string | null, override?: string) {
  const bin = said === null ? scratch() : fakeJJ(said);
  const env: Record<string, string> = { PATH: `${bin}:/usr/bin:/bin` };
  if (override !== undefined) {
    env.LEGION_JJ_PATH = join(fakeJJ(override), "jj");
  }
  const result = Bun.spawnSync(
    [
      Bun.which("bash") ?? "bash",
      "-c",
      `set -euo pipefail
${helperGuard}
check=arguments
${fail}
${fn("check_jj")}
check_jj`,
    ],
    { env }
  );
  const err = result.stderr.toString();
  expectNoMissingHelpers(result.stdout.toString(), err);
  return { code: result.exitCode, err };
}

// Both parts boot the whole daemon, whose boot refuses a jj below 0.38, so the arguments check
// fails the run before it builds anything on a jj below that, on one whose version it cannot read
// and on none, and takes 0.38 and every later release, a build's suffix included. It reads the jj
// the daemon runs: LEGION_JJ_PATH where it is set, and the jj on PATH otherwise.
describe("the arguments check's jj", () => {
  for (const said of ["jj 0.38.0", "jj 0.45.1-sami.2026-11-09-abc123", "jj 1.0.0"]) {
    test(`takes ${said}`, () => {
      expect(checkJJ(said)).toEqual({ code: 0, err: "" });
    });
  }

  test("fails on jj 0.37", () => {
    expect(checkJJ("jj 0.37.9")).toEqual({
      code: 1,
      err: "FAIL arguments: jj is jj 0.37, below 0.38, the oldest jj the daemon's boot starts with\n",
    });
  });

  test("takes the jj LEGION_JJ_PATH names where PATH holds none", () => {
    expect(checkJJ(null, "jj 0.38.0")).toEqual({ code: 0, err: "" });
  });

  test("fails on the jj LEGION_JJ_PATH names, whatever the jj on PATH", () => {
    const run = checkJJ("jj 0.45.1", "jj 0.37.9");
    expect(run.code).toBe(1);
    expect(run.err).toContain("is jj 0.37, below 0.38");
  });

  test("fails on a version it cannot read", () => {
    const run = checkJJ("jujutsu, a build of no release");
    expect(run.code).toBe(1);
    expect(run.err).toContain("names no release");
  });

  test("fails where PATH holds no jj", () => {
    const run = checkJJ(null);
    expect(run.code).toBe(1);
    expect(run.err).toContain("FAIL arguments: jj --version failed");
  });
});

/** Runs BODY in a bash whose EXIT trap is the script's finish, with the real lib/run-verdict.sh,
 * over the state finish reads as a run of part one alone leaves it standing in its `driver` check
 * with 3 of its 52 checks counted; VERDICT is the control's, "" for none. BODY calls run_end where
 * the script reaches one of its ends; the rows of "the script's run of part one" below drive the
 * script's own run_end. */
function finishRun(body: string, verdict = "") {
  const result = Bun.spawnSync([
    "bash",
    "-c",
    `set -euo pipefail
${helperGuard}
. ${JSON.stringify(join(import.meta.dir, "run-verdict.sh"))}
${fn("finish")}
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

// What finish prints for a run that ended before either of the script's ends with status 0:
// lib/run-verdict.sh's line that says so (stopped), then the FAIL line.
const interrupted = new RegExp(
  `^${stopped}\\napproved version news e2e: FAIL at driver after \\d+s \\(assertions: 3/52; part one alone, part two did not run, evidence: /evidence\\)\\n$`
);

describe("the last line", () => {
  test("prints no control verdict for a control run a SIGTERM ended before its exit", () => {
    const run = finishRun(
      "kill -TERM $$\nsleep 5\nrun_end",
      "control bites at a2 against a97cd593"
    );
    expect(run.out).toBe("");
    expect(run.err).toMatch(interrupted);
    expect(run.code === 143 || run.signal === "SIGTERM").toBe(true);
  });

  test("prints the control's verdict for a control run that reached its end", () => {
    const run = finishRun("run_end\nexit 0", "control bites at a2 against a97cd593");
    expect(run.code).toBe(0);
    expect(run.err).toBe("");
    expect(run.out).toMatch(
      /^approved version news e2e: control bites at a2 against a97cd593 in \d+s \(assertions: 3\/52; part one alone, part two did not run, evidence: \/evidence\)\n$/
    );
  });

  test("reads FAIL, and exits 1, for an exit 0 the script makes before either of its ends", () => {
    const run = finishRun("exit 0");
    expect(run.code).toBe(1);
    expect(run.out).toBe("");
    expect(run.err).toMatch(interrupted);
  });
});

// Part one's steps and the checks each makes, read from the script's own part_one_checks, so the
// stand-in driver below prints what the script counts.
const partOneChecks = assignment("part_one_checks")
  .slice("part_one_checks=(".length, -")".length)
  .trim()
  .split(/\s+/)
  .map((entry) => {
    const [step, checks] = entry.split(":");
    return { step, checks: Number(checks) };
  });
const expectedChecks = partOneChecks.reduce((sum, { checks }) => sum + checks, 0);
// What part one's driver prints when every step held: each step's checks and its ok line.
const partOneDriver = [
  `=== RUN   ${driverTest}`,
  ...partOneChecks.flatMap(({ step, checks }) => [
    `== ${step}`,
    ...Array.from({ length: checks }, (_, n) => `   assert ${step}: check ${n + 1} held`),
    `ok ${step} (1s)`,
  ]),
  `--- PASS: ${driverTest} (8.00s)`,
  "PASS",
];

/** Runs the script with --part one over lib/live-daemon-proof-fixture.ts's stand-ins, stopped at
 * KILL when given, in a process group of its own. Every line, its stderr's included, is in `out`:
 * the script's transcript sends both there. */
function partOneRun(kill?: { at: KillPoint; signal: StopSignal }) {
  const dir = scratch();
  const env = liveDaemonStandIns(dir, { driverOutput: partOneDriver, kill });
  env.APPROVED_VERSION_NEWS_EVIDENCE_DIR = join(dir, "evidence");
  return inOwnGroup(["bash", script, "--part", "one"], env);
}

// The script's run and its two ends, driven whole: part one's run is lib/live-daemon-proof.sh's
// over stand-ins, and the script's own run_end, its own finish and the real lib print the last
// line. A run a signal ends in part one reads FAIL at part-one; moving the script's run_end above
// part one makes it print PASS. A signal to the group ends part one's subshell too, and the two
// shells print their FAIL lines in either order.
describe.skipIf(withoutSetsid)("the script's run of part one", () => {
  test("reads each step of part_one_checks and the checks it makes", () => {
    expect(partOneChecks.length).toBeGreaterThan(0);
    for (const { step, checks } of partOneChecks) {
      expect(step).toMatch(/^[a-z0-9-]+$/);
      expect(checks).toBeGreaterThan(0);
    }
  });

  for (const signal of ["TERM", "HUP", "INT"] as const) {
    test(`reads FAIL at part-one for a run a group SIG${signal} ended in part one's driver`, () => {
      const run = partOneRun({ at: "proof", signal });
      expect(run.out).toContain("approved version news part one e2e: FAIL at proof after ");
      expect(run.out).not.toContain("e2e: PASS");
      expect(run.out).not.toContain("control bites");
      expect(run.out).toMatch(
        new RegExp(
          `\\napproved version news e2e: FAIL at (part-one|assertions) after \\d+s \\(assertions: \\d+/${expectedChecks}; part one alone, part two did not run, evidence: .*\\)\\n`
        )
      );
      expect(run.code === 0 && run.signal === null).toBe(false);
    });
  }

  test("reads FAIL at part-one for a run a group SIGTERM ended in part one's setup", () => {
    const run = partOneRun({ at: "build", signal: "TERM" });
    expect(run.out).toContain("approved version news part one e2e: FAIL at driver after ");
    expect(run.out).not.toContain("e2e: PASS");
    expect(run.out).toContain(`${stopped}\n`);
    expect(run.out).toMatch(
      new RegExp(
        `\\napproved version news e2e: FAIL at part-one after \\d+s \\(assertions: 0/${expectedChecks}; part one alone, part two did not run, evidence: .*\\)\\n`
      )
    );
    expect(run.code === 143 || run.signal === "SIGTERM").toBe(true);
  });

  test("reads PASS for a run of part one that reached its end", () => {
    const run = partOneRun();
    expect(run.code).toBe(0);
    expect(run.out).toContain("approved version news part one e2e: PASS in ");
    expect(run.out).not.toContain("FAIL");
    expect(run.out).toMatch(
      new RegExp(
        `\\napproved version news e2e: PASS in \\d+s \\(assertions: ${expectedChecks}/${expectedChecks}; part one alone, part two did not run\\)\\n$`
      )
    );
  });
});
