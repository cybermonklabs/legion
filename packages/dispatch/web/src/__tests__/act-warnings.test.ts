import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

/** `packages/dispatch`, where the suite's own command runs, so the child reads whatever
 *  `bunfig.toml` that directory holds. */
const PACKAGE_DIR = join(import.meta.dir, "..", "..", "..");

// React's own text of each act message the fixtures draw. Kept away from the assertions, so the
// source excerpt Bun prints when one fails holds no React text.
const NOT_CONFIGURED = "not configured to support act(";
const NOT_WRAPPED = "not wrapped in act(";
const WITHOUT_AWAIT = "You called act(async () => ...) without await";
const OVERLAPPING = "overlapping act() calls";
const SUSPENDED = "suspended inside an `act` scope";

/** The `test:web` script's words after its leading `bun`: the preloads and flags of every run, as
 *  the shell `bun run` starts it in expands them, so its `--parallel=$(bun …)` is the count that
 *  command prints rather than the text of the command. */
function suiteArguments(): string[] {
  const manifest = JSON.parse(readFileSync(join(PACKAGE_DIR, "package.json"), "utf8")) as {
    scripts: { "test:web": string };
  };
  const expanded = Bun.spawnSync(["sh", "-c", `printf '%s\\n' ${manifest.scripts["test:web"]}`], {
    cwd: PACKAGE_DIR,
    stderr: "pipe",
  });
  expect(expanded.exitCode, expanded.stderr.toString()).toBe(0);
  const [runner, ...args] = expanded.stdout.toString().trimEnd().split("\n");
  expect(runner).toBe("bun");
  return args;
}

/** What a fixture's child left: its exit code and all it wrote to stderr. */
interface ChildRun {
  exitCode: number;
  output: string;
}

/** The counts a child's summary prints, as the text it prints them in. */
interface SummaryCounts {
  fail: string;
  pass: string;
}

// Half the 60 s `test:web` gives a row, so a child that hangs is killed while its row still runs,
// and the row fails on what the child printed rather than on a bare timeout. Bun kills it with
// SIGTERM, and `exited` then gives 143 (measured on Bun 1.4.4).
const CHILD_DEADLINE_MS = 30_000;

/**
 * Runs one fixture of this directory in a child `bun test` built from the `test:web` script: the
 * real `setup.ts` against the messages React really prints. Bun runs a file whose name matches no
 * test pattern only when its path starts with `./`. The child's output is read, not printed, so
 * this file's own log holds none of React's act text.
 */
async function runFixture(fixture: string): Promise<ChildRun> {
  // FORCE_COLOR would make Bun write `✗` where a failed row reads `(fail)`, and NO_COLOR keeps the
  // lines free of colour codes.
  const { FORCE_COLOR: _forced, ...inherited } = process.env;
  const child = Bun.spawn(
    [process.execPath, ...suiteArguments(), `./web/src/__tests__/${fixture}`],
    {
      cwd: PACKAGE_DIR,
      env: { ...inherited, NO_COLOR: "1" },
      stderr: "pipe",
      stdout: "ignore",
      timeout: CHILD_DEADLINE_MS,
    }
  );
  // Bun writes the rows, their errors and the summary to stderr; stdout holds only its banner, so
  // it is not read.
  const [output, exitCode] = await Promise.all([new Response(child.stderr).text(), child.exited]);
  return { exitCode, output };
}

/** Expects the child to have exited 1 with these counts, and prints all it wrote when not. */
function expectFailedSummary(run: ChildRun, counts: SummaryCounts): void {
  const summary = {
    exitCode: run.exitCode,
    fail: /^ *(\d+) fail$/m.exec(run.output)?.[1],
    pass: /^ *(\d+) pass$/m.exec(run.output)?.[1],
  };
  expect(summary, `the child exited ${run.exitCode} and wrote:\n${run.output}`).toEqual({
    exitCode: 1,
    ...counts,
  });
}

/** A row's name in its `(fail)` line, without the time Bun prints after it. */
const FAILED_ROW = /^\(fail\) (.+?)(?: \[[^\]]*\])?$/;

/**
 * Each failed row's report by the row's name: the lines from the last one starting `error: ` above
 * the row's `(fail)` line down to that line. The rows are read by name, since the run may order
 * them at random. A line starting `error: ` restarts the report and a `(fail)` line ends it, after
 * which nothing is kept until the next `error: ` line, so one row cannot be read with another's
 * message; the `(pass)` line Bun prints for a passing row when AGENT and CLAUDECODE are unset needs
 * no reset of its own, since every report restarts at its own `error: ` line. A line must start
 * with `error: `: in CI Bun also prints a `::error ...` line whose title holds `error: ` too,
 * between the message and the `(fail)` line.
 */
function failedRows(output: string): Map<string, string> {
  const reports = new Map<string, string>();
  let report: string[] = [];
  for (const line of output.split("\n")) {
    if (line.startsWith("error: ")) report = [line];
    else if (report.length > 0) report.push(line);
    const failed = FAILED_ROW.exec(line);
    if (failed !== null) {
      reports.set(failed[1], report.join("\n"));
      report = [];
    }
  }
  return reports;
}

/** How many times `text` occurs in `output`. */
function occurrences(output: string, text: string): number {
  return output.split(text).length - 1;
}

// The rule proven end to end. A React upgrade that changes the text of a message a fixture draws
// fails its row, where every other row would go on passing with the rule silent.
test("the act rule fails each row React printed an act message in, and the file after its last row", async () => {
  const run = await runFixture("act-rule.fixture.tsx");
  // The summary counts the late update as a row of its own.
  expectFailedSummary(run, { fail: "4", pass: "1" });
  const failed = failedRows(run.output);
  expect([...failed.keys()].sort()).toEqual([
    "(unnamed)",
    "a state set outside act fails its row",
    "a state set outside act fails its row while console.error is quiet",
    "an act inside a waitFor callback fails its row",
  ]);
  const outside = failed.get("a state set outside act fails its row");
  expect(outside).toContain("error: React printed 1 act message(s) in or before this row:");
  expect(outside).toContain("update outside act in CountSetOutsideAct");
  const quiet = failed.get("a state set outside act fails its row while console.error is quiet");
  expect(quiet).toContain("error: React printed 1 act message(s) in or before this row:");
  expect(quiet).toContain("update outside act in CountSetWhileQuiet");
  const inWaitFor = failed.get("an act inside a waitFor callback fails its row");
  expect(inWaitFor).toContain("error: React printed 1 act message(s) in or before this row:");
  expect(inWaitFor).toContain("act environment off");
  const afterLastRow = failed.get("(unnamed)");
  expect(afterLastRow).toContain(
    "error: React printed 1 act message(s) in or after this file's last row:"
  );
  expect(afterLastRow).toContain("update outside act in LateTimerCount");
  // React's own lines, counted over the whole output so the count holds in any row order:
  // CountSetOutsideAct's and LateTimerCount's "not wrapped" lines, and the waitFor row's "not
  // configured" line. Neither the rule's message nor the source excerpt Bun prints above it
  // repeats React's words, so a count of React's lines counts each message once; and the recorder
  // still prints every act message, so a recorder that swallowed them would read 0 here. The
  // quiet row's message is recorded, as that row's failure shows, and never printed, so it adds
  // none; a `quietConsoleError` that printed its calls would make the "not wrapped" count 3.
  expect({
    notConfigured: occurrences(run.output, NOT_CONFIGURED),
    notWrapped: occurrences(run.output, NOT_WRAPPED),
  }).toEqual({ notConfigured: 1, notWrapped: 2 });
});

// The other three messages. React prints the first and the third once per process, and the first
// two leave its act scope changed for every later act in the process, so each comes from a fixture
// file of its own with one row, run in a child of its own: none can change another's row, and none
// depends on the order of rows. Each count of React's text is over the child's whole output, and
// shows the recorder printed the message as well as recording it.
test("the act rule fails the row that left an async act unawaited", async () => {
  const run = await runFixture("act-rule-unawaited-act.fixture.tsx");
  expectFailedSummary(run, { fail: "1", pass: "0" });
  const failed = failedRows(run.output);
  expect([...failed.keys()]).toEqual(["an async act nothing awaits fails its row"]);
  expect(failed.get("an async act nothing awaits fails its row")).toContain(
    "error: React printed 1 act message(s) in or before this row:\n  unawaited async act"
  );
  expect(occurrences(run.output, WITHOUT_AWAIT)).toBe(1);
});

// React prints its message at the end of each of the two acts.
test("the act rule fails the row whose async acts overlapped", async () => {
  const run = await runFixture("act-rule-overlapping-act.fixture.tsx");
  expectFailedSummary(run, { fail: "1", pass: "0" });
  const failed = failedRows(run.output);
  expect([...failed.keys()]).toEqual(["two async acts that end out of order fail their row"]);
  expect(failed.get("two async acts that end out of order fail their row")).toContain(
    "error: React printed 2 act message(s) in or before this row:\n  overlapping act"
  );
  expect(occurrences(run.output, OVERLAPPING)).toBe(2);
});

test("the act rule fails the row whose component suspended inside an unawaited act", async () => {
  const run = await runFixture("act-rule-unawaited-suspense.fixture.tsx");
  expectFailedSummary(run, { fail: "1", pass: "0" });
  const failed = failedRows(run.output);
  expect([...failed.keys()]).toEqual([
    "a component that suspends inside an act nothing awaits fails its row",
  ]);
  expect(
    failed.get("a component that suspends inside an act nothing awaits fails its row")
  ).toContain("error: React printed 1 act message(s) in or before this row:\n  unawaited suspense");
  expect(occurrences(run.output, SUSPENDED)).toBe(1);
});

// Both of the preload's restores of a spy of `quietConsoleError` that a row left in place: the
// `afterEach`, which a row that throws still reaches, and which runs before the act check, so a
// row whose act message makes that check throw reaches it too; and the `beforeEach`, which puts
// back one a row left when an `afterEach` of that row threw. Without either, or with the
// `afterEach` below the act check, a hook of the fixture fails, which Bun reports as one more
// failed row, named `(unnamed)`.
test("the preload puts back the spy of quietConsoleError a row leaves, even past a throwing afterEach", async () => {
  const run = await runFixture("act-rule-quiet-restore.fixture.tsx");
  expectFailedSummary(run, { fail: "4", pass: "1" });
  const failed = failedRows(run.output);
  const threw = "a quiet row that throws > fails before it restores console.error";
  const drewAct =
    "a quiet row whose act check throws > sets a state outside act while console.error is quiet";
  const leftQuiet = [
    "quiet rows whose afterEach throws > another leaves console.error quiet past its end",
    "quiet rows whose afterEach throws > one leaves console.error quiet past its end",
  ];
  expect([...failed.keys()].sort()).toEqual([threw, drewAct, ...leftQuiet]);
  expect(failed.get(threw)).toContain("error: the row failed with console.error quiet");
  const actCheck = failed.get(drewAct);
  expect(actCheck).toContain("error: React printed 1 act message(s) in or before this row:");
  expect(actCheck).toContain("update outside act in QuietActCount");
  for (const name of leftQuiet) {
    expect(failed.get(name)).toContain(
      "error: the row's afterEach failed, which skips the preload's afterEach hooks"
    );
  }
});

// The preload's last `afterEach`, which runs the network report and then the act check after every
// reset and restore above it. Each of the fixture's two leaking rows makes a request nothing
// answers, draws an act message while `console.error` is quiet, and leaves that spy, a fake clock
// and a `<Timestamp>` tick armed on it; each must fail with one report naming both, and every row
// must start clean, which the fixture's `afterAll` checks. With the two checks in two hooks, the
// network report's throw skips the act check, and the act message fails a later row or the file
// instead; with the network report registered early, its throw also skips every reset and restore
// after it, and the next row starts on the fake clock the leaking row left, which that `afterAll`
// reports as one more failed row, named `(unnamed)`.
test("the preload's last afterEach fails a row on its request and its act message at once, after every reset", async () => {
  const run = await runFixture("act-rule-final-hook.fixture.tsx");
  expectFailedSummary(run, { fail: "2", pass: "1" });
  const failed = failedRows(run.output);
  const leaking = "rows that leave everything the preload puts back";
  const reports = {
    [`${leaking} > another fails on its request and its act message in one report`]: {
      component: "CountAnotherRowLeaves",
      request: "GET /api/v1/nothing-answers/another",
    },
    [`${leaking} > one fails on its request and its act message in one report`]: {
      component: "CountOneRowLeaves",
      request: "GET /api/v1/nothing-answers/one",
    },
  };
  // Neither `(unnamed)` nor the row that leaves nothing: every row started clean, and that row
  // passed.
  expect([...failed.keys()].sort()).toEqual(Object.keys(reports).sort());
  for (const [name, { component, request }] of Object.entries(reports)) {
    expect(failed.get(name)).toContain(
      `error: requests reached the network, answered by nothing: ${request}\n` +
        "React printed 1 act message(s) in or before this row:\n" +
        `  update outside act in ${component}`
    );
  }
});
