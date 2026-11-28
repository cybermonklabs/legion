import { expect, test } from "bun:test";

import type { DisplayTree } from "../../api/types";
import { nameOf } from "../issue-page/projection-fixture";
import { warmMarkdownRenderer } from "./markdown-engine";
import { firstWordedBlock, wordsOf } from "./tree-fragment";

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

test("a bulleted list reads its first item alone", () => {
  const first = holding("list_item", [holding("paragraph", [run("Keep the old page")])]);
  const second = holding("list_item", [holding("paragraph", [run("Then drop it")])]);
  const reading = holding("doc", [holding("bullet_list", [first, second])]);

  expect(firstWordedBlock(reading)).toEqual(holding("doc", [holding("bullet_list", [first])]));
});

test("an item with a nested list reads its own first paragraph", () => {
  const first = holding("paragraph", [run("Outer words")]);
  const inner = holding("bullet_list", [
    holding("list_item", [holding("paragraph", [run("Inner words")])]),
  ]);
  const item = holding("list_item", [first, inner]);
  const reading = holding("doc", [holding("bullet_list", [item])]);
  const kept = holding("doc", [holding("bullet_list", [holding("list_item", [first])])]);

  expect(firstWordedBlock(reading)).toEqual(kept);
});

test("a block quote of two paragraphs reads the first", () => {
  const first = holding("paragraph", [run("First paragraph")]);
  const second = holding("paragraph", [run("Second paragraph")]);
  const reading = holding("doc", [holding("blockquote", [first, second])]);

  expect(firstWordedBlock(reading)).toEqual(holding("doc", [holding("blockquote", [first])]));
});

test("an ordered list keeps the number of the first item with words", () => {
  const empty = holding("list_item", [holding("paragraph", [])]);
  const item = holding("list_item", [holding("paragraph", [run("Keep this step")])]);
  const list = { ...holding("ordered_list", [empty, item]), attrs: { order: 4 } };
  const kept = { ...holding("ordered_list", [item]), attrs: { order: 5 } };

  expect(firstWordedBlock(holding("doc", [list]))).toEqual(holding("doc", [kept]));
});

test("a reading with no words has no first worded block", () => {
  const reading = holding("doc", [
    { type: "horizontal_rule" },
    holding("blockquote", [holding("paragraph", [run(" ")])]),
  ]);

  expect(firstWordedBlock(reading)).toBeUndefined();
});

test("a chip naming a block that opens with a list reads the first item alone", async () => {
  await warmMarkdownRenderer();
  const tree = holding("doc", [
    holding("bullet_list", [
      holding("list_item", [holding("paragraph", [run("Keep the old page")])]),
      holding("list_item", [holding("paragraph", [run("Then drop it")])]),
    ]),
  ]);

  expect(
    nameOf({
      answered: true,
      gone: false,
      reading: { tree },
      ref: "dispatch://CORE-1/artifact/plan-md#b-t1",
    })
  ).toBe("Keep the old page");
});
