import type { CaseVerdict } from "./build-corpus";

/**
 * The arithmetic the readouts of this directory are written in: an exact one-sided
 * Clopper-Pearson bound on a failure rate, the risk and coverage curve a confidence threshold is
 * chosen off, the largest band of cases that holds no disagreement at all, the baseline an arm is
 * worth measuring against, and the rule that keeps a bound off a bin too small to carry one.
 *
 * Every function here is pure: it reads no file, opens no socket and prints nothing, so a readout
 * can be recomputed from a corpus alone and a test can hold each number by hand.
 *
 * The bound is computed by the beta quantile identity rather than by a normal approximation,
 * because the counts these readouts rest on are small and the rates near zero, which is exactly
 * where the approximation is wrong in the unsafe direction.
 */

/** The one-sided confidence every bound here is taken at when the caller names no other. */
export const DEFAULT_CONFIDENCE = 0.9;

/**
 * How many decided cases a bin must hold before its bound is worth printing. 45 decided cases
 * with no disagreement give a one-sided 90 percent upper bound of 4.99 percent, so 45 is the
 * smallest count that can hold a 1-in-20 rate at all: any bin below it bounds nothing, however
 * clean it looks, and the readout prints its count and its observed agreement instead.
 */
export const MINIMUM_DECIDED = 45;

/** One case as one arm judged it: how far the judgements agreed, and whether the arm was right. */
export interface Judgement {
  caseId: string;
  /** The counted share of judgements that agreed with each other, from 0 to 1. */
  confidence: number;
  /** Whether the arm's majority verdict matched the person's. */
  agreed: boolean;
}

/** One threshold of the risk and coverage curve: what deciding at it would cost and cover. */
export interface CurvePoint {
  /** Cases at or above this confidence are decided by the arm. */
  threshold: number;
  decided: number;
  /** Decided cases whose verdict did not match the person's. */
  disagreements: number;
  /** The share of all judged cases this threshold decides. */
  coverage: number;
  /** disagreements / decided. */
  observedRate: number;
  /** The one-sided upper bound on the decided cases' disagreement rate. */
  upperBound: number;
}

/** What `chooseThreshold` answers: the point it chose, or why it could choose none. */
export interface ThresholdChoice {
  /** The lowest threshold whose bound holds the rate, or null when no threshold does. */
  chosen: CurvePoint | null;
  /** Why nothing was chosen, when `chosen` is null. */
  reason: string | null;
}

/** The largest band of cases that holds no observed disagreement, and the bound it still carries. */
export interface ZeroDisagreementBand {
  threshold: number;
  decided: number;
  coverage: number;
  /**
   * The one-sided upper bound over the band. No finite set of cases bounds a rate at zero, so the
   * caller prints this beside the count rather than calling the band perfect.
   */
  upperBound: number;
}

/** How often the person approved, which is what an arm that approves everything would score. */
export interface Baseline {
  /** How many of the counted cases the person approved. */
  approvals: number;
  /** How many cases were counted. */
  counted: number;
  /** approvals / counted, and 0 when nothing was counted. */
  share: number;
}

/** One bin of a readout: a name and the judged cases that fell into it. */
export interface Bin {
  name: string;
  cases: readonly Judgement[];
}

/** One line of a binned readout. A bin under `minimumDecided` carries no bound. */
export interface BinLine {
  name: string;
  decided: number;
  disagreements: number;
  /** The share of the bin's cases the arm got right. */
  observedAgreement: number;
  /** The one-sided upper bound on the bin's disagreement rate, or null when the bin is too small. */
  upperBound: number | null;
}

/** How `binReport` is told what counts as enough cases to bound, and at what confidence. */
export interface BinOptions {
  minimumDecided?: number;
  confidence?: number;
}

/** How `chooseThreshold` is told which rate must hold, and at what confidence. */
export interface ThresholdOptions {
  /** The disagreement rate the bound must be at or under, from 0 to 1. */
  rate: number;
  confidence?: number;
}

/** Where the continued fraction gives up: it converges in far fewer steps at every count here. */
const BETA_ITERATIONS = 300;
/** How close two successive terms of the continued fraction must be before it is done. */
const BETA_EPSILON = 3e-16;
/** A stand-in for a zero denominator, which the modified Lentz method may not divide by. */
const BETA_TINY = 1e-300;
/** Bisection steps on [0, 1]: 60 already reaches the spacing of a double near 1. */
const QUANTILE_ITERATIONS = 80;

/** Lanczos coefficients (g = 7, n = 9), the usual set for a double-precision log gamma. */
const LANCZOS = [
  0.999_999_999_999_809_93, 676.520_368_121_885_1, -1259.139_216_722_402_8, 771.323_428_777_653_13,
  -176.615_029_162_140_6, 12.507_343_278_686_905, -0.138_571_095_265_720_12,
  9.984_369_578_019_572e-6, 1.505_632_735_149_311_6e-7,
];

/** log Γ(x) for x > 0, by the Lanczos approximation. */
function logGamma(x: number): number {
  const z = x - 1;
  let series = LANCZOS[0] as number;
  for (let i = 1; i < LANCZOS.length; i += 1) series += (LANCZOS[i] as number) / (z + i);
  const t = z + 7.5;
  return 0.5 * Math.log(2 * Math.PI) + (z + 0.5) * Math.log(t) - t + Math.log(series);
}

/**
 * The continued fraction of the incomplete beta function, by the modified Lentz method. It
 * converges quickly only for x below (a + 1) / (a + b + 2), which is why its caller reflects.
 */
function betaContinuedFraction(a: number, b: number, x: number): number {
  const qab = a + b;
  const qap = a + 1;
  const qam = a - 1;
  let c = 1;
  let d = 1 - (qab * x) / qap;
  if (Math.abs(d) < BETA_TINY) d = BETA_TINY;
  d = 1 / d;
  let h = d;
  for (let m = 1; m <= BETA_ITERATIONS; m += 1) {
    const m2 = 2 * m;
    const even = (m * (b - m) * x) / ((qam + m2) * (a + m2));
    d = 1 + even * d;
    if (Math.abs(d) < BETA_TINY) d = BETA_TINY;
    c = 1 + even / c;
    if (Math.abs(c) < BETA_TINY) c = BETA_TINY;
    d = 1 / d;
    h *= d * c;
    const odd = (-(a + m) * (qab + m) * x) / ((a + m2) * (qap + m2));
    d = 1 + odd * d;
    if (Math.abs(d) < BETA_TINY) d = BETA_TINY;
    c = 1 + odd / c;
    if (Math.abs(c) < BETA_TINY) c = BETA_TINY;
    d = 1 / d;
    const step = d * c;
    h *= step;
    if (Math.abs(step - 1) < BETA_EPSILON) break;
  }
  return h;
}

/**
 * The regularized incomplete beta function I_x(a, b): the probability a Beta(a, b) draw is at or
 * below x, which for whole a and b is a sum of binomial terms, so the bound built on it is exact
 * binomial arithmetic rather than an approximation of it.
 */
export function regularizedIncompleteBeta(x: number, a: number, b: number): number {
  if (!(a > 0) || !(b > 0)) throw new RangeError("the beta shape parameters must be above zero");
  if (x <= 0) return 0;
  if (x >= 1) return 1;
  const front = Math.exp(
    logGamma(a + b) - logGamma(a) - logGamma(b) + a * Math.log(x) + b * Math.log1p(-x)
  );
  return x < (a + 1) / (a + b + 2)
    ? (front * betaContinuedFraction(a, b, x)) / a
    : 1 - (front * betaContinuedFraction(b, a, 1 - x)) / b;
}

/** The x at which I_x(a, b) is `p`, found by bisection on a function that only rises. */
function betaQuantile(p: number, a: number, b: number): number {
  let low = 0;
  let high = 1;
  for (let i = 0; i < QUANTILE_ITERATIONS; i += 1) {
    const middle = (low + high) / 2;
    if (middle <= low || middle >= high) break;
    if (regularizedIncompleteBeta(middle, a, b) < p) low = middle;
    else high = middle;
  }
  return (low + high) / 2;
}

/**
 * The one-sided Clopper-Pearson upper bound on the failure rate of `failures` in `trials`: the
 * largest rate under which what was seen is not yet improbable at `confidence`. It is the beta
 * quantile BetaInv(confidence; failures + 1, trials - failures), and 1 when every trial failed,
 * since no rate below 1 is then ruled out.
 */
export function clopperPearsonUpper(failures: number, trials: number, confidence: number): number {
  if (!Number.isInteger(trials) || trials <= 0) {
    throw new RangeError(`trials must be a whole number above zero, not ${trials}`);
  }
  if (!Number.isInteger(failures) || failures < 0 || failures > trials) {
    throw new RangeError(`failures must be a whole number from 0 to ${trials}, not ${failures}`);
  }
  if (!(confidence > 0) || !(confidence < 1)) {
    throw new RangeError(`confidence must be between 0 and 1, not ${confidence}`);
  }
  if (failures === trials) return 1;
  // I_x(1, n) = 1 - (1 - x)^n, so the quantile at no failure at all is closed form.
  if (failures === 0) return 1 - (1 - confidence) ** (1 / trials);
  return betaQuantile(confidence, failures + 1, trials - failures);
}

/** The cases an arm decides at `threshold`: the ones its judgements agreed on at least that far. */
export function decidedAt(judgements: readonly Judgement[], threshold: number): Judgement[] {
  return judgements.filter((one) => one.confidence >= threshold);
}

/**
 * The risk and coverage curve: one point per distinct confidence present, ordered by threshold
 * ascending. Coverage only falls as the threshold rises, which is the trade the curve is read for.
 */
export function riskCoverageCurve(
  rows: readonly Judgement[],
  confidence: number = DEFAULT_CONFIDENCE
): CurvePoint[] {
  const thresholds = [...new Set(rows.map((row) => row.confidence))].sort((one, two) => one - two);
  return thresholds.map((threshold) => {
    const decided = decidedAt(rows, threshold);
    const disagreements = decided.filter((one) => !one.agreed).length;
    return {
      threshold,
      decided: decided.length,
      disagreements,
      coverage: decided.length / rows.length,
      observedRate: disagreements / decided.length,
      upperBound: clopperPearsonUpper(disagreements, decided.length, confidence),
    };
  });
}

/**
 * The lowest threshold whose one-sided upper bound over the cases it decides is at or under
 * `rate`: the most cases an arm may decide while the rate it is held to still holds. When no
 * threshold holds it, the answer carries the reason instead, since the caller prints it.
 */
export function chooseThreshold(
  rows: readonly Judgement[],
  options: ThresholdOptions
): ThresholdChoice {
  const { rate, confidence = DEFAULT_CONFIDENCE } = options;
  if (!(rate >= 0) || !(rate <= 1)) {
    throw new RangeError(`the rate must be between 0 and 1, not ${rate}`);
  }
  const curve = riskCoverageCurve(rows, confidence);
  if (curve.length === 0) return { chosen: null, reason: "no case was judged" };
  const chosen = curve.find((point) => point.upperBound <= rate);
  if (chosen) return { chosen, reason: null };
  const best = curve.reduce((one, two) => (two.upperBound < one.upperBound ? two : one));
  return {
    chosen: null,
    reason:
      `no threshold holds a rate of ${rate} at ${confidence} confidence: the lowest bound is ` +
      `${best.upperBound} at threshold ${best.threshold}, over ${best.decided} decided ` +
      `cases with ${best.disagreements} disagreements`,
  };
}

/**
 * The largest band that holds no observed disagreement: the lowest threshold at which every
 * decided case matched the person, with the bound that band still carries. Null when every band
 * holds a disagreement, the lowest threshold included.
 */
export function zeroDisagreementBand(
  rows: readonly Judgement[],
  confidence: number = DEFAULT_CONFIDENCE
): ZeroDisagreementBand | null {
  const clean = riskCoverageCurve(rows, confidence).filter((point) => point.disagreements === 0);
  if (clean.length === 0) return null;
  const widest = clean.reduce((one, two) => (two.decided > one.decided ? two : one));
  return {
    threshold: widest.threshold,
    decided: widest.decided,
    coverage: widest.coverage,
    upperBound: widest.upperBound,
  };
}

/**
 * What an arm that approved everything would score: the share of these cases the person approved.
 * An arm no better than this number has measured nothing.
 */
export function alwaysApproveBaseline(cases: readonly { verdict: CaseVerdict | null }[]): Baseline {
  const approvals = cases.filter((one) => one.verdict === "approve").length;
  return {
    approvals,
    counted: cases.length,
    share: cases.length === 0 ? 0 : approvals / cases.length,
  };
}

/**
 * One line per bin, in the order the bins were given. A bin holding fewer than `minimumDecided`
 * cases carries its count and its observed agreement and no bound: a bound over so few cases says
 * nothing a reader can act on, and printing one invites them to act on it anyway.
 */
export function binReport(bins: readonly Bin[], options: BinOptions = {}): BinLine[] {
  const { minimumDecided = MINIMUM_DECIDED, confidence = DEFAULT_CONFIDENCE } = options;
  return bins.map((bin) => {
    const decided = bin.cases.length;
    const disagreements = bin.cases.filter((one) => !one.agreed).length;
    return {
      name: bin.name,
      decided,
      disagreements,
      observedAgreement: decided === 0 ? 0 : (decided - disagreements) / decided,
      upperBound:
        decided >= minimumDecided ? clopperPearsonUpper(disagreements, decided, confidence) : null,
    };
  });
}
