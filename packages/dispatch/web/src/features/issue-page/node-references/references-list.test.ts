import { expect, test } from "bun:test";

import { graphNode } from "../../../__tests__/graph-fixture";
import type { DisplayTree, GraphEdge } from "../../../api/types";
import { issueItemWords, referenceRows } from "./references-list";

const AT = "2026-11-15T10:00:00Z";

/** No item of the issue has words the page holds. */
const NO_WORDS: ReadonlyMap<string, string> = new Map();

/** One run of words of a reading, with the marks the server read around it. */
function run(text: string, ...marks: DisplayTree[]): DisplayTree {
  return marks.length === 0 ? { text, type: "text" } : { marks, text, type: "text" };
}

/** The server's reading of a stored text, one paragraph per element of `paragraphs`. */
function reading(...paragraphs: (string | readonly DisplayTree[])[]): DisplayTree {
  return {
    content: paragraphs.map((runs) => ({
      content: typeof runs === "string" ? [run(runs)] : runs,
      type: "paragraph",
    })),
    type: "doc",
  };
}

/** The words of the issue's comments, by id, from the readings its comments read sends. */
function commentWords(
  comments: Readonly<Record<string, DisplayTree>>
): ReadonlyMap<string, string> {
  return issueItemWords({
    asks: undefined,
    comments: Object.entries(comments).map(([id, tree]) => ({ body_tree: tree, id })),
    messages: undefined,
  });
}

function edge(fields: {
  at?: string;
  excerpt?: string;
  id: string;
  issueKey?: string;
  kind: GraphEdge["kind"];
  nodeKind: GraphEdge["node"]["kind"];
  parent?: NonNullable<GraphEdge["node"]["parent"]>;
  project?: string;
  ref?: string;
  text?: string;
}): GraphEdge {
  return {
    created_at: fields.at ?? AT,
    direction: "in",
    ...(fields.excerpt === undefined ? {} : { excerpt: { text: fields.excerpt } }),
    kind: fields.kind,
    node: graphNode({
      id: fields.id,
      ...(fields.issueKey === undefined ? {} : { issue_key: fields.issueKey }),
      kind: fields.nodeKind,
      ...(fields.parent === undefined ? {} : { parent: fields.parent }),
      ...(fields.project === undefined ? {} : { project: fields.project }),
      ...(fields.ref === undefined ? {} : { ref: fields.ref }),
      ...(fields.text === undefined ? {} : { text: fields.text }),
    }),
    source_seq: null,
  };
}

test("one row per citing node, in the order the read answers them", () => {
  const words = commentWords({ "c-1": reading("Thread 1 opens here.") });
  const rows = referenceRows(words, [
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
  expect(rows.map((row) => row.line)).toEqual(["Thread 1 opens here.", ""]);
  expect(rows.map((row) => row.ref)).toEqual([
    "dispatch://LAY-2/comment/c-1",
    "dispatch://LAY-2/artifact/verification-md#b-c1",
  ]);
});

test("a node that cites the row twice is one row", () => {
  const words = commentWords({ "c-1": reading("It names it here.", "And again here.") });
  const rows = referenceRows(words, [
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
  const rows = referenceRows(NO_WORDS, [
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

// A node that is no comment, message or question of this issue - another issue, a block of a
// document, a comment of another issue - is named by its word: the read's excerpt and the
// node's own text are its stored text, and no row draws them.
test("a node outside this issue's items carries no line of the read's text", () => {
  const words = commentWords({ "c-1": reading("Thread 1 opens here.") });
  const rows = referenceRows(words, [
    edge({
      id: "LAY-9",
      kind: "child_of",
      nodeKind: "issue",
      ref: "dispatch://LAY-9",
      text: "The rail becomes an icon rail.",
    }),
    edge({
      excerpt: "Check 1 of the redesign.",
      id: "v-1",
      kind: "serves",
      nodeKind: "block",
      ref: "dispatch://LAY-2/artifact/verification-md#b-c1",
      text: "Check 1 of the redesign.",
    }),
    edge({
      excerpt: "It cites the statement from another issue.",
      id: "c-9",
      issueKey: "LAY-9",
      kind: "mentions",
      nodeKind: "comment",
      ref: "dispatch://LAY-9/comment/c-9",
      text: "It cites the statement from another issue.",
    }),
  ]);
  expect(rows.map((row) => [row.key, row.line])).toEqual([
    ["issue:LAY-9", ""],
    ["block:v-1", ""],
    ["comment:c-9", ""],
  ]);
});

// The read's excerpt is the citing node's stored text: the markdown its author typed, with the
// addresses it cites. A comment of this issue is named as its row in the outline names it, by
// the words of the server's reading of it.
test("a citing item of this issue is named by the words of its tree, never its stored source", () => {
  const stored = "**Bob** checks the [cited statement](dispatch://CORE-2/spec#b-r1)";
  const words = commentWords({
    "c-1": reading([
      run("Bob", { type: "strong" }),
      run(" checks the "),
      run("cited statement", { attrs: { href: "dispatch://CORE-2/spec#b-r1" }, type: "link" }),
    ]),
  });
  const rows = referenceRows(words, [
    edge({
      excerpt: stored,
      id: "c-1",
      kind: "mentions",
      nodeKind: "comment",
      ref: "dispatch://LAY-2/comment/c-1",
      text: stored,
    }),
  ]);
  const line = rows[0]?.line ?? "";
  expect(line).toBe("Bob checks the cited statement");
  expect(line).not.toContain("**");
  expect(line).not.toContain("dispatch://");
});

test("a line is cut where every one line of a node is cut", () => {
  const words = commentWords({
    "c-1": reading("It opens on one sentence. The rest of the block follows it and is not drawn."),
  });
  const rows = referenceRows(words, [
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
  const rows = referenceRows(NO_WORDS, [
    edge({ id: "s-1", kind: "mentions", nodeKind: "session" }),
  ]);
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
// item: one row per item, named by the item's kind and its words, and opened at its own address.
test("a block of a comment, a message or a question that mentions the row is that item, once", () => {
  const comment = { id: "c-1", kind: "comment", ref: "dispatch://LAY-2/comment/c-1" };
  const words = issueItemWords({
    asks: [{ id: "a-1", question_tree: reading("Which statement is this about?") }],
    comments: [{ body_tree: reading("It names the row here.", "And here again."), id: "c-1" }],
    messages: [
      {
        message: { body_tree: reading("The thread opens."), id: "m-1" },
        replies: [{ body_tree: reading("Its reply names the row."), id: "m-2" }],
      },
    ],
  });
  const rows = referenceRows(words, [
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
    edge({
      excerpt: "Its reply names the row.",
      id: "m-2#b1",
      kind: "mentions",
      nodeKind: "block",
      parent: { id: "m-2", kind: "message", ref: "dispatch://LAY-2/message/m-2" },
    }),
  ]);
  expect(rows.map((row) => [row.key, row.nodeKind, row.ref, row.line])).toEqual([
    ["comment:c-1", "comment", "dispatch://LAY-2/comment/c-1", "It names the row here."],
    ["ask:a-1", "ask", "dispatch://LAY-2/ask/a-1", "Which statement is this about?"],
    ["message:m-2", "message", "dispatch://LAY-2/message/m-2", "Its reply names the row."],
  ]);
});

// A block of a document keeps its own kind and its own address: its parent is the document.
test("a block of a document is not read as an item", () => {
  const rows = referenceRows(NO_WORDS, [
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

// Each source in the old panel carried the citing node's issue or project, and each edge
// its time; a row carries both, and keeps the time of its first edge.
test("a row carries where the citing node is from and when it referenced the row", () => {
  const rows = referenceRows(NO_WORDS, [
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
