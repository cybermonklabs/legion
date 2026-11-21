import { expect } from "bun:test";

import { countReaderSteps } from "../features/refs/reader-steps";
import { expressionKey, expressionsRunBy, readerExpressions } from "./reader-expressions";

/**
 * How many steps past four times its steps on a text a reader may take on a text four times as
 * long. A reader of one pass takes `a·n + b` steps on a text of `n`, which at `4n` is within
 * `4·(a·n + b)` wherever `b` is not below zero, and past it by `-3b`, a few steps whatever `n`
 * is, where a loop skips a few characters it was handed. A reader that rereads its text takes
 * about 16 times its steps on four times the text, far past this at the lengths read here.
 */
export const ONE_PASS_SLACK = 16;

/** The expressions `features/refs/reader-expressions.test.ts` proves read a text in one pass, as
 *  `expressionKey` writes them. */
const checkedExpressions = new Set(
  readerExpressions.map(([, expression]) => expressionKey(expression))
);

/** The steps of this folder's text readers (`features/refs/reader-steps.ts`) that `read` takes
 *  on a text of `n` and on one of four times `n`, what `read` answered on the longer one, and the
 *  regular expressions it ran there over a text of at least `n` characters, a quarter of the
 *  read's own (`expressionsRunBy`). */
export function stepsAtNAndFourN<Answer>(
  read: (n: number) => Answer,
  n: number
): { atN: number; atFourN: number; answerAtFourN: Answer; expressions: Set<string> } {
  let answerAtFourN!: Answer;
  let expressions!: Set<string>;
  const atFourN = countReaderSteps(() => {
    expressions = expressionsRunBy(() => {
      answerAtFourN = read(4 * n);
    }, n);
  });
  return { answerAtFourN, atFourN, atN: countReaderSteps(() => read(n)), expressions };
}

/** Asserts that `read` reads its text in one pass: more steps on a text of `4n` than on one of
 *  `n`, no more than four times as many and `ONE_PASS_SLACK`, and no regular expression run that
 *  `readerExpressions` does not hold. A step count cannot see an expression that backtracks,
 *  which does all its work inside one call, so each expression a reader runs is proved safe on
 *  its own, from the expression, and this holds the reader to that list. Answers what `read`
 *  answered on the text of `4n`, so a row asserts on the counted read rather than reading it
 *  again. */
export function expectOnePass<Answer>(read: (n: number) => Answer, n: number): Answer {
  const { answerAtFourN, atN, atFourN, expressions } = stepsAtNAndFourN(read, n);
  expect(atFourN).toBeGreaterThan(atN);
  expect(atFourN).toBeLessThanOrEqual(4 * atN + ONE_PASS_SLACK);
  expect([...expressions].filter((key) => !checkedExpressions.has(key))).toEqual([]);
  return answerAtFourN;
}
