import { expect, test } from "bun:test";

import { drawnLine, firstLineOf } from "./first-line";
import { type LineNames, oneLineCut, referenceStandIns } from "./one-line";
import { NODE_TYPE_NAMES } from "./page-words";
import { knownIssueKeys } from "./row-references";

/** A face that draws every character 7 px wide, so each budget below is a sum a reader can
 *  check by hand. */
const widthOf = (text: string) => text.length * 7;

/** The line a row `room` px wide draws of `text`, with nothing beside it. */
function alone(text: string, room: number) {
  return oneLineCut({ after: 0, before: 0, room, text, widthOf });
}

/** What a page that published `names` - each target's name, an issue's title among them - and
 *  `lines` - the line a chip names a block by - names a plain line's references by
 *  (`useIssueNames`), on the issue `KEY-1`; nothing deleted, edited or ambiguous. */
function published(
  names: Readonly<Record<string, string>> = {},
  lines: Readonly<Record<string, string>> = {}
): LineNames {
  const named = new Map(Object.entries(names));
  return {
    ambiguous: new Set(),
    author: () => "",
    deleted: new Map(),
    edited: new Set(),
    issue: "KEY-1",
    issues: knownIssueKeys(named),
    lines: new Map(Object.entries(lines)),
    names: named,
  };
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
 * words, as the node's row in the outline names it - a block as its chip names it, an issue by
 * its key and its title, and a whole document by the page's word for one - and drops a typed
 * block's source line, which is markup and no word of the text. No address, block id or fence is
 * left.
 */
test("a plain line names each reference and drops a typed block's source line", () => {
  const block = "dispatch://KEY-1/spec#b-r1";
  const text = [
    ":::requirement{#r1}",
    `The page cites ${block}, dispatch://KEY-1 and dispatch://KEY-1/artifact/notes.`,
  ].join("\n");
  const references = referenceStandIns(
    published({ "dispatch://KEY-1": "The first issue" }, { [block]: "the whole issue" })
  );
  const line = references.named(references.standIn(text));
  expect(line).toBe(
    `The page cites the whole issue, KEY-1 The first issue and ${NODE_TYPE_NAMES.document}.`
  );
  for (const mark of [":::", "#b-", "dispatch://"]) {
    expect(line).not.toContain(mark);
  }
});

// A plain line names an issue as the node's row in the outline does (`rowReferenceWords`): by
// its key and its title, once the page has published the title. One the page published no name
// for - it has not answered, or is gone - reads as the words its author wrote, as the row draws
// it, and never by its key alone, which the row never draws.
test("a plain line names a resolved issue by its key and title, and an unresolved one as written", () => {
  const references = referenceStandIns(published({ "dispatch://KEY-7": "The new issue page" }));
  expect(
    references.named(references.standIn("See dispatch://KEY-7 and dispatch://KEY-9 first."))
  ).toBe("See KEY-7 The new issue page and dispatch://KEY-9 first.");
});

// R8: a reference the page published no name for - another issue's comment it has not answered
// for, or answered is gone - reads as the words its author wrote, as the row draws it, and never
// as its short form ("OTHER-1 comment"), which names no one comment.
test("a reference the page published no name for reads as written, never as its short form", () => {
  const gone = "dispatch://OTHER-1/comment/0f8e2c3a-6b1d-4e5f-9a7c-2d3e4f5a6b7c";
  const references = referenceStandIns(published());
  const line = references.named(
    drawnLine(firstLineOf(references.standIn(`It follows ${gone} closely. Then more.`)))
  );
  expect(line).toBe(`It follows ${gone} closely.`);
  expect(line).not.toContain("OTHER-1 comment");
});

// A bare key is a reference where the page named an issue for it (`knownIssueKeys`), as the row
// links one, and reads as the key and the title; a key the page named nothing for is the words
// it is.
test("a bare key the page named an issue for reads as its key and title", () => {
  const references = referenceStandIns(published({ "dispatch://KEY-7": "The new issue page" }));
  expect(references.named(references.standIn("Ask KEY-7 first, not KEY-9."))).toBe(
    "Ask KEY-7 The new issue page first, not KEY-9."
  );
});

// A block reads as its chip names it in the row (`chipName`): its line, cut at a word to a chip's
// 32 characters with its ellipsis.
test("a plain line names a block as its chip names it", () => {
  const block = "dispatch://KEY-1/spec#b-r3";
  const references = referenceStandIns(
    published({}, { [block]: "Every address in a node's words is drawn by its name" })
  );
  expect(references.named(references.standIn(`Keep ${block} as it is.`))).toBe(
    "Keep Every address in a node's words… as it is."
  );
});

// R1: a title is a name of many words, each its own token, so `firstLineOf`'s limit cuts the
// line at a word inside the title, and the line draws one ellipsis. 11 characters, then the
// title's: its sixth "draws" ends at the 100th character, and the space after it is the last one
// inside the limit.
test("a long title is cut at a word inside it, with one ellipsis", () => {
  const title = `The new issue page ${"draws a line ".repeat(8)}whole`;
  const references = referenceStandIns(published({ "dispatch://KEY-7": title }));
  const line = references.named(
    drawnLine(firstLineOf(references.standIn("Read dispatch://KEY-7 before the rest.")))
  );
  expect(line).toBe(`Read KEY-7 The new issue page ${"draws a line ".repeat(5)}draws…`);
  expect(line.match(/…/gu), "the line draws more than one ellipsis").toHaveLength(1);
});

/**
 * The tester's F20 on a line the page cuts: a reference is named only after the cut, so its
 * name's marks take no part in it. A statement's name opens with its label ("R2."), which read as
 * a sentence end would end the line inside the name; the limit falls at a space and never inside
 * a word of the name; and a reference counts against `firstLineOf`'s 100 characters as its
 * name's characters, which are what the line draws, not its address's.
 */
test("a line is cut with each reference still unnamed, and each is named after the cut", () => {
  const block = "dispatch://KEY-1/spec#b-r2";
  const names = published(
    { "dispatch://KEY-2": "Two", "dispatch://KEY-3": "Three" },
    { [block]: "R2. Every address" }
  );
  const lineOf = (text: string) => {
    const references = referenceStandIns(names);
    return references.named(drawnLine(firstLineOf(references.standIn(text))));
  };

  // The name's label ends no sentence: the line ends where its author's sentence does.
  expect(lineOf(`Keep ${block} and dispatch://KEY-2 whole. Then more.`)).toBe(
    "Keep R2. Every address and KEY-2 Two whole."
  );
  // 73 characters, the name's 17 and " says." are 96, so the sentence is the line; its address's
  // 26 characters would make it 105, and cut it inside the sentence.
  const before = `${"word ".repeat(14)}as `;
  expect(lineOf(`${before}${block} says. Then more.`)).toBe(`${before}R2. Every address says.`);
  // 88 characters and the name pass the limit inside the name. Each word of a name is its own
  // token, so the limit cuts at the last of the name's own spaces inside it, after "Every", and
  // never inside a word: the line keeps the words of the name that fit, with its ellipsis, rather
  // than end before the whole name.
  const long = `${"word ".repeat(17)}as`;
  expect(lineOf(`${long} ${block} says, and more words follow.`)).toBe(`${long} R2. Every…`);

  // One holder stands in for the references of several texts, each by tokens of its own.
  const references = referenceStandIns(names);
  const first = references.standIn(`See ${block}.`);
  const second = references.standIn("Ask dispatch://KEY-3 first.");
  expect(references.named(`${first} ${second}`)).toBe(
    "See R2. Every address. Ask KEY-3 Three first."
  );
});

// R1 and R5: a plain line draws its author's words as they wrote them, with each reference named.
// A reference stands in by a private-use character while the line is cut, and a text may hold
// that character of its own: an icon a reader pasted from a terminal or an icon font is one
// (U+E000 to U+F8FF). Found by the tester on a running build at 0058727e, where a comment that
// read "<U+E000> dispatch://ACME-7 then" drew its Contents entry as "ACME-7 ACME-7…":
// the author's own character was drawn as the reference's name.
test("a private-use character the author wrote is drawn as written, never as a reference's name", () => {
  const references = referenceStandIns(published({ "dispatch://KEY-2": "Two" }));
  const text = "\ue000 marks dispatch://KEY-2 here, and \ue001 is the next icon.";
  expect(references.named(references.standIn(text))).toBe(
    "\ue000 marks KEY-2 Two here, and \ue001 is the next icon."
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
  const references = referenceStandIns(published({}, { [block]: "R2. Every address…" }));
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
  const references = referenceStandIns(published({}, { [block]: "R2. Every address." }));
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
