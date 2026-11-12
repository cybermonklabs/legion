import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { type CaseReading, errorSplitOf, labelKey, readRound } from "./analyze";
import type { CaseKind, CaseRow } from "./build-corpus";
import {
  CannotRunError,
  CorpusError,
  count,
  num,
  outDirectory,
  readCorpus,
  table,
  UsageError,
} from "./corpus-io";
import type { SliceName } from "./draw";
import { RECIPES, type RecipeName } from "./recipes";
import { type JudgementRow, readRows, rowsPath } from "./round-io";
import {
  alwaysApproveBaseline,
  clopperPearsonUpper,
  DEFAULT_CONFIDENCE,
  MINIMUM_DECIDED,
} from "./stats";

/**
 * Selective prediction read off agreement across model families, which is the abstention signal
 * the round did not test.
 *
 * The round's thresholds are cut on one model's counted confidence: the share of its own five
 * judgements that agreed with each other. That number is correlated with itself by construction,
 * and no threshold on it found a subset of the report slice the arm decided well enough. This
 * reads the other signal that costs nothing to compute: the cases where arms on different models
 * answer the same thing. Where they do not, the agent abstains and the person sees the request.
 *
 * For each recipe it takes that recipe's arm on every model and forms the subset of the approval
 * cases of the report slice where all of them answered the same labels, then the same for every
 * pair of them, for a majority of them rather than unanimity, and for every arm of every recipe
 * at once. Each subset is one point of a coverage-against-bound curve: how much of the stream it
 * would take, and how wrong it would be over what it took.
 *
 * Two things keep the reading honest. A case is read exactly as the round's own analysis reads
 * it - the modal answer of the judgements that returned, a tie or fewer than three undecided -
 * so a subset here means the same thing a coverage there means, and an arm that is undecided on
 * a case agrees with nobody about it. And every subset carries the always-approve baseline
 * restricted to that same subset, because agreement is not independent of the person's answer:
 * a subset where the person approved nearly everything would let a rule that approves everything
 * look just as good, and the pair of numbers side by side is what says whether it does.
 *
 * It reads `rows.jsonl`, `round.json` and the corpus, and makes no call.
 */

const USAGE = `usage: bun scripts/approval-eval/consensus.ts --corpus <dir> --round <dir> \\
  [--out <dir>]

Prints the coverage and the bound of every subset of the report slice where arms on different
models answer the same thing, against the always-approve baseline over that same subset.

Options:
  --corpus <dir>   Required. A directory holding corpus.jsonl and snapshot.json.
  --round <dir>    Required. A directory holding the rows.jsonl a round wrote.
  --out <dir>      Where consensus.json is written. It must be an absolute path outside this
                   repository's workspace. Nothing is written where no --out is given.`;

/** The run printed its reading. */
const EXIT_PRINTED = 0;

/** The run could not start at all. */
const EXIT_CANNOT_RUN = 2;

/** The rate the question is asked at: one in twenty. */
export const RATE = 0.05;

/** The slice and the kind of case every number of this reading is over. */
const SLICE: SliceName = "report";
const KIND: CaseKind = "approval";

/** The question this reading answers, in the words it is asked in. */
export const QUESTION =
  "Does any of these consensus subsets hold a disagreement rate under 5 percent by its bound, " +
  "and at what coverage?";

/** What a group of arms must do with a case for the case to be in its subset. */
export type ConsensusRule = "unanimous" | "majority";

/** One arm of the round, as the rows name it. */
export interface ArmName {
  name: string;
  recipe: RecipeName;
  model: string;
}

/** One group of arms whose answers are compared, and the rule their answers must meet. */
export interface ConsensusGroup {
  name: string;
  arms: readonly string[];
  rule: ConsensusRule;
}

/** One case a group agreed on: the labels it agreed on, and a reading that carries them. */
export interface AgreedCase {
  caseId: string;
  labels: string[];
  /** The reading of one arm that answered the agreed labels, which is what the errors read. */
  reading: CaseReading;
}

/** What a rule that approved every case of a subset would score over that same subset. */
export interface BaselineLine {
  approvals: number;
  disagreements: number;
  rate: number;
  /** The one-sided upper bound, and null for a subset under `MINIMUM_DECIDED` cases. */
  bound: number | null;
}

/** One point of the curve: one subset, what it covers, and how wrong it is over what it covers. */
export interface ConsensusLine {
  name: string;
  rule: ConsensusRule;
  arms: string[];
  cases: number;
  /** The share of the slice's approval cases this subset holds. */
  coverage: number;
  disagreements: number;
  rate: number;
  bound: number | null;
  /** The person asked for changes and the arms approved. */
  falseApprovals: number;
  /** The person approved and the arms asked for changes. */
  falseRequests: number;
  /** A disagreement where either side chose some other option, which is neither kind. */
  otherErrors: number;
  baseline: BaselineLine;
}

/** What one run read and answered, as `consensus.json` holds it. */
export interface ConsensusRecord {
  corpus: string;
  round: string;
  person: string;
  slice: { name: SliceName; kind: CaseKind; cases: number };
  arms: ArmName[];
  /** The always-approve baseline over the whole slice, which every subset is read against. */
  baseline: BaselineLine;
  lines: ConsensusLine[];
  answer: string;
  rowsRead: number;
  rowsUnreadable: number;
  ranAt: string;
}

/** The flags as the command reads them. */
interface Flags {
  corpus: string;
  round: string;
  out: string | null;
}

/** The model as a table says it: the gateway's own name without the vendor it is served by. */
export function shortModel(model: string): string {
  return (model.split("/").pop() ?? model).replace(/^claude-/, "");
}

/** The bound as a cell, or the words that say why a subset this small carries none. */
function bound(cases: number, value: number | null): string {
  if (value !== null) return `at most ${num(value)}`;
  return `no bound at this count (${count(cases)} cases, ${MINIMUM_DECIDED} needed)`;
}

/**
 * Every group of arms the curve is read over: for each recipe, all of its arms together, each
 * pair of them, and a majority of them; and then every arm of every recipe at once. A pair takes
 * no majority of its own, a majority of two being the same subset as the pair agreeing.
 */
export function groupsOf(arms: readonly ArmName[]): ConsensusGroup[] {
  const groups: ConsensusGroup[] = [];
  const recipes = Object.keys(RECIPES) as RecipeName[];
  for (const recipe of recipes) {
    const ofRecipe = arms
      .filter((arm) => arm.recipe === recipe)
      .sort((one, two) => one.model.localeCompare(two.model));
    if (ofRecipe.length < 2) continue;
    const names = ofRecipe.map((arm) => arm.name);
    const models = ofRecipe.map((arm) => shortModel(arm.model));
    groups.push({
      name: `${recipe}: all ${models.length} agree (${models.join(", ")})`,
      arms: names,
      rule: "unanimous",
    });
    for (let one = 0; one < ofRecipe.length; one += 1) {
      for (let two = one + 1; two < ofRecipe.length; two += 1) {
        const left = ofRecipe[one];
        const right = ofRecipe[two];
        if (left === undefined || right === undefined) continue;
        groups.push({
          name: `${recipe}: ${shortModel(left.model)} and ${shortModel(right.model)} agree`,
          arms: [left.name, right.name],
          rule: "unanimous",
        });
      }
    }
    if (ofRecipe.length > 2) {
      groups.push({
        name: `${recipe}: a majority of ${models.length} agree`,
        arms: names,
        rule: "majority",
      });
    }
  }
  if (arms.length > 1) {
    groups.push({
      name: `every arm: all ${arms.length} agree`,
      arms: [...arms].map((arm) => arm.name).sort(),
      rule: "unanimous",
    });
  }
  return groups;
}

/**
 * The cases of one group's subset: those its rule agrees on. An arm with no reading of a case,
 * or one whose judgements left it undecided, agrees with nobody about that case, so unanimity
 * needs every arm decided and a majority needs more than half of the group decided on one answer.
 */
export function agreementOf(
  group: ConsensusGroup,
  byArm: ReadonlyMap<string, ReadonlyMap<string, CaseReading>>,
  caseIds: readonly string[]
): AgreedCase[] {
  const needed =
    group.rule === "unanimous" ? group.arms.length : Math.floor(group.arms.length / 2) + 1;
  const agreed: AgreedCase[] = [];
  for (const caseId of caseIds) {
    const readings = group.arms
      .map((arm) => byArm.get(arm)?.get(caseId))
      .filter((one): one is CaseReading => one !== undefined && one.verdict !== null);
    const counts = new Map<string, { labels: string[]; count: number }>();
    for (const reading of readings) {
      const key = labelKey(reading.verdict ?? []);
      const held = counts.get(key);
      if (held === undefined) counts.set(key, { labels: reading.verdict ?? [], count: 1 });
      else held.count += 1;
    }
    const ordered = [...counts.entries()].sort((one, two) => two[1].count - one[1].count);
    const best = ordered[0];
    const second = ordered[1];
    if (best === undefined || best[1].count < needed) continue;
    if (second !== undefined && second[1].count === best[1].count) continue;
    const reading = readings.find((one) => labelKey(one.verdict ?? []) === best[0]);
    if (reading === undefined) continue;
    agreed.push({ caseId, labels: best[1].labels, reading });
  }
  return agreed;
}

/** What a rule approving every case of these would score, which is the control on the subset. */
export function baselineOf(
  cases: readonly CaseRow[],
  confidence: number = DEFAULT_CONFIDENCE
): BaselineLine {
  const baseline = alwaysApproveBaseline(cases);
  const disagreements = baseline.counted - baseline.approvals;
  return {
    approvals: baseline.approvals,
    disagreements,
    rate: baseline.counted === 0 ? 0 : disagreements / baseline.counted,
    bound:
      baseline.counted >= MINIMUM_DECIDED
        ? clopperPearsonUpper(disagreements, baseline.counted, confidence)
        : null,
  };
}

/** One group read as one point of the curve. */
export function consensusLine(
  group: ConsensusGroup,
  agreed: readonly AgreedCase[],
  slice: number,
  confidence: number = DEFAULT_CONFIDENCE
): ConsensusLine {
  const errors = errorSplitOf(agreed.map((one) => one.reading));
  const disagreements = agreed.filter((one) => !one.reading.agreed).length;
  return {
    name: group.name,
    rule: group.rule,
    arms: [...group.arms],
    cases: agreed.length,
    coverage: slice === 0 ? 0 : agreed.length / slice,
    disagreements,
    rate: agreed.length === 0 ? 0 : disagreements / agreed.length,
    bound:
      agreed.length >= MINIMUM_DECIDED
        ? clopperPearsonUpper(disagreements, agreed.length, confidence)
        : null,
    falseApprovals: errors.falseApprovals.count,
    falseRequests: errors.falseRequests.count,
    otherErrors: errors.other,
    baseline: baselineOf(
      agreed.map((one) => one.reading.row),
      confidence
    ),
  };
}

/**
 * The answer the question takes over the curve: the subsets that hold the rate, or, where none
 * does, the lowest bound reached with the coverage and the count behind it.
 */
export function answerOf(lines: readonly ConsensusLine[]): string {
  const under = lines
    .filter((line) => line.bound !== null && line.bound < RATE)
    .sort((one, two) => two.coverage - one.coverage);
  const small = lines.filter((line) => line.bound === null).length;
  const tail =
    small === 0
      ? ""
      : ` ${count(small)} of ${count(lines.length)} ${small === 1 ? "holds" : "hold"} ` +
        `fewer than ${MINIMUM_DECIDED} cases and so ${small === 1 ? "carries" : "carry"} ` +
        "no bound.";
  if (under.length > 0) {
    const said = under
      .map(
        (line) =>
          `${line.name} at coverage ${num(line.coverage)} (${count(line.cases)} cases, ` +
          `${count(line.disagreements)} disagreements, bound at most ${num(line.bound ?? 0)}, ` +
          `against an always-approve bound of ` +
          `${line.baseline.bound === null ? "none at this count" : num(line.baseline.bound)} ` +
          "over the same cases)"
      )
      .join("; ");
    return (
      `Yes. ${count(under.length)} of ${count(lines.length)} subsets hold a disagreement ` +
      `rate under 5 percent by their bound: ${said}.${tail}`
    );
  }
  const bounded = lines.filter((line) => line.bound !== null);
  if (bounded.length === 0) {
    return (
      `No. No subset holds ${MINIMUM_DECIDED} cases, so none of them carries a bound ` +
      `at all.${tail}`
    );
  }
  const lowest = bounded.reduce((one, two) => ((two.bound ?? 1) < (one.bound ?? 1) ? two : one));
  return (
    `No. No consensus subset holds a disagreement rate under 5 percent by its bound. The ` +
    `lowest bound reached is ${num(lowest.bound ?? 1)}, by ${lowest.name}, at coverage ` +
    `${num(lowest.coverage)} over ${count(lowest.cases)} cases of which ` +
    `${count(lowest.disagreements)} disagree (rate ${num(lowest.rate)}). Over those same cases ` +
    `an always-approve rule disagrees at ${num(lowest.baseline.rate)}, bound ` +
    `${lowest.baseline.bound === null ? "none at this count" : `at most ${num(lowest.baseline.bound)}`}.${tail}`
  );
}

/** Every case of the slice this reading is over, in the order the table reads them. */
export function sliceCaseIds(
  rows: readonly JudgementRow[],
  corpus: ReadonlyMap<string, CaseRow>
): string[] {
  const ids = new Set<string>();
  for (const row of rows) {
    if (row.slice !== SLICE || row.kind !== KIND) continue;
    const one = corpus.get(row.caseId);
    if (one === undefined) continue;
    if (one.verdict !== "approve" && one.verdict !== "request_changes") continue;
    ids.add(row.caseId);
  }
  return [...ids].sort();
}

/**
 * Each arm's reading of each case of the slice, by the round's own rule for reading a case.
 * That rule is `readRound` of `analyze.ts` and is not written again here: a subset of this
 * curve means what a coverage there means only while one rule reads both. This adds what the
 * curve needs of it — the cases of this slice alone, and the readings by arm and case.
 */
export function readingsOf(
  rows: readonly JudgementRow[],
  corpus: ReadonlyMap<string, CaseRow>,
  caseIds: readonly string[]
): { arms: ArmName[]; byArm: Map<string, Map<string, CaseReading>> } {
  const wanted = new Set(caseIds);
  const ofSlice = rows.filter((row) => wanted.has(row.caseId));
  const names = new Map<string, ArmName>();
  for (const row of ofSlice) {
    names.set(row.arm, { name: row.arm, recipe: row.recipe, model: row.model });
  }
  const byArm = new Map<string, Map<string, CaseReading>>();
  for (const arm of readRound(ofSlice, corpus).arms) {
    byArm.set(arm.name, new Map(arm.cases.map((one) => [one.caseId, one])));
  }
  const arms = [...names.values()].sort((one, two) => one.name.localeCompare(two.name));
  return { arms, byArm };
}

/** The whole curve, every group read over the same slice and ordered by coverage. */
export function consensusLines(
  groups: readonly ConsensusGroup[],
  byArm: ReadonlyMap<string, ReadonlyMap<string, CaseReading>>,
  caseIds: readonly string[]
): ConsensusLine[] {
  return groups
    .map((group) => consensusLine(group, agreementOf(group, byArm, caseIds), caseIds.length))
    .sort((one, two) => two.coverage - one.coverage || one.name.localeCompare(two.name));
}

/** The whole record, in the order it is printed and written. */
export function renderConsensus(record: ConsensusRecord): string[] {
  return [
    "# Agreement across model families, as an abstention signal",
    "",
    `corpus            ${record.corpus}`,
    `round             ${record.round}`,
    `person            ${record.person}`,
    `slice             ${record.slice.name}, ${record.slice.kind} cases: ` +
      `${count(record.slice.cases)}, which every coverage below is a share of`,
    `arms              ${record.arms.length}: ${record.arms.map((arm) => arm.name).join(", ")}`,
    `rows              ${count(record.rowsRead)} read, ${count(record.rowsUnreadable)} unreadable`,
    `bound             one-sided Clopper-Pearson at ${num(100 * DEFAULT_CONFIDENCE, 0)} percent`,
    `read at           ${record.ranAt}`,
    "",
    "A case is read as the round's own analysis reads it: the modal answer of the judgements " +
      "that returned, a tie or fewer than three undecided. An arm undecided on a case agrees " +
      "with nobody about it, so that case is in no subset the arm is part of.",
    "",
    `Over the whole slice the person approved ${count(record.baseline.approvals)} of ` +
      `${count(record.slice.cases)} cases, so a rule that approved everything would disagree at ` +
      `${num(record.baseline.rate)}, bound ${bound(record.slice.cases, record.baseline.bound)}. ` +
      "The last two columns are that same baseline restricted to each subset.",
    "",
    "## Coverage against bound",
    "",
    QUESTION,
    "",
    record.answer,
    "",
    ...table(
      [
        "subset",
        "rule",
        "cases",
        "coverage",
        "disagreements",
        "rate",
        "bound",
        "false approvals",
        "false requests",
        "always-approve rate",
        "always-approve bound",
      ],
      record.lines.map((line) => [
        line.name,
        line.rule,
        count(line.cases),
        num(line.coverage),
        count(line.disagreements),
        num(line.rate),
        bound(line.cases, line.bound),
        count(line.falseApprovals),
        count(line.falseRequests),
        num(line.baseline.rate),
        bound(line.cases, line.baseline.bound),
      ])
    ),
    "",
    `A disagreement that is neither kind - either side choosing some other option - is counted ` +
      `apart: ${count(record.lines.reduce((sum, line) => sum + line.otherErrors, 0))} over every ` +
      "subset together.",
    "",
  ];
}

function readFlags(argv: readonly string[]): Flags {
  let values: { corpus?: string; round?: string; out?: string };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        corpus: { type: "string" },
        round: { type: "string" },
        out: { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
  const corpus = values.corpus?.trim();
  const round = values.round?.trim();
  if (!corpus || !round) throw new UsageError(USAGE);
  return { corpus, round, out: values.out?.trim() || null };
}

/** The run as the command makes it: the real corpus, the real rows, and no call. */
async function consensus(flags: Flags): Promise<number> {
  const out = flags.out === null ? null : await outDirectory(flags.out);
  const corpus = await readCorpus(flags.corpus);
  const { rows, read, unreadable } = await readRows(flags.round);
  const caseIds = sliceCaseIds(rows, corpus.byCaseId);
  if (caseIds.length === 0) {
    throw new CorpusError(
      `${rowsPath(flags.round)} names no ${KIND} case of the ${SLICE} slice that this corpus ` +
        "knows: --round takes a directory run-round.ts wrote"
    );
  }
  const { arms, byArm } = readingsOf(rows, corpus.byCaseId, caseIds);
  const lines = consensusLines(groupsOf(arms), byArm, caseIds);
  const cases = caseIds
    .map((caseId) => corpus.byCaseId.get(caseId))
    .filter((one): one is CaseRow => one !== undefined);
  const record: ConsensusRecord = {
    corpus: corpus.directory,
    round: flags.round,
    person: corpus.snapshot.person,
    slice: { name: SLICE, kind: KIND, cases: caseIds.length },
    arms,
    baseline: baselineOf(cases),
    lines,
    answer: answerOf(lines),
    rowsRead: read,
    rowsUnreadable: unreadable,
    ranAt: new Date().toISOString(),
  };
  for (const line of renderConsensus(record)) console.log(line);
  if (out !== null) {
    await mkdir(out, { recursive: true });
    const path = join(out, "consensus.json");
    await writeFile(path, `${JSON.stringify(record, null, 2)}\n`, "utf8");
    console.log("");
    console.log(`written           ${path}`);
  }
  return EXIT_PRINTED;
}

/** The command: the flags, the reading, and the exit status it takes. */
export async function main(argv: readonly string[]): Promise<number> {
  let flags: Flags;
  try {
    flags = readFlags(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(error.message);
    return EXIT_CANNOT_RUN;
  }
  try {
    return await consensus(flags);
  } catch (error) {
    if (error instanceof CannotRunError) {
      console.error(`${error.name}: ${error.message}`);
      return EXIT_CANNOT_RUN;
    }
    throw error;
  }
}

// Only when run: a test that imports the reading must not run a command.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
