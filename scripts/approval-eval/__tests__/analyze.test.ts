import { describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import {
  type Analysis,
  type ArmCoverage,
  analyseRound,
  type CaseNeighbours,
  chooseWinner,
  classTable,
  manifestHash,
  modeOf,
  readCase,
  readRound,
  renderReport,
} from "../analyze";
import type { CaseKind, CaseRow, CaseVerdict } from "../build-corpus";
import type { Corpus } from "../corpus-io";
import { type SliceName, TARGETS } from "../draw";
import type { JudgementRow } from "../run-round";
import { MINIMUM_DECIDED, mcnemarExact } from "../stats";

/**
 * The report over hand-written rows: nothing here reads a corpus directory, a round directory or
 * Dispatch, and nothing makes a call. What it holds the reading to is the rules the plan names -
 * the mode and its tie, the floor of three judgements that returned, the paired comparison with
 * the baseline, a threshold fitted on the fit slice and applied to the report slice, the winner
 * rule with its cost tie-break and its usefulness floor, the class table the corpus alone
 * answers, and the arm too short of judgements to carry a bound.
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
  } = {}
): JudgementRow {
  const faulted = options.fault === true;
  return {
    arm,
    recipe: "bare",
    model: "anthropic/claude-opus-5-5",
    endpoint: "claude",
    caseId,
    slice: options.slice ?? "report",
    kind: options.kind ?? "approval",
    judgement,
    seed: 1,
    verdict: faulted ? [] : verdict,
    probabilities: null,
    confidence: null,
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
  options: { slice?: SliceName; kind?: CaseKind; faulted?: number; excluded?: number } = {}
): JudgementRow[] {
  const faulted = options.faulted ?? 0;
  return Array.from({ length: 5 }, (_, index) =>
    judgementRow(arm, caseId, index, index < chose ? [APPROVE] : [CHANGES], {
      slice: options.slice,
      kind: options.kind,
      fault: index < faulted,
      excluded: options.excluded,
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
    confidenceTable: false,
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
