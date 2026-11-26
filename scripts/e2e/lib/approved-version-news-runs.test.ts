import { expect, test } from "bun:test";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, relative } from "node:path";
import { hostGate } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  a4,
  answers,
  controlFailure,
  driverTest,
  failed,
  fullPlan,
  off,
  passed,
  plan,
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
// test: its run_end, its finish and the real lib/run-verdict.sh print the last line. Runs of part
// one's negative control show the base the script finds, in scratch histories of their own, and a
// run of both parts shows that part two reads none. approved-version-news-verdicts.test.ts runs
// the script's controls, its plan of part two and its last line by name.

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
// The seconds a token has left when the script's refresher mints the next one, read from the
// script, so the stand-in `curl` below mints tokens the refresher replaces a few seconds on.
const keepSeconds = Number(assignment("gateway_keep_seconds").split("=")[1]);
// The header of every identity token the stand-in `curl` mints, base64url as a JWT's is.
const tokenHeader = Buffer.from(
  JSON.stringify({ alg: "RS256", kid: "fixture-signing-key", typ: "JWT" })
).toString("base64url");
// An identity token the stand-in `curl` minted: its header, a payload and a signature naming the
// mint's number in the run.
const mintedToken = new RegExp(`^${tokenHeader}\\.[A-Za-z0-9_-]+\\.fixtureSignatureOfMint(\\d+)$`);

// The value the script assigns to NAME, to the end of its line.
const scriptValue = (name: string) => assignment(name).slice(`${name}=`.length);
const repoRoot = join(dirname(script), "..", "..");
// Part two's driver, which the script reads the news marks from, and the prompts it finds the base
// by, each at the path the script reads it at.
const sessionsFile = scriptValue("sessions_file");
const promptsDir = scriptValue("prompts_dir");
// Part one's driver files, which the control copies from the checkout into the base tree.
const partOneDriverFiles = ["driver_file", "steps_file", "seed_file"].map(scriptValue);
// The Go of each seam the control copies into the base tree where the base defines none
// (control_compat), at the path the script copies it from.
const controlSeamFiles = [
  "approved-version-news-control-transact.go.txt",
  "approved-version-news-control-task.go.txt",
  "approved-version-news-control-contract.go.txt",
].map((name) => join("scripts", "e2e", "lib", name));
// This checkout's sessions_file, line by line.
const sessionsLines = readFileSync(join(repoRoot, sessionsFile), "utf8").split("\n");
// The news marks: the text of every sessions…Mark constant of this checkout's part two driver,
// which the scratch checkout holds as it is here, so a scratch prompt written with them is one the
// script's own read of that file finds.
const newsMarks = sessionsLines.flatMap((line) => {
  const mark = /^\s*sessions\w*Mark\s*=\s*"(.*)"\s*$/.exec(line);
  return mark === null ? [] : [mark[1]];
});
// Two prompt files of the scratch checkout under prompts_dir, a prompt with no news mark, and the
// same prompt with every news mark after it.
const reviewerPrompt = join(promptsDir, "roles", "reviewer.md");
const workerPrompt = join(promptsDir, "go", "worker-common.md");
const unmarked = "a base prompt\n";
const marked = `${unmarked}${newsMarks.map((mark) => `${mark}\n`).join("")}`;
// What the script's refusal of a history with no base says to do.
const remedy =
  "a shallow clone lacks that history: fetch it (git fetch --unshallow), or, for part one's control alone, name its base with --base-commit <rev>";

// The caller's variables a scratch run drops: a pick of part two's runs or an audience would
// change the run under test, and git's own would point git away from the scratch checkout.
const callerVariables = [
  "APPROVED_VERSION_NEWS_STEP",
  "APPROVED_VERSION_NEWS_ARM",
  "APPROVED_VERSION_NEWS_RUNS",
  "APPROVED_VERSION_NEWS_GATEWAY_AUDIENCE",
  "GIT_DIR",
  "GIT_WORK_TREE",
  "GIT_INDEX_FILE",
];

/** Runs git ARGS in CHECKOUT as the fixture's committer, with none of callerVariables, and
 * answers what it printed, trimmed. */
function git(checkout: string, ...args: string[]): string {
  const env = { ...process.env } as Record<string, string>;
  for (const name of callerVariables) delete env[name];
  const run = Bun.spawnSync(
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
  if (run.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${run.stderr.toString()}`);
  return run.stdout.toString().trim();
}

/**
 * What a History builds with: the scratch checkout's path, git in it, and `commit`, which writes
 * FILES (each a path under the checkout and its text) and commits every change as MESSAGE, and
 * answers the commit.
 */
type Scratch = {
  checkout: string;
  git: (...args: string[]) => string;
  commit: (message: string, files?: Record<string, string>) => string;
};

/** Builds the history of a scratch checkout on its first commit, BASE, leaves HEAD where the run
 * starts, and answers each commit a test names, by name. */
type History = (scratch: Scratch, base: string) => Record<string, string>;

/** The base, then the commit that adds the news marks to a prompt file, which is HEAD. */
const markAdded: History = ({ commit }, base) => ({
  base,
  mark: commit("the change", { [workerPrompt]: marked }),
});

/** The base alone: no commit adds a news mark. */
const noMark: History = (_, base) => ({ base });

/** markAdded's history, cut where a shallow clone of depth 1 cuts it: git takes each commit
 * .git/shallow names as one whose parents the clone lacks, so HEAD, the commit that adds the
 * marks, has no parent here. */
const shallowAtMark: History = (scratch, base) => {
  const commits = markAdded(scratch, base);
  writeFileSync(join(scratch.checkout, ".git", "shallow"), `${commits.mark}\n`);
  return commits;
};

/** A history whose first commit that adds a news mark adds the second mark sessions_file names
 * alone (c1), and whose next commit adds the rest (c2), which is HEAD. The real history adds them
 * the other way round (b537812fb5 the architect's, then 843864d305 the worker's), so this one shows
 * that the base follows the first adder of any mark, not the first mark's adder, whose parent here
 * is c1. */
const secondMarkFirst: History = ({ commit }, c0) => {
  const c1 = commit("the change adds the second news mark", {
    [workerPrompt]: `${unmarked}${newsMarks[1]}\n`,
  });
  const c2 = commit("the change adds the other news marks", { [workerPrompt]: marked });
  return { c0, c1, c2 };
};

/** The first line of sessionsLines that assigns a news mark in the form the script reads
 * (unreadLine), numbered from 1, with the name of the constant it assigns. */
const firstMarkIndex = sessionsLines.findIndex((line) =>
  /^\s*sessions\w*Mark\s*=\s*".*"$/.test(line)
);
const unreadLine = {
  number: firstMarkIndex + 1,
  name: sessionsLines[firstMarkIndex]?.match(/sessions\w*Mark/)?.[0] ?? "",
};

/** markAdded's history, whose HEAD also writes sessions_file with a comment after the news mark of
 * unreadLine, a form the script's read of that file does not take. A read that skipped the line
 * would still find the base, by the other marks, and pass. */
const unreadMark: History = (scratch, base) => {
  const commits = markAdded(scratch, base);
  const driver = sessionsLines
    .map((line, index) => (index === firstMarkIndex ? `${line} // a comment` : line))
    .join("\n");
  return {
    ...commits,
    driver: scratch.commit("a comment after a news mark", { [sessionsFile]: driver }),
  };
};

/** A branch merged forward: main adds the news marks after c0 (c1) and moves on (c2), which
 * origin/main names; the branch forks at c0, makes a commit of its own, and merges c2 forward,
 * and HEAD is that merge. */
const mergedForward: History = ({ git, commit }, c0) => {
  const c1 = commit("main adds the news marks", { [workerPrompt]: marked });
  const c2 = commit("main moves on", {
    [reviewerPrompt]: `${unmarked}a later sentence of main's\n`,
  });
  git("update-ref", "refs/remotes/origin/main", c2);
  git("checkout", "-q", "-b", "branch", c0);
  commit("the branch's own change", { "branch-change": "the branch's own change\n" });
  git("merge", "-q", "--no-ff", "--no-edit", "--no-gpg-sign", c2);
  return { c0, c1, c2 };
};

/**
 * Lays out a scratch checkout in DIR/checkout and builds its history. Its first commit holds the
 * script and the libraries it sources, this checkout's part two driver (sessions_file), part one's
 * three driver files and the Go of the control's two seams, each at the path it has here, a
 * placeholder of packages/envoy, which the control's base must hold, and two prompt files under
 * prompts_dir, with no news mark. HISTORY builds the rest on that commit.
 */
function scratchCheckout(dir: string, history: History) {
  const checkout = join(dir, "checkout");
  const place = (path: string) => {
    const to = join(checkout, path);
    mkdirSync(dirname(to), { recursive: true });
    return to;
  };
  const write = (files: Record<string, string>) => {
    for (const [path, text] of Object.entries(files)) writeFileSync(place(path), text);
  };
  for (const path of sourcedLibraries(script)) {
    copyFileSync(path, place(join("scripts", "e2e", relative(dirname(script), path))));
  }
  for (const path of [sessionsFile, ...partOneDriverFiles, ...controlSeamFiles]) {
    copyFileSync(join(repoRoot, path), place(path));
  }
  write({
    [join("packages", "envoy", "go.mod")]: "module fixture/envoy\n",
    [reviewerPrompt]: unmarked,
    [workerPrompt]: unmarked,
  });
  const scratch: Scratch = {
    checkout,
    git: (...args) => git(checkout, ...args),
    commit: (message, files = {}) => {
      write(files);
      git(checkout, "add", "-A");
      git(checkout, "commit", "-q", "--no-gpg-sign", "-m", message);
      return git(checkout, "rev-parse", "HEAD");
    },
  };
  git(checkout, "init", "-q");
  const base = scratch.commit("the base");
  return { checkout, commits: history(scratch, base) };
}

/**
 * Where a run of both parts takes part two's model key from (bothPartsRun's KEY): the key file
 * that holds `modelKey` unless `keyFile` is false, which names a file that does not exist; the
 * audience in APPROVED_VERSION_NEWS_GATEWAY_AUDIENCE when `audience` names one; with `leak`,
 * a stand-in driver that writes the key it was given into the daemon's log in the evidence
 * directory; with `dropKey`, a stand-in driver that removes the key file once it has read it, so
 * the key-leak check after part two finds none; and, with `xtrace`, the script run under
 * `bash -x`, which traces each command of the script's own shell, its subshells' among them,
 * into the run's output.
 */
type KeyRun = {
  keyFile?: boolean;
  audience?: string;
  leak?: boolean;
  dropKey?: boolean;
  xtrace?: boolean;
};

/**
 * Runs the script, both parts, with the variables PICK sets, over lib/live-daemon-proof-fixture.ts's
 * stand-ins, from a scratch checkout of its own whose history HISTORY builds (scratchCheckout): by
 * default markAdded's, whose first commit is the base. The stand-in driver prints part one's
 * output for part one's test; under -part-two-plan it prints PLANS.full when the run picks nothing
 * and PLANS.pick when it picks; and for part two's test it prints PART_TWO's run lines, the count
 * of model calls and the test's ENDED, a FAIL for a driver that stopped at a head run, with that
 * run's status. Part two's test also keeps in `seen/` of the run's directory what it was given as
 * its key file: the file's path, its mode and its text as the test starts, and, for a run that
 * mints the pod's identity token, its text once the script's refresher has written another token
 * into it. The stand-in `curl` answers the metadata server's identity route with a token whose
 * `exp` claim is `keepSeconds` and 4 seconds on, lists each call's arguments in
 * `curl-calls` and each token it answered in `tokens`, one a line. KEY says where the run takes
 * its model key from (KeyRun). Every line, its stderr's included, is in `out`, the script's own
 * last line is `last`, `dir` is the run's directory, `dsn` is the LEGION_TEST_PG_DSN the run was
 * given, `checkout` is the scratch checkout, and `commits` are the ones HISTORY named.
 */
function bothPartsRun(
  pick: Record<string, string>,
  plans: { full: string; pick?: string },
  partTwo: string[],
  ended: "PASS" | "FAIL" = "PASS",
  key: KeyRun = {},
  history: History = markAdded
) {
  const dir = scratch();
  const env = liveDaemonStandIns(dir, { driverOutput: partOneDriver });
  for (const name of callerVariables) delete env[name];
  const { checkout, commits } = scratchCheckout(dir, history);
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
    key=$APPROVED_VERSION_NEWS_MODEL_KEY_FILE
    seen=$LIVE_DAEMON_STAND_IN_DIR/seen
    mkdir -p "$seen"
    printf '%s\\n' "$key" >"$seen/path"
    stat -c %a "$key" >"$seen/mode"
    cp "$key" "$seen/first"
    if [ -n "\${STAND_IN_AWAIT_REFRESH:-}" ]; then
      for _ in $(seq 1 300); do
        [ "$(<"$key")" = "$(<"$seen/first")" ] || break
        sleep 0.1
      done
      cp "$key" "$seen/later"
    fi
    [ -z "\${STAND_IN_LEAK:-}" ] || cat "$key" >>"$LEGION_E2E_DAEMON_LOG"
    [ -z "\${STAND_IN_DROP_KEY:-}" ] || rm -f -- "$key"
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
  writeFileSync(
    join(dir, "bin", "curl"),
    `#!/usr/bin/env bash
printf '%s\\n' "$*" >>"$LIVE_DAEMON_STAND_IN_DIR/curl-calls"
n=$(($(cat "$LIVE_DAEMON_STAND_IN_DIR/minted" 2>/dev/null || echo 0) + 1))
echo "$n" >"$LIVE_DAEMON_STAND_IN_DIR/minted"
payload=$(printf '{"aud":"model-gateway-gce","exp":%d,"n":%d}' "$(($(date +%s) + ${keepSeconds + 4}))" "$n" |
  base64 -w0 | tr '+/' '-_' | tr -d '=')
token=$(printf '%s.%s.%s' '${tokenHeader}' "$payload" "fixtureSignatureOfMint$n")
printf '%s\\n' "$token" >>"$LIVE_DAEMON_STAND_IN_DIR/tokens"
printf '%s' "$token"
`
  );
  chmodSync(join(dir, "bin", "curl"), 0o755);
  Object.assign(env, pick, {
    PATH: `${env.PATH}:${dirname(process.execPath)}`,
    LEGION_OMP_PATH: join(dir, "omp"),
    ANTHROPIC_BASE_URL: "http://127.0.0.1:1",
    APPROVED_VERSION_NEWS_MODEL_KEY_FILE: join(dir, key.keyFile === false ? "no-key" : "model-key"),
    APPROVED_VERSION_NEWS_EVIDENCE_DIR: join(dir, "evidence"),
  });
  if (key.audience !== undefined) env.APPROVED_VERSION_NEWS_GATEWAY_AUDIENCE = key.audience;
  // A run that mints the token waits in part two's test for the refresher's next one.
  if (key.keyFile === false && key.audience !== undefined) env.STAND_IN_AWAIT_REFRESH = "1";
  if (key.leak) env.STAND_IN_LEAK = "1";
  if (key.dropKey) env.STAND_IN_DROP_KEY = "1";
  const xtrace = key.xtrace ? ["-x"] : [];
  const result = Bun.spawnSync(
    ["bash", ...xtrace, join(checkout, "scripts", "e2e", "approved-version-news.sh")],
    {
      env,
    }
  );
  const out = result.stdout.toString() + result.stderr.toString();
  return {
    code: result.exitCode,
    out,
    last: out.trimEnd().split("\n").at(-1) ?? "",
    dir,
    dsn: env.LEGION_TEST_PG_DSN ?? "",
    checkout,
    commits,
  };
}

// A full run's part two: every run passes.
const fullRun = [
  ...runsOf(a4, "head", passed, passed, passed),
  ...runsOf(off, "head", passed, passed, passed),
  ...runsOf(answers, "head", passed, passed, passed),
];
// The driver's plan of a pick of one run of each step, as pickSessions makes it.
const onePlan = plan(
  [
    ["a4", a4, "head"],
    ["off", off, "head"],
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
// full run's plan it left out and its count of runs where it is not three, and exits 0; a run of
// the pick that did not pass still reads FAIL. The counts and the checks follow the driver's
// plan, a plan of an arm other than head fails the plan check, and a pick the driver refuses is
// refused in its words.
gitHost.describe("the script's run of both parts", () => {
  test("reads PASS for a full run, which picks nothing", () => {
    const run = bothPartsRun({}, { full: fullPlan }, fullRun);
    expect(run.code).toBe(0);
    expect(run.last).toMatch(/^approved version news e2e: PASS in \d+s \(/);
    expect(run.last).toEndWith(
      `(assertions: ${expectedChecks}/${expectedChecks}; part two: a4 head 3/3; off head 3/3; answers head 3/3; model calls: 9)`
    );
    // The plan check's build is the run's one: each part's driver check takes it and builds none.
    expect(run.out.match(/^ {3}built .+\.test from /gm)).toHaveLength(1);
    expect(run.out.match(/^ {3}the caller built .+; this check builds nothing$/gm)).toHaveLength(2);
  }, 60_000);

  test("reads MEASURED, not PASS, for APPROVED_VERSION_NEWS_ARM=head, which leaves nothing out", () => {
    const run = bothPartsRun(
      { APPROVED_VERSION_NEWS_ARM: "head" },
      { full: fullPlan, pick: fullPlan },
      fullRun
    );
    expect(run.code).toBe(0);
    expect(run.last).toMatch(/^approved version news e2e: MEASURED in \d+s \(/);
    expect(run.last).toContain(
      `(assertions: ${expectedChecks}/${expectedChecks}; part two: a partial run picked by APPROVED_VERSION_NEWS_ARM=head: a4 head 3/3; off head 3/3; answers head 3/3; model calls: 9, evidence: `
    );
  }, 60_000);

  test("reads MEASURED, not PASS, for APPROVED_VERSION_NEWS_STEP=a4, naming each step it left out", () => {
    const run = bothPartsRun(
      { APPROVED_VERSION_NEWS_STEP: "a4" },
      { full: fullPlan, pick: plan([["a4", a4, "head"]], 3) },
      runsOf(a4, "head", passed, passed, passed)
    );
    expect(run.code).toBe(0);
    expect(run.last).toMatch(/^approved version news e2e: MEASURED in \d+s \(/);
    expect(run.last).toContain(
      `(assertions: ${expectedChecks}/${expectedChecks}; part two: a partial run picked by APPROVED_VERSION_NEWS_STEP=a4, leaving out off head, answers head: a4 head 3/3; model calls: 3, evidence: `
    );
  }, 60_000);

  test("reads MEASURED, not PASS, for APPROVED_VERSION_NEWS_RUNS=1, naming its count of runs", () => {
    const run = bothPartsRun(
      { APPROVED_VERSION_NEWS_RUNS: "1" },
      { full: fullPlan, pick: onePlan },
      [
        ...runsOf(a4, "head", passed),
        ...runsOf(off, "head", passed),
        ...runsOf(answers, "head", passed),
      ]
    );
    expect(run.code).toBe(0);
    expect(run.last).toMatch(/^approved version news e2e: MEASURED in \d+s \(/);
    expect(run.last).toContain(
      `(assertions: ${expectedChecks}/${expectedChecks}; part two: a partial run picked by APPROVED_VERSION_NEWS_RUNS=1, with 1 run of each arm in place of 3: a4 head 1/1; off head 1/1; answers head 1/1; model calls: 3, evidence: `
    );
  }, 60_000);

  test("reads FAIL for a pick whose run did not pass", () => {
    const run = bothPartsRun(
      { APPROVED_VERSION_NEWS_STEP: "off" },
      { full: fullPlan, pick: plan([["off", off, "head"]], 3) },
      runsOf(off, "head", passed, failed),
      "FAIL"
    );
    expect(run.code).toBe(1);
    expect(run.out).not.toContain("approved version news e2e: MEASURED");
    expect(run.last).toMatch(/^approved version news e2e: FAIL at part-two after \d+s \(/);
    expect(run.last).toContain(
      "part two: a partial run picked by APPROVED_VERSION_NEWS_STEP=off, leaving out a4 head, answers head: off head 1/2; model calls: 2, evidence: "
    );
  }, 60_000);

  // The script keeps no table of part two's steps: a step the driver's plan adds is counted, and
  // a plan that makes a4 in the retired base arm fails the run at the plan check, naming a4.
  test("counts each step the driver's plan adds", () => {
    const extra = "extra-a-step-the-driver-added";
    const withExtra = plan(
      [
        ["a4", a4, "head"],
        ["off", off, "head"],
        ["answers", answers, "head"],
        ["extra", extra, "head"],
      ],
      3
    );
    const run = bothPartsRun({}, { full: withExtra }, [
      ...fullRun,
      ...runsOf(extra, "head", passed, passed, passed),
    ]);
    expect(run.code).toBe(0);
    expect(run.last).toMatch(/^approved version news e2e: PASS in \d+s \(/);
    expect(run.last).toContain("; answers head 3/3; extra head 3/3; model calls: 12)");
  }, 60_000);

  test("fails at the plan check, naming a4, where the driver's plan makes a4 in the base arm", () => {
    const back = plan(
      [
        ["a4", a4, "head base"],
        ["off", off, "head"],
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
      "FAIL plan: part two's driver planned a4 in the arm 'base', which is not head, the one arm\n"
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

/** What part two's stand-in driver kept as NAME in `seen/` of the run's directory DIR. */
const seen = (dir: string, name: string) => readFileSync(join(dir, "seen", name), "utf8");

// A full run of both parts again, for where part two takes its model key from. Where the key file
// holds a key, the run takes it, an audience named beside it or not, and mints nothing. Where it
// holds none and APPROVED_VERSION_NEWS_GATEWAY_AUDIENCE names an audience, the run mints the
// pod's identity token from the stand-in `curl` as from the metadata server, into a key file of
// its own scratch, mode 600, which the driver is pointed at, which the refresher writes a later
// token into while part two runs, and which the run removes; no token is printed, and the
// key-leak check reads each token's header. A key file the key-leak check can no longer read fails
// the run there. A run under `bash -x` traces no part of the key file's key or of a token the run
// minted, since each function that holds one turns xtrace off for itself, and no part of
// LEGION_TEST_PG_DSN, which the run reads by its name. With neither, the run is refused before
// anything is built.
gitHost.describe("part two's model key", () => {
  const identityRoute =
    "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=model-gateway-gce&format=full";

  test("is the key file's key where the file holds one, with an audience beside it, and nothing is minted", () => {
    const run = bothPartsRun({}, { full: fullPlan }, fullRun, "PASS", {
      audience: "model-gateway-gce",
    });
    expect(run.code).toBe(0);
    expect(run.last).toMatch(/^approved version news e2e: PASS in \d+s \(/);
    expect(seen(run.dir, "path")).toBe(`${join(run.dir, "model-key")}\n`);
    expect(seen(run.dir, "first")).toBe(`${modelKey}\n`);
    expect(existsSync(join(run.dir, "curl-calls"))).toBe(false);
    expect(run.out).toContain(
      `, model key file ${join(run.dir, "model-key")} (read by the driver, never printed)\n`
    );
    expect(run.out).not.toContain("== gateway-token");
    expect(run.out).toContain("   the key's first 8 characters occur 0 times in ");
  }, 60_000);

  test("is the pod's identity token where the key file holds none, minted again while part two runs", () => {
    const run = bothPartsRun({}, { full: fullPlan }, fullRun, "PASS", {
      keyFile: false,
      audience: "model-gateway-gce",
    });
    expect(run.code).toBe(0);
    expect(run.last).toMatch(/^approved version news e2e: PASS in \d+s \(/);
    const calls = readFileSync(join(run.dir, "curl-calls"), "utf8").trimEnd().split("\n");
    expect(calls.length).toBeGreaterThanOrEqual(2);
    for (const call of calls) {
      expect(call).toBe(`-sSf --retry 3 -H Metadata-Flavor: Google ${identityRoute}`);
    }
    // The driver is pointed at a key file of the run's own scratch, mode 600, which the run removes.
    const path = seen(run.dir, "path").trimEnd();
    expect(path).toStartWith(join(run.dir, "tmp", "legion-e2e-approved-version-news-gateway."));
    expect(seen(run.dir, "mode")).toBe("600\n");
    expect(existsSync(dirname(path))).toBe(false);
    // The file holds a minted token as part two's driver starts, and a later one while it runs.
    const first = seen(run.dir, "first").match(mintedToken);
    const later = seen(run.dir, "later").match(mintedToken);
    expect(first).not.toBeNull();
    expect(later).not.toBeNull();
    expect(Number(later?.[1])).toBeGreaterThan(Number(first?.[1]));
    expect(run.out).toContain("\n== gateway-token\n");
    expect(run.out).toMatch(
      /^ {3}the identity token for the audience model-gateway-gce was minted again at \S+ into \S+ \(not printed\), with \d+ seconds left$/m
    );
    expect(run.out).toContain(
      "   the headers of the identity tokens the run minted occur 0 times in "
    );
    expect(run.out).not.toContain(tokenHeader);
    expect(run.out).not.toContain("fixtureSignatureOfMint");
  }, 60_000);

  test("fails at key-leak where a token the run minted is in the evidence directory", () => {
    const run = bothPartsRun({}, { full: fullPlan }, fullRun, "PASS", {
      keyFile: false,
      audience: "model-gateway-gce",
      leak: true,
    });
    expect(run.code).toBe(1);
    expect(run.out).toContain(
      "FAIL key-leak: the header of an identity token the run minted is in the evidence directory 1 times\n"
    );
    expect(run.last).toMatch(/^approved version news e2e: FAIL at key-leak after \d+s \(/);
    expect(run.out).not.toContain("fixtureSignatureOfMint");
  }, 60_000);

  // Under `bash -x` the script's shell traces each command it runs, the assignment of a variable
  // and a builtin's arguments among them. mint_gateway_token and jwt_exp hold the token in their
  // variables and arguments, and turn the trace off for themselves, so no part of any token the run
  // minted, at the gateway-token check or in the refresher's subshell, is in the run's output or
  // its stderr: not its header, not its payload and not its signature. The trace is off for those
  // functions alone: the script's own next command after the first mint is traced again.
  test("traces no part of a token the run minted under bash -x", () => {
    const run = bothPartsRun({}, { full: fullPlan }, fullRun, "PASS", {
      keyFile: false,
      audience: "model-gateway-gce",
      xtrace: true,
    });
    // The run is traced, the trace comes back on after the mint, and the driver read a minted
    // token and a later one. The assignment of the refresher's pid after the mint
    // (`gateway_refresher=$!`) traces with digits, while the empty one at the script's top traces
    // as `+ gateway_refresher=` long before the mint.
    expect(run.out).toMatch(/^\+ mint_gateway_token$/m);
    expect(run.out).toMatch(/^\+ gateway_refresher=\d+$/m);
    expect(seen(run.dir, "first")).toMatch(mintedToken);
    expect(seen(run.dir, "later")).toMatch(mintedToken);
    const tokens = readFileSync(join(run.dir, "tokens"), "utf8").trimEnd().split("\n");
    expect(tokens.length).toBeGreaterThanOrEqual(2);
    for (const token of tokens) {
      const parts = token.split(".");
      expect(parts.length).toBe(3);
      for (const part of parts) {
        expect(run.out).not.toContain(part);
      }
    }
    // Trace lines follow the run's last line, so the PASS line is matched where it stands.
    expect(run.code).toBe(0);
    expect(run.out).toMatch(/^approved version news e2e: PASS in \d+s \(/m);
  }, 60_000);

  // On a key file's key, a run under `bash -x` traces neither the key nor LEGION_TEST_PG_DSN. The
  // key-leak check reads the key into a variable of key_leaks alone, which turns the trace off for
  // itself. The arguments check and the lib's prerequisites read the DSN by its name
  // (lib/loopback-dsn.sh's require_loopback_env) and test it for a value without expanding it.
  test("traces no part of the key file's key and no LEGION_TEST_PG_DSN under bash -x", () => {
    const run = bothPartsRun({}, { full: fullPlan }, fullRun, "PASS", { xtrace: true });
    // The run is traced from its arguments check through its key-leak check.
    expect(run.out).toMatch(/^\+ begin arguments$/m);
    expect(run.out).toMatch(/^\+ require_loopback_env db_host LEGION_TEST_PG_DSN /m);
    expect(run.out).toMatch(/^\+ begin key-leak$/m);
    expect(run.out).not.toContain(modelKey.slice(0, 8));
    expect(run.dsn).not.toBe("");
    expect(run.out).not.toContain(run.dsn);
    expect(run.out).toContain("   the key's first 8 characters occur 0 times in ");
    expect(run.code).toBe(0);
    expect(run.out).toMatch(/^approved version news e2e: PASS in \d+s \(/m);
  }, 60_000);

  // The key-leak check counts the key file's key in the evidence directory, so a key file it
  // cannot read leaves it nothing to count: the run fails there, naming the file, rather than
  // counting an empty key's characters and passing.
  test("fails at key-leak where the model key file can no longer be read", () => {
    const run = bothPartsRun({}, { full: fullPlan }, fullRun, "PASS", { dropKey: true });
    expect(run.code).toBe(1);
    expect(run.out).toContain(
      `FAIL key-leak: the key-leak check could not read the model key file ${join(run.dir, "model-key")}\n`
    );
    expect(run.out).not.toContain("the key's first 8 characters occur");
    expect(run.last).toMatch(/^approved version news e2e: FAIL at key-leak after \d+s \(/);
  }, 60_000);

  test("refuses with exit 2, before anything is built, a run with no key in the key file and no audience", () => {
    const run = bothPartsRun({}, { full: fullPlan }, fullRun, "PASS", { keyFile: false });
    expect(run.code).toBe(2);
    expect(run.out).toContain(
      `FAIL arguments: the model key file ${join(run.dir, "no-key")} is not a readable file with a key in it, and APPROVED_VERSION_NEWS_GATEWAY_AUDIENCE names no audience of the pod's identity token to run on in its place: `
    );
    expect(run.out).not.toContain("== plan");
    expect(existsSync(join(run.dir, "curl-calls"))).toBe(false);
    expect(run.last).toMatch(/^approved version news e2e: FAIL at arguments after \d+s \(/);
  }, 60_000);
});

// Part one's negative control as the driver prints it on a daemon from before the change: the
// control step's setup checks, as many as the script's control_setup_checks, each held, then its
// one-approval check's failure in that check's own words.
const controlStep = scriptValue("control_step");
const controlDriver = [
  `=== RUN   ${driverTest}`,
  `== ${controlStep}`,
  ...Array.from(
    { length: Number(scriptValue("control_setup_checks")) },
    (_, n) => `   assert ${controlStep}: setup check ${n + 1} held`
  ),
  controlFailure(controlStep),
  `--- FAIL: ${driverTest} (191.40s)`,
  "FAIL",
];

/** Runs the script with --control and no --base-commit over lib/live-daemon-proof-fixture.ts's
 * stand-ins, whose driver prints controlDriver, from a scratch checkout whose history HISTORY
 * builds (scratchCheckout). It answers `code`, `out`, `last`, `checkout` and `commits` as
 * bothPartsRun does. */
function controlRun(history: History) {
  const dir = scratch();
  const env = liveDaemonStandIns(dir, { driverOutput: controlDriver });
  for (const name of callerVariables) delete env[name];
  const { checkout, commits } = scratchCheckout(dir, history);
  env.APPROVED_VERSION_NEWS_EVIDENCE_DIR = join(dir, "evidence");
  const result = Bun.spawnSync(
    ["bash", join(checkout, "scripts", "e2e", "approved-version-news.sh"), "--control"],
    { env }
  );
  const out = result.stdout.toString() + result.stderr.toString();
  return {
    code: result.exitCode,
    out,
    last: out.trimEnd().split("\n").at(-1) ?? "",
    checkout,
    commits,
  };
}

// The base part one's negative control runs against where no --base-commit names one: the first
// parent of the first commit in HEAD's history that adds a news mark. In a branch merged forward
// past main's commit that adds the marks, the base is that commit's parent, which carries none,
// while the merge base of HEAD and origin/main, the base before this rule, carries them: the
// first test shows both, so it fails on the old rule. Where the first commit that adds a mark adds
// the second mark sessions_file names, and a later one the first, the base is the first one's
// parent, and not the parent of the first mark's adder. A history in which no commit adds a mark,
// and one cut where a shallow clone cuts it, are refused with exit 2, naming the remedies. A
// driver line that assigns a news mark in a form the script does not read fails the run with exit
// 1, naming the line, where a read that skipped it passed. A run of both parts reads no base, since
// part two runs on this checkout's prompts alone, so it passes where no commit adds a mark.
gitHost.describe("the base", () => {
  test("is the parent of the first commit that adds a news mark, in a branch merged forward past it", () => {
    const run = controlRun(mergedForward);
    const { c0, c1, c2 } = run.commits;
    // The test's own control: the old rule's base, the merge base of HEAD and origin/main, is c2,
    // whose prompts carry the news marks, so a run on that rule fails every check below.
    expect(git(run.checkout, "merge-base", "HEAD", "origin/main")).toBe(c2);
    const mainPrompt = git(run.checkout, "show", `${c2}:${workerPrompt}`);
    expect(newsMarks.length).toBeGreaterThan(0);
    for (const mark of newsMarks) expect(mainPrompt).toContain(mark);
    expect(run.code).toBe(0);
    expect(run.out).toContain(
      `\n   the base: ${c0}, the parent of ${c1}, the first commit that adds a news mark\n`
    );
    expect(run.last).toMatch(
      new RegExp(`^approved version news e2e: control bites at ${controlStep} against ${c0} in `)
    );
  }, 60_000);

  test("is the parent of the first commit that adds any news mark, where that commit adds the second mark", () => {
    expect(newsMarks.length).toBeGreaterThanOrEqual(2);
    const run = controlRun(secondMarkFirst);
    const { c0, c1, c2 } = run.commits;
    // The test's own control: the first mark's own adder is c2, whose parent, c1, carries the
    // second mark, so a base read from the first mark alone fails every check below.
    expect(
      git(run.checkout, "log", "--format=%H", `-S${newsMarks[0]}`, "HEAD", "--", promptsDir)
    ).toBe(c2);
    expect(git(run.checkout, "show", `${c1}:${workerPrompt}`)).toContain(newsMarks[1]);
    expect(run.code).toBe(0);
    expect(run.out).toContain(
      `\n   the base: ${c0}, the parent of ${c1}, the first commit that adds a news mark\n`
    );
    expect(run.last).toMatch(
      new RegExp(`^approved version news e2e: control bites at ${controlStep} against ${c0} in `)
    );
  }, 60_000);

  test("fails the run with exit 1, naming the line, where the driver assigns a news mark in a form the script does not read", () => {
    expect(unreadLine.name).not.toBe("");
    const run = controlRun(unreadMark);
    const line = `${sessionsLines[firstMarkIndex]?.trimStart()} // a comment`;
    expect(run.code).toBe(1);
    expect(run.out).toContain(
      `FAIL arguments: ${sessionsFile} line ${unreadLine.number} assigns the news mark ${unreadLine.name} in a form this script does not read, so the run would find the base without that mark: write it as ${unreadLine.name} = "<text>", alone on its line; the line reads: ${line}\n`
    );
    expect(run.out).not.toContain("the base: ");
    expect(run.out).not.toContain("== base-tree");
    expect(run.last).toMatch(/^approved version news e2e: FAIL at arguments after \d+s \(/);
  }, 60_000);

  test("is refused with exit 2 where no commit in HEAD's history adds a news mark, naming the remedies", () => {
    const run = controlRun(noMark);
    expect(run.code).toBe(2);
    expect(run.out).toContain(
      `FAIL arguments: no commit in HEAD's history adds a news mark to ${promptsDir}, so this run finds no base for part one's control: ${remedy}\n`
    );
    expect(run.out).not.toContain("== base-tree");
    expect(run.last).toMatch(/^approved version news e2e: FAIL at arguments after \d+s \(/);
  }, 60_000);

  test("is refused with exit 2 where the clone lacks the parent of the first commit that adds a news mark", () => {
    const run = controlRun(shallowAtMark);
    expect(git(run.checkout, "rev-parse", "--is-shallow-repository")).toBe("true");
    expect(run.code).toBe(2);
    expect(run.out).toContain(
      `FAIL arguments: ${run.commits.mark}, the first commit in HEAD's history that adds a news mark to ${promptsDir}, has no parent in this clone, so this run finds no base for part one's control: ${remedy}\n`
    );
    expect(run.out).not.toContain("== base-tree");
    expect(run.last).toMatch(/^approved version news e2e: FAIL at arguments after \d+s \(/);
  }, 60_000);

  test("is not read by a run of both parts, which passes where no commit adds a news mark", () => {
    const run = bothPartsRun({}, { full: fullPlan }, fullRun, "PASS", {}, noMark);
    expect(run.code).toBe(0);
    expect(run.out).not.toContain("the base: ");
    expect(run.last).toMatch(/^approved version news e2e: PASS in \d+s \(/);
  }, 60_000);

  test("is the same for part one's negative control, --control with no --base-commit", () => {
    const run = controlRun(mergedForward);
    const { c0, c1 } = run.commits;
    expect(run.out).toContain(
      `\n   the base: ${c0}, the parent of ${c1}, the first commit that adds a news mark\n`
    );
    expect(run.out).toContain(`\n   part one's negative control against ${c0}\n`);
    expect(run.code).toBe(0);
    expect(run.last).toMatch(
      new RegExp(
        `^approved version news e2e: control bites at ${controlStep} against ${c0} in \\d+s \\(assertions: \\d+/\\d+; part one alone, part two did not run, evidence: `
      )
    );
  }, 60_000);
});
