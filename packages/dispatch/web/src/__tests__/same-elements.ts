import { expect } from "bun:test";

/**
 * Asserts that `actual` holds exactly the elements `expected` names, in order and by identity.
 *
 * A failed deep-equality matcher (`toEqual`, `toStrictEqual`, `toMatchObject`,
 * `toHaveBeenCalledWith`) whose values hold a DOM element formats each element's whole object
 * graph - the document, the window and React's fibers - and never finishes: the run neither
 * fails nor ends, and fills its machine's memory. This compares the count, then each element with
 * `toBe`, whose failure formats no graph, so a regression fails in a second.
 */
export function expectSameElements(actual: readonly Element[], expected: readonly Element[]): void {
  expect(actual.length).toBe(expected.length);
  for (const [index, element] of expected.entries()) {
    expect(actual[index]).toBe(element);
  }
}
