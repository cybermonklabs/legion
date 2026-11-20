import { createHash } from "node:crypto";
import { mkdir, readdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import type { CaseRow, CaseVerdict } from "./build-corpus";
import {
  CannotRunError,
  CorpusError,
  count,
  num,
  outDirectory,
  parsed,
  readCorpus,
  recordOf,
  stringOf,
  table,
} from "./corpus-io";
import {
  BUILD_CHAIN_NAMES,
  type BuildChain,
  type ClaimDraw,
  chainRefusal,
  claimDrawOf,
  type FinalTestTree,
} from "./draw-io";
import {
  CLAIM_RUN_VARIABLE,
  REPOSITORY_ROOT,
  RESERVE_VARIABLE,
  type Registration,
  runDigestOf,
} from "./reserve";
import {
  INVENTORY_FILE,
  type InventoryLine,
  type InventoryRequest,
  inventoryLinesOf,
  inventoryOf,
  STAGE_FILE,
  stageRecordOf,
} from "./s10-inputs-io";
import {
  COUNTING_RULE,
  HOLD_VERDICT,
  JUDGED_FILE,
  type JudgedOutcome,
  type JudgedStage,
  judgedStageOf,
  RULE_VERSIONS,
  type RuleVersion,
  recordedAnswersOf,
  reportedOutcomesOf,
  type ToolCallDeviation,
} from "./s10-judged-io";
import { type SendBackFigures, sendBackFiguresOf, sendBackTables } from "./send-back";
import { clopperPearsonUpper, DEFAULT_CONFIDENCE } from "./stats";
import { TREES_FILE } from "./trees";

/**
 * The claim's analysis of its one bundle, S10: the fifth command of point 13 of the registered
 * claim text, `analyze.ts --claim-s10`, by its points 6, 7, 10 and 12 (point 14 (d)). It reads the
 * claim run's build, draw, inputs and judged stages, calls nothing, and writes one file, its own
 * stage record (`s10-analysis.json`), which holds every figure it prints.
 *
 * **The admission and the chain (point 14 (e)).** It runs only under the claim run's registered
 * read, as every stage of the run does: `readCorpus` admits the read for this run's digest
 * (`reserveAccessOf`, `runDigestOf`, where `--corpus`, `--draw`, `--claim-s10` and `--out` are path
 * options) and takes only the build of the claim run `APPROVAL_EVAL_CLAIM_RUN` names. Each stage is
 * then held to the one before it, by its own parser and the sha256 its successor recorded, so no
 * directory can be swapped between stages: the draw is the claim run's draw of the build's bytes
 * now (`claimDrawOf`); the judged stage (`judgedStageOf`, each file held to its record) was written
 * by this claim run under this read; the inputs stage it names has the bytes of `s10-inputs.json`
 * whose sha256 the judged stage recorded, is written, and names the same claim run, build and
 * read and the draw's two files by their bytes now; and its inventory has the sha256 both records
 * name. The analysis's own record names the claim run, the read, its digest and the sha256 of each
 * stage's record. A run into an `--out` holding another run's record is refused, and so is any
 * other file there; a run into its own run's `--out` must give its record byte for byte, and
 * writes nothing.
 *
 * **The inventory (point 6).** Every answered approval request among the draw's report cases is
 * one request, keyed by ask id, the rule the inputs stage wrote its inventory by (`inventoryOf`);
 * the inventory the stage wrote must be that one, request for request, with the same issue, tree
 * and count of cases. More than one corpus case of an ask is provenance of its one request; a case
 * named twice, an ask in two trees, a report case of no tree, an inventory line repeated by ask id
 * and two outcomes of one ask are each refused. Each request has one outcome: the judged stage's
 * own, or, where the stage has none, a `started` deviation for a request a run started and never
 * finished and an `unjudged` one for a request no run reached (`reportedOutcomesOf`). The question
 * cases of the eligible trees are no approval request: they are in no figure, and are counted.
 *
 * **His answer (point 7).** A request is compared with his answer as the build's corpus records
 * it: the ask's first case by event id, which is his own `ask.answered` (the build keeps only his
 * own answers, and collapses an answer a document settlement wrote again to its earliest event),
 * its verdict read from the option he chose, and the (document, version) pairs it answered, from
 * that event's approval. An outcome whose judged documents (`judgement.documents`) are not those
 * pairs is a deviation (`documents`).
 *
 * **The verdict, recomputed (points 2 and 6).** Each verdict outcome is recomputed by point 2's
 * rule from the five answers its sandbox result kept (`recordedAnswersOf`), as
 * `s10_transcripts.py`'s `rule_verdict` states it: `never_decide:<kind>` where three or more of
 * the five name a kind (the most named, tied kinds in alphabetical order joined by `+`), else the
 * majority of the five verdicts. A kind is a `never_decide` other than `none`: the frozen schema's
 * enum is `none` and the kinds, and an answer outside it is the stage's `answers` deviation before
 * any verdict. A fact-check hold is `never_decide:waiting-legion-root` with no judge. A recorded
 * verdict unlike the recomputation, or five answers the rule cannot read, is a deviation
 * (`verdict`, found by the analysis), under each version of point 5's rule that gave a verdict.
 *
 * **What counts (points 8, 10 and 12).** A request is decided when its outcome is a valid
 * `approve` or `request_changes`, at the claim's threshold, 0.6: five answers give a majority of
 * at least three, so every such verdict carries at least 0.6. A fact-check hold and a valid
 * `never_decide:<kind>` are never-decides, not deviations. Every deviation, of the stage's 13
 * kinds, `unjudged`, or the analysis's two, counts against the claim whatever the confidence: for
 * item 8's bound it is a decided request that disagrees with his answer, so its tree disagrees,
 * and that bound is the penalized bound. n is the eligible trees with a decided request or a
 * deviation, a tree disagrees when any of them does, the bound is the one-sided 90 percent
 * Clopper-Pearson upper bound over n, and the trees with neither are never-decide, their share
 * of the eligible trees apart. Beside it: the outcomes by kind; S10's decided share, the valid
 * `approve` and `request_changes` outcomes over every request of the inventory, against the
 * planning figure d = 0.85; and the send-back figures with the always-approve baseline over the
 * inventory, a never-decide or a deviation a request S10 did not decide, which each figure counts
 * as wrong and none gives a verdict. Version 6's tool rule counts (point 5); under versions 3, 4
 * and 5 the same outcomes give their own penalized bound, and the requests whose deviation status
 * changes from one version to the next are listed with the calls the two versions refuse apart.
 */

/** The file the analysis writes under its `--out`: its stage record, which holds every figure. */
export const ANALYSIS_FILE = "s10-analysis.json";

/** The version of that record's shape. */
export const ANALYSIS_VERSION = 1;

/**
 * The claim's threshold (point 8): a verdict of five answers has at least three behind it, so
 * 0.6 is the lowest counted confidence a decided verdict can have, and not one fitted to data.
 */
export const CLAIM_THRESHOLD = 0.6;

/** The judges of one request (point 2). */
const JUDGES = 5;

/** How many of the five name a kind before the verdict is that never-decide (point 2's rule). */
const NAMED_TO_HOLD_BACK = 3;

/** The `never_decide` of an answer that names no kind. */
const NO_KIND = "none";

/** The rate item 8 holds the bound to: one disagreeing tree in twenty. */
const RATE = 0.05;

/** The planning figure d that point 10 reads the decided share against (the daily count's point 5). */
const PLANNING_D = 0.85;

/** The claim's analysis may not run as the registered text fixes it. */
export class S10AnalysisError extends CannotRunError {
  override readonly name = "S10AnalysisError";
}

/** The claim path's flags, as `analyze.ts` reads them. */
export interface ClaimS10Flags {
  kind: "claim-s10";
  corpus: string;
  draw: string;
  judged: string;
  out: string;
  format: "text" | "json";
}

/** What a test may hand in beside the flags: its own registration, and the root it is under. */
export interface ClaimS10Options {
  registration?: Registration;
  root?: string;
}

/** A request's outcome under one version of point 5's rule, once the analysis has read it. */
export interface RuleReading {
  status: "decided" | "never-decide" | "deviation";
  /** The valid verdict of a decided request or a never-decide; null for a deviation. */
  verdict: string | null;
  /** A deviation's kind and who found it; null for a valid outcome. */
  deviation: { kind: string; foundBy: "stage" | "analysis" } | null;
  /** Whether it counts as disagreeing with his answer: a deviation always; null for a never-decide. */
  disagrees: boolean | null;
  /** The tool calls this version refuses, as the stage recorded them. */
  refusedCalls: ToolCallDeviation[];
}

/** One (document, version) pair, as his answer or S10's judgement names it. */
export interface DocumentPair {
  artifactId: string | null;
  version: number | null;
}

/** One request of the inventory as the analysis reads it. */
export interface RequestReading {
  askId: string;
  issueKey: string | null;
  group: string;
  /** The corpus's cases of the ask: his answer's first, the rest its provenance. */
  cases: string[];
  /** His answer (point 7): the ask's first case, its event, its verdict and the pairs it answered. */
  answer: { caseId: string; eventId: number; verdict: CaseVerdict; documents: DocumentPair[] };
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
  /** Whether that bound is at or under the 1 in 20. */
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
  tally: OutcomeTally;
  measure: PenalizedMeasure;
  decidedShare: { decided: number; requests: number; share: number | null; planningD: number };
  /** The versions of point 5's rule other than the counting one, over the same outcomes. */
  otherRules: Partial<Record<RuleVersion, { tally: OutcomeTally; measure: PenalizedMeasure }>>;
  /** For each version from 4, the requests whose deviation status it changes from the one before. */
  ruleChanges: Array<{ from: RuleVersion; to: RuleVersion; requests: RuleChange[] }>;
  sendBack: SendBackFigures;
  requests: RequestReading[];
}

function sha256Of(bytes: Uint8Array | string): string {
  return createHash("sha256").update(bytes).digest("hex");
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

/** A pair as a key two lists of pairs are compared by. */
function pairKey(pair: DocumentPair): string {
  return JSON.stringify([pair.artifactId, pair.version]);
}

/** His answer to one request: the ask's first case by event id, which the build holds as his own. */
function answerOf(request: InventoryRequest): RequestReading["answer"] {
  const [first] = [...request.rows].sort((one, other) => one.eventId - other.eventId);
  if (first === undefined || first.verdict === null) {
    throw new CorpusError(
      `the request ${request.askId} has no approval case with a verdict: his answer is read from ` +
        "the option he chose"
    );
  }
  return {
    caseId: first.caseId,
    eventId: first.eventId,
    verdict: first.verdict,
    documents: (first.approval?.documents ?? []).map((document) => ({
      artifactId: document.artifactId,
      version: document.version,
    })),
  };
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
      return { problem: `a fact-check hold whose judgement records ${judges ?? "no"} judges` };
    }
    return { verdict: HOLD_VERDICT, share: 1 };
  }
  const kept = await recordedAnswersOf(directory, stage, outcome);
  if ("problem" in kept) return kept;
  return ruleVerdictOf(kept.answers);
}

/** One request read whole: his answer, its outcome, the analysis's two checks, each version's. */
async function requestReadingOf(
  directory: string,
  stage: JudgedStage,
  request: InventoryRequest,
  outcome: JudgedOutcome
): Promise<RequestReading> {
  const answer = answerOf(request);
  const judgedDocuments =
    outcome.judgement?.documents.map((document) => ({
      artifactId: document.artifact_id,
      version: document.version,
    })) ?? null;
  // A version's outcome other than a deviation is a verdict or a hold, which the checks read.
  const anyJudged = RULE_VERSIONS.some(
    (version) => outcome.byRule[version].outcome !== "deviation"
  );
  const recomputation = anyJudged ? await recomputedOf(directory, stage, outcome) : null;
  const recomputed = recomputation !== null && "verdict" in recomputation ? recomputation : null;
  let documentsProblem: string | null = null;
  if (anyJudged) {
    const theirs = (judgedDocuments ?? []).map(pairKey).sort();
    const his = answer.documents.map(pairKey).sort();
    if (JSON.stringify(theirs) !== JSON.stringify(his)) {
      documentsProblem =
        `S10 judged ${theirs.join(", ") || "no document"}, and his answer ` +
        `${answer.caseId} answered ${his.join(", ") || "no document"}`;
    }
  }
  const unlike = (verdict: string | null): string | null => {
    if (recomputation === null) return null;
    if ("problem" in recomputation) return `no recomputation: ${recomputation.problem}`;
    if (recomputation.verdict === verdict) return null;
    return `S10 recorded ${verdict}, and point 2's rule gives ${recomputation.verdict}`;
  };
  const byRule = {} as Record<RuleVersion, RuleReading>;
  let verdictProblem: string | null = null;
  for (const version of RULE_VERSIONS) {
    const one = outcome.byRule[version];
    const refusedCalls = one.toolCallDeviations;
    if (one.outcome === "deviation") {
      if (one.deviation === null) {
        throw new CorpusError(`the outcome of ${outcome.askId} is a deviation of no kind`);
      }
      byRule[version] = {
        status: "deviation",
        verdict: null,
        deviation: { kind: one.deviation, foundBy: "stage" },
        disagrees: true,
        refusedCalls,
      };
      continue;
    }
    const problem = unlike(one.verdict);
    verdictProblem ??= problem;
    const found = problem !== null ? "verdict" : documentsProblem !== null ? "documents" : null;
    if (found !== null) {
      byRule[version] = {
        status: "deviation",
        verdict: null,
        deviation: { kind: found, foundBy: "analysis" },
        disagrees: true,
        refusedCalls,
      };
      continue;
    }
    const decided = one.verdict === "approve" || one.verdict === "request_changes";
    byRule[version] = {
      status: decided ? "decided" : "never-decide",
      verdict: one.verdict,
      deviation: null,
      disagrees: decided ? one.verdict !== answer.verdict : null,
      refusedCalls,
    };
  }
  return {
    askId: request.askId,
    issueKey: request.issueKey,
    group: request.group,
    cases: [
      answer.caseId,
      ...request.rows.map((row) => row.caseId).filter((id) => id !== answer.caseId),
    ],
    answer,
    stage: {
      outcome: outcome.outcome,
      verdict: outcome.verdict,
      deviation: outcome.deviation?.kind ?? null,
    },
    judgedDocuments,
    recomputed,
    verdictProblem,
    documentsProblem: anyJudged ? documentsProblem : null,
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
export function tallyOf(readings: readonly RequestReading[], version: RuleVersion): OutcomeTally {
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
      tallied(tally.neverDecides.byVerdict, one.verdict ?? "");
    } else if (one.deviation !== null) {
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
 * point 10: a deviation is a decided request that disagrees with his answer.
 */
export function penalizedMeasureOf(
  trees: readonly FinalTestTree[],
  readings: readonly RequestReading[],
  version: RuleVersion
): PenalizedMeasure {
  const byGroup = new Map<string, TreeReading>();
  for (const tree of trees) {
    byGroup.set(tree.group, {
      group: tree.group,
      requests: 0,
      decided: 0,
      deviations: 0,
      neverDecides: 0,
      disagreeing: 0,
    });
  }
  for (const reading of readings) {
    const tree = byGroup.get(reading.group);
    if (tree === undefined) {
      throw new CorpusError(
        `the request ${reading.askId} is in the tree ${reading.group}, which the draw does not ` +
          "record as eligible"
      );
    }
    const one = reading.byRule[version];
    tree.requests += 1;
    if (one.status === "decided") tree.decided += 1;
    else if (one.status === "deviation") tree.deviations += 1;
    else tree.neverDecides += 1;
    if (one.disagrees === true) tree.disagreeing += 1;
  }
  const rows = [...byGroup.values()];
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
    withinRate: bound <= RATE,
    rows,
  };
}

/** A version's outcome of one request, in words a list of changes prints. */
function statusWords(reading: RuleReading): string {
  if (reading.status === "deviation") {
    return `deviation ${reading.deviation?.kind ?? ""}`.trim();
  }
  return `${reading.status} ${reading.verdict ?? ""}`.trim();
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
export function ruleChangesOf(readings: readonly RequestReading[]): S10Analysis["ruleChanges"] {
  return RULE_VERSIONS.slice(1).map((to, at) => {
    const from = RULE_VERSIONS[at] as RuleVersion;
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

/** What the analysis is computed from: every stage read and held to the chain, nothing more. */
export interface AnalysisInput {
  claimRun: BuildChain;
  reference: string;
  runDigest: string;
  build: string;
  draw: ClaimDraw;
  inputs: { directory: string; stageSha256: string; inventorySha256: string };
  judged: { directory: string; stage: JudgedStage };
  byCaseId: ReadonlyMap<string, CaseRow>;
  requests: readonly InventoryRequest[];
  inventory: readonly InventoryLine[];
}

/** The whole analysis, from the stages read: every figure the record holds and the text prints. */
export async function s10AnalysisOf(input: AnalysisInput): Promise<S10Analysis> {
  const byAsk = new Map(input.requests.map((request) => [request.askId, request]));
  const outcomes = reportedOutcomesOf(input.inventory, input.judged.stage);
  const readings: RequestReading[] = [];
  for (const outcome of outcomes) {
    const request = byAsk.get(outcome.askId);
    if (request === undefined) {
      throw new CorpusError(`the inventory's request ${outcome.askId} is no request of the draw`);
    }
    readings.push(
      await requestReadingOf(input.judged.directory, input.judged.stage, request, outcome)
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
  const otherRules: S10Analysis["otherRules"] = {};
  for (const version of RULE_VERSIONS) {
    if (version === COUNTING_RULE) continue;
    otherRules[version] = {
      tally: tallyOf(readings, version),
      measure: penalizedMeasureOf(input.draw.trees, readings, version),
    };
  }
  const tally = tallyOf(readings, COUNTING_RULE);
  const shares = readings
    .filter((reading) => reading.byRule[COUNTING_RULE].status === "decided")
    .map((reading) => reading.recomputed?.share ?? 0);
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
    },
    threshold: CLAIM_THRESHOLD,
    leastDecidedShare: shares.length === 0 ? null : Math.min(...shares),
    countingRule: COUNTING_RULE,
    inventory: {
      requests: readings.length,
      cases: input.requests.reduce((sum, request) => sum + request.rows.length, 0),
      trees: input.draw.trees.length,
      questionCases,
      questionAsks: questionAsks.size,
    },
    tally,
    measure: penalizedMeasureOf(input.draw.trees, readings, COUNTING_RULE),
    decidedShare: {
      decided: tally.decided.total,
      requests: readings.length,
      share: readings.length === 0 ? null : tally.decided.total / readings.length,
      planningD: PLANNING_D,
    },
    otherRules,
    ruleChanges: ruleChangesOf(readings),
    sendBack: sendBackFiguresOf(
      readings.map((reading) => {
        const one = reading.byRule[COUNTING_RULE];
        return {
          tree: reading.group,
          person: reading.answer.verdict,
          arm: one.status === "decided" ? (one.verdict as CaseVerdict) : null,
        };
      })
    ),
    requests: readings,
  };
}

/** The bytes of a file of a stage before this one, refused where they are not there. */
async function stageBytesOf(path: string, what: string): Promise<Buffer> {
  try {
    return await readFile(path);
  } catch {
    throw new S10AnalysisError(`${path} could not be read: ${what}`);
  }
}

/**
 * Every stage of the claim run this analysis reads, admitted and held to the chain (the module's
 * comment says how), as `s10AnalysisOf` takes them.
 */
async function chainedInput(
  flags: ClaimS10Flags,
  options: ClaimS10Options
): Promise<AnalysisInput> {
  const root = options.root ?? REPOSITORY_ROOT;
  const corpus = await readCorpus(flags.corpus, { registration: options.registration, root });
  const reference = corpus.reserve.readUnder;
  const claimRun = corpus.snapshot.reserve?.claimRun;
  if (reference === null || claimRun === undefined) {
    throw new S10AnalysisError(
      "the claim's analysis runs only under its registered read, over the build of the claim " +
        `run that read: set ${RESERVE_VARIABLE} and ${CLAIM_RUN_VARIABLE} as that build had them`
    );
  }
  const trees = await stageBytesOf(join(corpus.directory, TREES_FILE), "--corpus takes the build");
  const build: BuildChain = {
    id: claimRun,
    snapshotSha256: corpus.snapshotSha256,
    corpusSha256: corpus.corpusSha256,
    treesSha256: sha256Of(trees),
  };
  const draw = await claimDrawOf(resolve(flags.draw), build, reference);
  const judgedDirectory = resolve(flags.judged);
  const stage = await judgedStageOf(judgedDirectory);
  const judgedPath = join(judgedDirectory, JUDGED_FILE);
  if (stage.record.claimRun !== claimRun) {
    throw new S10AnalysisError(
      `${judgedPath} was written by the claim run ${stage.record.claimRun}, and this run's is ` +
        `${claimRun}: the analysis takes only its own claim run's judged stage`
    );
  }
  if (stage.record.reference !== reference) {
    throw new S10AnalysisError(
      `${judgedPath} was written under ${stage.record.reference}, and this run reads under ${reference}`
    );
  }
  const inputsPath = join(stage.record.inputs.directory, STAGE_FILE);
  const inputsBytes = await stageBytesOf(inputsPath, "the judged stage names it");
  if (sha256Of(inputsBytes) !== stage.record.inputs.stageSha256) {
    throw new S10AnalysisError(
      `${inputsPath} is not the inputs stage ${judgedPath} judged: its sha256 is ` +
        `${sha256Of(inputsBytes)}, and the judged stage names ${stage.record.inputs.stageSha256}`
    );
  }
  const inputs = stageRecordOf(inputsBytes.toString("utf8"), inputsPath);
  const refusal =
    chainRefusal(inputs.claimRun, build, BUILD_CHAIN_NAMES) ??
    chainRefusal(inputs, { reference }, { reference: "read" }) ??
    chainRefusal(
      inputs.draw,
      { drawSha256: draw.drawSha256, slicesSha256: draw.slicesSha256 },
      { drawSha256: "draw.json", slicesSha256: "slices.json" }
    );
  if (refusal !== null) {
    throw new S10AnalysisError(
      `${inputsPath} is not this claim run's inputs stage over the build and the draw it reads: ` +
        `${refusal}`
    );
  }
  if (inputs.state !== "written" || inputs.inventorySha256 === null) {
    throw new S10AnalysisError(`${inputsPath} records an inputs stage that never finished`);
  }
  const inventoryPath = join(stage.record.inputs.directory, INVENTORY_FILE);
  const inventoryBytes = await stageBytesOf(inventoryPath, "the inputs stage names it");
  const inventorySha256 = sha256Of(inventoryBytes);
  const judgedRead = stage.record.inputs.files[INVENTORY_FILE];
  if (inventorySha256 !== inputs.inventorySha256 || inventorySha256 !== judgedRead) {
    throw new S10AnalysisError(
      `${inventoryPath} is not the inventory the inputs stage wrote and the judging read: its ` +
        `sha256 is ${inventorySha256}, and the records name ${inputs.inventorySha256} and ` +
        `${judgedRead ?? "none"}`
    );
  }
  const inventory = inventoryLinesOf(inventoryBytes.toString("utf8"), inventoryPath);
  const requests = inventoryOf(corpus.byCaseId, draw.trees, draw.report);
  heldToDraw(inventory, requests, inventoryPath);
  return {
    claimRun: build,
    reference,
    runDigest: await runDigestOf({ argv: process.argv, env: process.env }, root),
    build: corpus.directory,
    draw,
    inputs: {
      directory: stage.record.inputs.directory,
      stageSha256: stage.record.inputs.stageSha256,
      inventorySha256,
    },
    judged: { directory: judgedDirectory, stage },
    byCaseId: corpus.byCaseId,
    requests,
    inventory,
  };
}

/**
 * Holds the inventory the inputs stage wrote to the one the draw gives (`inventoryOf`): each line
 * once by ask id, and request for request the same asks, issues, trees and counts of cases.
 */
function heldToDraw(
  inventory: readonly InventoryLine[],
  requests: readonly InventoryRequest[],
  path: string
): void {
  const lines = new Map<string, InventoryLine>();
  for (const line of inventory) {
    if (lines.has(line.askId)) {
      throw new CorpusError(`${path} names the request ${line.askId} twice`);
    }
    lines.set(line.askId, line);
  }
  for (const request of requests) {
    const line = lines.get(request.askId);
    if (line === undefined) {
      throw new S10AnalysisError(
        `${path} holds no line of the request ${request.askId}, which the draw's report trees hold`
      );
    }
    if (
      line.issueKey !== request.issueKey ||
      line.group !== request.group ||
      line.cases !== request.rows.length
    ) {
      throw new S10AnalysisError(
        `${path} holds the request ${request.askId} in ${line.group} with ${line.cases} cases, ` +
          `and the draw's report trees give it ${request.group} with ${request.rows.length}`
      );
    }
    lines.delete(request.askId);
  }
  const [extra] = lines.keys();
  if (extra !== undefined) {
    throw new S10AnalysisError(
      `${path} names the request ${extra}, which no approval case of the draw's report trees is`
    );
  }
}

/**
 * Where an analysis record found in `--out` is not this run's, worded as the tail of a refusal,
 * and null where it is: the same claim run and build, read, digest, and stages, by their sha256.
 */
function recordRefusal(found: unknown, wanted: S10Analysis): string | null {
  const record = recordOf(found);
  if (record?.stage !== "s10-analysis" || record.version !== ANALYSIS_VERSION) {
    return `it is no record of version ${ANALYSIS_VERSION} of the claim's analysis`;
  }
  const chain = recordOf(record.chain);
  return (
    chainRefusal(record.claimRun, wanted.claimRun, BUILD_CHAIN_NAMES) ??
    chainRefusal(record, wanted, { reference: "read", runDigest: "run digest" }) ??
    chainRefusal(chain?.draw, wanted.chain.draw, {
      drawSha256: "draw.json",
      slicesSha256: "slices.json",
    }) ??
    chainRefusal(chain?.inputs, wanted.chain.inputs, {
      stageSha256: "s10-inputs.json",
      inventorySha256: "inventory.jsonl",
    }) ??
    chainRefusal(chain?.judged, wanted.chain.judged, { stageSha256: JUDGED_FILE })
  );
}

/**
 * Writes the record into `out`, or keeps the one there: a directory holding nothing else takes it,
 * written beside its place and moved there; one holding this run's record must hold it byte for
 * byte, and keeps it. Another run's record, or any other file, is refused.
 */
async function writtenTo(out: string, record: S10Analysis): Promise<"written" | "kept"> {
  await mkdir(out, { recursive: true });
  const path = join(out, ANALYSIS_FILE);
  for (const name of await readdir(out)) {
    if (name.startsWith(`${ANALYSIS_FILE}.`) && name.endsWith(".partial")) {
      await rm(join(out, name), { force: true });
    } else if (name !== ANALYSIS_FILE) {
      throw new S10AnalysisError(
        `${join(out, name)} is no file this analysis writes, so no run here can show it is its own`
      );
    }
  }
  const text = `${JSON.stringify(record, null, 2)}\n`;
  const found = await readFile(path, "utf8").catch(() => null);
  if (found !== null) {
    const refusal = recordRefusal(parsed(found, path), record);
    if (refusal !== null) {
      throw new S10AnalysisError(
        `${path} is another run's analysis: ${refusal}. A run takes only its own claim run's ` +
          "--out, over the same stages"
      );
    }
    if (found !== text) {
      throw new S10AnalysisError(
        `${path} holds this run's analysis with other figures than this run gives, so it is not ` +
          "this run's own"
      );
    }
    return "kept";
  }
  const partial = `${path}.${process.pid}.partial`;
  await writeFile(partial, text, "utf8");
  await rename(partial, path);
  return "written";
}

/** The claim's analysis as a command: the stages, the record written or kept, the text printed. */
export async function claimS10(
  flags: ClaimS10Flags,
  options: ClaimS10Options = {}
): Promise<number> {
  const out = await outDirectory(flags.out);
  const input = await chainedInput(flags, options);
  const analysis = await s10AnalysisOf(input);
  const done = await writtenTo(out, analysis);
  console.log(
    flags.format === "json"
      ? JSON.stringify(analysis, null, 2)
      : renderS10Analysis(analysis).join("\n")
  );
  console.error(`${done} ${join(out, ANALYSIS_FILE)}`);
  return 0;
}

/** A share and its count over a total, or the words that say nothing was counted. */
function shareText(part: number, whole: number): string {
  return whole === 0
    ? `${count(part)} of 0`
    : `${count(part)} of ${count(whole)} (${num(part / whole)})`;
}

/** The counts of a record, one `name count` a cell, or `none`. */
function countsText(counts: Record<string, number>): string {
  const entries = Object.entries(counts);
  return entries.length === 0
    ? "none"
    : entries.map(([name, times]) => `${name} ${count(times)}`).join(", ");
}

/** One version's measure as a row of the rule table. */
function ruleRow(version: RuleVersion, tally: OutcomeTally, measure: PenalizedMeasure): string[] {
  return [
    version === COUNTING_RULE ? `${version} (counts)` : version,
    count(tally.decided.total),
    count(tally.neverDecides.total),
    count(tally.deviations.total),
    count(measure.n),
    count(measure.disagreeing),
    `at most ${num(measure.bound)}`,
  ];
}

/** The analysis as it prints: every figure of the record, in the order the registered text reads them. */
export function renderS10Analysis(analysis: S10Analysis): string[] {
  const { chain, inventory, tally, measure, decidedShare } = analysis;
  const lines = ["# The claim's analysis of S10, over its judged stage", ""];
  lines.push(
    "Points 6, 7, 10 and 12 of the registered claim text, for its one bundle, S10, at " +
      `--claim-threshold ${num(analysis.threshold, 1)} (point 8): a verdict of five answers has ` +
      "at least three behind it, so every approve or request_changes is decided" +
      (analysis.leastDecidedShare === null
        ? "."
        : ` (the least share behind one here is ${num(analysis.leastDecidedShare, 1)}).`) +
      ` Version ${analysis.countingRule.slice(1)} of point 5's tool rule counts.`,
    "",
    "## The chain",
    "",
    `- claim run ${analysis.claimRun.id}, read under ${analysis.reference}, this run's digest ` +
      analysis.runDigest,
    `- build ${chain.build}: snapshot.json ${analysis.claimRun.snapshotSha256}, corpus.jsonl ` +
      `${analysis.claimRun.corpusSha256}, trees.json ${analysis.claimRun.treesSha256}`,
    `- draw ${chain.draw.directory}: draw.json ${chain.draw.drawSha256}, slices.json ` +
      chain.draw.slicesSha256,
    `- inputs ${chain.inputs.directory}: s10-inputs.json ${chain.inputs.stageSha256}, ` +
      `inventory.jsonl ${chain.inputs.inventorySha256}`,
    `- judged ${chain.judged.directory}: ${JUDGED_FILE} ${chain.judged.stageSha256}, ` +
      `${chain.judged.state}` +
      (chain.judged.stops.length === 0
        ? ""
        : `, after ${count(chain.judged.stops.length)} stops (the last: ` +
          `${chain.judged.stops[chain.judged.stops.length - 1]?.reason})`),
    "",
    "## The inventory (point 6)",
    "",
    `${count(inventory.requests)} answered approval requests of the draw's report trees, one an ` +
      `ask, from ${count(inventory.cases)} corpus cases: a case past an ask's first is its ` +
      `provenance, never a row. ${count(inventory.questionCases)} question cases on ` +
      `${count(inventory.questionAsks)} asks in the same trees are no approval request, and in ` +
      `no figure below. ${count(inventory.trees)} eligible trees, ` +
      `${count(measure.withoutRequest)} of them with no request.`,
    "",
    "## Each request's one outcome (points 6 and 7)",
    "",
    ...table(
      ["outcome", "requests", "by kind"],
      [
        [
          "decided",
          count(tally.decided.total),
          `approve ${count(tally.decided.approve)}, request_changes ` +
            count(tally.decided.requestChanges),
        ],
        ["never-decide", count(tally.neverDecides.total), countsText(tally.neverDecides.byVerdict)],
        ["deviation", count(tally.deviations.total), countsText(tally.deviations.byKind)],
      ]
    ),
    "",
    "A fact-check hold and a valid never_decide verdict are never-decides, not deviations. Of the " +
      `deviations, the analysis found ${countsText(tally.deviations.foundByAnalysis)}: ` +
      "verdict, a recorded verdict unlike point 2's rule recomputed from the five answers the " +
      "stage kept; documents, his answer's (document, version) pairs unlike the judged ones. " +
      "started is a request a run started and never finished; unjudged one no run reached.",
    "",
    "## Item 8's measure, penalized (points 10 and 12)",
    "",
    "Over the draw's eligible trees: every deviation is a decided request that disagrees with his " +
      "answer, whatever the confidence, so its tree disagrees. n is the trees with a decided " +
      "request or a deviation; a tree disagrees when any of them does; the trees with neither " +
      "are never-decide, apart and not in n, their share of every eligible tree and of those " +
      "holding a request. The penalized bound is the one-sided 90 percent Clopper-Pearson upper " +
      "bound on the disagreeing trees over n, and 1 where n is 0.",
    "",
    ...table(
      [
        "eligible trees",
        "no request",
        "n",
        "disagreeing",
        "never-decide",
        "of those with a request",
        "penalized bound",
        `at or under ${num(RATE, 2)}`,
      ],
      [
        [
          count(measure.trees),
          count(measure.withoutRequest),
          count(measure.n),
          count(measure.disagreeing),
          measure.neverDecide.share === null
            ? count(measure.neverDecide.count)
            : `${count(measure.neverDecide.count)} (${num(measure.neverDecide.share)})`,
          measure.neverDecide.withRequest.share === null
            ? count(measure.neverDecide.withRequest.count)
            : `${count(measure.neverDecide.withRequest.count)} ` +
              `(${num(measure.neverDecide.withRequest.share)})`,
          `at most ${num(measure.bound)}`,
          measure.withinRate ? "yes" : "no",
        ],
      ]
    ),
    "",
    "## S10's decided share (point 10)",
    "",
    `${shareText(decidedShare.decided, decidedShare.requests)}: the requests with a valid ` +
      "approve or request_changes over every request of the inventory, deviations in the " +
      `denominator and never in the numerator; against d = ${num(decidedShare.planningD, 2)}, a ` +
      "planning figure taken before S10 began (the daily count's point 5), not S10's measured " +
      "share.",
    "",
    "## Point 5's tool rules",
    "",
    `The same outcomes under each version of point 5's rule, version ` +
      `${analysis.countingRule.slice(1)}'s the one that counts, each with its penalized bound.`,
    ""
  );
  lines.push(
    ...table(
      ["rule", "decided", "never-decide", "deviations", "n", "disagreeing", "penalized bound"],
      RULE_VERSIONS.map((version) => {
        const other = analysis.otherRules[version];
        return other === undefined
          ? ruleRow(version, tally, measure)
          : ruleRow(version, other.tally, other.measure);
      })
    )
  );
  for (const change of analysis.ruleChanges) {
    lines.push(
      "",
      `Version ${change.to.slice(1)}'s rule changes the deviation status of ` +
        `${count(change.requests.length)} requests from version ${change.from.slice(1)}'s` +
        (change.requests.length === 0 ? "." : ":")
    );
    for (const one of change.requests) {
      const calls = (list: readonly ToolCallDeviation[]): string =>
        list.length === 0
          ? "none"
          : list.map((call) => `${call.judge} ${call.tools.join("+")} (${call.reason})`).join("; ");
      lines.push(
        `- ${one.askId}: ${one.before} to ${one.after}; refused only by version ` +
          `${change.from.slice(1)}: ${calls(one.refusedOnlyBefore)}; refused only by version ` +
          `${change.to.slice(1)}: ${calls(one.refusedOnlyAfter)}`
      );
    }
  }
  lines.push(
    "",
    "### The send-back figures beside item 8",
    "",
    "For the catalog (version 14), over every request of the inventory, each once. A send-back " +
      "is his chosen option, request changes; any other option he chose is no send-back, and " +
      "the answers of an option that is neither approve nor request changes are counted in a " +
      "column of their own. S10 sends a request back where its decided verdict is request " +
      "changes. A never-decide or a deviation is a request S10 did not decide: no verdict is " +
      "made up for it, and each figure counts it as wrong, a missed send-back where he sent it " +
      "back and a false one where he did not; the not-decided counts in brackets say how many. " +
      "Each interval is two-sided at 95 percent and exact over the requests; balanced accuracy's " +
      "is the mean of recall's and specificity's intervals at 97.5 percent each.",
    "",
    ...sendBackTables("bundle", [{ name: "S10", figures: analysis.sendBack }]),
    "",
    "## Each request",
    "",
    ...table(
      ["ask", "tree", "his answer", "outcome", "decided", "disagrees"],
      analysis.requests.map((reading) => {
        const one = reading.byRule[analysis.countingRule];
        return [
          reading.askId,
          reading.group,
          reading.answer.verdict,
          statusWords(one),
          one.status === "decided" ? "yes" : "no",
          one.disagrees === null ? "-" : one.disagrees ? "yes" : "no",
        ];
      })
    )
  );
  return lines;
}
