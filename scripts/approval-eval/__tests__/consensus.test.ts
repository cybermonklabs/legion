import { describe, expect, test } from "bun:test";
import type { CaseReading } from "../analyze";
import type { CaseKind, CaseRow, CaseVerdict } from "../build-corpus";
import {
  type ArmName,
  agreementOf,
  answerOf,
  baselineOf,
  type ConsensusGroup,
  consensusLine,
  consensusLines,
  groupsOf,
  QUESTION,
  RATE,
  readingsOf,
  shortModel,
  sliceCaseIds,
} from "../consensus";
import type { SliceName } from "../draw";
import type { RecipeName } from "../recipes";
import type { Endpoint, JudgementRow } from "../round-io";
import { MINIMUM_DECIDED } from "../stats";

/**
 * The consensus reading over hand-written rows: nothing here reads a corpus directory, a round
 * directory or Dispatch, and nothing makes a call. What it holds the reading to is the rules it
 * claims - a subset of the cases every arm of a group answered the same way, its coverage of the
 * slice, its disagreement rate with the person and the exact bound on it, the two kinds of error
 * apart, the floor of 45 cases under which no bound is printed, the majority rule beside
 * unanimity, and the always-approve baseline restricted to the same subset.
 *
 * The bounds below were computed a second way, by the exact binomial sum the Clopper-Pearson
 * bound inverts, in pure Python:
 *
 * ```
 * python3 -c "
 * from math import comb
 * def cp_upper(f, n, c=0.9):
 *     lo, hi = 0.0, 1.0
 *     for _ in range(200):
 *         mid = (lo + hi) / 2
 *         s = sum(comb(n, k) * mid ** k * (1 - mid) ** (n - k) for k in range(f + 1))
 *         lo, hi = (mid, hi) if s > 1 - c else (lo, mid)
 *     return (lo + hi) / 2
 * for f, n in [(3, 60), (0, 50), (12, 60)]:
 *     print(f, n, '%.6f' % cp_upper(f, n))
 * "
 * ```
 *
 * which prints 0.107965 for 3 of 60, 0.045007 for none of 50 and 0.281867 for 12 of 60.
 */

/** The two labels every case of this file offers, which is what a verdict is one of. */
const APPROVE = "Approve";
const CHANGES = "Request changes";

/** The three models the arms of this file stand on, which is what a family is. */
const MODELS = ["anthropic/claude-opus-5-5", "anthropic/claude-fable-5-1", "jev-latest"] as const;

/** How many judgements each case and arm carries here, as the round asks of them. */
const JUDGEMENTS = 5;

/** One case of the corpus, with only the fields the reading reads named. */
function caseRow(
  caseId: string,
  options: { kind?: CaseKind; verdict?: CaseVerdict | null } = {}
): CaseRow {
  const kind = options.kind ?? "approval";
  const verdict = options.verdict === undefined ? "approve" : options.verdict;
  return {
    caseId,
    eventId: 1,
    askId: `ask-${caseId}`,
    issueKey: "PROJ-1",
    project: "INFRA",
    kind,
    urgency: "high",
    multiple: false,
    question: `Approve the spec of ${caseId}?`,
    options: [
      { id: "o1", label: APPROVE, description: "it is ready" },
      { id: "o2", label: CHANGES, description: "it is not" },
    ],
    selected: [verdict === "request_changes" ? CHANGES : APPROVE],
    words: null,
    hasWords: false,
    askCreatedAt: "2026-11-06T09:00:00Z",
    answeredAt: "2026-11-06T12:00:00Z",
    eventCreatedAt: "2026-11-06T12:00:00Z",
    approval: {
      documents: [
        { artifactId: `${caseId}-document`, name: "spec.md", version: 3, requestedVersion: 3 },
      ],
      artifactSlug: "spec",
      artifactPrimary: true,
    },
    verdict,
    confounds: {
      version: 3,
      answersThatDayBefore: 10,
      hourUtc: 18,
      hourPacific: 10,
      changesAlreadyRequested: false,
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
  options: { slice?: SliceName; kind?: CaseKind; recipe?: RecipeName; model?: string } = {}
): JudgementRow {
  const model = options.model ?? MODELS[0];
  const endpoint: Endpoint = model === "jev-latest" ? "jev" : "claude";
  return {
    arm,
    recipe: options.recipe ?? "bare",
    model,
    endpoint,
    caseId,
    slice: options.slice ?? "report",
    kind: options.kind ?? "approval",
    judgement,
    seed: 1,
    verdict,
    probabilities: null,
    confidence: null,
    inputTokens: 100,
    outputTokens: 10,
    latencyMs: 10,
    attempts: 1,
    stateCut: false,
    retrievalRoute: null,
    examples: 0,
    excluded: null,
    promptBytes: 100,
    fault: null,
    faultDetail: null,
    at: "2026-11-12T00:00:00.000Z",
  };
}

/** Every judgement of one arm over one case, all of them the same answer unless told otherwise. */
function armRows(
  arm: string,
  caseId: string,
  answers: readonly string[][],
  options: { recipe?: RecipeName; model?: string } = {}
): JudgementRow[] {
  return answers.map((verdict, at) => judgementRow(arm, caseId, at, verdict, options));
}

/** The same answer, as many times as the round asks for. */
function same(verdict: string[], times = JUDGEMENTS): string[][] {
  return Array.from({ length: times }, () => verdict);
}

/** The arms of one recipe on the three models, named as the round names them. */
function armsOfRecipe(recipe: RecipeName): ArmName[] {
  return MODELS.map((model) => ({ name: `${recipe}-${shortModel(model)}`, recipe, model }));
}

/** The readings of a hand table, which is what every subset below is taken over. */
function readHand(rows: readonly JudgementRow[], cases: readonly CaseRow[]) {
  const corpus = new Map(cases.map((row) => [row.caseId, row]));
  const caseIds = sliceCaseIds(rows, corpus);
  return { ...readingsOf(rows, corpus, caseIds), caseIds, corpus };
}

describe("shortModel", () => {
  test("names the model without the vendor that serves it", () => {
    expect(shortModel("anthropic/claude-opus-5-5")).toBe("opus-5-5");
    expect(shortModel("jev-latest")).toBe("jev-latest");
  });
});

describe("groupsOf", () => {
  test("takes every recipe's arms together, each pair of them, and a majority of them", () => {
    const groups = groupsOf([...armsOfRecipe("bare"), ...armsOfRecipe("distilled")]);
    expect(groups.map((group) => group.name)).toEqual([
      "bare: all 3 agree (fable-5-1, opus-5-5, jev-latest)",
      "bare: fable-5-1 and opus-5-5 agree",
      "bare: fable-5-1 and jev-latest agree",
      "bare: opus-5-5 and jev-latest agree",
      "bare: a majority of 3 agree",
      "distilled: all 3 agree (fable-5-1, opus-5-5, jev-latest)",
      "distilled: fable-5-1 and opus-5-5 agree",
      "distilled: fable-5-1 and jev-latest agree",
      "distilled: opus-5-5 and jev-latest agree",
      "distilled: a majority of 3 agree",
      "every arm: all 6 agree",
    ]);
    expect(groups.filter((group) => group.rule === "majority")).toHaveLength(2);
    expect(groups.at(-1)?.arms).toHaveLength(6);
  });

  test("a recipe with one arm alone is no group, there being nothing for it to agree with", () => {
    const groups = groupsOf([{ name: "bare-opus-5-5", recipe: "bare", model: MODELS[0] }]);
    expect(groups).toEqual([]);
  });
});

describe("agreementOf", () => {
  const cases = [
    caseRow("all-approve"),
    caseRow("all-changes", { verdict: "request_changes" }),
    caseRow("split"),
    caseRow("undecided"),
  ];
  const arms = armsOfRecipe("bare");
  const rows = [
    // Every arm approves, and so did the person.
    ...arms.flatMap((arm) =>
      armRows(arm.name, "all-approve", same([APPROVE]), { model: arm.model })
    ),
    // Every arm approves, and the person asked for changes: one false approval.
    ...arms.flatMap((arm) =>
      armRows(arm.name, "all-changes", same([APPROVE]), { model: arm.model })
    ),
    // Two arms approve and the third does not: a majority, and no unanimity.
    ...armRows("bare-opus-5-5", "split", same([APPROVE]), { model: MODELS[0] }),
    ...armRows("bare-fable-5-1", "split", same([APPROVE]), { model: MODELS[1] }),
    ...armRows("bare-jev-latest", "split", same([CHANGES]), { model: MODELS[2] }),
    // One arm's judgements tie, so it has no verdict and agrees with nobody.
    ...armRows("bare-opus-5-5", "undecided", same([APPROVE]), { model: MODELS[0] }),
    ...armRows("bare-fable-5-1", "undecided", same([APPROVE]), { model: MODELS[1] }),
    ...armRows("bare-jev-latest", "undecided", [[APPROVE], [APPROVE], [CHANGES], [CHANGES]], {
      model: MODELS[2],
    }),
  ];

  test("unanimity takes the cases every arm answered the same way, and no other", () => {
    const { byArm, caseIds } = readHand(rows, cases);
    const group = groupsOf(arms)[0] as ConsensusGroup;
    expect(group.rule).toBe("unanimous");
    const agreed = agreementOf(group, byArm, caseIds);
    expect(agreed.map((one) => one.caseId)).toEqual(["all-approve", "all-changes"]);
    expect(agreed.map((one) => one.labels)).toEqual([[APPROVE], [APPROVE]]);
    expect(agreed.map((one) => one.reading.agreed)).toEqual([true, false]);
  });

  test("a majority takes a case two of three agreed on, an undecided arm among them", () => {
    const { byArm, caseIds } = readHand(rows, cases);
    const group = groupsOf(arms).find((one) => one.rule === "majority") as ConsensusGroup;
    const agreed = agreementOf(group, byArm, caseIds);
    expect(agreed.map((one) => one.caseId)).toEqual([
      "all-approve",
      "all-changes",
      "split",
      "undecided",
    ]);
    expect(agreed.every((one) => one.labels[0] === APPROVE)).toBe(true);
  });

  test("a pair holds a case only where both of its arms answered it the same way", () => {
    const { byArm, caseIds } = readHand(rows, cases);
    const pair = groupsOf(arms).find((one) => one.name.includes("opus-5-5 and jev-latest"));
    const agreed = agreementOf(pair as ConsensusGroup, byArm, caseIds);
    expect(agreed.map((one) => one.caseId)).toEqual(["all-approve", "all-changes"]);
  });
});

describe("consensusLine", () => {
  /**
   * A hand table every arm of one recipe is unanimous over: `falseApprovals` cases the person
   * asked for changes on and the arms approved, `falseRequests` cases the person approved and
   * the arms asked for changes, and `agreedCases` both approved. The two error counts are given
   * different values so that a reading that swapped them would be caught.
   */
  function handTable(falseApprovals: number, falseRequests: number, agreedCases: number) {
    const arms = armsOfRecipe("many-shot");
    const cases: CaseRow[] = [];
    const rows: JudgementRow[] = [];
    const add = (caseId: string, person: CaseVerdict, armLabel: string) => {
      cases.push(caseRow(caseId, { verdict: person }));
      for (const arm of arms) {
        rows.push(
          ...armRows(arm.name, caseId, same([armLabel]), { model: arm.model, recipe: "many-shot" })
        );
      }
    };
    for (let at = 0; at < falseApprovals; at += 1) {
      add(`false-approval-${at}`, "request_changes", APPROVE);
    }
    for (let at = 0; at < falseRequests; at += 1) add(`false-request-${at}`, "approve", CHANGES);
    for (let at = 0; at < agreedCases; at += 1) add(`agreed-${at}`, "approve", APPROVE);
    return { arms, cases, rows };
  }

  test("reads the subset's coverage, its rate, its exact bound and its two kinds of error", () => {
    const { arms, cases, rows } = handTable(2, 1, 57);
    const { byArm, caseIds } = readHand(rows, cases);
    const group = groupsOf(arms)[0] as ConsensusGroup;
    const line = consensusLine(group, agreementOf(group, byArm, caseIds), caseIds.length);
    expect(line.cases).toBe(60);
    expect(line.coverage).toBe(1);
    expect(line.disagreements).toBe(3);
    expect(line.rate).toBeCloseTo(0.05, 10);
    expect(line.bound ?? 0).toBeCloseTo(0.107965, 5);
    expect(line.falseApprovals).toBe(2);
    expect(line.falseRequests).toBe(1);
    expect(line.otherErrors).toBe(0);
  });

  test("a subset under the floor carries its count and no bound", () => {
    const { arms, cases, rows } = handTable(1, 1, 42);
    const { byArm, caseIds } = readHand(rows, cases);
    const group = groupsOf(arms)[0] as ConsensusGroup;
    const line = consensusLine(group, agreementOf(group, byArm, caseIds), caseIds.length);
    expect(line.cases).toBe(44);
    expect(line.cases).toBeLessThan(MINIMUM_DECIDED);
    expect(line.bound).toBeNull();
    expect(line.baseline.bound).toBeNull();
  });

  test("the coverage is of the slice, and not of the cases the subset itself holds", () => {
    const { arms, cases, rows } = handTable(0, 0, 60);
    const extra = caseRow("not-agreed");
    const { byArm, caseIds } = readHand(
      [
        ...rows,
        ...armRows("many-shot-opus-5-5", "not-agreed", same([APPROVE]), {
          model: MODELS[0],
          recipe: "many-shot",
        }),
        ...armRows("many-shot-fable-5-1", "not-agreed", same([CHANGES]), {
          model: MODELS[1],
          recipe: "many-shot",
        }),
      ],
      [...cases, extra]
    );
    const group = groupsOf(arms)[0] as ConsensusGroup;
    const line = consensusLine(group, agreementOf(group, byArm, caseIds), caseIds.length);
    expect(caseIds).toHaveLength(61);
    expect(line.cases).toBe(60);
    expect(line.coverage).toBeCloseTo(60 / 61, 10);
  });
});

describe("baselineOf", () => {
  test("is the share of a subset the person did not approve, bounded over that subset alone", () => {
    const subset = [
      ...Array.from({ length: 48 }, (_, at) => caseRow(`approve-${at}`)),
      ...Array.from({ length: 12 }, (_, at) =>
        caseRow(`changes-${at}`, { verdict: "request_changes" })
      ),
    ];
    const baseline = baselineOf(subset);
    expect(baseline.approvals).toBe(48);
    expect(baseline.disagreements).toBe(12);
    expect(baseline.rate).toBeCloseTo(0.2, 10);
    expect(baseline.bound ?? 0).toBeCloseTo(0.281867, 5);
  });

  test("a subset the person approved every case of is as good as any rule over it", () => {
    const subset = Array.from({ length: 50 }, (_, at) => caseRow(`approve-${at}`));
    const baseline = baselineOf(subset);
    expect(baseline.disagreements).toBe(0);
    expect(baseline.rate).toBe(0);
    expect(baseline.bound ?? 1).toBeCloseTo(0.045007, 5);
    expect(baseline.bound ?? 1).toBeLessThan(RATE);
  });

  test("a subset under the floor carries its counts and no bound", () => {
    const baseline = baselineOf(Array.from({ length: 10 }, (_, at) => caseRow(`a-${at}`)));
    expect(baseline.bound).toBeNull();
  });
});

describe("sliceCaseIds", () => {
  test("takes the report slice's approval cases the person answered, and no other", () => {
    const cases = [
      caseRow("report-approval"),
      caseRow("report-question", { kind: "question", verdict: null }),
      caseRow("report-other", { verdict: "other" }),
      caseRow("fit-approval"),
    ];
    const corpus = new Map(cases.map((row) => [row.caseId, row]));
    const rows = [
      judgementRow("bare-opus-5-5", "report-approval", 0, [APPROVE]),
      judgementRow("bare-opus-5-5", "report-question", 0, [APPROVE], { kind: "question" }),
      judgementRow("bare-opus-5-5", "report-other", 0, [APPROVE]),
      judgementRow("bare-opus-5-5", "fit-approval", 0, [APPROVE], { slice: "fit" }),
      judgementRow("bare-opus-5-5", "not-in-the-corpus", 0, [APPROVE]),
    ];
    expect(sliceCaseIds(rows, corpus)).toEqual(["report-approval"]);
  });
});

describe("consensusLines", () => {
  test("orders the curve by coverage, the widest subset first", () => {
    const arms = armsOfRecipe("both");
    const cases = Array.from({ length: 10 }, (_, at) => caseRow(`case-${at}`));
    const rows = cases.flatMap((row, at) =>
      arms.flatMap((arm) =>
        armRows(
          arm.name,
          row.caseId,
          // The jev arm asks for changes on the first three cases, and agrees on the rest.
          same([arm.model === MODELS[2] && at < 3 ? CHANGES : APPROVE]),
          { model: arm.model, recipe: "both" }
        )
      )
    );
    const { byArm, caseIds } = readHand(rows, cases);
    const lines = consensusLines(groupsOf(arms), byArm, caseIds);
    const coverages = lines.map((line) => line.coverage);
    expect(coverages).toEqual([...coverages].sort((one, two) => two - one));
    // Two subsets cover the whole slice here, and a tie in coverage is broken by name.
    expect(lines.filter((line) => line.coverage === 1).map((line) => line.name)).toEqual([
      "both: a majority of 3 agree",
      "both: fable-5-1 and opus-5-5 agree",
    ]);
    expect(lines.find((line) => line.name.startsWith("both: all 3"))?.cases).toBe(7);
    expect(lines.at(-1)?.coverage).toBeCloseTo(0.7, 10);
  });
});

describe("answerOf", () => {
  const line = (
    name: string,
    cases: number,
    disagreements: number,
    coverage: number,
    bound: number | null
  ) => ({
    name,
    rule: "unanimous" as const,
    arms: ["one", "two"],
    cases,
    coverage,
    disagreements,
    rate: cases === 0 ? 0 : disagreements / cases,
    bound,
    falseApprovals: disagreements,
    falseRequests: 0,
    otherErrors: 0,
    baseline: { approvals: cases, disagreements: 0, rate: 0, bound: 0.045 },
  });

  test("says yes, with the coverage and the baseline over the same cases, where one holds", () => {
    const answer = answerOf([line("clean", 50, 0, 0.5, 0.045), line("dirty", 50, 20, 0.5, 0.49)]);
    expect(answer).toStartWith("Yes. 1 of 2 subsets hold a disagreement rate under 5 percent");
    expect(answer).toContain("clean at coverage 0.500 (50 cases, 0 disagreements");
    expect(answer).toContain("against an always-approve bound of 0.045 over the same cases");
  });

  test("says no, with the lowest bound, its coverage and the count behind it, where none does", () => {
    const answer = answerOf([line("low", 60, 3, 0.6, 0.108), line("high", 60, 12, 0.6, 0.282)]);
    expect(answer).toStartWith("No. No consensus subset holds a disagreement rate under 5 percent");
    expect(answer).toContain("The lowest bound reached is 0.108, by low, at coverage 0.600");
    expect(answer).toContain("60 cases of which 3 disagree (rate 0.050)");
    expect(answer).toContain("an always-approve rule disagrees at 0.000, bound at most 0.045");
  });

  test("counts the subsets too small to bound", () => {
    const answer = answerOf([line("low", 60, 3, 0.6, 0.108), line("small", 10, 0, 0.1, null)]);
    expect(answer).toEndWith("1 of 2 holds fewer than 45 cases and so carries no bound.");
  });
});

describe("the question", () => {
  test("is asked in the words the reading reports it in", () => {
    expect(QUESTION).toBe(
      "Does any of these consensus subsets hold a disagreement rate under 5 percent by its " +
        "bound, and at what coverage?"
    );
    expect(RATE).toBe(0.05);
  });
});

describe("readingsOf", () => {
  test("reads one case per arm by the round's own rule, the last row of a judgement winning", () => {
    const cases = [caseRow("one")];
    const corpus = new Map(cases.map((row) => [row.caseId, row]));
    const rows = [
      ...armRows("bare-opus-5-5", "one", same([CHANGES])),
      // The same judgement written again, which the row store holds as the later of the two.
      judgementRow("bare-opus-5-5", "one", 0, [APPROVE]),
    ];
    const { arms, byArm } = readingsOf(rows, corpus, ["one"]);
    expect(arms).toEqual([{ name: "bare-opus-5-5", recipe: "bare", model: MODELS[0] }]);
    const reading = byArm.get("bare-opus-5-5")?.get("one") as CaseReading;
    expect(reading.returned).toBe(JUDGEMENTS);
    expect(reading.verdict).toEqual([CHANGES]);
    expect(reading.agreed).toBe(false);
  });
});
