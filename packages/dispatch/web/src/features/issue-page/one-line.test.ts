import { expect, test } from "bun:test";

import { shortForm } from "../refs/routes";
import { drawnLine, firstLineOf } from "./first-line";
import { oneLineCut, referenceStandIns } from "./one-line";

/** A face that draws every character 7 px wide, so each budget below is a sum a reader can
 *  check by hand. */
const widthOf = (text: string) => text.length * 7;

/** The line a row `room` px wide draws of `text`, with nothing beside it. */
function alone(text: string, room: number) {
  return oneLineCut({ after: 0, before: 0, room, text, widthOf });
}

/**
 * R1, R14: a Contents entry draws its node's first line on one line, and no entry truncates a
 * word, at every width the pane is drawn at. The cut is made in pixels,
 * against the room the entry's own box leaves the line, so the ellipsis is part of what has to
 * fit.
 */
test("a one-line row cuts its first line at a word to the pixels it has, never inside one", () => {
  // Short enough: the whole line, and no ellipsis, since nothing was cut.
  expect(alone("Short enough.", 200)).toEqual({
    cut: false,
    line: "Short enough.",
    sentence: true,
    under: false,
    wraps: false,
  });

  // A sentence that ends inside the room ends the line, as it does on a row that wraps, and
  // says the text carries on.
  expect(alone("One sentence. And another.", 200)).toEqual({
    cut: true,
    line: "One sentence.",
    sentence: true,
    under: false,
    wraps: false,
  });

  // A whole sentence is drawn with no ellipsis after its own mark (`drawnLine`), so none is
  // counted: the sentence fits a room its 13 characters fill, where its ellipsis would not.
  expect(alone("One sentence. And another.", 13 * 7 + 1).line).toBe("One sentence.");

  // Longer than the room: the line ends at the last word whose run and ellipsis fit, so the
  // stored text carries on at a word break. 169 px less the pixel kept for rounding is 24
  // characters: "The page draws the" and its ellipsis are 19, "The page draws the whole" and
  // its are 25.
  const long = "The page draws the whole issue in one reading, with nothing lost.";
  const cut = alone(long, 169);
  expect(cut).toEqual({
    cut: true,
    line: "The page draws the",
    sentence: false,
    under: false,
    wraps: false,
  });
  expect(long.charAt(cut.line.length)).toBe(" ");

  // The ellipsis is counted: a run that fits only without it is one word too long.
  expect(alone(long, 8 * 7 + 1).line).toBe("The");
  expect(alone(long, 9 * 7 + 1).line).toBe("The page");

  // An empty text draws an empty line.
  expect(alone("", 100)).toEqual({
    cut: false,
    line: "",
    sentence: false,
    under: false,
    wraps: false,
  });
});

/**
 * The parts beside the line take from it: what stands before it (`before`) and after it
 * (`after`). Where they leave it less than its first word, it goes on a line of its own under
 * them, which only `after` still takes from: an entry that kept it beside them would draw it
 * 0 px wide, and hide the parts after it past its own edge. The Contents list passes every part
 * of its row as `before`, so a line under the row has the row's whole width at its indent.
 */
test("a one-line row's first line goes under its other words where they leave it no word", () => {
  const long = "Does the fold open to the frontier on a phone too?";
  // 300 px, 200 before and 60 after: 39 px beside them, the pixel for rounding kept, and
  // "Does…" is 35.
  expect(oneLineCut({ after: 60, before: 200, room: 300, text: long, widthOf })).toEqual({
    cut: true,
    line: "Does",
    sentence: false,
    under: false,
    wraps: false,
  });
  // 230 before leaves 9 px, which holds no word: the line goes under, where it has
  // 300 - 60 - 1 = 239 px, which is 34 characters, and "Does the fold open to the frontier"
  // with its ellipsis is 35.
  expect(oneLineCut({ after: 60, before: 230, room: 300, text: long, widthOf })).toEqual({
    cut: true,
    line: "Does the fold open to the",
    sentence: false,
    under: true,
    wraps: false,
  });
  // Parts before it as wide as the row itself send it under too, where it is drawn whole.
  expect(oneLineCut({ after: 0, before: 400, room: 400, text: long, widthOf })).toEqual({
    cut: false,
    line: long,
    sentence: true,
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
    sentence: false,
    under: true,
    wraps: true,
  });
  expect(alone("a".repeat(40), 100)).toEqual({
    cut: false,
    line: "a".repeat(40),
    sentence: false,
    under: true,
    wraps: true,
  });
});

/**
 * The tester's F20: a plain line the page draws is no link, so it names each reference in plain
 * words - a block by the page's name for it, an issue by its key, and anything the page has no
 * name for by its short form - and drops a typed block's source line, which is markup and no
 * word of the text. No address, block id or fence is left.
 */
test("a plain line names each reference and drops a typed block's source line", () => {
  const block = "dispatch://KEY-1/spec#b-r1";
  const text = [
    ":::requirement{#r1}",
    `The page cites ${block}, dispatch://KEY-1 and dispatch://KEY-1/artifact/notes.`,
  ].join("\n");
  const references = referenceStandIns((reference) =>
    reference === block ? "the whole issue" : undefined
  );
  const line = references.named(references.standIn(text));
  const notes = shortForm({ key: "KEY-1", kind: "artifact", slug: "notes" });
  expect(line).toBe(`The page cites the whole issue, KEY-1 and ${notes}.`);
  for (const mark of [":::", "#b-", "dispatch://"]) {
    expect(line).not.toContain(mark);
  }
});

/**
 * The tester's F20 on a line the page cuts: a reference is named only after the cut, so its
 * name takes no part in it. A statement's name opens with its label ("R2."), which read as a
 * sentence end would end the line inside the name; the limit never falls at one of the name's
 * own spaces; and a reference counts against `firstLineOf`'s 100 characters as its name's
 * characters, which are what the line draws, not its address's.
 */
test("a line is cut with each reference still unnamed, and each is named after the cut", () => {
  const block = "dispatch://KEY-1/spec#b-r2";
  const nameOf = (reference: string) => (reference === block ? "R2. Every address" : undefined);
  const lineOf = (text: string) => {
    const references = referenceStandIns(nameOf);
    return references.named(drawnLine(firstLineOf(references.standIn(text))));
  };

  // The name's label ends no sentence: the line ends where its author's sentence does.
  expect(lineOf(`Keep ${block} and dispatch://KEY-2 whole. Then more.`)).toBe(
    "Keep R2. Every address and KEY-2 whole."
  );
  // 73 characters, the name's 17 and " says." are 96, so the sentence is the line; its address's
  // 26 characters would make it 105, and cut it inside the sentence.
  const before = `${"word ".repeat(14)}as `;
  expect(lineOf(`${before}${block} says. Then more.`)).toBe(`${before}R2. Every address says.`);
  // 88 characters and the name pass the limit inside the name: the line ends before it, at the
  // author's own space, and is drawn with its ellipsis.
  const long = `${"word ".repeat(17)}as`;
  expect(lineOf(`${long} ${block} says, and more words follow.`)).toBe(`${long}…`);

  // One holder stands in for the references of several texts, each by a token of its own.
  const references = referenceStandIns(nameOf);
  const first = references.standIn(`See ${block}.`);
  const second = references.standIn("Ask dispatch://KEY-3 first.");
  expect(references.named(`${first} ${second}`)).toBe("See R2. Every address. Ask KEY-3 first.");
});

// R1 and R5: a plain line draws its author's words as they wrote them, with each reference named.
// A reference stands in by a private-use character while the line is cut, and a text may hold
// that character of its own: an icon a reader pasted from a terminal or an icon font is one
// (U+E000 to U+F8FF). Found by the tester on a running build at 0058727e, where a comment that
// read "<U+E000> dispatch://ACME-7 then" drew its Contents entry as "ACME-7 ACME-7…":
// the author's own character was drawn as the reference's name.
test("a private-use character the author wrote is drawn as written, never as a reference's name", () => {
  const references = referenceStandIns(() => undefined);
  const text = "\ue000 marks dispatch://KEY-2 here, and \ue001 is the next icon.";
  expect(references.named(references.standIn(text))).toBe(
    "\ue000 marks KEY-2 here, and \ue001 is the next icon."
  );
});

// R1 and R3: a line cut inside its sentence draws one ellipsis. A name the page gives a statement
// is cut by `chipWords` and so can end with its own ellipsis, and a cut that falls at the space
// right after the name drew a second one. Found by the tester on a running build at
// 0058727e, where a comment that read "Does it hold, as dispatch://ACME-1/spec#b-r-home
// says? I think so." drew its entry in the Outline sheet as "Does it hold, as R5. A spec node
// shows the……".
test("a line cut right after a name that ends with an ellipsis draws one ellipsis", () => {
  const block = "dispatch://KEY-1/spec#b-r2";
  const references = referenceStandIns((reference) =>
    reference === block ? "R2. Every address…" : undefined
  );
  // 78 characters, then the name's 18: the space after it is the last one inside the limit.
  const before = `${"word ".repeat(15)}as `;
  const text = `${before}${block} says the rest of it, and more words follow on.`;
  const line = references.named(drawnLine(firstLineOf(references.standIn(text))));
  expect(line.startsWith(`${before}R2. Every address`), `the line is ${line}`).toBe(true);
  expect(line, "the line draws two ellipses").not.toContain("……");
});

// R1: a line cut inside its sentence draws its ellipsis whatever its last character, so a cut
// right after a name that ends with a full stop - the last word of a sentence the name quotes -
// still says the author's sentence carries on. Only a name that ends with an ellipsis of its own
// draws no second one.
test("a line cut right after a name that ends with a full stop still draws its ellipsis", () => {
  const block = "dispatch://KEY-1/spec#b-r2";
  const references = referenceStandIns((reference) =>
    reference === block ? "R2. Every address." : undefined
  );
  // 78 characters, then the name's 18: the space after it is the last one inside the limit.
  const before = `${"word ".repeat(15)}as `;
  const text = `${before}${block} says the rest of it, and more words follow on.`;
  expect(references.named(drawnLine(firstLineOf(references.standIn(text))))).toBe(
    `${before}R2. Every address.…`
  );
});

// R1 and R3: a run cut at a word inside the line is measured as it is drawn. Where the author's
// own ellipsis ends it, it is drawn with no second, so it fits the pixels it takes with one:
// "Hold on, wait…" is 14 characters, 98 px, inside the 99 px a 100 px row leaves it, where
// "Hold on, wait……" would be 105 px and the line would end a word sooner.
test("a run that ends with the author's own ellipsis is measured with that one alone", () => {
  expect(alone("Hold on, wait… then the rest of the line runs on and on.", 100)).toEqual({
    cut: true,
    line: "Hold on, wait…",
    sentence: false,
    under: false,
    wraps: false,
  });
});
