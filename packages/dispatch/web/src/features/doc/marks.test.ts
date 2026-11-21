import { expect, test } from "bun:test";
import { Schema } from "prosemirror-model";

import { blockPlacements, composerKindFor, insertMarksIn, markPlacements } from "./marks";

const schema = new Schema({
  marks: {
    dispatchAsk: { attrs: { by: {}, id: {} } },
    proofComment: { attrs: { by: {}, id: {} } },
    proofSuggestion: { attrs: { by: {}, id: {}, kind: {} }, excludes: "" },
  },
  nodes: {
    doc: { content: "paragraph+" },
    paragraph: { attrs: { blockId: { default: null } }, content: "inline*", group: "block" },
    text: { group: "inline" },
  },
});

test("markPlacements reports the first position of every record-bearing mark", () => {
  const comment = schema.marks.proofComment.create({ by: "alice", id: "c-1" });
  const ask = schema.marks.dispatchAsk.create({ by: "bob", id: "a-1" });
  const doc = schema.node("doc", undefined, [
    schema.node("paragraph", undefined, [
      schema.text("The "),
      schema.text("quick", [comment]),
      schema.text(" brown "),
      schema.text("fox", [ask]),
      schema.text(" and "),
      schema.text("quick", [comment]),
    ]),
  ]);

  expect([
    ...markPlacements(
      doc,
      new Map([
        ["c-1", 40],
        ["a-1", 72],
      ])
    ),
  ]).toEqual([
    ["c-1", { pos: 5, top: 40 }],
    ["a-1", { pos: 17, top: 72 }],
  ]);
});

test("markPlacements reports the first position and matching vertical offset of every mark", () => {
  const comment = schema.marks.proofComment.create({ by: "alice", id: "c-1" });
  const ask = schema.marks.dispatchAsk.create({ by: "bob", id: "a-1" });
  const doc = schema.node("doc", undefined, [
    schema.node("paragraph", undefined, [
      schema.text("The "),
      schema.text("quick", [comment]),
      schema.text(" brown "),
      schema.text("fox", [ask]),
    ]),
  ]);

  expect([
    ...markPlacements(
      doc,
      new Map([
        ["c-1", 40],
        ["a-1", 72],
      ])
    ),
  ]).toEqual([
    ["c-1", { pos: 5, top: 40 }],
    ["a-1", { pos: 17, top: 72 }],
  ]);
});

test("blockPlacements reports each stable block's position and offset", () => {
  const doc = schema.node("doc", undefined, [
    schema.node("paragraph", { blockId: "block-1" }, schema.text("First")),
    schema.node("paragraph", { blockId: "block-2" }, schema.text("Second")),
  ]);

  expect([
    ...blockPlacements(
      doc,
      new Map([
        ["block-1", 40],
        ["block-2", 72],
      ])
    ),
  ]).toEqual([
    ["block-1", { pos: 0, top: 40 }],
    ["block-2", { pos: 7, top: 72 }],
  ]);
});

// The words a selection names lie in a pending insert wherever any of them carries the insert's
// mark: inside it, across its edge, or across two blocks with an insert in each. A replace
// suggestion's words are the document's own, and words beside an insert are none of its: the
// list is empty for them, which says they lie in no insert (`ComposerAnchor.insertMarks`).
test("insertMarksIn names each pending insert any selected word lies in, once, in document order, and an empty list for words in no insert", () => {
  const insert = (id: string) =>
    schema.marks.proofSuggestion.create({ by: "bob", id, kind: "insert" });
  const replace = schema.marks.proofSuggestion.create({ by: "bob", id: "r-1", kind: "replace" });
  const doc = schema.node("doc", undefined, [
    schema.node("paragraph", undefined, [
      schema.text("Hold it "),
      schema.text("for the security review", [insert("i-1")]),
      schema.text("."),
    ]),
    schema.node("paragraph", undefined, [
      schema.text("Ship ", [replace]),
      schema.text("it", [insert("i-2")]),
      schema.text(" soon", [insert("i-1")]),
    ]),
  ]);
  // "Hold it " is 1-9, the first insert 9-32 and "." 32-33; in the second paragraph, "Ship " is
  // 35-40, "it" 40-42 and " soon", the first insert's again, 42-47.
  expect({
    across: insertMarksIn(doc, 6, 47),
    beside: insertMarksIn(doc, 1, 9),
    edge: insertMarksIn(doc, 6, 12),
    inside: insertMarksIn(doc, 17, 32),
    replaced: insertMarksIn(doc, 36, 40),
  }).toEqual({
    across: ["i-1", "i-2"],
    beside: [],
    edge: ["i-1"],
    inside: ["i-1"],
    replaced: [],
  });
});

test("composerKindFor maps every selection-bar composer action", () => {
  expect(composerKindFor("comment")).toBe("comment");
  expect(composerKindFor("suggest")).toBe("suggestion");
  expect(composerKindFor("ask")).toBe("ask");
});
