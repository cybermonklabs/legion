import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { OutputPathError, outDirectory, randomStream, table, UsageError } from "./corpus-io";
import { type AnswerRow, type ArmName, answersPath, type Shape } from "./fresh-agent";
import { DEFAULT_CONFIDENCE, type Interval, regularizedIncompleteBeta } from "./stats";

/**
 * The pooled reading over several runs of one fresh-agent comparison.
 *
 * Several runs of one comparison over one case list are several observations per case, and not
 * several experiments: every run puts the same 250 cases to the same two arms of the same
 * model, so what a run adds is another judgement of each case and not another question. So the
 * estimator is read per case and never per run:
 *
 * - each case carries the mean over the runs of the skill arm's outcome (1 where that arm
 *   agreed with the person, 0 where it did not) and the mean of the control's, and the case's
 *   difference is the first mean less the second;
 * - the pooled estimate is the mean of those case-level differences;
 * - its interval is a paired bootstrap over the case-level differences, resampling the cases
 *   with replacement, with the resample count and the seed printed beside the number.
 *
 * The bootstrap, and not McNemar, because a case's outcome is no longer binary once several
 * runs have judged it: a case the skill arm agreed on in three runs of five carries 0.6, which
 * no 2x2 table of discordant pairs can hold, and McNemar's exact interval is built over exactly
 * that table. McNemar stays where a single run is reported, which is `fresh-agent.ts`, where
 * each case's outcome is one judgement and the table is the whole of the evidence.
 *
 * A t interval over the same case-level differences is printed beside the bootstrap one, so a
 * reader sees whether the two agree. They are two readings of one quantity and not two
 * quantities.
 *
 * **The bootstrap holds the runs fixed.** Resampling cases says what the expected difference is
 * given these runs, and carries nothing of the variation between them. So this prints each
 * run's own difference, their range and their standard deviation beside the pooled interval,
 * and says in words that the pooled interval does not include that spread.
 *
 * The record names the runs it pooled by their directories, and the build they were made
 * against, since a pooled number over runs of two builds is a number of neither. Where the runs
 * named are not all of the named build, the record carries two readings where a reader meets the
 * number: the pooled estimate over every run named, and the same estimate over the runs of that
 * one build, each with its own interval, resamples and seed, with one sentence saying which of
 * the two a reader quotes and why.
 */

/** The shape of run this pools where no `--shape` names another. */
const DEFAULT_SHAPE: Shape = "without";

/** How many resamples the bootstrap takes where no `--resamples` names a count. */
const DEFAULT_RESAMPLES = 20_000;

/** The seed the bootstrap is drawn under where no `--seed` names one. */
const DEFAULT_SEED = 7;

/** How many case ids a refusal names before it counts the rest. */
const NAMED_CASES = 10;

/** How many characters of a sha256 a line carries, which is enough to tell two builds apart. */
const DIGEST_SHOWN = 16;

/**
 * The bytes the pooled runs were made against, which the record names so that a reader knows
 * which build this is a number of. They are not read from this checkout: this checkout moves on,
 * and the question is which bytes the runs were made against, not which bytes are here now.
 */
const BUILD = {
  command: "a2bfbc4fc2897bc4278391dd348ecf5161a0bbc9572dbe8720851bbb475a37da",
  skill: "19029752f2db9f9ce326539ac9f55fffec8cf7ae5df970a3a30cd6ae31082bf2",
} as const;

/** Where a run's own copy of the skill sits, which `fresh-agent.ts` writes under `--out`. */
const SKILL_IN_RUN = join("skills", "approval-review", "SKILL.md");

/** Where this checkout's own copy of each of those two files sits. */
const COMMAND_PATH = join(import.meta.dir, "fresh-agent.ts");
const SKILL_PATH = join(import.meta.dir, "..", "..", "skills", "approval-review", "SKILL.md");

/** How far the bisection that answers a t quantile may look, which no t of this size reaches. */
const T_CEILING = 1_000;

/** Bisection steps of that quantile: 80 already reaches the spacing of a double at this size. */
const T_STEPS = 80;

/** The reading was made. */
const EXIT_OK = 0;

/** The reading could not be made at all. */
const EXIT_CANNOT_RUN = 2;

const USAGE = `usage: bun scripts/approval-eval/pool-runs.ts --run <dir> [--run <dir>]... \\
  [--shape <with|without>] [--resamples <n>] [--seed <n>] [--out <dir>]

Pools several runs of one fresh-agent comparison over one case list, reading each case as the
mean over the runs of each arm's outcome, and prints the pooled difference with a paired
bootstrap interval over the case-level differences.

Options:
  --run <dir>        Required, and repeated: each a directory fresh-agent.ts wrote, holding
                     answers-<shape>.jsonl. Two runs that do not cover one case list are
                     refused, naming the cases.
  --shape <shape>    Which shape of run is pooled: with or without. Default ${DEFAULT_SHAPE}.
  --resamples <n>    How many resamples the bootstrap takes. Default ${DEFAULT_RESAMPLES}.
  --seed <n>         The seed the bootstrap is drawn under, printed beside the interval so the
                     same interval is drawn again. Default ${DEFAULT_SEED}.
  --out <dir>        Where pool-runs.json and pool-runs.md are written. An absolute path
                     outside this repository's workspace.

Exits ${EXIT_OK} when the reading was made and ${EXIT_CANNOT_RUN} when it could not be.`;

/** The runs named cannot be pooled, and the message says which and why. */
export class PoolError extends Error {
  override readonly name = "PoolError";
}

/** Whether each arm agreed with the person on one case, in one run. */
export interface ArmOutcome {
  skill: boolean;
  control: boolean;
}

/** One run read back: what each arm answered on each case, and what the run itself was. */
export interface RunOutcomes {
  directory: string;
  /** One entry per case both arms answered, which a refusal keeps the same across the runs. */
  cases: Map<string, ArmOutcome>;
  /** When the run finished, as its `fresh-agent.json` records it, and null where it records none. */
  finishedAt: string | null;
  /** The sha256 of the skill this run's own copy holds, and null where it holds no copy. */
  skillSha: string | null;
}

/** One run as the pooled record names it: where it is, when it ran, and what it alone measured. */
export interface RunReading {
  directory: string;
  finishedAt: string | null;
  skillSha: string | null;
  cases: number;
  skill: number;
  control: number;
  difference: number;
}

/** One case of the pooled reading: the mean of each arm over the runs, and the difference. */
export interface PooledCase {
  caseId: string;
  runs: number;
  skill: number;
  control: number;
  difference: number;
  /** Whether the two arms differed on this case in at least one of the runs. */
  differed: boolean;
}

/** How far the runs' own differences spread, which the pooled interval does not carry. */
export interface Spread {
  lowest: number;
  highest: number;
  range: number;
  /** The sample standard deviation over the runs, and null where one run cannot have one. */
  deviation: number | null;
}

/** The pooled reading whole, as it is printed and written. */
export interface Pooling {
  shape: Shape;
  runs: RunReading[];
  cases: PooledCase[];
  /** How many runs each case carries, which every case carries equally or the runs are refused. */
  runsPerCase: number;
  difference: number;
  confidence: number;
  bootstrap: Interval;
  resamples: number;
  seed: number;
  t: Interval;
  spread: Spread;
  /** How many cases the two arms differed on in at least one run. */
  differedInSomeRun: number;
}

/** The flags, as the command reads them. */
interface Flags {
  runs: string[];
  shape: Shape;
  resamples: number;
  seed: number;
  out: string | null;
}

/** An agreement as the record prints it. */
function rate(value: number): string {
  return value.toFixed(3);
}

/** A difference as the record prints it, which always carries its sign. */
function signed(value: number): string {
  return `${value < 0 ? "−" : "+"}${Math.abs(value).toFixed(3)}`;
}

/** An interval as the record prints it. */
function span(interval: Interval): string {
  return `[${signed(interval.lower)}, ${signed(interval.upper)}]`;
}

/** A count, as every printed count here is written. */
function count(value: number): string {
  return value.toLocaleString("en-US");
}

/** The sha256 of one file, and null where there is no such file to read. */
async function sha256Of(path: string): Promise<string | null> {
  try {
    return createHash("sha256")
      .update(await readFile(path))
      .digest("hex");
  } catch {
    return null;
  }
}

/** The rows of one run's answers file, the last row of each arm and case winning. */
function outcomesOf(text: string): Map<string, Partial<Record<ArmName, boolean>>> {
  const held = new Map<string, Partial<Record<ArmName, boolean>>>();
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      continue;
    }
    // The file is `fresh-agent.ts`'s own, so a line that parses is a row it wrote.
    const row = parsed as AnswerRow;
    if (typeof row.arm !== "string" || typeof row.caseId !== "string") continue;
    const outcome = held.get(row.caseId) ?? {};
    held.set(row.caseId, outcome);
    // A faulted row is never an agreement, which is how a single run's own reading counts it.
    outcome[row.arm] = row.fault === null && row.agreed === true;
  }
  return held;
}

/** When one run finished, from the `fresh-agent.json` it wrote, where it holds that shape. */
async function finishedAtOf(directory: string, shape: Shape): Promise<string | null> {
  let text: string;
  try {
    text = await readFile(join(directory, "fresh-agent.json"), "utf8");
  } catch {
    return null;
  }
  let parsed: unknown;
  try {
    // The file is `fresh-agent.ts`'s own, so what it holds is what that command wrote.
    parsed = JSON.parse(text);
  } catch {
    return null;
  }
  const shapes = parsed as Record<string, { finishedAt?: unknown } | undefined>;
  const finishedAt = shapes[shape]?.finishedAt;
  return typeof finishedAt === "string" ? finishedAt : null;
}

/**
 * One run read back: one outcome per case and arm, the last row of each winning, as the run's
 * own resume reads them. A case only one arm answered is refused rather than dropped: a run
 * missing half a pair is a run whose difference is read over cases it never paired.
 */
export async function readRun(directory: string, shape: Shape): Promise<RunOutcomes> {
  const path = answersPath(directory, shape);
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch {
    throw new PoolError(
      `${path} could not be read: --run takes a directory fresh-agent.ts wrote with ` +
        `--neighbours ${shape}`
    );
  }
  const held = outcomesOf(text);
  const cases = new Map<string, ArmOutcome>();
  const half: string[] = [];
  for (const [caseId, outcome] of [...held].sort(([one], [two]) => one.localeCompare(two))) {
    const skill = outcome["with-skill"];
    const control = outcome["no-skill"];
    if (skill === undefined || control === undefined) half.push(caseId);
    else cases.set(caseId, { skill, control });
  }
  if (half.length > 0) {
    throw new PoolError(
      `${path} answers one arm and not the other on ${count(half.length)} ` +
        `${half.length === 1 ? "case" : "cases"}: ${named(half)}`
    );
  }
  if (cases.size === 0) throw new PoolError(`${path} holds no case both arms answered`);
  return {
    directory,
    cases,
    finishedAt: await finishedAtOf(directory, shape),
    skillSha: await sha256Of(join(directory, SKILL_IN_RUN)),
  };
}

/** Some case ids of a refusal, named, with the rest counted. */
function named(caseIds: readonly string[]): string {
  const head = caseIds.slice(0, NAMED_CASES).join(", ");
  return caseIds.length <= NAMED_CASES
    ? head
    : `${head}, and ${count(caseIds.length - NAMED_CASES)} more`;
}

/** One run's own reading: its agreement in each arm, and the difference it alone measured. */
function runReadingOf(run: RunOutcomes): RunReading {
  const outcomes = [...run.cases.values()];
  const share = (of: (one: ArmOutcome) => boolean): number =>
    outcomes.filter((one) => of(one)).length / outcomes.length;
  const skill = share((one) => one.skill);
  const control = share((one) => one.control);
  return {
    directory: run.directory,
    finishedAt: run.finishedAt,
    skillSha: run.skillSha,
    cases: outcomes.length,
    skill,
    control,
    difference: skill - control,
  };
}

/**
 * The one case list the runs are pooled over, or a refusal naming the cases they differ on.
 * Pooling is per case, so a case one run never judged is not a case this reading has an
 * observation of in every run, and averaging over runs of different case lists would weigh the
 * cases unequally without saying so.
 */
function caseListOf(runs: readonly RunOutcomes[]): string[] {
  const first = runs[0];
  if (first === undefined) throw new PoolError("--run names no run, so there is nothing to pool");
  const wanted = [...first.cases.keys()];
  const held = new Set(wanted);
  for (const run of runs.slice(1)) {
    const missing = wanted.filter((caseId) => !run.cases.has(caseId));
    const extra = [...run.cases.keys()].filter((caseId) => !held.has(caseId));
    if (missing.length === 0 && extra.length === 0) continue;
    const says = [
      missing.length === 0
        ? null
        : `does not cover ${count(missing.length)} of them: ${named(missing)}`,
      extra.length === 0 ? null : `covers ${count(extra.length)} it does not: ${named(extra)}`,
    ].filter((one): one is string => one !== null);
    throw new PoolError(
      `the runs do not cover one case list: ${first.directory} covers ` +
        `${count(wanted.length)} cases and ${run.directory} ${says.join(", and ")}`
    );
  }
  return wanted;
}

/** The quantile of a sorted sample, by linear interpolation between its order statistics. */
function quantileOf(sorted: Float64Array, at: number): number {
  const last = sorted.length - 1;
  if (last < 0) return 0;
  const place = Math.min(last, Math.max(0, at * last));
  const below = Math.floor(place);
  const above = Math.min(last, below + 1);
  const part = place - below;
  return (sorted[below] as number) * (1 - part) + (sorted[above] as number) * part;
}

/**
 * The paired bootstrap interval of the mean of `differences`: `resamples` resamples of the
 * cases themselves, with replacement, each giving one mean, with each tail taking half of what
 * `confidence` leaves. The same seed over the same differences draws the same interval, which
 * is what makes the seed worth printing beside it.
 */
export function bootstrapInterval(
  differences: readonly number[],
  resamples: number,
  seed: number,
  confidence: number = DEFAULT_CONFIDENCE
): Interval {
  if (differences.length === 0 || resamples === 0) return { lower: 0, upper: 0 };
  const next = randomStream(seed);
  const size = differences.length;
  const means = new Float64Array(resamples);
  for (let resample = 0; resample < resamples; resample += 1) {
    let total = 0;
    for (let drawn = 0; drawn < size; drawn += 1) {
      total += differences[Math.min(size - 1, Math.floor(next() * size))] as number;
    }
    means[resample] = total / size;
  }
  means.sort();
  const tail = (1 - confidence) / 2;
  return { lower: quantileOf(means, tail), upper: quantileOf(means, 1 - tail) };
}

/**
 * The two-sided Student t quantile at `confidence` on `freedom` degrees of freedom, by
 * bisection on the incomplete beta: the probability a t exceeds `value` in size is
 * I_x(freedom / 2, 1 / 2) at x = freedom / (freedom + value squared), which only falls as the
 * value rises.
 */
function tQuantile(confidence: number, freedom: number): number {
  const wanted = 1 - confidence;
  let low = 0;
  let high = T_CEILING;
  for (let step = 0; step < T_STEPS; step += 1) {
    const middle = (low + high) / 2;
    const size = regularizedIncompleteBeta(freedom / (freedom + middle * middle), freedom / 2, 0.5);
    if (size > wanted) low = middle;
    else high = middle;
  }
  return (low + high) / 2;
}

/**
 * The t interval of the mean of `differences`, on the standard error of those same
 * differences. It is printed beside the bootstrap interval so that a reader sees the two agree
 * or not: they are two readings of one quantity, and a reader told only one of them cannot tell
 * whether the shape of the differences made the difference.
 */
export function tInterval(
  differences: readonly number[],
  confidence: number = DEFAULT_CONFIDENCE
): Interval {
  const size = differences.length;
  if (size < 2) return { lower: 0, upper: 0 };
  const mean = differences.reduce((one, two) => one + two, 0) / size;
  const variance =
    differences.reduce((total, one) => total + (one - mean) * (one - mean), 0) / (size - 1);
  const error = Math.sqrt(variance / size);
  const half = tQuantile(confidence, size - 1) * error;
  return { lower: mean - half, upper: mean + half };
}

/** How far the runs' own differences spread, which is the variation the bootstrap holds fixed. */
export function spreadOf(runs: readonly RunReading[]): Spread {
  const differences = runs.map((run) => run.difference);
  if (differences.length === 0) return { lowest: 0, highest: 0, range: 0, deviation: null };
  const lowest = Math.min(...differences);
  const highest = Math.max(...differences);
  if (differences.length < 2) {
    return { lowest, highest, range: highest - lowest, deviation: null };
  }
  const mean = differences.reduce((one, two) => one + two, 0) / differences.length;
  const variance =
    differences.reduce((total, one) => total + (one - mean) * (one - mean), 0) /
    (differences.length - 1);
  return { lowest, highest, range: highest - lowest, deviation: Math.sqrt(variance) };
}

/** How the pooling is drawn, where the caller names anything other than the defaults. */
export interface PoolOptions {
  resamples?: number;
  seed?: number;
  confidence?: number;
  shape?: Shape;
}

/**
 * The pooled reading over the runs: one observation per case per run, the case-level
 * differences, their mean, and the two intervals of that mean, beside what each run alone
 * measured and how far those measurements spread.
 */
export function pooledOf(runs: readonly RunOutcomes[], options: PoolOptions = {}): Pooling {
  const {
    resamples = DEFAULT_RESAMPLES,
    seed = DEFAULT_SEED,
    confidence = DEFAULT_CONFIDENCE,
    shape = DEFAULT_SHAPE,
  } = options;
  const caseIds = caseListOf(runs);
  const cases: PooledCase[] = caseIds.map((caseId) => {
    const outcomes = runs.map((run) => run.cases.get(caseId) as ArmOutcome);
    const mean = (of: (one: ArmOutcome) => boolean): number =>
      outcomes.filter((one) => of(one)).length / outcomes.length;
    const skill = mean((one) => one.skill);
    const control = mean((one) => one.control);
    return {
      caseId,
      runs: outcomes.length,
      skill,
      control,
      difference: skill - control,
      differed: outcomes.some((one) => one.skill !== one.control),
    };
  });
  const differences = cases.map((one) => one.difference);
  const readings = runs.map(runReadingOf);
  return {
    shape,
    runs: readings,
    cases,
    runsPerCase: runs.length,
    difference: differences.reduce((one, two) => one + two, 0) / differences.length,
    confidence,
    bootstrap: bootstrapInterval(differences, resamples, seed, confidence),
    resamples,
    seed,
    t: tInterval(differences, confidence),
    spread: spreadOf(readings),
    differedInSomeRun: cases.filter((one) => one.differed).length,
  };
}

/** What this checkout holds now of the two files the named build is of. */
export interface CheckoutBuild {
  command: string | null;
  skill: string | null;
}

/** Whether one run was made against the named skill bytes, in the words the record prints. */
function skillSaysOf(run: RunReading): string {
  if (run.skillSha === null) return "no copy of the skill under this run, so it cannot be told";
  const digest = run.skillSha.slice(0, DIGEST_SHOWN);
  return `${digest} — ${run.skillSha === BUILD.skill ? "the named build" : "NOT the named build"}`;
}

/** The runs pooled, named by their directories, with what each one alone measured. */
function runLines(pooling: Pooling): string[] {
  return table(
    ["run", "directory", "finished", "skill arm", "control", "difference", "skill bytes"],
    pooling.runs.map((run, at) => [
      `${at + 1}`,
      run.directory,
      run.finishedAt ?? "not recorded",
      rate(run.skill),
      rate(run.control),
      signed(run.difference),
      skillSaysOf(run),
    ])
  );
}

/** What the record says about the build the pooled runs were made against. */
function buildLines(pooling: Pooling, checkout: CheckoutBuild): string[] {
  const off = pooling.runs.filter((run) => run.skillSha !== null && run.skillSha !== BUILD.skill);
  const untold = pooling.runs.filter((run) => run.skillSha === null);
  const places = (runs: readonly RunReading[]): string =>
    runs.map((run) => `run ${pooling.runs.indexOf(run) + 1} (${run.directory})`).join(", ");
  const lines = [
    "## The build these runs were made against",
    "",
    `A pooled number over runs of two builds is a number of neither, so the record names the ` +
      "bytes. The named build is:",
    "",
    `    scripts/approval-eval/fresh-agent.ts            sha256 ${BUILD.command}`,
    `    skills/approval-review/SKILL.md                 sha256 ${BUILD.skill}`,
    "",
  ];
  if (off.length === 0 && untold.length === 0) {
    lines.push(
      `Every one of the ${count(pooling.runs.length)} pooled runs holds the named skill bytes ` +
        "under its own skills/approval-review/SKILL.md, which is the copy the skill arm read, " +
        "so every pooled run was made against that skill."
    );
  } else {
    if (off.length > 0) {
      lines.push(
        `${count(off.length)} of the ${count(pooling.runs.length)} pooled runs ` +
          `${off.length === 1 ? "holds" : "hold"} skill bytes that are not the named ones: ` +
          `${places(off)}. So this pooled number is over more than one build of the skill and ` +
          "is not a number of the named build alone."
      );
    }
    if (untold.length > 0) {
      lines.push(
        `${count(untold.length)} of the pooled runs ${untold.length === 1 ? "holds" : "hold"} ` +
          `no copy of the skill, so which skill ${untold.length === 1 ? "it was" : "they were"} ` +
          `made against could not be told: ${places(untold)}.`
      );
    }
  }
  lines.push(
    "",
    "A run records no copy of the command that made it, so which bytes of fresh-agent.ts each " +
      "run was made against cannot be told from the run itself. This checkout holds " +
      `${checkout.command === null ? "no fresh-agent.ts to read" : `sha256 ${checkout.command}`}` +
      `${
        checkout.command === null
          ? ""
          : checkout.command === BUILD.command
            ? ", which is the named build."
            : ", which is not the named build: the checkout has moved since these runs were made."
      }`,
    `This checkout's skills/approval-review/SKILL.md is ` +
      `${checkout.skill === null ? "not readable" : `sha256 ${checkout.skill}`}` +
      `${
        checkout.skill === null || checkout.skill === BUILD.skill
          ? checkout.skill === null
            ? "."
            : ", which is the named build."
          : ", which is not the named build."
      }`
  );
  return lines;
}

/** The estimate and both of its intervals, in the three lines each reading prints them in. */
function estimateLines(pooling: Pooling): string[] {
  const cases = pooling.cases.length;
  const percent = `${Math.round(pooling.confidence * 100)} per cent`;
  return [
    `pooled difference ${signed(pooling.difference)} over ${count(cases)} cases`,
    `  bootstrap       ${span(pooling.bootstrap)} at ${percent}, from ` +
      `${count(pooling.resamples)} resamples of the ${count(cases)} case-level differences ` +
      `under seed ${pooling.seed}`,
    `  t interval      ${span(pooling.t)} at ${percent}, on those same ${count(cases)} ` +
      `differences, ${count(Math.max(0, cases - 1))} degrees of freedom`,
  ];
}

/**
 * The same reading over the runs that hold the named skill bytes alone, where the runs pooled
 * do not all hold them. Both readings sit where a reader meets the number, since one of them is
 * of a single build of the skill and the other is the wider evidence, and a reader given only
 * the wider one would quote a number of two builds as a number of the build the skill ships.
 */
function oneBuildLines(pooling: Pooling, verified: Pooling): string[] {
  const kept = new Set(verified.runs.map((run) => run.directory));
  const left = pooling.runs.filter((run) => !kept.has(run.directory));
  // The runs of the second reading are read a second time, so they are numbered by where they
  // are and never by which object they are.
  const places = (runs: readonly RunReading[]): string =>
    runs
      .map((run) => {
        const at = pooling.runs.findIndex((one) => one.directory === run.directory);
        return `run ${at + 1} (${run.directory})`;
      })
      .join(", ");
  return [
    "## The same reading over the runs of one build",
    "",
    ...estimateLines(verified),
    `  over            ${count(verified.runs.length)} runs, each holding the named skill ` +
      `bytes: ${places(verified.runs)}`,
    "",
    `Left out: ${places(left)}. ` +
      `${left.length === 1 ? "Its own copy" : "Their own copies"} of the skill ` +
      `${left.length === 1 ? "is" : "are"} ` +
      `${left.map((run) => run.skillSha?.slice(0, DIGEST_SHOWN) ?? "no copy at all").join(", ")}` +
      ", and not the named bytes. A run directory holds one copy of the skill, which the last " +
      "run into that directory wrote, so a copy that differs tells a reader the directory is " +
      "of more than one build and never which bytes an earlier run's arm read: the bytes " +
      `${left.length === 1 ? "that arm" : "those arms"} read are recorded nowhere.`,
    "",
    `A reader quotes the ${count(verified.runs.length)}-run number, which is the only one of ` +
      "the two that is a number of a single build of the skill the repository ships; the " +
      `${count(pooling.runs.length)}-run number belongs beside it as the wider evidence, and ` +
      "neither is quoted without the spread between the runs below.",
    "",
  ];
}

/**
 * The pooled reading as the record prints it, and beside it the same reading over the runs of
 * the named build alone, where `verified` holds one.
 */
export function reportLines(
  pooling: Pooling,
  checkout: CheckoutBuild,
  verified: Pooling | null = null
): string[] {
  const runs = pooling.runs.length;
  const cases = pooling.cases.length;
  const spread = pooling.spread;
  const lines = [
    "# The pooled reading of the fresh-agent comparison",
    "",
    `shape             ${pooling.shape}`,
    `runs pooled       ${count(runs)}`,
    `cases             ${count(cases)}, each carried by ${count(pooling.runsPerCase)} runs`,
    `arms differed on  ${count(pooling.differedInSomeRun)} of ${count(cases)} cases in at ` +
      "least one run",
    "",
    "## The runs pooled",
    "",
    ...runLines(pooling),
    "",
    "## The pooled difference",
    "",
    ...estimateLines(pooling),
    "",
    `Each case carries the mean over the ${count(runs)} runs of the skill arm's outcome and ` +
      "the mean of the control's; the pooled estimate is the mean of the " +
      `${count(cases)} case-level differences. ${count(runs)} runs of one comparison over one ` +
      "case list are " +
      `${count(runs)} observations per case and not ${count(runs)} experiments, which is why ` +
      "the estimator is read per case.",
    "",
    "McNemar's exact test is not printed here. A case's outcome is no longer binary once " +
      `${count(runs)} runs have judged it — a case the skill arm agreed on in three runs of ` +
      "five carries 0.6 — and McNemar's exact interval is built over a 2x2 table of discordant " +
      "pairs, which no such case fits. McNemar stays where a single run is reported, in " +
      "fresh-agent.ts, where each case's outcome is one judgement.",
    "",
    ...(verified === null ? [] : oneBuildLines(pooling, verified)),
    "## The spread between the runs",
    "",
    `run differences   ${pooling.runs.map((run) => signed(run.difference)).join(", ")}`,
    `lowest            ${signed(spread.lowest)}`,
    `highest           ${signed(spread.highest)}`,
    `range             ${rate(spread.range)}`,
    `standard deviation ${spread.deviation === null ? "no spread: one run" : rate(spread.deviation)}`,
    "",
    `The pooled interval does not include the spread between the runs: the bootstrap resamples ` +
      `the ${count(cases)} cases and holds these ${count(runs)} runs fixed, so it says what the ` +
      "expected difference is given these runs and carries none of the variation between them. " +
      `Single runs of this comparison ranged over ${rate(spread.range)} of agreement, from ` +
      `${signed(spread.lowest)} to ${signed(spread.highest)}.`,
    "",
    ...buildLines(pooling, checkout),
  ];
  return lines;
}

/** One whole-number flag, at or above `least`. */
function whole(value: string | undefined, flag: string, fallback: number, least: number): number {
  if (value === undefined) return fallback;
  const number = Number(value.trim());
  if (!Number.isSafeInteger(number) || number < least) {
    throw new UsageError(`${flag} takes a whole number of ${least} or above\n\n${USAGE}`);
  }
  return number;
}

/** The shape a `--shape` names, or a refusal naming the two it may be. */
function shapeOf(value: string | undefined): Shape {
  if (value === undefined) return DEFAULT_SHAPE;
  const named = value.trim();
  if (named === "with" || named === "without") return named;
  throw new UsageError(`--shape takes with or without, and not ${named}\n\n${USAGE}`);
}

function readFlags(argv: readonly string[]): Flags {
  let values: {
    run?: string[];
    shape?: string;
    resamples?: string;
    seed?: string;
    out?: string;
  };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        run: { type: "string", multiple: true },
        shape: { type: "string" },
        resamples: { type: "string" },
        seed: { type: "string" },
        out: { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
  const runs = (values.run ?? []).map((one) => one.trim()).filter((one) => one !== "");
  if (runs.length === 0) throw new UsageError(USAGE);
  return {
    runs,
    shape: shapeOf(values.shape),
    resamples: whole(values.resamples, "--resamples", DEFAULT_RESAMPLES, 1),
    seed: whole(values.seed, "--seed", DEFAULT_SEED, 0),
    out: values.out?.trim() || null,
  };
}

/** The reading as the command makes it: the runs off a disk, and the record printed and written. */
async function pool(flags: Flags): Promise<number> {
  const runs: RunOutcomes[] = [];
  for (const directory of flags.runs) runs.push(await readRun(directory, flags.shape));
  const options = { resamples: flags.resamples, seed: flags.seed, shape: flags.shape };
  const pooling = pooledOf(runs, options);
  // The runs of the named build alone, where the runs named are not all of it: two readings of
  // two different quantities, and a reader who met only the wider one would quote a number of
  // more than one build as a number of the build the skill ships.
  const ofBuild = runs.filter((one) => one.skillSha === BUILD.skill);
  const verified =
    ofBuild.length > 0 && ofBuild.length < runs.length ? pooledOf(ofBuild, options) : null;
  const checkout: CheckoutBuild = {
    command: await sha256Of(COMMAND_PATH),
    skill: await sha256Of(SKILL_PATH),
  };
  const lines = reportLines(pooling, checkout, verified);
  console.log(lines.join("\n"));
  if (flags.out !== null) {
    const out = await outDirectory(flags.out);
    await mkdir(out, { recursive: true });
    await writeFile(
      join(out, "pool-runs.json"),
      `${JSON.stringify({ ...pooling, ofNamedBuild: verified, build: { named: BUILD, checkout } }, null, 2)}\n`,
      "utf8"
    );
    await writeFile(join(out, "pool-runs.md"), `${lines.join("\n")}\n`, "utf8");
    console.log("");
    console.log(`written           ${join(out, "pool-runs.json")}`);
    console.log(`                  ${join(out, "pool-runs.md")}`);
  }
  return EXIT_OK;
}

export async function main(argv: readonly string[]): Promise<number> {
  try {
    return await pool(readFlags(argv));
  } catch (error) {
    if (
      error instanceof UsageError ||
      error instanceof OutputPathError ||
      error instanceof PoolError
    ) {
      console.error(error.message);
      return EXIT_CANNOT_RUN;
    }
    throw error;
  }
}

// Only when run: a test that imports `pooledOf` must not run a command.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
