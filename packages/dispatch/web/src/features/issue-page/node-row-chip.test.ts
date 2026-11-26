import { expect, test } from "bun:test";

import { CHIP_LIMIT, chipWords } from "./node-row-chip";

// The architect's build decision 5: a chip is the target's short name where the model holds one,
// and until then the target's first words, cut at a word within 32 characters.

test("a name that fits is drawn whole, its white space as single spaces", () => {
  expect(chipWords("Add the routes.")).toBe("Add the routes.");
  expect(chipWords("  CORE-12 \n")).toBe("CORE-12");
  expect(chipWords("Store each\n\nnotice in a table.")).toBe("Store each notice in a table.");
  // Exactly the limit is no cut.
  const whole = "a".repeat(CHIP_LIMIT);
  expect(chipWords(whole)).toBe(whole);
});

test("a longer name is its first words, cut at a word, inside the limit with its ellipsis", () => {
  const cut = chipWords("The page draws the whole issue in one reading and nothing is lost.");
  expect(cut).toBe("The page draws the whole issue…");
  expect(cut.length).toBeLessThanOrEqual(CHIP_LIMIT);
  // A word that ends exactly at the edge of the room is kept whole.
  const edge = chipWords("One command, a seeded corpus, a signed-in browser.");
  expect(edge).toBe("One command, a seeded corpus, a…");
  expect(edge.length).toBe(CHIP_LIMIT);
  // A word that would cross the edge is dropped, and the cut never ends on the mark before it.
  expect(chipWords("One command, a seeded corpus, as a signed-in browser.")).toBe(
    "One command, a seeded corpus…"
  );
});

test("one unbroken run is cut at the limit, since no word boundary decides it", () => {
  const cut = chipWords("x".repeat(80));
  expect(cut).toBe(`${"x".repeat(CHIP_LIMIT - 1)}…`);
});

test("a target with no words of its own is named by nothing", () => {
  expect(chipWords("")).toBe("");
  expect(chipWords(" \n ")).toBe("");
});
