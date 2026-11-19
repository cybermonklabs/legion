import { expect, test } from "bun:test";

import { graphNodeFields } from "../../../__tests__/graph-fixture";
import type { GraphEdge } from "../../../api/types";
import { nodeWordsOf, referenceRows } from "./references-list";

const AT = "2026-11-15T10:00:00Z";

function edge(fields: {
  at?: string;
  excerpt?: string;
  id: string;
  issueKey?: string;
  kind: string;
  nodeKind: string;
  parent?: { id: string; kind: string; ref?: string };
  project?: string;
  ref?: string;
  text?: string;
}): GraphEdge {
  return {
    created_at: fields.at ?? AT,
    direction: "in",
    ...(fields.excerpt === undefined ? {} : { excerpt: { text: fields.excerpt } }),
    kind: fields.kind,
    node: {
      ...graphNodeFields(),
      id: fields.id,
      ...(fields.issueKey === undefined ? {} : { issue_key: fields.issueKey }),
      kind: fields.nodeKind,
      ...(fields.parent === undefined ? {} : { parent: fields.parent }),
      ...(fields.project === undefined ? {} : { project: fields.project }),
      ...(fields.ref === undefined ? {} : { ref: fields.ref }),
      ...(fields.text === undefined ? {} : { text: fields.text }),
    },
    source_seq: null,
  };
}

test("one row per citing node, in the order the read answers them", () => {
  const rows = referenceRows([
    edge({
      excerpt: "Thread 1 opens here.",
      id: "c-1",
      kind: "mentions",
      nodeKind: "comment",
      ref: "dispatch://LAY-2/comment/c-1",
    }),
    edge({
      excerpt: "Check 1 of the redesign.",
      id: "v-1",
      kind: "serves",
      nodeKind: "block",
      ref: "dispatch://LAY-2/artifact/verification-md#b-c1",
    }),
  ]);
  expect(rows.map((row) => row.key)).toEqual(["comment:c-1", "block:v-1"]);
  expect(rows.map((row) => row.line)).toEqual(["Thread 1 opens here.", "Check 1 of the redesign."]);
  expect(rows.map((row) => row.ref)).toEqual([
    "dispatch://LAY-2/comment/c-1",
    "dispatch://LAY-2/artifact/verification-md#b-c1",
  ]);
});

test("a node that cites the row twice is one row", () => {
  const rows = referenceRows([
    edge({
      excerpt: "It names it here.",
      id: "c-1",
      kind: "mentions",
      nodeKind: "comment",
      ref: "dispatch://LAY-2/comment/c-1",
    }),
    edge({
      excerpt: "And again here.",
      id: "c-1",
      kind: "mentions",
      nodeKind: "comment",
      ref: "dispatch://LAY-2/comment/c-1",
    }),
  ]);
  expect(rows).toHaveLength(1);
  expect(rows[0]?.line).toBe("It names it here.");
});

test("a tuple of the model names the row over a mention, and keeps the mention's place", () => {
  const rows = referenceRows([
    edge({
      excerpt: "Task 8 of the redesign.",
      id: "p-8",
      kind: "mentions",
      nodeKind: "block",
      ref: "dispatch://LAY-2/artifact/plan-md#b-t8",
    }),
    edge({
      excerpt: "Thread 1 opens here.",
      id: "c-1",
      kind: "mentions",
      nodeKind: "comment",
      ref: "dispatch://LAY-2/comment/c-1",
    }),
    edge({
      excerpt: "Task 8 of the redesign.",
      id: "p-8",
      kind: "needs",
      nodeKind: "block",
      ref: "dispatch://LAY-2/artifact/plan-md#b-t8",
    }),
  ]);
  expect(rows.map((row) => `${row.key} ${row.kind}`)).toEqual([
    "block:p-8 needs",
    "comment:c-1 mentions",
  ]);
});

test("a typed block's words leave out its directive line and its closing fence", () => {
  const excerpt =
    ':::check{#c1 serves="dispatch://LAY-2/spec#b-r1" when="before_merge"}\n' +
    "Check 1 of the redesign holds its own row and its own state.\n" +
    ":::";
  expect(nodeWordsOf(edge({ excerpt, id: "v-1", kind: "serves", nodeKind: "block" }))).toBe(
    "Check 1 of the redesign holds its own row and its own state."
  );
});

test("a node's own text names it where the read sends no excerpt", () => {
  const rows = referenceRows([
    edge({
      id: "LAY-9",
      kind: "child_of",
      nodeKind: "issue",
      ref: "dispatch://LAY-9",
      text: "The rail becomes an icon rail.",
    }),
  ]);
  expect(rows[0]?.line).toBe("The rail becomes an icon rail.");
});

test("a line is cut where every one line of a node is cut", () => {
  const rows = referenceRows([
    edge({
      excerpt: "It opens on one sentence. The rest of the block follows it and is not drawn.",
      id: "c-1",
      kind: "mentions",
      nodeKind: "comment",
      ref: "dispatch://LAY-2/comment/c-1",
    }),
  ]);
  expect(rows[0]?.line).toBe("It opens on one sentence.");
});

test("a node the read carries no words and no address of is still one row", () => {
  const rows = referenceRows([edge({ id: "s-1", kind: "mentions", nodeKind: "session" })]);
  expect(rows).toEqual([
    {
      at: AT,
      key: "session:s-1",
      kind: "mentions",
      line: "",
      nodeKind: "session",
      ref: undefined,
      source: undefined,
    },
  ]);
});

// The graph resolves a mention inside a comment's, a message's or a question's words to that
// item's block, with the item as its parent and no document (`refs/item_block_nodes.go`), and
// hides the item's own mention whenever one of its blocks holds the link. Such a block is its
// item: one row per item, named by the item's kind and opened at the item's own address.
test("a block of a comment, a message or a question that mentions the row is that item, once", () => {
  const comment = { id: "c-1", kind: "comment", ref: "dispatch://LAY-2/comment/c-1" };
  const rows = referenceRows([
    edge({
      excerpt: "It names the row here.",
      id: "c-1#b1",
      kind: "mentions",
      nodeKind: "block",
      parent: comment,
      ref: "dispatch://LAY-2/comment/c-1#b1",
    }),
    edge({
      excerpt: "And here again.",
      id: "c-1#b2",
      kind: "mentions",
      nodeKind: "block",
      parent: comment,
      ref: "dispatch://LAY-2/comment/c-1#b2",
    }),
    edge({
      excerpt: "Which statement is this about?",
      id: "a-1#b1",
      kind: "mentions",
      nodeKind: "block",
      parent: { id: "a-1", kind: "ask", ref: "dispatch://LAY-2/ask/a-1" },
    }),
  ]);
  expect(rows.map((row) => [row.key, row.nodeKind, row.ref, row.line])).toEqual([
    ["comment:c-1", "comment", "dispatch://LAY-2/comment/c-1", "It names the row here."],
    ["ask:a-1", "ask", "dispatch://LAY-2/ask/a-1", "Which statement is this about?"],
  ]);
});

// A block of a document keeps its own kind and its own address: its parent is the document.
test("a block of a document is not read as an item", () => {
  const rows = referenceRows([
    {
      ...edge({
        id: "v-1",
        kind: "serves",
        nodeKind: "block",
        ref: "dispatch://LAY-2/artifact/v#b-c1",
      }),
      node: {
        ...edge({ id: "v-1", kind: "serves", nodeKind: "block" }).node,
        document: "dispatch://LAY-2/artifact/v",
        parent: { id: "artifact-v", kind: "artifact" },
        ref: "dispatch://LAY-2/artifact/v#b-c1",
      },
    },
  ]);
  expect(rows.map((row) => [row.key, row.nodeKind, row.ref])).toEqual([
    ["block:v-1", "block", "dispatch://LAY-2/artifact/v#b-c1"],
  ]);
});

// R19: each source in the old panel carried the citing node's issue or project, and each edge
// its time; a row carries both, and keeps the time of its first edge.
test("a row carries where the citing node is from and when it referenced the row", () => {
  const rows = referenceRows([
    edge({
      at: "2026-11-14T09:00:00Z",
      id: "c-9",
      issueKey: "LAY-9",
      kind: "mentions",
      nodeKind: "comment",
    }),
    edge({ id: "a-1", kind: "mentions", nodeKind: "artifact", project: "LAY" }),
  ]);
  expect(rows.map((row) => [row.source, row.at])).toEqual([
    ["LAY-9", "2026-11-14T09:00:00Z"],
    ["LAY", AT],
  ]);
});
