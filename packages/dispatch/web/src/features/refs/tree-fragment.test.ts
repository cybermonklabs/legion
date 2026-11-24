import { expect, test } from "bun:test";

import type { DisplayTree } from "../../api/types";
import { wordsOf } from "./tree-fragment";

/** One run of text, with the marks it is drawn in. */
function run(text: string, marks: readonly string[] = []): DisplayTree {
  return marks.length === 0
    ? { text, type: "text" }
    : { marks: marks.map((type) => ({ type })), text, type: "text" };
}

/** A node holding `content`. */
function holding(type: string, content: readonly DisplayTree[]): DisplayTree {
  return { content: [...content], type };
}

// The words of the server's reading of a stored text: what an author typed to format them - the
// `**` of a strong run, a list's bullets - is a mark or a node of the tree and no character of
// its words. Two runs that are neighbours in one parent are one stretch of words; anything else
// between two runs - a paragraph's end, a list item's, a hard break - is the one space the
// server writes there (`pmdoc.buildFlattenedText`).
test("the words of a reading hold no formatting symbol, and one space where two runs are apart", () => {
  const reading = holding("doc", [
    holding("paragraph", [run("Keep "), run("the tree", ["strong"]), run(" pure.")]),
    holding("bullet_list", [
      holding("list_item", [holding("paragraph", [run("one")])]),
      holding("list_item", [holding("paragraph", [run("two", ["em"])])]),
    ]),
    holding("paragraph", [run("a line"), { type: "hard_break" }, run("its next")]),
  ]);

  expect(wordsOf(reading)).toBe("Keep the tree pure. one two a line its next");
  expect(wordsOf(run("alone"))).toBe("alone");
  expect(wordsOf(holding("doc", []))).toBe("");
});
