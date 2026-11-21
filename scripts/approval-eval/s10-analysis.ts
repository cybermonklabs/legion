import type { CaseRow, CaseVerdict } from "./build-corpus";
import { CorpusError, recordOf, stringOf } from "./corpus-io";
import type { BuildChain, ClaimDraw, FinalTestTree } from "./draw-io";
import {
  type CandidateCounts,
  DROP_SEEN,
  type DropSeen,
  type FirstAnswer,
  type InventoryLine,
  type InventoryRequest,
  type Settlement,
} from "./s10-inputs-io";
import {
  COUNTING_RULE,
  DEVIATION_KINDS,
  HOLD_VERDICT,
  JUDGES,
  type JudgedOutcome,
  type JudgedStage,
  RESULTS_FILE,
  RULE_VERSIONS,
  type RuleVersion,
  recordedAnswersOf,
  reportedOutcomesOf,
  SANDBOX_DIRECTORY,
  type ToolCallDeviation,
  UNJUDGED,
} from "./s10-judged-io";
import { type SendBackFigures, sendBackFiguresOf } from "./send-back";
import { clopperPearsonUpper, DEFAULT_CONFIDENCE } from "./stats";

/**
 * The claim's analysis of its one bundle, S10: the fifth command of point 13 of the registered
 * claim text, `analyze.ts --claim-s10`, by its points 4, 6, 7, 10 and 12 (point 14 (d)). It reads
 * the claim run's build, draw, inputs and judged stages, and Dispatch's event log through the
 * build's pin (`s10-analysis-io.ts`), and writes one file, its own stage record
 * (`s10-analysis.json`), which holds every figure this module computes and the text prints
 * (`s10-analysis-text.ts`).
 *
 * **The admission and the chain (point 14 (e)).** It starts as the inputs stage starts
 * (`claimStartOf`): admitted under the claim run's registered read before any file
 * is opened, so a run under no read never meets a held case or prints one's id; then the build of
 * that claim run, and the draw of the build's bytes now. Each later stage is held to the one
 * before it, by its own parser and the sha256 its successor recorded, so no directory can be
 * swapped between stages: the judged stage (`judgedStageOf`, every file it names held to its
 * sha256) was written by this claim run under this read; the inputs stage it names has the bytes
 * of `s10-inputs.json` whose sha256 the judged stage recorded, is written, and names the same
 * claim run, build and read and the draw's two files by their bytes now; and its inventory has the
 * sha256 both records name. The log is walked through the build's pin and must read as many events
 * as the build read. The digest the record names is the one the admission admitted: taken right
 * after the admission and held to the registration under the read it found (`claimStartOf`), and
 * taken again once every stage is read, which must agree. The record names the claim run, the
 * read, the digest and the sha256 of each stage's record. `--claim-s10` and `--out` are paths
 * only: no placement of either is checked, since every byte the analysis reads is held to the
 * chain.
 *
 * **The inventory (point 6).** Every answered approval request among the draw's report cases is
 * one request, keyed by ask id, the rule the inputs stage wrote its inventory by (`inventoryOf`);
 * the inventory the stage wrote must be that one, request for request, with the same issue, tree
 * and count of cases. More than one corpus case of an ask is provenance of its one request; a case
 * named twice, an ask in two trees, a report case of no tree, an inventory line repeated by ask id
 * and two outcomes of one ask are each refused. Each request has one outcome: the judged stage's
 * own, or, where the stage has none, a `started` deviation for a request a run started and never
 * finished and an `unjudged` one for a request no run reached (`reportedOutcomesOf`). A request
 * the inputs stage made a deviation must have the stage's `inputs` deviation, or none yet
 * (`unjudged`); any other outcome of it is an `inputs` deviation the analysis finds. The question
 * cases of the eligible trees are no approval request: they are in no figure, and are counted.
 *
 * **His answer (point 7).** A request is compared with his answer as "His answer" of the live
 * agreement's registration places it: the first event after the ask's first opening that is his
 * `ask.answered` of the ask or, for an ask that names one document, his `artifact.approved` or
 * `artifact.changes_requested` on that document at the version the ask names. The rule is the
 * inputs stage's own (`AnswerLog`), over the one walk of the log through the pin both stages make
 * (`walkThroughPin`). Where the inputs stage placed his own answer, its inventory line records
 * the judged moment, the statement that stood then; this walk must place his own answer at that
 * statement itself, by its event id, type and time. Where it placed no answer of his, an answer
 * not his own, or his own answer with no statement before it, the line records no moment, and is
 * a deviation, and this walk must place the same. Two walks of one log place one answer at one
 * moment, so a line either way it does not, or a ready line with no moment, is refused. A
 * settlement's verdict is its kind and its pair the one document at that version; an
 * `ask.answered`'s verdict is read from the option he chose and its pairs from that event's
 * approval, both as the build's corpus records that event. A request whose answer is not his own,
 * or is none, is on a line the inputs stage made a deviation, whose `inputs` deviation (or
 * `unjudged`) it keeps; his own answer that the build holds no case of is a deviation (`answer`);
 * an outcome whose judged documents (`judgement.documents`) are not his answered pairs is a
 * deviation (`documents`).
 *
 * **The kept evidence (point 6).** Before an outcome counts, every file the judged record names
 * must be there with the sha256 it names (`judgedStageOf`), and a verdict's record must name each
 * judge's transcript and bundle and the answers: a request whose transcript is lost is a
 * `transcript` deviation, whose answers are lost an `answers` one, and whose other kept files (its
 * bundles, its calls, the sandbox's record, its `started.json`, an outcome kept pending while
 * its read-back did not run) are lost a `sandbox` one. A lost or changed `outcome.json` or
 * `outcomes.jsonl`, or any file outside every request's directory, refuses the stage: no outcome
 * can be read from it. A request with a pending outcome and no `outcome.json` is `started` until a
 * run commits that outcome.
 *
 * **The verdict, recomputed (points 2 and 6).** Each verdict outcome is recomputed by point 2's
 * rule from the five answers its sandbox result kept (`recordedAnswersOf`, `ruleVerdictOf`). A
 * fact-check hold is `never_decide:waiting-legion-root` with no judge. A recorded verdict unlike
 * the recomputation, a hold that records judges, or answers the rule cannot read, is a deviation
 * (`verdict`, found by the analysis), under each version of point 5's rule that gave a verdict.
 *
 * **What counts (points 8, 10 and 12).** A request is decided when its outcome is a valid
 * `approve` or `request_changes`, at the claim's threshold, 0.6: five answers give a majority of
 * at least three, so every such verdict carries at least 0.6. A fact-check hold and a valid
 * `never_decide:<kind>` are never-decides, not deviations. Every deviation, of the stage's 13
 * kinds, `unjudged`, or one the analysis finds, counts against the claim whatever the confidence:
 * for item 8's bound it is a decided request that disagrees with his answer, so its tree
 * disagrees, and that bound is the penalized bound. n is the eligible trees with a decided request
 * or a deviation, a tree disagrees when any of them does, the bound is the one-sided 90 percent
 * Clopper-Pearson upper bound over n, and the trees with neither are never-decide, their share
 * of the eligible trees apart. Beside it: the outcomes by kind; S10's decided share, the valid
 * `approve` and `request_changes` outcomes over every request of the inventory, against the
 * planning figure d = 0.85; and the send-back figures with the always-approve baseline over the
 * inventory, a never-decide or a deviation a request S10 did not decide, which each figure counts
 * as wrong and none gives a verdict. Version 6's tool rule counts (point 5); each version's tally
 * and penalized bound are recorded (`byRule`), and the requests whose deviation status changes
 * from one version to the next are listed with the calls the two versions refuse apart. Point 4's
 * disclosure is beside them: the inventory's candidates dropped for an event after the judged
 * moment, by where the event was seen, and that the ranks come from the run's start.
 *
 * **Its own `--out`.** A directory holding nothing takes the record, written beside its place and
 * moved there; one holding this run's record must hold it byte for byte, and keeps it. A file
 * written beside the place by this process (`s10-analysis.json.<pid>.partial`) whose bytes are
 * the start of this run's record is removed; every other file, another process's partial among
 * them, is refused.
 */

/** The version of the record's shape (`s10-analysis.json`). */
const ANALYSIS_VERSION = 1;

/**
 * The claim's threshold (point 8): a verdict of five answers has at least three behind it, so
 * 0.6 is the lowest counted confidence a decided verdict can have, and not one fitted to data.
 */
export const CLAIM_THRESHOLD = 0.6;

/** The rate item 8 holds the bound to: one disagreeing tree in twenty. */
const RATE = 0.05;

/** The planning figure d that point 10 reads the decided share against (the daily count's point 5). */
const PLANNING_D = 0.85;

/** The verdict of each settlement "His answer" counts for an ask of one document. */
const SETTLEMENT_VERDICTS: Readonly<Record<Settlement["type"], CaseVerdict>> = {
  "artifact.approved": "approve",
  "artifact.changes_requested": "request_changes",
};

/** How many of the five name a kind before the verdict is that never-decide (point 2's rule). */
const NAMED_TO_HOLD_BACK = 3;

/** The `never_decide` of an answer that names no kind. */
const NO_KIND = "none";

/**
 * Point 4's statement, which the record and the text carry beside the count of candidates dropped
 * for an event after the judged moment.
 */
export const RANKS_STATEMENT =
  "The ranks of the neighbours' search hits come from an index of the run's start, which can " +
  "order candidates by text or records written after the judged moment; the count of candidates " +
  "dropped for an event after that moment is a partial check of that, not a bound on it.";

/** A deviation's kind, and whether the stage or the analysis found it. */
export interface FoundDeviation {
  kind: string;
  foundBy: "stage" | "analysis";
}

/**
 * A request's outcome under one version of point 5's rule, once the analysis has read it: decided,
 * with S10's verdict and whether it disagrees with his answer; a never-decide, which is compared
 * with nothing; or a deviation, which always disagrees. Each carries the calls that version
 * refuses, as the stage recorded them.
 */
export type RuleReading =
  | {
      status: "decided";
      verdict: "approve" | "request_changes";
      deviation: null;
      disagrees: boolean;
      refusedCalls: ToolCallDeviation[];
    }
  | {
      status: "never-decide";
      verdict: string;
      deviation: null;
      disagrees: null;
      refusedCalls: ToolCallDeviation[];
    }
  | {
      status: "deviation";
      verdict: null;
      deviation: FoundDeviation;
      disagrees: true;
      refusedCalls: ToolCallDeviation[];
    };

/** One (document, version) pair, as his answer or S10's judgement names it. */
export interface DocumentPair {
  artifactId: string | null;
  version: number | null;
}

/**
 * His answer to a request (point 7): the first qualifying event, its kind, the case of it where it
 * is an `ask.answered` the build holds, its verdict and the pairs it answered; or, where it cannot
 * be compared, why (`problem`), with the verdict of the ask's first case in the corpus as the label
 * the send-back figures read, since every figure needs one and the request is a deviation anyway.
 */
export interface HisAnswer {
  eventId: number | null;
  type: string | null;
  caseId: string | null;
  verdict: CaseVerdict;
  documents: DocumentPair[];
  problem: string | null;
}

/** Point 4's disclosure: the inventory's candidate counts summed, and the statement beside them. */
export interface NeighbourDisclosure {
  /** The requests whose candidates were counted (a fact hold and an inputs deviation have none). */
  requests: number;
  askHits: number;
  candidates: number;
  held: number;
  reserveErrors: number;
  kept: number;
  /** Dropped for an event after the judged moment, by where the event was seen, and in all. */
  droppedLaterEvent: Record<DropSeen, number> & { total: number };
  /** Each end of a drop scan the requests' candidates were dropped by, lowest first. */
  dropScanEnds: number[];
  statement: string;
}

/** One request of the inventory as the analysis reads it. */
export interface RequestReading {
  askId: string;
  issueKey: string | null;
  group: string;
  /** The corpus's cases of the ask: his answer's case first where it is one, the rest provenance. */
  cases: string[];
  /** His answer (point 7), and the inputs stage's counts of the request's candidates (point 4). */
  answer: HisAnswer;
  candidates: CandidateCounts | null;
  /** The stage's outcome: its kind, its verdict and its deviation's kind. */
  stage: { outcome: JudgedOutcome["outcome"]; verdict: string | null; deviation: string | null };
  /** The documents S10 judged, and null where it made no judgement. */
  judgedDocuments: DocumentPair[] | null;
  /**
   * Point 2's verdict recomputed from the five kept answers (a hold's from its zero judges), with
   * the share of the five behind it; null where no version gave a verdict or a hold.
   */
  recomputed: { verdict: string; share: number } | null;
  /** Why the recorded verdict is unlike the recomputation, or why there is none; else null. */
  verdictProblem: string | null;
  /** Why the judged documents are not his answered pairs; else null. */
  documentsProblem: string | null;
  /** Each file of the request's kept evidence that is lost, and why; empty where none is. */
  lostEvidence: Array<{ file: string; problem: string; kind: string }>;
  /** The outcome under each version of point 5's rule; `COUNTING_RULE`'s counts. */
  byRule: Record<RuleVersion, RuleReading>;
}

/** The outcomes of the inventory, by kind, under one version of the rule. */
export interface OutcomeTally {
  requests: number;
  decided: { total: number; approve: number; requestChanges: number };
  /** The never-decides, by verdict: the fact-check hold's and each `never_decide:<kind>`. */
  neverDecides: { total: number; byVerdict: Record<string, number> };
  /** The deviations by kind, and of each kind those the analysis found. */
  deviations: {
    total: number;
    byKind: Record<string, number>;
    foundByAnalysis: Record<string, number>;
  };
}

/** One eligible tree under one version of the rule. */
export interface TreeReading {
  group: string;
  requests: number;
  decided: number;
  deviations: number;
  neverDecides: number;
  /** Its decided requests that disagree with his answer, and its deviations. */
  disagreeing: number;
}

/** Item 8's measure, penalized by point 10, under one version of the rule. */
export interface PenalizedMeasure {
  /** Every eligible tree the draw recorded. */
  trees: number;
  /** Of those, the trees holding no request of the inventory. */
  withoutRequest: number;
  /** n: the trees with a decided request or a deviation. */
  n: number;
  /** Of n, the trees in which one of them disagrees with his answer. */
  disagreeing: number;
  /** The trees outside n, their share of every eligible tree, and of those holding a request. */
  neverDecide: {
    count: number;
    share: number | null;
    withRequest: { count: number; share: number | null };
  };
  /** The one-sided 90 percent Clopper-Pearson upper bound on the disagreeing share of n; 1 at 0. */
  bound: number;
  /** The rate item 8 holds the bound to, one disagreeing tree in twenty, and whether it is met. */
  rate: number;
  withinRate: boolean;
  rows: TreeReading[];
}

/** One request whose deviation status one version's rule changes from the version before. */
export interface RuleChange {
  askId: string;
  before: string;
  after: string;
  /** The calls the earlier version refuses and the later does not, and the other way about. */
  refusedOnlyBefore: ToolCallDeviation[];
  refusedOnlyAfter: ToolCallDeviation[];
}

/** `s10-analysis.json`: the analysis's stage record, with every figure it prints. */
export interface S10Analysis {
  stage: "s10-analysis";
  version: number;
  claimRun: BuildChain;
  reference: string;
  runDigest: string;
  chain: {
    build: string;
    draw: { directory: string; drawSha256: string; slicesSha256: string };
    inputs: { directory: string; stageSha256: string; inventorySha256: string };
    judged: {
      directory: string;
      stageSha256: string;
      state: "started" | "written";
      stops: Array<{ at: string; reason: string }>;
    };
    /** The event log as the analysis walked it to place his answers: through the build's pin. */
    log: { throughEventId: number; eventsRead: number };
  };
  threshold: number;
  /** The least share of the five behind a decided verdict, null where none is decided. */
  leastDecidedShare: number | null;
  countingRule: RuleVersion;
  inventory: {
    requests: number;
    cases: number;
    trees: number;
    questionCases: number;
    questionAsks: number;
  };
  /** The counting rule's tally and penalized measure, which are `byRule`'s under it. */
  tally: OutcomeTally;
  measure: PenalizedMeasure;
  decidedShare: { decided: number; requests: number; share: number | null; planningD: number };
  /** Each version of point 5's rule, over the same outcomes: its tally and penalized measure. */
  byRule: Record<RuleVersion, { tally: OutcomeTally; measure: PenalizedMeasure }>;
  /** For each version from 4, the requests whose deviation status it changes from the one before. */
  ruleChanges: Array<{ from: RuleVersion; to: RuleVersion; requests: RuleChange[] }>;
  sendBack: SendBackFigures;
  /** Point 4's disclosure, from the inventory's candidate counts. */
  neighbours: NeighbourDisclosure;
  requests: RequestReading[];
}

/** One request of the inventory, its line, and his first answer as the walk of the log placed it. */
export interface Placed {
  request: InventoryRequest;
  line: InventoryLine;
  answer: FirstAnswer | null;
}

/**
 * What the analysis is computed from (`s10-analysis-io.ts` reads it): every stage read and held
 * to the chain, nothing more.
 */
export interface AnalysisInput {
  claimRun: BuildChain;
  reference: string;
  runDigest: string;
  build: string;
  draw: ClaimDraw;
  inputs: { directory: string; stageSha256: string; inventorySha256: string };
  judged: { directory: string; stage: JudgedStage };
  byCaseId: ReadonlyMap<string, CaseRow>;
  /** Each request with its inventory line and his first answer, in the inventory's order. */
  placed: readonly Placed[];
  /** The log his answers were placed in. */
  log: { throughEventId: number; eventsRead: number };
}

/** A pair as a key two lists of pairs are compared by. */
function pairKey(pair: DocumentPair): string {
  return JSON.stringify([pair.artifactId, pair.version]);
}

/**
 * His answer to one request (point 7), from the first answer `AnswerLog` placed (`placed`, its
 * event, whether it is his own, and the settlement it is): a settlement's verdict is its kind and
 * its pair the one it settled; his own `ask.answered`'s are its case's in the corpus. Where the
 * log gives no answer of his, or one not his own, there is a problem, but the request is on a line
 * the inputs stage made a deviation, whose `inputs` deviation (or `unjudged`) it keeps; his own
 * answer that the build holds no case of is the problem that makes a request a deviation
 * (`answer`).
 */
function answerOf(request: InventoryRequest, placed: FirstAnswer | null): HisAnswer {
  const ordered = [...request.rows].sort((one, other) => one.eventId - other.eventId);
  const label = ordered[0]?.verdict ?? null;
  if (label === null) {
    throw new CorpusError(
      `the request ${request.askId} has no approval case with a verdict: the send-back figures ` +
        "read every request's answer"
    );
  }
  const unplaced = (eventId: number | null, problem: string): HisAnswer => ({
    eventId,
    type: eventId === null ? null : "ask.answered",
    caseId: null,
    verdict: label,
    documents: [],
    problem,
  });
  if (placed === null) {
    return unplaced(
      null,
      "the log through the build's pin holds no answer of his to the ask after its first opening"
    );
  }
  if (placed.settlement !== null) {
    const { type, artifactId, version } = placed.settlement;
    return {
      eventId: placed.eventId,
      type,
      caseId: null,
      verdict: SETTLEMENT_VERDICTS[type],
      documents: [{ artifactId, version }],
      problem: null,
    };
  }
  if (!placed.own) {
    return unplaced(
      placed.eventId,
      `his first answer to the ask, event ${placed.eventId}, is not his own, so it holds no verdict`
    );
  }
  const row = ordered.find((one) => one.eventId === placed.eventId);
  if (row === undefined || row.verdict === null) {
    return unplaced(
      placed.eventId,
      `the build holds no case of his first answer to the ask, event ${placed.eventId}`
    );
  }
  return {
    eventId: placed.eventId,
    type: "ask.answered",
    caseId: row.caseId,
    verdict: row.verdict,
    documents: (row.approval?.documents ?? []).map((document) => ({
      artifactId: document.artifactId,
      version: document.version,
    })),
    problem: null,
  };
}

/** The deviation kind a lost file of a request's kept evidence makes (point 6). */
function lostKindOf(file: string): string {
  if (file.startsWith(`${SANDBOX_DIRECTORY}/sessions/`)) return "transcript";
  if (file === `${SANDBOX_DIRECTORY}/${RESULTS_FILE}`) return "answers";
  return "sandbox";
}

/**
 * The request's kept evidence that is lost: each file under its directory that the record names
 * and that is not there with its bytes (`JudgedStage.evidence`); and, for an outcome some version
 * gave a verdict, each of the five judges' transcript and bundle and the answers that the record
 * does not name at all.
 */
function lostEvidenceOf(
  stage: JudgedStage,
  outcome: JudgedOutcome
): RequestReading["lostEvidence"] {
  const lost = (stage.evidence.get(outcome.askId) ?? []).map((one) => ({
    ...one,
    kind: lostKindOf(one.file),
  }));
  if (!RULE_VERSIONS.some((version) => outcome.byRule[version].outcome === "verdict")) return lost;
  const unnamed = (file: string, kind: string): void => {
    if (Object.hasOwn(stage.record.files, `requests/${outcome.askId}/${file}`)) return;
    lost.push({ file, problem: "is not named in the stage record", kind });
  };
  if (outcome.judges.length !== JUDGES) {
    lost.push({
      file: "outcome.json",
      problem: `records ${outcome.judges.length} judges, not ${JUDGES}`,
      kind: "transcript",
    });
  }
  for (const judge of outcome.judges) {
    unnamed(
      `${SANDBOX_DIRECTORY}/${judge.transcript ?? `${judge.label}'s transcript`}`,
      "transcript"
    );
    unnamed(judge.bundle?.split("/").pop() ?? `${judge.label}'s bundle`, "sandbox");
  }
  unnamed(`${SANDBOX_DIRECTORY}/${RESULTS_FILE}`, "answers");
  return lost;
}

/**
 * Point 2's verdict over five answers, as `s10_transcripts.py`'s `rule_verdict` states it, with
 * the share of the five behind it; or why the answers are not five the rule reads.
 */
export function ruleVerdictOf(
  answers: readonly unknown[]
): { verdict: string; share: number } | { problem: string } {
  if (answers.length !== JUDGES) return { problem: `${answers.length} answers, not ${JUDGES}` };
  const named = new Map<string, number>();
  const votes = new Map<string, number>();
  for (const [at, answer] of answers.entries()) {
    const record = recordOf(answer);
    const kind = stringOf(record?.never_decide);
    const verdict = record?.verdict;
    if (kind === null || (verdict !== "approve" && verdict !== "request_changes")) {
      return { problem: `answer ${at + 1} names no never_decide or no verdict the rule reads` };
    }
    if (kind !== NO_KIND) named.set(kind, (named.get(kind) ?? 0) + 1);
    votes.set(verdict, (votes.get(verdict) ?? 0) + 1);
  }
  let namedTotal = 0;
  for (const times of named.values()) namedTotal += times;
  if (namedTotal >= NAMED_TO_HOLD_BACK) {
    const top = Math.max(...named.values());
    const kinds = [...named].filter(([, times]) => times === top).map(([kind]) => kind);
    return { verdict: `never_decide:${kinds.sort().join("+")}`, share: top / JUDGES };
  }
  let chosen = "";
  let most = 0;
  for (const [verdict, times] of votes) {
    if (times > most) [chosen, most] = [verdict, times];
  }
  return { verdict: chosen, share: most / JUDGES };
}

/**
 * Point 2's verdict recomputed for an outcome some version of whose rule gave a verdict or a hold:
 * a hold's is the fact check's with no judge, a verdict's the rule's over the five kept answers.
 */
async function recomputedOf(
  directory: string,
  stage: JudgedStage,
  outcome: JudgedOutcome
): Promise<{ verdict: string; share: number } | { problem: string }> {
  if (outcome.outcome === "hold") {
    const judges = outcome.judgement?.judges ?? null;
    if (judges !== 0 || outcome.judges.length > 0) {
      return {
        problem:
          `a fact-check hold whose judgement records ${judges ?? "no"} judges and whose outcome ` +
          `records ${outcome.judges.length}, where a hold has none`,
      };
    }
    return { verdict: HOLD_VERDICT, share: 1 };
  }
  const kept = await recordedAnswersOf(directory, stage, outcome);
  if ("problem" in kept) return kept;
  return ruleVerdictOf(kept.answers);
}

/**
 * One request read whole: his answer, its outcome, the inputs stage's counts of its candidates,
 * the analysis's checks (the inputs stage's deviation, its kept evidence, his answer, the verdict
 * recomputed, the documents) and each version's outcome.
 */
async function requestReadingOf(
  directory: string,
  stage: JudgedStage,
  { request, line, answer: placed }: Placed,
  outcome: JudgedOutcome
): Promise<RequestReading> {
  const answer = answerOf(request, placed);
  const judgedDocuments =
    outcome.judgement?.documents.map((document) => ({
      artifactId: document.artifact_id,
      version: document.version,
    })) ?? null;
  // A request the inputs stage made a deviation has the stage's own `inputs` deviation, or none
  // yet; any other outcome of it is not the stage over these inputs.
  const unlikeItsInputs =
    line.state !== "ready" &&
    outcome.deviation?.kind !== "inputs" &&
    outcome.deviation?.kind !== UNJUDGED;
  // A version's outcome other than a deviation is a verdict or a hold, which the checks read.
  const anyJudged = RULE_VERSIONS.some(
    (version) => outcome.byRule[version].outcome !== "deviation"
  );
  const lostEvidence = anyJudged ? lostEvidenceOf(stage, outcome) : [];
  const lostKind = DEVIATION_KINDS.find((kind) => lostEvidence.some((one) => one.kind === kind));
  // Lost evidence ends the checks: the answers the recomputation reads may be among it.
  const recomputation =
    anyJudged && lostKind === undefined ? await recomputedOf(directory, stage, outcome) : null;
  const recomputed = recomputation !== null && "verdict" in recomputation ? recomputation : null;
  let documentsProblem: string | null = null;
  if (anyJudged && answer.problem === null) {
    const theirs = (judgedDocuments ?? []).map(pairKey).sort();
    const his = answer.documents.map(pairKey).sort();
    if (JSON.stringify(theirs) !== JSON.stringify(his)) {
      documentsProblem =
        `S10 judged ${theirs.join(", ") || "no document"}, and his answer, event ` +
        `${answer.eventId} (${answer.type}), answered ${his.join(", ") || "no document"}`;
    }
  }
  const unlike = (verdict: string): string | null => {
    if (recomputation === null) return null;
    if ("problem" in recomputation) return `no recomputation: ${recomputation.problem}`;
    if (recomputation.verdict === verdict) return null;
    return `S10 recorded ${verdict}, and point 2's rule gives ${recomputation.verdict}`;
  };
  let verdictProblem: string | null = null;
  const readingUnder = (version: RuleVersion): RuleReading => {
    const one = outcome.byRule[version];
    const refusedCalls = one.toolCallDeviations;
    const deviation = (kind: string, foundBy: FoundDeviation["foundBy"]): RuleReading => ({
      status: "deviation",
      verdict: null,
      deviation: { kind, foundBy },
      disagrees: true,
      refusedCalls,
    });
    if (unlikeItsInputs) return deviation("inputs", "analysis");
    if (one.outcome === "deviation") return deviation(one.deviation, "stage");
    const problem = unlike(one.verdict);
    verdictProblem ??= problem;
    const found =
      lostKind ??
      (answer.problem !== null
        ? "answer"
        : problem !== null
          ? "verdict"
          : documentsProblem !== null
            ? "documents"
            : null);
    if (found !== null) return deviation(found, "analysis");
    if (one.verdict === "approve" || one.verdict === "request_changes") {
      return {
        status: "decided",
        verdict: one.verdict,
        deviation: null,
        disagrees: one.verdict !== answer.verdict,
        refusedCalls,
      };
    }
    return {
      status: "never-decide",
      verdict: one.verdict,
      deviation: null,
      disagrees: null,
      refusedCalls,
    };
  };
  const byRule = {
    v3: readingUnder("v3"),
    v4: readingUnder("v4"),
    v5: readingUnder("v5"),
    v6: readingUnder("v6"),
  };
  return {
    askId: request.askId,
    issueKey: request.issueKey,
    group: request.group,
    cases: [
      ...(answer.caseId === null ? [] : [answer.caseId]),
      ...[...request.rows]
        .sort((one, other) => one.eventId - other.eventId)
        .map((row) => row.caseId)
        .filter((id) => id !== answer.caseId),
    ],
    answer,
    candidates: line.counts,
    stage: {
      outcome: outcome.outcome,
      verdict: outcome.verdict,
      deviation: outcome.deviation?.kind ?? null,
    },
    judgedDocuments,
    recomputed,
    verdictProblem,
    documentsProblem,
    lostEvidence,
    byRule,
  };
}

/** Adds one to `counts[key]`. */
function tallied(counts: Record<string, number>, key: string): void {
  counts[key] = (counts[key] ?? 0) + 1;
}

/** A record of counts with its keys in order, so two readings of the same outcomes print alike. */
function sortedCounts(counts: Record<string, number>): Record<string, number> {
  return Object.fromEntries(
    Object.entries(counts).sort(([one], [other]) => (one < other ? -1 : 1))
  );
}

/** The outcomes of the inventory under one version of the rule, by kind. */
function tallyOf(readings: readonly RequestReading[], version: RuleVersion): OutcomeTally {
  const tally: OutcomeTally = {
    requests: readings.length,
    decided: { total: 0, approve: 0, requestChanges: 0 },
    neverDecides: { total: 0, byVerdict: {} },
    deviations: { total: 0, byKind: {}, foundByAnalysis: {} },
  };
  for (const reading of readings) {
    const one = reading.byRule[version];
    if (one.status === "decided") {
      tally.decided.total += 1;
      if (one.verdict === "approve") tally.decided.approve += 1;
      else tally.decided.requestChanges += 1;
    } else if (one.status === "never-decide") {
      tally.neverDecides.total += 1;
      tallied(tally.neverDecides.byVerdict, one.verdict);
    } else {
      tally.deviations.total += 1;
      tallied(tally.deviations.byKind, one.deviation.kind);
      if (one.deviation.foundBy === "analysis") {
        tallied(tally.deviations.foundByAnalysis, one.deviation.kind);
      }
    }
  }
  tally.neverDecides.byVerdict = sortedCounts(tally.neverDecides.byVerdict);
  tally.deviations.byKind = sortedCounts(tally.deviations.byKind);
  tally.deviations.foundByAnalysis = sortedCounts(tally.deviations.foundByAnalysis);
  return tally;
}

/**
 * Item 8's measure over the draw's eligible trees under one version of the rule, penalized by
 * point 10: a deviation is a decided request that disagrees with his answer. Every request is in
 * one of the trees, since the inventory is `inventoryOf` over those same trees.
 */
function penalizedMeasureOf(
  trees: readonly FinalTestTree[],
  readings: readonly RequestReading[],
  version: RuleVersion
): PenalizedMeasure {
  const rows = trees.map((tree): TreeReading => {
    const row: TreeReading = {
      group: tree.group,
      requests: 0,
      decided: 0,
      deviations: 0,
      neverDecides: 0,
      disagreeing: 0,
    };
    for (const reading of readings) {
      if (reading.group !== tree.group) continue;
      const one = reading.byRule[version];
      row.requests += 1;
      if (one.status === "decided") row.decided += 1;
      else if (one.status === "deviation") row.deviations += 1;
      else row.neverDecides += 1;
      if (one.disagrees === true) row.disagreeing += 1;
    }
    return row;
  });
  const counted = rows.filter((tree) => tree.decided + tree.deviations > 0);
  const disagreeing = counted.filter((tree) => tree.disagreeing > 0).length;
  const withRequest = rows.filter((tree) => tree.requests > 0).length;
  const neverDecide = rows.length - counted.length;
  const bound =
    counted.length === 0 ? 1 : clopperPearsonUpper(disagreeing, counted.length, DEFAULT_CONFIDENCE);
  return {
    trees: rows.length,
    withoutRequest: rows.length - withRequest,
    n: counted.length,
    disagreeing,
    neverDecide: {
      count: neverDecide,
      share: rows.length === 0 ? null : neverDecide / rows.length,
      withRequest: {
        count: withRequest - counted.length,
        share: withRequest === 0 ? null : (withRequest - counted.length) / withRequest,
      },
    },
    bound,
    rate: RATE,
    withinRate: bound <= RATE,
    rows,
  };
}

/** A version's outcome of one request in words, as the record's changes and the text print it. */
export function statusWords(reading: RuleReading): string {
  return reading.status === "deviation"
    ? `deviation ${reading.deviation.kind}`
    : `${reading.status} ${reading.verdict}`;
}

/** The calls `one` refuses that `other` does not, by judge and call id. */
function refusedApart(
  one: readonly ToolCallDeviation[],
  other: readonly ToolCallDeviation[]
): ToolCallDeviation[] {
  const theirs = new Set(other.map((call) => `${call.judge}\u0000${call.callId}`));
  return one.filter((call) => !theirs.has(`${call.judge}\u0000${call.callId}`));
}

/**
 * For each version of the rule from the second, the requests whose deviation status it changes
 * from the version before it (point 5: what each version's rule alone changes), with the calls the
 * two versions refuse apart.
 */
function ruleChangesOf(readings: readonly RequestReading[]): S10Analysis["ruleChanges"] {
  return RULE_VERSIONS.slice(1).map((to, at) => {
    const from = RULE_VERSIONS[at];
    const requests: RuleChange[] = [];
    for (const reading of readings) {
      const before = reading.byRule[from];
      const after = reading.byRule[to];
      if ((before.status === "deviation") === (after.status === "deviation")) continue;
      requests.push({
        askId: reading.askId,
        before: statusWords(before),
        after: statusWords(after),
        refusedOnlyBefore: refusedApart(before.refusedCalls, after.refusedCalls),
        refusedOnlyAfter: refusedApart(after.refusedCalls, before.refusedCalls),
      });
    }
    return { from, to, requests };
  });
}

/** Point 4's disclosure: the candidate counts of every inventory line that has them, summed. */
function neighbourDisclosureOf(inventory: readonly InventoryLine[]): NeighbourDisclosure {
  const dropped = { log: 0, "no-event-through-pin": 0, "after-pin": 0, total: 0 };
  const disclosure: NeighbourDisclosure = {
    requests: 0,
    askHits: 0,
    candidates: 0,
    held: 0,
    reserveErrors: 0,
    kept: 0,
    droppedLaterEvent: dropped,
    dropScanEnds: [],
    statement: RANKS_STATEMENT,
  };
  const ends = new Set<number>();
  for (const line of inventory) {
    const counts = line.counts;
    if (counts === null) continue;
    disclosure.requests += 1;
    disclosure.askHits += counts.askHits;
    disclosure.candidates += counts.candidates;
    disclosure.held += counts.held;
    disclosure.reserveErrors += counts.reserveErrors;
    disclosure.kept += counts.kept;
    for (const seen of DROP_SEEN) {
      dropped[seen] += counts.droppedLaterEvent[seen];
      dropped.total += counts.droppedLaterEvent[seen];
    }
    if (line.dropScanThrough !== null) ends.add(line.dropScanThrough);
  }
  disclosure.dropScanEnds = [...ends].sort((one, other) => one - other);
  return disclosure;
}

/**
 * The whole analysis, from the stages read (`s10-analysis-io.ts`): every figure the record holds
 * and the text prints.
 */
export async function s10AnalysisOf(input: AnalysisInput): Promise<S10Analysis> {
  const outcomes = reportedOutcomesOf(
    input.placed.map((one) => one.line),
    input.judged.stage
  );
  const readings: RequestReading[] = [];
  for (const [at, one] of input.placed.entries()) {
    readings.push(
      await requestReadingOf(input.judged.directory, input.judged.stage, one, outcomes[at])
    );
  }
  let questionCases = 0;
  const questionAsks = new Set<string>();
  for (const tree of input.draw.trees) {
    for (const caseId of tree.cases) {
      const row = input.byCaseId.get(caseId);
      if (row?.kind !== "question") continue;
      questionCases += 1;
      questionAsks.add(row.askId);
    }
  }
  const figuresUnder = (version: RuleVersion) => ({
    tally: tallyOf(readings, version),
    measure: penalizedMeasureOf(input.draw.trees, readings, version),
  });
  const byRule = {
    v3: figuresUnder("v3"),
    v4: figuresUnder("v4"),
    v5: figuresUnder("v5"),
    v6: figuresUnder("v6"),
  };
  const { tally, measure } = byRule[COUNTING_RULE];
  const shares = readings.flatMap((reading) =>
    reading.byRule[COUNTING_RULE].status === "decided" && reading.recomputed !== null
      ? [reading.recomputed.share]
      : []
  );
  const record = input.judged.stage.record;
  return {
    stage: "s10-analysis",
    version: ANALYSIS_VERSION,
    claimRun: input.claimRun,
    reference: input.reference,
    runDigest: input.runDigest,
    chain: {
      build: input.build,
      draw: {
        directory: input.draw.directory,
        drawSha256: input.draw.drawSha256,
        slicesSha256: input.draw.slicesSha256,
      },
      inputs: input.inputs,
      judged: {
        directory: input.judged.directory,
        stageSha256: input.judged.stage.recordSha256,
        state: record.state,
        stops: record.stops,
      },
      log: input.log,
    },
    threshold: CLAIM_THRESHOLD,
    leastDecidedShare: shares.length === 0 ? null : Math.min(...shares),
    countingRule: COUNTING_RULE,
    inventory: {
      requests: readings.length,
      cases: input.placed.reduce((sum, one) => sum + one.request.rows.length, 0),
      trees: input.draw.trees.length,
      questionCases,
      questionAsks: questionAsks.size,
    },
    tally,
    measure,
    decidedShare: {
      decided: tally.decided.total,
      requests: readings.length,
      share: readings.length === 0 ? null : tally.decided.total / readings.length,
      planningD: PLANNING_D,
    },
    byRule,
    ruleChanges: ruleChangesOf(readings),
    sendBack: sendBackFiguresOf(
      readings.map((reading) => {
        const one = reading.byRule[COUNTING_RULE];
        return {
          tree: reading.group,
          person: reading.answer.verdict,
          arm: one.status === "decided" ? one.verdict : null,
        };
      })
    ),
    neighbours: neighbourDisclosureOf(input.placed.map((one) => one.line)),
    requests: readings,
  };
}
