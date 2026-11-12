import { afterEach, describe, expect, spyOn, test } from "bun:test";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  type Analysis,
  type ArmCoverage,
  analyseRound,
  type CaseNeighbours,
  chooseWinner,
  classTable,
  confidenceTableOf,
  main,
  manifestHash,
  modeOf,
  readCase,
  readRound,
  renderReport,
} from "../analyze";
import type { CaseKind, CaseRow, CaseVerdict } from "../build-corpus";
import type { Corpus } from "../corpus-io";
import { type SliceName, TARGETS } from "../draw";
import type { RecipeName } from "../recipes";
import type { Endpoint, JudgementRow } from "../run-round";
import { MINIMUM_DECIDED, mcnemarExact } from "../stats";

/**
 * The report over hand-written rows: nothing of it reads a corpus directory, a round directory
 * or Dispatch, and nothing makes a call. What it holds the reading to is the rules the plan
 * names - the mode and its tie, the floor of three judgements that returned, the paired
 * comparison with the baseline, a threshold fitted on the fit slice and applied to the report
 * slice, the winner rule with its cost tie-break and its usefulness floor, the class table the
 * corpus alone answers, and the arm too short of judgements to carry a bound.
 *
 * The last block is the command rather than the reading, and writes a corpus into a scratch
 * directory: a file it cannot read is a refusal that returns 2, never an error thrown past it.
 */

/** The two labels every case of this file offers, which is what a verdict is one of. */
const APPROVE = "Approve";
const CHANGES = "Request changes";

/** One case of the corpus, with only the fields the reading and the class table read named. */
function caseRow(
  caseId: string,
  options: {
    kind?: CaseKind;
    selected?: string[];
    verdict?: CaseVerdict | null;
    project?: string | null;
    version?: number | null;
    answersThatDayBefore?: number;
    hourUtc?: number;
    changesAlreadyRequested?: boolean;
    answeredAt?: string;
    artifactId?: string;
    issueKey?: string;
  } = {}
): CaseRow {
  const kind = options.kind ?? "approval";
  const selected = options.selected ?? [APPROVE];
  const artifactId = options.artifactId ?? `${caseId}-document`;
  return {
    caseId,
    eventId: 1,
    askId: `ask-${caseId}`,
    issueKey: options.issueKey ?? "PROJ-1",
    project: options.project === undefined ? "INFRA" : options.project,
    kind,
    urgency: "med",
    multiple: false,
    question: `Approve the spec of ${caseId}?`,
    options: [
      { id: "o1", label: APPROVE, description: "it is ready" },
      { id: "o2", label: CHANGES, description: "it is not" },
    ],
    selected,
    words: null,
    hasWords: false,
    askCreatedAt: "2026-11-06T09:00:00Z",
    answeredAt: options.answeredAt ?? "2026-11-06T12:00:00Z",
    eventCreatedAt: options.answeredAt ?? "2026-11-06T12:00:00Z",
    approval:
      kind === "approval"
        ? {
            documents: [{ artifactId, name: "spec.md", version: 3, requestedVersion: 3 }],
            artifactSlug: "spec",
            artifactPrimary: true,
          }
        : null,
    verdict:
      options.verdict === undefined
        ? kind === "approval"
          ? selected.includes(APPROVE)
            ? "approve"
            : "request_changes"
          : null
        : options.verdict,
    confounds: {
      version: options.version === undefined ? 3 : options.version,
      answersThatDayBefore: options.answersThatDayBefore ?? 10,
      hourUtc: options.hourUtc ?? 18,
      hourPacific: 10,
      changesAlreadyRequested: options.changesAlreadyRequested ?? false,
      priorCasesOnDocument: 0,
      caseKind: kind,
    },
  };
}

/** One judgement row, with only the fields the reading reads named. */
function judgementRow(
  arm: string,
  caseId: string,
  judgement: number,
  verdict: string[],
  options: {
    slice?: SliceName;
    kind?: CaseKind;
    fault?: boolean;
    stateCut?: boolean;
    excluded?: number;
    recipe?: RecipeName;
    endpoint?: Endpoint;
    /** The endpoint's own stated probability of what it chose, which only Jev states. */
    confidence?: number;
    /** The model the arm is on, which is the endpoint's own where the caller names none. */
    model?: string;
  } = {}
): JudgementRow {
  const faulted = options.fault === true;
  const endpoint = options.endpoint ?? "claude";
  return {
    arm,
    recipe: options.recipe ?? "bare",
    model: options.model ?? (endpoint === "jev" ? "jev-latest" : "anthropic/claude-opus-5-5"),
    endpoint,
    caseId,
    slice: options.slice ?? "report",
    kind: options.kind ?? "approval",
    judgement,
    seed: 1,
    verdict: faulted ? [] : verdict,
    probabilities: null,
    confidence: options.confidence ?? null,
    inputTokens: 100,
    outputTokens: 10,
    latencyMs: 10,
    attempts: 1,
    stateCut: options.stateCut === true,
    retrievalRoute: null,
    examples: 0,
    excluded: options.excluded ?? null,
    promptBytes: 100,
    fault: faulted ? "transport" : null,
    faultDetail: faulted ? "the endpoint closed the connection" : null,
    at: "2026-11-12T00:00:00Z",
  };
}

/** Five judgements of one case: `agreeing` of them chose the first label and the rest the other. */
function fiveRows(
  arm: string,
  caseId: string,
  chose: number,
  options: {
    slice?: SliceName;
    kind?: CaseKind;
    faulted?: number;
    excluded?: number;
    recipe?: RecipeName;
    endpoint?: Endpoint;
    confidence?: number;
  } = {}
): JudgementRow[] {
  const faulted = options.faulted ?? 0;
  return Array.from({ length: 5 }, (_, index) =>
    judgementRow(arm, caseId, index, index < chose ? [APPROVE] : [CHANGES], {
      slice: options.slice,
      kind: options.kind,
      fault: index < faulted,
      excluded: options.excluded,
      recipe: options.recipe,
      endpoint: options.endpoint,
      confidence: options.confidence,
    })
  );
}

/** A corpus as `analyseRound` is handed one, built from rows alone. */
function corpusOf(rows: readonly CaseRow[]): Corpus {
  return {
    directory: "/nowhere/corpus",
    snapshot: {
      person: "someone",
      throughEventId: 1,
      eventsRead: 1,
      keptEvents: 1,
      readStartedAt: "2026-11-11T00:00:00Z",
      readFinishedAt: "2026-11-11T01:00:00Z",
    },
    rows: [...rows],
    byCaseId: new Map(rows.map((row) => [row.caseId, row])),
  };
}

describe("the mode of the judgements that returned", () => {
  test("the mode is the answer most of them chose, and the confidence its share", () => {
    const mode = modeOf([[APPROVE], [APPROVE], [CHANGES], [APPROVE], [CHANGES]]);
    expect(mode.labels).toEqual([APPROVE]);
    expect(mode.count).toBe(3);
    expect(mode.returned).toBe(5);
    expect(mode.confidence).toBeCloseTo(0.6, 10);
    expect(mode.reason).toBeNull();
  });

  test("a multi-option answer is one answer, compared as a set", () => {
    const mode = modeOf([[CHANGES, APPROVE], [APPROVE, CHANGES], [APPROVE, CHANGES], [APPROVE]]);
    expect(mode.labels).toEqual([APPROVE, CHANGES]);
    expect(mode.confidence).toBeCloseTo(0.75, 10);
  });

  test("a tie for the mode is undecided and carries its reason", () => {
    const mode = modeOf([[APPROVE], [APPROVE], [CHANGES], [CHANGES]]);
    expect(mode.labels).toBeNull();
    expect(mode.confidence).toBe(0);
    expect(mode.reason).toContain("tied");
  });

  test("fewer than three judgements returned is undecided", () => {
    const mode = modeOf([[APPROVE], [APPROVE]]);
    expect(mode.labels).toBeNull();
    expect(mode.reason).toContain("2 of its judgements returned");
  });
});

describe("one case as one arm judged it", () => {
  test("a case with two judgements returned is undecided and is never a disagreement", () => {
    const row = caseRow("case-1");
    const rows = [
      judgementRow("bare-opus", "case-1", 0, [APPROVE]),
      judgementRow("bare-opus", "case-1", 1, [APPROVE]),
      judgementRow("bare-opus", "case-1", 2, [], { fault: true }),
      judgementRow("bare-opus", "case-1", 3, [], { fault: true }),
      judgementRow("bare-opus", "case-1", 4, [], { fault: true }),
    ];
    const reading = readCase(rows, row);
    expect(reading.returned).toBe(2);
    expect(reading.faulted).toBe(3);
    expect(reading.verdict).toBeNull();
    expect(reading.agreed).toBe(false);
    expect(reading.confidence).toBe(0);
  });

  test("no case leaves the coverage denominator because a judgement faulted", () => {
    const rows = [caseRow("case-1"), caseRow("case-2")];
    const judgements = [
      ...fiveRows("bare-opus", "case-1", 5),
      ...fiveRows("bare-opus", "case-2", 5, { faulted: 5 }),
    ];
    const { arms } = readRound(judgements, corpusOf(rows).byCaseId);
    expect(arms[0]?.cases).toHaveLength(2);
    expect(arms[0]?.cases.filter((one) => one.verdict === null)).toHaveLength(1);
  });

  test("agreement is set equality with the person's own answer", () => {
    const row = caseRow("case-2", { selected: [CHANGES] });
    expect(readCase(fiveRows("bare-opus", "case-2", 5), row).agreed).toBe(false);
    expect(readCase(fiveRows("bare-opus", "case-2", 0), row).agreed).toBe(true);
  });
});

describe("the paired comparison with the baseline", () => {
  test("McNemar's exact test over a hand-computed table", () => {
    // 20 cases both got right, 8 the arm alone, 2 the baseline alone, 5 neither.
    const cases = [
      ...Array.from({ length: 20 }, () => ({ armAgreed: true, otherAgreed: true })),
      ...Array.from({ length: 8 }, () => ({ armAgreed: true, otherAgreed: false })),
      ...Array.from({ length: 2 }, () => ({ armAgreed: false, otherAgreed: true })),
      ...Array.from({ length: 5 }, () => ({ armAgreed: false, otherAgreed: false })),
    ];
    const paired = mcnemarExact(cases);
    expect(paired.counted).toBe(35);
    expect(paired.armOnly).toBe(8);
    expect(paired.otherOnly).toBe(2);
    expect(paired.discordant).toBe(10);
    expect(paired.armShare).toBeCloseTo(28 / 35, 10);
    expect(paired.otherShare).toBeCloseTo(22 / 35, 10);
    expect(paired.difference).toBeCloseTo(6 / 35, 10);
    // 2 * P(X <= 2) for ten draws at one half: 2 * (1 + 10 + 45) / 1024.
    expect(paired.pValue).toBeCloseTo(0.109_375, 10);
    expect(paired.interval.lower).toBeLessThan(paired.difference);
    expect(paired.interval.upper).toBeGreaterThan(paired.difference);
  });

  test("two readings that never differ carry a p of 1 and an interval of zero", () => {
    const paired = mcnemarExact([
      { armAgreed: true, otherAgreed: true },
      { armAgreed: false, otherAgreed: false },
    ]);
    expect(paired.discordant).toBe(0);
    expect(paired.pValue).toBe(1);
    expect(paired.interval).toEqual({ lower: 0, upper: 0 });
  });
});

/**
 * One arm over a fit slice that fits a threshold of 1.0 and a report slice that is read at it:
 * 45 fit cases its five judgements all agreed on and the person agreed with, 20 it split three
 * to two on, five of which the person answered the other way; then 60 report cases of the first
 * shape and 40 of the second.
 */
function fittedArm(
  arm: string,
  options: { faulted?: number } = {}
): {
  corpus: Corpus;
  rows: JudgementRow[];
} {
  const cases: CaseRow[] = [];
  const rows: JudgementRow[] = [];
  const add = (
    caseId: string,
    slice: SliceName,
    chose: number,
    selected: string[],
    faulted = 0
  ): void => {
    cases.push(caseRow(caseId, { selected, artifactId: `${caseId}-document` }));
    rows.push(...fiveRows(arm, caseId, chose, { slice, faulted }));
  };
  for (let index = 0; index < 45; index += 1) add(`fit-clean-${index}`, "fit", 5, [APPROVE]);
  for (let index = 0; index < 15; index += 1) add(`fit-split-${index}`, "fit", 3, [APPROVE]);
  for (let index = 0; index < 5; index += 1) add(`fit-wrong-${index}`, "fit", 3, [CHANGES]);
  const faulted = options.faulted ?? 0;
  for (let index = 0; index < 60; index += 1) {
    add(`report-clean-${index}`, "report", 5, [APPROVE], index < faulted ? 5 : 0);
  }
  for (let index = 0; index < 40; index += 1) add(`report-split-${index}`, "report", 3, [CHANGES]);
  return { corpus: corpusOf(cases), rows };
}

describe("the threshold, fitted on the fit slice and applied to the report slice", () => {
  const { corpus, rows } = fittedArm("bare-opus");
  const analysis = analyseRound({
    corpus,
    roundDirectory: "/nowhere/round",
    rows,
    rowsRead: rows.length,
    rowsUnreadable: 0,
    summary: null,
    slices: null,
    groupKey: null,
    evidence: [],
    cut: null,
    confidenceTable: false,
    confidenceArm: null,
    wantedCases: null,
    seed: null,
  });
  const arm = analysis.arms[0];

  test("the threshold is the lowest fit-slice value whose bound holds the rate", () => {
    expect(arm?.approval.fit.cases).toBe(65);
    expect(arm?.approval.fit.decided).toBe(65);
    expect(arm?.approval.fit.threshold).toBe(1);
  });

  test("the report slice is read at that threshold and not at one fitted on itself", () => {
    expect(arm?.approval.cases).toBe(100);
    expect(arm?.approval.atThreshold.decided).toBe(60);
    expect(arm?.approval.atThreshold.coverage).toBeCloseTo(0.6, 10);
    expect(arm?.approval.atThreshold.disagreements).toBe(0);
    // 60 decided with no disagreement: 1 - 0.10 ^ (1 / 60), the closed form of the bound.
    expect(arm?.approval.atThreshold.bound).toBeCloseTo(0.037_649, 5);
    expect(arm?.approval.atThreshold.adjustedBound).toBeGreaterThan(
      arm?.approval.atThreshold.bound ?? 0
    );
  });

  test("agreement, the baseline and the paired difference are read over the same cases", () => {
    expect(arm?.approval.agreement.count).toBe(60);
    expect(arm?.approval.agreement.total).toBe(100);
    expect(arm?.approval.baseline.counted).toBe(100);
    expect(arm?.approval.baseline.approvals).toBe(60);
    expect(arm?.approval.paired.counted).toBe(100);
    expect(arm?.approval.paired.armOnly).toBe(0);
    expect(arm?.approval.paired.otherOnly).toBe(0);
  });

  test("the adjusted bound is read wherever the bound is, on that same decided count", () => {
    // The count a bound needs is the only thing either of them turns on: an arm whose bound is
    // read always has one of those too, and an arm whose bound is not read has neither.
    const at = arm?.approval.atThreshold;
    expect(at?.decided).toBeGreaterThanOrEqual(MINIMUM_DECIDED);
    expect(at?.bound).not.toBeNull();
    expect(at?.adjustedBound).not.toBeNull();
  });

  test("the question cases are reported apart and are empty here", () => {
    expect(arm?.question.cases).toBe(0);
    expect(arm?.question.agreement.bound).toBeNull();
    expect(arm?.question.agreement.noBound).toBe("no case at this count");
  });

  test("the sensitivity readings count one case per document", () => {
    expect(arm?.approval.sensitivity.oneCasePerDocument.total).toBe(100);
    expect(arm?.approval.sensitivity.first.total).toBe(100);
    expect(arm?.approval.sensitivity.modal.count).toBe(60);
  });
});

describe("an arm missing more than 5 percent of its judgements", () => {
  const { corpus, rows } = fittedArm("bare-opus", { faulted: 12 });
  const analysis = analyseRound({
    corpus,
    roundDirectory: "/nowhere/round",
    rows,
    rowsRead: rows.length,
    rowsUnreadable: 0,
    summary: null,
    slices: null,
    groupKey: null,
    evidence: [],
    cut: null,
    confidenceTable: false,
    confidenceArm: null,
    wantedCases: null,
    seed: null,
  });
  const arm = analysis.arms[0];

  test("it is named as missing them, with the share it is missing", () => {
    expect(arm?.missingShare).toBeGreaterThan(0.05);
    expect(arm?.bounded).toBe(false);
    expect(analysis.round.armsWithoutBound).toEqual(["bare-opus"]);
  });

  test("it carries no bound, and every number of it says so instead", () => {
    expect(arm?.approval.agreement.bound).toBeNull();
    expect(arm?.approval.agreement.noBound).toContain("missing");
    expect(arm?.approval.atThreshold.bound).toBeNull();
    expect(arm?.approval.atThreshold.noBound).toContain("missing");
    expect(arm?.approval.anchor.bound).toBeNull();
    expect(arm?.approval.atThreshold.adjustedBound).toBeNull();
    expect(arm?.approval.cutFree.agreement.bound).toBeNull();
  });
});

describe("the winner rule", () => {
  /** One arm as the rule reads it, the fields it does not read left at nothing. */
  function covered(
    arm: string,
    coverage: number,
    bound: number | null,
    interval: [number, number],
    costPerCase: number | null
  ): ArmCoverage {
    return {
      arm,
      threshold: 0.8,
      decided: Math.round(coverage * 250),
      total: 250,
      coverage,
      interval: { lower: interval[0], upper: interval[1] },
      bound,
      costPerCase,
    };
  }

  test("the winner is the highest coverage among the arms whose bound holds the rate", () => {
    const decision = chooseWinner([
      covered("a", 0.7, 0.08, [0.64, 0.75], 0.01),
      covered("b", 0.6, 0.04, [0.54, 0.66], 0.02),
      covered("c", 0.45, 0.03, [0.39, 0.51], 0.01),
    ]);
    expect(decision.eligible).toEqual(["b", "c"]);
    expect(decision.winner).toBe("b");
    expect(decision.byCost).toBe(false);
    expect(decision.useful).toBe(true);
  });

  test("where two coverage intervals overlap the lower cost per case wins, and it says so", () => {
    const decision = chooseWinner([
      covered("dear", 0.62, 0.04, [0.56, 0.68], 0.05),
      covered("cheap", 0.6, 0.04, [0.54, 0.66], 0.002),
    ]);
    expect(decision.overlapping).toEqual(["cheap"]);
    expect(decision.winner).toBe("cheap");
    expect(decision.byCost).toBe(true);
    expect(decision.lines.join(" ")).toContain("lower measured cost per case");
  });

  test("under the usefulness floor the report records the curve and no verdict rule", () => {
    const decision = chooseWinner([covered("thin", 0.2, 0.03, [0.15, 0.26], 0.01)]);
    expect(decision.winner).toBe("thin");
    expect(decision.useful).toBe(false);
    expect(decision.lines.join(" ")).toContain("no verdict rule");
  });

  test("no arm holding the rate means no winner at all", () => {
    const decision = chooseWinner([
      covered("a", 0.7, 0.2, [0.64, 0.75], 0.01),
      covered("b", 0.5, null, [0.44, 0.56], 0.01),
    ]);
    expect(decision.winner).toBeNull();
    expect(decision.eligible).toEqual([]);
    expect(decision.lines.join(" ")).toContain("names no winner");
  });
});

describe("the class table, which the corpus alone answers", () => {
  const rows = [
    caseRow("a1", { project: "INFRA", selected: [APPROVE] }),
    caseRow("a2", { project: "INFRA", selected: [APPROVE] }),
    caseRow("a3", { project: "INFRA", selected: [CHANGES], changesAlreadyRequested: true }),
    caseRow("a4", { project: "BET", selected: [APPROVE] }),
    caseRow("q1", { project: "INFRA", kind: "question", selected: ["Later"] }),
  ];
  const table = classTable(rows);
  const lineOf = (dimension: string, className: string) =>
    table.find((line) => line.dimension === dimension && line.className === className);

  test("a project's share is its approval decisions, and its count is beside it", () => {
    const line = lineOf("by project", "INFRA");
    expect(line?.cases).toBe(4);
    expect(line?.counted).toBe(3);
    expect(line?.approvals).toBe(2);
    expect(line?.share).toBeCloseTo(2 / 3, 10);
  });

  test("a question case counts in the class and not in its share", () => {
    const line = lineOf("by kind", "question");
    expect(line?.cases).toBe(1);
    expect(line?.counted).toBe(0);
    expect(line?.share).toBe(0);
  });

  test("the split by changes already requested reads the corpus's own confound", () => {
    expect(lineOf("by changes already requested on that document", "yes")?.counted).toBe(1);
    expect(lineOf("by changes already requested on that document", "yes")?.approvals).toBe(0);
    expect(lineOf("by changes already requested on that document", "no")?.approvals).toBe(3);
  });

  test("a class under the floor carries no bound, and says so", () => {
    const line = lineOf("by project", "BET");
    expect(line?.bound).toBeNull();
    expect(line?.noBound).toContain(`a bound needs ${MINIMUM_DECIDED}`);
  });

  test("a class over the floor carries one", () => {
    const many = Array.from({ length: 50 }, (_, index) =>
      caseRow(`m${index}`, { project: "INFRA", selected: [APPROVE] })
    );
    const line = classTable(many).find((one) => one.className === "INFRA");
    expect(line?.counted).toBe(50);
    expect(line?.share).toBe(1);
    // 50 approvals and nothing else: 1 - 0.10 ^ (1 / 50) bounds the share that went otherwise.
    expect(line?.bound).toBeCloseTo(0.045_007, 5);
  });
});

/** An analysis over hand-written cases, rows, slices and evidence, and nothing read off a disk. */
function analysisOf(options: {
  cases: readonly CaseRow[];
  rows: readonly JudgementRow[];
  slices?: Record<string, string[]> | null;
  groupKey?: string | null;
  evidence?: CaseNeighbours[];
  confidenceTable?: boolean;
  confidenceArm?: string | null;
}): Analysis {
  return analyseRound({
    corpus: corpusOf(options.cases),
    roundDirectory: "/nowhere/round",
    rows: options.rows,
    rowsRead: options.rows.length,
    rowsUnreadable: 0,
    summary: null,
    slices: options.slices ?? null,
    groupKey: options.groupKey ?? null,
    evidence: options.evidence ?? [],
    cut: null,
    confidenceTable: options.confidenceTable ?? false,
    confidenceArm: options.confidenceArm ?? null,
    wantedCases: null,
    seed: null,
  });
}

describe("an issue whose cases sit in the fit slice and the report slice", () => {
  const cases = [
    caseRow("fit-1", { issueKey: "INFRA-760", artifactId: "doc-a" }),
    caseRow("report-1", { issueKey: "INFRA-760", artifactId: "doc-b" }),
    caseRow("fit-2", { issueKey: "INFRA-900" }),
    caseRow("report-2", { issueKey: "INFRA-901" }),
  ];
  const rows = [
    ...fiveRows("bare-opus", "fit-1", 5, { slice: "fit" }),
    ...fiveRows("bare-opus", "report-1", 5, { slice: "report" }),
    ...fiveRows("bare-opus", "fit-2", 5, { slice: "fit" }),
    ...fiveRows("bare-opus", "report-2", 5, { slice: "report" }),
  ];
  const analysis = analysisOf({
    cases,
    rows,
    slices: { fit: ["fit-1", "fit-2"], report: ["report-1", "report-2"], anchor: [] },
  });
  const crossing = analysis.leakage.crossings[0];

  test("the crossing names the issue, its cases and the slices they are in", () => {
    expect(analysis.leakage.crossings.map((one) => one.issue)).toEqual(["INFRA-760"]);
    expect(crossing?.slices).toEqual(["fit", "report"]);
    expect(crossing?.cases.map((one) => [one.caseId, one.slice])).toEqual([
      ["fit-1", "fit"],
      ["report-1", "report"],
    ]);
  });

  test("the fit case is dropped and nothing of the report slice is", () => {
    expect(analysis.leakage.droppedFromFit).toEqual(["fit-1"]);
    expect(crossing?.droppedFromFit).toEqual(["fit-1"]);
    expect(analysis.leakage.shortfalls).toEqual([]);
    const arm = analysis.arms[0];
    expect(arm?.approval.fit.cases).toBe(1);
    expect(arm?.approval.fit.dropped).toBe(1);
    expect(arm?.approval.cases).toBe(2);
  });

  test("the fit slice's own count stands beside its target", () => {
    expect(analysis.leakage.fit.approval).toEqual({
      drawn: 2,
      kept: 1,
      target: TARGETS.approval.fit,
    });
  });

  test("the report names the dropped case and why it was dropped", () => {
    const text = renderReport(analysis).join("\n");
    expect(text).toContain("INFRA-760");
    expect(text).toContain("fit-1");
    expect(text).toContain("dropped from the fit slice");
  });
});

describe("an issue whose cases sit in the report slice and the anchor slice", () => {
  const cases = [
    caseRow("report-1", { issueKey: "INFRA-760", artifactId: "doc-a" }),
    caseRow("anchor-1", { issueKey: "INFRA-760", artifactId: "doc-b" }),
  ];
  const rows = [
    ...fiveRows("bare-opus", "report-1", 5, { slice: "report" }),
    ...fiveRows("bare-opus", "anchor-1", 5, { slice: "anchor" }),
  ];
  const analysis = analysisOf({
    cases,
    rows,
    slices: { fit: [], report: ["report-1"], anchor: ["anchor-1"] },
  });

  test("nothing is dropped, and the issue is named as a shortfall", () => {
    expect(analysis.leakage.crossings.map((one) => one.issue)).toEqual(["INFRA-760"]);
    expect(analysis.leakage.crossings[0]?.reportAndAnchor).toBe(true);
    expect(analysis.leakage.droppedFromFit).toEqual([]);
    expect(analysis.leakage.shortfalls).toEqual(["INFRA-760"]);
    expect(analysis.arms[0]?.approval.cases).toBe(1);
    expect(analysis.arms[0]?.approval.anchor.total).toBe(1);
  });

  test("the report says nothing was dropped for it and calls it a shortfall", () => {
    const text = renderReport(analysis).join("\n");
    expect(text).toContain("shortfall");
    expect(text).toContain("kept, and every number reads it");
  });
});

describe("false approvals and false requests for changes, over a hand table", () => {
  const cases: CaseRow[] = [];
  const rows: JudgementRow[] = [];
  // Three the person asked changes of and the arm approved, two the person approved and the arm
  // asked changes of, and four both approved.
  for (let index = 0; index < 3; index += 1) {
    cases.push(caseRow(`let-through-${index}`, { selected: [CHANGES] }));
    rows.push(...fiveRows("bare-opus", `let-through-${index}`, 5));
  }
  for (let index = 0; index < 2; index += 1) {
    cases.push(caseRow(`held-back-${index}`, { selected: [APPROVE] }));
    rows.push(...fiveRows("bare-opus", `held-back-${index}`, 0));
  }
  for (let index = 0; index < 4; index += 1) {
    cases.push(caseRow(`agreed-${index}`, { selected: [APPROVE] }));
    rows.push(...fiveRows("bare-opus", `agreed-${index}`, 5));
  }
  const analysis = analysisOf({ cases, rows });
  const errors = analysis.arms[0]?.approval.errors?.report;

  test("the two kinds are counted apart, each over the cases the arm decided", () => {
    expect(errors?.decided).toBe(9);
    expect(errors?.falseApprovals.count).toBe(3);
    expect(errors?.falseApprovals.share).toBeCloseTo(3 / 9, 10);
    expect(errors?.falseRequests.count).toBe(2);
    expect(errors?.falseRequests.share).toBeCloseTo(2 / 9, 10);
    expect(errors?.other).toBe(0);
  });

  test("the single disagreement rate still stands, since the bound is computed on it", () => {
    const agreement = analysis.arms[0]?.approval.agreement;
    expect(agreement?.count).toBe(4);
    expect(agreement?.total).toBe(9);
  });

  test("a question case carries no such count, the two kinds being of approval cases", () => {
    expect(analysis.arms[0]?.question.errors).toBeNull();
  });

  test("the report prints both kinds", () => {
    const text = renderReport(analysis).join("\n");
    expect(text).toContain("false approvals");
    expect(text).toContain("false requests for changes");
  });
});

describe("the manifest of each slice", () => {
  const cases = [
    caseRow("fit-1", { issueKey: "INFRA-760" }),
    caseRow("fit-2", { issueKey: "INFRA-900" }),
    caseRow("report-1", { issueKey: "INFRA-760" }),
    caseRow("anchor-1", { issueKey: "INFRA-901" }),
  ];
  const analysis = analysisOf({
    cases,
    rows: fiveRows("bare-opus", "report-1", 5),
    slices: { fit: ["fit-1", "fit-2"], report: ["report-1"], anchor: ["anchor-1"] },
  });

  test("each slice carries its role, its count and the hash of its id list", () => {
    expect(analysis.manifests.map((one) => one.slice)).toEqual(["fit", "report", "anchor"]);
    const report = analysis.manifests.find((one) => one.slice === "report");
    expect(report?.role).toContain("test set");
    expect(report?.count).toBe(1);
    expect(report?.caseIds).toEqual(["report-1"]);
    expect(report?.sha256).toBe(createHash("sha256").update("report-1", "utf8").digest("hex"));
    expect(analysis.manifests.find((one) => one.slice === "anchor")?.role).toContain("fixed set");
  });

  test("the fit manifest holds what the crossing left it, and hashes that list", () => {
    const fit = analysis.manifests.find((one) => one.slice === "fit");
    expect(fit?.role).toContain("calibration set");
    expect(fit?.caseIds).toEqual(["fit-2"]);
    expect(fit?.dropped).toEqual(["fit-1"]);
    expect(fit?.count).toBe(1);
    expect(fit?.sha256).toBe(manifestHash(["fit-2"]));
    expect(fit?.sha256).toBe(createHash("sha256").update("fit-2", "utf8").digest("hex"));
  });

  test("the hash of a longer list is the hash of the ids joined by one newline", () => {
    expect(manifestHash(["a", "b", "c"])).toBe(
      createHash("sha256").update("a\nb\nc", "utf8").digest("hex")
    );
  });
});

describe("the proof that retrieval returned no neighbour on the judged case's own document", () => {
  const cases = [
    caseRow("judged", { artifactId: "doc-a" }),
    caseRow("near", { artifactId: "doc-a" }),
    caseRow("far", { artifactId: "doc-b" }),
  ];
  const rows = fiveRows("bare-opus", "judged", 5, { excluded: 2 });

  test("the count of excluded neighbours comes from the rows, and the leak count is zero", () => {
    const analysis = analysisOf({
      cases,
      rows,
      evidence: [{ caseId: "judged", neighbours: ["far"], excluded: 2 }],
    });
    expect(analysis.retrieval.rowsCounted).toBe(5);
    expect(analysis.retrieval.excluded).toBe(10);
    expect(analysis.retrieval.casesRead).toBe(1);
    expect(analysis.retrieval.neighbours).toBe(1);
    expect(analysis.retrieval.onOwnDocument).toEqual([]);
    expect(analysis.retrieval.rowsOnOwnDocument).toBe(0);
  });

  test("a neighbour on the judged case's own document is found and named", () => {
    const analysis = analysisOf({
      cases,
      rows,
      evidence: [{ caseId: "judged", neighbours: ["near", "far"], excluded: 0 }],
    });
    expect(analysis.retrieval.onOwnDocument).toEqual([
      { caseId: "judged", neighbour: "near", artifactId: "doc-a" },
    ]);
    expect(analysis.retrieval.rowsOnOwnDocument).toBe(5);
    expect(renderReport(analysis).join("\n")).toContain("the document both name");
  });
});

describe("the arm the confidence table is read on", () => {
  // Six approval cases of the report slice, four the person approved and two the person asked
  // for changes on, judged by two recipes on a generative model and the same two on Jev. The
  // bare arm agrees on two of the six and the many-shot arm on five, which is the one thing
  // the default reads, and the bare arm is the first of the two by name, which it no longer is.
  const cases = [
    caseRow("a1"),
    caseRow("a2"),
    caseRow("a3"),
    caseRow("a4"),
    caseRow("c1", { selected: [CHANGES] }),
    caseRow("c2", { selected: [CHANGES] }),
  ];

  /** One arm over all six cases, `chose[at]` of each case's five judgements approving. */
  function armRows(
    arm: string,
    recipe: RecipeName,
    endpoint: Endpoint,
    chose: readonly number[]
  ): JudgementRow[] {
    return cases.flatMap((row, at) =>
      fiveRows(arm, row.caseId, chose[at] ?? 0, {
        recipe,
        endpoint,
        confidence: endpoint === "jev" ? 0.9 : undefined,
      })
    );
  }

  const rows = [
    ...armRows("bare-opus-5-5", "bare", "claude", [5, 0, 0, 0, 5, 0]),
    ...armRows("many-shot-opus-5-5", "many-shot", "claude", [5, 4, 3, 5, 0, 5]),
    ...armRows("bare-jev", "bare", "jev", [5, 5, 5, 5, 5, 5]),
    ...armRows("many-shot-jev", "many-shot", "jev", [5, 5, 5, 5, 0, 0]),
  ];
  const { arms } = readRound(rows, corpusOf(cases).byCaseId);

  test("with no arm named, the table is read on the generative arm that agrees most", () => {
    const table = confidenceTableOf(arms, null, null, null);
    expect(table.arm).toBe("many-shot-opus-5-5");
    expect(table.armChoice).toBe("best");
    expect(table.recipe).toBe("many-shot");
    // Five of the six approval cases of the report slice, against two of six for the bare arm.
    expect(table.armAgreement?.count).toBe(5);
    expect(table.armAgreement?.total).toBe(6);
    expect(confidenceTableOf(arms, null, null, "bare-opus-5-5").armAgreement?.count).toBe(2);
  });

  test("the arm the caller names is the arm the table is read on", () => {
    const table = confidenceTableOf(arms, null, null, "bare-opus-5-5");
    expect(table.arm).toBe("bare-opus-5-5");
    expect(table.armChoice).toBe("named");
    expect(table.recipe).toBe("bare");
  });

  test("an arm the round has no rows for is refused, naming the arms it has", () => {
    expect(() => confidenceTableOf(arms, null, null, "many-shot-fable-5-1")).toThrow(
      "--confidence-arm many-shot-fable-5-1 names no generative arm of this round. The " +
        "generative arms are bare-opus-5-5, many-shot-opus-5-5"
    );
    expect(() => confidenceTableOf(arms, null, null, "bare-jev")).toThrow(
      "names no generative arm of this round"
    );
  });

  test("the Jev half is read on the Jev arm of the generative arm's own recipe", () => {
    expect(confidenceTableOf(arms, null, null, null).jevArm).toBe("many-shot-jev");
    expect(confidenceTableOf(arms, null, null, "bare-opus-5-5").jevArm).toBe("bare-jev");
  });

  test("the report names each half's arm, how the generative one was chosen, and the recipe", () => {
    const text = renderReport(analysisOf({ cases, rows, confidenceTable: true })).join("\n");
    expect(text).toContain("The generative arm is many-shot-opus-5-5, of the many-shot recipe");
    expect(text).toContain("chosen as the generative arm that agrees most");
    expect(text).toContain("That arm agrees 0.833 over the 6 approval cases of the report slice");
    expect(text).toContain("The arm is many-shot-jev, the Jev arm of the many-shot recipe");
    const named = renderReport(
      analysisOf({ cases, rows, confidenceTable: true, confidenceArm: "bare-opus-5-5" })
    ).join("\n");
    expect(named).toContain("The generative arm is bare-opus-5-5, of the bare recipe, named by");
    expect(named).toContain("The arm is bare-jev, the Jev arm of the bare recipe");
  });
});

describe("an arm that decided nothing", () => {
  const cases = [caseRow("a1"), caseRow("a2")];
  const rows = [
    ...fiveRows("silent-opus-5-5", "a1", 0, { faulted: 5 }),
    ...fiveRows("silent-opus-5-5", "a2", 0, { faulted: 5 }),
    ...fiveRows("steady-opus-5-5", "a1", 5),
    ...fiveRows("steady-opus-5-5", "a2", 5),
  ];

  test("is no arm whose judgements are identical, since it judged none of them", () => {
    const { arms } = readRound(rows, corpusOf(cases).byCaseId);
    expect(arms.find((arm) => arm.name === "silent-opus-5-5")?.cases).toHaveLength(2);
    expect(confidenceTableOf(arms, null, null, null).identicalArms).toEqual(["steady-opus-5-5"]);
  });

  test("is not named by the report among the arms whose confidence takes one value", () => {
    const text = renderReport(analysisOf({ cases, rows })).join("\n");
    const line = text
      .split("\n")
      .find((one) => one.includes("judgements are identical on every case they decided"));
    expect(line).toContain("steady-opus-5-5");
    expect(line).not.toContain("silent-opus-5-5");
  });
});

/** The directories this file's last block made, each removed by its own exact path. */
const made: string[] = [];

afterEach(async () => {
  for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
});

/** One scratch directory under the scratch volume, never under the workspace. */
async function scratch(): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "analyze-"));
  made.push(directory);
  return directory;
}

/** What one run of the command printed on stderr, with the status it returned. */
async function run(argv: readonly string[]): Promise<{ printed: string; exitCode: number }> {
  const lines: string[] = [];
  const error = spyOn(console, "error").mockImplementation((line?: unknown) => {
    lines.push(String(line));
  });
  const log = spyOn(console, "log").mockImplementation(() => {});
  let exitCode: number;
  try {
    exitCode = await main(argv);
  } finally {
    error.mockRestore();
    log.mockRestore();
  }
  return { printed: lines.join("\n"), exitCode };
}

/** What one run of the command printed on stdout, with the status it returned. */
async function runReading(argv: readonly string[]): Promise<{ printed: string; exitCode: number }> {
  const lines: string[] = [];
  const log = spyOn(console, "log").mockImplementation((line?: unknown) => {
    lines.push(String(line));
  });
  const error = spyOn(console, "error").mockImplementation(() => {});
  let exitCode: number;
  try {
    exitCode = await main(argv);
  } finally {
    log.mockRestore();
    error.mockRestore();
  }
  return { printed: lines.join("\n"), exitCode };
}

/**
 * A corpus and a round the confidence table can be read over: forty approval cases of the
 * report slice, twenty the person approved and twenty he asked for changes on, each judged
 * five times by one generative arm, by the Jev arm of the same recipe, and by two generative
 * arms of another recipe that always approve. Four arms over three models is the floor the
 * report is read at, and the three, four or five judgements of the forty that chose the
 * person's own answer give the first arm's counted confidence three values to take.
 */
async function tableFixture(): Promise<{ corpus: string; round: string }> {
  const corpus = await scratch();
  const round = await scratch();
  const rows: CaseRow[] = [];
  const judged: JudgementRow[] = [];
  for (let at = 0; at < 40; at += 1) {
    const caseId = `case-${String(at).padStart(2, "0")}`;
    const approved = at % 2 === 0;
    rows.push(caseRow(caseId, { selected: approved ? [APPROVE] : [CHANGES] }));
    const agreeing = 3 + (at % 3);
    for (let judgement = 0; judgement < 5; judgement += 1) {
      const chose = judgement < agreeing === approved ? [APPROVE] : [CHANGES];
      judged.push(
        judgementRow("many-shot-one", caseId, judgement, chose, { recipe: "many-shot" }),
        judgementRow("many-shot-jev", caseId, judgement, chose, {
          recipe: "many-shot",
          endpoint: "jev",
          confidence: 0.7,
        }),
        judgementRow("bare-one", caseId, judgement, [APPROVE], { recipe: "bare" }),
        judgementRow("bare-fable", caseId, judgement, [APPROVE], {
          recipe: "bare",
          model: "anthropic/claude-fable-5-1",
        })
      );
    }
  }
  await writeFile(
    join(corpus, "snapshot.json"),
    `${JSON.stringify({ person: "someone", throughEventId: 1 })}\n`,
    "utf8"
  );
  await writeFile(
    join(corpus, "corpus.jsonl"),
    `${rows.map((row) => JSON.stringify(row)).join("\n")}\n`,
    "utf8"
  );
  await writeFile(
    join(round, "rows.jsonl"),
    `${judged.map((row) => JSON.stringify(row)).join("\n")}\n`,
    "utf8"
  );
  return { corpus, round };
}

describe("the confidence table is the same on two runs of one command", () => {
  test("two runs over one corpus and one round read the same thirty cases", async () => {
    const { corpus, round } = await tableFixture();
    const argv = ["--corpus", corpus, "--round", round, "--confidence-table", "--format", "json"];
    const one = await runReading(argv);
    const two = await runReading(argv);
    expect(one.exitCode).toBe(0);
    expect(two.exitCode).toBe(0);
    const first = (JSON.parse(one.printed) as Analysis).confidenceTable;
    const second = (JSON.parse(two.printed) as Analysis).confidenceTable;
    expect(first?.caseIds).toHaveLength(30);
    // The check this table answers is judged on what the table shows, so two runs of one command
    // over one round must show it over the same cases: a set drawn afresh each run makes the
    // check's verdict a draw rather than a reading of the round.
    expect(second?.caseIds).toEqual(first?.caseIds as string[]);
  });

  test("the seed it printed draws those same cases again, and another seed others", async () => {
    const { corpus, round } = await tableFixture();
    const argv = ["--corpus", corpus, "--round", round, "--confidence-table", "--format", "json"];
    const drawn = (JSON.parse((await runReading(argv)).printed) as Analysis).confidenceTable;
    const seed = drawn?.seed as number;
    expect(Number.isInteger(seed)).toBe(true);
    expect(drawn?.seedChoice).toBe("derived");
    const again = await runReading([...argv, "--confidence-seed", String(seed)]);
    const read = (JSON.parse(again.printed) as Analysis).confidenceTable;
    expect(read?.seedChoice).toBe("named");
    expect(read?.caseIds).toEqual(drawn?.caseIds as string[]);
    const other = await runReading([...argv, "--confidence-seed", String(seed + 1)]);
    const others = (JSON.parse(other.printed) as Analysis).confidenceTable;
    expect(others?.seed).toBe(seed + 1);
    expect(others?.caseIds).not.toEqual(drawn?.caseIds as string[]);
  });

  test("the report names the seed and reads the thirty against the whole slice", async () => {
    const { corpus, round } = await tableFixture();
    const argv = ["--corpus", corpus, "--round", round, "--confidence-table", "--confidence-seed"];
    const { printed, exitCode } = await runReading([...argv, "77"]);
    expect(exitCode).toBe(0);
    expect(printed).toContain("drawn under seed 77");
    expect(printed).toContain("--confidence-seed 77 to read the same cases");
    const table = (
      JSON.parse((await runReading([...argv, "77", "--format", "json"])).printed) as Analysis
    ).confidenceTable;
    // The thirty are a sample of the forty approval cases of the report slice, and the whole
    // slice is printed beside them so a sample taking one value is read against the slice.
    expect(table?.sliceDecided).toBe(40);
    expect(table?.sliceLines.reduce((total, line) => total + line.decided, 0)).toBe(40);
    expect(table?.sliceLines.map((line) => line.name)).toEqual(["0.60", "0.80", "1.00"]);
    expect(table?.lines.reduce((total, line) => total + line.decided, 0)).toBe(30);
  });
});

describe("the report refuses a round it cannot be read over", () => {
  test("fewer than four arms is a non-zero status naming the arms and the models", async () => {
    const corpus = await scratch();
    const round = await scratch();
    const rows = [caseRow("case-1"), caseRow("case-2")];
    const judged = rows.flatMap((row) => [
      ...fiveRows("bare-one", row.caseId, 5),
      ...fiveRows("bare-jev", row.caseId, 5, { endpoint: "jev", confidence: 0.7 }),
    ]);
    await writeFile(
      join(corpus, "snapshot.json"),
      `${JSON.stringify({ person: "someone", throughEventId: 1 })}\n`,
      "utf8"
    );
    await writeFile(
      join(corpus, "corpus.jsonl"),
      `${rows.map((row) => JSON.stringify(row)).join("\n")}\n`,
      "utf8"
    );
    await writeFile(
      join(round, "rows.jsonl"),
      `${judged.map((row) => JSON.stringify(row)).join("\n")}\n`,
      "utf8"
    );
    const { printed, exitCode } = await run(["--corpus", corpus, "--round", round]);
    // The report compares arms and chooses a winner among them, so a round of two arms on two
    // models answers none of it: the status says so, and the words say what the round held.
    expect(exitCode).toBe(1);
    expect(printed).toContain("at least 4 arms and 3 models");
    expect(printed).toContain("2 arms (bare-jev, bare-one)");
    expect(printed).toContain("2 models (anthropic/claude-opus-5-5, jev-latest)");
  });

  test("the floor is met by the four arms and three models of the table fixture", async () => {
    const { corpus, round } = await tableFixture();
    const { exitCode } = await run(["--corpus", corpus, "--round", round]);
    expect(exitCode).toBe(0);
  });
});

describe("the command over files it cannot read", () => {
  test("a slices file that is not JSON is a refusal and not a thrown error", async () => {
    const corpus = await scratch();
    await mkdir(join(corpus, "slices"), { recursive: true });
    await writeFile(
      join(corpus, "snapshot.json"),
      `${JSON.stringify({ person: "someone", throughEventId: 1 })}\n`,
      "utf8"
    );
    await writeFile(join(corpus, "corpus.jsonl"), `${JSON.stringify(caseRow("case-1"))}\n`, "utf8");
    await writeFile(join(corpus, "slices", "slices.json"), "{not json at all\n", "utf8");
    const { printed, exitCode } = await run(["--corpus", corpus, "--round", await scratch()]);
    expect(exitCode).toBe(2);
    expect(printed).toContain("slices.json is not JSON");
  });

  test("--cases without the table it names the cases of is refused, as its sibling is", async () => {
    const argv = ["--corpus", "/nowhere", "--round", "/nowhere", "--cases", "/nowhere/cases.txt"];
    const { printed, exitCode } = await run(argv);
    expect(exitCode).toBe(2);
    expect(printed).toContain("--cases names the cases --confidence-table is read over");
    expect(printed).toContain("that table was not asked for");
  });
});
