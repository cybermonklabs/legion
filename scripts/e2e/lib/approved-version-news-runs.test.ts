import { expect, test } from "bun:test";
import { chmodSync, copyFileSync, mkdirSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { hostGate } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  a4,
  answers,
  driverTest,
  failed,
  fullPlan,
  off,
  passed,
  plan,
  retired,
  runsOf,
  script,
} from "./approved-version-news-fixture";
import {
  inOwnGroup,
  type KillPoint,
  liveDaemonStandIns,
  type StopSignal,
  stopped,
} from "./live-daemon-proof-fixture";
import { scriptAssignments, sourcedLibraries } from "./script-functions";

// approved-version-news.sh's runs of part one and of both parts, driven whole over
// lib/live-daemon-proof-fixture.ts's stand-ins, so the script's own end mark is the one under
// test: its run_end, its finish and the real lib/run-verdict.sh print the last line.
// approved-version-news-verdicts.test.ts runs the script's controls, its plan check and its last
// line by name.

const assignment = scriptAssignments(script);
const scratch = temporaryDirectories("approved-version-news-run-test");
// Read once, at module scope: a host without setsid, or without git and tar, skips the rows that
// need them naming what it lacks, and under GITHUB_ACTIONS fails each by its own name (hostGate).
const setsidHost = hostGate({ programs: ["setsid"] });
const gitHost = hostGate({ programs: ["git", "tar"] });

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
setsidHost.describe("the script's run of part one", () => {
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

const sessionsTest = "TestRealSessionsTellAndAnswerANewVersionLive";
// The model key the run's key file holds: its first 8 characters are in nothing the run prints.
const modelKey = "Kq81mvXz-a-model-key-of-the-fixture";

/**
 * Runs the script, both parts, with the variables PICK sets, over lib/live-daemon-proof-fixture.ts's
 * stand-ins, from a scratch checkout of its own: the script and the libraries it sources at the
 * paths they have here, in a git repository whose one commit holds the base's prompt files and is
 * origin/main, so that commit is the base. The stand-in driver prints part one's output for part
 * one's test; under -part-two-plan it prints PLANS.full when the run picks nothing and PLANS.pick
 * when it picks; and for part two's test it prints PART_TWO's run lines, the count of model calls
 * and the test's ENDED, a FAIL for a driver that stopped at a head run, with that run's status.
 * Every line, its stderr's included, is in `out`, and the script's own last line is `last`.
 */
function bothPartsRun(
  pick: Record<string, string>,
  plans: { full: string; pick?: string },
  partTwo: string[],
  ended: "PASS" | "FAIL" = "PASS"
) {
  const dir = scratch();
  const checkout = join(dir, "checkout");
  for (const path of sourcedLibraries(script)) {
    const to = join(checkout, "scripts", "e2e", relative(dirname(script), path));
    mkdirSync(dirname(to), { recursive: true });
    copyFileSync(path, to);
  }
  for (const prompt of ["roles/reviewer.md", "go/worker-common.md"]) {
    const to = join(checkout, "packages", "daemon", "internal", "prompts", prompt);
    mkdirSync(dirname(to), { recursive: true });
    writeFileSync(to, "a base prompt\n");
  }
  const env = liveDaemonStandIns(dir, { driverOutput: partOneDriver });
  for (const name of [
    "APPROVED_VERSION_NEWS_STEP",
    "APPROVED_VERSION_NEWS_ARM",
    "APPROVED_VERSION_NEWS_RUNS",
    "GIT_DIR",
    "GIT_WORK_TREE",
    "GIT_INDEX_FILE",
  ]) {
    delete env[name];
  }
  for (const args of [
    ["init", "-q"],
    ["add", "."],
    ["commit", "-q", "--no-gpg-sign", "-m", "the base"],
    ["update-ref", "refs/remotes/origin/main", "HEAD"],
  ]) {
    const git = Bun.spawnSync(
      [
        "git",
        "-C",
        checkout,
        "-c",
        "user.name=fixture",
        "-c",
        "user.email=fixture@example.invalid",
        ...args,
      ],
      { env }
    );
    if (git.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${git.stderr.toString()}`);
  }
  writeFileSync(join(dir, "full-plan"), `${plans.full}\n`);
  writeFileSync(join(dir, "pick-plan"), `${plans.pick ?? plans.full}\n`);
  const output = [
    `=== RUN   ${sessionsTest}`,
    ...partTwo,
    `model calls: ${partTwo.length} in all, over ${partTwo.length} runs`,
    `--- ${ended}: ${sessionsTest} (100.00s)`,
    ended,
  ];
  writeFileSync(join(dir, "part-two-output"), output.map((line) => `${line}\n`).join(""));
  writeFileSync(
    join(dir, "driver"),
    `#!/usr/bin/env bash
case " $* " in
  *" -part-two-plan "*)
    if [ -n "\${APPROVED_VERSION_NEWS_STEP:-}\${APPROVED_VERSION_NEWS_ARM:-}\${APPROVED_VERSION_NEWS_RUNS:-}" ]; then
      cat "$LIVE_DAEMON_STAND_IN_DIR/pick-plan"
    else
      cat "$LIVE_DAEMON_STAND_IN_DIR/full-plan"
    fi
    echo PASS
    ;;
  *" ^${sessionsTest}$ "*)
    cat "$LIVE_DAEMON_STAND_IN_DIR/part-two-output"
    [ ${ended} = PASS ]
    ;;
  *) cat "$LIVE_DAEMON_STAND_IN_DIR/driver-output" ;;
esac
`
  );
  chmodSync(join(dir, "driver"), 0o755);
  writeFileSync(join(dir, "omp"), "#!/bin/sh\nexit 0\n");
  chmodSync(join(dir, "omp"), 0o755);
  writeFileSync(join(dir, "model-key"), `${modelKey}\n`);
  Object.assign(env, pick, {
    PATH: `${env.PATH}:${dirname(process.execPath)}`,
    LEGION_OMP_PATH: join(dir, "omp"),
    ANTHROPIC_BASE_URL: "http://127.0.0.1:1",
    APPROVED_VERSION_NEWS_MODEL_KEY_FILE: join(dir, "model-key"),
    APPROVED_VERSION_NEWS_EVIDENCE_DIR: join(dir, "evidence"),
  });
  const result = Bun.spawnSync(
    ["bash", join(checkout, "scripts", "e2e", "approved-version-news.sh")],
    {
      env,
    }
  );
  const out = result.stdout.toString() + result.stderr.toString();
  return { code: result.exitCode, out, last: out.trimEnd().split("\n").at(-1) ?? "" };
}

const a4Head = retired[0].headOnly;
const answersHead = retired[1].headOnly;
const noControl = "(no control: the base arm passed every run that reached its assertion)";
// The driver's plan of each pick below, as pickSessions makes it from the plan of a full run.
const basePlan = plan(
  [
    ["a4", a4, "base"],
    ["off", off, "base"],
    ["answers", answers, "base"],
  ],
  3
);
const onePlan = plan(
  [
    ["a4", a4, "head"],
    ["off", off, "head base"],
    ["answers", answers, "head"],
  ],
  1
);

// The script's run of both parts, driven whole over stand-ins: its plan check builds the one
// driver both parts run and reads the stand-in driver's plan, part one's and part two's runs are
// lib/live-daemon-proof.sh's, and the script's own finish and the real lib print the last line. A
// full run reads PASS. A run that sets any of
// APPROVED_VERSION_NEWS_ARM, APPROVED_VERSION_NEWS_STEP and APPROVED_VERSION_NEWS_RUNS is a
// measurement: its last line reads MEASURED, never PASS, names the pick, each step and arm of the
// full run's plan it left out and its count of runs where it is not three, and exits 0; a head run
// of the pick that did not pass still reads FAIL. The counts and the checks follow the driver's
// plan, and a pick the driver refuses is refused in its words.
gitHost.describe("the script's run of both parts", () => {
  test("reads PASS for a full run, which picks nothing", () => {
    const run = bothPartsRun({}, { full: fullPlan }, [
      ...runsOf(a4, "head", passed, passed, passed),
      ...runsOf(off, "head", passed, passed, passed),
      ...runsOf(off, "base", failed, failed, failed),
      ...runsOf(answers, "head", passed, passed, passed),
    ]);
    expect(run.code).toBe(0);
    expect(run.last).toMatch(/^approved version news e2e: PASS in \d+s \(/);
    expect(run.last).toEndWith(
      `(assertions: ${expectedChecks}/${expectedChecks}; part two: a4 head 3/3 ${a4Head}; off head 3/3, base 0/3 (control bites); answers head 3/3 ${answersHead}; model calls: 12)`
    );
    // The plan check's build is the run's one: each part's driver check takes it and builds none.
    expect(run.out.match(/^ {3}built .+\.test from /gm)).toHaveLength(1);
    expect(run.out.match(/^ {3}the caller built .+; this check builds nothing$/gm)).toHaveLength(2);
  }, 60_000);

  test("reads MEASURED, not PASS, for APPROVED_VERSION_NEWS_ARM=base, naming each head arm it left out", () => {
    const run = bothPartsRun(
      { APPROVED_VERSION_NEWS_ARM: "base" },
      { full: fullPlan, pick: basePlan },
      [
        ...runsOf(a4, "base", passed, passed, passed),
        ...runsOf(off, "base", failed, failed, failed),
        ...runsOf(answers, "base", passed, passed, passed),
      ]
    );
    expect(run.code).toBe(0);
    expect(run.last).toMatch(/^approved version news e2e: MEASURED in \d+s \(/);
    expect(run.last).toContain(
      `(assertions: ${expectedChecks}/${expectedChecks}; part two: a partial run picked by APPROVED_VERSION_NEWS_ARM=base, leaving out a4 head, off head, answers head: a4 base 3/3 ${noControl}; off base 0/3 (control bites); answers base 3/3 ${noControl}; model calls: 9, evidence: `
    );
  }, 60_000);

  test("reads MEASURED, not PASS, for APPROVED_VERSION_NEWS_STEP=a4, naming each step and arm it left out", () => {
    const run = bothPartsRun(
      { APPROVED_VERSION_NEWS_STEP: "a4" },
      { full: fullPlan, pick: plan([["a4", a4, "head"]], 3) },
      runsOf(a4, "head", passed, passed, passed)
    );
    expect(run.code).toBe(0);
    expect(run.last).toMatch(/^approved version news e2e: MEASURED in \d+s \(/);
    expect(run.last).toContain(
      `(assertions: ${expectedChecks}/${expectedChecks}; part two: a partial run picked by APPROVED_VERSION_NEWS_STEP=a4, leaving out off head, off base, answers head: a4 head 3/3 ${a4Head}; model calls: 3, evidence: `
    );
  }, 60_000);

  test("reads MEASURED, not PASS, for APPROVED_VERSION_NEWS_RUNS=1, naming its count of runs", () => {
    const run = bothPartsRun(
      { APPROVED_VERSION_NEWS_RUNS: "1" },
      { full: fullPlan, pick: onePlan },
      [
        ...runsOf(a4, "head", passed),
        ...runsOf(off, "head", passed),
        ...runsOf(off, "base", failed),
        ...runsOf(answers, "head", passed),
      ]
    );
    expect(run.code).toBe(0);
    expect(run.last).toMatch(/^approved version news e2e: MEASURED in \d+s \(/);
    expect(run.last).toContain(
      `(assertions: ${expectedChecks}/${expectedChecks}; part two: a partial run picked by APPROVED_VERSION_NEWS_RUNS=1, with 1 run of each arm in place of 3: a4 head 1/1 ${a4Head}; off head 1/1, base 0/1 (control bites); answers head 1/1 ${answersHead}; model calls: 4, evidence: `
    );
  }, 60_000);

  test("reads FAIL for a pick whose head run did not pass", () => {
    const run = bothPartsRun(
      { APPROVED_VERSION_NEWS_STEP: "off" },
      { full: fullPlan, pick: plan([["off", off, "head base"]], 3) },
      runsOf(off, "head", passed, failed),
      "FAIL"
    );
    expect(run.code).toBe(1);
    expect(run.out).not.toContain("approved version news e2e: MEASURED");
    expect(run.last).toMatch(/^approved version news e2e: FAIL at part-two after \d+s \(/);
    expect(run.last).toContain(
      "part two: a partial run picked by APPROVED_VERSION_NEWS_STEP=off, leaving out a4 head, answers head: off head 1/2, base 0/0 (base did not run); model calls: 2, evidence: "
    );
  }, 60_000);

  // The script keeps no table of part two's steps: a step the driver's plan adds is counted in
  // each arm the plan makes it in, and a plan that gives a4 its base arm back, while head_only
  // still holds the measurement that retired it, fails the run at the plan check, naming a4.
  test("counts each arm of a step the driver's plan adds", () => {
    const extra = "extra-a-step-the-driver-added";
    const withExtra = plan(
      [
        ["a4", a4, "head"],
        ["off", off, "head base"],
        ["answers", answers, "head"],
        ["extra", extra, "head base"],
      ],
      3
    );
    const run = bothPartsRun({}, { full: withExtra }, [
      ...runsOf(a4, "head", passed, passed, passed),
      ...runsOf(off, "head", passed, passed, passed),
      ...runsOf(off, "base", failed, failed, failed),
      ...runsOf(answers, "head", passed, passed, passed),
      ...runsOf(extra, "head", passed, passed, passed),
      ...runsOf(extra, "base", passed, failed, passed),
    ]);
    expect(run.code).toBe(0);
    expect(run.last).toMatch(/^approved version news e2e: PASS in \d+s \(/);
    expect(run.last).toContain(
      `; answers head 3/3 ${answersHead}; extra head 3/3, base 2/3 (control bites); model calls: 18)`
    );
  }, 60_000);

  test("fails at the plan check, naming a4, where the driver's plan gives a4 its base arm back", () => {
    const back = plan(
      [
        ["a4", a4, "head base"],
        ["off", off, "head base"],
        ["answers", answers, "head"],
      ],
      3
    );
    const run = bothPartsRun({}, { full: back }, [
      ...runsOf(a4, "head", passed, passed, passed),
      ...runsOf(a4, "base", failed, failed, failed),
    ]);
    expect(run.code).toBe(1);
    expect(run.out).toContain(
      "FAIL plan: the driver's plan of a full run makes a4 in both arms (head base), and head_only holds a measurement that retired its base arm\n"
    );
    expect(run.last).toMatch(/^approved version news e2e: FAIL at plan after \d+s \(/);
  }, 60_000);

  test("refuses with exit 2, in the driver's words, a pick the driver refuses", () => {
    const refusal =
      'plan refused: APPROVED_VERSION_NEWS_STEP="a5" names no step of part two: it is one of a4, off, answers';
    const run = bothPartsRun(
      { APPROVED_VERSION_NEWS_STEP: "a5" },
      { full: fullPlan, pick: refusal },
      []
    );
    expect(run.code).toBe(2);
    expect(run.out).toContain(
      `FAIL plan: part two's driver refuses the pick: ${refusal.slice("plan refused: ".length)}\n`
    );
    expect(run.last).toMatch(/^approved version news e2e: FAIL at plan after \d+s \(/);
  }, 60_000);
});
