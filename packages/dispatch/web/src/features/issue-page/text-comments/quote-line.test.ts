import { expect, test } from "bun:test";

import { quoteLine } from "./quote-line";

/**
 * R4: the quoted words a thread's card draws first are their text up to the first line break,
 * cut in script at the last word end before the 80th character with an ellipsis, so no style
 * clamps them.
 */

test("the quote stops at its first line break", () => {
  expect(quoteLine("The page draws the plan.\nIt draws the spec too.")).toBe(
    "The page draws the plan."
  );
  expect(quoteLine("First line\r\nsecond line")).toBe("First line");
  expect(quoteLine("One\rtwo")).toBe("One");
  // A quote of one line, and of 80 characters or fewer, is drawn whole.
  expect(quoteLine("Draw the plan beside the spec.")).toBe("Draw the plan beside the spec.");
  const eighty = `${"word ".repeat(15)}words six.ab`.slice(0, 80);
  expect(Array.from(eighty)).toHaveLength(80);
  expect(quoteLine(eighty)).toBe(eighty);
});

test("a long quote is cut at the last word end before 80 characters", () => {
  // Words of nine characters and a space: the eighth ends at the 79th character, the ninth at
  // the 89th, past the 80th.
  const word = "abcdefghi";
  const quote = Array.from({ length: 12 }, () => word).join(" ");
  const cut = quoteLine(quote);
  expect(cut).toBe(`${Array.from({ length: 8 }, () => word).join(" ")}\u2026`);
  expect(Array.from(cut).length).toBeLessThanOrEqual(80);
  // A word that ends on the 80th character itself is past the cut.
  const ending = `${"a".repeat(70)} ${"b".repeat(9)} tail`;
  expect(quoteLine(ending)).toBe(`${"a".repeat(70)}\u2026`);
  // The cut is made on the first line alone.
  expect(quoteLine(`${quote}\nA second line.`)).toBe(cut);
  // One word longer than the line is cut inside itself, the ellipsis the 80th character.
  const long = "x".repeat(120);
  expect(quoteLine(long)).toBe(`${"x".repeat(79)}\u2026`);
  // A character outside the basic plane counts as one.
  const wide = `${"\u{1F600}".repeat(70)} ${"y".repeat(20)}`;
  expect(quoteLine(wide)).toBe(`${"\u{1F600}".repeat(70)}\u2026`);
});
