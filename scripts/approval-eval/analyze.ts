import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import type { CaseKind, CaseRow } from "./build-corpus";
import {
  type Corpus,
  CorpusError,
  newSeed,
  OutputPathError,
  outDirectory,
  pickAtRandom,
  readCorpus,
  recordOf,
  UsageError,
} from "./corpus-io";
import { leadDocumentOf, type SliceName } from "./draw";
import type { JudgementRow, RoundSummary } from "./run-round";
import {
  alwaysApproveBaseline,
  type Baseline,
  type Bin,
  type BinLine,
  binReport,
  chooseThreshold,
  clopperPearsonInterval,
  clopperPearsonUpper,
  DEFAULT_CONFIDENCE,
  decidedAt,
  type Interval,
  type Judgement,
  MINIMUM_DECIDED,
  mcnemarExact,
  type PairedCase,
  type PairedComparison,
  riskCoverageCurve,
  zeroDisagreementBand,
} from "./stats";

/**
 * Reads one round's judgement rows and prints the report the pre-analysis plan asks for
 * (dispatch://INFRA-2897/artifact/plan-of-analysis-md@v2).
 *
 * It reads two things and calls nothing: `rows.jsonl` and `round.json` of a round directory, and
 * the corpus the round scored. A round still running has written part of its rows and no
 * summary, which is a report over fewer cases and never a crash: every table renders, every
 * number prints its n, and a number too thin to bound prints the words that say so instead.
 *
 * **How a case is scored.** The case's verdict is the mode of the judgements that returned. A
 * tie for the mode is undecided: it counts against coverage and is never a disagreement. The
 * counted confidence is the share of the judgements that returned that chose the mode, and a
 * case with fewer than three returned is undecided. Agreement is set equality between the labels
 * the arm chose and the labels the person chose. A faulted judgement takes no case out of the
 * coverage denominator; an arm missing more than 5 percent of its judgements carries no bound at
 * all, which is printed in place of every bound of that arm.
 *
 * **What is fitted and what is reported.** An arm's threshold is fitted on the fit slice alone
 * and applied to the report slice, and the approval cases and the question cases are reported
 * apart and never pooled. The winner's bound is printed twice: pointwise, and adjusted for the
 * choice among twelve arms.
 *
 * **What comes from the corpus and not from the round.** The class table is a pass over the
 * corpus's own rows: it reads no judgement, and says which classes of case the person answered
 * the same way every time. An arm's number says how well a model predicts the person's answer;
 * that table says where there was anything to predict.
 */

const USAGE = `usage: bun scripts/approval-eval/analyze.ts --corpus <dir> --round <dir> \\
  [--out <dir>] [--confidence-table] [--cases <file>] [--format text|json]

  --corpus            the corpus directory a build wrote, which the round scored
  --round             the round directory, holding rows.jsonl and round.json
  --out               where report.md and report.json are written, which must be an absolute
                      path outside this repository's workspace
  --confidence-table  print the counted confidence of one generative arm over 30 report-slice
                      cases the person decided both ways, beside Jev's own probability on them
  --cases <file>      the 30 case ids that table reads, one per line or as a JSON list; with
                      none, they are picked from the report slice under a printed seed and
                      written to --out, so a later run reads the same 30
  --format            text, the report as it prints, or json, the same numbers as one object

Exit status: 0 when the report was printed, 2 when it could not run.`;

/** The disagreement rate the round is held to, which is the person's own 1 in 20. */
const RATE = 0.05;

/** How many arms the winner is chosen among, which is what the adjusted bound corrects for. */
const ARMS_CHOSEN_AMONG = 12;

/** The confidence the winner's adjusted bound is read at: 1 - 0.10 / 12, by Bonferroni. */
const ADJUSTED_CONFIDENCE = 1 - (1 - DEFAULT_CONFIDENCE) / ARMS_CHOSEN_AMONG;

/** Coverage at or above this is useful; under it the skill records the curve and no verdict rule. */
const USEFUL_COVERAGE = 0.3;

/** How many judgements must return before a case is decided at all. */
const LEAST_RETURNED = 3;

/** How much of an arm's judgements may be missing before the arm carries no bound. */
const MOST_MISSING = 0.05;

/** The corpus median of the person's answers that day before this one, as the plan declares it. */
const ANSWERS_MEDIAN = 85;

/** Where the person's working afternoon starts, in UTC, as the plan's confound table splits it. */
const AFTERNOON_START_UTC = 15;

/** The project the project confound splits on, every other project being the other side. */
const MAIN_PROJECT = "INFRA";

/** How many cases the confidence table reads, half of each answer the person gave. */
const CONFIDENCE_TABLE_CASES = 30;

/** How many judgements each case and arm was asked, where nothing read from the round says. */
const DEFAULT_JUDGEMENTS = 5;

/** How wide Jev's own stated probability is binned, it taking a value of its own on every case. */
const PROBABILITY_BIN = 0.1;

/** How many case ids of a picked confidence-table set one printed line carries. */
const IDS_PER_LINE = 2;

/**
 * Dollars per million tokens, by model. These are the list prices the model catalog this
 * deployment ships carries for these models, and they are what "measured cost" multiplies the
 * round's own token counts by. A model with no entry here prints its token counts and no cost,
 * and the decision rule's cost tie-break passes over it.
 */
const PRICES: Readonly<Record<string, { input: number; output: number }>> = {
  "anthropic/claude-opus-5-5": { input: 4, output: 20 },
  "anthropic/claude-fable-5-1": { input: 10, output: 50 },
  "jev-latest": { input: 0.042, output: 0 },
};

/** What one million is, since a price is quoted per million tokens. */
const PER_MILLION = 1_000_000;

/** Which end of a Jev state the runner's cut takes, read out of `models.ts`'s `cutState`. */
const CUT_SENTENCE =
  "Where Jev refuses a state for its size the runner halves it and sends it again, and " +
  "`cutState` in models.ts keeps the head and the tail of the state in equal parts and drops " +
  "the middle, marking the gap in the text it sends. What a cut Jev read loses is therefore " +
  "the middle of the evidence: the opening of the first document and the close of the last " +
  "survive, and the body between them does not.";

/** The flags, as the command reads them. */
interface Flags {
  corpus: string;
  round: string;
  out?: string;
  confidenceTable: boolean;
  cases?: string;
  format: "text" | "json";
}

/** A share with its count, and either its bound or the words that say why it carries none. */
export interface Reading {
  count: number;
  total: number;
  share: number;
  /** The one-sided upper bound on the rate of the complement, or null when nothing bounds it. */
  bound: number | null;
  /** Why there is no bound, in the words the report prints. */
  noBound: string | null;
}

/** How `readingOf` is told what would stop it bounding a number. */
export interface ReadingOptions {
  confidence?: number;
  /** How many cases a bound needs, which is `MINIMUM_DECIDED` where the caller names none. */
  floor?: number;
  /** The words that replace every bound of an arm missing too many of its judgements. */
  missing?: string | null;
}

/** The modal answer of the judgements that returned, and why there is none where there is none. */
export interface ModalVerdict {
  /** The labels the mode chose, as a sorted set, or null where the case is undecided. */
  labels: string[] | null;
  returned: number;
  /** How many of the judgements that returned chose the mode. */
  count: number;
  /** count / returned, and 0 where the case is undecided. */
  confidence: number;
  /** Why the case is undecided, in the words the report prints. */
  reason: string | null;
}

/** One case as one arm judged it, which is the unit every number of the report is counted over. */
export interface CaseReading {
  caseId: string;
  slice: SliceName;
  kind: CaseKind;
  /** The corpus row, which the confound bins and the document grouping read. */
  row: CaseRow;
  /** How many rows this case and arm hold, faulted ones included. */
  rows: number;
  returned: number;
  faulted: number;
  verdict: string[] | null;
  reason: string | null;
  confidence: number;
  agreed: boolean;
  /** Whether the first judgement alone matched, and null where it did not return. */
  firstAgreed: boolean | null;
  /** Whether any judgement of this case and arm was answered over a cut state. */
  stateCut: boolean;
  /** The endpoint's own stated probability of what it chose, averaged, where it states one. */
  statedProbability: number | null;
}

/** One arm's rows read back: the cases it judged, and what its rows tallied. */
export interface ArmReading {
  name: string;
  recipe: string;
  model: string;
  endpoint: string;
  cases: CaseReading[];
  rows: number;
  answered: number;
  faulted: number;
  inputTokens: number;
  outputTokens: number;
  routes: Record<string, number>;
  excluded: number;
  stateCutRows: number;
  faultsByKind: Record<string, number>;
}

/** What the report prints for one arm over one kind of case. */
export interface KindAnalysis {
  kind: CaseKind;
  /** The report-slice cases of this kind the arm judged. */
  cases: number;
  /** How many cases the slice itself holds, where the draw was readable. */
  sliceCases: number | null;
  undecided: number;
  agreement: Reading;
  /** The threshold fitted on the fit slice, and what it was fitted over. */
  fit: {
    cases: number;
    decided: number;
    threshold: number | null;
    /** The lowest bound any confidence value of the fit slice reached, where none held. */
    lowestBound: number | null;
    reason: string | null;
  };
  /** That threshold applied to the report slice. */
  atThreshold: {
    decided: number;
    disagreements: number;
    coverage: number;
    coverageInterval: Interval | null;
    bound: number | null;
    noBound: string | null;
    /** The same bound read at 1 - 0.10 / 12, for the choice among twelve arms. */
    adjustedBound: number | null;
  };
  zeroBand: { threshold: number; decided: number; coverage: number; bound: number } | null;
  baseline: Baseline;
  paired: PairedComparison;
  anchor: Reading;
  /** The same agreement over the cases no arm had cut, which is the comparable one. */
  cutFree: { cases: number; agreement: Reading; differs: boolean };
  sensitivity: { modal: Reading; first: Reading; oneCasePerDocument: Reading };
}

/** Everything the report prints for one arm. */
export interface ArmAnalysis {
  arm: string;
  recipe: string;
  model: string;
  endpoint: string;
  rows: number;
  answered: number;
  faulted: number;
  planned: number;
  missingShare: number;
  /** False when the arm is missing more than 5 percent of its judgements. */
  bounded: boolean;
  /** The words that replace every bound of this arm, where it carries none. */
  noBound: string | null;
  casesBySlice: Record<string, Record<string, number>>;
  tokens: { input: number; output: number };
  cost: { total: number | null; perCase: number | null };
  retrieval: { routes: Record<string, number>; excluded: number };
  stateCut: { rows: number; cases: number };
  /** The distinct counted-confidence values this arm's cases took. */
  confidenceValues: number[];
  faultsByKind: Record<string, number>;
  approval: KindAnalysis;
  question: KindAnalysis;
}

/** One arm as the decision rule reads it. */
export interface ArmCoverage {
  arm: string;
  threshold: number | null;
  decided: number;
  total: number;
  coverage: number;
  interval: Interval | null;
  bound: number | null;
  /** The measured cost of one case, where a price is known for the arm's model. */
  costPerCase: number | null;
}

/** What the decision rule answered, with the rule as it was applied. */
export interface DecisionReading {
  winner: string | null;
  eligible: string[];
  /** The eligible arms whose coverage interval overlaps the leader's. */
  overlapping: string[];
  /** Whether the cost tie-break chose among overlapping arms. */
  byCost: boolean;
  /** Whether the winner's coverage is at or above the usefulness floor. */
  useful: boolean;
  lines: string[];
}

/** One confound and its bins, each printed under `binReport`'s floor rule. */
export interface ConfoundTable {
  confound: string;
  lines: BinLine[];
}

/** One class of the corpus: how often the person approved, and over how many cases. */
export interface ClassLine {
  dimension: string;
  className: string;
  /** Every case of the class. */
  cases: number;
  /** Those carrying an approve or request-changes verdict, which is what a share is read over. */
  counted: number;
  approvals: number;
  share: number;
  /** The one-sided upper bound on the share the person did not approve. */
  bound: number | null;
  noBound: string | null;
}

/** The confidence table of one generative arm, beside Jev's own probability on the same cases. */
export interface ConfidenceTable {
  arm: string | null;
  /** Why there is no table, where there is none. */
  reason: string | null;
  caseIds: string[];
  /** The seed the cases were picked under, and null where a file named them. */
  seed: number | null;
  approved: number;
  requestedChanges: number;
  lines: BinLine[];
  jevArm: string | null;
  jevLines: BinLine[];
  /** Arms whose counted confidence took one value, so no threshold selects anything in them. */
  identicalArms: string[];
}

/** The whole report, which is what `report.json` holds and what the text is rendered from. */
export interface Analysis {
  corpus: {
    directory: string;
    person: string;
    throughEventId: number;
    cases: number;
    cut: string | null;
  };
  round: {
    directory: string;
    /** What `round.json` says, where the round has written it. */
    summary: {
      startedAt: string;
      finishedAt: string;
      finished: boolean;
      planned: number;
      asked: number;
      answered: number;
      left: number;
    } | null;
    rowsRead: number;
    rowsUnreadable: number;
    rowsOfUnknownCase: number;
    judgementsPerCase: number;
    casesWithRows: number;
    casesBySlice: Record<string, Record<string, number>>;
    sliceSizes: Record<string, number> | null;
    arms: string[];
    models: string[];
    faultsByKind: Record<string, { count: number; example: string | null }>;
    faultsByArm: Record<string, { count: number; example: string | null }>;
    armsWithoutBound: string[];
  };
  arms: ArmAnalysis[];
  decision: { approval: DecisionReading; adjusted: number | null; pointwise: number | null };
  stateCut: {
    sentence: string;
    jevRowsCut: number;
    casesCut: number;
    cutFreeBySlice: Record<string, { cutFree: number; cases: number }>;
  };
  confounds: { arm: string; tables: ConfoundTable[] }[];
  classes: { whole: ClassLine[]; postCut: ClassLine[]; cut: string | null };
  confidenceTable: ConfidenceTable | null;
}

/** The labels of one answer as a set: deduplicated, sorted and joined, so two answers compare. */
export function labelKey(labels: readonly string[]): string {
  return [...new Set(labels)].sort().join("\u0000");
}

/**
 * The modal answer of the judgements that returned. A tie for the mode is undecided, and so is a
 * case fewer than three judgements returned for: both count against coverage, and neither is
 * ever a disagreement.
 */
export function modeOf(answers: readonly (readonly string[])[]): ModalVerdict {
  const returned = answers.length;
  if (returned < LEAST_RETURNED) {
    return {
      labels: null,
      returned,
      count: 0,
      confidence: 0,
      reason: `${returned} of its judgements returned, and a decided case needs ${LEAST_RETURNED}`,
    };
  }
  const counts = new Map<string, { labels: string[]; count: number }>();
  for (const answer of answers) {
    const key = labelKey(answer);
    const held = counts.get(key);
    if (held === undefined) counts.set(key, { labels: [...new Set(answer)].sort(), count: 1 });
    else held.count += 1;
  }
  const ordered = [...counts.values()].sort((one, two) => two.count - one.count);
  const best = ordered[0];
  if (best === undefined) {
    return { labels: null, returned, count: 0, confidence: 0, reason: "no judgement returned" };
  }
  const second = ordered[1];
  if (second !== undefined && second.count === best.count) {
    return {
      labels: null,
      returned,
      count: best.count,
      confidence: 0,
      reason: `the mode is tied at ${best.count} of ${returned}`,
    };
  }
  return {
    labels: best.labels,
    returned,
    count: best.count,
    confidence: best.count / returned,
    reason: null,
  };
}

/** The endpoint's own stated probability of what one judgement chose, where it states one. */
function statedProbabilityOf(row: JudgementRow): number | null {
  const label = row.verdict[0];
  const stated = label === undefined ? undefined : row.probabilities?.[label];
  if (typeof stated === "number" && Number.isFinite(stated)) return stated;
  return row.confidence;
}

/** One case as one arm judged it, from that arm's rows for it and the person's own answer. */
export function readCase(rows: readonly JudgementRow[], row: CaseRow): CaseReading {
  const first = rows[0];
  if (first === undefined) throw new RangeError(`no row was given for case ${row.caseId}`);
  const returned = rows.filter((one) => one.fault === null && one.verdict.length > 0);
  const mode = modeOf(returned.map((one) => one.verdict));
  const firstJudgement = returned.find((one) => one.judgement === 0);
  const stated = returned
    .map(statedProbabilityOf)
    .filter((one): one is number => one !== null && Number.isFinite(one));
  const answer = labelKey(row.selected);
  return {
    caseId: row.caseId,
    slice: first.slice,
    kind: first.kind,
    row,
    rows: rows.length,
    returned: returned.length,
    faulted: rows.filter((one) => one.fault !== null).length,
    verdict: mode.labels,
    reason: mode.reason,
    confidence: mode.confidence,
    agreed: mode.labels !== null && labelKey(mode.labels) === answer,
    firstAgreed: firstJudgement === undefined ? null : labelKey(firstJudgement.verdict) === answer,
    stateCut: rows.some((one) => one.stateCut),
    statedProbability:
      stated.length === 0 ? null : stated.reduce((one, two) => one + two, 0) / stated.length,
  };
}

/** Every arm's cases, read from the rows the round has written so far. */
export function readRound(
  rows: readonly JudgementRow[],
  corpus: ReadonlyMap<string, CaseRow>
): { arms: ArmReading[]; unknownCases: number } {
  const byArm = new Map<string, Map<string, Map<number, JudgementRow>>>();
  let unknownCases = 0;
  for (const row of rows) {
    if (!corpus.has(row.caseId)) {
      unknownCases += 1;
      continue;
    }
    let cases = byArm.get(row.arm);
    if (cases === undefined) {
      cases = new Map();
      byArm.set(row.arm, cases);
    }
    let judgements = cases.get(row.caseId);
    if (judgements === undefined) {
      judgements = new Map();
      cases.set(row.caseId, judgements);
    }
    // One row per (arm, case, judgement), the last written winning, as the row store holds them.
    judgements.set(row.judgement, row);
  }
  const arms: ArmReading[] = [];
  for (const name of [...byArm.keys()].sort()) {
    const cases = byArm.get(name) ?? new Map<string, Map<number, JudgementRow>>();
    const armRows: JudgementRow[] = [];
    const readings: CaseReading[] = [];
    for (const [caseId, judgements] of cases) {
      const ofCase = [...judgements.values()].sort((one, two) => one.judgement - two.judgement);
      armRows.push(...ofCase);
      const row = corpus.get(caseId);
      if (row !== undefined) readings.push(readCase(ofCase, row));
    }
    readings.sort((one, two) => one.caseId.localeCompare(two.caseId));
    const head = armRows[0];
    const routes: Record<string, number> = {};
    const faultsByKind: Record<string, number> = {};
    let inputTokens = 0;
    let outputTokens = 0;
    let excluded = 0;
    let answered = 0;
    let faulted = 0;
    let stateCutRows = 0;
    for (const row of armRows) {
      inputTokens += row.inputTokens ?? 0;
      outputTokens += row.outputTokens ?? 0;
      excluded += row.excluded ?? 0;
      if (row.retrievalRoute !== null)
        routes[row.retrievalRoute] = (routes[row.retrievalRoute] ?? 0) + 1;
      if (row.fault === null) answered += 1;
      else {
        faulted += 1;
        faultsByKind[row.fault] = (faultsByKind[row.fault] ?? 0) + 1;
      }
      if (row.stateCut) stateCutRows += 1;
    }
    arms.push({
      name,
      recipe: head?.recipe ?? "",
      model: head?.model ?? "",
      endpoint: head?.endpoint ?? "",
      cases: readings,
      rows: armRows.length,
      answered,
      faulted,
      inputTokens,
      outputTokens,
      routes,
      excluded,
      stateCutRows,
      faultsByKind,
    });
  }
  return { arms, unknownCases };
}

/** A share with its count, bounded unless something named here stops it being bounded. */
export function readingOf(count: number, total: number, options: ReadingOptions = {}): Reading {
  const { confidence = DEFAULT_CONFIDENCE, floor = MINIMUM_DECIDED, missing = null } = options;
  const share = total === 0 ? 0 : count / total;
  if (total === 0) return { count, total, share, bound: null, noBound: "no case at this count" };
  if (missing !== null && missing !== "") {
    return { count, total, share, bound: null, noBound: missing };
  }
  if (total < floor) {
    return {
      count,
      total,
      share,
      bound: null,
      noBound:
        `no bound at this count (${total} case${total === 1 ? "" : "s"}, and a bound needs ` +
        `${floor})`,
    };
  }
  return {
    count,
    total,
    share,
    bound: clopperPearsonUpper(total - count, total, confidence),
    noBound: null,
  };
}

/** The decided cases of a set, as the arithmetic of `stats.ts` reads them. */
function judgementsOf(cases: readonly CaseReading[]): Judgement[] {
  return cases
    .filter((one) => one.verdict !== null)
    .map((one) => ({ caseId: one.caseId, confidence: one.confidence, agreed: one.agreed }));
}

/** The cases of one slice and one kind, in the order the arm judged them. */
function sliceCases(
  cases: readonly CaseReading[],
  slice: SliceName,
  kind: CaseKind
): CaseReading[] {
  return cases.filter((one) => one.slice === slice && one.kind === kind);
}

/** One case per lead document, which is the reading free of the within-document correlation. */
function oneCasePerDocument(cases: readonly CaseReading[]): CaseReading[] {
  const seen = new Set<string>();
  const kept: CaseReading[] = [];
  for (const one of cases) {
    const lead = leadDocumentOf(one.row);
    const key = lead === null ? `case:${one.caseId}` : `document:${lead.artifactId}`;
    if (seen.has(key)) continue;
    seen.add(key);
    kept.push(one);
  }
  return kept;
}

/** Whether two intervals overlap, which is what the winner's tie-break turns on. */
function overlaps(one: Interval | null, two: Interval | null): boolean {
  if (one === null || two === null) return false;
  return one.lower <= two.upper && two.lower <= one.upper;
}

/**
 * The winner, by the rule as the plan writes it: the highest coverage on the approval report
 * slice at the arm's own fitted threshold, among the arms whose report-slice bound is still at
 * or under the rate; where two coverage intervals overlap, the lower measured cost per case.
 */
export function chooseWinner(
  arms: readonly ArmCoverage[],
  rate: number = RATE,
  floor: number = USEFUL_COVERAGE
): DecisionReading {
  const lines: string[] = [];
  const eligible = arms.filter(
    (one) => one.threshold !== null && one.bound !== null && one.bound <= rate
  );
  lines.push(
    `${eligible.length} of ${arms.length} arms fitted a threshold on the fit slice and still ` +
      `hold a report-slice bound at or under ${rate.toFixed(2)}.`
  );
  let leader = eligible[0];
  if (leader === undefined) {
    lines.push("The rule names no winner: no arm decides anything at the rate it is held to.");
    return { winner: null, eligible: [], overlapping: [], byCost: false, useful: false, lines };
  }
  for (const one of eligible) if (one.coverage > leader.coverage) leader = one;
  lines.push(
    `The highest coverage among them is ${leader.arm} at ${leader.coverage.toFixed(3)} of ` +
      `${leader.total} approval report cases.`
  );
  const overlapping = eligible.filter(
    (one) => one.arm !== leader.arm && overlaps(one.interval, leader.interval)
  );
  let byCost = false;
  if (overlapping.length > 0) {
    const group = [leader, ...overlapping];
    const priced = group.filter((one): one is ArmCoverage & { costPerCase: number } => {
      return one.costPerCase !== null;
    });
    const cheapest = priced.reduce<(ArmCoverage & { costPerCase: number }) | null>(
      (one, two) => (one === null || two.costPerCase < one.costPerCase ? two : one),
      null
    );
    if (cheapest === null) {
      lines.push(
        `The coverage intervals of ${group.map((one) => one.arm).join(", ")} overlap, and no ` +
          "price is known for any of them, so the winner stays the arm with the highest coverage."
      );
    } else {
      byCost = true;
      leader = cheapest;
      lines.push(
        `The coverage intervals of ${group.map((one) => one.arm).join(", ")} overlap, so the ` +
          `winner is the one with the lower measured cost per case, which is ${cheapest.arm} at ` +
          `${cheapest.costPerCase.toFixed(5)} dollars per case.`
      );
    }
  }
  const useful = leader.coverage >= floor;
  lines.push(
    useful
      ? `${leader.arm} covers ${leader.coverage.toFixed(3)}, at or above the usefulness floor of ` +
          `${floor.toFixed(2)}, so the rule names it the winner.`
      : `${leader.arm} covers ${leader.coverage.toFixed(3)}, under the usefulness floor of ` +
          `${floor.toFixed(2)}: the skill records the curve and no verdict rule, and the curve ` +
          "goes to the person who decides."
  );
  return {
    winner: leader.arm,
    eligible: eligible.map((one) => one.arm),
    overlapping: overlapping.map((one) => one.arm),
    byCost,
    useful,
    lines,
  };
}

/** One coarse split of the plan's confound table: the bins it declares, and which one a case is. */
interface Split {
  confound: string;
  bins: readonly string[];
  binOf(row: CaseRow): string;
}

/** The six splits of the plan's confound table, each sized so both sides can carry a bound. */
const SPLITS: readonly Split[] = [
  {
    confound: "the version answered at",
    bins: ["version 1 to 9", "version 10 and above", "no version"],
    binOf: (row) => {
      const version = row.confounds.version;
      if (version === null) return "no version";
      return version <= 9 ? "version 1 to 9" : "version 10 and above";
    },
  },
  {
    confound: "answers that day before this one",
    bins: [`${ANSWERS_MEDIAN} or fewer`, `more than ${ANSWERS_MEDIAN}`],
    binOf: (row) =>
      row.confounds.answersThatDayBefore <= ANSWERS_MEDIAN
        ? `${ANSWERS_MEDIAN} or fewer`
        : `more than ${ANSWERS_MEDIAN}`,
  },
  {
    confound: "the hour",
    bins: ["15:00 to 23:59 UTC", "00:00 to 14:59 UTC"],
    binOf: (row) =>
      row.confounds.hourUtc >= AFTERNOON_START_UTC ? "15:00 to 23:59 UTC" : "00:00 to 14:59 UTC",
  },
  {
    confound: "the project",
    bins: [MAIN_PROJECT, "every other project"],
    binOf: (row) => (row.project === MAIN_PROJECT ? MAIN_PROJECT : "every other project"),
  },
  {
    confound: "changes already requested on that document",
    bins: ["yes", "no"],
    binOf: (row) => (row.confounds.changesAlreadyRequested ? "yes" : "no"),
  },
  {
    confound: "the kind of case",
    bins: ["approval", "question"],
    binOf: (row) => row.kind,
  },
];

/** The bins of one split over the decided cases, in the order the split declares them. */
function binsOf(cases: readonly CaseReading[], split: Split): Bin[] {
  const decided = cases.filter((one) => one.verdict !== null);
  return split.bins.map((name) => ({
    name,
    cases: judgementsOf(decided.filter((one) => split.binOf(one.row) === name)),
  }));
}

/** The bins of one finer breakdown, which are whichever values the cases themselves took. */
function finerBins(cases: readonly CaseReading[], nameOf: (row: CaseRow) => string): Bin[] {
  const decided = cases.filter((one) => one.verdict !== null);
  const names = [...new Set(decided.map((one) => nameOf(one.row)))].sort();
  return names.map((name) => ({
    name,
    cases: judgementsOf(decided.filter((one) => nameOf(one.row) === name)),
  }));
}

/**
 * Every confound of one arm, read over every scored case of it and not over the report slice
 * alone, each bin printed under the floor rule: under 45 decided cases, the count and the
 * observed agreement and no bound.
 */
export function confoundTables(cases: readonly CaseReading[]): ConfoundTable[] {
  const tables: ConfoundTable[] = SPLITS.map((split) => ({
    confound: split.confound,
    lines: binReport(binsOf(cases, split)),
  }));
  tables.push({
    confound: "the hour, finer",
    lines: binReport(
      finerBins(cases, (row) => `${String(row.confounds.hourUtc).padStart(2, "0")}:00 UTC`)
    ),
  });
  tables.push({
    confound: "the project, finer",
    lines: binReport(finerBins(cases, (row) => row.project ?? "no project")),
  });
  return tables;
}

/** One line of the class table over one class of the corpus's own rows. */
function classLineOf(
  dimension: string,
  className: string,
  rows: readonly CaseRow[],
  floor: number
): ClassLine {
  const decided = rows.filter((row) => row.verdict !== null);
  const baseline = alwaysApproveBaseline(decided);
  const reading = readingOf(baseline.approvals, baseline.counted, { floor });
  return {
    dimension,
    className,
    cases: rows.length,
    counted: baseline.counted,
    approvals: baseline.approvals,
    share: baseline.share,
    bound: reading.bound,
    noBound: reading.noBound,
  };
}

/**
 * The class table: for each class the confounds already carry, the share of those cases the
 * person approved and the count. It is a pass over the corpus's own rows and reads no judgement
 * of any arm, so it says which classes the person answered the same way every time.
 */
export function classTable(rows: readonly CaseRow[], floor: number = MINIMUM_DECIDED): ClassLine[] {
  const lines: ClassLine[] = [];
  const projects = [...new Set(rows.map((row) => row.project ?? "no project"))].sort();
  for (const project of projects) {
    const of = rows.filter((row) => (row.project ?? "no project") === project);
    lines.push(classLineOf("by project", project, of, floor));
  }
  for (const kind of ["approval", "question"] as const) {
    lines.push(
      classLineOf(
        "by kind",
        kind,
        rows.filter((row) => row.kind === kind),
        floor
      )
    );
  }
  for (const already of [true, false]) {
    lines.push(
      classLineOf(
        "by changes already requested on that document",
        already ? "yes" : "no",
        rows.filter((row) => row.confounds.changesAlreadyRequested === already),
        floor
      )
    );
  }
  return lines;
}

/** What an arm's judgements cost, by the round's own token counts and the price of its model. */
function costOf(arm: ArmReading, cases: number): { total: number | null; perCase: number | null } {
  const price = PRICES[arm.model];
  if (price === undefined) return { total: null, perCase: null };
  const total =
    (arm.inputTokens * price.input) / PER_MILLION + (arm.outputTokens * price.output) / PER_MILLION;
  return { total, perCase: cases === 0 ? null : total / cases };
}

/** One kind of case of one arm, over the report slice, with the fit slice behind its threshold. */
function kindAnalysisOf(
  arm: ArmReading,
  kind: CaseKind,
  cutFree: ReadonlySet<string>,
  sliceSizes: Record<string, number> | null,
  missing: string | null
): KindAnalysis {
  const report = sliceCases(arm.cases, "report", kind);
  const fit = sliceCases(arm.cases, "fit", kind);
  const anchor = sliceCases(arm.cases, "anchor", kind);
  const reportJudged = judgementsOf(report);
  const fitJudged = judgementsOf(fit);
  const agreed = report.filter((one) => one.agreed).length;
  const agreement = readingOf(agreed, report.length, { missing });
  const fitCurve = riskCoverageCurve(fitJudged);
  const choice = chooseThreshold(fitJudged, { rate: RATE });
  const threshold = choice.chosen?.threshold ?? null;
  const decided = threshold === null ? [] : decidedAt(reportJudged, threshold);
  const disagreements = decided.filter((one) => !one.agreed).length;
  const coverage = report.length === 0 ? 0 : decided.length / report.length;
  const bounded = missing === null && decided.length >= MINIMUM_DECIDED;
  const band = zeroDisagreementBand(reportJudged);
  const baseline = alwaysApproveBaseline(report.map((one) => ({ verdict: one.row.verdict })));
  const paired: PairedCase[] = report.map((one) => ({
    armAgreed: one.agreed,
    otherAgreed: one.row.verdict === "approve",
  }));
  const cutFreeCases = report.filter((one) => cutFree.has(one.caseId));
  const cutFreeAgreement = readingOf(
    cutFreeCases.filter((one) => one.agreed).length,
    cutFreeCases.length,
    { missing }
  );
  const interval =
    report.length === 0 ? null : clopperPearsonInterval(agreed, report.length, DEFAULT_CONFIDENCE);
  const differs =
    interval !== null &&
    cutFreeCases.length > 0 &&
    Math.abs(cutFreeAgreement.share - agreement.share) > (interval.upper - interval.lower) / 2;
  const firstReturned = report.filter((one) => one.firstAgreed !== null);
  const perDocument = oneCasePerDocument(report);
  return {
    kind,
    cases: report.length,
    sliceCases: sliceSizes?.report ?? null,
    undecided: report.filter((one) => one.verdict === null).length,
    agreement,
    fit: {
      cases: fit.length,
      decided: fitJudged.length,
      threshold,
      lowestBound:
        threshold !== null || fitCurve.length === 0
          ? null
          : Math.min(...fitCurve.map((point) => point.upperBound)),
      reason: choice.reason,
    },
    atThreshold: {
      decided: decided.length,
      disagreements,
      coverage,
      coverageInterval:
        report.length === 0
          ? null
          : clopperPearsonInterval(decided.length, report.length, DEFAULT_CONFIDENCE),
      bound: bounded
        ? clopperPearsonUpper(disagreements, decided.length, DEFAULT_CONFIDENCE)
        : null,
      noBound: bounded
        ? null
        : (missing ??
          `no bound at this count (${decided.length} decided, and a bound needs ` +
            `${MINIMUM_DECIDED})`),
      adjustedBound:
        bounded && decided.length > 0
          ? clopperPearsonUpper(disagreements, decided.length, ADJUSTED_CONFIDENCE)
          : null,
    },
    zeroBand:
      band === null
        ? null
        : {
            threshold: band.threshold,
            decided: band.decided,
            coverage: report.length === 0 ? 0 : band.decided / report.length,
            bound: band.upperBound,
          },
    baseline,
    paired: mcnemarExact(paired),
    anchor: readingOf(anchor.filter((one) => one.agreed).length, anchor.length, { missing }),
    cutFree: { cases: cutFreeCases.length, agreement: cutFreeAgreement, differs },
    sensitivity: {
      modal: agreement,
      first: readingOf(
        firstReturned.filter((one) => one.firstAgreed === true).length,
        firstReturned.length,
        { missing }
      ),
      oneCasePerDocument: readingOf(
        perDocument.filter((one) => one.agreed).length,
        perDocument.length,
        { missing }
      ),
    },
  };
}

/** One arm, read whole: its counts, its spend, and its numbers over each kind of case. */
function armAnalysisOf(
  arm: ArmReading,
  planned: number,
  cutFree: ReadonlySet<string>,
  sliceSizes: Record<string, number> | null
): ArmAnalysis {
  const missingShare = planned === 0 ? 1 : Math.max(0, (planned - arm.answered) / planned);
  const bounded = missingShare <= MOST_MISSING;
  const noBound = bounded
    ? null
    : `no bound: this arm is missing ${(100 * missingShare).toFixed(1)} percent of its ` +
      `judgements (${count(arm.answered)} of ${count(planned)} answered), and the plan gives ` +
      "no bound past " +
      `${(100 * MOST_MISSING).toFixed(0)} percent`;
  const casesBySlice: Record<string, Record<string, number>> = {};
  for (const one of arm.cases) {
    const bySlice = casesBySlice[one.slice] ?? {};
    casesBySlice[one.slice] = bySlice;
    bySlice[one.kind] = (bySlice[one.kind] ?? 0) + 1;
  }
  return {
    arm: arm.name,
    recipe: arm.recipe,
    model: arm.model,
    endpoint: arm.endpoint,
    rows: arm.rows,
    answered: arm.answered,
    faulted: arm.faulted,
    planned,
    missingShare,
    bounded,
    noBound,
    casesBySlice,
    tokens: { input: arm.inputTokens, output: arm.outputTokens },
    cost: costOf(arm, arm.cases.length),
    retrieval: { routes: arm.routes, excluded: arm.excluded },
    stateCut: { rows: arm.stateCutRows, cases: arm.cases.filter((one) => one.stateCut).length },
    confidenceValues: [
      ...new Set(arm.cases.filter((one) => one.verdict !== null).map((one) => one.confidence)),
    ].sort((one, two) => one - two),
    faultsByKind: arm.faultsByKind,
    approval: kindAnalysisOf(arm, "approval", cutFree, sliceSizes, noBound),
    question: kindAnalysisOf(arm, "question", cutFree, sliceSizes, noBound),
  };
}

/** The cases no arm read over a cut state, which are the only ones the arms are comparable on. */
function cutFreeCasesOf(arms: readonly ArmReading[]): Set<string> {
  const cut = new Set<string>();
  const seen = new Set<string>();
  for (const arm of arms) {
    for (const one of arm.cases) {
      seen.add(one.caseId);
      if (one.stateCut) cut.add(one.caseId);
    }
  }
  for (const caseId of cut) seen.delete(caseId);
  return seen;
}

/** The faults of the round by kind and by arm, each with the first detail one of them carried. */
function faultsOf(rows: readonly JudgementRow[]): {
  byKind: Record<string, { count: number; example: string | null }>;
  byArm: Record<string, { count: number; example: string | null }>;
} {
  const byKind: Record<string, { count: number; example: string | null }> = {};
  const byArm: Record<string, { count: number; example: string | null }> = {};
  for (const row of rows) {
    if (row.fault === null) continue;
    const kind = byKind[row.fault] ?? { count: 0, example: null };
    byKind[row.fault] = kind;
    kind.count += 1;
    kind.example ??= row.faultDetail;
    const arm = byArm[row.arm] ?? { count: 0, example: null };
    byArm[row.arm] = arm;
    arm.count += 1;
    arm.example ??= row.faultDetail;
  }
  return { byKind, byArm };
}

/** `wanted` of `items`, drawn one at a time under `seed` and never one of them twice. */
function pickSome<T>(items: readonly T[], wanted: number, seed: number): T[] {
  const left = [...items];
  const picked: T[] = [];
  for (let index = 0; index < wanted && left.length > 0; index += 1) {
    const one = pickAtRandom(left, seed + index);
    if (one === undefined) break;
    picked.push(one);
    left.splice(left.indexOf(one), 1);
  }
  return picked;
}

/** The arm the confidence table is read over: the generative arm holding the most judged cases. */
function generativeArm(arms: readonly ArmReading[]): ArmReading | null {
  const generative = arms.filter((arm) => arm.endpoint === "claude");
  return generative.reduce<ArmReading | null>(
    (one, two) => (one === null || two.cases.length > one.cases.length ? two : one),
    null
  );
}

/** The Jev arm its own probability is read from: the Jev arm holding the most judged cases. */
function jevArm(arms: readonly ArmReading[]): ArmReading | null {
  return arms
    .filter((arm) => arm.endpoint === "jev")
    .reduce<ArmReading | null>(
      (one, two) => (one === null || two.cases.length > one.cases.length ? two : one),
      null
    );
}

/** Jev's own stated probability on one case, binned, since it takes a value of its own per case. */
function probabilityBin(value: number): string {
  const bin = Math.round(value / PROBABILITY_BIN) * PROBABILITY_BIN;
  return bin.toFixed(1);
}

/**
 * The confidence table: one generative arm's counted confidence over cases the person decided
 * both ways, against the cases at each value and the disagreements at it, and beside it Jev's
 * own stated probability over the same cases, read as its own arm and never mixed in.
 */
export function confidenceTableOf(
  arms: readonly ArmReading[],
  wanted: readonly string[] | null,
  seed: number | null
): ConfidenceTable {
  const identicalArms = arms
    .filter((arm) => {
      const values = new Set(
        arm.cases.filter((one) => one.verdict !== null).map((one) => one.confidence)
      );
      return arm.cases.length > 0 && values.size <= 1;
    })
    .map((arm) => arm.name);
  const arm = generativeArm(arms);
  if (arm === null) {
    return {
      arm: null,
      reason: "no generative arm has written a row yet, so no counted confidence can be read",
      caseIds: [],
      seed,
      approved: 0,
      requestedChanges: 0,
      lines: [],
      jevArm: null,
      jevLines: [],
      identicalArms,
    };
  }
  const report = sliceCases(arm.cases, "report", "approval");
  let chosen: CaseReading[];
  if (wanted !== null) {
    const byId = new Map(report.map((one) => [one.caseId, one]));
    chosen = wanted
      .map((caseId) => byId.get(caseId))
      .filter((one): one is CaseReading => one !== undefined);
  } else {
    const half = CONFIDENCE_TABLE_CASES / 2;
    const approve = report.filter((one) => one.row.verdict === "approve").map((one) => one.caseId);
    const changes = report
      .filter((one) => one.row.verdict === "request_changes")
      .map((one) => one.caseId);
    const picked = new Set([
      ...pickSome(approve, half, seed ?? 0),
      ...pickSome(changes, half, (seed ?? 0) + half),
    ]);
    chosen = report.filter((one) => picked.has(one.caseId));
  }
  const jev = jevArm(arms);
  const jevCases =
    jev === null
      ? []
      : jev.cases.filter(
          (one) =>
            chosen.some((other) => other.caseId === one.caseId) && one.statedProbability !== null
        );
  const jevBins = [
    ...new Set(jevCases.map((one) => probabilityBin(one.statedProbability ?? 0))),
  ].sort();
  return {
    arm: arm.name,
    reason:
      chosen.length < CONFIDENCE_TABLE_CASES
        ? `only ${chosen.length} of the ${CONFIDENCE_TABLE_CASES} cases this table wants are ` +
          `scored by ${arm.name} so far, so the table is read over those`
        : null,
    caseIds: chosen.map((one) => one.caseId),
    seed: wanted === null ? seed : null,
    approved: chosen.filter((one) => one.row.verdict === "approve").length,
    requestedChanges: chosen.filter((one) => one.row.verdict === "request_changes").length,
    lines: binReport(
      [...new Set(chosen.filter((one) => one.verdict !== null).map((one) => one.confidence))]
        .sort((one, two) => one - two)
        .map((value) => ({
          name: value.toFixed(2),
          cases: judgementsOf(chosen.filter((one) => one.confidence === value)),
        }))
    ),
    jevArm: jev?.name ?? null,
    jevLines: binReport(
      jevBins.map((bin) => ({
        name: bin,
        cases: judgementsOf(
          jevCases.filter((one) => probabilityBin(one.statedProbability ?? 0) === bin)
        ),
      }))
    ),
    identicalArms,
  };
}

/** What `analyseRound` is given: everything read off a disk, and nothing read by it. */
export interface AnalysisInput {
  corpus: Corpus;
  roundDirectory: string;
  rows: readonly JudgementRow[];
  rowsRead: number;
  rowsUnreadable: number;
  summary: RoundSummary | null;
  sliceSizes: Record<string, number> | null;
  cut: string | null;
  confidenceTable: boolean;
  wantedCases: readonly string[] | null;
  seed: number | null;
}

/** The whole report, computed from the rows and the corpus and reading nothing else. */
export function analyseRound(input: AnalysisInput): Analysis {
  const { arms: armReadings, unknownCases } = readRound(input.rows, input.corpus.byCaseId);
  const cutFree = cutFreeCasesOf(armReadings);
  const casesWithRows = new Set(input.rows.map((row) => row.caseId));
  const judgementsPerCase =
    input.summary !== null && input.summary.cases.total > 0 && input.summary.arms.length > 0
      ? Math.max(
          1,
          Math.round(
            input.summary.judgements.planned /
              (input.summary.cases.total * input.summary.arms.length)
          )
        )
      : Math.max(DEFAULT_JUDGEMENTS, ...input.rows.map((row) => row.judgement + 1));
  const casesPlanned =
    input.summary?.cases.total ??
    (input.sliceSizes === null
      ? casesWithRows.size
      : (input.sliceSizes.fit ?? 0) +
        (input.sliceSizes.report ?? 0) +
        (input.sliceSizes.anchor ?? 0));
  const planned = casesPlanned * judgementsPerCase;
  const faults = faultsOf(input.rows);
  const arms = armReadings.map((arm) => armAnalysisOf(arm, planned, cutFree, input.sliceSizes));
  const sliceOfCase = new Map<string, string>();
  for (const row of input.rows)
    if (!sliceOfCase.has(row.caseId)) sliceOfCase.set(row.caseId, row.slice);
  const casesBySlice: Record<string, Record<string, number>> = {};
  const cutFreeBySlice: Record<string, { cutFree: number; cases: number }> = {};
  for (const [caseId, slice] of sliceOfCase) {
    const counts = cutFreeBySlice[slice] ?? { cutFree: 0, cases: 0 };
    cutFreeBySlice[slice] = counts;
    counts.cases += 1;
    if (cutFree.has(caseId)) counts.cutFree += 1;
    const row = input.corpus.byCaseId.get(caseId);
    if (row === undefined) continue;
    const byKind = casesBySlice[slice] ?? {};
    casesBySlice[slice] = byKind;
    byKind[row.kind] = (byKind[row.kind] ?? 0) + 1;
  }
  const coverage: ArmCoverage[] = arms.map((arm) => ({
    arm: arm.arm,
    threshold: arm.approval.fit.threshold,
    decided: arm.approval.atThreshold.decided,
    total: arm.approval.cases,
    coverage: arm.approval.atThreshold.coverage,
    interval: arm.approval.atThreshold.coverageInterval,
    bound: arm.approval.atThreshold.bound,
    costPerCase: arm.cost.perCase,
  }));
  const decision = chooseWinner(coverage);
  const winner = arms.find((arm) => arm.arm === decision.winner) ?? null;
  const postCut =
    input.cut === null
      ? []
      : input.corpus.rows.filter(
          (row) => Date.parse(row.answeredAt) >= Date.parse(input.cut ?? "")
        );
  return {
    corpus: {
      directory: input.corpus.directory,
      person: input.corpus.snapshot.person,
      throughEventId: input.corpus.snapshot.throughEventId,
      cases: input.corpus.rows.length,
      cut: input.cut,
    },
    round: {
      directory: input.roundDirectory,
      summary:
        input.summary === null
          ? null
          : {
              startedAt: input.summary.startedAt,
              finishedAt: input.summary.finishedAt,
              finished: input.summary.finished,
              planned: input.summary.judgements.planned,
              asked: input.summary.judgements.asked,
              answered: input.summary.judgements.answered,
              left: input.summary.judgements.left,
            },
      rowsRead: input.rowsRead,
      rowsUnreadable: input.rowsUnreadable,
      rowsOfUnknownCase: unknownCases,
      judgementsPerCase,
      casesWithRows: casesWithRows.size,
      casesBySlice,
      sliceSizes: input.sliceSizes,
      arms: arms.map((arm) => arm.arm),
      models: [...new Set(arms.map((arm) => arm.model))].sort(),
      faultsByKind: faults.byKind,
      faultsByArm: faults.byArm,
      armsWithoutBound: arms.filter((arm) => !arm.bounded).map((arm) => arm.arm),
    },
    arms,
    decision: {
      approval: decision,
      pointwise: winner?.approval.atThreshold.bound ?? null,
      adjusted: winner?.approval.atThreshold.adjustedBound ?? null,
    },
    stateCut: {
      sentence: CUT_SENTENCE,
      jevRowsCut: input.rows.filter((row) => row.endpoint === "jev" && row.stateCut).length,
      casesCut: new Set(input.rows.filter((row) => row.stateCut).map((row) => row.caseId)).size,
      cutFreeBySlice,
    },
    confounds: armReadings.map((arm) => ({
      arm: arm.name,
      tables: confoundTables(arm.cases),
    })),
    classes: {
      whole: classTable(input.corpus.rows),
      postCut: classTable(postCut),
      cut: input.cut,
    },
    confidenceTable: input.confidenceTable
      ? confidenceTableOf(armReadings, input.wantedCases, input.seed)
      : null,
  };
}

/** A count, as every printed count of the report is written. */
function count(value: number): string {
  return value.toLocaleString("en-US");
}

/** A share, to three places, which is as far as any count here carries. */
function num(value: number, places = 3): string {
  return value.toFixed(places);
}

/** A reading as one cell: the share, its count, and the bound or the words that replace it. */
function say(reading: Reading): string {
  if (reading.total === 0) return reading.noBound ?? "no case at this count";
  const share = `${num(reading.share)} (${count(reading.count)} of ${count(reading.total)})`;
  if (reading.bound === null) return `${share}, ${reading.noBound}`;
  return `${share}, disagreement at most ${num(reading.bound)}`;
}

/** A reading as two cells, for a table that keeps its bound in a column of its own. */
function cells(reading: Reading): [string, string] {
  if (reading.total === 0) return ["no case", reading.noBound ?? "no case at this count"];
  return [
    `${num(reading.share)} (${count(reading.count)} of ${count(reading.total)})`,
    reading.bound === null ? (reading.noBound ?? "no bound") : `at most ${num(reading.bound)}`,
  ];
}

/** One markdown table, its columns padded so the text reads as a table too. */
function table(headers: readonly string[], rows: readonly (readonly string[])[]): string[] {
  const widths = headers.map((header, index) =>
    Math.max(header.length, ...rows.map((row) => (row[index] ?? "").length))
  );
  const line = (row: readonly string[]): string =>
    `| ${widths.map((width, index) => (row[index] ?? "").padEnd(width)).join(" | ")} |`;
  return [
    line(headers),
    `| ${widths.map((width) => "-".repeat(width)).join(" | ")} |`,
    ...rows.map(line),
  ];
}

/** The head of the report: what was read, and how far the round had got when it was read. */
function headLines(analysis: Analysis): string[] {
  const { corpus, round } = analysis;
  const lines = [
    "# The approval agreement round, read",
    "",
    `Corpus: \`${corpus.directory}\`, ${count(corpus.cases)} cases of one person's answers, ` +
      `read through event ${count(corpus.throughEventId)}.`,
    `Round: \`${round.directory}\`, ${count(round.rowsRead)} rows read` +
      (round.rowsUnreadable > 0
        ? `, ${count(round.rowsUnreadable)} lines unreadable and skipped`
        : "") +
      (round.rowsOfUnknownCase > 0
        ? `, ${count(round.rowsOfUnknownCase)} rows naming a case the corpus does not hold`
        : "") +
      ".",
    "",
  ];
  if (round.summary === null) {
    lines.push(
      "`round.json` is not written yet, so the round has not finished. Every count below is " +
        "over the rows the round has written so far, and an arm missing judgements carries no " +
        "bound, which each of its numbers says in place of one."
    );
  } else {
    lines.push(
      `The round ran from ${round.summary.startedAt} to ${round.summary.finishedAt} and ` +
        `${round.summary.finished ? "finished" : "did not finish"}: ` +
        `${count(round.summary.answered)} of ${count(round.summary.planned)} judgements were ` +
        `answered and ${count(round.summary.left)} were left when it last wrote round.json, ` +
        "which a running round writes at its probe and again when it stops."
    );
  }
  lines.push("");
  lines.push("## The round's counts");
  lines.push("");
  lines.push(
    `${count(round.casesWithRows)} cases carry a row, at ${round.judgementsPerCase} judgements ` +
      `per case and arm. Arms with rows: ${round.arms.length === 0 ? "none" : round.arms.join(", ")}.` +
      ` Models: ${round.models.length === 0 ? "none" : round.models.join(", ")}.`
  );
  lines.push("");
  const slices = Object.keys(round.casesBySlice).sort();
  lines.push(
    ...table(
      ["slice", "approval cases", "question cases", "cases of the slice"],
      slices.map((slice) => [
        slice,
        count(round.casesBySlice[slice]?.approval ?? 0),
        count(round.casesBySlice[slice]?.question ?? 0),
        round.sliceSizes === null ? "not read" : count(round.sliceSizes[slice] ?? 0),
      ])
    )
  );
  const answered = analysis.arms.reduce((sum, arm) => sum + arm.answered, 0);
  const faulted = analysis.arms.reduce((sum, arm) => sum + arm.faulted, 0);
  const planned = analysis.arms.reduce((sum, arm) => sum + arm.planned, 0);
  lines.push("");
  lines.push(
    `The rows hold ${count(answered + faulted)} judgements asked, of which ` +
      `${count(answered)} answered and ${count(faulted)} faulted, against ${count(planned)} ` +
      `planned over these arms.`
  );
  lines.push("");
  if (round.armsWithoutBound.length > 0) {
    lines.push(
      `${round.armsWithoutBound.length} arms are missing more than ` +
        `${num(100 * MOST_MISSING, 0)} percent of their judgements and so carry no bound: ` +
        `${round.armsWithoutBound.join(", ")}.`
    );
  } else {
    lines.push("No arm is missing more than 5 percent of its judgements.");
  }
  return lines;
}

/** The faults, the tokens and the measured cost, and the retrieval route of each arm. */
function spendLines(analysis: Analysis): string[] {
  const lines = ["", "## Faults, tokens, cost and retrieval", ""];
  const kinds = Object.entries(analysis.round.faultsByKind);
  if (kinds.length === 0) lines.push("No judgement faulted.");
  else {
    lines.push(
      ...table(
        ["fault", "count", "one example of what it said"],
        kinds.map(([kind, fault]) => [
          kind,
          count(fault.count),
          (fault.example ?? "no detail").slice(0, 120),
        ])
      )
    );
    lines.push("");
    lines.push(
      ...table(
        ["arm", "faults", "one example of what it said"],
        Object.entries(analysis.round.faultsByArm).map(([arm, fault]) => [
          arm,
          count(fault.count),
          (fault.example ?? "no detail").slice(0, 120),
        ])
      )
    );
  }
  lines.push("");
  lines.push(
    ...table(
      [
        "arm",
        "model",
        "answered",
        "faulted",
        "input tokens",
        "output tokens",
        "cost",
        "cost per case",
        "retrieval",
        "excluded",
      ],
      analysis.arms.map((arm) => [
        arm.arm,
        arm.model,
        count(arm.answered),
        count(arm.faulted),
        count(arm.tokens.input),
        count(arm.tokens.output),
        arm.cost.total === null ? "no price known" : `${num(arm.cost.total, 4)} dollars`,
        arm.cost.perCase === null ? "no price known" : num(arm.cost.perCase, 5),
        Object.entries(arm.retrieval.routes)
          .map(([route, howMany]) => `${route} ${count(howMany)}`)
          .join(", ") || "no examples",
        count(arm.retrieval.excluded),
      ])
    )
  );
  return lines;
}

/** What the cut state does to a comparison between a Jev arm and a Claude arm. */
function cutLines(analysis: Analysis): string[] {
  const lines = ["", "## The state Jev cut, and what it costs the comparison", ""];
  lines.push(analysis.stateCut.sentence);
  lines.push("");
  lines.push(
    `${count(analysis.stateCut.jevRowsCut)} Jev rows were answered over a cut state, over ` +
      `${count(analysis.stateCut.casesCut)} cases. A case is cut-free when no row of any arm ` +
      "for it was cut, and only the cut-free cases are a comparison between a Jev arm and a " +
      "Claude arm over the same evidence."
  );
  lines.push("");
  lines.push(
    ...table(
      ["slice", "cut-free cases", "cases with rows"],
      Object.keys(analysis.stateCut.cutFreeBySlice)
        .sort()
        .map((slice) => [
          slice,
          count(analysis.stateCut.cutFreeBySlice[slice]?.cutFree ?? 0),
          count(analysis.stateCut.cutFreeBySlice[slice]?.cases ?? 0),
        ])
    )
  );
  lines.push("");
  lines.push(
    ...table(
      ["arm", "cases cut", "rows cut", "agreement, all cases", "agreement, cut-free cases"],
      analysis.arms.map((arm) => [
        arm.arm,
        count(arm.stateCut.cases),
        count(arm.stateCut.rows),
        say(arm.approval.agreement),
        say(arm.approval.cutFree.agreement),
      ])
    )
  );
  lines.push("");
  const differing = analysis.arms.filter((arm) => arm.approval.cutFree.differs);
  if (differing.length === 0) {
    lines.push(
      "No arm's two numbers differ by more than the interval of the first, so the cut moved " +
        "nothing a reader can measure here."
    );
  } else {
    lines.push(
      `${differing.map((arm) => arm.arm).join(", ")}: the two numbers differ by more than the ` +
        "interval. The cut-free number is the comparable number; the all-cases number flatters " +
        "or punishes the Jev arms depending on what the cut removed."
    );
  }
  return lines;
}

/** One kind of case, over every arm: the five tables the primary measure is printed in. */
function kindLines(analysis: Analysis, kind: CaseKind): string[] {
  const of = (arm: ArmAnalysis): KindAnalysis =>
    kind === "approval" ? arm.approval : arm.question;
  const lines = ["", `## The ${kind} cases of the report slice`, ""];
  if (analysis.arms.length === 0) {
    lines.push("No arm has written a row, so there is nothing to read here yet.");
    return lines;
  }
  lines.push(
    "The approval cases and the question cases are never pooled. Every share prints its count, " +
      "and a count too small to bound prints the words that say so."
  );
  lines.push("");
  lines.push("### Agreement with the person");
  lines.push("");
  lines.push(
    ...table(
      ["arm", "cases", "undecided", "agreement", "bound"],
      analysis.arms.map((arm) => {
        const one = of(arm);
        return [arm.arm, count(one.cases), count(one.undecided), ...cells(one.agreement)];
      })
    )
  );
  lines.push("");
  lines.push("### Decided at 1 in 20, at the threshold fitted on the fit slice");
  lines.push("");
  lines.push(
    ...table(
      ["arm", "fit cases", "threshold", "decided", "coverage", "disagreements", "report bound"],
      analysis.arms.map((arm) => {
        const one = of(arm);
        return [
          arm.arm,
          `${count(one.fit.decided)} decided of ${count(one.fit.cases)}`,
          one.fit.threshold === null ? "none holds the rate" : num(one.fit.threshold, 2),
          count(one.atThreshold.decided),
          `${num(one.atThreshold.coverage)} of ${count(one.cases)}`,
          count(one.atThreshold.disagreements),
          one.atThreshold.bound === null
            ? (one.atThreshold.noBound ?? "no bound")
            : `at most ${num(one.atThreshold.bound)}`,
        ];
      })
    )
  );
  const unfitted = analysis.arms.filter((arm) => of(arm).fit.threshold === null);
  if (unfitted.length > 0) {
    lines.push("");
    for (const arm of unfitted) {
      const fit = of(arm).fit;
      lines.push(
        `- ${arm.arm} fitted no threshold on its ${count(fit.decided)} decided fit cases, so ` +
          "it decides nothing: " +
          (fit.lowestBound === null
            ? "no fit case of this kind was judged"
            : `the lowest bound any confidence value of them reaches is ${num(fit.lowestBound)}, ` +
              `above the ${num(RATE, 2)} it must hold`) +
          "."
      );
    }
  }
  lines.push("");
  lines.push("### The largest band holding zero observed disagreements");
  lines.push("");
  lines.push(
    "No finite set of cases gives a bound of zero, so this is the largest confidence band " +
      "holding zero observed disagreements, with that band's count and its actual bound."
  );
  lines.push("");
  lines.push(
    ...table(
      ["arm", "threshold", "decided", "coverage", "actual bound"],
      analysis.arms.map((arm) => {
        const band = of(arm).zeroBand;
        if (band === null) {
          return [arm.arm, "none", "0", "0.000", "every band holds a disagreement"];
        }
        return [
          arm.arm,
          num(band.threshold, 2),
          count(band.decided),
          num(band.coverage),
          arm.bounded ? `at most ${num(band.bound)}` : (arm.noBound ?? "no bound"),
        ];
      })
    )
  );
  lines.push("");
  lines.push("### Against the always-approve baseline, paired over the same cases");
  lines.push("");
  lines.push(
    ...table(
      ["arm", "baseline", "arm", "arm only", "baseline only", "McNemar p", "difference interval"],
      analysis.arms.map((arm) => {
        const paired = of(arm).paired;
        const baseline = of(arm).baseline;
        return [
          arm.arm,
          `${num(baseline.share)} (${count(baseline.approvals)} of ${count(baseline.counted)})`,
          `${num(paired.armShare)} (${count(paired.counted)})`,
          count(paired.armOnly),
          count(paired.otherOnly),
          paired.discordant === 0 ? "1.000 (no case differs)" : num(paired.pValue),
          `${num(paired.difference)} in [${num(paired.interval.lower)}, ` +
            `${num(paired.interval.upper)}]`,
        ];
      })
    )
  );
  lines.push("");
  lines.push("### The anchor set");
  lines.push("");
  lines.push(
    ...table(
      ["arm", "anchor agreement", "bound"],
      analysis.arms.map((arm) => [arm.arm, ...cells(of(arm).anchor)])
    )
  );
  return lines;
}

/** The decision rule, printed as it was applied. */
function decisionLines(analysis: Analysis): string[] {
  const lines = ["", "## The decision rule, as it was applied", ""];
  lines.push(
    "The threshold of an arm is the lowest counted-confidence value whose one-sided 90 percent " +
      "upper bound on the fit slice's decided approval cases is at or under 0.05; an arm with " +
      "no such threshold decides nothing and its coverage is zero. The winner is the arm with " +
      "the highest coverage on the approval report slice at its own fitted threshold, among the " +
      "arms whose report-slice bound is still at or under 0.05."
  );
  lines.push("");
  for (const line of analysis.decision.approval.lines) lines.push(`- ${line}`);
  const flat = analysis.arms.filter(
    (arm) => arm.confidenceValues.length <= 1 && arm.approval.cases + arm.question.cases > 0
  );
  lines.push(
    flat.length === 0
      ? "- Every arm's counted confidence takes more than one value, so a threshold has " +
          "something to select on in each of them."
      : "- These arms' judgements are identical on every case they decided, so their counted " +
          "confidence takes one value and no threshold selects anything in them, which the " +
          "plan names as a result that would falsify this round: " +
          `${flat.map((arm) => arm.arm).join(", ")}.`
  );
  lines.push("");
  const winner = analysis.arms.find((arm) => arm.arm === analysis.decision.approval.winner);
  if (winner === undefined) {
    lines.push(
      "No arm is named the winner, so no bound is adjusted for the choice among twelve arms."
    );
    return lines;
  }
  lines.push(
    `The winner is ${winner.arm}. Its pointwise bound is ` +
      `${analysis.decision.pointwise === null ? (winner.approval.atThreshold.noBound ?? "no bound") : `at most ${num(analysis.decision.pointwise)}`}` +
      `, over ${count(winner.approval.atThreshold.decided)} decided cases with ` +
      `${count(winner.approval.atThreshold.disagreements)} disagreements.`
  );
  lines.push(
    "A bound chosen as the best of twelve is not a 90 percent bound on the chosen one, so it " +
      `is read again at ${num(ADJUSTED_CONFIDENCE, 5)}, which is 1 - 0.10 / ` +
      `${ARMS_CHOSEN_AMONG}: ` +
      `${analysis.decision.adjusted === null ? (winner.approval.atThreshold.noBound ?? "no bound") : `at most ${num(analysis.decision.adjusted)}`}.`
  );
  const paired = winner.approval.paired;
  lines.push(
    `Against the always-approve baseline on those same cases, paired: the difference is ` +
      `${num(paired.difference)} in [${num(paired.interval.lower)}, ` +
      `${num(paired.interval.upper)}] over ${count(paired.counted)} cases, at an exact McNemar ` +
      `p of ${num(paired.pValue)} over ${count(paired.discordant)} discordant pairs. ` +
      (paired.interval.lower > 0
        ? "The arm beats the baseline by more than the interval of the paired difference."
        : "The arm does not beat the baseline by more than the interval of the paired " +
          "difference, which the plan names as a result that would falsify this round.")
  );
  return lines;
}

/** The three readings the plan calls the primary one and its two sensitivities. */
function sensitivityLines(analysis: Analysis): string[] {
  const lines = ["", "## The sensitivity readings", ""];
  lines.push(
    "The primary reading is the modal verdict of five judgements. The first is what one call " +
      "would have given. The third takes one case per document, which is free of the " +
      "within-document correlation the grouped draw does not remove. All three are over the " +
      "approval cases of the report slice."
  );
  lines.push("");
  lines.push(
    ...table(
      ["arm", "modal of five", "first judgement alone", "one case per document"],
      analysis.arms.map((arm) => [
        arm.arm,
        say(arm.approval.sensitivity.modal),
        say(arm.approval.sensitivity.first),
        say(arm.approval.sensitivity.oneCasePerDocument),
      ])
    )
  );
  return lines;
}

/** The confounds of every arm, each bin under the floor rule. */
function confoundLines(analysis: Analysis): string[] {
  const lines = ["", "## The confounds", ""];
  lines.push(
    "Each confound is read over every scored case of the arm, the fit, report and anchor " +
      `slices together, and not over the report slice alone. A bin with fewer than ` +
      `${MINIMUM_DECIDED} decided cases prints its count and its observed agreement and no ` +
      "bound. The finer breakdown by hour and by project follows the six splits, under the " +
      "same rule."
  );
  if (analysis.confounds.length === 0) {
    lines.push("");
    lines.push("No arm has written a row, so no confound has a case in it yet.");
    return lines;
  }
  for (const arm of analysis.confounds) {
    lines.push("");
    lines.push(`### ${arm.arm}`);
    lines.push("");
    const rows: string[][] = [];
    for (const confound of arm.tables) {
      for (const line of confound.lines) {
        rows.push([
          confound.confound,
          line.name,
          count(line.decided),
          count(line.disagreements),
          num(line.observedAgreement),
          line.upperBound === null
            ? `no bound at this count (${line.decided} decided, ${MINIMUM_DECIDED} needed)`
            : `at most ${num(line.upperBound)}`,
        ]);
      }
    }
    lines.push(
      ...table(["confound", "bin", "decided", "disagreements", "agreement", "bound"], rows)
    );
  }
  return lines;
}

/** The class table, which comes from the corpus and not from the round. */
function classLines(analysis: Analysis): string[] {
  const lines = ["", "## The classes the person answered the same way", ""];
  lines.push(
    "This table is a pass over the corpus's own rows. It reads no judgement of any arm and no " +
      "row of the round: for each class, the share of that class's approval decisions the " +
      "person approved, and the count. An arm's number says how well a model predicts the " +
      "person's answer; this table says which classes the person answers the same way every " +
      "time. A question case carries no approve-or-request-changes verdict, so it counts in " +
      "the class's cases and not in its share."
  );
  const render = (title: string, rows: readonly ClassLine[], about: string): void => {
    lines.push("");
    lines.push(`### ${title}`);
    lines.push("");
    lines.push(about);
    lines.push("");
    if (rows.length === 0) {
      lines.push("No case falls in this part of the corpus.");
      return;
    }
    lines.push(
      ...table(
        ["class", "which", "cases", "approval decisions", "approved", "bound on the rest"],
        rows.map((line) => [
          line.dimension,
          line.className,
          count(line.cases),
          count(line.counted),
          line.counted === 0
            ? "no approval decision in this class"
            : `${num(line.share)} (${count(line.approvals)} of ${count(line.counted)})`,
          line.bound === null ? (line.noBound ?? "no bound") : `at most ${num(line.bound)}`,
        ])
      )
    );
  };
  render(
    "The whole corpus",
    analysis.classes.whole,
    `Every case of the corpus: ${count(analysis.corpus.cases)} of them.`
  );
  render(
    "The post-cut part",
    analysis.classes.postCut,
    analysis.classes.cut === null
      ? "The cut is not known, so this part is empty: no draw naming a cut was readable."
      : `The cases answered at or after ${analysis.classes.cut}, which is the part no recipe ` +
          "was built from."
  );
  return lines;
}

/** The confidence table, where it was asked for. */
function confidenceLines(analysis: Analysis): string[] {
  const held = analysis.confidenceTable;
  if (held === null) return [];
  const lines = ["", "## The confidence table", ""];
  if (held.identicalArms.length > 0) {
    lines.push(
      `These arms' judgements are identical on every case they scored, so their counted ` +
        `confidence takes one value and no threshold selects anything in them: ` +
        `${held.identicalArms.join(", ")}.`
    );
    lines.push("");
  }
  if (held.arm === null) {
    lines.push(held.reason ?? "There is no generative arm to read.");
    return lines;
  }
  lines.push(
    `The generative arm is ${held.arm}, read over ${count(held.caseIds.length)} report-slice ` +
      `cases: ${count(held.approved)} the person approved and ` +
      `${count(held.requestedChanges)} the person asked for changes on.` +
      (held.seed === null ? " The cases were named by --cases." : ` Seed ${held.seed}.`)
  );
  if (held.reason !== null) {
    lines.push("");
    lines.push(held.reason);
  }
  lines.push("");
  lines.push(
    ...table(
      ["counted confidence", "cases", "disagreements", "agreement", "bound"],
      held.lines.map((line) => [
        line.name,
        count(line.decided),
        count(line.disagreements),
        num(line.observedAgreement),
        line.upperBound === null
          ? `no bound at this count (${line.decided} decided, ${MINIMUM_DECIDED} needed)`
          : `at most ${num(line.upperBound)}`,
      ])
    )
  );
  lines.push("");
  if (held.jevArm === null) {
    lines.push("No Jev arm has written a row for these cases, so it has no probability to read.");
  } else {
    lines.push(
      `Jev's own probability over the same cases, as its own arm, which it is and is never ` +
        `mixed into the counted confidence. The arm is ${held.jevArm}, and the value is its ` +
        "stated probability of what it chose, to the nearest tenth."
    );
    lines.push("");
    lines.push(
      ...table(
        ["Jev's own probability", "cases", "disagreements", "agreement", "bound"],
        held.jevLines.map((line) => [
          line.name,
          count(line.decided),
          count(line.disagreements),
          num(line.observedAgreement),
          line.upperBound === null
            ? `no bound at this count (${line.decided} decided, ${MINIMUM_DECIDED} needed)`
            : `at most ${num(line.upperBound)}`,
        ])
      )
    );
  }
  lines.push("");
  lines.push("The cases this table was read over:");
  lines.push("");
  for (let index = 0; index < held.caseIds.length; index += IDS_PER_LINE) {
    lines.push(`    ${held.caseIds.slice(index, index + IDS_PER_LINE).join("  ")}`);
  }
  return lines;
}

/** The whole report, in the order it is printed and written. */
export function renderReport(analysis: Analysis): string[] {
  return [
    ...headLines(analysis),
    ...spendLines(analysis),
    ...cutLines(analysis),
    ...kindLines(analysis, "approval"),
    ...kindLines(analysis, "question"),
    ...decisionLines(analysis),
    ...sensitivityLines(analysis),
    ...confoundLines(analysis),
    ...classLines(analysis),
    ...confidenceLines(analysis),
    "",
  ];
}

/** One line of `rows.jsonl` read back, or null where it is not a row the runner wrote. */
function rowOf(line: string): JudgementRow | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(line);
  } catch {
    return null;
  }
  const record = recordOf(parsed);
  if (record === null) return null;
  if (typeof record.arm !== "string" || typeof record.caseId !== "string") return null;
  if (typeof record.judgement !== "number" || !Array.isArray(record.verdict)) return null;
  // The rest of the line is what the runner wrote, whose shape is `JudgementRow`.
  return record as unknown as JudgementRow;
}

/** The rows of a round, read as far as they go: a round still running has written part of them. */
async function readRows(
  round: string
): Promise<{ rows: JudgementRow[]; read: number; unreadable: number }> {
  let text: string;
  try {
    text = await readFile(join(round, "rows.jsonl"), "utf8");
  } catch {
    return { rows: [], read: 0, unreadable: 0 };
  }
  const rows: JudgementRow[] = [];
  let unreadable = 0;
  let read = 0;
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    read += 1;
    const row = rowOf(line);
    if (row === null) unreadable += 1;
    else rows.push(row);
  }
  return { rows, read, unreadable };
}

/** The round's own summary, which a round writes when it stops and not while it runs. */
async function readSummary(round: string): Promise<RoundSummary | null> {
  let text: string;
  try {
    text = await readFile(join(round, "round.json"), "utf8");
  } catch {
    return null;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new CorpusError(`${join(round, "round.json")} is not JSON`);
  }
  const record = recordOf(parsed);
  if (record === null || !Array.isArray(record.arms) || recordOf(record.judgements) === null) {
    throw new CorpusError(`${join(round, "round.json")} is not a round summary`);
  }
  return record as unknown as RoundSummary;
}

/** How many cases each slice holds, from the draw the corpus directory carries, where it does. */
async function readSliceSizes(corpus: string): Promise<Record<string, number> | null> {
  let text: string;
  try {
    text = await readFile(join(corpus, "slices", "slices.json"), "utf8");
  } catch {
    return null;
  }
  const record = recordOf(JSON.parse(text));
  if (record === null) return null;
  const sizes: Record<string, number> = {};
  for (const [slice, ids] of Object.entries(record)) {
    if (Array.isArray(ids)) sizes[slice] = ids.length;
  }
  return sizes;
}

/** The cut the draw was made at, which is what the post-cut class table is read over. */
async function readCut(corpus: string): Promise<string | null> {
  let text: string;
  try {
    text = await readFile(join(corpus, "slices", "draw.json"), "utf8");
  } catch {
    return null;
  }
  const record = recordOf(JSON.parse(text));
  const cut = record?.cut;
  return typeof cut === "string" && Number.isFinite(Date.parse(cut)) ? cut : null;
}

/** The case ids a `--cases` file names: one per line, or a JSON list of them. */
async function readCases(path: string): Promise<string[]> {
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch {
    throw new UsageError(`--cases names ${path}, which could not be read`);
  }
  const trimmed = text.trim();
  if (trimmed.startsWith("[")) {
    const parsed: unknown = JSON.parse(trimmed);
    if (!Array.isArray(parsed)) throw new UsageError(`${path} is not a list of case ids`);
    return parsed.filter((one): one is string => typeof one === "string");
  }
  return trimmed
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "" && !line.startsWith("#"));
}

function readFlags(argv: readonly string[]): Flags {
  let values: {
    corpus?: string;
    round?: string;
    out?: string;
    "confidence-table"?: boolean;
    cases?: string;
    format?: string;
  };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        corpus: { type: "string" },
        round: { type: "string" },
        out: { type: "string" },
        "confidence-table": { type: "boolean" },
        cases: { type: "string" },
        format: { type: "string" },
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
  const format = values.format?.trim() ?? "text";
  if (format !== "text" && format !== "json") {
    throw new UsageError(`--format takes text or json, not ${format}\n\n${USAGE}`);
  }
  return {
    corpus,
    round,
    out: values.out?.trim(),
    confidenceTable: values["confidence-table"] === true,
    cases: values.cases?.trim(),
    format,
  };
}

async function analyze(flags: Flags): Promise<number> {
  const corpus = await readCorpus(flags.corpus);
  const out = flags.out === undefined ? null : await outDirectory(flags.out);
  const { rows, read, unreadable } = await readRows(flags.round);
  const summary = await readSummary(flags.round);
  const sliceSizes = await readSliceSizes(corpus.directory);
  const cut = await readCut(corpus.directory);
  const wantedCases = flags.cases === undefined ? null : await readCases(flags.cases);
  const seed = wantedCases === null && flags.confidenceTable ? newSeed() : null;
  const analysis = analyseRound({
    corpus,
    roundDirectory: flags.round,
    rows,
    rowsRead: read,
    rowsUnreadable: unreadable,
    summary,
    sliceSizes,
    cut,
    confidenceTable: flags.confidenceTable,
    wantedCases,
    seed,
  });
  const text = renderReport(analysis).join("\n");
  console.log(flags.format === "json" ? JSON.stringify(analysis, null, 2) : text);
  if (out === null) return 0;
  await mkdir(out, { recursive: true });
  await writeFile(join(out, "report.md"), `${text}\n`, "utf8");
  await writeFile(join(out, "report.json"), `${JSON.stringify(analysis, null, 2)}\n`, "utf8");
  const ids = analysis.confidenceTable?.caseIds ?? [];
  if (analysis.confidenceTable !== null && analysis.confidenceTable.seed !== null) {
    await writeFile(join(out, "confidence-cases.txt"), `${ids.join("\n")}\n`, "utf8");
  }
  console.error(`written           ${join(out, "report.md")}`);
  console.error(`                  ${join(out, "report.json")}`);
  return 0;
}

async function main(argv: readonly string[]): Promise<number> {
  let flags: Flags;
  try {
    flags = readFlags(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(error.message);
    return 2;
  }
  try {
    return await analyze(flags);
  } catch (error) {
    if (
      error instanceof UsageError ||
      error instanceof OutputPathError ||
      error instanceof CorpusError
    ) {
      console.error(`${error.name}: ${error.message}`);
      return 2;
    }
    throw error;
  }
}

// Only when run: a test that imports the readings must not run a command.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
