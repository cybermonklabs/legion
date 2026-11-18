import { expect, test } from "bun:test";

import { lineWordsRaw } from "./issue-tree";
import { plainWordsRaw } from "./tree-drafts";

/** A wall-clock budget for one stored text read to words, not one of the machine-sensitive waits
 *  this package's defaults cover (`web/src/__tests__/setup.ts`). The regression it guards, a
 *  parse with no bound on how deeply lists nest, spends about 9 s on the body below, and the
 *  bounded parse well under one, so a budget far above what a loaded pod spends on the bounded
 *  parse fails only an unbounded one. */
const NESTING_BUDGET_MS = 3_000;

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
