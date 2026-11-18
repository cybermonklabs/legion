import { expect, test } from "bun:test";

import { lineWordsRaw } from "./issue-tree";
import { plainWordsRaw } from "./tree-drafts";

/** A wall-clock budget for one stored text read to words, not one of the machine-sensitive waits
 *  this package's defaults cover (`web/src/__tests__/setup.ts`). The regression it guards, a
 *  parse with no bound on how deeply lists nest, spends about 9 s on the body below, and the
 *  bounded parse well under one, so a budget far above what a loaded pod spends on the bounded
 *  parse fails only an unbounded one. */
const NESTING_BUDGET_MS = 3_000;

/** A wall-clock budget for drawing again the lines of an issue's stored texts, each read once
 *  already. A result has no reading, so its line is read from its stored text, and an issue
 *  under review holds hundreds of results: the redesign's own root issue held 431 results and
 *  492 distinct stored texts on 2026-11-18. Reading 900 such texts again from what the first
 *  draw read costs tens of milliseconds; parsing each again, as a first draw does, costs about
 *  a second on a pod's CPU. So a budget far above the first and far below the second fails only
 *  a draw that parses every text again. */
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
// from its stored text where the node has no reading, a result's always. The readings of the texts
// a draw read are remembered so that the next draw reads them rather than parses them again: an
// issue with more distinct stored texts than the memo holds must still draw again within the
// budget, the second draw in the same order as the first, as a page draws its rows.
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
