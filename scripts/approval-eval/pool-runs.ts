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
 * **One run is an input too.** The command pools whatever runs `--run` names, down to one, so
 * every sentence of the record is written to hold over one run as over several: a count goes
 * into a sentence beside the noun it counts, the paragraph on the estimator says that one run
 * gives one observation per case and that the mean over runs is then that run's own outcome,
 * and the spread section says there is no spread to read rather than printing a lowest, a
 * highest and a range over a set of one, which would read as a spread measured and found to
 * be none.
 *
 * The record names the runs it pooled by their directories, and groups them by the build each
 * was made against: a run directory holds the copy of the skill that run's skill arm read, so
 * the sha256 of that copy is the name of that run's build, read off the run itself and written
 * by hand nowhere. A pooled number over runs of two builds is a number of neither, so the
 * record carries the reading over every run named and, beside it, one reading per build, each
 * naming its own runs and carrying its own interval, resamples and seed. Every reading narrower
 * than the reading over every run carries one sentence, generated from the two estimates,
 * saying which runs it leaves out and whether leaving them out raises or lowers the estimate
 * and by how much. A run whose directory holds no copy of the skill is of no build: it is
 * named, its provenance is said to be unrecorded, and it is left out of every per-build reading
 * while staying in the reading over every run.
 *
 * This checkout is compared against the builds the runs fall into and never against a constant.
 * Where this checkout's own skill bytes are none of them, the record says why no run can ever
 * be of the bytes that quote its own result: writing a measured number into a file changes the
 * file.
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

/** Where a run's own copy of the skill sits, which `fresh-agent.ts` writes under `--out`. */
const SKILL_IN_RUN = join("skills", "approval-review", "SKILL.md");

/** Where this checkout's own copy of the command and of the skill sits. */
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

/** One run as a reading that is not the whole one names it: where it sits, and what it holds. */
export interface RunPlace {
  /** Where the run sits among the runs given, counting from one, as every line names it. */
  place: number;
  directory: string;
  /** The sha256 of the run's own copy of the skill, and null where it holds no copy. */
  skillSha: string | null;
}

/**
 * One build the runs fall into: the skill bytes the runs of it hold their own copies of, and
 * the reading over those runs alone. The bytes are the build's only name, since a run records
 * no other name for what its skill arm read.
 */
export interface BuildReading {
  /** The sha256 of the skill every run of this build holds its own copy of. */
  skillSha: string;
  /** The runs of this build, in the order the runs were given. */
  runs: RunPlace[];
  /** The reading over those runs alone, drawn as the reading over every run is drawn. */
  pooling: Pooling;
  /** The runs this reading leaves out, which is empty where it is over every run given. */
  leftOut: RunPlace[];
  /** How far this reading's estimate sits from the reading over every run, signed. */
  shift: number;
}

/** The record whole: the reading over every run, each build's reading, and this checkout. */
export interface PooledRecord {
  /** The reading over every run given, whichever build each of them was made against. */
  whole: Pooling;
  /** One reading per build, in the order the runs first name each build. */
  builds: BuildReading[];
  /** The runs holding no copy of the skill, which are of no build and in no build's reading. */
  unrecorded: RunPlace[];
  /** What this checkout holds now of the command and of the skill. */
  checkout: CheckoutBuild;
  /** The build this checkout's skill bytes are, and null where they are none of the builds. */
  checkoutBuild: string | null;
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

/**
 * A difference as the record prints it, which always carries its sign. A value whose magnitude
 * rounds away takes `+`, so that two numbers equal at the precision printed are never printed
 * with opposite signs, which is how a reading that moved by nothing came to read as two.
 */
function signed(value: number): string {
  const magnitude = Math.abs(value).toFixed(3);
  return `${value < 0 && magnitude !== rate(0) ? "−" : "+"}${magnitude}`;
}

/** An interval as the record prints it. */
function span(interval: Interval): string {
  return `[${signed(interval.lower)}, ${signed(interval.upper)}]`;
}

/** A count, as every printed count here is written. */
function count(value: number): string {
  return value.toLocaleString("en-US");
}

/**
 * A count beside the noun it counts, which is how every count this record writes into a
 * sentence is written. The command takes one run as readily as several, so a sentence that
 * carries a bare count reads as "1 runs" over an input the command accepts.
 */
function counted(value: number, one: string, several = `${one}s`): string {
  return `${count(value)} ${value === 1 ? one : several}`;
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

/** What this checkout holds now of the command and of the skill, each read off this checkout. */
export interface CheckoutBuild {
  command: string | null;
  skill: string | null;
}

/**
 * The record over the runs given: the reading over every one of them, one reading per build
 * they fall into, the runs whose provenance no copy of the skill records, and this checkout
 * beside those builds. Every hash it carries is one read off a file.
 */
export function recordOf(
  runs: readonly RunOutcomes[],
  checkout: CheckoutBuild,
  options: PoolOptions = {}
): PooledRecord {
  const whole = pooledOf(runs, options);
  // One place per run given, in the order they were given, which every line that names a run
  // reads out of this list rather than recovering it from the pooled reading.
  const runPlaces: RunPlace[] = runs.map((run, at) => ({
    place: at + 1,
    directory: run.directory,
    skillSha: run.skillSha,
  }));
  // Insertion order, so the builds are read in the order the runs first name them.
  const byBuild = new Map<string, number[]>();
  const unrecorded: RunPlace[] = [];
  runs.forEach((run, at) => {
    if (run.skillSha === null) {
      unrecorded.push(runPlaces[at] as RunPlace);
      return;
    }
    const held = byBuild.get(run.skillSha) ?? [];
    byBuild.set(run.skillSha, held);
    held.push(at);
  });
  const builds = [...byBuild].map(([skillSha, places]): BuildReading => {
    const kept = new Set(places);
    const leftOut: RunPlace[] = [];
    for (let at = 0; at < runs.length; at += 1) {
      if (!kept.has(at)) leftOut.push(runPlaces[at] as RunPlace);
    }
    // A build holding every run is the reading over every run: the same runs in the same
    // order under the same options, and the bootstrap draws the same interval under the same
    // seed. So the reading already in hand is that reading, and its resamples are not redrawn.
    const pooling =
      places.length === runs.length
        ? whole
        : pooledOf(
            places.map((at) => runs[at] as RunOutcomes),
            options
          );
    return {
      skillSha,
      runs: places.map((at) => runPlaces[at] as RunPlace),
      pooling,
      leftOut,
      shift: pooling.difference - whole.difference,
    };
  });
  return {
    whole,
    builds,
    unrecorded,
    checkout,
    checkoutBuild: builds.find((build) => build.skillSha === checkout.skill)?.skillSha ?? null,
  };
}

/** The record as the JSON beside the markdown carries it, each build under its own hash. */
export interface RecordJson {
  whole: Pooling;
  builds: Record<string, BuildReading>;
  unrecorded: RunPlace[];
  checkout: CheckoutBuild & { build: string | null };
}

/**
 * The record as JSON. Its readers are other programs, so it carries every fact the markdown
 * carries: the whole reading, each build's reading under that build's hash, the runs of
 * unrecorded provenance, and this checkout's own bytes with the build they are.
 */
export function recordJson(record: PooledRecord): RecordJson {
  return {
    whole: record.whole,
    builds: Object.fromEntries(record.builds.map((build) => [build.skillSha, build])),
    unrecorded: record.unrecorded,
    checkout: { ...record.checkout, build: record.checkoutBuild },
  };
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
      // The build this run was made against: the digest of the run's own copy of the skill.
      run.skillSha === null
        ? "no copy of the skill under this run, so it cannot be told"
        : run.skillSha.slice(0, DIGEST_SHOWN),
    ])
  );
}

/** The runs named as every line that names one names them: where each sits, and where it is. */
function placesSay(places: readonly RunPlace[]): string {
  return places.map((place) => `run ${place.place} (${place.directory})`).join(", ");
}

/**
 * Which way a reading narrower than the reading over every run cuts: which runs it leaves out,
 * and whether leaving them out raises or lowers the estimate, and by how much. It is generated
 * from the two estimates, so a narrowing that moves the number towards a claim carries it, and
 * one that moves the number away from a claim earns its trust by carrying it too.
 */
function narrowingSays(build: BuildReading, whole: Pooling): string {
  // The sentence prints the shift at the precision `rate` prints at, so the branch is decided
  // at that same precision: a shift that prints as 0.000 is not said to raise or lower
  // anything, and two estimates that are equal in arithmetic are rarely equal bit for bit.
  const moved = rate(Math.abs(build.shift));
  const way =
    moved === rate(0)
      ? "leaves the estimate where it is"
      : `${build.shift > 0 ? "raises" : "lowers"} the estimate by ${moved}`;
  const one = build.leftOut.length === 1;
  const all = counted(whole.runs.length, "run");
  const kept = counted(build.pooling.runs.length, "run");
  return (
    `This reading is narrower than the reading over all ${all}: it leaves out ` +
    `${placesSay(build.leftOut)}, and leaving ${one ? "that run" : "those runs"} out ${way}, ` +
    `from ${signed(whole.difference)} over ${all} to ${signed(build.pooling.difference)} ` +
    `over ${kept}.`
  );
}

/** One build's reading: the bytes that name it, its runs, its estimate, and what it leaves out. */
function buildLines(build: BuildReading, whole: Pooling): string[] {
  const runs = build.pooling.runs.length;
  const lines = [
    `### Build ${build.skillSha.slice(0, DIGEST_SHOWN)} — ${counted(runs, "run")}`,
    "",
    `skill bytes       sha256 ${build.skillSha}`,
    `runs              ${placesSay(build.runs)}`,
    "",
    ...estimateLines(build.pooling),
    "",
  ];
  if (runs === 1) {
    lines.push(
      "One run holds these bytes, so this is that run's own reading and not a pooling of " +
        "several: its interval is drawn over its own cases, and it has no spread between runs " +
        "to read.",
      ""
    );
  }
  if (build.leftOut.length > 0) lines.push(narrowingSays(build, whole), "");
  return lines;
}

/** The builds the runs fall into, each read on its own, and the runs that are of no build. */
function buildsLines(record: PooledRecord): string[] {
  const { whole, builds, unrecorded } = record;
  const recorded = whole.runs.length - unrecorded.length;
  const lines = [
    whole.runs.length === 1
      ? "## The build this run falls into"
      : "## The builds these runs fall into",
    "",
    "A run directory holds the copy of skills/approval-review/SKILL.md that run's skill arm " +
      "read, so the sha256 of that copy is the build of that run, read off the run itself and " +
      "written by hand nowhere. A pooled number over runs of two builds is a number of " +
      "neither, so each build is read on its own here, beside the reading over every run above.",
    "",
  ];
  if (builds.length === 0) {
    lines.push(
      whole.runs.length === 1
        ? "The one run holds no copy of the skill, so its build cannot be told and there is " +
            "no reading of one build to make."
        : `No one of the ${count(whole.runs.length)} runs holds a copy of the skill, so no ` +
            "run's build can be told and there is no reading of one build to make.",
      ""
    );
  } else {
    lines.push(
      `The ${counted(recorded, "run")} whose provenance is recorded ` +
        `${recorded === 1 ? "falls" : "fall"} into ${counted(builds.length, "build")}.`,
      ""
    );
    for (const build of builds) lines.push(...buildLines(build, whole));
  }
  if (unrecorded.length > 0) {
    const one = unrecorded.length === 1;
    // Over one run "1 of the 1 runs" is the count said twice and neither time in words a
    // sentence about one run can hold, so that record names the run it has.
    const which =
      whole.runs.length === 1
        ? "The one run holds no copy of the skill"
        : `${count(unrecorded.length)} of the ${count(whole.runs.length)} runs ` +
          `${one ? "holds" : "hold"} no copy of the skill`;
    lines.push(
      `${which}: ${placesSay(unrecorded)}. ` +
        `${one ? "Its" : "Their"} provenance is unrecorded — the bytes ` +
        `${one ? "that arm" : "those arms"} read are recorded nowhere — so ` +
        `${one ? "it is" : "they are"} of no build and ${one ? "is" : "are"} left out of every ` +
        "per-build reading here, while staying in the reading over every run above.",
      ""
    );
  }
  return lines;
}

/** This checkout's own bytes beside the builds the runs fall into. */
function checkoutLines(record: PooledRecord): string[] {
  const { checkout, checkoutBuild } = record;
  // The bytes a reading was made against are the bytes of its runs, which the command takes
  // one of as readily as several.
  const made =
    record.whole.runs.length === 1
      ? "the bytes the run was made against"
      : "the bytes the runs were made against";
  const lines = ["## This checkout beside those builds", ""];
  if (checkout.skill === null) {
    lines.push(
      "This checkout holds no skills/approval-review/SKILL.md to read, so its own bytes are " +
        "none of the builds above."
    );
  } else {
    lines.push(
      `skills/approval-review/SKILL.md  sha256 ${checkout.skill}`,
      "",
      checkoutBuild === null
        ? "These bytes are none of the builds above, and no run can ever be of the bytes that " +
            "quote its own result, because writing a measured number into a file changes the " +
            `file. So a number quoted in this checkout's skill is a number of ${made} ` +
            "and never of the bytes that quote it, which is a standing " +
            "property of this measurement and not a fault to be put right."
        : `These bytes are build ${checkoutBuild.slice(0, DIGEST_SHOWN)} above, so that ` +
            "build's reading is a reading of the skill bytes this checkout holds now."
    );
  }
  lines.push(
    "",
    "A run records no copy of the command that made it, so which bytes of fresh-agent.ts a run " +
      "was made against cannot be told from the run itself. This checkout holds " +
      `${checkout.command === null ? "no fresh-agent.ts to read" : `sha256 ${checkout.command}`}.`
  );
  return lines;
}

/** The estimate and both of its intervals, in the three lines each reading prints them in. */
function estimateLines(pooling: Pooling): string[] {
  const cases = pooling.cases.length;
  const percent = `${Math.round(pooling.confidence * 100)} per cent`;
  // A reading over one run is that run's own reading, and calling it pooled would dress it as
  // a pooling of several that it is not.
  const named = (pooling.runs.length === 1 ? "difference" : "pooled difference").padEnd(17);
  return [
    `${named} ${signed(pooling.difference)} over ${count(cases)} cases`,
    `  bootstrap       ${span(pooling.bootstrap)} at ${percent}, from ` +
      `${count(pooling.resamples)} resamples of the ${count(cases)} case-level differences ` +
      `under seed ${pooling.seed}`,
    `  t interval      ${span(pooling.t)} at ${percent}, on those same ${count(cases)} ` +
      `differences, ${counted(Math.max(0, cases - 1), "degree")} of freedom`,
  ];
}

/**
 * Why McNemar's exact test is not the interval printed here, which is a different sentence
 * over one run and over several. Over one run each case carries one judgement, so it is binary
 * and does fit the 2x2 table of discordant pairs that test is built over, which is why the
 * test belongs where a single run is reported. Over several runs a case carries a share of the
 * runs, which no such table holds.
 */
function mcnemarSays(runs: number): string {
  if (runs === 1) {
    return (
      "McNemar's exact test is not printed here. One run judges each case once, so every case " +
      "of this reading carries 0 or 1 and does fit the 2x2 table of discordant pairs McNemar's " +
      "exact interval is built over. One judgement per case being binary is exactly why McNemar " +
      "is printed where a single run is reported, in fresh-agent.ts. This command reads over " +
      "any number of runs, so it prints the estimator that holds over all of them: the " +
      "bootstrap over the case-level differences."
    );
  }
  return (
    "McNemar's exact test is not printed here. A case's outcome is no longer binary once " +
    `${count(runs)} runs have judged it — a case the skill arm agreed on in one of them ` +
    `carries ${rate(1 / runs)} — and McNemar's exact interval is built over a 2x2 table of ` +
    "discordant pairs, which no such case fits. McNemar stays where a single run is reported, " +
    "in fresh-agent.ts, where each case's outcome is one judgement."
  );
}

/**
 * Why the estimator is read per case, which is a different sentence over one run and over
 * several. Over several runs a case carries a share of those runs, which are repeated
 * observations of one case list and not experiments of their own. Over one run the mean over
 * runs is that run's own outcome, so the sentence that counts observations against
 * experiments has one of each to count.
 */
function estimatorSays(runs: number, cases: number): string {
  if (runs === 1) {
    return (
      "Each case carries this run's own outcome in the skill arm and in the control, since " +
      "the mean over one run is that run's own outcome; the estimate is the mean of the " +
      `${count(cases)} case-level differences. One run of one comparison over one case list ` +
      "is one observation per case and not one experiment, which is why the estimator is read " +
      "per case."
    );
  }
  return (
    `Each case carries the mean over the ${count(runs)} runs of the skill arm's outcome and ` +
    "the mean of the control's; the pooled estimate is the mean of the " +
    `${count(cases)} case-level differences. ${count(runs)} runs of one comparison over one ` +
    "case list are " +
    `${count(runs)} observations per case and not ${count(runs)} experiments, which is why ` +
    "the estimator is read per case."
  );
}

/**
 * The spread between the runs, which is a section over several runs and a sentence saying
 * there is none over one. A set of one has a lowest and a highest that are the one number and
 * a range of 0.000, and printing those is a spread measured and found to be none, which is a
 * claim one run cannot carry: the build of one run above already says so of itself. A spread
 * with no deviation is a spread of one run, since a reading with no run at all is refused
 * before it is drawn.
 */
function spreadLines(pooling: Pooling): string[] {
  const cases = pooling.cases.length;
  const { lowest, highest, range, deviation } = pooling.spread;
  if (deviation === null) {
    return [
      "## The spread between runs, which one run cannot show",
      "",
      "One run gives one difference, so there is no spread between runs to read here: a " +
        "lowest, a highest and a range over a set of one are that one difference and 0.000, " +
        "which would read as a spread measured and found to be none. The interval above is " +
        `drawn over the ${count(cases)} cases of this one run and holds the run itself fixed, ` +
        "so it carries none of the variation between runs of this comparison — pooling a " +
        "second run of it is what would first read that variation.",
      "",
    ];
  }
  return [
    "## The spread between the runs",
    "",
    `run differences   ${pooling.runs.map((run) => signed(run.difference)).join(", ")}`,
    `lowest            ${signed(lowest)}`,
    `highest           ${signed(highest)}`,
    `range             ${rate(range)}`,
    `standard deviation ${rate(deviation)}`,
    "",
    "The pooled interval does not include the spread between the runs: the bootstrap " +
      `resamples the ${count(cases)} cases and holds these ${count(pooling.runs.length)} runs ` +
      "fixed, so it says what the expected difference is given these runs and carries none of " +
      "the variation between them. Single runs of this comparison ranged over " +
      `${rate(range)} of agreement, from ${signed(lowest)} to ${signed(highest)}.`,
    "",
  ];
}

/**
 * The record as it is printed and written: the reading over every run, the reading of each
 * build those runs fall into, the spread between the runs, and this checkout beside those
 * builds. No line of it carries a hash a person typed.
 */
export function reportLines(record: PooledRecord): string[] {
  const pooling = record.whole;
  const runs = pooling.runs.length;
  const cases = pooling.cases.length;
  const unrecorded = record.unrecorded.length;
  const one = runs === 1;
  return [
    "# The pooled reading of the fresh-agent comparison",
    "",
    `shape             ${pooling.shape}`,
    `runs pooled       ${count(runs)}`,
    `cases             ${count(cases)}, each carried by ${counted(pooling.runsPerCase, "run")}`,
    `arms differed on  ${count(pooling.differedInSomeRun)} of ${count(cases)} cases in at ` +
      "least one run",
    `builds            ${count(record.builds.length)} by ` +
      (one ? "the run's own copy of the skill" : "the runs' own copies of the skill") +
      (unrecorded === 0 ? "" : `, and ${counted(unrecorded, "run")} of unrecorded provenance`),
    "",
    one ? "## The run pooled" : "## The runs pooled",
    "",
    ...runLines(pooling),
    "",
    "## The reading over every run",
    "",
    ...estimateLines(pooling),
    "",
    estimatorSays(runs, cases),
    "",
    mcnemarSays(runs),
    "",
    ...buildsLines(record),
    ...spreadLines(pooling),
    ...checkoutLines(record),
  ];
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
  const checkout: CheckoutBuild = {
    command: await sha256Of(COMMAND_PATH),
    skill: await sha256Of(SKILL_PATH),
  };
  // The builds are the runs' own, read off each run's copy of the skill: which builds there
  // are is the runs' to say, and a reading over runs of two builds is a number of neither.
  const record = recordOf(runs, checkout, options);
  const lines = reportLines(record);
  console.log(lines.join("\n"));
  if (flags.out !== null) {
    const out = await outDirectory(flags.out);
    await mkdir(out, { recursive: true });
    await writeFile(
      join(out, "pool-runs.json"),
      `${JSON.stringify(recordJson(record), null, 2)}\n`,
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
