import { beforeAll, expect, test } from "bun:test";

import type { DisplayTree } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { firstWordedBlock, wordsOf } from "../refs/tree-fragment";
import { projectedNames } from "./projection-fixture";
import {
  countParses,
  isPlainText,
  parsedReading,
  plainReading,
  storedReading,
} from "./stored-reading";

// One row reads a reading through the shared engine, which loads lazily, and the rows run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The words of a stored text's reading on one line (`storedReading`), its white space
 *  collapsed to single spaces, which is what a chip's name is projected from. */
function wordsOfText(markdown: string, holder?: object): string {
  return collapsed(wordsOf(storedReading(markdown, holder)));
}

/** The words of the first block of `reading` that holds any (`firstWordedBlock`), which is what
 *  a node's one line is projected from; `""` for a reading none of whose blocks holds a word. */
function firstBlockWords(reading: DisplayTree): string {
  const block = firstWordedBlock(reading);
  return block === undefined ? "" : collapsed(wordsOf(block));
}

// sjawhar/legion#2519: a stored text is read on the page's own thread, and micromark gives every
// open list or quote a turn on every line, so a body that opens thousands of them cost seconds or
// overflowed the stack. The parse holds lists and quotes to the document service's bound of 100
// blocks, where a list marker opens two (the list and its item), so the first 50 markers open
// lists and the rest read as the text they are, as the server reads them. The words are the
// proof: a parse with no bound opens a list for every marker and leaves none of them as text,
// and one that overflows the stack throws, so neither reads to these words, and no clock is read.
test("a stored text of thousands of nested list markers reads to words within the nesting bound", () => {
  expect(wordsOfText(`${"- ".repeat(3000)}x`)).toBe(`${"- ".repeat(2950)}x`);
});

// The server keeps the number a stored ordered list starts on as the list's `order`
// (`pmdoc/parse.go`'s `parseList`, `list.Start`), and so does the reading of a stored text the
// read sent none for, so a name the page projects from it reads `3. Ship it`, never `1.`.
test("a stored list that starts at 3 reads its first item as 3", () => {
  const reading = storedReading("3. Ship it\n4. Then the rest");
  const [list] = (reading.content ?? []) as DisplayTree[];
  expect(list?.type).toBe("ordered_list");
  expect(list?.attrs).toEqual({ order: 3 });
  expect(projectedNames(new Map([["stored", { tree: reading }]])).get("stored")).toBe(
    "3. Ship it 4. Then the rest"
  );
  const [bullets] = (storedReading("- Ship it").content ?? []) as DisplayTree[];
  expect(bullets?.attrs, "a bullet list holds no number").toBeUndefined();
});

// An author's own private-use character - an icon font's, a pasted terminal glyph - is a word of
// the text: a stored text's reading holds it as written outside code, inside a code span and
// beside an escape, as the server's reading does, and the shared projection reads it to the same
// plain words, as the names store projects a stored text's reading (`projectedNames`).
test("a stored text read to plain words keeps the author's private-use characters as written", () => {
  const star = "\ue02a";
  const low = "\ue05f";
  for (const [stored, words] of [
    [`an icon ${star} and ${low} here`, `an icon ${star} and ${low} here`],
    [`Run \`a${star}b${low}c\` now`, `Run a${star}b${low}c now`],
    [`\\*${star} and ${low}\\_`, `*${star} and ${low}_`],
    [`${star} \\*not emphasis\\* and \`a*b\``, `${star} *not emphasis* and a*b`],
    ["\ue000\\! and `\ue020!`", "\ue000! and \ue020!"],
  ] as const) {
    expect(wordsOfText(stored), stored).toBe(words);
    const projected = projectedNames(new Map([["stored", { tree: storedReading(stored) }]]));
    expect(projected.get("stored"), stored).toBe(words);
  }
});

/** What a stored text reads to on one line, as the row above reads it: the words of its reading
 *  (`wordsOfText`), and the name the names store projects from that reading (`projectedNames`),
 *  `""` where the reading holds no word. */
function plainLine(stored: string): { name: string; words: string } {
  const projected = projectedNames(new Map([["stored", { tree: storedReading(stored) }]]));
  return { name: projected.get("stored") ?? "", words: wordsOfText(stored) };
}

// A stored text reads to the words the server's reading of it holds: what markdown reads
// literally stays and its marks go. A bare address, and an autolink's, is the words its author
// wrote, as written, while the name projected from the reading writes it as its target's short
// form, as a name reads no other name; an address in a code span stays as written in both.
test("a stored text read to plain words keeps what markdown reads literally and drops its marks", () => {
  for (const [stored, words, name = words] of [
    // A backslash before punctuation is that character, never a mark nor a sentence's end.
    ["**1\\. Stand the harness up.** Run it.", "1. Stand the harness up. Run it."],
    ["\\*not emphasis\\*", "*not emphasis*"],
    ["\\# not a heading", "# not a heading"],
    ["\\`not code\\`", "`not code`"],
    // A code span's content is verbatim: no mark and no escape is read inside it.
    ["Run `a * b \\. c` now", "Run a * b \\. c now"],
    ["Run `**not bold**` now", "Run **not bold** now"],
    ["``a `tick` b``", "a `tick` b"],
    ["an `open run", "an `open run"],
    // An image holds no words; a link reads as its words, an autolink as its URL.
    ["see ![diagram](shot.png) here", "see here"],
    ["![diagram](shot.png)", ""],
    ["[the plan](https://example.com/plan)", "the plan"],
    ["<https://example.com/a_b*c*>", "https://example.com/a_b*c*"],
    // A link by reference links its words to its label's definition, so one whose words are its
    // own address names it as an autolink does.
    ["[dispatch://CORE-4][core]\n\n[core]: dispatch://CORE-4", "dispatch://CORE-4", "CORE-4"],
    // An escaped mark after an address is the character it escapes, so it trails the address as
    // a sentence's end.
    [
      "see dispatch://CORE-2 and <dispatch://CORE-3>",
      "see dispatch://CORE-2 and dispatch://CORE-3",
      "see CORE-2 and CORE-3",
    ],
    ["see dispatch://CORE-2\\.", "see dispatch://CORE-2.", "see CORE-2."],
    ["Run `dispatch read dispatch://CORE-2` now", "Run dispatch read dispatch://CORE-2 now"],
    [
      "`dispatch://CORE-2` is dispatch://CORE-2",
      "dispatch://CORE-2 is dispatch://CORE-2",
      "dispatch://CORE-2 is CORE-2",
    ],
    // An underscore inside a word is no mark; marks nest.
    ["snake_case_name", "snake_case_name"],
    ["_emphasis_ and __strong__", "emphasis and strong"],
    ["***x***", "x"],
    ["**a *b* c** and ~~gone~~", "a b c and gone"],
    ["2 * 3 * 4", "2 * 3 * 4"],
    // A heading marker goes at the start of any line, and white space collapses.
    ["## Keep it\nKeep the model pure.", "Keep it Keep the model pure."],
    ["   ### Indented", "Indented"],
    ["  one\n\ntwo   three  ", "one two three"],
  ] as const) {
    expect(plainLine(stored), stored).toEqual({ name, words });
  }
});

// A blockquote's and a list item's markers are markup, which the server's reading holds as the
// blocks they open and no word of them, so a stored text reads to words without them at the
// start of each line, nested ones and a task box included, and without a heading's marker after
// them; the name projected from the reading writes an ordered item's number as a rendered body
// draws it, and no other marker. What markdown reads literally keeps them: an escaped `1\.` or
// `\-` opens no list, and `**bold**` at a line's start is no bullet, so a plan task's label still
// reads `1.`.
test("a stored text read to plain words drops the markers of a quote and of a list", () => {
  for (const [stored, words, name = words] of [
    ["- Hold the deploy.\n- Then ship.", "Hold the deploy. Then ship."],
    ["> quoted words\n\nmy reply", "quoted words my reply"],
    ["> > deeper\n>> deepest\n   > indented", "deeper deepest indented"],
    ["1. one\n2. two\n10) ten", "one two ten", "1. one 2. two 10. ten"],
    ["- [x] done\n* [ ] to do\n+ [X] also done", "done to do also done"],
    [
      "- one\n  - nested\n    1. deeper\n\t+ tabbed",
      "one nested deeper tabbed",
      "one nested 1. deeper tabbed",
    ],
    ["> ## Title\n> - item", "Title item"],
    ["1\\. not a list\n\\- nor this", "1. not a list - nor this"],
    [
      "**1\\. Stand the harness up.** Run it.\n**2\\. Then ship.**",
      "1. Stand the harness up. Run it. 2. Then ship.",
    ],
    ["*emphasis* stays a word", "emphasis stays a word"],
  ] as const) {
    expect(plainLine(stored), stored).toEqual({ name, words });
  }
});

// A quote marker opens one block, so the first 100 of 150 open quotes and the 50 past the bound
// read as the text they are, as the server keeps them.
test("a stored text of quote markers past the nesting bound reads them as text", () => {
  expect(wordsOfText(`${">".repeat(150)} x`)).toBe(`${">".repeat(50)} x`);
});

// Front matter, a `---` fenced block a text opens with, reads as the server writes it: its lines
// between two plain fences, which are its words, so a line cut from the text's first block is the
// front matter. A `---` nothing closes is no front matter: it is a thematic break, which holds no
// words, before the paragraph.
test("a stored text's front matter reads as its fenced lines, and an unclosed fence as a rule", () => {
  const text = "---\ntitle: Spec\nowner: me\n---\nBody text.";
  expect(wordsOfText(text)).toBe("--- title: Spec owner: me --- Body text.");
  expect(firstBlockWords(storedReading(text))).toBe("--- title: Spec owner: me ---");
  const unclosed = "---\n\nKeep the model pure.";
  expect(wordsOfText(unclosed)).toBe("Keep the model pure.");
  expect(firstBlockWords(storedReading(unclosed))).toBe("Keep the model pure.");
});

// A page draws its rows again on every read that lands and every event, and a chip naming an
// ask, a comment or a message is named from its stored text's reading, as is the reading the
// Change of a decision with no reading quotes. The readings of the texts a draw read are
// remembered so that the next draw reads them rather than parses them again: 900 distinct stored
// texts, each reading held by what asked for it, read twice in the same order as a page draws
// its rows, are parsed once each by the first draw and not at all by the second. The parses are
// counted (`countParses`), since a draw that parses every text again is quick on a fast machine
// and a timed bound would pass it there. Each run of this row reads texts of its own, so a
// `--rerun-each` run finds none of them remembered from the run before it.
test("the readings of 900 stored texts are read again without parsing them again", () => {
  const run = crypto.randomUUID();
  const texts = Array.from(
    { length: 900 },
    (_, at) =>
      `The run \`bun run test:web web/src/case-${at}.test.ts\` ends with **0 fail**, and names each test below as passed (${run}).\n` +
      `- the first named test of case ${at}: pass\n- the second named test of case ${at}: pass\n` +
      `${"It held at the head, read with the junit report. ".repeat(12)}`
  );
  const holders = texts.map(() => ({}));
  const draw = () => {
    for (const [at, text] of texts.entries()) storedReading(text, holders[at]);
  };
  expect(countParses(draw)).toBe(texts.length);
  expect(countParses(draw)).toBe(0);
});

/** `words` on one line, its white space collapsed to single spaces, as every one-line reading of
 *  a stored text writes it. */
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
 *  parse does (`parsedReading`), to the character: to the same words and the same first block a
 *  node's line is projected from. */
function readsAsParsed(text: string): boolean {
  const plain = plainReading(text);
  const parsed = parsedReading(text);
  return wordsOf(plain) === wordsOf(parsed) && firstBlockWords(plain) === firstBlockWords(parsed);
}

// A text that holds no markup is read without the parse (`plainReading`), so its words must be
// the parse's words to the character: its whole words and the first block a node's line is
// projected from.
test("a plain text is read without the parse into the words the parse reads", () => {
  for (const text of PLAIN_TEXTS) {
    const label = JSON.stringify(text);
    expect(isPlainText(text), label).toBe(true);
    expect(readsAsParsed(text), label).toBe(true);
    const parsed = parsedReading(text);
    expect(wordsOfText(text), label).toBe(collapsed(wordsOf(parsed)));
    expect(firstBlockWords(storedReading(text)), label).toBe(firstBlockWords(parsed));
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
    expect(wordsOfText(text), text).toBe(words);
    expect(firstBlockWords(storedReading(text)), text).toBe(words);
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
// it is written as, with no link, and a bare address beside them as written too.
test("the parse reads a bare URL, a www address and an email as the words they are written as", () => {
  const text =
    "See https://example.com/a?b=c,\nwww.example.com  and someone@example.com\nbeside dispatch://ACME-12.";
  const reading = parsedReading(text);
  expect(JSON.stringify(reading)).not.toContain('"link"');
  expect(collapsed(wordsOf(reading))).toBe(collapsed(text));
});
