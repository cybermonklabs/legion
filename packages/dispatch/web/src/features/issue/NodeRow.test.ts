import { expect, test } from "bun:test";

import { oneLineFirstLine, PHONE_ONE_LINE_MEASURE, WIDE_ONE_LINE_MEASURE } from "./NodeRow";

/**
 * R1: a row carries its type, its author, its own first line and its state, and no row truncates
 * a word - at every width, the one-line widths of the zoomed tree included
 * (dispatch://INFRA-1196, c-tree). The row's other words are given to the cut because they share
 * the one line with the text, so each of them shortens what is left for it.
 */
test("a one-line row cuts its first line at a word, never inside one", () => {
  // Short enough for the measure: the whole line, and no ellipsis, since nothing was cut.
  expect(oneLineFirstLine("Short enough.", PHONE_ONE_LINE_MEASURE, [])).toEqual({
    cut: false,
    line: "Short enough.",
  });

  // A sentence that ends inside the measure ends the line, as it does on a row that wraps.
  expect(oneLineFirstLine("One sentence. And another.", PHONE_ONE_LINE_MEASURE, [])).toEqual({
    cut: true,
    line: "One sentence.",
  });

  // Longer than the measure: the line ends at the last space inside it, so the stored text
  // carries on at a word break and no part of a word is drawn without the rest of it.
  const long = "The page draws the whole issue in one reading, with nothing lost.";
  const cut = oneLineFirstLine(long, PHONE_ONE_LINE_MEASURE, []);
  expect(cut.cut).toBe(true);
  expect(cut.line.length).toBeLessThanOrEqual(PHONE_ONE_LINE_MEASURE);
  expect(long.startsWith(cut.line)).toBe(true);
  expect(long.charAt(cut.line.length)).toBe(" ");

  // The type, the author and the state share the one line with it, so the line is shorter where
  // they are longer, and it still ends at a word break.
  const beside = oneLineFirstLine(long, PHONE_ONE_LINE_MEASURE, ["Requirement", "planner", "Open"]);
  expect(beside.line.length).toBeLessThan(cut.line.length);
  expect(long.startsWith(beside.line)).toBe(true);
  expect(long.charAt(beside.line.length)).toBe(" ");

  // A first word longer than what the row has left is drawn whole: a piece of a word with an
  // ellipsis after it is the one thing R1 bans, and the box hides what it cannot hold.
  const path = `${"a".repeat(60)}/b.ts and the words after it.`;
  expect(oneLineFirstLine(path, PHONE_ONE_LINE_MEASURE, ["Requirement"])).toEqual({
    cut: true,
    line: `${"a".repeat(60)}/b.ts`,
  });

  // One word and nothing after it: the line is that word, and `firstLineOf` has already said
  // whether the node's text carries on past it.
  expect(oneLineFirstLine("a".repeat(40), PHONE_ONE_LINE_MEASURE, [])).toEqual({
    cut: false,
    line: "a".repeat(40),
  });
  expect(oneLineFirstLine("", PHONE_ONE_LINE_MEASURE, [])).toEqual({ cut: false, line: "" });
});

/**
 * The band a row is drawn in is what it may say: a row at 768 px and above is four times a
 * phone's row (`WIDE_ONE_LINE_MEASURE`), so it keeps more of the same text, and both cuts land
 * on a space rather than inside a word (R1).
 */
test("a one-line row keeps more of its text in the wider band, and still cuts at a word", () => {
  const long =
    "Every property of the issue that carries a value is visible at every width the page " +
    "draws, with no control opened and no sideways scroll";
  const beside = ["Requirement", "planner", "Open"];
  const phone = oneLineFirstLine(long, PHONE_ONE_LINE_MEASURE, beside);
  const wide = oneLineFirstLine(long, WIDE_ONE_LINE_MEASURE, beside);

  expect(WIDE_ONE_LINE_MEASURE).toBeGreaterThan(PHONE_ONE_LINE_MEASURE);
  expect(wide.line.length).toBeGreaterThan(phone.line.length);
  expect(wide.line.startsWith(phone.line)).toBe(true);
  for (const drawn of [phone, wide]) {
    expect(drawn.cut).toBe(true);
    expect(long.startsWith(drawn.line)).toBe(true);
    expect(long.charAt(drawn.line.length)).toBe(" ");
  }
});
