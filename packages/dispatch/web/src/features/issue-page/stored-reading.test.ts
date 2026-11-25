import { expect, test } from "bun:test";

import type { DisplayTree } from "../../api/types";
import fixtures from "../refs/__fixtures__/display-trees.json";
import { wordsOf } from "../refs/tree-fragment";
import { lineWordsRaw } from "./issue-tree";
import { isPlainText, parsedReading, plainReading, storedReading } from "./stored-reading";
import { plainWords, plainWordsRaw, withShortAddresses } from "./tree-drafts";

/** A wall-clock budget for one stored text read to words, not one of the machine-sensitive waits
 *  this package's defaults cover (`web/src/__tests__/setup.ts`). The regression it guards, a
 *  parse with no bound on how deeply lists nest, spends about 9 s on the body below, and the
 *  bounded parse well under one, so a budget far above what a loaded pod spends on the bounded
 *  parse fails only an unbounded one. */
const NESTING_BUDGET_MS = 3_000;

/** A wall-clock budget for drawing again the lines of an issue's stored texts, each read once
 *  already. A result whose read carries no reading - a server of the build before sends none -
 *  has its line read from its stored text, and an issue under review holds hundreds of results:
 *  the redesign's own root issue held 431 results and 492 distinct stored texts on 2026-11-18.
 *  Reading 900 such texts again from what the first draw read costs tens of milliseconds; parsing
 *  each again, as a first draw does, costs about a second on a pod's CPU. So a budget far above
 *  the first and far below the second fails only a draw that parses every text again. */
const REDRAW_BUDGET_MS = 300;

// sjawhar/legion#2519: a stored text is read on the page's own thread, and micromark gives every
// open list or quote a turn on every line, so a body that opens thousands of them cost seconds or
// overflowed the stack. The parse holds lists and quotes to the document service's bound of 100
// blocks, where a list marker opens two (the list and its item), so the first 50 markers open
// lists and the rest read as the text they are, as the server reads them.
test("a stored text of thousands of nested list markers reads to words within the nesting bound", () => {
  const start = performance.now();
  const words = plainWordsRaw(`${"- ".repeat(3000)}x`);
  expect(performance.now() - start).toBeLessThan(NESTING_BUDGET_MS);
  expect(words).toBe(`${"- ".repeat(2950)}x`);
});

// A quote marker opens one block, so the first 100 of 150 open quotes and the 50 past the bound
// read as the text they are, as the server keeps them.
test("a stored text of quote markers past the nesting bound reads them as text", () => {
  expect(plainWordsRaw(`${">".repeat(150)} x`)).toBe(`${">".repeat(50)} x`);
});

// Front matter, a `---` fenced block a text opens with, reads as the server writes it: its lines
// between two plain fences, which are its words, so a line cut from the text's first block is the
// front matter. A `---` nothing closes is no front matter: it is a thematic break, which holds no
// words, before the paragraph.
test("a stored text's front matter reads as its fenced lines, and an unclosed fence as a rule", () => {
  const text = "---\ntitle: Spec\nowner: me\n---\nBody text.";
  expect(plainWordsRaw(text)).toBe("--- title: Spec owner: me --- Body text.");
  expect(lineWordsRaw({ text })).toBe("--- title: Spec owner: me ---");
  const unclosed = "---\n\nKeep the model pure.";
  expect(plainWordsRaw(unclosed)).toBe("Keep the model pure.");
  expect(lineWordsRaw({ text: unclosed })).toBe("Keep the model pure.");
});

// A page draws its rows again on every read that lands and every event, and a row's line is read
// from its stored text where the node has no reading, as a result's is from a server that sends
// none. The readings of the texts a draw read are remembered so that the next draw reads them
// rather than parses them again: 900 distinct stored texts, each reading held by what asked for
// it, must draw again within the budget, the second draw in the same order as the first, as a
// page draws its rows.
test("the lines of more stored texts than the memo holds are drawn again without parsing them again", () => {
  const texts = Array.from(
    { length: 900 },
    (_, at) =>
      `The run \`bun run test:web web/src/case-${at}.test.ts\` ends with **0 fail**, and names each test below as passed.\n` +
      `- the first named test of case ${at}: pass\n- the second named test of case ${at}: pass\n` +
      `${"It held at the head, read with the junit report. ".repeat(12)}`
  );
  for (const text of texts) lineWordsRaw({ text });
  const start = performance.now();
  for (const text of texts) lineWordsRaw({ text });
  expect(performance.now() - start).toBeLessThan(REDRAW_BUDGET_MS);
});

/** `words` on one line, its white space collapsed to single spaces, as every one-line reading of
 *  a stored text writes it (`plainWords`, `lineWords`). */
function collapsed(words: string): string {
  return words.replace(/\s+/gu, " ").trim();
}

/** Texts that hold nothing the parse reads as markup, each with a turn the plain path must read as
 *  the parse does: line breaks soft and hard, the spaces and tabs a line opens and ends with,
 *  blank lines, white space the parse keeps, addresses and links the parse reads as words, and
 *  the line endings markdown knows. */
const PLAIN_TEXTS: readonly string[] = [
  "Keep the model pure.",
  "The first line\nwraps onto a second\nand a third.",
  "A line that ends in two spaces  \nbreaks before this one.",
  "A line that ends in three spaces   \nbreaks too.",
  "A line that ends in one space \nruns on.",
  "A line that ends in a tab\t\nruns on.",
  "One paragraph.\n\nAnother paragraph.",
  "One paragraph.\n   \nAnother after a blank line of spaces.\n \n\nAnd a third.",
  "   Up to three spaces open this line,\n  two open this one,\n and one this.",
  "A non-breaking\u00a0space between two words.",
  "A line ending in a non-breaking space\u00a0\nruns on.",
  "Tabs\tinside\ta line.",
  "See dispatch://ACME-12 for the plan.",
  "Read dispatch://ACME-12/spec#b-r1 first, then dispatch://ACME-12.",
  "The page https://example.com/a?b=c answers.",
  "Or www.example.com, which links too.",
  "Mail ops@example.com about it.",
  "exit 0 (73 pass, 0 fail).",
  "2026 was the year it shipped.",
  "1a. is no list marker, nor is 1234567890. with ten digits.",
  "1234567890. ten digits open no list.",
  "Issue #12 holds it, and so does this # sign.",
  "A well-known, long-standing rule.",
  "Time: 10:30, path: /usr/local/bin, ratio 3/4.",
  "Ünïcödé wörds, 漢字, Ελληνικά and an emoji 🚀 or 👩‍💻.",
  "Quotes ‘curly’ and “double”, an ellipsis… and a dash — here.",
  "A question? An answer! Fifty percent, 50%, five dollars, $5, a caret ^ and braces {a}.",
  "A plus + sign, an equals = sign and a minus - sign inside a line.",
  "   \n  \n ",
  "",
  "x",
  "First line\rsecond line\r\rthird paragraph",
  "First line\r\nsecond line\r\n\r\nthird paragraph\r\n",
  "Mixed\r\nline\nendings\rhere",
  "\n\nLeading blank lines, then words.",
  "Words, then trailing blank lines and spaces.\n\n  \n",
  "A paragraph whose last line ends in two spaces  ",
  "A line\n\n\n\nfar from the next",
];

/** Whether the plain path (`plainReading`) reads `text`, a text it takes (`isPlainText`), as the
 *  parse does (`parsedReading`), to the character: to the same words, the same first block a
 *  node's line is cut from, and the same words with each address written short. */
function readsAsParsed(text: string): boolean {
  const plain = plainReading(text);
  const parsed = parsedReading(text);
  return (
    wordsOf(plain) === wordsOf(parsed) &&
    lineWordsRaw({ text, tree: plain }) === lineWordsRaw({ text, tree: parsed }) &&
    wordsOf(withShortAddresses(plain)) === wordsOf(withShortAddresses(parsed))
  );
}

// A text that holds no markup is read without the parse (`plainReading`), so its words must be
// the parse's words to the character: its whole words, the first block a node's line is cut from,
// and its words with each address written short.
test("a plain text is read without the parse into the words the parse reads", () => {
  for (const text of PLAIN_TEXTS) {
    const label = JSON.stringify(text);
    expect(isPlainText(text), label).toBe(true);
    expect(readsAsParsed(text), label).toBe(true);
    const parsed = parsedReading(text);
    expect(plainWordsRaw(text), label).toBe(collapsed(wordsOf(parsed)));
    expect(plainWords(text), label).toBe(collapsed(wordsOf(withShortAddresses(parsed))));
    expect(lineWordsRaw({ text }), label).toBe(collapsed(lineWordsRaw({ text, tree: parsed })));
  }
});

// Each family of markup the parse reads sends a text to the parse, and the text then reads as
// the parse reads it.
test("a text holding any family of markup is read by the parse", () => {
  for (const text of [
    "a `b` c",
    "a *b*",
    "a _b_",
    "a \\* b",
    "[x](y)",
    "![i](u)",
    "a [^1] note",
    "- [x] done",
    "<!-- c -->",
    "<https://example.com>",
    "> quote",
    "a &amp; b",
    "a | b",
    "~~x~~",
    "a\0b",
    "\uFEFF# Title",
    "# h",
    "   ## indented heading",
    "- item",
    "+ item",
    "1. one",
    "2) two",
    "a\n---",
    "---",
    "a\n===",
    "---\ntitle: x\n---\nbody",
    "    code",
    "\tcode",
    "  \tcode",
    "a\n    lazy",
  ]) {
    const label = JSON.stringify(text);
    expect(isPlainText(text), label).toBe(false);
    expect(storedReading(text), label).toEqual(parsedReading(text));
  }
});

// A GFM table needs no pipe: a paragraph line followed by a delimiter row of one cell is a table
// of one column, and its delimiter row may open with the colon that aligns the column. A running
// build of this head's server read each text below as a table whose one header cell is the
// paragraph line, the delimiter row no word of it, as the page's parse does; a delimiter cell
// written after other words is no delimiter row, and stays words.
test("a delimiter row that opens with a colon makes a table, read as the server reads it", () => {
  for (const [text, words] of [
    ["a\n:--", "a"],
    ["Name\n:---:", "Name"],
    ["one\n:-", "one"],
    ["a b\n :--", "a b"],
    ["Total\n:--- more", "Total :--- more"],
  ] as const) {
    expect(plainWordsRaw(text), text).toBe(words);
    expect(collapsed(lineWordsRaw({ text })), text).toBe(words);
  }
});

/** Every ASCII character, the control characters among them. */
const ASCII_CHARACTERS: readonly string[] = Array.from({ length: 0x80 }, (_, code) =>
  String.fromCharCode(code)
);

/** The characters the character sweep writes into its shapes: every ASCII one, and past ASCII the
 *  white space and invisible characters a parse could read apart from a word's letters, and the
 *  replacement character the parse writes for NUL. */
const SWEPT_CHARACTERS: readonly string[] = [
  ...ASCII_CHARACTERS,
  "\u00a0",
  "\u2028",
  "\u2029",
  "\u3000",
  "\ufeff",
  "\u200b",
  "\ufffd",
];

/** The short texts the character sweep reads, `X` standing for the swept character: alone, at a
 *  word's edges and between words, repeated, opening a line under a paragraph line as a table's
 *  delimiter row does, after spaces, after a blank line, after a digit, ending a line, and beside
 *  an address, which both readings must write short alike. */
const SWEPT_SHAPES: readonly string[] = [
  "X",
  "Xa",
  "X a",
  "aX",
  "aXb",
  "a X b",
  "a XX b",
  "XXX",
  "a\nX",
  "a\nXXX",
  "a\nX-",
  "a\nX--",
  "a\nX-X",
  "a\n Xb",
  "  Xa",
  "a\n\nX",
  "X\na",
  "aX\nb",
  "1X a",
  "a\n1X",
  "XaX",
  "a\nXaX",
  "see dispatch://ACME-12 X",
  "dispatch://ACME-12/spec#b-r1X",
];

// The plain path must read every text it takes as the parse does, whatever character the text
// holds where markup would stand. Each swept character is written into each short shape, and
// every text the plain path takes must read as parsed; the texts that do not are listed. The
// sweep must take a text holding each letter, so a plain path that took no text at all fails it.
test("each character in each short shape the plain path takes reads as the parse reads it", () => {
  const failing: string[] = [];
  const taken = new Set<string>();
  for (const character of SWEPT_CHARACTERS) {
    for (const shape of SWEPT_SHAPES) {
      const text = shape.replaceAll("X", character);
      if (!isPlainText(text)) continue;
      taken.add(character);
      if (!readsAsParsed(text)) failing.push(JSON.stringify(text));
    }
  }
  expect(failing).toEqual([]);
  const letters = ASCII_CHARACTERS.filter((character) => /[A-Za-z]/u.test(character));
  expect(letters.filter((letter) => !taken.has(letter))).toEqual([]);
});

/** A small seeded generator of numbers from 0 up to 1 (mulberry32), so the random sweep draws the
 *  same texts on every run and a text it fails on fails again. */
function seededRandom(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let mixed = Math.imul(state ^ (state >>> 15), state | 1);
    mixed ^= mixed + Math.imul(mixed ^ (mixed >>> 7), mixed | 61);
    return ((mixed ^ (mixed >>> 14)) >>> 0) / 2 ** 32;
  };
}

/** The printable ASCII characters that are neither letters, digits nor white space. */
const ASCII_MARKS = ASCII_CHARACTERS.filter((mark) => /[\p{P}\p{S}]/u.test(mark));

/** What the random sweep draws its texts from, one symbol at a time: two letters and a digit, a
 *  space, a tab and a line feed, every printable ASCII mark, a non-breaking space, and an address,
 *  drawn whole, so that marks and white space meet an address as they meet words. */
const RANDOM_ALPHABET: readonly string[] = [
  ..."ab1 \t\n",
  ...ASCII_MARKS,
  "\u00a0",
  "dispatch://ACME-1",
];

// Symbols drawn at random meet in ways no list of shapes names. Each of 3000 texts of 1 to 14
// symbols drawn from words, marks, white space and an address that the plain path takes must read
// as the parse reads it; the texts that do not are listed. The seed is fixed, so every run draws
// the same texts.
test("random short texts the plain path takes read as the parse reads them", () => {
  const random = seededRandom(180);
  const failing: string[] = [];
  for (let drawn = 0; drawn < 3000; drawn += 1) {
    let text = "";
    for (let left = 1 + Math.floor(random() * 14); left > 0; left -= 1) {
      text += RANDOM_ALPHABET[Math.floor(random() * RANDOM_ALPHABET.length)];
    }
    if (isPlainText(text) && !readsAsParsed(text)) failing.push(JSON.stringify(text));
  }
  expect(failing).toEqual([]);
});

// A parsed reading is remembered while any holder that asked for its text lives, and shared:
// another holder, and a caller that names none, read the same reading rather than a parse of
// their own.
test("a parsed reading is shared by every holder of its text and by a caller naming none", () => {
  const text = "A **held** reading of `code` no other test reads.";
  const first = {};
  const second = {};
  const reading = storedReading(text, first);
  expect(storedReading(text, second)).toBe(reading);
  expect(storedReading(text)).toBe(reading);
  expect(storedReading(text, first)).toBe(reading);
});

// GFM's autolink literals are left out of the parse (`STORED_TEXT_PARSE`): a bare URL, a `www.`
// address and an email read as the same words linked or not, so the parse reads each as the text
// it is written as, with no link, and a bare address beside them is still written short.
test("the parse reads a bare URL, a www address and an email as the words they are written as", () => {
  const text =
    "See https://example.com/a?b=c,\nwww.example.com  and someone@example.com\nbeside dispatch://ACME-12.";
  const reading = parsedReading(text);
  expect(JSON.stringify(reading)).not.toContain('"link"');
  expect(collapsed(wordsOf(reading))).toBe(collapsed(text));
  expect(collapsed(wordsOf(withShortAddresses(reading)))).toBe(
    "See https://example.com/a?b=c, www.example.com and someone@example.com beside ACME-12."
  );
});

/** One text of the display-tree fixture `pmdoc`'s `TestWebDisplayFixture` writes: its name, the
 *  markdown, and the server's reading of that markdown's latest version. */
interface ServerReading {
  readonly name: string;
  readonly markdown: string;
  readonly latest: DisplayTree;
}

/** The fixture's text named `name`, read by the server. */
function serverReading(name: string): ServerReading {
  const found = (fixtures as ServerReading[]).find((each) => each.name === name);
  if (found === undefined) throw new Error(`no text named ${name} in the display-tree fixture`);
  return found;
}

/** The first `ordered_list` among the blocks of `tree`. */
function orderedListOf(tree: DisplayTree): DisplayTree | undefined {
  return (tree.content as readonly DisplayTree[] | undefined)?.find(
    (block) => block.type === "ordered_list"
  );
}

/** The number `list` says it starts at, `undefined` where it says none. */
function orderOf(list: DisplayTree | undefined): unknown {
  return (list?.attrs as { readonly order?: unknown } | undefined)?.order;
}

// The server writes the number an ordered list starts at as the list's `order`, and leaves it
// out where that is 1, so the page's own reading of a stored text must do the same: a list from
// 3 read as a list from 1 draws the wrong numbers. The server's reading of the text from 3 is
// the fixture's, written from the same text.
test("an ordered list from 3 reads its start as the server's reading does, and one from 1 none", () => {
  const fromThree = "3. Keep the old page.\n4. Then drop it.";
  const server = serverReading("ordered-from-3");
  expect(server.markdown).toBe(`${fromThree}\n`);
  const list = orderedListOf(storedReading(fromThree));
  expect(orderOf(list)).toBe(3);
  expect(orderOf(list)).toBe(orderOf(orderedListOf(server.latest)));
  const fromOne = orderedListOf(storedReading("1. one\n2. two"));
  expect(fromOne?.type).toBe("ordered_list");
  expect(orderOf(fromOne)).toBeUndefined();
});
