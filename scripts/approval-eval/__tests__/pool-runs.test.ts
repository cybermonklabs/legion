import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { counted } from "../corpus-io";
import type { AnswerRow } from "../fresh-agent";
import {
  type ArmOutcome,
  type BootstrapReading,
  bootstrapInterval,
  type CheckoutBuild,
  PoolError,
  type Pooling,
  pooledOf,
  type RunOutcomes,
  readRun,
  recordJson,
  recordOf,
  reportLines,
  spreadOf,
  type TReading,
  tInterval,
} from "../pool-runs";

/**
 * The pooled reading over hand-written runs: nothing here reads a corpus, a round or an agent.
 * What it holds the reading to is what the pooling is for - a case is the mean over the runs of
 * each arm's outcome, the estimate is the mean of the case-level differences, the bootstrap
 * draws the same interval under the same seed, runs covering different cases are refused by
 * name, a t interval too few differences cannot read is said to be absent rather than printed
 * as a pair of zeros, and the spread between the runs is printed beside an interval that does
 * not carry it, in the whole reading and in the section of each build of several runs.
 */

/** The directories this file made, each removed by its own exact path. */
const made: string[] = [];

afterEach(async () => {
  for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
});

/** One scratch directory under the scratch volume, never under the workspace. */
async function scratch(): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "pool-runs-"));
  made.push(directory);
  return directory;
}

/** One run as the pooling reads it, from the outcome of each arm on each case. */
function run(
  directory: string,
  cases: Record<string, [boolean, boolean]>,
  skillSha: string | null = null
): RunOutcomes {
  const held = new Map<string, ArmOutcome>();
  for (const [caseId, [skill, control]] of Object.entries(cases))
    held.set(caseId, {
      skill,
      control,
    });
  return { directory, cases: held, finishedAt: null, skillSha };
}

/** One answer row as `fresh-agent.ts` writes it. */
function row(arm: AnswerRow["arm"], caseId: string, agreed: boolean): string {
  const written: AnswerRow = {
    arm,
    caseId,
    model: "claude-opus-5-5",
    chosen: [agreed ? "Approve" : "Request changes"],
    agreed,
    fault: null,
    faultDetail: null,
    latencyMs: 11,
    at: "2026-11-12T09:00:00.000Z",
  };
  return JSON.stringify(written);
}

/** One run directory of answers, as `fresh-agent.ts` left it. */
async function runDirectory(
  cases: Record<string, [boolean, boolean]>,
  options: { skill?: string; finishedAt?: string } = {}
): Promise<string> {
  const directory = await scratch();
  const lines: string[] = [];
  for (const [caseId, [skill, control]] of Object.entries(cases)) {
    lines.push(row("no-skill", caseId, control), row("with-skill", caseId, skill));
  }
  await writeFile(join(directory, "answers-without.jsonl"), `${lines.join("\n")}\n`, "utf8");
  if (options.finishedAt !== undefined) {
    await writeFile(
      join(directory, "fresh-agent.json"),
      JSON.stringify({ without: { finishedAt: options.finishedAt } }),
      "utf8"
    );
  }
  if (options.skill !== undefined) {
    await mkdir(join(directory, "skills", "approval-review"), { recursive: true });
    await writeFile(
      join(directory, "skills", "approval-review", "SKILL.md"),
      options.skill,
      "utf8"
    );
  }
  return directory;
}

/** A checkout holding neither the command nor the skill, which most of these records read. */
const nothing: CheckoutBuild = { command: null, skill: null };

/** The resamples and the seed a record is drawn under where a test asks for no others. */
const options = { resamples: 128, seed: 7 };

/** One row of the spread table, which is what every reading that has a spread prints. */
const spreadField = /^(run differences|lowest|highest|range|standard deviation)\b/;

describe("the case-level mean and the pooled difference", () => {
  // Three runs over four cases. The skill arm agreed on case-a in every run and the control in
  // one, so that case carries 1 − 1/3; case-b is the other way about; case-c both arms always
  // agreed on, and case-d neither ever did.
  const runs = [
    run("/run-1", {
      "case-a": [true, true],
      "case-b": [true, true],
      "case-c": [true, true],
      "case-d": [false, false],
    }),
    run("/run-2", {
      "case-a": [true, false],
      "case-b": [false, true],
      "case-c": [true, true],
      "case-d": [false, false],
    }),
    run("/run-3", {
      "case-a": [true, false],
      "case-b": [false, true],
      "case-c": [true, true],
      "case-d": [false, false],
    }),
  ];

  test("each case carries the mean over the runs of each arm, and their difference", () => {
    const pooling = pooledOf(runs, { resamples: 100, seed: 1 });
    const byId = new Map(pooling.cases.map((one) => [one.caseId, one]));
    expect(byId.get("case-a")?.skill).toBeCloseTo(1, 12);
    expect(byId.get("case-a")?.control).toBeCloseTo(1 / 3, 12);
    expect(byId.get("case-a")?.difference).toBeCloseTo(2 / 3, 12);
    expect(byId.get("case-b")?.difference).toBeCloseTo(-2 / 3, 12);
    expect(byId.get("case-c")?.difference).toBeCloseTo(0, 12);
    expect(byId.get("case-d")?.difference).toBeCloseTo(0, 12);
    expect(pooling.cases.every((one) => one.runs === 3)).toBe(true);
    expect(pooling.runsPerCase).toBe(3);
  });

  test("the pooled estimate is the mean of the case-level differences", () => {
    const pooling = pooledOf(runs, { resamples: 100, seed: 1 });
    expect(pooling.difference).toBeCloseTo(0, 12);
    expect(pooling.cases).toHaveLength(4);
  });

  test("counts the cases the two arms differed on in at least one run", () => {
    const pooling = pooledOf(runs, { resamples: 100, seed: 1 });
    expect(pooling.differedInSomeRun).toBe(2);
  });

  test("a case-level difference is not the difference of the two arms' own agreements alone", () => {
    // The skill arm agreed 7 of 12 and the control 7 of 12, so the arms' own agreements tie,
    // which a pooled reading says only because it read each case and found the gains cancel.
    const pooling = pooledOf(runs, { resamples: 100, seed: 1 });
    expect(pooling.runs.map((one) => one.skill)).toEqual([0.75, 0.5, 0.5]);
    expect(pooling.runs.map((one) => one.control)).toEqual([0.75, 0.5, 0.5]);
    expect(pooling.difference).toBeCloseTo(0, 12);
  });
});

describe("the bootstrap interval", () => {
  const gaining = [
    run("/run-1", { a: [true, false], b: [true, false], c: [true, true], d: [false, false] }),
    run("/run-2", { a: [true, false], b: [false, false], c: [true, true], d: [true, false] }),
  ];

  test("is the same interval under the same seed, and is drawn again from the printed one", () => {
    const one = pooledOf(gaining, { resamples: 500, seed: 7 });
    const two = pooledOf(gaining, { resamples: 500, seed: 7 });
    expect(two.bootstrap).toEqual(one.bootstrap);
    expect(one.seed).toBe(7);
    expect(one.resamples).toBe(500);
    const differences = one.cases.map((case_) => case_.difference);
    expect(bootstrapInterval(differences, 500, 7)).toEqual(one.bootstrap);
  });

  test("is a different interval under a different seed, these cases being mixed", () => {
    const one = pooledOf(gaining, { resamples: 500, seed: 7 });
    const two = pooledOf(gaining, { resamples: 500, seed: 8 });
    expect(two.bootstrap).not.toEqual(one.bootstrap);
  });

  test("holds the estimate it is the interval of", () => {
    const pooling = pooledOf(gaining, { resamples: 2_000, seed: 7 });
    expect(pooling.difference).toBeGreaterThanOrEqual(pooling.bootstrap.lower);
    expect(pooling.difference).toBeLessThanOrEqual(pooling.bootstrap.upper);
  });

  test("is a point where every case carries the one difference, however it is resampled", () => {
    const same = [run("/run-1", { a: [true, false], b: [true, false] })];
    const pooling = pooledOf(same, { resamples: 200, seed: 3 });
    expect(pooling.bootstrap.lower).toBeCloseTo(1, 12);
    expect(pooling.bootstrap.upper).toBeCloseTo(1, 12);
  });
});

describe("the t interval beside it", () => {
  test("is the mean and the standard error of the same differences, on Student t", () => {
    // Four zeroes and a one: mean 0.2, standard deviation 0.4472136, standard error 0.2, and
    // t at 90 per cent on 4 degrees of freedom is 2.1318468.
    const interval = tInterval([0, 0, 0, 0, 1]);
    const half = 2.1318468 * 0.2;
    expect((interval.lower + interval.upper) / 2).toBeCloseTo(0.2, 12);
    expect((interval.upper - interval.lower) / 2).toBeCloseTo(half, 6);
  });

  test("is printed beside the bootstrap one, and the two are read against each other", () => {
    const runs = [
      run("/run-1", { a: [true, false], b: [true, false], c: [false, false], d: [true, true] }),
      run("/run-2", { a: [true, false], b: [false, false], c: [true, false], d: [true, true] }),
    ];
    const pooling = pooledOf(runs, { resamples: 2_000, seed: 7 });
    expect(pooling.t.lower).toBeLessThan(pooling.difference);
    expect(pooling.t.upper).toBeGreaterThan(pooling.difference);
    expect(Math.abs(pooling.t.lower - pooling.bootstrap.lower)).toBeLessThan(0.4);
  });

  test("is not printed as a number over one case, where it cannot be read", () => {
    // One case the skill arm agreed on and the control did not, which readRun accepts and the
    // record prints. The bootstrap says +1.000; Student t on 0 degrees of freedom says nothing
    // at all, and the interval printed beside a difference must at least hold it.
    const pooling = pooledOf([run("/the/only", { a: [true, false] })], {
      resamples: 1,
      seed: 7,
    });
    expect(pooling.difference).toBe(1);
    expect(pooling.bootstrap.lower).toBe(1);
    expect(pooling.t.lower).toBeLessThanOrEqual(pooling.difference);
    expect(pooling.t.upper).toBeGreaterThanOrEqual(pooling.difference);
  });
});

describe("runs that do not cover one case list", () => {
  test("are refused, naming the cases one of them does not cover", () => {
    const runs = [
      run("/run-1", { a: [true, false], b: [true, false] }),
      run("/run-2", { a: [true, false] }),
    ];
    expect(() => pooledOf(runs)).toThrow(PoolError);
    expect(() => pooledOf(runs)).toThrow("the runs do not cover one case list");
    expect(() => pooledOf(runs)).toThrow("does not cover 1 of them: b");
  });

  test("are refused too where the second run covers a case the first does not", () => {
    const runs = [
      run("/run-1", { a: [true, false] }),
      run("/run-2", { a: [true, false], z: [false, false] }),
    ];
    expect(() => pooledOf(runs)).toThrow("covers 1 it does not: z");
  });

  test("no run at all is refused rather than read as a reading of nothing", () => {
    expect(() => pooledOf([])).toThrow("--run names no run");
  });
});

describe("the spread between the runs", () => {
  const runs = [
    run("/run-1", { a: [true, false], b: [true, false], c: [true, false], d: [true, false] }),
    run("/run-2", { a: [true, false], b: [false, false], c: [false, false], d: [false, false] }),
    run("/run-3", { a: [true, false], b: [true, false], c: [false, false], d: [false, false] }),
  ];

  test("is the lowest and the highest of the runs' own differences, and their deviation", () => {
    const pooling = pooledOf(runs, { resamples: 200, seed: 7 });
    expect(pooling.runs.map((one) => one.difference)).toEqual([1, 0.25, 0.5]);
    expect(pooling.spread.lowest).toBeCloseTo(0.25, 12);
    expect(pooling.spread.highest).toBeCloseTo(1, 12);
    expect(pooling.spread.range).toBeCloseTo(0.75, 12);
    // The sample standard deviation of 1, 0.25 and 0.5, which is 0.3818813.
    expect(pooling.spread.deviation ?? 0).toBeCloseTo(0.3818813, 6);
  });

  test("one run has no spread to read, and says so rather than reading zero", () => {
    const only = spreadOf([
      {
        directory: "/run-1",
        finishedAt: null,
        skillSha: null,
        cases: 4,
        skill: 1,
        control: 0,
        difference: 1,
      },
    ]);
    expect(only.deviation).toBeNull();
    expect(only.range).toBe(0);
  });

  test("is printed beside the interval, which is said not to carry it", () => {
    const record = recordOf(runs, nothing, { resamples: 200, seed: 7 });
    const printed = reportLines(record).join("\n");
    expect(printed).toContain("+1.000, +0.250, +0.500");
    expect(printed).toContain("range             0.750");
    expect(printed).toContain("standard deviation 0.382");
    expect(printed).toContain("The pooled interval does not include the spread between the runs");
    expect(printed).toContain("Single runs of this comparison ranged over 0.750");
  });
});

describe("the record a reader meets the number in", () => {
  /** Two builds of the skill, named by the bytes alone, as a run directory names its build. */
  const bytesA = "a1".repeat(32);
  const bytesB = "b2".repeat(32);

  /**
   * Three runs over two cases. The two runs of build A agreed on both cases in the skill arm
   * and on neither in the control, so build A alone reads +1.000; the one run of build B reads
   * +0.000; and over all three runs each case carries 2/3 less 0, so the whole reads +0.667.
   * So the narrowing to build A raises the estimate and the narrowing to build B lowers it.
   */
  const builtRuns = [
    run("/build-a-1", { a: [true, false], b: [true, false] }, bytesA),
    run("/build-a-2", { a: [true, false], b: [true, false] }, bytesA),
    run("/build-b-1", { a: [false, false], b: [false, false] }, bytesB),
  ];

  /**
   * The report over those three runs, read once: `recordOf` is pure and the seed is fixed, so
   * every line below reads the same report rather than drawing two bootstraps again.
   */
  const builtPrinted = reportLines(recordOf(builtRuns, nothing, options)).join("\n");

  /** Two runs of build A alone, so the one build they fall into holds every run given. */
  const ofOneBuild = [builtRuns[0] as RunOutcomes, builtRuns[1] as RunOutcomes];
  const oneBuildRecord = recordOf(ofOneBuild, nothing, options);

  test("names every run pooled by its directory, and when it ran", () => {
    const runs = [
      { ...run("/the/first", { a: [true, false] }), finishedAt: "2026-11-12T07:58:37.776Z" },
      { ...run("/the/second", { a: [false, false] }), finishedAt: null },
    ];
    const printed = reportLines(recordOf(runs, nothing, { resamples: 100, seed: 7 })).join("\n");
    expect(printed).toContain("/the/first");
    expect(printed).toContain("2026-11-12T07:58:37.776Z");
    expect(printed).toContain("/the/second");
    expect(printed).toContain("not recorded");
  });

  test("carries the resamples and the seed the interval was drawn under", () => {
    const record = recordOf([run("/run-1", { a: [true, false] })], nothing, {
      resamples: 128,
      seed: 19,
    });
    const printed = reportLines(record).join("\n");
    expect(printed).toContain("128 resamples");
    expect(printed).toContain("under seed 19");
  });

  test("says why McNemar is not the interval here, of the runs the reading has", () => {
    const one = reportLines(recordOf([run("/run-1", { a: [true, false] })], nothing, options)).join(
      "\n"
    );
    expect(one).toContain("McNemar's exact test is not printed here");
    // One run judges each case once, so this reading's own cases do fit that 2x2 table.
    expect(one).toContain("carries 0 or 1 and does fit the 2x2 table of discordant pairs");
    expect(one).not.toContain("no longer binary");
    expect(builtPrinted).toContain("McNemar's exact test is not printed here");
    expect(builtPrinted).toContain("no longer binary once 3 runs have judged it");
    expect(builtPrinted).toContain("carries 0.333");
  });

  test("runs of two builds give one reading over every run and one reading per build", () => {
    const record = recordOf(builtRuns, nothing, options);
    expect(record.builds.map((build) => build.skillSha)).toEqual([bytesA, bytesB]);
    expect(record.builds[0]?.runs.map((place) => place.place)).toEqual([1, 2]);
    expect(record.builds[0]?.pooling.difference).toBeCloseTo(1, 12);
    expect(record.builds[1]?.runs.map((place) => place.directory)).toEqual(["/build-b-1"]);
    expect(record.builds[1]?.pooling.difference).toBeCloseTo(0, 12);
    expect(record.whole.difference).toBeCloseTo(2 / 3, 12);
    // Each build is read under the same resamples and seed as the reading over every run.
    expect(record.builds.map((build) => build.pooling.resamples)).toEqual([128, 128]);
    expect(record.builds.map((build) => build.pooling.seed)).toEqual([7, 7]);
    const printed = reportLines(record).join("\n");
    expect(printed).toContain(`sha256 ${bytesA}`);
    expect(printed).toContain(`sha256 ${bytesB}`);
    expect(printed).toContain("run 1 (/build-a-1), run 2 (/build-a-2)");
    expect(printed).toContain("pooled difference +1.000 over 2 cases");
  });

  test("names each build by a hash it read off a run and by no other name", () => {
    expect(builtPrinted).toContain(`### Build ${bytesA.slice(0, 16)} — 2 runs`);
    expect(builtPrinted).toContain(`### Build ${bytesB.slice(0, 16)} — 1 run`);
    expect(builtPrinted).not.toContain("the named build");
  });

  test("a narrowing that raises the estimate says so, and by how much", () => {
    expect(builtPrinted).toContain(
      "leaves out run 3 (/build-b-1), and leaving that run out raises the estimate by 0.333, " +
        "from +0.667 over 3 runs to +1.000 over 2 runs."
    );
  });

  test("a narrowing that lowers the estimate says so too, and by how much", () => {
    expect(builtPrinted).toContain(
      "leaves out run 1 (/build-a-1), run 2 (/build-a-2), and leaving those runs out lowers " +
        "the estimate by 0.667, from +0.667 over 3 runs to +0.000 over 1 run."
    );
  });

  test("a shift that prints as 0.000 is not said to raise or lower the estimate", () => {
    // Two estimates equal in arithmetic are rarely equal bit for bit: over these runs the
    // reading over every run is −2.8e-17 and each build's is 0, which no printed number shows.
    const near = [
      run("/near-a-1", { a: [true, true], b: [true, false] }, bytesA),
      run("/near-a-2", { a: [false, true], b: [false, false] }, bytesA),
      run("/near-b-1", { a: [true, true], b: [false, false] }, bytesB),
    ];
    const record = recordOf(near, nothing, options);
    expect(record.builds[0]?.shift).not.toBe(0);
    const printed = reportLines(record).join("\n");
    expect(printed).toContain("leaving that run out leaves the estimate where it is");
    expect(printed).not.toContain("the estimate by 0.000");
  });

  test("two numbers equal at the precision printed are printed with one sign", () => {
    // The reading over every run of this fixture is −2.8e-17 and each build's is 0. Printed
    // from their own signs they would read −0.000 and +0.000: two numbers a reader compares,
    // written as though they differed, in the one sentence that says they do not.
    const near = [
      run("/sign-a-1", { a: [true, true], b: [true, false] }, bytesA),
      run("/sign-a-2", { a: [false, true], b: [false, false] }, bytesA),
      run("/sign-b-1", { a: [true, true], b: [false, false] }, bytesB),
    ];
    const printed = reportLines(recordOf(near, nothing, options)).join("\n");
    expect(printed).not.toContain("−0.000");
    expect(printed).toContain("from +0.000 over 3 runs to +0.000 over 2 runs");
  });

  test("a build of one run is that run's own reading and not a pooling of several", () => {
    expect(builtPrinted).toContain("difference        +0.000 over 2 cases");
    expect(builtPrinted).toContain("One run holds these bytes, so this is that run's own reading");
  });

  test("a build over every run given carries no narrowing sentence", () => {
    expect(oneBuildRecord.builds).toHaveLength(1);
    expect(oneBuildRecord.builds[0]?.leftOut).toEqual([]);
    expect(reportLines(oneBuildRecord).join("\n")).not.toContain("This reading is narrower");
  });

  test("a build over every run given carries the whole reading, field for field", () => {
    // The build's runs are the whole reading's runs under the same options, so that reading is
    // not drawn a second time; drawing it again over those same runs gives the same one.
    expect(oneBuildRecord.builds[0]?.pooling).toEqual(oneBuildRecord.whole);
    expect(oneBuildRecord.builds[0]?.pooling).toEqual(pooledOf(ofOneBuild, options));
    expect(oneBuildRecord.builds[0]?.shift).toBe(0);
  });

  test("a run holding no copy of the skill is named, and is of no build", () => {
    const record = recordOf(
      [...builtRuns, run("/no-copy", { a: [true, true], b: [true, true] })],
      nothing,
      options
    );
    expect(record.unrecorded).toEqual([{ place: 4, directory: "/no-copy", skillSha: null }]);
    expect(record.builds.flatMap((build) => build.runs.map((place) => place.directory))).toEqual([
      "/build-a-1",
      "/build-a-2",
      "/build-b-1",
    ]);
    // It stays in the reading over every run, which is over four runs and not three.
    expect(record.whole.runs).toHaveLength(4);
    const printed = reportLines(record).join("\n");
    expect(printed).toContain("run 4 (/no-copy)");
    expect(printed).toContain("provenance is unrecorded");
    expect(printed).toContain("no copy of the skill under this run, so it cannot be told");
  });

  test("says which build this checkout's own skill bytes are, where they are one", () => {
    const printed = reportLines(
      recordOf(builtRuns, { command: "0".repeat(64), skill: bytesA }, options)
    ).join("\n");
    expect(printed).toContain(`sha256 ${bytesA}`);
    expect(printed).toContain(`These bytes are build ${bytesA.slice(0, 16)} above`);
    expect(printed).not.toContain("no run can ever be of the bytes that quote its own result");
  });

  test("says why no run is of the bytes that quote it, where the checkout is no build", () => {
    const checkout = { command: "0".repeat(64), skill: "cd".repeat(32) };
    const record = recordOf(builtRuns, checkout, options);
    expect(record.checkoutBuild).toBeNull();
    const printed = reportLines(record).join("\n");
    expect(printed).toContain("These bytes are none of the builds above");
    expect(printed).toContain(
      "no run can ever be of the bytes that quote its own result, because writing a measured " +
        "number into a file changes the file"
    );
  });

  test("the JSON carries each build under its own hash, and no fact in prose alone", () => {
    const checkout = { command: "0".repeat(64), skill: bytesB };
    const written = recordJson(
      recordOf(
        [...builtRuns, run("/no-copy", { a: [true, true], b: [true, true] })],
        checkout,
        options
      )
    );
    expect(Object.keys(written.builds)).toEqual([bytesA, bytesB]);
    expect(written.builds[bytesA]?.pooling.difference).toBeCloseTo(1, 12);
    expect(written.builds[bytesA]?.shift).toBeCloseTo(0.5, 12);
    expect(written.builds[bytesA]?.leftOut.map((place) => place.place)).toEqual([3, 4]);
    expect(written.unrecorded.map((place) => place.directory)).toEqual(["/no-copy"]);
    expect(written.checkout).toEqual({ command: "0".repeat(64), skill: bytesB, build: bytesB });
    expect(written.whole.runs).toHaveLength(4);
    // The round trip is what another program reads, so it holds no Map and no undefined.
    expect(JSON.parse(JSON.stringify(written)).builds[bytesB].pooling.runs).toHaveLength(1);
  });

  test("reads each run's build off that run's own copy of the skill", async () => {
    const copied = await readRun(
      await runDirectory({ a: [true, false] }, { skill: "# the skill\n" }),
      "without"
    );
    const bare = await readRun(await runDirectory({ a: [true, false] }), "without");
    const bytes = copied.skillSha as string;
    const record = recordOf([copied, bare], nothing, options);
    const printed = reportLines(record).join("\n");
    expect(record.builds.map((build) => build.skillSha)).toEqual([bytes]);
    expect(printed).toContain(`### Build ${bytes.slice(0, 16)} — 1 run`);
    expect(printed).toContain("no copy of the skill under this run, so it cannot be told");
  });
});

describe("a run read back off a disk", () => {
  test("reads one outcome per arm and case, and the time the run finished", async () => {
    const directory = await runDirectory(
      { "case-a": [true, false], "case-b": [false, false] },
      { finishedAt: "2026-11-12T08:23:03.134Z" }
    );
    const read = await readRun(directory, "without");
    expect([...read.cases.keys()]).toEqual(["case-a", "case-b"]);
    expect(read.cases.get("case-a")).toEqual({ skill: true, control: false });
    expect(read.finishedAt).toBe("2026-11-12T08:23:03.134Z");
    expect(read.skillSha).toBeNull();
  });

  test("counts a faulted row as no agreement, as the run's own reading counts it", async () => {
    const directory = await scratch();
    const faulted: AnswerRow = {
      arm: "with-skill",
      caseId: "case-a",
      model: null,
      chosen: [],
      agreed: false,
      fault: "agent",
      faultDetail: "the agent exited 1",
      latencyMs: 3,
      at: "2026-11-12T09:00:00.000Z",
    };
    await writeFile(
      join(directory, "answers-without.jsonl"),
      `${[row("no-skill", "case-a", true), JSON.stringify(faulted)].join("\n")}\n`,
      "utf8"
    );
    const read = await readRun(directory, "without");
    expect(read.cases.get("case-a")).toEqual({ skill: false, control: true });
  });

  test("a case only one arm answered is refused, and never half a pairing", async () => {
    const directory = await scratch();
    await writeFile(
      join(directory, "answers-without.jsonl"),
      `${row("no-skill", "case-a", true)}\n`,
      "utf8"
    );
    expect(readRun(directory, "without")).rejects.toThrow("answers one arm and not the other");
  });

  test("a directory holding no answers of that shape is refused, naming the file", async () => {
    const directory = await scratch();
    expect(readRun(directory, "without")).rejects.toThrow("answers-without.jsonl");
  });
});

describe("the record over one run", () => {
  /**
   * One run is an input the command takes, so every sentence of the record must hold over it.
   * The sentences built from the run count once read as though it were several, and the spread
   * section once printed a range over a set of one; these hold the record to neither.
   */
  const alone = [run("/the/only", { a: [true, false], b: [false, false] }, "c3".repeat(32))];
  const printed = reportLines(
    recordOf(alone, { command: null, skill: null }, { resamples: 128, seed: 7 })
  ).join("\n");

  test("counts one run in words a sentence about one run can hold", () => {
    expect(printed).not.toContain("1 runs");
    expect(printed).not.toContain("1 observations");
    expect(printed).not.toContain("1 experiments");
  });

  test("reads no spread between runs where there is one run to read", () => {
    expect(printed).not.toContain("ranged over 0.000 of agreement");
  });
});

describe("the sentences the record builds from the run count", () => {
  const bytes = "c3".repeat(32);

  /** One run, which is an input the command takes, read as the record prints it. */
  const alone = [run("/the/only", { a: [true, false], b: [false, false] }, bytes)];
  const overOne = reportLines(recordOf(alone, nothing, options)).join("\n");

  /** Three runs of that same build, which is the record this change leaves as it was. */
  const several = [
    run("/run-1", { a: [true, false], b: [true, false] }, bytes),
    run("/run-2", { a: [true, false], b: [false, false] }, bytes),
    run("/run-3", { a: [false, false], b: [false, false] }, bytes),
  ];
  const overSeveral = reportLines(recordOf(several, nothing, options)).join("\n");

  /**
   * One case, and one resample of it. Both are inputs the command takes: `readRun` refuses only
   * a run with no case both arms answered, and `--resamples` takes any count from 1 up. So the
   * cases and the resamples reach these sentences as 1 as readily as the runs do.
   */
  const oneResample = { resamples: 1, seed: 7 };
  const oneCase = [run("/one-case", { a: [true, false] }, bytes)];
  const oneCaseOverOne = reportLines(recordOf(oneCase, nothing, oneResample)).join("\n");

  /** Three runs over that one case, which is the record the spread section counts cases in. */
  const oneCaseRuns = [
    run("/one-case-1", { a: [true, false] }, bytes),
    run("/one-case-2", { a: [false, false] }, bytes),
    run("/one-case-3", { a: [true, false] }, bytes),
  ];
  const oneCaseOverSeveral = reportLines(recordOf(oneCaseRuns, nothing, options)).join("\n");

  test("counts no one thing in the plural, wherever the count is built", () => {
    // Not the four counts the tests above name alone: a count written into a sentence beside
    // a plural noun is the defect, and the record over one run should hold none of them.
    expect(overOne.match(/\b1 [a-z]+s\b/g)).toBeNull();
  });

  test("counts one case, and the one resample drawn of it, in the singular too", () => {
    expect(oneCaseOverOne.match(/\b1 [a-z]+s\b/g)).toBeNull();
    expect(oneCaseOverOne).toContain("arms differed on  1 of 1 case in at least one run");
    expect(oneCaseOverOne).toContain("difference        +1.000 over 1 case");
    expect(oneCaseOverOne).toContain("from 1 resample of the 1 case-level difference under seed 7");
    expect(oneCaseOverOne).toContain(
      "t interval      no interval at this count (1 case-level difference, 2 needed)"
    );
    expect(oneCaseOverOne).toContain("the estimate is the mean of the 1 case-level difference.");
    expect(oneCaseOverOne).toContain("drawn over the 1 case of this one run");
  });

  test("counts one case in the singular over several runs as well", () => {
    expect(oneCaseOverSeveral.match(/\b1 [a-z]+s\b/g)).toBeNull();
    expect(oneCaseOverSeveral).toContain("the mean of the 1 case-level difference.");
    expect(oneCaseOverSeveral).toContain("resamples the 1 case and holds these 3 runs fixed");
  });

  test("says over one run that the mean over runs is that run's own outcome", () => {
    expect(overOne).toContain(
      "Each case carries this run's own outcome in the skill arm and in the control, since " +
        "the mean over one run is that run's own outcome"
    );
    expect(overOne).toContain("one observation per case and not one experiment");
  });

  test("reads no spread over one run, and prints no field of one", () => {
    expect(overOne).toContain("there is no spread between runs to read here");
    expect(overOne.split("\n").filter((line) => spreadField.test(line))).toEqual([]);
    // The interval is still said to carry none of the variation between runs.
    expect(overOne).toContain("carries none of the variation between runs of this comparison");
  });

  test("names the one run it has where that run holds no copy of the skill", () => {
    const bare = [run("/no-copy", { a: [true, false], b: [false, false] })];
    const printed = reportLines(recordOf(bare, nothing, options)).join("\n");
    expect(printed).toContain(
      "The one run holds no copy of the skill, so its build cannot be told"
    );
    expect(printed).toContain("That run is run 1 (/no-copy).");
    expect(printed.match(/\b1 [a-z]+s\b/g)).toBeNull();
  });

  test("says which bytes a reading was made against, over one run and over several", () => {
    const checkout = { command: null, skill: "ab".repeat(32) };
    expect(reportLines(recordOf(alone, checkout, options)).join("\n")).toContain(
      "a number of the bytes the run was made against"
    );
    expect(reportLines(recordOf(several, checkout, options)).join("\n")).toContain(
      "a number of the bytes the runs were made against"
    );
  });

  test("says over several runs what it said over them before", () => {
    expect(overSeveral).toContain("## The runs pooled");
    expect(overSeveral).toContain("each carried by 3 runs");
    expect(overSeveral).toContain("by the runs' own copies of the skill");
    expect(overSeveral).toContain(
      "Each case carries the mean over the 3 runs of the skill arm's outcome and the mean of " +
        "the control's; the pooled estimate is the mean of the 2 case-level differences. " +
        "3 runs of one comparison over one case list are 3 observations per case and not " +
        "3 experiments, which is why the estimator is read per case."
    );
    expect(overSeveral).toContain("The 3 runs whose provenance is recorded fall into 1 build.");
    expect(overSeveral).toContain("## The spread between the runs");
    // Five rows of the spread between every run, and five more in the section of the one
    // build those runs fall into, which carries the spread of its own runs there.
    expect(overSeveral.split("\n").filter((line) => spreadField.test(line))).toHaveLength(10);
    expect(overSeveral).toContain(
      "the bootstrap resamples the 2 cases and holds these 3 runs fixed"
    );
    expect(overSeveral).toContain(
      "Single runs of this comparison ranged over 1.000 of agreement, from +0.000 to +1.000."
    );
  });

  test("counts one degree of freedom in the singular, whatever the run count", () => {
    // Two cases leave one degree of freedom, which the t line of either record counted as
    // "1 degrees" until every count it writes went through one place.
    expect(overOne).toContain("differences, 1 degree of freedom");
    expect(overSeveral).toContain("differences, 1 degree of freedom");
  });
});

describe("a t interval under two differences, which cannot be read at all", () => {
  const bytes = "d4".repeat(32);

  /** The one-case run the record printed a pair of zeros for, and the words it prints now. */
  const oneCase = recordOf([run("/the/only", { a: [true, false] }, bytes)], nothing, {
    resamples: 1,
    seed: 7,
  });
  const says =
    "no interval at this count (1 case-level difference, 2 needed): a standard error needs " +
    "two differences, and Student t is not defined below one degree of freedom";

  test("carries the absence itself, and no bound a reader could take for a measurement", () => {
    expect(oneCase.whole.t).toEqual({
      read: false,
      lower: Number.NEGATIVE_INFINITY,
      upper: Number.POSITIVE_INFINITY,
      freedom: 0,
      noInterval: says,
    });
  });

  test("prints what it lacks where the interval would be, and the estimate above it stands", () => {
    const printed = reportLines(oneCase).join("\n");
    expect(printed).toContain("difference        +1.000 over 1 case");
    expect(printed).toContain(`  t interval      ${says}`);
    expect(printed).not.toContain("[+0.000, +0.000]");
  });

  test("tells a program reading the JSON alone that no interval was read", () => {
    // What another program reads is the round trip, and JSON holds no infinity: a bound that
    // bounds nothing is null there, and `read` is what tells it from a bound of 0.
    const written = JSON.parse(JSON.stringify(recordJson(oneCase)));
    expect(written.whole.t).toEqual({
      read: false,
      lower: null,
      upper: null,
      freedom: 0,
      noInterval: says,
    });
    expect(written.builds[bytes].pooling.t.read).toBe(false);
  });

  test("is read and printed as an interval as soon as a second difference reads one", () => {
    const two = recordOf(
      [run("/the/only", { a: [true, false], b: [false, false] }, bytes)],
      nothing
    );
    const t = two.whole.t;
    expect(t.read).toBe(true);
    expect(t.freedom).toBe(1);
    expect(t.lower).toBeLessThanOrEqual(two.whole.difference);
    expect(t.upper).toBeGreaterThanOrEqual(two.whole.difference);
    const printed = reportLines(two).join("\n");
    expect(printed).toContain("on those same 2 differences, 1 degree of freedom");
    expect(printed).not.toContain("no interval at this count");
  });
});

describe("a bootstrap whose one resample draws no interval of the estimate", () => {
  const bytes = "e5".repeat(32);

  /**
   * Two cases, one the skill arm alone agreed on and one neither did: the differences are
   * +1.000 and +0.000 and the estimate is +0.500. The seed is chosen, not taken as read. One
   * resample draws two of these two cases with replacement, so it draws the estimate itself
   * whenever it draws one of each, and seeds 1 to 4 all do: their one mean is +0.500, which
   * the reading holds and reads as an interval of no width. Seed 7, the seed the command
   * draws under where none is named, draws the first case twice, so its one mean is +1.000
   * and both tails of it fall above the estimate they would be printed beside.
   */
  const oneResample = recordOf(
    [run("/the/only", { a: [true, false], b: [false, false] }, bytes)],
    nothing,
    { resamples: 1, seed: 7 }
  );
  const says =
    "no interval at this resample count (1 resample of the 2 case-level differences under " +
    "seed 7): the tails of the means drawn both fall above the estimate of +0.500, so they " +
    "bound those means and not the estimate they would be printed beside, which more " +
    "resamples would reach";

  test("carries the absence itself, and no bound a reader could take for a measurement", () => {
    expect(oneResample.whole.bootstrap).toEqual({
      read: false,
      lower: Number.NEGATIVE_INFINITY,
      upper: Number.POSITIVE_INFINITY,
      noInterval: says,
    });
  });

  test("prints what it lacks where the interval would be, and the estimate above it stands", () => {
    const printed = reportLines(oneResample).join("\n");
    expect(printed).toContain("difference        +0.500 over 2 cases");
    expect(printed).toContain(`  bootstrap       ${says}`);
    expect(printed).not.toContain("[+1.000, +1.000]");
  });

  test("tells a program reading the JSON alone that no interval was read", () => {
    // What another program reads is the round trip, and JSON holds no infinity: a bound that
    // bounds nothing is null there, and `read` is what tells it from a bound of 0.
    const written = JSON.parse(JSON.stringify(recordJson(oneResample)));
    expect(written.whole.bootstrap).toEqual({
      read: false,
      lower: null,
      upper: null,
      noInterval: says,
    });
    expect(written.builds[bytes].pooling.bootstrap.read).toBe(false);
  });

  test("is read and printed as an interval as soon as more resamples reach the estimate", () => {
    const more = recordOf(
      [run("/the/only", { a: [true, false], b: [false, false] }, bytes)],
      nothing,
      options
    );
    const bootstrap = more.whole.bootstrap;
    expect(bootstrap.read).toBe(true);
    expect(bootstrap.lower).toBeLessThanOrEqual(more.whole.difference);
    expect(bootstrap.upper).toBeGreaterThanOrEqual(more.whole.difference);
    const printed = reportLines(more).join("\n");
    expect(printed).toContain("from 128 resamples of the 2 case-level differences under seed 7");
    expect(printed).not.toContain("no interval at this resample count");
  });
});

describe("the spread a build's own section carries", () => {
  const bytesA = "d4".repeat(32);
  const bytesB = "e5".repeat(32);

  /**
   * Three runs over four cases: two of build A, whose own differences are +1.000 and +0.500,
   * and one of build B, whose difference is +0.000. So build A's runs spread over 0.500 and
   * every run over 1.000, and the two numbers are told apart by which section carries them.
   */
  const runs = [
    run("/a-1", { a: [true, false], b: [true, false], c: [true, false], d: [true, false] }, bytesA),
    run(
      "/a-2",
      { a: [true, false], b: [true, false], c: [false, false], d: [false, false] },
      bytesA
    ),
    run(
      "/b-1",
      { a: [false, false], b: [false, false], c: [false, false], d: [false, false] },
      bytesB
    ),
  ];
  const record = recordOf(runs, nothing, options);
  const printed = reportLines(record).join("\n");

  /** One section of the record, from its own heading to the next heading of any depth. */
  function sectionOf(heading: string): string {
    const lines = printed.split("\n");
    const at = lines.findIndex((line) => line.startsWith(heading));
    expect(at).toBeGreaterThanOrEqual(0);
    const rest = lines.slice(at + 1);
    const next = rest.findIndex((line) => line.startsWith("##"));
    return [lines[at], ...(next === -1 ? rest : rest.slice(0, next))].join("\n");
  }

  test("a build of several runs prints the spread of exactly its own runs", () => {
    const section = sectionOf(`### Build ${bytesA.slice(0, 16)}`);
    expect(section).toContain("run differences   +1.000, +0.500");
    expect(section).toContain("lowest            +0.500");
    expect(section).toContain("highest           +1.000");
    expect(section).toContain("range             0.500");
    // The sample standard deviation of 1 and 0.5, which is 0.3535534.
    expect(section).toContain("standard deviation 0.354");
    expect(section).toContain(
      "Single runs of this build ranged over 0.500 of agreement, from +0.500 to +1.000."
    );
  });

  test("that spread is the spread of its runs and not the spread over every run", () => {
    expect(record.builds[0]?.pooling.spread.lowest).toBeCloseTo(0.5, 12);
    expect(record.builds[0]?.pooling.spread.highest).toBeCloseTo(1, 12);
    expect(record.builds[0]?.pooling.spread.range).toBeCloseTo(0.5, 12);
    expect(record.builds[0]?.pooling.spread.deviation ?? 0).toBeCloseTo(0.3535534, 6);
    // Every run spreads over 1.000, which is the number a reader of the build's section would
    // have read had it carried the spread of runs it is not a reading of.
    expect(record.whole.spread.range).toBeCloseTo(1, 12);
    expect(spreadOf(record.whole.runs).range).toBeCloseTo(1, 12);
    expect(printed).toContain("Single runs of this comparison ranged over 1.000 of agreement");
    expect(sectionOf(`### Build ${bytesA.slice(0, 16)}`)).not.toContain("1.000 of agreement");
  });

  test("a build of one run prints no spread, and says why it has none", () => {
    const section = sectionOf(`### Build ${bytesB.slice(0, 16)}`);
    expect(section).toContain("One run holds these bytes, so this is that run's own reading");
    expect(section).toContain("it has no spread between runs to read");
    expect(section.split("\n").filter((line) => spreadField.test(line))).toEqual([]);
    expect(section).not.toContain("ranged over 0.000 of agreement");
  });
});

/**
 * What must hold of every record, over the cross product of the degenerate inputs the command
 * takes. One defect class has been met on four axes here — the runs, the cases, the resamples
 * and the builds the runs fall into — and each of the last two arrived inside the fix for an
 * earlier one, because each fix was driven at the axis that had just failed. So this drives the
 * axes together and holds the whole record, rather than one more sentence, to what cannot be
 * false of any of them.
 *
 * The bound: each axis carries its degenerate value and the ordinary values the record's own
 * sentences turn on. The runs carry three, since one run is the degenerate reading, two is the
 * least pooling there is, and three is the least count that puts a build of two runs beside a
 * build of one. The cases carry three, since one case-level difference reads no t interval,
 * two is the least that reads one, and three is the least that reads it on more than one
 * degree of freedom, which is where that count is printed in the plural. The shapes carry
 * four, since one run of no build and two of them are the singular and the plural of the
 * paragraph that names them. 3 x 3 x 2 x 4 is 72, less the 12 points where one run can hold
 * neither two builds nor two runs of no build, which is 60 records. A further value of any
 * axis reaches no sentence these do not: every branch of the record turns on none, one or
 * several, and 3 and 20 are already several.
 */
describe("what must hold of every record, over the inputs the command takes", () => {
  /** Two builds of the skill, named by the bytes alone, as a run directory names its build. */
  const bytesA = "f6".repeat(32);
  const bytesB = "0b".repeat(32);

  /**
   * What each arm answered on each case in each run, read down to the cases and runs a point of
   * the product asks for. The outcomes are mixed on purpose: the runs part company over case-0
   * and over case-1 the other way about, and over case-2 one run alone gains, so the
   * case-level differences are neither all equal nor all zero, which is what gives an interval
   * a span to have and the runs a range to spread over.
   */
  const outcomes: [boolean, boolean][][] = [
    // case-0, in run 1, run 2 and run 3.
    [
      [true, false],
      [true, false],
      [false, false],
    ],
    // case-1, in those same three runs.
    [
      [true, true],
      [false, true],
      [true, false],
    ],
    // case-2, in those same three runs.
    [
      [false, false],
      [true, false],
      [true, true],
    ],
  ];

  /** The fourth axis: which build each run of a point holds its own copy of the skill of. */
  const shapes: Record<string, (runs: number) => (string | null)[]> = {
    "all one build": (runs) => Array.from({ length: runs }, () => bytesA),
    "two builds": (runs) => Array.from({ length: runs }, (_, at) => (at === 0 ? bytesA : bytesB)),
    "a run of no build": (runs) =>
      Array.from({ length: runs }, (_, at) => (at === 0 ? null : bytesA)),
    "two runs of no build": (runs) =>
      Array.from({ length: runs }, (_, at) => (at < 2 ? null : bytesA)),
  };

  /**
   * Three checkouts, taken in turn across the product rather than as a fifth axis of it: the
   * sentences they choose between turn on the run count and on nothing else, so each shape of
   * checkout meets each run count several times over the 60 points.
   */
  const checkouts: [string, CheckoutBuild][] = [
    ["this checkout holds no skill", nothing],
    ["this checkout holds a build's bytes", { command: null, skill: bytesA }],
    ["this checkout holds bytes of no build", { command: null, skill: "9c".repeat(32) }],
  ];

  /** One point of the product: the runs it pools, how they are read, and against what. */
  interface Point {
    name: string;
    /** One sha per run, in the order the runs are given, and null where a run holds no copy. */
    shas: (string | null)[];
    cases: number;
    resamples: number;
    checkout: CheckoutBuild;
  }

  const points: Point[] = [];
  let taken = 0;
  for (const runs of [1, 2, 3]) {
    for (const cases of [1, 2, 3]) {
      for (const resamples of [1, 20]) {
        for (const [shape, shasOf] of Object.entries(shapes)) {
          // One run holds one copy of the skill, so neither two builds nor two runs of no
          // build is a reading one run can have.
          if (runs < 2 && shape !== "all one build" && shape !== "a run of no build") continue;
          const [said, checkout] = checkouts[taken % checkouts.length];
          taken += 1;
          points.push({
            name:
              `${counted(runs, "run")}, ${counted(cases, "case")}, ` +
              `${counted(resamples, "resample")}, ${shape}, ${said}`,
            shas: shasOf(runs),
            cases,
            resamples,
            checkout,
          });
        }
      }
    }
  }

  /** The runs of one point, as `readRun` would have read them back off a disk. */
  function runsOf(point: Point): RunOutcomes[] {
    return point.shas.map((skillSha, at) => {
      const answers: Record<string, [boolean, boolean]> = {};
      for (let one = 0; one < point.cases; one += 1) answers[`case-${one}`] = outcomes[one][at];
      return run(`/run-${at + 1}`, answers, skillSha);
    });
  }

  /** One number as the record prints it, read back: minus is printed U+2212. */
  function printedNumber(text: string): number {
    return Number(text.replace("−", "-"));
  }

  /** Either interval as the JSON carries it, where a bound that bounds nothing is null. */
  interface Written {
    read: boolean;
    lower: number | null;
    upper: number | null;
  }

  test("drives one record per point of the product, which is 60 of them", () => {
    expect(points).toHaveLength(60);
  });

  for (const point of points) {
    test(`holds over ${point.name}`, () => {
      const record = recordOf(runsOf(point), point.checkout, {
        resamples: point.resamples,
        seed: 7,
      });
      const lines = reportLines(record);
      const printed = lines.join("\n");
      /** Every reading the record carries: the one over every run, and each build's own. */
      const readings: [string, Pooling][] = [
        ["the reading over every run", record.whole],
        ...record.builds.map((build): [string, Pooling] => [
          `the reading of build ${build.skillSha.slice(0, 16)}`,
          build.pooling,
        ]),
      ];
      // Every invariant broken is collected, so a red run says each thing this input broke and
      // not only the first, and the test's own name says which input broke them.
      const faults: string[] = [];

      // Every interval printed beside an estimate holds that estimate. An interval that could
      // not be read says what it lacks in place of its bounds and prints no bounds at all.
      let estimate: number | null = null;
      let estimates = 0;
      for (const line of lines) {
        const head = /^(?:pooled )?difference\s+([+−][\d.]+) over /.exec(line);
        if (head !== null) {
          estimate = printedNumber(head[1]);
          estimates += 1;
          continue;
        }
        const beside = /^ {2}(bootstrap|t interval)\s+(.+)$/.exec(line);
        if (beside === null) continue;
        const [, which, says] = beside;
        if (estimate === null) {
          faults.push(`${which} is printed under no estimate: ${says}`);
          continue;
        }
        const bounds = /^\[([+−][\d.]+), ([+−][\d.]+)\]/.exec(says);
        if (bounds === null) {
          if (!says.startsWith("no interval at this")) {
            faults.push(`${which} prints neither an interval nor what it lacks: ${says}`);
          }
          if (says.includes("[")) {
            faults.push(`${which} prints bounds where it has none: ${says}`);
          }
          continue;
        }
        const lower = printedNumber(bounds[1]);
        const upper = printedNumber(bounds[2]);
        if (lower > upper)
          faults.push(`${which} prints ${bounds[0]}, whose bounds are the wrong way about`);
        if (lower > estimate || upper < estimate) {
          faults.push(`${which} prints ${bounds[0]} beside ${estimate}, which it excludes`);
        }
      }
      if (estimates !== readings.length) {
        faults.push(`${estimates} estimates are printed for ${readings.length} readings`);
      }

      // The object says the same as the lines: a reading that was read holds the estimate it
      // is of, and one that was not carries no bound a reader could take for a measurement.
      for (const [where, reading] of readings) {
        const intervals: [string, BootstrapReading | TReading][] = [
          ["the bootstrap", reading.bootstrap],
          ["the t interval", reading.t],
        ];
        for (const [what, interval] of intervals) {
          if (interval.read) {
            if (interval.lower > reading.difference || interval.upper < reading.difference) {
              faults.push(`${what} of ${where} excludes the ${reading.difference} it is of`);
            }
          } else if (
            interval.lower !== Number.NEGATIVE_INFINITY ||
            interval.upper !== Number.POSITIVE_INFINITY
          ) {
            faults.push(`${what} of ${where} was not read and carries a bound all the same`);
          }
        }
      }

      // No sentence claims a quantity the input cannot carry. A t interval is read over two
      // case-level differences or more, on one degree of freedom or more, and under that count
      // the reading carries what it lacks instead.
      for (const [where, reading] of readings) {
        const differences = reading.cases.length;
        const t = reading.t;
        if (differences < 2) {
          if (t.read) faults.push(`${where} reads a t interval over ${differences} of them`);
          else if (t.freedom >= 1 || t.noInterval === "") {
            faults.push(`${where} says it read no t interval on ${t.freedom} degrees of freedom`);
          }
        } else if (!t.read) {
          faults.push(`${where} reads no t interval over ${differences} differences`);
        } else if (t.freedom !== differences - 1 || t.freedom < 1) {
          faults.push(`${where} reads a t interval on ${t.freedom} degrees of freedom`);
        }
      }

      // A spread is read over two runs or more, and a reading of one run prints no field of
      // one: a lowest, a highest and a range over a set of one read as a spread measured.
      const spreads = readings.filter(([, reading]) => reading.runs.length > 1).length;
      const fields: [RegExp, string][] = [
        [/^run differences {2,}/gm, "a line of run differences"],
        [/^lowest {2,}/gm, "a lowest"],
        [/^highest {2,}/gm, "a highest"],
        [/^range {2,}/gm, "a range"],
        [/^standard deviation /gm, "a standard deviation"],
        [/ ranged over /g, "a sentence saying how far single runs ranged"],
      ];
      for (const [pattern, what] of fields) {
        const found = (printed.match(pattern) ?? []).length;
        if (found !== spreads) {
          faults.push(
            `${what} is printed ${found} times for ${spreads} readings of two runs or more`
          );
        }
      }
      for (const [where, reading] of readings) {
        const several = reading.runs.length > 1;
        if (several !== (reading.spread.deviation !== null)) {
          faults.push(
            `${where} is over ${counted(reading.runs.length, "run")} and carries ` +
              `${reading.spread.deviation === null ? "no" : "a"} standard deviation`
          );
        }
      }

      // No count is printed in the plural where it is one. The plural noun is not always the
      // word after the count — `1 case-level differences` is the one a pattern that reads only
      // that word walks past — so a hyphenated modifier may stand between them. Only a
      // hyphenated one: any word at all there reads `carries 0 or 1 and does fit` as a count
      // of does.
      const plural = printed.match(/(?<![\d.])\b1 (?:[a-z]+-[a-z-]+ )?[a-z]+s\b/g);
      if (plural !== null) faults.push(`counts one thing in the plural: ${plural.join(", ")}`);

      // Nor is a count written beside the noun of another count: a degrees-of-freedom count is
      // one or more wherever it is printed, and a sentence counting observations against
      // experiments counts two of each or more, one of each against itself saying nothing.
      for (const [said, digits, noun] of printed.matchAll(/\b(\d+) (degrees?) of freedom/g)) {
        if (Number(digits) < 1) faults.push(`prints "${said}", which is no t interval at all`);
        if ((Number(digits) === 1) !== (noun === "degree")) faults.push(`prints "${said}"`);
      }
      const estimator = /\b(\d+) observations? per case and not \d+ experiments?/g;
      for (const [said, digits] of printed.matchAll(estimator)) {
        if (Number(digits) < 2) faults.push(`prints "${said}", which says nothing`);
      }

      // Every count the head prints is a count of the record it heads.
      const head: [string, RegExpExecArray | null, number[]][] = [
        ["the runs pooled", /^runs pooled\s+(\d+)$/m.exec(printed), [record.whole.runs.length]],
        [
          "the cases and the runs each carries",
          /^cases\s+(\d+), each carried by (\d+) runs?$/m.exec(printed),
          [record.whole.cases.length, record.whole.runsPerCase],
        ],
        [
          "the cases the arms differed on",
          /^arms differed on\s+(\d+) of (\d+) cases? in at least one run$/m.exec(printed),
          [record.whole.differedInSomeRun, record.whole.cases.length],
        ],
        ["the builds", /^builds\s+(\d+) by /m.exec(printed), [record.builds.length]],
      ];
      for (const [what, found, wanted] of head) {
        if (found === null) {
          faults.push(`the head prints no count of ${what}`);
          continue;
        }
        const read = wanted.map((_, at) => Number(found[at + 1]));
        if (read.join(", ") !== wanted.join(", ")) {
          faults.push(`the head counts ${what} as ${read}, where the record holds ${wanted}`);
        }
      }
      const headings = (printed.match(/^### Build /gm) ?? []).length;
      if (headings !== record.builds.length) {
        faults.push(`${headings} builds are read on their own, of ${record.builds.length}`);
      }
      const narrowings = (printed.match(/^This reading is narrower/gm) ?? []).length;
      const narrower = record.builds.filter((build) => build.leftOut.length > 0).length;
      if (narrowings !== narrower) {
        faults.push(`${narrowings} readings say they are narrower, of ${narrower} that are`);
      }

      // A program reading the JSON alone meets no bound of an interval that was not read: JSON
      // holds no infinity, so a bound that bounds nothing is null there and `read` is what
      // tells an absent interval from a measured one.
      const written = JSON.parse(JSON.stringify(recordJson(record))) as {
        whole: { bootstrap: Written; t: Written };
        builds: Record<string, { pooling: { bootstrap: Written; t: Written } }>;
      };
      const faces: [string, { bootstrap: Written; t: Written }][] = [
        ["the whole reading", written.whole],
        ...Object.entries(written.builds).map(
          ([sha, build]): [string, { bootstrap: Written; t: Written }] => [
            `build ${sha.slice(0, 16)}`,
            build.pooling,
          ]
        ),
      ];
      for (const [where, face] of faces) {
        const intervals: [string, Written][] = [
          ["the bootstrap", face.bootstrap],
          ["the t interval", face.t],
        ];
        for (const [what, interval] of intervals) {
          if (interval.read !== (interval.lower !== null || interval.upper !== null)) {
            faults.push(
              `${what} of ${where} reads ${interval.read} in the JSON and carries ` +
                `${interval.lower}, ${interval.upper}`
            );
          }
        }
      }

      expect(faults).toEqual([]);
    });
  }
});
