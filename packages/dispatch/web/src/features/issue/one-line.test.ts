import { expect, test } from "bun:test";

import { oneLineCut } from "./one-line";

/** A face that draws every character 7 px wide, so each budget below is a sum a reader can
 *  check by hand. */
const widthOf = (text: string) => text.length * 7;

/** The line a row `room` px wide draws of `text`, with nothing beside it. */
function alone(text: string, room: number) {
  return oneLineCut({ after: 0, before: 0, room, text, widthOf });
}

/**
 * R1: a row carries its type, its author, its own first line and its state, and no row truncates
 * a word - at every width, the one-line widths of the zoomed tree included
 * (dispatch://INFRA-1196, c-tree). The cut is made in pixels, against the room the row's own box
 * leaves the line, so the ellipsis is part of what has to fit.
 */
test("a one-line row cuts its first line at a word to the pixels it has, never inside one", () => {
  // Short enough: the whole line, and no ellipsis, since nothing was cut.
  expect(alone("Short enough.", 200)).toEqual({
    cut: false,
    line: "Short enough.",
    under: false,
    wraps: false,
  });

  // A sentence that ends inside the room ends the line, as it does on a row that wraps, and
  // says the text carries on.
  expect(alone("One sentence. And another.", 200)).toEqual({
    cut: true,
    line: "One sentence.",
    under: false,
    wraps: false,
  });

  // Longer than the room: the line ends at the last word whose run and ellipsis fit, so the
  // stored text carries on at a word break. 169 px less the pixel kept for rounding is 24
  // characters: "The page draws the" and its ellipsis are 19, "The page draws the whole" and
  // its are 25.
  const long = "The page draws the whole issue in one reading, with nothing lost.";
  const cut = alone(long, 169);
  expect(cut).toEqual({ cut: true, line: "The page draws the", under: false, wraps: false });
  expect(long.charAt(cut.line.length)).toBe(" ");

  // The ellipsis is counted: a run that fits only without it is one word too long.
  expect(alone(long, 8 * 7 + 1).line).toBe("The");
  expect(alone(long, 9 * 7 + 1).line).toBe("The page");

  // An empty text draws an empty line.
  expect(alone("", 100)).toEqual({ cut: false, line: "", under: false, wraps: false });
});

/**
 * The parts beside the line take from it: the twisty, the type and the author before it, the
 * state after it. Where they leave it less than its first word, it goes on a line of its own
 * under them, where only the state shares its width - a row that kept it beside them would draw
 * it 0 px wide, and hide the parts after it past its own edge.
 */
test("a one-line row's first line goes under its other words where they leave it no word", () => {
  const long = "Does the fold open to the frontier on a phone too?";
  // 300 px, 200 before and 60 after: 39 px beside them, the pixel for rounding kept, and
  // "Does…" is 35.
  expect(oneLineCut({ after: 60, before: 200, room: 300, text: long, widthOf })).toEqual({
    cut: true,
    line: "Does",
    under: false,
    wraps: false,
  });
  // 230 before leaves 9 px, which holds no word: the line goes under, where it has
  // 300 - 60 - 1 = 239 px, which is 34 characters, and "Does the fold open to the frontier"
  // with its ellipsis is 35.
  expect(oneLineCut({ after: 60, before: 230, room: 300, text: long, widthOf })).toEqual({
    cut: true,
    line: "Does the fold open to the",
    under: true,
    wraps: false,
  });
  // Parts before it as wide as the row itself send it under too, where it is drawn whole.
  expect(oneLineCut({ after: 0, before: 400, room: 400, text: long, widthOf })).toEqual({
    cut: false,
    line: long,
    under: true,
    wraps: false,
  });
});

/**
 * A first word no line of the row holds - a path, a URL - is drawn whole and wraps inside
 * itself: the row's box cutting its tail would cut inside a word, which is the one thing R1
 * bans, and `firstLineOf` has already held a token longer than its own limit to that limit.
 */
test("a first word no line of the row holds is drawn whole, wrapping inside itself", () => {
  const path = `${"a".repeat(60)}/b.ts and the words after it.`;
  expect(oneLineCut({ after: 0, before: 100, room: 200, text: path, widthOf })).toEqual({
    cut: true,
    line: `${"a".repeat(60)}/b.ts`,
    under: true,
    wraps: true,
  });
  expect(alone("a".repeat(40), 100)).toEqual({
    cut: false,
    line: "a".repeat(40),
    under: true,
    wraps: true,
  });
});
