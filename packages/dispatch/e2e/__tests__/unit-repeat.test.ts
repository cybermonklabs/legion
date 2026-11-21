/** e2e/tools/unit-repeat.ts on the junit files real Bun writes (INFRA-1511).
 *
 *  Each row writes tiny test files into a directory of its own and runs Bun over them there,
 *  with `process.execPath`, an explicit `--seed`, `--rerun-each` and Bun's junit reporter. Then
 *  it hands what Bun left behind to the tool's pure functions. The directory is outside
 *  packages/dispatch, so the package's bunfig.toml does not reach that run. HOME is the same
 *  directory, so no global bunfig of the host reaches it either. Other rows run the tool itself
 *  there instead: on an `--out` the tool must refuse before it starts Bun, and on copies of the
 *  package whose rows never end, which the tool is stopped on, or whose worker or Bun's test
 *  process is killed, or whose step GitHub's runner cancels. unit-repeat-budgets.test.ts holds
 *  the budgets of .github/workflows/dispatch-unit-repeat.yaml to the measurements beside them.
 *
 *  On each rerun Bun 1.4.4 runs the test file's own top-level code again, so a counter declared
 *  there starts again at 0. A module the file imports keeps its state between reruns (measured
 *  for INFRA-1511's plan), and so does `globalThis`. Each scratch file here is a single file with
 *  no module of its own to import, so a row that must fail on a later run counts its runs on
 *  `globalThis`.
 *
 *  Each row says which lie of a weekly result it catches.
 */

import { expect, test } from "bun:test";
import { copyFileSync, mkdirSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import {
  annotations,
  diedFiles,
  junitAt,
  type LaneRun,
  markdown,
  parseArguments,
  type Report,
  type RowlessKind,
  runningFiles,
  type Stop,
  summarise,
} from "../tools/unit-repeat";
import { rowLimit, runBounded } from "./bounded-subprocess";

const scratch = temporaryDirectories("unit-repeat");

const tool = fileURLToPath(new URL("../tools/unit-repeat.ts", import.meta.url));

const SEED = 4242;

/** Passes its first run and fails every later one. */
const failsAfterFirstRun = `import { describe, expect, test } from "bun:test";

const counter = globalThis as { flakyRuns?: number };

describe("outer", () => {
  test("fails after its first run", () => {
    counter.flakyRuns = (counter.flakyRuns ?? 0) + 1;
    expect(counter.flakyRuns).toBe(1);
  });
  test("passes", () => {});
});
`;

const passes = `import { test } from "bun:test";

test("passes one", () => {});
test("passes two", () => {});
test.skip("is skipped", () => {});
`;

interface Spawned {
  readonly code: number;
  readonly output: string;
}

/** Runs Bun in `directory` with `args`, a bounded child under the short bound, the bound of one
 *  command that starts nothing: a `bun test` over a few tiny files, with no `--parallel`, runs in
 *  one process and takes a fraction of a second. */
async function spawnBun(
  directory: string,
  args: readonly string[],
  environment: Readonly<Record<string, string>> = {}
): Promise<Spawned> {
  const { code, stderr, stdout } = await runBounded({
    argv: [process.execPath, ...args],
    bound: "short",
    cwd: directory,
    env: { HOME: directory, PATH: process.env.PATH ?? "", TMPDIR: tmpdir(), ...environment },
    from: import.meta.url,
  });
  return { code, output: `${stdout}${stderr}` };
}

/** Writes `files` into a fresh directory and runs `bun test` there as a lane run would. */
async function laneRun(
  files: Readonly<Record<string, string>>,
  reruns = 3,
  environment: Readonly<Record<string, string>> = {}
): Promise<LaneRun> {
  const directory = scratch();
  for (const [name, text] of Object.entries(files)) await Bun.write(join(directory, name), text);
  const junit = join(directory, "junit.xml");
  const started = performance.now();
  const { code, output } = await spawnBun(
    directory,
    [
      "test",
      `--seed=${SEED}`,
      `--rerun-each=${reruns}`,
      "--reporter=junit",
      `--reporter-outfile=${junit}`,
    ],
    environment
  );
  return {
    lane: "harness",
    target: "e2e/__tests__",
    seed: SEED,
    reruns,
    wallMs: performance.now() - started,
    exitCode: code,
    signal: null,
    junit: junitAt(junit),
    log: output,
    environment,
    // Bun runs here without runLane, which is what reads /proc/meminfo, so there is no reading.
    memory: [],
    stop: null,
    early: null,
  };
}

function kinds(report: Report): RowlessKind[] {
  return report.rowlessFailures.map((failure) => failure.kind);
}

test(
  "a row that fails only on its reruns is named with its count, the seed and its command",
  async () => {
    // The lie: a row that fails 2 of 3 runs goes unnamed, or is named without the seed and the
    // rerun that bring it back, so no one can reproduce it. The run carries GitHub's variables,
    // so Bun writes the `<properties>` it writes in the weekly job.
    const github = {
      GITHUB_ACTIONS: "true",
      GITHUB_EVENT_NAME: "schedule",
      GITHUB_REPOSITORY: "cybermonklabs/legion",
      GITHUB_RUN_ATTEMPT: "1",
      GITHUB_RUN_ID: "77",
      GITHUB_SERVER_URL: "https://github.com",
      GITHUB_SHA: "0123abcd",
    };
    const run = await laneRun(
      { "flaky.test.ts": failsAfterFirstRun, "steady.test.ts": passes },
      3,
      github
    );
    expect("text" in run.junit && run.junit.text.includes("<properties>")).toBe(true);
    const report = summarise(run);
    const reproduce = `bun run test:harness flaky.test.ts --seed=${SEED} --rerun-each=2`;
    expect(report.failingRows).toEqual([
      {
        file: "flaky.test.ts",
        name: "outer > fails after its first run",
        fails: 2,
        runs: 3,
        firstFailedRun: 2,
        message: expect.stringContaining("Received: 2"),
        reproduce,
      },
    ]);
    // Two files; two rows and three rows, one of them skipped. Each ran three times.
    expect(report.oneRun).toEqual({ files: 2, rows: 5, skippedRows: 1 });
    expect(report.testcases).toEqual({ passed: 10, failed: 2, skipped: 3 });
    expect(report.rowlessFailures).toEqual([]);
    expect(report.exitCode).toBe(1);
    expect(report.github.url).toBe("https://github.com/cybermonklabs/legion/actions/runs/77");

    const text = markdown(report);
    expect(text).toContain(`- Seed: \`${SEED}\``);
    expect(text).toContain(reproduce);
    expect(text).toContain(
      "[run 77, attempt 1](https://github.com/cybermonklabs/legion/actions/runs/77)"
    );
    expect(text).not.toContain("steady.test.ts");
    const errors = annotations(report).filter((line) => line.startsWith("::error "));
    expect(errors).toHaveLength(1);
    expect(errors[0]).toStartWith(
      "::error file=packages/dispatch/flaky.test.ts,title=Flaky unit test::" +
        `outer > fails after its first run failed 2/3 runs (seed ${SEED}): `
    );
  },
  rowLimit("short")
);

test(
  "a row that fails on every run is named 3 of 3, with a command that runs the file once",
  async () => {
    // The lie: a row that fails every run is called flaky, and its command asks for reruns it
    // does not need.
    const report = summarise(
      await laneRun({
        "broken.test.ts": `import { test } from "bun:test";\n\ntest("always fails", () => {\n  throw new Error("always");\n});\n`,
      })
    );
    expect(report.failingRows).toEqual([
      {
        file: "broken.test.ts",
        name: "always fails",
        fails: 3,
        runs: 3,
        firstFailedRun: 1,
        message: "always",
        reproduce: `bun run test:harness broken.test.ts --seed=${SEED}`,
      },
    ]);
    expect(report.exitCode).toBe(1);
    expect(annotations(report)[0]).toStartWith(
      "::error file=packages/dispatch/broken.test.ts,title=Failing unit test::" +
        `always fails failed 3/3 runs (seed ${SEED}): always`
    );
  },
  rowLimit("short")
);

test(
  "a junit file with no testcase is refused as a failure that belongs to no row",
  async () => {
    // The lie: a run that tested nothing reads as a clean week. Bun 1.4.4 writes no junit file
    // when no testcase ran, so this one is a real file with its suites taken out.
    const run = await laneRun({ "steady.test.ts": passes });
    if (!("text" in run.junit)) throw new Error(`Bun wrote no junit file:\n${run.log}`);
    const [declaration, root] = run.junit.text.split("\n");
    expect(root).toStartWith("<testsuites ");
    const empty = { ...run, junit: { text: `${declaration}\n${root}\n</testsuites>\n` } };
    expect(run.exitCode).toBe(0);
    const report = summarise(empty);
    expect(kinds(report)).toEqual(["no-testcase"]);
    expect(report.failingRows).toEqual([]);
    expect(report.exitCode).toBe(1);
  },
  rowLimit("short")
);

test(
  "a missing junit file is refused as a failure that belongs to no row",
  async () => {
    // The lie: Bun runs no test, exits 0 and writes no junit file, and the report reads that as
    // no failure.
    const run = await laneRun({
      "empty.test.ts": `import { describe } from "bun:test";\n\ndescribe("holds no test", () => {});\n`,
    });
    expect(run.exitCode).toBe(0);
    expect("unreadable" in run.junit).toBe(true);
    const report = summarise(run);
    expect(kinds(report)).toEqual(["junit-unreadable"]);
    expect(report.exitCode).toBe(1);
  },
  rowLimit("short")
);

test(
  "a non-zero exit of Bun with no failing testcase is a failure that belongs to no row",
  async () => {
    // The lie: a file that fails to load leaves no testcase in the junit file. Every testcase
    // there passed, so the report would read as clean although Bun failed.
    const run = await laneRun({
      "steady.test.ts": passes,
      "throws-on-load.test.ts": `import { test } from "bun:test";\n\ntest("never runs", () => {});\nthrow new Error("the file fails to load");\n`,
    });
    expect(run.exitCode).not.toBe(0);
    const report = summarise(run);
    expect(report.failingRows).toEqual([]);
    expect(kinds(report)).toEqual(["bun-exit", "bun-errors"]);
    expect(report.bunLogTail.join("\n")).toContain("the file fails to load");
    expect(report.exitCode).toBe(1);
    const errors = annotations(report).filter((line) => line.startsWith("::error "));
    expect(errors).toHaveLength(2);
    expect(errors[0]).toStartWith(
      "::error title=Failure outside any test::Bun exited with status 1"
    );
  },
  rowLimit("short")
);

test(
  "a file whose rows did not all run every time is a failure that belongs to no row",
  async () => {
    // The lie: a row that runs on fewer reruns than asked reads as passing all of them.
    const run = await laneRun({
      "first-load-only.test.ts": `import { test } from "bun:test";\n\nconst counter = globalThis as { loads?: number };\ncounter.loads = (counter.loads ?? 0) + 1;\ntest("always here", () => {});\nif (counter.loads === 1) test("only on the first run", () => {});\n`,
    });
    expect(run.exitCode).toBe(0);
    const report = summarise(run);
    expect(kinds(report)).toEqual(["uneven-file"]);
    expect(report.exitCode).toBe(1);
  },
  rowLimit("short")
);

test(
  "a row whose name holds &, < and a double quote is reported by its real name",
  async () => {
    // The lie: the report names a row as the junit file escapes it, which no one can search for.
    const report = summarise(
      await laneRun(
        {
          "names.test.ts": `import { describe, test } from "bun:test";\n\ndescribe("a <describe>", () => {\n  test('keeps & < and " in its name', () => {\n    throw new Error('bad & <x> "y"');\n  });\n});\n`,
        },
        1
      )
    );
    expect(report.failingRows.map(({ name, message }) => ({ name, message }))).toEqual([
      { name: 'a <describe> > keeps & < and " in its name', message: 'bad & <x> "y"' },
    ]);
    expect(markdown(report)).toContain('`a <describe> > keeps & < and " in its name`');
  },
  rowLimit("short")
);

test(
  "a row that throws an error with no message is reported with the error and the line that threw it",
  async () => {
    // The lie: Bun writes no message for an error that has none, and writes its text and stack
    // only as the failure's body. A report that reads only the message names the row and says
    // nothing of what failed or where.
    const report = summarise(
      await laneRun(
        {
          "no-message.test.ts": `import { test } from "bun:test";\n\ntest("throws an error with no message", () => {\n  throw new Error("");\n});\n`,
        },
        1
      )
    );
    expect(report.failingRows).toEqual([
      {
        file: "no-message.test.ts",
        name: "throws an error with no message",
        fails: 1,
        runs: 1,
        firstFailedRun: 1,
        message: expect.stringMatching(/^Error\n\s+at no-message\.test\.ts:4:\d+$/),
        reproduce: `bun run test:harness no-message.test.ts --seed=${SEED}`,
      },
    ]);
    expect(report.rowlessFailures).toEqual([]);
  },
  rowLimit("short")
);

test(
  "the annotation of a row escapes the newline and the percent sign of its message",
  async () => {
    // The lie: GitHub cuts an annotation at its first raw newline, and reads a raw % as the start
    // of an escape, so the message it shows is not the failure's.
    const report = summarise(
      await laneRun(
        {
          "lines.test.ts": `import { test } from "bun:test";\n\ntest("fails on two lines", () => {\n  throw new Error("first line\\nsecond line, 100% sure");\n});\n`,
        },
        1
      )
    );
    const [error] = annotations(report);
    expect(error).toBe(
      "::error file=packages/dispatch/lines.test.ts,title=Failing unit test::" +
        `fails on two lines failed 1/1 runs (seed ${SEED}): first line%0Asecond line, 100%25 sure`
    );
    expect(annotations(report).every((line) => !line.includes("\n"))).toBe(true);
  },
  rowLimit("short")
);

test(
  "past GitHub's 10 errors a step, the failure outside any test comes first and the last error counts the rows left out",
  async () => {
    // The lie: GitHub keeps 10 error annotations a step and drops the rest with no word. A
    // failure outside any test printed after the rows is dropped with them, and the rows past the
    // tenth error vanish from the run's page.
    const run = await laneRun({ "steady.test.ts": passes }, 1);
    const report: Report = {
      ...summarise(run),
      failingRows: Array.from({ length: 12 }, (_, index) => ({
        file: "broken.test.ts",
        name: `row ${index + 1}`,
        fails: 1,
        runs: 1,
        firstFailedRun: 1,
        message: "broken",
        reproduce: `bun run test:harness broken.test.ts --seed=${SEED}`,
      })),
      rowlessFailures: [{ kind: "bun-errors", text: "Bun counted 1 error outside any test." }],
    };
    expect(annotations(report)).toEqual([
      `::error title=Failure outside any test::Bun counted 1 error outside any test. (seed ${SEED})`,
      ...[1, 2, 3, 4, 5, 6, 7, 8].map(
        (count) =>
          "::error file=packages/dispatch/broken.test.ts,title=Failing unit test::" +
          `row ${count} failed 1/1 runs (seed ${SEED}): broken`
      ),
      "::error title=More failures in report.md::" +
        `report.md lists 4 more failing rows: GitHub keeps 10 errors a step (seed ${SEED}).`,
      expect.stringMatching(/^::notice /),
    ]);
  },
  rowLimit("short")
);

test(
  "the report gives the machine's memory from its readings, and says when there were none",
  async () => {
    // The lie: a run that took no reading shows 0 MiB, which reads as a measurement. Or the
    // report takes the first or the last reading, not the lowest MemAvailable, and shows less
    // than the most memory in use.
    const run = await laneRun({ "steady.test.ts": passes }, 1);
    const gibibyte = 1024 * 1024; // in KiB, as /proc/meminfo counts
    const measured = summarise({
      ...run,
      memory: [
        { memTotalKiB: 16 * gibibyte, memAvailableKiB: 12 * gibibyte },
        { memTotalKiB: 16 * gibibyte, memAvailableKiB: 6 * gibibyte },
        { memTotalKiB: 16 * gibibyte, memAvailableKiB: 10 * gibibyte },
      ],
    });
    expect(measured.machineMemory).toEqual({
      memTotalMiB: 16384,
      lowestAvailableMiB: 6144,
      mostInUseMiB: 10240,
      samples: 3,
    });
    const reading =
      "MemTotal 16384 MiB, lowest MemAvailable 6144 MiB, so 10240 MiB in use at the most, " +
      "from 3 readings of /proc/meminfo, one at the start and then one every 5 s";
    expect(markdown(measured)).toContain(
      `- Machine memory, which counts every process on the machine and not Bun's alone: ${reading}`
    );
    expect(annotations(measured).at(-1)).toContain(reading);

    const unmeasured = summarise({ ...run, memory: [] });
    expect(unmeasured.machineMemory).toBeNull();
    for (const text of [markdown(unmeasured), annotations(unmeasured).at(-1) ?? ""]) {
      expect(text).toContain("not measured, because /proc/meminfo could not be read");
      expect(text).not.toMatch(/\b0 MiB/);
    }
  },
  rowLimit("short")
);

test(
  "every seed the tool accepts, Bun accepts, and the tool refuses the first one past them",
  async () => {
    // The lie: the tool draws a seed Bun refuses, so the weekly run stops before any test.
    const options = (seed: string) =>
      parseArguments(["--lane", "web", "--out", "out", "--seed", seed], () => 0);
    expect(options("0")).toEqual({ options: { lane: "web", out: "out", reruns: 20, seed: 0 } });
    expect(options("4294967295")).toMatchObject({ options: { seed: 4294967295 } });
    for (const refused of ["4294967296", "-1", "1.5", "0x10", ""]) {
      expect(options(refused)).toHaveProperty("problem");
    }
    expect(parseArguments(["--lane", "web", "--out", "out"], () => 7)).toMatchObject({
      options: { seed: 7 },
    });

    const directory = scratch();
    await Bun.write(join(directory, "steady.test.ts"), passes);
    for (const seed of ["0", "4294967295"]) {
      expect((await spawnBun(directory, ["test", `--seed=${seed}`])).code).toBe(0);
    }
    expect((await spawnBun(directory, ["test", "--seed=4294967296"])).code).not.toBe(0);
  },
  // The three runs of Bun, one after another, each a bounded child under the short bound.
  rowLimit("short", "short", "short")
);

test(
  "an --out that names a file is refused as a bad command line, and the file is left as it was",
  async () => {
    // The lie: the tool throws on an --out that names a file and exits 1, the status of a run
    // whose rows failed, although no test ran. Or it writes over the file it was given.
    const directory = scratch();
    const file = join(directory, "a-file");
    const text = "this file is no directory\n";
    await Bun.write(file, text);
    const { code, output } = await spawnBun(directory, [
      tool,
      "--lane",
      "web",
      "--reruns",
      "1",
      "--seed",
      "1",
      "--out",
      file,
    ]);
    expect(code).toBe(2);
    expect(output).toContain(`unit-repeat: --out ${file} cannot be the output directory: EEXIST`);
    expect(output).toContain("usage: bun run test:repeat --lane <web|web-cold|harness>");
    expect(output).not.toContain("seed=1 lane=web");
    expect(await Bun.file(file).text()).toBe(text);
  },
  rowLimit("short")
);

/** One line of a progress file, as e2e/tools/unit-repeat-progress.ts writes it. */
function progressLine(fields: Readonly<Record<string, string | number | boolean>>): string {
  return JSON.stringify(fields);
}

test(
  "a stop names each file a worker was in, with its run and the row it was in, and passes over a finished file",
  () => {
    // The lie: a leg stopped at its deadline reads as one nameless timeout, or names a file every
    // run of which had ended, or names the row in hand by the wrong place in the seed's order, or
    // takes a row whose `end` a throwing afterEach skipped for a row still running.
    const lines = [
      // Worker 1 ran a.test.ts's first run to its end, then began the second run's rows: row 2 of
      // run 2 is in hand.
      progressLine({ event: "load", load: "a", at: 1_000, file: "a.test.ts", pid: 1 }),
      progressLine({ event: "run", load: "a", at: 1_001 }),
      progressLine({ event: "start", load: "a", at: 1_002 }),
      progressLine({ event: "end", load: "a", at: 1_003 }),
      progressLine({ event: "done", load: "a", at: 1_004 }),
      progressLine({ event: "run", load: "a", at: 1_005 }),
      progressLine({ event: "start", load: "a", at: 1_006 }),
      progressLine({ event: "end", load: "a", at: 1_007 }),
      progressLine({ event: "start", load: "a", at: 4_000 }),
      // Worker 2 finished b.test.ts's two runs and loaded c.test.ts, whose first row ended: a
      // hook or the code between rows holds it.
      progressLine({ event: "load", load: "b", at: 1_000, file: "b.test.ts", pid: 2 }),
      progressLine({ event: "run", load: "b", at: 1_001 }),
      progressLine({ event: "done", load: "b", at: 1_002 }),
      progressLine({ event: "run", load: "b", at: 1_003 }),
      progressLine({ event: "done", load: "b", at: 1_004 }),
      progressLine({ event: "load", load: "c", at: 2_000, file: "c.test.ts", pid: 2 }),
      progressLine({ event: "run", load: "c", at: 2_001 }),
      progressLine({ event: "start", load: "c", at: 2_002 }),
      progressLine({ event: "end", load: "c", at: 7_000 }),
      // Worker 3 finished d.test.ts alone: no file of its is running.
      progressLine({ event: "load", load: "d", at: 1_000, file: "d.test.ts", pid: 3 }),
      progressLine({ event: "run", load: "d", at: 1_001 }),
      progressLine({ event: "done", load: "d", at: 1_002 }),
      progressLine({ event: "run", load: "d", at: 1_003 }),
      progressLine({ event: "done", load: "d", at: 1_004 }),
      // Worker 4 is in e.test.ts's own code: no run of it has begun.
      progressLine({ event: "load", load: "e", at: 9_000, file: "e.test.ts", pid: 4 }),
      // Worker 5 died in f.test.ts's first row before the stop, a crash of its own or a kill from
      // outside Bun: the stop names it as a file its worker died in, and not as running.
      progressLine({ event: "load", load: "f", at: 1_000, file: "f.test.ts", pid: 5 }),
      progressLine({ event: "run", load: "f", at: 1_001 }),
      progressLine({ event: "start", load: "f", at: 1_002 }),
      // The last line of a file read while a worker wrote it can be cut short.
      '{"event":"sta',
      // Worker 6: g.test.ts's first row's afterEach threw, which skips the preload's `end`, then
      // its second row began and ended, and a hook after it holds the worker. The second start
      // shows that the first row ended.
      progressLine({ event: "load", load: "g", at: 5_000, file: "g.test.ts", pid: 6 }),
      progressLine({ event: "run", load: "g", at: 5_001 }),
      progressLine({ event: "start", load: "g", at: 5_002 }),
      progressLine({ event: "start", load: "g", at: 5_003 }),
      progressLine({ event: "end", load: "g", at: 5_500 }),
      // Worker 7: h.test.ts runs its rows at once; of two begun, one ended.
      progressLine({
        event: "load",
        load: "h",
        at: 8_000,
        file: "h.test.ts",
        pid: 7,
        atOnce: true,
      }),
      progressLine({ event: "run", load: "h", at: 8_001 }),
      progressLine({ event: "start", load: "h", at: 8_002 }),
      progressLine({ event: "start", load: "h", at: 8_003 }),
      progressLine({ event: "end", load: "h", at: 8_004 }),
    ];
    const stop: Stop = {
      signal: "SIGTERM",
      at: 10_000,
      afterMs: 9_500,
      afterBun: false,
      progress: { text: lines.join("\n") },
      alive: [1, 2, 3, 4, 6, 7],
      names: {
        "a.test.ts": { names: ["a one", "outer > a two", "a three"] },
        "c.test.ts": { unreadable: "the run that reads its rows' names exited 1" },
        "g.test.ts": { names: ["g one", "g two"] },
        "f.test.ts": { names: ["f one", "f two"] },
      },
      bunEnd: null,
      namesRuns: {},
    };
    const lane = { reruns: 2, seed: SEED, script: "test:harness" };
    expect(runningFiles(stop, lane)).toEqual([
      {
        file: "a.test.ts",
        run: 2,
        place: "row",
        rowsBegun: 2,
        rowsRunning: 0,
        rowName: "outer > a two",
        nameUnread: null,
        quietSeconds: 6,
        reproduce: `bun run test:harness a.test.ts --seed=${SEED} --rerun-each=2`,
      },
      {
        file: "g.test.ts",
        run: 1,
        place: "after-row",
        rowsBegun: 2,
        rowsRunning: 0,
        rowName: "g two",
        nameUnread: null,
        quietSeconds: 4.5,
        reproduce: `bun run test:harness g.test.ts --seed=${SEED}`,
      },
      {
        file: "c.test.ts",
        run: 1,
        place: "after-row",
        rowsBegun: 1,
        rowsRunning: 0,
        rowName: null,
        nameUnread: "the run that reads its rows' names exited 1",
        quietSeconds: 3,
        reproduce: `bun run test:harness c.test.ts --seed=${SEED}`,
      },
      {
        file: "h.test.ts",
        run: 1,
        place: "rows-at-once",
        rowsBegun: 2,
        rowsRunning: 1,
        rowName: null,
        nameUnread: null,
        quietSeconds: 2,
        reproduce: `bun run test:harness h.test.ts --seed=${SEED}`,
      },
      {
        file: "e.test.ts",
        run: 1,
        place: "loading",
        rowsBegun: 0,
        rowsRunning: 0,
        rowName: null,
        nameUnread: null,
        quietSeconds: 1,
        reproduce: `bun run test:harness e.test.ts --seed=${SEED}`,
      },
    ]);
    expect(diedFiles({ stop, early: null }, lane)).toEqual([
      {
        file: "f.test.ts",
        run: 1,
        place: "row",
        rowsBegun: 1,
        rowsRunning: 0,
        rowName: "f one",
        nameUnread: null,
        quietSeconds: 9,
        reproduce: `bun run test:harness f.test.ts --seed=${SEED}`,
      },
    ]);

    // The report of that stop: Bun wrote no junit file and exited 143, which the stop explains,
    // so the one failure outside any test is the stop, and each running file has an error of its
    // own, the row's name in it.
    const report = summarise({
      lane: "harness",
      target: "e2e/__tests__",
      seed: SEED,
      reruns: 2,
      wallMs: 9_600,
      exitCode: 143,
      signal: null,
      junit: { unreadable: "Bun wrote no junit file: junit.xml is missing." },
      log: "",
      environment: {},
      memory: [],
      stop,
      early: null,
    });
    expect(kinds(report)).toEqual(["stopped", "worker-died"]);
    expect(report.exitCode).toBe(1);
    const errors = annotations(report).filter((line) => line.startsWith("::error "));
    expect(errors).toEqual([
      expect.stringContaining("The leg was stopped by SIGTERM after 9.5 s, before Bun ended"),
      expect.stringContaining("1 test file was cut short before the stop"),
      "::error file=packages/dispatch/a.test.ts,title=Running when the leg was stopped::a.test.ts: " +
        'in row 2, "outer > a two", or a hook after it, of run 2 of 2, 6.0 s since it began, when ' +
        `SIGTERM stopped the leg after 9.5 s (seed ${SEED}). Run it again from packages/dispatch: ` +
        `bun run test:harness a.test.ts --seed=${SEED} --rerun-each=2`,
      expect.stringContaining('g.test.ts: in run 1 of 2, after row 2, "g two", ended'),
      expect.stringContaining(
        "c.test.ts: in run 1 of 2, after row 1, whose name was not read (the run that reads its " +
          "rows' names exited 1), ended"
      ),
      expect.stringContaining(
        "h.test.ts: in run 1 of 2, in 1 row of the 2 begun, which the file runs at once"
      ),
      expect.stringContaining("e.test.ts: in run 1 of 2, before it began"),
      "::error file=packages/dispatch/f.test.ts,title=Running when its worker died::f.test.ts: in " +
        'row 1, "f one", or a hook after it, of run 1 of 2, 9.0 s since it began, when its worker ' +
        `or Bun's test process died (seed ${SEED}). Run it again from packages/dispatch: ` +
        `bun run test:harness f.test.ts --seed=${SEED}`,
    ]);
    expect(markdown(report)).toContain("### The leg was stopped while 5 test files were running");
    expect(markdown(report)).toContain(
      "### 1 test file was cut short when a worker or Bun's test process died"
    );

    // Bun's junit file after a stop: a `(worker crashed)` testcase says Bun had not taken in that
    // file's results when the stop came. It is named as such, and no failing row is made of it,
    // unless its worker died before the stop: then the file is the one it died in.
    const crashed = (file: string) =>
      `  <testsuite name="${file}" file="${file}" tests="1" assertions="0" failures="1" skipped="0" time="1" hostname="runner">\n` +
      `    <testcase name="(worker crashed)" classname="" time="0" file="${file}" assertions="0">\n` +
      `      <failure type="Error" message="worker process crashed before reporting results" />\n` +
      "    </testcase>\n  </testsuite>\n";
    const withJunit = summarise({
      lane: "harness",
      target: "e2e/__tests__",
      seed: SEED,
      reruns: 2,
      wallMs: 9_600,
      exitCode: 143,
      signal: null,
      junit: {
        text:
          '<?xml version="1.0" encoding="UTF-8"?>\n' +
          '<testsuites name="bun test" tests="2" assertions="0" failures="2" skipped="0" time="1">\n' +
          `${crashed("a.test.ts")}${crashed("f.test.ts")}</testsuites>\n`,
      },
      log: "",
      environment: {},
      memory: [],
      stop,
      early: null,
    });
    expect(withJunit.failingRows).toEqual([]);
    expect(withJunit.stop?.unreported).toEqual(["a.test.ts"]);
    expect(withJunit.died.map((file) => file.file)).toEqual(["f.test.ts"]);
    expect(kinds(withJunit)).toEqual(["stopped", "worker-died"]);
    expect(withJunit.rowlessFailures[0].text).toEndWith(
      " Bun had not reported the results of 1 file when the stop came: a.test.ts."
    );

    // A bound that ends a run is named: Bun's grace after the stop, and the bound of a run that
    // reads a file's names. One that does not is given with its time.
    const bounded = summarise({
      lane: "harness",
      target: "e2e/__tests__",
      seed: SEED,
      reruns: 2,
      wallMs: 9_600,
      exitCode: null,
      signal: "SIGKILL",
      junit: { unreadable: "Bun wrote no junit file: junit.xml is missing." },
      log: "",
      environment: {},
      memory: [],
      stop: {
        ...stop,
        bunEnd: { ms: 3_004, boundMs: 3_000, killed: true },
        namesRuns: {
          "a.test.ts": { ms: 812, boundMs: 2_000, killed: false, read: false },
          "g.test.ts": { ms: 2_003, boundMs: 2_000, killed: true, read: false },
        },
      },
      early: null,
    });
    expect(bounded.rowlessFailures[0].text).toEndWith(
      " Bun did not end within its 3 s grace after the stop, so the tool killed its process " +
        "group, and its junit file can lack rows that ended."
    );
    const text = markdown(bounded);
    expect(text).toContain("- Bun, after the stop, was killed at its 3.0 s bound");
    expect(text).toContain(
      "- The run that read the names of `a.test.ts`'s rows ended after 0.8 s of its 2.0 s bound"
    );
    expect(text).toContain(
      "- The run that read the names of `g.test.ts`'s rows was killed at its 2.0 s bound"
    );
  },
  rowLimit()
);

/** What a leg the tool was stopped on left behind. */
interface StoppedLeg {
  readonly code: number;
  readonly stdout: string;
  readonly report: Report;
  /** How many processes had the copy package as their directory once the tool had ended. */
  readonly leftBehind: number;
}

/** Copies the tool and its preload into `directory` as a package of `files` under
 *  `e2e/__tests__`, with a `test:harness` of two workers whatever the machine's CPUs. Runs the
 *  tool there as the harness lane with two reruns, and sends the tool alone the SIGTERM the
 *  step's `timeout` sends, once every one of `marks` exists, or once the tool has ended first.
 *  Then counts the processes whose directory is the package, for up to 2 s while any killed one
 *  is still dying, in the driver itself: the bounded child's own kill of what it left would hide
 *  them. `bounds` are the tool's two stop bounds, which the caller sets so that its verdict
 *  rests on its marks and not on how long Bun or a names run takes. `how.hold`, where given, is a
 *  preload of the tool's own process, and a file the driver writes once it has sent the SIGTERM,
 *  which that preload can wait for. `how.killFrom`, where given, is a file that holds a pid: the
 *  driver sends that process a SIGKILL in place of the SIGTERM to the tool, as the out-of-memory
 *  killer would, and the tool hears of no stop. */
async function stoppedLeg(
  directory: string,
  files: Readonly<Record<string, string>>,
  marks: readonly string[],
  bounds: { readonly graceMs: number; readonly namesMs: number },
  how: {
    readonly hold?: { readonly preload: string; readonly release: string };
    readonly killFrom?: string;
  } = {}
): Promise<StoppedLeg> {
  const { hold, killFrom } = how;
  const tools = join(directory, "e2e", "tools");
  const rows = join(directory, "e2e", "__tests__");
  mkdirSync(tools, { recursive: true });
  mkdirSync(rows, { recursive: true });
  for (const name of ["unit-repeat.ts", "unit-repeat-progress.ts"]) {
    copyFileSync(fileURLToPath(new URL(`../tools/${name}`, import.meta.url)), join(tools, name));
  }
  await Bun.write(
    join(directory, "package.json"),
    `${JSON.stringify({ name: "stopped-leg", private: true, scripts: { "test:harness": "bun test --parallel=2 --timeout 60000" } })}\n`
  );
  for (const [name, text] of Object.entries(files)) await Bun.write(join(rows, name), text);
  const out = join(directory, "out");
  const driver =
    '"$@" & tool=$!; ready() { for m in $MARKS; do [ -e "$m" ] || return 1; done; }; ' +
    'until ready || ! kill -0 "$tool" 2>/dev/null; do sleep 0.05; done; ' +
    'if [ -n "$KILL_FROM" ]; then kill -KILL "$(cat "$KILL_FROM")" 2>/dev/null; else kill -TERM "$tool" 2>/dev/null; fi; ' +
    'if [ -n "$RELEASE" ]; then : >"$RELEASE"; fi; wait "$tool"; code=$?; ' +
    // One `find`, run from `/` so that its own directory is not the package's, lists every
    // process whose directory is; the driver itself is left out. A process the kill took can take
    // a moment to die, so the count is read again for up to 2 s while it is not 0.
    'dir=$PWD; for i in $(seq 40); do left=$(cd / && find /proc -mindepth 2 -maxdepth 2 -name cwd -lname "$dir" 2>/dev/null | grep -cvx "/proc/$$/cwd"); ' +
    '[ "$left" = 0 ] && break; sleep 0.05; done; echo "left behind: $left"; exit "$code"';
  const { code, stdout } = await runBounded({
    argv: [
      "bash",
      "-c",
      driver,
      "driver",
      process.execPath,
      ...(hold === undefined ? [] : ["--preload", hold.preload]),
      join(tools, "unit-repeat.ts"),
      "--lane",
      "harness",
      "--reruns",
      "2",
      "--seed",
      String(SEED),
      "--out",
      out,
    ],
    bound: "stoppedLeg",
    cwd: directory,
    env: {
      HOME: directory,
      PATH: process.env.PATH ?? "",
      TMPDIR: tmpdir(),
      GITHUB_ACTIONS: "true",
      MARKS: marks.join(" "),
      RELEASE: hold?.release ?? "",
      KILL_FROM: killFrom ?? "",
      DISPATCH_UNIT_REPEAT_STOP_GRACE_MS: String(bounds.graceMs),
      DISPATCH_UNIT_REPEAT_NAMES_BOUND_MS: String(bounds.namesMs),
    },
    from: import.meta.url,
  });
  const left = /^left behind: (\d+)$/m.exec(stdout);
  if (left === null) throw new Error(`the driver counted no process left behind:\n${stdout}`);
  return {
    code,
    stdout,
    report: JSON.parse(readFileSync(join(out, "report.json"), "utf8")),
    leftBehind: Number(left[1]),
  };
}

/** A row that writes `mark` and then spins without yielding, which no timer of Bun's ends. */
function spinningRow(row: string, mark: string): string {
  return `test(${JSON.stringify(row)}, () => {\n  writeFileSync(${JSON.stringify(mark)}, "");\n  for (;;) {}\n});\n`;
}

const spinningHead = `import { afterAll, test } from "bun:test";\nimport { writeFileSync } from "node:fs";\n\n`;

test(
  "a leg stopped while rows never end names each of them and exits 1",
  async () => {
    // The lie: the leg that reaches its deadline is ended from outside and names nothing, as
    // three web-cold legs did at their two-minute budget. Here the tool itself, on a copy of the
    // package with two files whose rows spin without yielding, gets the SIGTERM `timeout` sends
    // at the deadline, once both rows have begun. Both workers are in a row at that moment
    // whatever the machine's load: an earlier form of this row stopped the leg while a file of
    // passing rows could still be running or not, and failed 6 of 120 runs in the harness legs of
    // runs 44751162563 and 44751677485 when that file's worker was still in it. The row sets both
    // of the tool's stop bounds to 120 s, past its driver's own `stoppedLeg` bound of 90 s, so
    // that neither decides its verdict: a review held the names run 25 s and Bun's end 15 s under
    // the tool's defaults, and the row failed on what each bound then cut short.
    const directory = scratch();
    const marks = [join(directory, "spins-began"), join(directory, "beside-began")];
    const { code, stdout, report, leftBehind } = await stoppedLeg(
      directory,
      {
        "spins.test.ts": `${spinningHead}test("before the spin", () => {});\n${spinningRow("spins for ever", marks[0])}test("after the spin", () => {});\n`,
        "beside.test.ts": `${spinningHead}${spinningRow("spins beside it", marks[1])}`,
      },
      marks,
      { graceMs: 120_000, namesMs: 120_000 }
    );
    expect(code).toBe(1);
    expect(report.stop?.signal).toBe("SIGTERM");
    const running = [...(report.stop?.running ?? [])].sort((one, other) =>
      one.file.localeCompare(other.file)
    );
    expect(running).toEqual([
      expect.objectContaining({
        file: "e2e/__tests__/beside.test.ts",
        run: 1,
        place: "row",
        rowsBegun: 1,
        rowName: "spins beside it",
      }),
      expect.objectContaining({
        file: "e2e/__tests__/spins.test.ts",
        run: 1,
        place: "row",
        rowName: "spins for ever",
      }),
    ]);
    // Bun's own `(worker crashed)` testcase for each file the stop ended says the stop came
    // before its results: the report names the file so, and makes no failing row of it.
    expect(report.failingRows).toEqual([]);
    expect([...(report.stop?.unreported ?? [])].sort()).toEqual([
      "e2e/__tests__/beside.test.ts",
      "e2e/__tests__/spins.test.ts",
    ]);
    expect(kinds(report)).toEqual(["stopped"]);
    expect(report.stop?.bunEnd?.killed).toBe(false);
    expect(stdout).toMatch(
      /^::error file=packages\/dispatch\/e2e\/__tests__\/spins\.test\.ts,title=Running when the leg was stopped::e2e\/__tests__\/spins\.test\.ts: in row [1-3], "spins for ever", or a hook after it, of run 1 of 2, /m
    );
    expect(stdout).toMatch(
      /^::error file=packages\/dispatch\/e2e\/__tests__\/beside\.test\.ts,title=Running when the leg was stopped::e2e\/__tests__\/beside\.test\.ts: in row 1, "spins beside it", or a hook after it, of run 1 of 2, /m
    );
    expect(leftBehind).toBe(0);
    // The record of where each file was stays beside the report.
    expect(readFileSync(join(directory, "out", "progress.jsonl"), "utf8")).toContain(
      '"event":"start"'
    );
  },
  rowLimit("stoppedLeg")
);

test(
  "a names run that hangs in an afterAll is ended once it has printed the row's name, with its whole process group, and leaves no process behind",
  async () => {
    // The lie: the tool kills only the `bun run` it started, so Bun's test process and its worker
    // live on after the tool exits, spinning at full CPU with init as their parent; or it waits
    // for a names run that hangs in a hook after printing the name it needs, so a clock decides
    // whether the name is read. Here the leg's one file ends its row and then spins in its
    // afterAll, so the run that reads its rows' names, which still runs the file's hooks, prints
    // the row's name and then spins there too. The tool must end that run once it has read the
    // name, whatever the machine's load. Both bounds are past the row's own, so neither decides.
    const directory = scratch();
    const mark = join(directory, "hook-began");
    const { code, report, leftBehind } = await stoppedLeg(
      directory,
      {
        "hook.test.ts": `${spinningHead}test("ends before its hook", () => {});\nafterAll(() => {\n  writeFileSync(${JSON.stringify(mark)}, "");\n  for (;;) {}\n});\n`,
      },
      [mark],
      { graceMs: 120_000, namesMs: 120_000 }
    );
    expect(code).toBe(1);
    expect(report.stop?.running).toEqual([
      expect.objectContaining({
        file: "e2e/__tests__/hook.test.ts",
        run: 1,
        place: "after-row",
        rowsBegun: 1,
        rowName: "ends before its hook",
        nameUnread: null,
      }),
    ]);
    expect(report.stop?.namesRuns["e2e/__tests__/hook.test.ts"]).toEqual(
      expect.objectContaining({ boundMs: 120_000, killed: false, read: true })
    );
    expect(report.stop?.bunEnd?.killed).toBe(false);
    expect(leftBehind).toBe(0);
  },
  rowLimit("stoppedLeg")
);

test(
  "a names run that never prints the row's name is killed at its bound with its whole process group",
  async () => {
    // The lie: a names run that hangs before it prints a name runs on past the tool, or keeps
    // the report waiting. Here the leg's one file spins in its own code in the names run alone
    // (the tool marks that run with DISPATCH_UNIT_REPEAT_NAMES_ONLY), so that run can never
    // print the row's name, and only its 2 s bound ends it, whatever the machine's load.
    const directory = scratch();
    const mark = join(directory, "row-began");
    const { code, report, leftBehind } = await stoppedLeg(
      directory,
      {
        "never.test.ts": `${spinningHead}if (process.env.DISPATCH_UNIT_REPEAT_NAMES_ONLY === "1") {\n  for (;;) {}\n}\n${spinningRow("spins in the leg", mark)}`,
      },
      [mark],
      { graceMs: 120_000, namesMs: 2_000 }
    );
    expect(code).toBe(1);
    expect(report.stop?.running).toEqual([
      expect.objectContaining({
        file: "e2e/__tests__/never.test.ts",
        run: 1,
        place: "row",
        rowsBegun: 1,
        rowName: null,
        nameUnread:
          "the run that reads its rows' names was killed at its 2 s bound and printed no row of it",
      }),
    ]);
    expect(report.stop?.namesRuns["e2e/__tests__/never.test.ts"]).toEqual(
      expect.objectContaining({ boundMs: 2_000, killed: true, read: false })
    );
    expect(leftBehind).toBe(0);
  },
  rowLimit("stoppedLeg")
);

/** A preload of the tool's own process that holds it between Bun's end and its report: at the
 *  tool's first read of bun.log, which comes once Bun has ended, it writes `mark` and waits until
 *  `release` exists, which the driver writes once it has sent its SIGTERM. Wrapping the module's
 *  `readFileSync` reaches the tool's own import of it (measured on Bun 1.4.4). The wait is
 *  bounded by the row's own bound, which ends the driver and the tool with it. */
function holdsTheReport(mark: string, release: string): string {
  return (
    'const fs = require("node:fs");\n' +
    "const read = fs.readFileSync;\n" +
    "let held = false;\n" +
    "fs.readFileSync = function (path, ...rest) {\n" +
    '  if (!held && String(path).endsWith("/bun.log")) {\n' +
    "    held = true;\n" +
    `    fs.writeFileSync(${JSON.stringify(mark)}, "");\n` +
    `    while (!fs.existsSync(${JSON.stringify(release)})) Bun.sleepSync(20);\n` +
    "  }\n" +
    "  return read.call(this, path, ...rest);\n" +
    "};\n"
  );
}

test(
  "a stop that comes after Bun has ended, while the tool writes its report, is named in the report",
  async () => {
    // The lie: the tool stops listening for the deadline's SIGTERM once Bun has ended, so a
    // SIGTERM that lands while it writes its report ends it at once, with no report, and the
    // step fails as a nameless timeout. A review of bf15b119a9 found that window. Here a preload
    // in the tool's own process holds the tool at its read of bun.log, after Bun has ended and
    // before the report is written, until the driver has sent the SIGTERM. The leg's one row
    // ends by itself, so Bun's results are whole, and the report must keep them and name the
    // stop beside them.
    const directory = scratch();
    const mark = join(directory, "report-held");
    const release = join(directory, "report-released");
    const preload = join(directory, "holds-the-report.js");
    await Bun.write(preload, holdsTheReport(mark, release));
    const { code, stdout, report, leftBehind } = await stoppedLeg(
      directory,
      { "ends.test.ts": 'import { test } from "bun:test";\n\ntest("ends by itself", () => {});\n' },
      [mark],
      { graceMs: 120_000, namesMs: 120_000 },
      { hold: { preload, release } }
    );
    expect(code).toBe(1);
    expect(report.stop).toEqual(
      expect.objectContaining({
        signal: "SIGTERM",
        afterBun: true,
        running: [],
        unreported: [],
        bunEnd: null,
      })
    );
    expect(report.testcases).toEqual({ passed: 2, failed: 0, skipped: 0 });
    expect(report.failingRows).toEqual([]);
    expect(kinds(report)).toEqual(["stopped"]);
    expect(stdout).toMatch(
      /^unit-repeat: SIGTERM after [0-9.]+ s, after Bun had ended by itself: the report names the stop$/m
    );
    expect(stdout).toMatch(
      new RegExp(
        "^::error title=Failure outside any test::The leg was stopped by SIGTERM after [0-9.]+ s, " +
          "after Bun had ended by itself, while the tool wrote this report: no test file was " +
          `running, and every result Bun wrote stands\\. \\(seed ${SEED}\\)$`,
        "m"
      )
    );
    expect(readFileSync(join(directory, "out", "report.md"), "utf8")).toContain(
      "### The leg was stopped after Bun had ended, and no test file was running"
    );
    expect(leftBehind).toBe(0);
  },
  rowLimit("stoppedLeg")
);

/** A test file of one row that writes the pid `of` gives into `pidFile`, whole, and then spins
 *  without yielding: `process.pid` for its worker, `process.ppid` for Bun's test process. Bun
 *  1.4.4 under `--parallel=2` runs a lane of one file in its test process itself, and starts
 *  workers (`bun test --test-worker`) only for two files or more (measured on 2026-11-21), so a
 *  row that kills one puts `besideIt` beside it. */
function killedRow(pidFile: string, of: "process.pid" | "process.ppid"): string {
  const written = `${pidFile}.writing`;
  return (
    'import { test } from "bun:test";\n' +
    'import { renameSync, writeFileSync } from "node:fs";\n\n' +
    'test("is killed", () => {\n' +
    `  writeFileSync(${JSON.stringify(written)}, String(${of}));\n` +
    `  renameSync(${JSON.stringify(written)}, ${JSON.stringify(pidFile)});\n` +
    "  for (;;) {}\n" +
    "});\n"
  );
}

/** The second file of a lane whose row kills a worker or Bun's test process. */
const besideIt = 'import { test } from "bun:test";\n\ntest("beside it", () => {});\n';

test(
  "a worker killed in a row names its file and row, and the progress file stays",
  async () => {
    // The lie: a worker killed from outside Bun, as the out-of-memory killer kills one, leaves
    // only Bun's "(worker crashed)" testcase: the row it was in is named nowhere, and the tool
    // deletes the progress file that records it. Here the row writes its worker's pid and spins;
    // the driver sends that worker a SIGKILL, and the tool no signal at all.
    const directory = scratch();
    const pid = join(directory, "worker-pid");
    const { code, stdout, report, leftBehind } = await stoppedLeg(
      directory,
      { "killed.test.ts": killedRow(pid, "process.pid"), "beside.test.ts": besideIt },
      [pid],
      { graceMs: 120_000, namesMs: 120_000 },
      { killFrom: pid }
    );
    expect(code).toBe(1);
    expect(report.stop).toBeNull();
    expect(report.died).toEqual([
      expect.objectContaining({
        file: "e2e/__tests__/killed.test.ts",
        run: 1,
        place: "row",
        rowsBegun: 1,
        rowName: "is killed",
      }),
    ]);
    expect(kinds(report)).toContain("worker-died");
    // Bun's own testcase of the file is folded into the place the worker died in.
    expect(report.failingRows).toEqual([]);
    expect(stdout).toMatch(
      /^::error file=packages\/dispatch\/e2e\/__tests__\/killed\.test\.ts,title=Running when its worker died::e2e\/__tests__\/killed\.test\.ts: in row 1, "is killed", or a hook after it, of run 1 of 2, /m
    );
    expect(readFileSync(join(directory, "out", "progress.jsonl"), "utf8")).toContain(
      '"event":"start"'
    );
    expect(leftBehind).toBe(0);
  },
  rowLimit("stoppedLeg")
);

test(
  "Bun's test process killed in a row names the file and row its worker was in",
  async () => {
    // The lie: when Bun's test process itself is killed from outside, Bun writes no junit file,
    // and the report says only that: no file and no row is named, and the progress file that
    // records the row is deleted. Here the row writes its parent's pid, Bun's test process, and
    // spins; the driver sends that process a SIGKILL, and the tool no signal at all.
    const directory = scratch();
    const pid = join(directory, "test-process-pid");
    const { code, report, leftBehind } = await stoppedLeg(
      directory,
      { "killed.test.ts": killedRow(pid, "process.ppid"), "beside.test.ts": besideIt },
      [pid],
      { graceMs: 120_000, namesMs: 120_000 },
      { killFrom: pid }
    );
    expect(code).toBe(1);
    expect(report.stop).toBeNull();
    expect(report.died).toEqual([
      expect.objectContaining({
        file: "e2e/__tests__/killed.test.ts",
        run: 1,
        place: "row",
        rowsBegun: 1,
        rowName: "is killed",
      }),
    ]);
    expect(kinds(report)).toContain("worker-died");
    expect(kinds(report)).toContain("junit-unreadable");
    expect(readFileSync(join(directory, "out", "progress.jsonl"), "utf8")).toContain(
      '"event":"start"'
    );
    expect(leftBehind).toBe(0);
  },
  rowLimit("stoppedLeg")
);

/** The deadline the cancelled step runs under. It only ends a leg whose SIGINT never reached the
 *  tool: the row sends that SIGINT once its row has begun, which took under 1 s on the dev box at
 *  a load of about 1,500 (2026-11-21), so 40 s is far past it, and the deadline plus the stop's
 *  own work stays inside the 90 s `stoppedLeg` bound that the step's driver runs under. */
const CANCELLED_DEADLINE_SECONDS = 40;

test(
  "a step the run cancels names its running row, as GitHub's runner signals it",
  async () => {
    // The lie: a leg that GitHub cancels, or ends at its step budget, is ended from outside and
    // names nothing. GitHub's runner (actions/runner, ProcessInvoker.cs, SendSignal) sends SIGINT
    // to the step's top process alone, then SIGTERM 7.5 s later, then kills it 2.5 s after that.
    // That top process is the bash that runs the step's script file, which passes no signal on
    // to a child it waits for, so the tool heard nothing until its deadline. Here the workflow's
    // own step text runs from a script file under `bash -e`, as the runner runs it, over a copy
    // of the package whose one row spins, and the driver sends SIGINT to that bash alone once the
    // row has begun. The report must name the SIGINT and the row, well before the deadline.
    const directory = scratch();
    const mark = join(directory, "row-began");
    const rows = join(directory, "e2e", "__tests__");
    const tools = join(directory, "e2e", "tools");
    mkdirSync(tools, { recursive: true });
    mkdirSync(rows, { recursive: true });
    for (const name of ["unit-repeat.ts", "unit-repeat-progress.ts"]) {
      copyFileSync(fileURLToPath(new URL(`../tools/${name}`, import.meta.url)), join(tools, name));
    }
    const { scripts } = JSON.parse(
      readFileSync(fileURLToPath(new URL("../../package.json", import.meta.url)), "utf8")
    ) as { scripts: Record<string, string> };
    await Bun.write(
      join(directory, "package.json"),
      `${JSON.stringify({ name: "cancelled-step", private: true, scripts: { "test:repeat": scripts["test:repeat"], "test:harness": scripts["test:harness"], "build:contracts": "true" } })}\n`
    );
    await Bun.write(join(rows, "spins.test.ts"), `${spinningHead}${spinningRow("spins", mark)}`);
    const workflow = Bun.YAML.parse(
      readFileSync(
        fileURLToPath(
          new URL("../../../../.github/workflows/dispatch-unit-repeat.yaml", import.meta.url)
        ),
        "utf8"
      )
    ) as { jobs: { repeat: { steps: { name?: string; run?: string }[] } } };
    const step = workflow.jobs.repeat.steps.find((each) => each.name?.startsWith("Run the lane"));
    const text = (step?.run ?? "")
      .replaceAll(`\${{ matrix.lane }}`, "harness")
      .replaceAll(`\${{ matrix.reruns }}`, "2")
      .replaceAll(`\${{ matrix.deadline_seconds }}`, String(CANCELLED_DEADLINE_SECONDS));
    expect(text).toContain("bun run test:repeat");
    expect(text).not.toContain("${{");
    await Bun.write(join(directory, "step.sh"), `${text}\n`);
    const runner = join(directory, "runner");
    mkdirSync(runner);
    // `set -m`, so that the step's bash runs as the runner starts it, with SIGINT not ignored as
    // a shell ignores it for a job it puts in the background with no job control.
    const driver =
      'set -m; bash -e step.sh & step=$!; until [ -e "$MARK" ] || ! kill -0 "$step" 2>/dev/null; ' +
      'do sleep 0.05; done; kill -INT "$step" 2>/dev/null; wait "$step"; code=$?; ' +
      'dir=$PWD; for i in $(seq 40); do left=$(cd / && find /proc -mindepth 2 -maxdepth 2 -name cwd -lname "$dir" 2>/dev/null | grep -cvx "/proc/$$/cwd"); ' +
      '[ "$left" = 0 ] && break; sleep 0.05; done; echo "left behind: $left"; exit "$code"';
    const { code, stdout } = await runBounded({
      argv: ["bash", "-c", driver],
      bound: "stoppedLeg",
      cwd: directory,
      env: {
        HOME: directory,
        PATH: process.env.PATH ?? "",
        TMPDIR: tmpdir(),
        GITHUB_ACTIONS: "true",
        CI: "true",
        RUNNER_TEMP: runner,
        MARK: mark,
      },
      from: import.meta.url,
    });
    const report = JSON.parse(
      readFileSync(join(runner, "unit-repeat", "report.json"), "utf8")
    ) as Report;
    expect(report.stop?.signal).toBe("SIGINT");
    expect(report.stop?.running).toEqual([
      expect.objectContaining({
        file: "e2e/__tests__/spins.test.ts",
        place: "row",
        rowName: "spins",
      }),
    ]);
    expect(stdout).toMatch(
      /^::error file=packages\/dispatch\/e2e\/__tests__\/spins\.test\.ts,title=Running when the leg was stopped::e2e\/__tests__\/spins\.test\.ts: in row 1, "spins", or a hook after it, of run 1 of 2, [0-9.]+ s since it began, when SIGINT stopped the leg/m
    );
    // The step ends on the tool's own exit, not on the deadline's 124.
    expect(code).toBe(1);
    expect(/^left behind: 0$/m.test(stdout)).toBe(true);
  },
  rowLimit("stoppedLeg")
);
