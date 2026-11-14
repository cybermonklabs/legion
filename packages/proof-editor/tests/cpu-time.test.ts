/**
 * The growth check's own arithmetic, read at synthetic costs: these rows run no parse.
 *
 * A port of `TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack` in
 * `packages/envoy/internal/dispatch/pmdoc/linear_test.go`. `costGrewTooFast`'s check is
 * `full > slack * step * small`. A cost linear in its input grows about `step` times across the
 * step, a quadratic one about `step * step`, so the step must be greater than the slack before a
 * quadratic's growth can clear the bound of `slack * step` at all.
 *
 * The step and the slack are read from ./cpu-time, never copied here, so this file checks the
 * arithmetic the rows in tests/heavy-bodies.test.ts and tests/nested-directive.test.ts use: raise
 * `linearSlack` to 8 there and the quadratic row below goes red.
 */

import { expect, test } from "bun:test";
import { costGrewTooFast, expectCostGrowsLinearly, growthStep, linearSlack } from "./cpu-time";

/** The small side of a step. `costGrewTooFast` reads the two costs as a ratio, not as bytes. */
const smallSize = 1;

/**
 * The step rounds 1 and 2 of `linear_test.go`'s own history used, which that file's `growthStep`
 * doc comment records as a step that discriminates at no slack. The rows in
 * tests/heavy-bodies.test.ts and tests/nested-directive.test.ts had no growth check before
 * INFRA-1629 and so no step: each measured one size once against a wall-clock constant.
 */
const twoFoldStep = 2;

/** A cost that grows as its input does: what a healthy parse costs. */
function linearCost(size: number): number {
  return size;
}

/** A cost that grows with the square of its input: what the broken parsers cost. */
function quadraticCost(size: number): number {
  return size * size;
}

test("a cost that grows as the input does passes at this file's step and slack", () => {
  // Linear growth across the step is 8, against a bound of `linearSlack * growthStep` of 32, so
  // a healthy parse clears the check with 4 times of room for the measurement's own error.
  const full = linearCost(smallSize * growthStep);
  expect(costGrewTooFast(linearCost(smallSize), full)).toBe(false);
  // The wrapper reports the same decision, so a healthy parse passes through it too.
  expectCostGrowsLinearly("a synthetic linear cost", linearCost(smallSize), full);
});

test("a cost that grows with the square of the input fails at this file's step and slack", () => {
  // Quadratic growth across the step is 8 times 8, which is 64, past the bound of 32. This is
  // the direction the check exists for: a healthy parse and a quadratic one fall either side of
  // 32.
  const full = quadraticCost(smallSize * growthStep);
  expect(costGrewTooFast(quadraticCost(smallSize), full)).toBe(true);
});

test("at a two-fold step the same quadratic cost wrongly passes", () => {
  // The defect the eightfold step fixed, kept here so a reader can run it and watch it reproduce
  // rather than take the claim on faith. A quadratic grows `step * step` and the bound is
  // `slack * step`, so the growth clears the bound only while the step is greater than the slack.
  // At a step of 2 and this file's slack of 4 the growth is 4 against a bound of 8, so the check
  // reads a quadratic cost as linear at any slack of 2 or above.
  const full = quadraticCost(smallSize * twoFoldStep);
  expect(costGrewTooFast(quadraticCost(smallSize), full, twoFoldStep, linearSlack)).toBe(false);
});
