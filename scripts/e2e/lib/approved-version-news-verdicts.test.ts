import { describe, expect, test } from "bun:test";
import { chmodSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  temporaryDirectories,
  temporaryDirectory,
} from "@legion/test-fixtures/temporary-directories";
import {
  a4,
  controlFailure,
  driverTest,
  failed,
  fullPlan,
  off,
  passed,
  plan,
  runLine,
  runsOf,
  script,
} from "./approved-version-news-fixture";
import { stopped } from "./live-daemon-proof-fixture";
import {
  expectNoMissingHelpers,
  nameMissingHelpers,
  scriptAssignments,
  scriptFunctions,
} from "./script-functions";

// approved-version-news.sh's negative control, its plan of part two and its last line, taken from
// the script by name and run over driver output written as each driver prints it. Part one's
// negative control (control_verdict) bites only when A2's one-approval check fails in its own
// words after the step's five setup checks: never on the line the driver prints when its harness
// fails, never on a killed run. Part two's steps, their arm and the count of runs are the
// driver's plan, which the script reads and keeps no copy of: its count of each step
// (part_two_step) reads the arm the plan makes a step in. That count counts only runs that
// reached the step's assertion: a run that stopped at its setup is named and not counted, and is
// no pass. The last line (finish, the script's EXIT trap, through lib/run-verdict.sh) reads PASS,
// MEASURED or the control's verdict only for a run that reached one of the script's ends: a
// signal ends a shell with its EXIT trap reading status 0, so a killed run must read FAIL. A run
// that picks part two's runs reads MEASURED, never PASS. approved-version-news-runs.test.ts drives
// the script's runs of part one and of both parts whole, so its own end mark is the one under test.

const assignment = scriptAssignments(script);
const fn = scriptFunctions(script);
const note = scriptFunctions(join(import.meta.dir, "live-daemon-proof.sh"))("note");
const fail = scriptFunctions(join(import.meta.dir, "live-daemon-proof.sh"))("fail");
const helperGuard = nameMissingHelpers(script);
const dir = temporaryDirectory("approved-version-news-verdicts-test");
const scratch = temporaryDirectories("approved-version-news-verdicts-jj");

let files = 0;
/** A driver output file holding LINES, one a line; null writes no file at all. */
function proofLog(lines: string[] | null): string {
  const path = join(dir, `proof-${++files}.log`);
  if (lines !== null) writeFileSync(path, lines.map((line) => `${line}\n`).join(""));
  return path;
}

/** Runs BODY after the script's control settings, its control, plan and part-two functions and
 * the lib's note and fail, in its plan check, over the driver's plan of a full run at this change
 * (fullPlan) with no pick: BODY sets another plan or a pick. */
function controls(body: string) {
  const result = Bun.spawnSync([
    "bash",
    "-c",
    `set -euo pipefail
${helperGuard}
${note}
${fail}
${assignment("control_step")}
${assignment("control_setup_checks")}
${assignment("control_failure")}
${assignment("part_two_minutes")}
${assignment("part_two_setup_minutes")}
${fn("control_verdict")}
${fn("plan_words")}
${fn("plan_step")}
${fn("plan_runs")}
${fn("part_two_deadline")}
${fn("pick_said")}
${fn("part_two_step")}
check=plan full_plan=${quoted(fullPlan)} part_two_plan=${quoted(fullPlan)} pick_step='' pick_arm='' pick_runs=''
${body}`,
  ]);
  const out = result.stdout.toString();
  const err = result.stderr.toString();
  expectNoMissingHelpers(out, err);
  return { code: result.exitCode, out, err };
}

const a2 = "a2-the-reviewer-is-told-the-changed-sections";
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
const ownFailure = controlFailure(a2);
const goFailure = [
  `    approved_version_news_part_one_steps_live_test.go:147: ${a2}: waited 2m0s for a task queued for the reviewer of AVQ1-1 behind its open turn`,
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

/** TEXT as one single-quoted word of bash. */
const quoted = (text: string) => `'${text.replaceAll("'", `'\\''`)}'`;
const setupFailed =
  "SETUP: the reviewer of AS1-2 did not end its start turn within the run's bound";
const harnessFailed = "HARNESS: the harness failed; its log is above";

/** part_two_step over the lines LINES of the step named WORD on the last line, after PICK, the
 * plan and the pick the script's plan check and arguments check leave ("" for a full run's): the
 * step's part of the last line, the head runs that missed, and the notes it printed. */
function partTwoStep(word: string, lines: string[], pick = "") {
  const proof = proofLog([`=== RUN   TestRealSessionsTellAndAnswerANewVersionLive`, ...lines]);
  const run = controls(`${pick}
part_two_step ${JSON.stringify(proof)} ${word}
printf 'said=%s\\nmissed=%s\\n' "$step_said" "$head_missed"`);
  expect(run.err).toBe("");
  const said = /^said=(.*)$/m.exec(run.out)?.[1];
  const missed = /^missed=(.*)$/m.exec(run.out)?.[1];
  return { said, missed, notes: run.out };
}

describe("part two's count of each step", () => {
  test("counts only the runs that reached the assertion, and names each run it did not count", () => {
    const lines = [
      runLine(off, "head", 1, passed),
      runLine(off, "head", 2, setupFailed),
      runLine(off, "head", 3, harnessFailed),
    ];
    const step = partTwoStep("off", lines);
    expect(step.said).toBe("off head 1/1 with 2 not counted");
    expect(step.missed).toBe(`; ${lines[1]}; ${lines[2]}`);
    expect(step.notes).toContain(
      `   ${off} head: 1 of 1 runs that reached the assertion passed; 2 not counted (SETUP or HARNESS)\n`
    );
    expect(step.notes).toContain(`   not counted: ${lines[1]}\n`);
    expect(step.notes).toContain(`   not counted: ${lines[2]}\n`);
  });

  test("names a run that stopped at its setup, which is no pass, and each run that did not run", () => {
    // The driver stops at the first run that does not pass, so the rest never run.
    const step = partTwoStep("off", [
      runLine(off, "head", 1, passed),
      runLine(off, "head", 2, setupFailed),
    ]);
    expect(step.said).toBe("off head 1/1 with 1 not counted");
    expect(step.missed).toBe(
      `; off: 2 of 3 head runs ran; ${runLine(off, "head", 2, setupFailed)}`
    );
  });

  // The plan makes each step in the head arm alone, so a line of another arm, such as the
  // retired base arm's, is not read: a FAIL there would count for nothing.
  test("reads no line of an arm the plan does not make the step in", () => {
    const step = partTwoStep("a4", [
      ...runsOf(a4, "head", passed, passed, passed),
      runLine(a4, "base", 1, failed),
    ]);
    expect(step.said).toBe("a4 head 3/3");
    expect(step.missed).toBe("");
    expect(step.notes).not.toContain(`${a4} base`);
  });

  // A pick of one step and a count of runs, the command that measures that step: the arm is
  // counted against the pick's own count of runs, and each run that did not pass is named.
  test("counts a pick of one step against its own count of runs", () => {
    const outcomes = [passed, failed, failed, setupFailed, passed];
    const lines = runsOf(a4, "head", ...outcomes, failed, failed, passed, failed, failed);
    const step = partTwoStep(
      "a4",
      lines,
      `pick_runs=10 part_two_plan=${quoted(plan([["a4", a4, "head"]], 10))}`
    );
    expect(step.said).toBe("a4 head 3/9 with 1 not counted");
    expect(step.missed).toBe(
      lines
        .filter((line) => !line.includes(": PASS "))
        .map((line) => `; ${line}`)
        .join("")
    );
    expect(step.notes).toContain(
      `   ${a4} head: 3 of 9 runs that reached the assertion passed; 1 not counted (SETUP or HARNESS)\n`
    );
  });
});

/** part_two_deadline after PICK, the plan of a pick ("" for a full run's). */
function partTwoDeadline(pick = "") {
  const run = controls(`${pick}
part_two_deadline`);
  expect(run.err).toBe("");
  return run.out.trim();
}

// The driver's deadline counts the runs its plan makes: nine minutes a run and twenty more. A full
// run makes each of its three steps three times, 9 runs, and a pick of one step counts that
// step's runs alone.
describe("part two's deadline", () => {
  test("counts each run of a full run's plan", () => {
    expect(partTwoDeadline()).toBe(`${9 * 9 + 20}m`);
  });

  test("counts the runs of a pick of one step", () => {
    expect(partTwoDeadline(`part_two_plan=${quoted(plan([["a4", a4, "head"]], 10))}`)).toBe(
      `${10 * 9 + 20}m`
    );
  });
});

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

const transactSeam =
  "packages/daemon/internal/daemon/approved_version_news_control_transact_live_test.go";
const taskSeam = "packages/daemon/internal/daemon/approved_version_news_control_task_live_test.go";
const contractSeam =
  "packages/daemon/internal/daemon/approved_version_news_control_contract_live_test.go";
// The checkout the script's own root names, whose scripts/e2e/lib holds the Go of each seam that
// control_compat copies.
const checkout = join(import.meta.dir, "../../..");

/** Runs control_compat over a scratch base tree whose package daemon holds three Go files, the
 * first defining seedTransact where TRANSACT says so, the second recordTaskAt where TASK says so
 * and the third versionNewsPluginContract where CONTRACT says so, and answers the tree, the run's
 * output and its exit. */
function controlCompat(transact: boolean, task: boolean, contract: boolean) {
  const tree = scratch();
  const daemonDir = join(tree, "packages/daemon/internal/daemon");
  mkdirSync(daemonDir, { recursive: true });
  writeFileSync(
    join(daemonDir, "store_test.go"),
    `package daemon\n\n${transact ? "func seedTransact() error { return nil }\n" : "func seed() {}\n"}`
  );
  writeFileSync(
    join(daemonDir, "seed_test.go"),
    `package daemon\n\n${task ? "func recordTaskAt() error { return nil }\n" : "func startRoles() {}\n"}`
  );
  writeFileSync(
    join(daemonDir, "claims_test.go"),
    `package daemon\n\n${contract ? "func versionNewsPluginContract() int { return 0 }\n" : "func registerClaims() {}\n"}`
  );
  const result = Bun.spawnSync([
    Bun.which("bash") ?? "bash",
    "-c",
    `set -euo pipefail
${helperGuard}
check=base-tree root=${JSON.stringify(checkout)}
${note}
${fail}
${fn("control_compat")}
control_compat ${JSON.stringify(tree)}`,
  ]);
  const out = result.stdout.toString();
  const err = result.stderr.toString();
  expectNoMissingHelpers(out, err);
  return { tree, out, err, code: result.exitCode };
}

// Part one's control builds this checkout's part-one driver in a scratch copy of the base, and
// the driver calls three seams an older base lacks: seedTransact and recordTaskAt, which its seed
// calls, and versionNewsPluginContract, the contract its claims register with. The control copies
// the base's version of each, a test file of package daemon under //go:build e2e that
// scripts/e2e/lib holds as a .go.txt file, where the base tree defines no such function, and
// writes no file where it does; its transcript names each file it writes and each seam the base
// already defines.
describe("the control's seam files", () => {
  test("are written where the base defines no seam", () => {
    const run = controlCompat(false, false, false);
    expect(run.code).toBe(0);
    expect(run.err).toBe("");
    const transact = readFileSync(join(run.tree, transactSeam), "utf8");
    expect(transact).toBe(
      readFileSync(join(import.meta.dir, "approved-version-news-control-transact.go.txt"), "utf8")
    );
    expect(transact.startsWith("//go:build e2e\n")).toBe(true);
    expect(transact).toContain("\npackage daemon\n");
    expect(transact).toContain(
      "\nfunc seedTransact(ctx context.Context, pool *store.Pool, write func(ctx context.Context, tx pgx.Tx) error) error {\n" +
        "\tctx, committed := intake.WithCommitHooks(ctx)\n"
    );
    expect(transact.endsWith("\n\tcommitted()\n\treturn nil\n}\n")).toBe(true);
    const task = readFileSync(join(run.tree, taskSeam), "utf8");
    expect(task).toBe(
      readFileSync(join(import.meta.dir, "approved-version-news-control-task.go.txt"), "utf8")
    );
    expect(task.startsWith("//go:build e2e\n")).toBe(true);
    expect(task).toContain("\npackage daemon\n");
    expect(task).toContain(
      "\nfunc recordTaskAt(context.Context, pgx.Tx, record.Store, record.Issue) error { return nil }\n"
    );
    const contract = readFileSync(join(run.tree, contractSeam), "utf8");
    expect(contract).toBe(
      readFileSync(join(import.meta.dir, "approved-version-news-control-contract.go.txt"), "utf8")
    );
    expect(contract.startsWith("//go:build e2e\n")).toBe(true);
    expect(contract).toContain("\npackage daemon\n");
    expect(contract).toContain(
      "\nfunc versionNewsPluginContract() int { return api.DaemonAPIVersion }\n"
    );
    expect(run.out).toBe(
      `   wrote ${join(run.tree, transactSeam)}: the base's package daemon defines no seedTransact, which part one's driver takes its transaction through\n` +
        `   wrote ${join(run.tree, taskSeam)}: the base's package daemon defines no recordTaskAt, which part one's driver calls\n` +
        `   wrote ${join(run.tree, contractSeam)}: the base's package daemon defines no versionNewsPluginContract, which part one's driver registers its claims' contract through\n`
    );
  });

  test("are not written where the base defines all three seams", () => {
    const run = controlCompat(true, true, true);
    expect(run.code).toBe(0);
    expect(run.err).toBe("");
    expect(existsSync(join(run.tree, transactSeam))).toBe(false);
    expect(existsSync(join(run.tree, taskSeam))).toBe(false);
    expect(existsSync(join(run.tree, contractSeam))).toBe(false);
    expect(run.out).toBe(
      "   the base's package daemon defines seedTransact, so the control writes no file for it\n" +
        "   the base's package daemon defines recordTaskAt, so the control writes no file for it\n" +
        "   the base's package daemon defines versionNewsPluginContract, so the control writes no file for it\n"
    );
  });

  test("are written for the one seam the base lacks", () => {
    const seams = [transactSeam, taskSeam, contractSeam];
    for (const lacking of seams.keys()) {
      const run = controlCompat(lacking !== 0, lacking !== 1, lacking !== 2);
      expect(run.code).toBe(0);
      for (const [place, seam] of seams.entries()) {
        expect(existsSync(join(run.tree, seam))).toBe(place === lacking);
      }
    }
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
parts=one base_tree='' prompts_tree='' plan_tree='' gateway_tree='' gateway_refresher='' counted=3 expected=52 part_two_said=''
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
