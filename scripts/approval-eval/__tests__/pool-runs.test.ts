import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { AnswerRow } from "../fresh-agent";
import {
  type ArmOutcome,
  bootstrapInterval,
  PoolError,
  pooledOf,
  type RunOutcomes,
  readRun,
  recordJson,
  recordOf,
  reportLines,
  spreadOf,
  tInterval,
} from "../pool-runs";

/**
 * The pooled reading over hand-written runs: nothing here reads a corpus, a round or an agent.
 * What it holds the reading to is what the pooling is for - a case is the mean over the runs of
 * each arm's outcome, the estimate is the mean of the case-level differences, the bootstrap
 * draws the same interval under the same seed, runs covering different cases are refused by
 * name, and the spread between the runs is printed beside an interval that does not carry it.
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
    const record = recordOf(runs, { command: null, skill: null }, { resamples: 200, seed: 7 });
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
  const options = { resamples: 128, seed: 7 };
  const nothing = { command: null, skill: null };

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
    const record = recordOf([copied, bare], { command: null, skill: null }, options);
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
   * One run is an input the command takes, and the McNemar paragraph was taught to say the true
   * thing on it. The sentences around it were not: they are built from the run count and read as
   * though it were several, and the spread section prints a range over a set of one.
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
  const nothing = { command: null, skill: null };
  const options = { resamples: 128, seed: 7 };
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

  /** One field of the spread table, which is what a reading of a spread prints. */
  const field = /^(lowest|highest|range|standard deviation)\b/;

  test("counts no one thing in the plural, wherever the count is built", () => {
    // Not the four counts the tests above name alone: a count written into a sentence beside
    // a plural noun is the defect, and the record over one run should hold none of them.
    expect(overOne.match(/\b1 [a-z]+s\b/g)).toBeNull();
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
    expect(overOne.split("\n").filter((line) => field.test(line))).toEqual([]);
    // The interval is still said to carry none of the variation between runs.
    expect(overOne).toContain("carries none of the variation between runs of this comparison");
  });

  test("names the one run it has where that run holds no copy of the skill", () => {
    const bare = [run("/no-copy", { a: [true, false], b: [false, false] })];
    const printed = reportLines(recordOf(bare, nothing, options)).join("\n");
    expect(printed).toContain(
      "The one run holds no copy of the skill, so its build cannot be told"
    );
    expect(printed).toContain("The one run holds no copy of the skill: run 1 (/no-copy).");
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
    expect(overSeveral.split("\n").filter((line) => field.test(line))).toHaveLength(4);
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
