/** e2e/tools/unit-repeat.ts on the junit files real Bun writes (INFRA-1511).
 *
 *  Each row writes tiny test files into a directory of its own and runs Bun over them there,
 *  with `process.execPath`, the arguments the tool gives Bun (`runArguments`: an explicit
 *  `--seed`, `--rerun-each` and, for a shard, `--shard`) and Bun's junit reporter. Then it hands
 *  what Bun left behind to the tool's pure functions. The directory is outside packages/dispatch,
 *  so the package's bunfig.toml does not reach that run. HOME is the same directory, so no global
 *  bunfig of the host reaches it either. Two rows run the tool itself there instead, on a command
 *  line the tool must refuse before it starts Bun: an `--out` that names a file, and a `--shard`
 *  that names no shard.
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
import { existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import {
  annotations,
  type JunitText,
  junitAt,
  type LaneRun,
  markdown,
  parseArguments,
  type Report,
  type RowlessKind,
  runArguments,
  type Shard,
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
  return runIn(directory, reruns, null, environment);
}

/** Runs `bun test` in `directory` as a lane run of `shard` would, with the arguments the tool
 *  gives Bun (`runArguments`) and Bun's junit reporter. */
async function runIn(
  directory: string,
  reruns: number,
  shard: Shard | null,
  environment: Readonly<Record<string, string>> = {}
): Promise<LaneRun> {
  const junit = join(directory, "junit.xml");
  const started = performance.now();
  const { code, output } = await spawnBun(
    directory,
    [
      "test",
      ...runArguments({ reruns, seed: SEED, shard }),
      "--reporter=junit",
      `--reporter-outfile=${junit}`,
    ],
    environment
  );
  return {
    lane: "harness",
    target: "e2e/__tests__",
    seed: SEED,
    shard,
    reruns,
    wallMs: performance.now() - started,
    exitCode: code,
    signal: null,
    junit: junitAt(junit),
    log: output,
    environment,
    // Bun runs here without runLane, which is what reads /proc/meminfo, so there is no reading.
    memory: [],
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
    expect(options("0")).toEqual({
      options: { lane: "web", out: "out", reruns: 20, seed: 0, shard: null },
    });
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

/** A test file of six rows, the first of which fails on every run: the failing row names the
 *  file in its report, and the junit file gives the order its rows ran in. */
function sixRows(file: string): string {
  const passing = [1, 2, 3, 4, 5].map((row) => `test("${file} passes ${row}", () => {});`);
  return [
    'import { test } from "bun:test";',
    "",
    `test("${file} fails", () => {`,
    `  throw new Error("${file}");`,
    "});",
    ...passing,
    "",
  ].join("\n");
}

/** The names of the testcases of `file` in `junit`, in the order Bun ran them. */
function orderIn(junit: JunitText, file: string): string[] {
  if (!("text" in junit)) throw new Error(junit.unreadable);
  return [...junit.text.matchAll(/<testcase name="([^"]*)"[^>]*\bfile="([^"]*)"/g)]
    .filter((testcase) => testcase[2] === file)
    .map((testcase) => testcase[1]);
}

test(
  "the two shards of a run read its files between them, each file once, and each report names its shard",
  async () => {
    // The lie: the tool hands Bun no shard, so each leg of a sharded round reads every file, and
    // the round runs each row twice as often as it says, at twice the cost. Or a report does not
    // say which shard it read, so the two legs of a round read as one leg twice. Or a row of a
    // shard is given a command that names the shard, which runs nothing for a file of any shard
    // but the first.
    const directory = scratch();
    const files = ["a.test.ts", "b.test.ts", "c.test.ts"];
    for (const file of files) await Bun.write(join(directory, file), sixRows(file));
    const shards: { readonly report: Report; readonly run: LaneRun }[] = [];
    for (const index of [1, 2]) {
      const run = await runIn(directory, 1, { count: 2, index });
      shards.push({ report: summarise(run), run });
    }
    const read = shards.map(({ report }) => report.failingRows.map((row) => row.file));
    for (const each of read) expect(each.length).toBeGreaterThan(0);
    expect(read.flat().sort()).toEqual(files);

    for (const [position, { report, run }] of shards.entries()) {
      const shard = `${position + 1}/2`;
      expect(report.shard).toBe(shard);
      expect(report.oneRun.files).toBe(read[position].length);
      expect(report.command).toBe(
        `bun run test:harness e2e/__tests__ --seed=${SEED} --rerun-each=1 --shard=${shard}`
      );
      const text = markdown(report);
      expect(text).toStartWith(`## Repeated unit tests: harness lane, shard ${shard}\n`);
      expect(text).toContain(`- Shard: \`${shard}\` of the lane's files`);
      expect(annotations(report).at(-1)).toStartWith(
        `::notice title=Repeated unit tests%3A harness lane%2C shard ${shard}::`
      );
      // Each row's command, run as it reads, runs its file in the order the file ran in its shard.
      for (const row of report.failingRows) {
        expect(row.reproduce).toBe(`bun run test:harness ${row.file} --seed=${SEED}`);
        const alone = join(directory, "alone.xml");
        const { code } = await spawnBun(directory, [
          "test",
          ...row.reproduce.replace("bun run test:harness ", "").split(" "),
          "--reporter=junit",
          `--reporter-outfile=${alone}`,
        ]);
        expect(code).toBe(1);
        expect(orderIn(junitAt(alone), row.file)).toEqual(orderIn(run.junit, row.file));
      }
    }
  },
  // The two shards and the three files alone, one after another, each a bounded child under the
  // short bound.
  rowLimit("short", "short", "short", "short", "short")
);

test(
  "a --shard that names no shard of the files is refused as a bad command line, before Bun starts",
  async () => {
    // The lie: the tool hands Bun a shard Bun refuses, and Bun exits 1, the status of a run whose
    // rows failed, although no test ran.
    const options = (shard: string) =>
      parseArguments(["--lane", "web", "--out", "out", "--seed", "1", "--shard", shard]);
    expect(options("2/2")).toMatchObject({ options: { shard: { count: 2, index: 2 } } });
    expect(options("1/1")).toMatchObject({ options: { shard: { count: 1, index: 1 } } });
    const refused = ["0/2", "3/2", "a/b", "1/0"];
    for (const shard of [...refused, "1", "1/2/3", ""]) {
      expect(options(shard)).toHaveProperty("problem");
    }

    const directory = scratch();
    const out = join(directory, "out");
    for (const shard of refused) {
      const { code, output } = await spawnBun(directory, [
        tool,
        "--lane",
        "web",
        "--reruns",
        "1",
        "--seed",
        "1",
        "--shard",
        shard,
        "--out",
        out,
      ]);
      expect(code).toBe(2);
      expect(output).toContain(
        `unit-repeat: --shard is <i>/<n>, two whole numbers with n at least 1 and i from 1 to n, not ${shard}`
      );
      expect(output).toContain("usage: bun run test:repeat --lane <web|web-cold|harness>");
      expect(output).not.toContain("seed=1 lane=web");
      // Refused before the tool's first write, as an --out it cannot use is.
      expect(existsSync(out)).toBe(false);
    }
  },
  // The four runs of the tool, one after another, each a bounded child under the short bound.
  rowLimit("short", "short", "short", "short")
);
