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
function run(directory: string, cases: Record<string, [boolean, boolean]>): RunOutcomes {
  const held = new Map<string, ArmOutcome>();
  for (const [caseId, [skill, control]] of Object.entries(cases))
    held.set(caseId, {
      skill,
      control,
    });
  return { directory, cases: held, finishedAt: null, skillSha: null };
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
    const pooling = pooledOf(runs, { resamples: 200, seed: 7 });
    const printed = reportLines(pooling, { command: null, skill: null }).join("\n");
    expect(printed).toContain("+1.000, +0.250, +0.500");
    expect(printed).toContain("range             0.750");
    expect(printed).toContain("standard deviation 0.382");
    expect(printed).toContain("The pooled interval does not include the spread between the runs");
    expect(printed).toContain("Single runs of this comparison ranged over 0.750");
  });
});

describe("the record a reader meets the number in", () => {
  test("names every run pooled by its directory, and when it ran", () => {
    const runs = [
      { ...run("/the/first", { a: [true, false] }), finishedAt: "2026-11-12T07:58:37.776Z" },
      { ...run("/the/second", { a: [false, false] }), finishedAt: null },
    ];
    const printed = reportLines(pooledOf(runs, { resamples: 100, seed: 7 }), {
      command: null,
      skill: null,
    }).join("\n");
    expect(printed).toContain("/the/first");
    expect(printed).toContain("2026-11-12T07:58:37.776Z");
    expect(printed).toContain("/the/second");
    expect(printed).toContain("not recorded");
  });

  test("carries the resamples, the seed and why McNemar is not the interval here", () => {
    const pooling = pooledOf([run("/run-1", { a: [true, false] })], { resamples: 128, seed: 19 });
    const printed = reportLines(pooling, { command: null, skill: null }).join("\n");
    expect(printed).toContain("128 resamples");
    expect(printed).toContain("under seed 19");
    expect(printed).toContain("McNemar's exact test is not printed here");
    expect(printed).toContain("no longer binary");
  });

  test("carries both readings where the runs are not all of one build, and which to quote", () => {
    const left = {
      ...run("/off-build", { a: [true, false], b: [true, false] }),
      skillSha: "ab".repeat(32),
    };
    const kept = [
      run("/of-the-build-1", { a: [true, false], b: [false, false] }),
      run("/of-the-build-2", { a: [true, false], b: [false, false] }),
    ];
    const options = { resamples: 256, seed: 7 };
    const printed = reportLines(
      pooledOf([left, ...kept], options),
      { command: null, skill: null },
      pooledOf(kept, options)
    ).join("\n");
    expect(printed).toContain("## The same reading over the runs of one build");
    // Over the three runs the gain is 0.667 of case a and 0.333 of case b, halved; over the
    // two kept runs it is 1 of case a and 0 of case b, halved.
    expect(printed).toContain("pooled difference +0.500 over 2 cases");
    expect(printed).toContain(
      "over            2 runs, each holding the named skill bytes: run 2 (/of-the-build-1), " +
        "run 3 (/of-the-build-2)"
    );
    expect(printed).toContain("Left out: run 1 (/off-build)");
    expect(printed).toContain("the bytes that arm read are recorded nowhere");
    expect(printed).toContain(
      "A reader quotes the 2-run number, which is the only one of the two that is a number " +
        "of a single build of the skill the repository ships"
    );
  });

  test("carries one reading where every run pooled holds the one build", () => {
    const pooling = pooledOf([run("/run-1", { a: [true, false] })], { resamples: 100, seed: 7 });
    const printed = reportLines(pooling, { command: null, skill: null }).join("\n");
    expect(printed).not.toContain("## The same reading over the runs of one build");
  });

  test("says of each run whether it was made against the named skill bytes", async () => {
    const named = await readRun(
      await runDirectory({ a: [true, false] }, { skill: "# the skill\n" }),
      "without"
    );
    const bare = await readRun(await runDirectory({ a: [true, false] }), "without");
    const printed = reportLines(pooledOf([named, bare], { resamples: 100, seed: 7 }), {
      command: "0".repeat(64),
      skill: null,
    }).join("\n");
    expect(printed).toContain("NOT the named build");
    expect(printed).toContain("no copy of the skill under this run, so it cannot be told");
    expect(printed).toContain("which is not the named build: the checkout has moved");
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
