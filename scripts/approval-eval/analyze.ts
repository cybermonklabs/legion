import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import type { CaseKind, CaseRow } from "./build-corpus";
import {
  artifactsOf,
  CannotRunError,
  type Corpus,
  CorpusError,
  count,
  leadDocumentOf,
  num,
  outDirectory,
  randomStream,
  readCorpus,
  recordOf,
  signed,
  table,
  UsageError,
  verdictOf,
} from "./corpus-io";
import { KINDS, SCORED_SLICES, type ScoredSlice, type SliceName, TARGETS } from "./draw";
import { corpusDrawPath, cutOf, groupKeyOf, readDraw } from "./draw-io";
import {
  type CaseNeighbours,
  type JudgementRow,
  readEvidenceNeighbours,
  readRows,
} from "./round-io";
import type { RoundSummary } from "./run-round";
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
 * It reads four things and calls nothing: `rows.jsonl`, `round.json` and the evidence cache of
 * a round directory, and the corpus the round scored with the draw it carries. A round still
 * running has written part of its rows and no summary, which is a report over fewer cases and
 * never a crash: every table renders, every number prints its n, and a number too thin to bound
 * prints the words that say so instead.
 *
 * **Where the slices cross.** The draw now in use grouped an approval case by its lead
 * document, so two cases of one issue could reach two slices. The report finds every issue
 * whose cases sit in more than one of the fit, report and anchor slices and prints them. Where
 * the crossing touches the fit slice, the fit case is dropped from the fit slice and from
 * nothing else: that slice only chooses a threshold and carries no reported number, while the
 * report and anchor slices carry every number. A crossing between the report and the anchor
 * slice drops nothing and is printed as a shortfall, since taking a case out of either of those
 * is resizing a reported slice.
 *
 * **What the manifests hold.** One manifest per slice is written into `--out`, naming the
 * slice's role in words, its case ids in order, its count and the SHA-256 of that id list, so a
 * reader learns what each set holds without opening an archive.
 *
 * **The two kinds of error.** An arm's disagreements over approval cases are printed as two
 * numbers: false approvals, where the arm approved and the person requested changes, and false
 * requests for changes, where it is the other way about. Only the first lets through a change
 * the person would have stopped, and the rate the person set is about that one.
 *
 * **What proves retrieval returned no near-label.** The report counts, over every scored case,
 * the neighbours retrieval dropped for sitting on the judged case's own document or issue
 * (`excluded` of `rows.jsonl`) and the neighbours the evidence cache recorded that name the
 * judged case's own document at any version (`nearest.neighbours` of the cache, against
 * `approval.documents` of the corpus). The second count must be zero, and the cases are printed
 * where it is not.
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
 * **The floor, and the status that holds it.** The report is read across arms and models: every
 * number of it is a comparison, and the winner is chosen among the arms. Under four arms or
 * under three models it is printed and written as ever and the command exits 1, naming the arms
 * and the models the round held.
 *
 * **The confidence table.** `--confidence-table` draws 30 report-slice cases the person decided
 * both ways under a seed, which `--confidence-seed` names or the round itself gives, and prints
 * the seed in the table and in `report.json` so the same 30 can be drawn again. Beside those 30
 * it prints the same arm's counted confidence over the whole report slice, since 30 cases can
 * take one value where the slice they were drawn from takes several.
 *
 * **What comes from the corpus and not from the round.** The class table is a pass over the
 * corpus's own rows: it reads no judgement, and says which classes of case the person answered
 * the same way every time. An arm's number says how well a model predicts the person's answer;
 * that table says where there was anything to predict.
 */

/** The report is read across arms and models, and refuses a round holding too few of either. */
const LEAST_ARMS = 4;
const LEAST_MODELS = 3;

/** The round holds fewer arms or models than the report can be read over. */
const EXIT_UNDER_FLOOR = 1;

const USAGE = `usage: bun scripts/approval-eval/analyze.ts --corpus <dir> --round <dir> \\
  [--out <dir>] [--confidence-table] [--confidence-arm <name>] [--cases <file>] \\
  [--confidence-seed <n>] [--format text|json]

  --corpus            the corpus directory a build wrote, which the round scored
  --round             the round directory, holding rows.jsonl, round.json and the evidence cache
  --out               where report.md, report.json and one manifest per slice are written,
                      which must be an absolute path outside this repository's workspace
  --confidence-table  print the counted confidence of one generative arm over 30 report-slice
                      cases the person decided both ways, beside that arm's counted confidence
                      over the whole report slice and the probability the Jev arm of the same
                      recipe stated on the 30
  --confidence-arm    which generative arm that table is read on; with none, the generative arm
                      that agrees most over the approval cases of the report slice
  --cases <file>      the 30 case ids that table reads, one per line or as a JSON list; with
                      none, they are drawn from the report slice under the seed below and
                      written to --out, so a later run reads the same 30
  --confidence-seed   the seed those 30 are drawn under; with none, the seed is derived from
                      the round itself, and either way it is printed in the table and in
                      report.json, so the same 30 can be drawn again
  --format            text, the report as it prints, or json, the same numbers as one object

Exit status: 0 when the report was printed, 1 when the round is under the floor the report
needs, which is ${LEAST_ARMS} arms and ${LEAST_MODELS} models, and 2 when it could not run.`;

/** The disagreement rate the round is held to, which is the person's own 1 in 20. */
const RATE = 0.05;

/** What each of those three slices is for, in the words each manifest and the report carry. */
export const SLICE_ROLES: Readonly<Record<ScoredSlice, string>> = {
  fit: "the calibration set: it chooses the threshold and carries no reported number",
  report: "the test set: every reported number is read from it",
  anchor: "the fixed set a later round's move is read against",
};

/** How a manifest's hash is taken, which is what the manifest itself says it is. */
const HASHED_AS =
  "the SHA-256 of the case ids of the slice, joined by one newline, in the order the manifest " +
  "holds them";

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
  /** Which generative arm that table is read on, and null where the caller named none. */
  confidenceArm: string | null;
  cases?: string;
  /** The seed the 30 cases are drawn under, and null where the round itself gives one. */
  confidenceSeed: number | null;
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

/** One kind of mistake: how many cases carry it, and what share of the decided cases that is. */
export interface ErrorKind {
  count: number;
  share: number;
}

/**
 * The mistakes of an arm over approval cases, counted apart. They are never one number: only a
 * false approval lets through a change the person would have stopped, and the rate the person
 * set is about that one.
 */
export interface ErrorSplit {
  /** The cases this count is read over: the ones the arm reached a verdict on. */
  decided: number;
  /** The arm approved where the person requested changes. */
  falseApprovals: ErrorKind;
  /** The arm requested changes where the person approved. */
  falseRequests: ErrorKind;
  /** Disagreements that are neither, one side having chosen some other option. */
  other: number;
}

/** One case of a crossing: the slice the draw put it in, and whether the report dropped it. */
export interface CrossingCase {
  caseId: string;
  slice: ScoredSlice;
  kind: CaseKind;
  dropped: boolean;
}

/** One issue whose cases sit in more than one of the fit, report and anchor slices. */
export interface Crossing {
  issue: string;
  /** The slices its cases sit in, in the order the report prints them. */
  slices: ScoredSlice[];
  cases: CrossingCase[];
  /** The ids dropped from the fit slice, which is every fit case of this issue. */
  droppedFromFit: string[];
  /** True where its cases sit in both the report and the anchor slice, which drops nothing. */
  reportAndAnchor: boolean;
}

/** What the drawn slices cross on, and what the report did about each crossing. */
export interface Leakage {
  /** False where no draw could be read, so nothing could be checked. */
  read: boolean;
  /** The key the draw grouped by, where `draw.json` records one. */
  groupKey: string | null;
  crossings: Crossing[];
  /** Every id dropped from the fit slice, over all the crossings. */
  droppedFromFit: string[];
  /** The issues crossing the report and the anchor slice, which are shortfalls and not drops. */
  shortfalls: string[];
  /** The fit slice as drawn, as kept, and against the target it was drawn for, by kind. */
  fit: Record<CaseKind, { drawn: number; kept: number; target: number }>;
}

/** One slice as the manifest written for it holds it. */
export interface Manifest {
  slice: ScoredSlice;
  /** What the slice is for, in words. */
  role: string;
  caseIds: string[];
  count: number;
  sha256: string;
  /** How that hash was taken, so a reader can take it again. */
  hashedAs: string;
  /** The ids the crossing count dropped from this slice, which `caseIds` no longer holds. */
  dropped: string[];
}

/** One neighbour sitting on the judged case's own document, which must never be there. */
export interface OwnDocumentNeighbour {
  caseId: string;
  neighbour: string;
  /** The artifact both of them name, whatever version each named. */
  artifactId: string;
}

/** The proof that retrieval returned no neighbour on the judged case's own document. */
export interface RetrievalProof {
  /** The scored cases: the cases the round has written a row for. */
  cases: number;
  /** The rows carrying an `excluded` count, which is what the first number is summed over. */
  rowsCounted: number;
  /** The sum of `excluded` over those rows. */
  excluded: number;
  /** The cases of the evidence cache this proof read, and the neighbours they recorded. */
  casesRead: number;
  neighbours: number;
  /** Neighbours naming the judged case's own document at any version, which must be none. */
  onOwnDocument: OwnDocumentNeighbour[];
  /** The rows of the cases that carry one, which is the count that must be zero. */
  rowsOnOwnDocument: number;
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
    /** How many fit cases of this kind the crossing count dropped before the fit. */
    dropped: number;
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
  /** The two kinds of error apart, over approval cases, and null over question cases. */
  errors: { report: ErrorSplit; atThreshold: ErrorSplit } | null;
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

/** One value of a counted-confidence table, beside what approving everything would have scored. */
export interface SliceBaseline {
  /** The value, as the table's own first column names it. */
  name: string;
  /** The arm's agreement over the cases that took the value. */
  agreement: number;
  /** What an arm that approved every one of those same cases would have scored on them. */
  baseline: Baseline;
  /** Whether the value carries a bound, which is the value a reader would act on. */
  bounded: boolean;
}

/** The confidence table of one generative arm, beside Jev's own probability on the same cases. */
export interface ConfidenceTable {
  arm: string | null;
  /** The information recipe of that arm, which the Jev half of the table is read on too. */
  recipe: string | null;
  /** Whether the caller named the arm, or it was chosen as the one that agrees most. */
  armChoice: "named" | "best";
  /** That arm's own agreement over the approval cases of the report slice. */
  armAgreement: Reading | null;
  /** Why there is no table, where there is none. */
  reason: string | null;
  caseIds: string[];
  /** The seed the cases were drawn under, and null where a file named them. */
  seed: number | null;
  /** Whether the caller named that seed, or it was derived from the round itself. */
  seedChoice: "named" | "derived" | null;
  approved: number;
  requestedChanges: number;
  lines: BinLine[];
  /**
   * The same arm's counted confidence over every approval case of the report slice it decided,
   * which is the slice the cases above were drawn from and the distribution they are read
   * against: 30 cases can show one value where the slice they came from shows several.
   */
  sliceLines: BinLine[];
  /** How many approval cases of the report slice that arm decided, which `sliceLines` counts. */
  sliceDecided: number;
  /**
   * What an arm that approved every one of those same cases would have scored, which is the
   * number the whole table is read against: an agreement above it is a finding and one below it
   * is a reading of the slice's own approval rate.
   */
  sliceBaseline: Baseline;
  /**
   * The always-approve baseline on the cases at each value of `sliceLines`, in that same order:
   * an agreement read alone is a number a reader takes for a finding, and the baseline on the
   * very same cases is what says whether it is one.
   */
  sliceBaselines: SliceBaseline[];
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
      /** The arms that invocation named, which are the arms its planned count is read over. */
      arms: string[];
    } | null;
    /**
     * What the rows say about the invocation `round.json` records: a round run one arms file at
     * a time writes one summary per invocation, and the last one names that invocation's arms
     * alone while the directory holds every invocation's rows.
     */
    invocation: {
      /** Judgements the rows answer for the arms that summary names. */
      answered: number;
      left: number;
      /** Arms with rows that the last invocation did not name, which earlier ones wrote. */
      armsNotNamed: string[];
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
  leakage: Leakage;
  manifests: Manifest[];
  retrieval: RetrievalProof;
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

/** The issue a case belongs to, which is what a crossing is counted over. */
function issueOf(row: CaseRow): string {
  return row.issueKey ?? `ask:${row.askId}`;
}

/**
 * Every issue whose cases sit in more than one of the fit, report and anchor slices, with what
 * the report does about each: a crossing touching the fit slice drops that issue's fit cases,
 * since the fit slice only chooses a threshold; a crossing between the report and the anchor
 * slice drops nothing, since taking a case out of either of those resizes a reported slice.
 */
export function crossingsOf(
  slices: Readonly<Record<string, readonly string[]>> | null,
  corpus: ReadonlyMap<string, CaseRow>,
  groupKey: string | null = null
): Leakage {
  const fit: Record<CaseKind, { drawn: number; kept: number; target: number }> = {
    approval: { drawn: 0, kept: 0, target: TARGETS.approval.fit },
    question: { drawn: 0, kept: 0, target: TARGETS.question.fit },
  };
  if (slices === null) {
    return { read: false, groupKey, crossings: [], droppedFromFit: [], shortfalls: [], fit };
  }
  const byIssue = new Map<string, CrossingCase[]>();
  for (const slice of SCORED_SLICES) {
    for (const caseId of slices[slice] ?? []) {
      const row = corpus.get(caseId);
      if (row === undefined) continue;
      if (slice === "fit") fit[row.kind].drawn += 1;
      const held = byIssue.get(issueOf(row)) ?? [];
      byIssue.set(issueOf(row), held);
      held.push({ caseId, slice, kind: row.kind, dropped: false });
    }
  }
  const crossings: Crossing[] = [];
  const droppedFromFit: string[] = [];
  const shortfalls: string[] = [];
  for (const [issue, cases] of [...byIssue.entries()].sort((one, two) =>
    one[0] < two[0] ? -1 : 1
  )) {
    const slices = SCORED_SLICES.filter((slice) => cases.some((one) => one.slice === slice));
    if (slices.length < 2) continue;
    const inFit = cases.filter((one) => one.slice === "fit");
    const reportAndAnchor = slices.includes("report") && slices.includes("anchor");
    for (const one of inFit) {
      one.dropped = true;
      droppedFromFit.push(one.caseId);
    }
    if (reportAndAnchor) shortfalls.push(issue);
    crossings.push({
      issue,
      slices: [...slices],
      cases: [...cases].sort((one, two) => (one.caseId < two.caseId ? -1 : 1)),
      droppedFromFit: inFit.map((one) => one.caseId),
      reportAndAnchor,
    });
  }
  const dropped = new Set(droppedFromFit);
  for (const caseId of slices.fit ?? []) {
    const row = corpus.get(caseId);
    if (row !== undefined && !dropped.has(caseId)) fit[row.kind].kept += 1;
  }
  return { read: true, groupKey, crossings, droppedFromFit, shortfalls, fit };
}

/** The SHA-256 a manifest carries: the ids joined by one newline, in the order it holds them. */
export function manifestHash(caseIds: readonly string[]): string {
  return createHash("sha256").update(caseIds.join("\n"), "utf8").digest("hex");
}

/** One manifest per slice, the fit slice holding what the crossing count left it. */
export function manifestsOf(
  slices: Readonly<Record<string, readonly string[]>> | null,
  droppedFromFit: readonly string[]
): Manifest[] {
  if (slices === null) return [];
  const dropped = new Set(droppedFromFit);
  return SCORED_SLICES.map((slice) => {
    const drawn = slices[slice] ?? [];
    const caseIds = slice === "fit" ? drawn.filter((one) => !dropped.has(one)) : [...drawn];
    return {
      slice,
      role: SLICE_ROLES[slice],
      caseIds,
      count: caseIds.length,
      sha256: manifestHash(caseIds),
      hashedAs: HASHED_AS,
      dropped: slice === "fit" ? drawn.filter((one) => dropped.has(one)) : [],
    };
  });
}

/**
 * The proof that retrieval returned no neighbour on the judged case's own document: what the
 * rows say was dropped for sharing ground, and what the evidence cache recorded as returned.
 * The second count is over the cases the round scored, and must be zero.
 */
export function retrievalProofOf(
  rows: readonly JudgementRow[],
  evidence: readonly CaseNeighbours[],
  corpus: ReadonlyMap<string, CaseRow>
): RetrievalProof {
  const scored = new Set(rows.map((row) => row.caseId));
  let rowsCounted = 0;
  let excluded = 0;
  for (const row of rows) {
    if (typeof row.excluded !== "number") continue;
    rowsCounted += 1;
    excluded += row.excluded;
  }
  let casesRead = 0;
  let neighbours = 0;
  const onOwnDocument: OwnDocumentNeighbour[] = [];
  const leaking = new Set<string>();
  for (const one of evidence) {
    const row = corpus.get(one.caseId);
    if (row === undefined || !scored.has(one.caseId)) continue;
    casesRead += 1;
    neighbours += one.neighbours.length;
    const judged = artifactsOf(row);
    if (judged.size === 0) continue;
    for (const neighbour of one.neighbours) {
      const near = corpus.get(neighbour);
      if (near === undefined) continue;
      for (const artifactId of artifactsOf(near)) {
        if (!judged.has(artifactId)) continue;
        onOwnDocument.push({ caseId: one.caseId, neighbour, artifactId });
        leaking.add(one.caseId);
      }
    }
  }
  return {
    cases: scored.size,
    rowsCounted,
    excluded,
    casesRead,
    neighbours,
    onOwnDocument,
    rowsOnOwnDocument: rows.filter((row) => leaking.has(row.caseId)).length,
  };
}

/**
 * The two kinds of error apart, over the cases the arm reached a verdict on. A disagreement
 * where either side chose some other option is neither kind and is counted as that.
 */
export function errorSplitOf(cases: readonly CaseReading[]): ErrorSplit {
  const decided = cases.filter((one) => one.verdict !== null);
  let falseApprovals = 0;
  let falseRequests = 0;
  let other = 0;
  for (const one of decided) {
    if (one.agreed) continue;
    const arm = verdictOf(one.verdict ?? [], one.row.options);
    const person = one.row.verdict;
    if (person === "request_changes" && arm === "approve") falseApprovals += 1;
    else if (person === "approve" && arm === "request_changes") falseRequests += 1;
    else other += 1;
  }
  const counted = decided.length;
  return {
    decided: counted,
    falseApprovals: { count: falseApprovals, share: counted === 0 ? 0 : falseApprovals / counted },
    falseRequests: { count: falseRequests, share: counted === 0 ? 0 : falseRequests / counted },
    other,
  };
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
  droppedFromFit: ReadonlySet<string>,
  missing: string | null
): KindAnalysis {
  const report = sliceCases(arm.cases, "report", kind);
  const drawnFit = sliceCases(arm.cases, "fit", kind);
  // A fit case of an issue that also reaches the report or the anchor slice is dropped here and
  // nowhere else: the threshold is all this slice decides, so dropping it costs no reading.
  const fit = drawnFit.filter((one) => !droppedFromFit.has(one.caseId));
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
  // The cases the threshold decides, as readings rather than judgements: the two kinds of error
  // are read off the person's verdict and the arm's labels, which a judgement does not carry.
  const decidedCases =
    threshold === null
      ? []
      : report.filter((one) => one.verdict !== null && one.confidence >= threshold);
  return {
    kind,
    cases: report.length,
    sliceCases: sliceSizes?.report ?? null,
    undecided: report.filter((one) => one.verdict === null).length,
    agreement,
    fit: {
      cases: fit.length,
      decided: fitJudged.length,
      dropped: drawnFit.length - fit.length,
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
      adjustedBound: bounded
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
    errors:
      kind === "approval"
        ? { report: errorSplitOf(report), atThreshold: errorSplitOf(decidedCases) }
        : null,
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
  sliceSizes: Record<string, number> | null,
  droppedFromFit: ReadonlySet<string>
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
    approval: kindAnalysisOf(arm, "approval", cutFree, sliceSizes, droppedFromFit, noBound),
    question: kindAnalysisOf(arm, "question", cutFree, sliceSizes, droppedFromFit, noBound),
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

/**
 * What the rows say about the invocation `round.json` records: how many of the judgements it
 * planned the rows answer, how many are left of them, and the arms it did not name whose rows
 * the directory holds anyway. A round is run one arms file at a time, and each invocation
 * writes the summary over its own arms, so the last summary of a round names the last arms
 * file alone while `rows.jsonl` holds every invocation's rows: a report that read the two
 * against each other would count rows of arms that summary never planned.
 */
function invocationOf(
  summary: RoundSummary | null,
  rows: readonly JudgementRow[]
): { answered: number; left: number; armsNotNamed: string[] } | null {
  if (summary === null) return null;
  const named = new Set(summary.arms);
  const answered = rows.filter((row) => named.has(row.arm) && row.fault === null).length;
  return {
    answered,
    left: Math.max(0, summary.judgements.planned - answered),
    armsNotNamed: [
      ...new Set(rows.filter((row) => !named.has(row.arm)).map((row) => row.arm)),
    ].sort(),
  };
}

/**
 * `wanted` of `items`, drawn one at a time from `next` and never one of them twice. The stream
 * is the caller's, so two draws in a row take the one seeded walk forward rather than restart
 * it, and the same seed over the same lists draws the same items every time.
 */
function pickSome<T>(items: readonly T[], wanted: number, next: () => number): T[] {
  const left = [...items];
  const picked: T[] = [];
  for (let index = 0; index < wanted && left.length > 0; index += 1) {
    const at = Math.min(left.length - 1, Math.floor(next() * left.length));
    picked.push(left[at] as T);
    left.splice(at, 1);
  }
  return picked;
}

/** One arm's agreement over the approval cases of the report slice, which the table is read in. */
function reportAgreementOf(arm: ArmReading): Reading {
  const report = sliceCases(arm.cases, "report", "approval");
  return readingOf(report.filter((one) => one.agreed).length, report.length);
}

/**
 * The generative arm the confidence table is read on: the one the caller named, or, where it
 * named none, the generative arm that agrees most over the approval cases of the report slice.
 *
 * The best arm is the default, and not the first by name or by case count, because of what the
 * table is for: a reader looks at it to see whether disagreement falls as the counted
 * confidence rises. Read on an arm that agrees barely more than chance, it answers that
 * question about an arm nobody would ship, and such an arm's counted confidence barely varies,
 * so there is no fall to see either way. The arm that agrees most is the one a reader would
 * act on, so it is the one the table speaks about unless the caller asks for another.
 */
function confidenceArmOf(arms: readonly ArmReading[], named: string | null): ArmReading | null {
  const generative = arms.filter((arm) => arm.endpoint === "claude");
  if (named !== null) {
    const found = generative.find((arm) => arm.name === named);
    if (found === undefined) {
      throw new UsageError(
        `--confidence-arm ${named} names no generative arm of this round. The generative arms ` +
          `are ${generative.length === 0 ? "none" : generative.map((arm) => arm.name).join(", ")}`
      );
    }
    return found;
  }
  // The arms arrive sorted by name, so a tie in agreement keeps the first of them by name.
  let best: { arm: ArmReading; agreement: number } | null = null;
  for (const arm of generative) {
    const { share } = reportAgreementOf(arm);
    if (best === null || share > best.agreement) best = { arm, agreement: share };
  }
  return best === null ? null : best.arm;
}

/** Jev's own stated probability on one case, binned, since it takes a value of its own per case. */
function probabilityBin(value: number): string {
  const bin = Math.round(value / PROBABILITY_BIN) * PROBABILITY_BIN;
  return bin.toFixed(1);
}

/**
 * The seed the 30 cases are drawn under where the caller named none: the first four bytes of
 * the SHA-256 of the arm's name and the case ids of the slice they are drawn from, in the
 * order the arm judged them. It is a seed of the round and not of the clock, so every run over
 * one round and one arm draws the one set of 30, and a reader who re-drives the table under
 * the printed seed is given that same set back.
 */
function seedOfSlice(arm: string, cases: readonly CaseReading[]): number {
  return createHash("sha256")
    .update(`${arm}\n${cases.map((one) => one.caseId).join("\n")}`, "utf8")
    .digest()
    .readUInt32BE(0);
}

/** One value of a counted-confidence table and the cases that took it, which is what it counts. */
interface ConfidenceBin {
  name: string;
  cases: CaseReading[];
}

/** The values these cases' counted confidence took, lowest first, with the cases at each. */
function confidenceBins(cases: readonly CaseReading[]): ConfidenceBin[] {
  const decided = cases.filter((one) => one.verdict !== null);
  return [...new Set(decided.map((one) => one.confidence))]
    .sort((one, two) => one - two)
    .map((value) => ({
      name: value.toFixed(2),
      cases: decided.filter((one) => one.confidence === value),
    }));
}

/**
 * One line per value, in that order, under `binReport`'s floor rule: a value too few cases took
 * carries its count and its observed agreement and no bound. A value is never dropped for
 * carrying no bound, since a table a reader cannot see the whole of is a table they read wrong.
 */
function binLinesOf(bins: readonly ConfidenceBin[]): BinLine[] {
  return binReport(bins.map((bin) => ({ name: bin.name, cases: judgementsOf(bin.cases) })));
}

/**
 * Each value of such a table beside the always-approve baseline on the very cases that took it:
 * what an arm that approved all of them would have scored. The lines are the table's own, so
 * the bound a value carries here is the bound the table printed for it.
 */
function sliceBaselines(
  bins: readonly ConfidenceBin[],
  lines: readonly BinLine[]
): SliceBaseline[] {
  return bins.map((bin, at) => {
    const line = lines[at];
    return {
      name: bin.name,
      agreement: line?.observedAgreement ?? 0,
      baseline: alwaysApproveBaseline(bin.cases.map((one) => ({ verdict: one.row.verdict }))),
      bounded: line !== undefined && line.upperBound !== null,
    };
  });
}

/**
 * The confidence table: one generative arm's counted confidence over 30 cases the person
 * decided both ways, against the cases at each value and the disagreements at it; the same
 * arm's counted confidence over the whole report slice those 30 were drawn from, so a sample
 * showing one value is read against the slice rather than taken for the arm's behaviour; and
 * beside them the probability the Jev arm of the same recipe stated over the 30, read as its
 * own arm and never mixed in. The arm is the one `named`, or the best-agreeing one where that
 * is null. The 30 are drawn under `seed`, or under a seed of the round where that is null, and
 * the table carries whichever seed drew them.
 */
export function confidenceTableOf(
  arms: readonly ArmReading[],
  wanted: readonly string[] | null,
  seed: number | null,
  named: string | null
): ConfidenceTable {
  const identicalArms = arms
    .filter((arm) => {
      const values = new Set(
        arm.cases.filter((one) => one.verdict !== null).map((one) => one.confidence)
      );
      return arm.cases.length > 0 && values.size === 1;
    })
    .map((arm) => arm.name);
  const armChoice = named === null ? "best" : "named";
  const arm = confidenceArmOf(arms, named);
  if (arm === null) {
    return {
      arm: null,
      recipe: null,
      armChoice,
      armAgreement: null,
      reason: "no generative arm has written a row yet, so no counted confidence can be read",
      caseIds: [],
      seed,
      seedChoice: seed === null ? null : "named",
      approved: 0,
      requestedChanges: 0,
      lines: [],
      sliceLines: [],
      sliceDecided: 0,
      sliceBaseline: { approvals: 0, counted: 0, share: 0 },
      sliceBaselines: [],
      jevArm: null,
      jevLines: [],
      identicalArms,
    };
  }
  const report = sliceCases(arm.cases, "report", "approval");
  // No --confidence-seed names one, so the round names it: the same round draws the same 30
  // every run, and a reader re-drives the table from the seed the table prints.
  const drawnUnder = seed ?? seedOfSlice(arm.name, report);
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
    // Both halves are drawn from the one stream of that seed, in this order.
    const next = randomStream(drawnUnder);
    const picked = new Set([...pickSome(approve, half, next), ...pickSome(changes, half, next)]);
    chosen = report.filter((one) => picked.has(one.caseId));
  }
  // The Jev half is read on the Jev arm of the same recipe, so that the two halves differ in
  // the model and in nothing else, and the reader compares the models and not the recipes.
  const jev = arms.find((one) => one.endpoint === "jev" && one.recipe === arm.recipe) ?? null;
  const jevCases =
    jev === null
      ? []
      : jev.cases.filter(
          (one) =>
            chosen.some((other) => other.caseId === one.caseId) && one.statedProbability !== null
        );
  const sliceBins = confidenceBins(report);
  const decided = report.filter((one) => one.verdict !== null);
  const sliceLines = binLinesOf(sliceBins);
  const jevBins = [
    ...new Set(jevCases.map((one) => probabilityBin(one.statedProbability ?? 0))),
  ].sort();
  return {
    arm: arm.name,
    recipe: arm.recipe,
    armChoice,
    armAgreement: reportAgreementOf(arm),
    reason:
      chosen.length < CONFIDENCE_TABLE_CASES
        ? `only ${chosen.length} of the ${CONFIDENCE_TABLE_CASES} cases this table wants are ` +
          `scored by ${arm.name} so far, so the table is read over those`
        : null,
    caseIds: chosen.map((one) => one.caseId),
    seed: wanted === null ? drawnUnder : null,
    seedChoice: wanted !== null ? null : seed === null ? "derived" : "named",
    approved: chosen.filter((one) => one.row.verdict === "approve").length,
    requestedChanges: chosen.filter((one) => one.row.verdict === "request_changes").length,
    lines: binLinesOf(confidenceBins(chosen)),
    sliceLines,
    sliceDecided: decided.length,
    sliceBaseline: alwaysApproveBaseline(decided.map((one) => ({ verdict: one.row.verdict }))),
    sliceBaselines: sliceBaselines(sliceBins, sliceLines),
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
  /** The case ids of each drawn slice, and null where the draw could not be read. */
  slices: Record<string, string[]> | null;
  /** What `draw.json` says the cases were grouped by, where it says. */
  groupKey: string | null;
  /** The neighbours the round's evidence cache recorded, one entry per case it holds. */
  evidence: readonly CaseNeighbours[];
  cut: string | null;
  confidenceTable: boolean;
  /** Which generative arm that table is read on, and null for the best-agreeing one. */
  confidenceArm: string | null;
  /** The case ids the confidence table reads, where a file named them. */
  wantedCases: readonly string[] | null;
  /** The seed those 30 are drawn under, and null to draw them under a seed of the round. */
  seed: number | null;
}

/** The whole report, computed from the rows and the corpus and reading nothing else. */
export function analyseRound(input: AnalysisInput): Analysis {
  const { arms: armReadings, unknownCases } = readRound(input.rows, input.corpus.byCaseId);
  const cutFree = cutFreeCasesOf(armReadings);
  const casesWithRows = new Set(input.rows.map((row) => row.caseId));
  const leakage = crossingsOf(input.slices, input.corpus.byCaseId, input.groupKey);
  const droppedFromFit = new Set(leakage.droppedFromFit);
  const manifests = manifestsOf(input.slices, leakage.droppedFromFit);
  const retrieval = retrievalProofOf(input.rows, input.evidence, input.corpus.byCaseId);
  const sliceSizes =
    input.slices === null
      ? null
      : Object.fromEntries(Object.entries(input.slices).map(([slice, ids]) => [slice, ids.length]));
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
    (sliceSizes === null
      ? casesWithRows.size
      : (sliceSizes.fit ?? 0) + (sliceSizes.report ?? 0) + (sliceSizes.anchor ?? 0));
  const planned = casesPlanned * judgementsPerCase;
  const faults = faultsOf(input.rows);
  const arms = armReadings.map((arm) =>
    armAnalysisOf(arm, planned, cutFree, sliceSizes, droppedFromFit)
  );
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
              arms: [...input.summary.arms],
            },
      invocation: invocationOf(input.summary, input.rows),
      rowsRead: input.rowsRead,
      rowsUnreadable: input.rowsUnreadable,
      rowsOfUnknownCase: unknownCases,
      judgementsPerCase,
      casesWithRows: casesWithRows.size,
      casesBySlice,
      sliceSizes,
      arms: arms.map((arm) => arm.arm),
      models: [...new Set(arms.map((arm) => arm.model))].sort(),
      faultsByKind: faults.byKind,
      faultsByArm: faults.byArm,
      armsWithoutBound: arms.filter((arm) => !arm.bounded).map((arm) => arm.arm),
    },
    arms,
    leakage,
    manifests,
    retrieval,
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
      ? confidenceTableOf(armReadings, input.wantedCases, input.seed, input.confidenceArm)
      : null,
  };
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
    const summary = round.summary;
    const seen = round.invocation ?? { answered: 0, left: 0, armsNotNamed: [] };
    lines.push(
      `The round's last invocation ran from ${summary.startedAt} to ${summary.finishedAt} over ` +
        `the ${count(summary.arms.length)} arms it named, and ` +
        (seen.left === 0
          ? `every one of the ${count(summary.planned)} judgements it planned is answered in ` +
            "the rows."
          : `${count(seen.answered)} of the ${count(summary.planned)} judgements it planned ` +
            `are answered in the rows and ${count(seen.left)} are left.`)
    );
    if (seen.armsNotNamed.length > 0) {
      lines.push("");
      lines.push(
        `The directory holds rows of ${count(seen.armsNotNamed.length)} further arms that ` +
          `invocation did not name: ${seen.armsNotNamed.join(", ")}. A round run one arms file ` +
          "at a time writes one round.json per invocation, over the arms that invocation named, " +
          "so the file left behind is the last one's and the rows are every one's. The two " +
          "counts above are read over the arms that file names, and every count below is read " +
          "over all the rows."
      );
    }
    if (summary.finished !== (seen.left === 0)) {
      lines.push("");
      lines.push(
        "`round.json` says of itself that the round " +
          `${summary.finished ? "finished" : "did not finish"}, with ` +
          `${count(summary.answered)} answered against ${count(summary.planned)} planned: a ` +
          "summary written before those two were counted over one set read every row of the " +
          "directory against the arms of its own invocation, which is why the counts above are " +
          "read from the rows instead."
      );
    }
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

/** Where the drawn slices cross on one issue, what that dropped, and what it did not. */
function crossingLines(analysis: Analysis): string[] {
  const { leakage } = analysis;
  const lines = ["", "## Where the slices cross, and what was dropped", ""];
  if (!leakage.read) {
    lines.push(
      "The corpus directory carries no readable draw, so no crossing could be looked for and " +
        "no case was dropped."
    );
    return lines;
  }
  lines.push(
    (leakage.groupKey === null
      ? "The draw records no group key, so it grouped an approval case by its lead document " +
        "and a question by its issue, and two cases of one issue could reach two slices. "
      : `The draw grouped its cases by the ${leakage.groupKey}. `) +
      "An issue below holds cases in more than one of the fit, report and anchor slices."
  );
  lines.push("");
  if (leakage.crossings.length === 0) {
    lines.push("No issue holds cases in more than one of those slices.");
  } else {
    lines.push(
      ...table(
        ["issue", "the slices it is in", "case", "its slice", "what was done"],
        leakage.crossings.flatMap((crossing) =>
          crossing.cases.map((one) => [
            crossing.issue,
            crossing.slices.join(" and "),
            one.caseId,
            `${one.slice} (${one.kind})`,
            one.dropped ? "dropped from the fit slice" : "kept, and every number reads it",
          ])
        )
      )
    );
    lines.push("");
    lines.push(
      `${count(leakage.crossings.length)} issues cross, and ` +
        `${count(leakage.droppedFromFit.length)} cases were dropped from the fit slice for it. ` +
        "A case is dropped from the fit slice and from nothing else: that slice only chooses a " +
        "threshold and carries no reported number, while the report and anchor slices carry " +
        "every number."
    );
    for (const crossing of leakage.crossings) {
      if (crossing.droppedFromFit.length === 0) continue;
      lines.push(
        `- ${crossing.issue}: ${crossing.droppedFromFit.join(", ")} left the fit slice, since ` +
          `that issue also holds a case in the ` +
          `${crossing.slices.filter((slice) => slice !== "fit").join(" and ")} slice.`
      );
    }
  }
  if (leakage.shortfalls.length > 0) {
    lines.push("");
    lines.push(
      `${count(leakage.shortfalls.length)} of those crossings is between the report slice and ` +
        `the anchor slice: ${leakage.shortfalls.join(", ")}. Nothing is dropped for it, since ` +
        "taking a case out of either of those resizes a slice every number is read from. It " +
        "stands as a shortfall of this draw, and a draw grouping by the issue does not make it."
    );
  }
  lines.push("");
  lines.push(
    ...table(
      ["the fit slice", "as drawn", "kept", "its target"],
      KINDS.map((kind) => [
        `${kind} cases`,
        count(leakage.fit[kind].drawn),
        count(leakage.fit[kind].kept),
        count(leakage.fit[kind].target),
      ])
    )
  );
  return lines;
}

/** One manifest per slice: what the slice is for, what it holds and the hash of its ids. */
function manifestLines(analysis: Analysis): string[] {
  const lines = ["", "## The manifest of each slice", ""];
  if (analysis.manifests.length === 0) {
    lines.push("No draw could be read, so no manifest was written.");
    return lines;
  }
  lines.push(
    "One manifest per slice is written beside this report, as `manifest-<slice>.json`. Each " +
      "holds the role in words, the case ids in order, the count and the hash below, which is " +
      `${HASHED_AS}.`
  );
  lines.push("");
  lines.push(
    ...table(
      ["slice", "what it is for", "cases", "dropped", "sha256 of its ids"],
      analysis.manifests.map((manifest) => [
        manifest.slice,
        manifest.role,
        count(manifest.count),
        count(manifest.dropped.length),
        manifest.sha256,
      ])
    )
  );
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
  const { retrieval } = analysis;
  lines.push("");
  lines.push("### Retrieval returned no neighbour on the judged case's own document");
  lines.push("");
  lines.push(
    `Over the ${count(retrieval.cases)} scored cases, retrieval dropped ` +
      `${count(retrieval.excluded)} neighbours for sitting on the judged case's own document ` +
      `or issue (the \`excluded\` field of ${count(retrieval.rowsCounted)} rows of ` +
      "`rows.jsonl`), and " +
      `${count(retrieval.rowsOnOwnDocument)} rows carry a neighbour naming the judged case's ` +
      "own document at any version (the `nearest.neighbours` field of " +
      `${count(retrieval.casesRead)} evidence cache entries, ` +
      `${count(retrieval.neighbours)} neighbours in all, read against the ` +
      "`approval.documents` field of the corpus). The second count is zero where retrieval " +
      "held, and the cases are named where it did not."
  );
  if (retrieval.onOwnDocument.length > 0) {
    lines.push("");
    lines.push(
      ...table(
        ["case", "the neighbour returned for it", "the document both name"],
        retrieval.onOwnDocument.map((one) => [one.caseId, one.neighbour, one.artifactId])
      )
    );
  }
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
  // Over approval cases the two kinds of error are printed apart, here and in the table of
  // their own below: one of them lets through a change the person would have stopped.
  const apart = kind === "approval";
  lines.push(
    ...table(
      [
        "arm",
        "fit cases",
        "threshold",
        "decided",
        "coverage",
        "disagreements",
        ...(apart ? ["false approvals", "false requests for changes"] : []),
        "report bound",
      ],
      analysis.arms.map((arm) => {
        const one = of(arm);
        const errors = one.errors?.atThreshold ?? null;
        return [
          arm.arm,
          `${count(one.fit.decided)} decided of ${count(one.fit.cases)}` +
            (one.fit.dropped > 0 ? `, ${count(one.fit.dropped)} dropped for a crossing` : ""),
          one.fit.threshold === null ? "none holds the rate" : num(one.fit.threshold, 2),
          count(one.atThreshold.decided),
          `${num(one.atThreshold.coverage)} of ${count(one.cases)}`,
          count(one.atThreshold.disagreements),
          ...(apart && errors !== null
            ? [
                `${count(errors.falseApprovals.count)} (${num(errors.falseApprovals.share)})`,
                `${count(errors.falseRequests.count)} (${num(errors.falseRequests.share)})`,
              ]
            : apart
              ? ["no count", "no count"]
              : []),
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
  if (apart) {
    lines.push("");
    lines.push("### False approvals and false requests for changes, apart");
    lines.push("");
    lines.push(
      "These two are not one error: only a false approval lets through a change the person " +
        "would have stopped, and the rate the person set is about that one. The single " +
        "disagreement rate stands beside them, since the bound is computed on it. Each share " +
        "is of the cases the arm reached a verdict on."
    );
    lines.push("");
    lines.push(
      ...table(
        [
          "arm",
          "decided",
          "false approvals",
          "false requests for changes",
          "neither kind",
          "disagreements in all",
        ],
        analysis.arms.map((arm) => {
          const errors = of(arm).errors?.report ?? null;
          if (errors === null) return [arm.arm, "no count", "no count", "no count", "0", "0"];
          const all = errors.falseApprovals.count + errors.falseRequests.count + errors.other;
          return [
            arm.arm,
            count(errors.decided),
            `${count(errors.falseApprovals.count)} (${num(errors.falseApprovals.share)})`,
            `${count(errors.falseRequests.count)} (${num(errors.falseRequests.share)})`,
            count(errors.other),
            `${count(all)} (${num(errors.decided === 0 ? 0 : all / errors.decided)})`,
          ];
        })
      )
    );
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
    (arm) => arm.confidenceValues.length === 1 && arm.approval.cases + arm.question.cases > 0
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

/** One binned readout as a table, under the name its first column takes. */
function binTable(first: string, lines: readonly BinLine[]): string[] {
  return table(
    [first, "cases", "disagreements", "agreement", "bound"],
    lines.map((line) => [
      line.name,
      count(line.decided),
      count(line.disagreements),
      num(line.observedAgreement),
      line.upperBound === null
        ? `no bound at this count (${line.decided} decided, ${MINIMUM_DECIDED} needed)`
        : `at most ${num(line.upperBound)}`,
    ])
  );
}

/**
 * What an arm that approved every case would have scored on the same cases: over the slice as a
 * whole, which is the number the table is read against, and over the very cases of each value.
 * An agreement read alone is a number a reader takes for a finding: 0.784 is a high number until
 * it is set beside the 0.792 an arm that read nothing would have scored on those same cases, and
 * then it is no finding at all. Every value is printed, the ones carrying no bound named as
 * carrying none: a value this paragraph left out is a value a reader cannot see is there, and a
 * table that prints one value is a result and not an error to be filtered away.
 */
function baselineLines(held: ConfidenceTable): string[] {
  if (held.sliceBaselines.length === 0) {
    return [
      "This arm decided no approval case of the report slice, so there is no always-approve " +
        "baseline to read the table against.",
    ];
  }
  const bounded = held.sliceBaselines.filter((one) => one.bounded);
  const slice = held.sliceBaseline;
  const says = bounded
    .map(
      (one) =>
        `${one.name} carries agreement ${num(one.agreement)} over ` +
        `${count(one.baseline.counted)} cases, a gain of ` +
        `${signed(one.agreement - slice.share)} on the baseline`
    )
    .join("; ");
  const lines = [
    "Beside the table, the always-approve baseline on those same cases: an arm that approved " +
      `every one of the ${count(slice.counted)} approval cases of the report slice this arm ` +
      `decided would have scored ${num(slice.share)} ` +
      `(${count(slice.approvals)} of ${count(slice.counted)} approved). An agreement is read ` +
      "against that number and never alone: a reader who sees an agreement by itself sees a " +
      "high number, and a reader who sees it against the baseline sees the finding. " +
      (bounded.length === 0
        ? "No value of this table carries a bound, so none of them is a value to act on."
        : held.sliceBaselines.length === 1
          ? `That one value carries a bound: ${says}.`
          : `${count(bounded.length)} of the ${count(held.sliceBaselines.length)} values ` +
            `${bounded.length === 1 ? "carries a bound" : "carry bounds"}: ${says}.`) +
      (held.sliceBaselines.length === 1
        ? " The slice took one value, so this table has one row and there is no threshold in " +
          "it to select on."
        : ""),
    "",
    "Each value against the baseline on the very cases that took it:",
    "",
  ];
  for (const one of held.sliceBaselines) {
    const counted = `${count(one.baseline.counted)} ${one.baseline.counted === 1 ? "case" : "cases"}`;
    lines.push(
      `    counted confidence ${one.name}: the arm agrees ${num(one.agreement)} over ` +
        `${counted}, and always-approve scores ${num(one.baseline.share)} over those same ` +
        `cases (${count(one.baseline.approvals)} approved), a gain of ` +
        `${signed(one.agreement - one.baseline.share)}` +
        (one.bounded
          ? "."
          : `, at a count that carries no bound (${count(one.baseline.counted)} decided, ` +
            `${MINIMUM_DECIDED} needed).`)
    );
  }
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
  const recipe = held.recipe ?? "unnamed";
  const agreement = held.armAgreement;
  lines.push(
    `The generative arm is ${held.arm}, of the ${recipe} recipe, ` +
      (held.armChoice === "named"
        ? "named by --confidence-arm."
        : "chosen as the generative arm that agrees most over the approval cases of the " +
          "report slice, since a table read on an arm nobody would ship says nothing about " +
          "the arms a reader would.") +
      (agreement === null
        ? ""
        : ` That arm agrees ${num(agreement.share)} over the ${count(agreement.total)} ` +
          "approval cases of the report slice, which is the light this table is read in.")
  );
  lines.push("");
  lines.push(
    `The table is read over ${count(held.caseIds.length)} report-slice cases: ` +
      `${count(held.approved)} the person approved and ${count(held.requestedChanges)} the ` +
      "person asked for changes on." +
      (held.seed === null
        ? " The cases were named by --cases."
        : ` They were drawn under seed ${held.seed}, ` +
          (held.seedChoice === "named"
            ? "which --confidence-seed named."
            : "which no --confidence-seed named and which is derived from the arm and the " +
              "case ids of the slice they were drawn from.") +
          ` Run this command again over this round with --confidence-seed ${held.seed} to ` +
          "read the same cases.")
  );
  if (held.reason !== null) {
    lines.push("");
    lines.push(held.reason);
  }
  lines.push("");
  lines.push(...binTable("counted confidence", held.lines));
  lines.push("");
  lines.push(
    `The same arm over the whole slice those ${count(held.caseIds.length)} cases were drawn ` +
      `from: each counted-confidence value the ${count(held.sliceDecided)} approval cases of ` +
      "the report slice this arm decided took, with the cases and the disagreements at it. " +
      "That is the distribution the cases above are a sample of: a sample of 30 can take one " +
      "value where the slice it came from takes several, and the slice is what the arm's " +
      "counted confidence does."
  );
  lines.push("");
  lines.push(...binTable("counted confidence", held.sliceLines));
  lines.push("");
  lines.push(...baselineLines(held));
  lines.push("");
  if (held.jevArm === null) {
    lines.push(
      `No Jev arm of the ${recipe} recipe has written a row for these cases, so there is no ` +
        "stated probability to read beside the counted confidence above."
    );
  } else {
    lines.push(
      `Jev's own probability over the same cases, as its own arm, which it is and is never ` +
        `mixed into the counted confidence. The arm is ${held.jevArm}, the Jev arm of the ` +
        `${recipe} recipe, which is the recipe the arm above carries too: the two halves of ` +
        "this table are the same information recipe on two models, so what differs between " +
        "them is the model. The value is its stated probability of what it chose, to the " +
        "nearest tenth."
    );
    lines.push("");
    lines.push(...binTable("Jev's own probability", held.jevLines));
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
    ...crossingLines(analysis),
    ...manifestLines(analysis),
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

/** The case ids of each slice, from the draw the corpus directory carries, where it does. */
async function readSlices(corpus: string): Promise<Record<string, string[]> | null> {
  const path = join(corpus, "slices", "slices.json");
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch {
    return null;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new CorpusError(`${path} is not JSON`);
  }
  const record = recordOf(parsed);
  if (record === null) return null;
  const slices: Record<string, string[]> = {};
  for (const [slice, ids] of Object.entries(record)) {
    if (Array.isArray(ids))
      slices[slice] = ids.filter((one): one is string => typeof one === "string");
  }
  return slices;
}

/**
 * The draw's own facts the report reads: the cut the post-cut class table is read over, and the
 * key the cases were grouped by, which a draw made before that key was recorded does not carry.
 */
async function readDrawFacts(
  corpus: string
): Promise<{ cut: string | null; groupKey: string | null }> {
  const draw = await readDraw(corpusDrawPath(corpus));
  return { cut: cutOf(draw), groupKey: groupKeyOf(draw) };
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
    let parsed: unknown;
    try {
      parsed = JSON.parse(trimmed);
    } catch {
      throw new UsageError(`--cases names ${path}, which is not JSON`);
    }
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
    "confidence-arm"?: string;
    cases?: string;
    "confidence-seed"?: string;
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
        "confidence-arm": { type: "string" },
        cases: { type: "string" },
        "confidence-seed": { type: "string" },
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
  const confidenceTable = values["confidence-table"] === true;
  const confidenceArm = values["confidence-arm"]?.trim();
  if (confidenceArm !== undefined && !confidenceTable) {
    throw new UsageError(
      "--confidence-arm names the arm --confidence-table is read on, and that table was not " +
        `asked for\n\n${USAGE}`
    );
  }
  const cases = values.cases?.trim();
  if (cases !== undefined && !confidenceTable) {
    throw new UsageError(
      "--cases names the cases --confidence-table is read over, and that table was not " +
        `asked for\n\n${USAGE}`
    );
  }
  const named = values["confidence-seed"]?.trim();
  if (named !== undefined && !confidenceTable) {
    throw new UsageError(
      "--confidence-seed names the seed --confidence-table draws its cases under, and that " +
        `table was not asked for\n\n${USAGE}`
    );
  }
  if (named !== undefined && cases !== undefined) {
    throw new UsageError(
      "--confidence-seed draws the cases of the confidence table and --cases names them, so " +
        `only one of the two can be given\n\n${USAGE}`
    );
  }
  const confidenceSeed = named === undefined ? null : Number(named);
  if (confidenceSeed !== null && !Number.isInteger(confidenceSeed)) {
    throw new UsageError(`--confidence-seed takes a whole number, not ${named}\n\n${USAGE}`);
  }
  return {
    corpus,
    round,
    out: values.out?.trim(),
    confidenceTable,
    confidenceArm: confidenceArm === undefined || confidenceArm === "" ? null : confidenceArm,
    cases,
    confidenceSeed,
    format,
  };
}

/**
 * The floor the report is read at: every number of it is a comparison across arms, and the
 * winner is chosen among them, so a round holding fewer than four arms or fewer than three
 * models answers none of the questions the report is written to answer. The report is printed
 * and written either way, since a reader of a short round still wants to see what it holds,
 * and the status says it is no report of a round: it names the arms and the models it had.
 */
function underFloor(analysis: Analysis): number {
  const { arms, models } = analysis.round;
  if (arms.length >= LEAST_ARMS && models.length >= LEAST_MODELS) return 0;
  console.error(
    `The report is read over at least ${LEAST_ARMS} arms and ${LEAST_MODELS} models, and this ` +
      `round has ${arms.length} ${arms.length === 1 ? "arm" : "arms"} ` +
      `(${arms.length === 0 ? "none" : arms.join(", ")}) and ${models.length} ` +
      `${models.length === 1 ? "model" : "models"} ` +
      `(${models.length === 0 ? "none" : models.join(", ")}).`
  );
  return EXIT_UNDER_FLOOR;
}

async function analyze(flags: Flags): Promise<number> {
  const corpus = await readCorpus(flags.corpus);
  const out = flags.out === undefined ? null : await outDirectory(flags.out);
  const { rows, read, unreadable } = await readRows(flags.round);
  const summary = await readSummary(flags.round);
  const slices = await readSlices(corpus.directory);
  const { cut, groupKey } = await readDrawFacts(corpus.directory);
  const evidence = await readEvidenceNeighbours(flags.round);
  const wantedCases = flags.cases === undefined ? null : await readCases(flags.cases);
  const seed = wantedCases === null ? flags.confidenceSeed : null;
  const analysis = analyseRound({
    corpus,
    roundDirectory: flags.round,
    rows,
    rowsRead: read,
    rowsUnreadable: unreadable,
    summary,
    slices,
    groupKey,
    evidence,
    cut,
    confidenceTable: flags.confidenceTable,
    confidenceArm: flags.confidenceArm,
    wantedCases,
    seed,
  });
  const text = renderReport(analysis).join("\n");
  console.log(flags.format === "json" ? JSON.stringify(analysis, null, 2) : text);
  if (out !== null) {
    await mkdir(out, { recursive: true });
    await writeFile(join(out, "report.md"), `${text}\n`, "utf8");
    await writeFile(join(out, "report.json"), `${JSON.stringify(analysis, null, 2)}\n`, "utf8");
    const ids = analysis.confidenceTable?.caseIds ?? [];
    if (analysis.confidenceTable !== null && analysis.confidenceTable.seed !== null) {
      await writeFile(join(out, "confidence-cases.txt"), `${ids.join("\n")}\n`, "utf8");
    }
    console.error(`written           ${join(out, "report.md")}`);
    console.error(`                  ${join(out, "report.json")}`);
    for (const manifest of analysis.manifests) {
      const path = join(out, `manifest-${manifest.slice}.json`);
      await writeFile(path, `${JSON.stringify(manifest, null, 2)}\n`, "utf8");
      console.error(`                  ${path}`);
    }
  }
  return underFloor(analysis);
}

/** The command: the flags, the report, and the exit status it takes. */
export async function main(argv: readonly string[]): Promise<number> {
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
    if (error instanceof CannotRunError) {
      console.error(`${error.name}: ${error.message}`);
      return 2;
    }
    throw error;
  }
}

// Only when run: a test that imports the readings must not run a command.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
