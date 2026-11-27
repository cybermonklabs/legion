import { expect, test } from "bun:test";
import { DISPATCH_TEXT_REFERENCES, isIdPrefix, readerStep } from "@legion/contracts";

import { expectOnePass, ONE_PASS_SLACK, stepsAtNAndFourN } from "../../__tests__/one-pass";
import { type CodeRange, codeRanges } from "./code-ranges";
import { referenceHref } from "./routes";
import { readTextReferences } from "./text-references";

const origin = "https://dispatch.test";

/** The references a reading names, in order of first appearance, as a row's `refs` lists them. */
function cited(body: string, facts: Parameters<typeof readTextReferences>[1]): string[] {
  const seen = new Set<string>();
  for (const found of readTextReferences(body, facts).references) {
    if (found.reference !== undefined) {
      seen.add(found.reference);
    }
  }
  return [...seen];
}

/** The references a reading reads in a code context, in order of first appearance and each
 *  once, as a row's `inCode` lists them. A reference the text also writes outside code is none
 *  of them: that occurrence names its party, so the column names only what nothing outside code
 *  names (the server's `inCodeRefs`). */
function citedInCode(body: string, facts: Parameters<typeof readTextReferences>[1]): string[] {
  const inCode: string[] = [];
  const outside = new Set<string>();
  for (const found of readTextReferences(body, facts).references) {
    if (found.reference === undefined) {
      continue;
    }
    if (!found.inCode) {
      outside.add(found.reference);
    } else if (!inCode.includes(found.reference)) {
      inCode.push(found.reference);
    }
  }
  return inCode.filter((reference) => !outside.has(reference));
}

// The reader's half of the shared text table: every row, the ones a bare key, a `#id` or an id
// prefix writes included, read against what the row says exists. The Go reader `text.Mentions`
// walks the same rows, so neither where a reference ends nor what it names can change on one
// side alone.
test.each(DISPATCH_TEXT_REFERENCES.map((row) => [row.body, row.origin ?? origin, row] as const))(
  "%s cites, read against %s, its row's references",
  (body, server, row) => {
    expect(
      cited(body, {
        issues: row.issues,
        items: row.items,
        origin: server,
        questions: row.questions,
      })
    ).toEqual([...row.refs]);
  }
);

// The `inCode` column of the same table, row for row: which of a row's references a fenced
// block, an indented block or a code span holds. The Go reader is held to the very same column
// (`TestExtractMatchesTheSharedTextTable`), so where a code context begins and ends cannot
// change on one side alone - and that is what decides, on both, who a write is delivered to.
test.each(DISPATCH_TEXT_REFERENCES.map((row) => [row.body, row.origin ?? origin, row] as const))(
  "%s reads, against %s, its row's references in code",
  (body, server, row) => {
    expect(
      citedInCode(body, {
        issues: row.issues,
        items: row.items,
        origin: server,
        questions: row.questions,
      })
    ).toEqual([...(row.inCode ?? [])]);
  }
);

// A body that writes the same reference inside code and outside it marks each occurrence on its
// own, which is what keeps the party that body names a delivering one (`draftParties`).
test("each occurrence of a reference is marked where it stands", () => {
  const { references } = readTextReferences(
    "Run `dispatch://role/reviewer` and ask dispatch://role/reviewer about it.",
    { origin }
  );

  expect(references.map((found) => found.inCode)).toEqual([true, false]);
});

// What a caller that has asked the server nothing would have to ask: the distinct keys, whether
// a `#id` is there, and each reference whose id is written short. A hook starts exactly these.
test("a reading answers what it would have to ask before it has asked anything", () => {
  const { needs, references } = readTextReferences(
    "Both CORE-1 and ACME-12 are blocked; CORE-1 again, see #q2 and dispatch://CORE-1/ask/2a7f1c90.",
    { origin }
  );

  expect(needs).toEqual({
    keys: ["CORE-1", "ACME-12"],
    prefixes: ["dispatch://CORE-1/ask/2a7f1c90"],
    questions: true,
  });
  // Nothing is resolved yet, and every candidate says so.
  expect(references.map(({ form, text, unresolved }) => ({ form, text, unresolved }))).toEqual([
    { form: "key", text: "CORE-1", unresolved: true },
    { form: "key", text: "ACME-12", unresolved: true },
    { form: "key", text: "CORE-1", unresolved: true },
    { form: "question", text: "#q2", unresolved: true },
    { form: "address", text: "dispatch://CORE-1/ask/2a7f1c90", unresolved: true },
  ]);
});

test("a text with nothing to resolve asks nothing", () => {
  const { needs, references } = readTextReferences("Tied to dispatch://CORE-1 here.", { origin });

  expect(needs).toEqual({ keys: [], prefixes: [], questions: false });
  expect(references.map((found) => found.reference)).toEqual(["dispatch://CORE-1"]);
  expect(references[0].unresolved).toBe(false);
});

// An address is read as written and again without the trailing `_` and `-` a sentence or an
// emphasis leaves on it (`trimmedMention`), and that trim was once `/[_-]+$/`, which read a run
// of them again from every position in it: 6 s on an address whose query held 32,768
// underscores and a letter. One pass is a count (`reader-steps.ts`): the reading takes at most
// four times its steps, and a few more, on a run four times as long, the body each row reads
// from being that longer one. A count cannot see inside one call of an expression, so the old
// one is a control of `reader-expressions.test.ts`, which calls it vulnerable. Where the run
// trails the address, the author's own characters (`named`) end where the run starts.
test.each([
  [
    "underscores and a letter",
    (run: number) => `${origin}/issues/CORE-1?x=${"_".repeat(run)}y`,
    false,
  ],
  ["hyphens", (run: number) => `${origin}/issues/CORE-1?x=y${"-".repeat(run)}`, true],
] as const)("an address whose query holds a run of %s is read in one pass", (_name, body, trails) => {
  const run = 1 << 15;
  const { references } = expectOnePass((n) => readTextReferences(body(n), { origin }), run / 4);
  expect(references.map(({ named, reference }) => ({ named, reference }))).toEqual([
    { named: trails ? body(0).length : undefined, reference: "dispatch://CORE-1" },
  ]);
});

// The pills under a draft read it through this reader, each reference with the page it opens
// (`draft-pills.tsx`). A project's own page is no node a text can name, so a pasted URL of it
// cites nothing and gets no pill: of a project's pages, only its documents are references.
test("Composer ignores a pasted project page URL because only documents are references", () => {
  expect(cited(`${origin}/projects/CORE`, { origin })).toEqual([]);
});

// A project's document is a reference whether the draft pastes its page's URL or writes its
// address, and the page its pill opens is the document's own.
test("Composer preserves a project document reference with its document page href", () => {
  for (const body of [
    `${origin}/projects/CORE/documents/design-notes`,
    "dispatch://CORE/artifact/design-notes",
  ]) {
    const { references } = readTextReferences(body, { origin });
    expect(
      references.map(({ reference, route }) => ({
        href: route === undefined ? undefined : referenceHref(route),
        reference,
      })),
      body
    ).toEqual([
      {
        href: "/projects/CORE/documents/design-notes",
        reference: "dispatch://CORE/artifact/design-notes",
      },
    ]);
  }
});

// The id prefix pattern, which the server spells `text.IsIDPrefix`: a hex digit and seven or
// more hex digits and hyphens, read without case, and never a whole uuid.
test.each([
  ["2a7f1c90", true],
  ["2A7F1C90", true],
  ["2a7f1c90-1f4e", true],
  ["2a7f1c9", false],
  ["xa7f1c90", false],
  ["2a7f1c9g", false],
  ["2a7f1c90-1f4e-4a2b-9c3d-5e6f70819a2b", false],
  ["2a7f1c901f4e4a2b9c3d5e6f70819a2b", false],
] as const)("%s is an id prefix: %p", (value, expected) => {
  expect(isIdPrefix(value)).toBe(expected);
});

// A body near the server's 1 MiB request cap that trails a reference with a run of closing
// parentheses, alone or between the emphasis delimiters the trim also drops, still cites that
// reference, and a long run of closers followed by anything else is read in one pass too. A trim
// that rescans the run for every start, as an end-anchored regular expression does, took over
// ten seconds on the 64 KiB body alone. One pass is what a count shows, not a clock: the
// reader's loops (`reader-steps.ts`) take at most four times their steps, and a few more, on a
// run four times as long, the body each row cites from being that longer one. The expressions
// it runs over the run are checked as they are written (`reader-expressions.test.ts`).
test.each([
  [
    "parentheses",
    (run: number) => `dispatch://CORE-1${")".repeat(run)}`,
    1 << 20,
    ["dispatch://CORE-1"],
  ],
  [
    "parentheses and underscores",
    (run: number) => `dispatch://CORE-1${")_".repeat(run)}`,
    1 << 19,
    ["dispatch://CORE-1"],
  ],
  [
    "parentheses and asterisks after a dashboard URL",
    (run: number) => `${origin}/issues/CORE-1${")*".repeat(run)}`,
    1 << 19,
    ["dispatch://CORE-1"],
  ],
  [
    "parentheses then a letter",
    (run: number) => `dispatch://CORE-1${")".repeat(run)}x`,
    1 << 16,
    [],
  ],
] as const)("a reference trailed by %s is read in one pass", (_name, body, run, refs) => {
  expect(expectOnePass((n) => cited(body(n), { origin }), run / 4)).toEqual([...refs]);
});

/** The trim those rows guard against: for every end it tries, it counts the parentheses before
 *  that end again, which is what an end-anchored expression costs on a run of closers. */
function trimRescanningTheRun(value: string): string {
  let end = value.length;
  for (; end > 0; end -= 1) {
    let opened = 0;
    let closed = 0;
    for (let index = 0; index < end; index += 1) {
      readerStep();
      const char = value.charAt(index);
      if (char === "(") opened += 1;
      else if (char === ")") closed += 1;
    }
    const char = value.charAt(end - 1);
    if (char === ")" ? opened >= closed : !".,;:!?*_~".includes(char)) {
      break;
    }
  }
  return value.slice(0, end);
}

// The control of the rows above: the same bound fails the trim that rescans the run, so a reader
// that went back to it would fail them.
test("the one-pass bound fails a trim that rescans a closing run for every end it tries", () => {
  const body = (run: number) => `dispatch://CORE-1${")".repeat(run)}`;
  const { atN, atFourN } = stepsAtNAndFourN((n) => trimRescanningTheRun(body(n)), 1 << 8);
  expect(trimRescanningTheRun(body(1 << 8))).toBe("dispatch://CORE-1");
  expect(atFourN).toBeGreaterThan(4 * atN + ONE_PASS_SLACK);
});

// Every reading of a text reads its code contexts first (`codeRanges`), and this holds that
// reading to one pass on one shape, by the same count: on a text of four times the units - a line
// of prose holding a code span, then a fenced block - it takes at most four times its steps, and
// a few more. A reading that went back over the text before each block it found would take the
// square.
test("a text of lines holding a code span, each followed by a fenced block, has its code read in one pass", () => {
  const unit = "`CORE-1` and\n```\ncode\n```\n";
  const quarter = 1 << 10;
  const ranges = expectOnePass((n) => codeRanges(unit.repeat(n)), quarter);
  const text = unit.repeat(4 * quarter);
  expect(ranges.map((range) => [text.slice(range.start, range.end), range.block])).toEqual(
    Array.from({ length: 4 * quarter }, () => [
      ["`CORE-1`", false],
      ["```\ncode\n```\n", true],
    ]).flat()
  );
});

// A text of lines holding a code span, each parted from the fenced block after it by a blank
// line: the paragraph breaks are walked once for all the stretches the blocks leave, each going
// on from where the last one stopped (`appendCodeSpans`), where a search of them by halves once
// a block added a factor of about log n.
test("a text of code spans and fenced blocks parted by blank lines has its code read in one pass", () => {
  const unit = "`a` b\n\n```\nc\n```\n";
  const quarter = 1 << 10;
  const ranges = expectOnePass((n) => codeRanges(unit.repeat(n)), quarter);
  const text = unit.repeat(4 * quarter);
  expect(ranges.map((range) => [text.slice(range.start, range.end), range.block])).toEqual(
    Array.from({ length: 4 * quarter }, () => [
      ["`a`", false],
      ["```\nc\n```\n", true],
    ]).flat()
  );
});

/** The reading the row above guards against: `codeRanges`' own, and then, once a block, a search
 *  of the ranges by halves for where it stands, as the paragraph breaks were once searched for
 *  the first one past each block. */
function codeSearchedByHalvesOnceABlock(text: string): CodeRange[] {
  const ranges = codeRanges(text);
  for (const block of ranges) {
    readerStep();
    if (!block.block) continue;
    let low = 0;
    let high = ranges.length;
    while (low < high) {
      readerStep();
      const middle = (low + high) >> 1;
      if (ranges[middle].start <= block.start) low = middle + 1;
      else high = middle;
    }
  }
  return ranges;
}

// The control of the row above: the same bound fails a reading that searches by halves once a
// block, on the same text.
test("the one-pass bound fails a code reading that searches by halves once a block", () => {
  const unit = "`a` b\n\n```\nc\n```\n";
  const { atN, atFourN } = stepsAtNAndFourN(
    (n) => codeSearchedByHalvesOnceABlock(unit.repeat(n)),
    1 << 10
  );
  expect(atFourN).toBeGreaterThan(4 * atN + ONE_PASS_SLACK);
});

// A text of code spans each followed by a reference, one written in the span and one after it:
// the references of each list - the reference-shaped spans, then the bare keys and `#id`s - are
// read in order, so each list walks the code ranges once (`codeCursor`), where a search of them
// by halves once a reference added a factor of about log n.
test("a text of code spans each followed by a reference has every reference marked in one pass", () => {
  const unit = "`CORE-1` CORE-2 ";
  const quarter = 1 << 10;
  const { references } = expectOnePass(
    (n) => readTextReferences(unit.repeat(n), { origin }),
    quarter
  );
  expect(references.map(({ inCode, text }) => [text, inCode])).toEqual(
    Array.from({ length: 4 * quarter }, () => [
      ["CORE-1", true],
      ["CORE-2", false],
    ]).flat()
  );
});

/** The marking the row above guards against: `readTextReferences`' own reading, and then, once a
 *  reference, a search of the text's code ranges by halves for its first character. */
function inCodeSearchedByHalvesOnceAReference(text: string): boolean[] {
  const ranges = codeRanges(text);
  return readTextReferences(text, { origin }).references.map(({ start }) => {
    readerStep();
    let low = 0;
    let high = ranges.length - 1;
    while (low <= high) {
      readerStep();
      const middle = (low + high) >> 1;
      const range = ranges[middle];
      if (range.end <= start) low = middle + 1;
      else if (range.start > start) high = middle - 1;
      else return true;
    }
    return false;
  });
}

// The control of the row above: the same bound fails a marking that searches by halves once a
// reference, on the same text.
test("the one-pass bound fails a marking that searches the code by halves once a reference", () => {
  const unit = "`CORE-1` CORE-2 ";
  const { atN, atFourN } = stepsAtNAndFourN(
    (n) => inCodeSearchedByHalvesOnceAReference(unit.repeat(n)),
    1 << 10
  );
  expect(atFourN).toBeGreaterThan(4 * atN + ONE_PASS_SLACK);
});

/** One paragraph of `n` characters: runs of 1, 2, 3 and more backticks, each after a space, as
 *  many as fit, then spaces to the end. No two runs are as wide, so none closes another, and a
 *  text four times as long holds about twice as many runs. */
function unclosedRuns(n: number): string {
  let text = "";
  for (let width = 1; text.length + 1 + width <= n; width += 1) {
    text += ` ${"`".repeat(width)}`;
  }
  return text.padEnd(n, " ");
}

// One paragraph of runs of backticks of distinct widths, none closed: each run's closer is looked
// for among the text's runs of its own width, read once (`closingRun`), where a search of the
// rest of the paragraph from each opener took about 8 times the steps on 4 times the text.
test("a paragraph of unclosed backtick runs of distinct widths has its code read in one pass", () => {
  expect(expectOnePass((n) => codeRanges(unclosedRuns(n)), 1 << 12)).toEqual([]);
});

/** Where the run of backticks starting at `at` ends. */
function backticksEnd(text: string, at: number): number {
  let end = at;
  while (text.charAt(end) === "`") {
    readerStep();
    end += 1;
  }
  return end;
}

/** The spans the row above reads, read the way it guards against: from each opener, the rest of
 *  the paragraph is searched again for the next run of the opener's width. */
function spansRescanningTheParagraph(text: string): [number, number][] {
  const spans: [number, number][] = [];
  for (let at = 0; at < text.length; ) {
    readerStep();
    if (text.charAt(at) !== "`") {
      at += 1;
      continue;
    }
    const opener = backticksEnd(text, at);
    let closer = -1;
    for (let next = opener; closer < 0 && next < text.length; ) {
      readerStep();
      if (text.charAt(next) !== "`") {
        next += 1;
        continue;
      }
      const end = backticksEnd(text, next);
      if (end - next === opener - at) closer = next;
      next = end;
    }
    if (closer < 0) {
      at = opener;
      continue;
    }
    spans.push([at, closer + opener - at]);
    at = closer + opener - at;
  }
  return spans;
}

// The control of the row above: the same bound fails a span reading that searches the rest of the
// paragraph again from each opener, on the same text.
test("the one-pass bound fails a span reading that rescans the paragraph from each opener", () => {
  const { atN, atFourN } = stepsAtNAndFourN(
    (n) => spansRescanningTheParagraph(unclosedRuns(n)),
    1 << 12
  );
  expect(spansRescanningTheParagraph(unclosedRuns(1 << 12))).toEqual([]);
  expect(atFourN).toBeGreaterThan(4 * atN + ONE_PASS_SLACK);
});
