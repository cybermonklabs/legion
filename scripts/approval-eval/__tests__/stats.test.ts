import { describe, expect, test } from "bun:test";
import {
  alwaysApproveBaseline,
  type Bin,
  binReport,
  chooseThreshold,
  clopperPearsonUpper,
  decidedAt,
  type Judgement,
  MINIMUM_DECIDED,
  riskCoverageCurve,
  zeroDisagreementBand,
} from "../stats";

/**
 * The arithmetic over hand-written counts, which is the whole of this test: nothing here reads a
 * corpus, a file or Dispatch. Every bound below was computed a second way, by
 * `scipy.stats.beta.ppf(0.90, k + 1, n - k)`, and the table holds this module to those readings.
 */

/** One-sided, as every bound of a readout is. */
const CONFIDENCE = 0.9;

/** How far a bound may sit from the reading computed by scipy, in percentage points. */
const TOLERANCE = 0.001;

/** One judged case, with only the fields the arithmetic reads named. */
function judged(caseId: string, confidence: number, agreed: boolean): Judgement {
  return { caseId, confidence, agreed };
}

/** `count` cases at one confidence, the first `disagreements` of them wrong. */
function judgedCases(
  prefix: string,
  count: number,
  confidence: number,
  disagreements: number
): Judgement[] {
  return Array.from({ length: count }, (_, index) =>
    judged(`${prefix}-${index}`, confidence, index >= disagreements)
  );
}

describe("clopperPearsonUpper", () => {
  // failures, trials, the one-sided 90 percent upper bound in percent, as scipy reads it.
  const table: [number, number, number][] = [
    [0, 45, 4.988],
    [2, 100, 5.235],
    [3, 150, 4.399],
    [5, 200, 4.588],
    [5, 250, 3.678],
  ];

  for (const [failures, trials, percent] of table) {
    test(`${failures} of ${trials} bounds the rate at ${percent} percent`, () => {
      const bound = 100 * clopperPearsonUpper(failures, trials, CONFIDENCE);
      expect(Math.abs(bound - percent)).toBeLessThan(TOLERANCE);
    });
  }

  test("one trial with no failure rules out only the rate above the confidence", () => {
    expect(clopperPearsonUpper(0, 1, CONFIDENCE)).toBeCloseTo(0.9, 12);
  });

  test("no failure at all is the closed form of the beta quantile", () => {
    for (const trials of [1, 45, 250]) {
      expect(clopperPearsonUpper(0, trials, CONFIDENCE)).toBeCloseTo(
        1 - (1 - CONFIDENCE) ** (1 / trials),
        12
      );
    }
  });

  test("every trial failing rules out no rate below one", () => {
    for (const trials of [1, 7, 45]) {
      expect(clopperPearsonUpper(trials, trials, CONFIDENCE)).toBe(1);
    }
  });

  test("the bound only rises as the failures rise", () => {
    let last = 0;
    for (let failures = 0; failures <= 20; failures += 1) {
      const bound = clopperPearsonUpper(failures, 100, CONFIDENCE);
      expect(bound).toBeGreaterThan(last);
      last = bound;
    }
  });

  test("refuses counts and confidences that bound nothing", () => {
    expect(() => clopperPearsonUpper(0, 0, CONFIDENCE)).toThrow(/trials/);
    expect(() => clopperPearsonUpper(3, 2, CONFIDENCE)).toThrow(/failures/);
    expect(() => clopperPearsonUpper(1.5, 10, CONFIDENCE)).toThrow(/failures/);
    expect(() => clopperPearsonUpper(1, 10, 1)).toThrow(/confidence/);
  });
});

/**
 * 45 cases the judgements agreed on at 0.9, every one of them right, and 15 at 0.5 of which 5 are
 * wrong. So deciding at 0.9 is 45 clean cases, whose bound is 4.988 percent, and deciding at 0.5
 * is 60 cases with 5 disagreements, whose bound is far above any rate worth holding.
 */
const twoBands = [...judgedCases("high", 45, 0.9, 0), ...judgedCases("low", 15, 0.5, 5)];

describe("decidedAt", () => {
  test("decides the cases at or above the threshold", () => {
    expect(decidedAt(twoBands, 0.9)).toHaveLength(45);
    expect(decidedAt(twoBands, 0.5)).toHaveLength(60);
    expect(decidedAt(twoBands, 0.95)).toHaveLength(0);
  });
});

describe("riskCoverageCurve", () => {
  test("answers one point per distinct confidence, ordered by threshold", () => {
    const curve = riskCoverageCurve(twoBands, CONFIDENCE);
    expect(curve.map((point) => point.threshold)).toEqual([0.5, 0.9]);
  });

  test("counts each band's decided cases, disagreements and coverage", () => {
    const [low, high] = riskCoverageCurve(twoBands, CONFIDENCE);
    expect(low).toMatchObject({ decided: 60, disagreements: 5, coverage: 1 });
    expect(low?.observedRate).toBeCloseTo(5 / 60, 12);
    expect(high).toMatchObject({ decided: 45, disagreements: 0, observedRate: 0 });
    expect(high?.coverage).toBeCloseTo(0.75, 12);
    expect(100 * (high?.upperBound ?? 0)).toBeCloseTo(4.988, 3);
  });

  test("coverage only falls as the threshold rises", () => {
    const rows = [
      ...judgedCases("a", 12, 0.55, 4),
      ...judgedCases("b", 9, 0.7, 2),
      ...judgedCases("c", 20, 0.85, 1),
      ...judgedCases("d", 7, 1, 0),
    ];
    const curve = riskCoverageCurve(rows, CONFIDENCE);
    expect(curve.map((point) => point.threshold)).toEqual([0.55, 0.7, 0.85, 1]);
    for (let i = 1; i < curve.length; i += 1) {
      const earlier = curve[i - 1];
      const later = curve[i];
      expect(later?.threshold).toBeGreaterThan(earlier?.threshold ?? 0);
      expect(later?.coverage).toBeLessThanOrEqual(earlier?.coverage ?? 1);
      expect(later?.decided).toBeLessThanOrEqual(earlier?.decided ?? 0);
    }
    expect(curve.at(0)?.coverage).toBe(1);
  });

  test("answers nothing over no case at all", () => {
    expect(riskCoverageCurve([], CONFIDENCE)).toEqual([]);
  });
});

describe("chooseThreshold", () => {
  test("chooses the lowest threshold whose bound holds the rate", () => {
    const choice = chooseThreshold(twoBands, { rate: 0.05, confidence: CONFIDENCE });
    expect(choice.reason).toBeNull();
    expect(choice.chosen).toMatchObject({ threshold: 0.9, decided: 45, disagreements: 0 });
    expect(choice.chosen?.coverage).toBeCloseTo(0.75, 12);
    expect(100 * (choice.chosen?.upperBound ?? 0)).toBeCloseTo(4.988, 3);
  });

  test("says why when no threshold holds the rate", () => {
    const choice = chooseThreshold(twoBands, { rate: 0.04, confidence: CONFIDENCE });
    expect(choice.chosen).toBeNull();
    expect(choice.reason).toMatch(/no threshold holds a rate of 0.04/);
    expect(choice.reason).toMatch(/threshold 0.9/);
  });

  test("takes the lowest threshold when a looser rate holds everywhere", () => {
    const choice = chooseThreshold(twoBands, { rate: 0.2, confidence: CONFIDENCE });
    expect(choice.chosen?.threshold).toBe(0.5);
    expect(choice.chosen?.decided).toBe(60);
  });

  test("says so when nothing was judged", () => {
    expect(chooseThreshold([], { rate: 0.05, confidence: CONFIDENCE })).toEqual({
      chosen: null,
      reason: "no case was judged",
    });
  });

  test("refuses a rate that is no rate", () => {
    expect(() => chooseThreshold(twoBands, { rate: 1.5 })).toThrow(/rate/);
  });
});

describe("zeroDisagreementBand", () => {
  /**
   * 10 right at 0.95, 10 more right at 0.8, and 10 at 0.6 of which one is wrong. The bands at
   * 0.95 and 0.8 are clean; the one at 0.6 is not, so the largest clean band is 20 cases at 0.8.
   */
  const threeBands = [
    ...judgedCases("top", 10, 0.95, 0),
    ...judgedCases("middle", 10, 0.8, 0),
    ...judgedCases("bottom", 10, 0.6, 1),
  ];

  test("answers the largest band that holds no disagreement", () => {
    const band = zeroDisagreementBand(threeBands, CONFIDENCE);
    expect(band).toMatchObject({ threshold: 0.8, decided: 20 });
    expect(band?.coverage).toBeCloseTo(20 / 30, 12);
  });

  test("carries the band's own bound, which no finite set drives to zero", () => {
    const band = zeroDisagreementBand(threeBands, CONFIDENCE);
    expect(band?.upperBound).toBeGreaterThan(0);
    expect(band?.upperBound).toBeCloseTo(1 - 0.1 ** (1 / 20), 12);
  });

  test("answers nothing when every band holds a disagreement", () => {
    const rows = [judged("wrong", 0.99, false), ...judgedCases("right", 20, 0.7, 0)];
    expect(zeroDisagreementBand(rows, CONFIDENCE)).toBeNull();
  });
});

describe("alwaysApproveBaseline", () => {
  test("counts the share of cases the person approved", () => {
    const cases = [
      { verdict: "approve" as const },
      { verdict: "approve" as const },
      { verdict: "request_changes" as const },
      { verdict: "other" as const },
      { verdict: null },
    ];
    expect(alwaysApproveBaseline(cases)).toEqual({ approvals: 2, counted: 5, share: 0.4 });
  });

  test("counts nothing out of no case", () => {
    expect(alwaysApproveBaseline([])).toEqual({ approvals: 0, counted: 0, share: 0 });
  });
});

describe("binReport", () => {
  test("a bin under the minimum carries its counts and no bound", () => {
    const bins: Bin[] = [
      { name: "forty-four", cases: judgedCases("small", MINIMUM_DECIDED - 1, 0.9, 0) },
      { name: "forty-five", cases: judgedCases("big", MINIMUM_DECIDED, 0.9, 0) },
    ];
    const [small, big] = binReport(bins, { minimumDecided: MINIMUM_DECIDED });
    expect(small).toMatchObject({ name: "forty-four", decided: 44, upperBound: null });
    expect(small?.observedAgreement).toBe(1);
    expect(big).toMatchObject({ name: "forty-five", decided: 45, disagreements: 0 });
    expect(100 * (big?.upperBound ?? 0)).toBeCloseTo(4.988, 3);
  });

  test("45 is the minimum, since it is the smallest clean count under a 1-in-20 rate", () => {
    expect(MINIMUM_DECIDED).toBe(45);
    expect(clopperPearsonUpper(0, MINIMUM_DECIDED, CONFIDENCE)).toBeLessThan(0.05);
    expect(clopperPearsonUpper(0, MINIMUM_DECIDED - 1, CONFIDENCE)).toBeGreaterThan(0.05);
  });

  test("reports a bin's observed agreement whether or not it is bounded", () => {
    const bins: Bin[] = [{ name: "mixed", cases: judgedCases("mixed", 50, 0.8, 5) }];
    const [line] = binReport(bins, { minimumDecided: MINIMUM_DECIDED });
    expect(line).toMatchObject({ decided: 50, disagreements: 5 });
    expect(line?.observedAgreement).toBeCloseTo(0.9, 12);
    expect(line?.upperBound).toBeCloseTo(clopperPearsonUpper(5, 50, CONFIDENCE), 12);
  });

  test("the minimum is 45 when no caller names another", () => {
    const bins: Bin[] = [{ name: "forty-four", cases: judgedCases("small", 44, 0.9, 0) }];
    expect(binReport(bins).at(0)?.upperBound).toBeNull();
  });
});
