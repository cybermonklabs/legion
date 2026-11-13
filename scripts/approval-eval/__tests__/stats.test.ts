import { describe, expect, test } from "bun:test";
import {
  alwaysApproveBaseline,
  type Bin,
  binReport,
  chooseThreshold,
  clopperPearsonInterval,
  clopperPearsonUpper,
  decidedAt,
  type Judgement,
  MINIMUM_DECIDED,
  mcnemarExact,
  type PairedCase,
  riskCoverageCurve,
  zeroDisagreementBand,
} from "../stats";

/**
 * The arithmetic over hand-written counts, which is the whole of this test: nothing here reads a
 * corpus, a file or Dispatch. Every bound and every interval below was computed a second way, by
 * scipy, and the tables hold this module to those readings. scipy is the control on the
 * arithmetic and never a second program the round runs.
 *
 * The control values were produced by this command, whose output is quoted beside each table:
 *
 * ```
 * UV_CACHE_DIR="$TMPDIR/uv-cache" uv run --with scipy==1.14.1 python -c "
 * from scipy.stats import beta
 * def two_sided(k, n, c):
 *     tail = (1 + c) / 2
 *     lower = 0.0 if k == 0 else beta.ppf(1 - tail, k, n - k + 1)
 *     upper = 1.0 if k == n else beta.ppf(tail, k + 1, n - k)
 *     return lower, upper
 * for f, n in [(0, 45), (2, 100), (3, 150), (5, 200), (5, 250)]:
 *     print('one-sided', f, n, '%.4f' % (100 * beta.ppf(0.90, f + 1, n - f)))
 * for k, n in [(45, 50), (250, 300), (5, 45)]:
 *     print('two-sided', k, n, '%.4f %.4f' % tuple(100 * x for x in two_sided(k, n, 0.9)))
 * for b, c, n in [(20, 5, 100), (3, 9, 60)]:
 *     lower, upper = two_sided(b, b + c, 0.9)
 *     weight = (b + c) / n
 *     print('paired', b, c, n, '%.4f %.4f' %
 *           (100 * (2 * lower - 1) * weight, 100 * (2 * upper - 1) * weight))
 * "
 * ```
 *
 * Measured against that output, this module's one-sided bound, its two-sided interval and its
 * paired interval agree with scipy to within 0.0005 percentage points, the widest difference
 * over the interval tables being 0.000045 percentage points.
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
  // failures, trials, the one-sided 90 percent upper bound in percent: the `one-sided` lines of
  // the control command's output, 4.9881, 5.2345, 4.3994, 4.5879 and 3.6782, to three places.
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

describe("clopperPearsonInterval", () => {
  // successes, trials, the two-sided 90 percent interval in percent, as the control's
  // `two-sided` lines read it.
  const table: [number, number, number, number][] = [
    [45, 50, 80.1167, 95.9763],
    [250, 300, 79.3843, 86.7815],
    [5, 45, 4.4815, 21.9536],
  ];

  for (const [successes, trials, lower, upper] of table) {
    test(`${successes} of ${trials} lies in [${lower}, ${upper}] percent`, () => {
      const interval = clopperPearsonInterval(successes, trials, CONFIDENCE);
      expect(Math.abs(100 * interval.lower - lower)).toBeLessThan(TOLERANCE);
      expect(Math.abs(100 * interval.upper - upper)).toBeLessThan(TOLERANCE);
    });
  }

  test("each tail takes half of what the confidence leaves", () => {
    const interval = clopperPearsonInterval(45, 50, CONFIDENCE);
    expect(interval.upper).toBeCloseTo(clopperPearsonUpper(45, 50, (1 + CONFIDENCE) / 2), 12);
  });

  test("no success and every success run to the ends", () => {
    expect(clopperPearsonInterval(0, 30, CONFIDENCE).lower).toBe(0);
    expect(clopperPearsonInterval(30, 30, CONFIDENCE).upper).toBe(1);
  });
});

describe("the paired interval of mcnemarExact", () => {
  /** `armOnly` pairs the arm won, `otherOnly` the other won, the rest agreed with both. */
  function paired(armOnly: number, otherOnly: number, counted: number): PairedCase[] {
    return [
      ...Array.from({ length: armOnly }, () => ({ armAgreed: true, otherAgreed: false })),
      ...Array.from({ length: otherOnly }, () => ({ armAgreed: false, otherAgreed: true })),
      ...Array.from({ length: counted - armOnly - otherOnly }, () => ({
        armAgreed: true,
        otherAgreed: true,
      })),
    ];
  }

  // armOnly, otherOnly, counted, and the interval of the paired difference in percent, as the
  // control's `paired` lines read it: the exact interval on the share of the discordant pairs
  // the arm won, carried across to the difference by how many of all the cases those pairs are.
  const table: [number, number, number, number, number][] = [
    [20, 5, 100, 6.2297, 20.8855],
    [3, 9, 60, -17.1252, 1.0931],
  ];

  for (const [armOnly, otherOnly, counted, lower, upper] of table) {
    test(`${armOnly} to ${otherOnly} of ${counted} differs by [${lower}, ${upper}] percent`, () => {
      const comparison = mcnemarExact(paired(armOnly, otherOnly, counted), CONFIDENCE);
      expect(comparison.discordant).toBe(armOnly + otherOnly);
      expect(Math.abs(100 * comparison.interval.lower - lower)).toBeLessThan(TOLERANCE);
      expect(Math.abs(100 * comparison.interval.upper - upper)).toBeLessThan(TOLERANCE);
    });
  }

  test("no discordant pair carries no interval and no evidence either way", () => {
    const comparison = mcnemarExact(paired(0, 0, 40), CONFIDENCE);
    expect(comparison.interval).toEqual({ lower: 0, upper: 0 });
    expect(comparison.pValue).toBe(1);
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
