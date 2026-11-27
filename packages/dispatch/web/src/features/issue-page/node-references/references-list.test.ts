import { expect, test } from "bun:test";

import { graphNode } from "../../../__tests__/graph-fixture";
import type { DisplayTree, GraphEdge, GraphReferences } from "../../../api/types";
import { buildDispatchReference } from "../../refs/routes";
import { NODE_REFERENCES_WORDS } from "../page-words";
import { issueItemWords, referenceRows } from "./references-list";

const AT = "2026-11-15T10:00:00Z";

/** No item of the issue has words the page holds. */
const NO_WORDS: ReadonlyMap<string, string> = new Map();

/** No known reference names or edited marks: passed explicitly by cases that need neither. */
const NO_KNOWN = { edited: new Set<string>(), names: new Map<string, string>() };

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

/** One edge of the references read, its excerpt with the server's reading of it where the read
 *  sent one (`text_tree`). */
type ReadEdge = NonNullable<GraphReferences["edges"]>[number];

function edge(fields: {
  at?: string;
  excerpt?: string;
  id: string;
  issueKey?: string;
  kind: GraphEdge["kind"];
  nodeKind: GraphEdge["node"]["kind"];
  parent?: NonNullable<GraphEdge["node"]["parent"]>;
  project?: string;
  /** The server's reading of `excerpt`, which a read sends beside it (`text_tree`). */
  reading?: DisplayTree;
  ref?: string;
  text?: string;
}): ReadEdge {
  return {
    created_at: fields.at ?? AT,
    direction: "in",
    ...(fields.excerpt === undefined
      ? {}
      : {
          excerpt: {
            text: fields.excerpt,
            ...(fields.reading === undefined ? {} : { text_tree: fields.reading }),
          },
        }),
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

/** A task of the plan document `plan-md` of `LAY-2` that serves the focused statement, as the
 *  read answers it: its excerpt is the task's stored text, the directive its author typed with
 *  the statement's address, and its reading the task holding `content`, one paragraph of `words`
 *  where the caller names no other. */
function servingTask(
  block: string,
  words: string,
  content: readonly DisplayTree[] = [{ content: [run(words)], type: "paragraph" }]
): ReadEdge {
  const statement = "dispatch://LAY-2/spec#b-r1";
  return edge({
    excerpt: `:::plan_task{#${block} serves="${statement}"}\n${words}\n:::`,
    id: `plan-md#${block}`,
    issueKey: "LAY-2",
    kind: "serves",
    nodeKind: "block",
    reading: {
      content: [{ attrs: { blockId: block, serves: [statement] }, content, type: "plan_task" }],
      type: "doc",
    },
    ref: `dispatch://LAY-2/artifact/plan-md#b-${block}`,
  });
}

test("one row per citing node, in the order the read answers them", () => {
  const words = commentWords({ "c-1": reading("Thread 1 opens here.") });
  const rows = referenceRows(
    words,
    [
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
    ],
    NO_KNOWN,
    undefined
  );
  expect(rows.map((row) => row.key)).toEqual(["comment:c-1", "block:v-1"]);
  expect(rows.map((row) => row.words)).toEqual(["Thread 1 opens here.", ""]);
  expect(rows.map((row) => row.ref)).toEqual([
    "dispatch://LAY-2/comment/c-1",
    "dispatch://LAY-2/artifact/verification-md#b-c1",
  ]);
});

test("a node that cites the row twice is one row", () => {
  const words = commentWords({ "c-1": reading("It names it here.", "And again here.") });
  const rows = referenceRows(
    words,
    [
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
    ],
    NO_KNOWN,
    undefined
  );
  expect(rows).toHaveLength(1);
  expect(rows[0]?.words).toBe("It names it here. And again here.");
});

test("a tuple of the model names the row over a mention, and keeps the mention's place", () => {
  const rows = referenceRows(
    NO_WORDS,
    [
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
    ],
    NO_KNOWN,
    undefined
  );
  expect(rows.map((row) => `${row.key} ${row.kind}`)).toEqual([
    "block:p-8 needs",
    "comment:c-1 mentions",
  ]);
});

// A node that is no comment, message or question of this issue - another issue, a block of a
// document, a comment of another issue - whose excerpt the read sent no reading of is named by
// its word: the read's excerpt and the node's own text are its stored text, and no row draws
// them.
test("a node outside this issue's items carries no line of the read's text", () => {
  const words = commentWords({ "c-1": reading("Thread 1 opens here.") });
  const rows = referenceRows(
    words,
    [
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
    ],
    NO_KNOWN,
    undefined
  );
  expect(rows.map((row) => [row.key, row.words])).toEqual([
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
  const rows = referenceRows(
    words,
    [
      edge({
        excerpt: stored,
        id: "c-1",
        kind: "mentions",
        nodeKind: "comment",
        ref: "dispatch://LAY-2/comment/c-1",
        text: stored,
      }),
    ],
    NO_KNOWN,
    undefined
  );
  const line = rows[0]?.words ?? "";
  expect(line).toBe("Bob checks the cited statement");
  expect(line).not.toContain("**");
  expect(line).not.toContain("dispatch://");
});

// A row of what references a node is no place the page shortens: a citing item is named by all
// of its words, past its first sentence and past the length of one line of a node, with no cut
// and no ellipsis.
test("a citing item is named by all of its words, with no cut", () => {
  const paragraphs = [
    "It opens on one sentence.",
    "The rest of the block follows it, and runs on past the hundred characters one line of a node holds.",
  ];
  const words = commentWords({ "c-1": reading(...paragraphs) });
  const rows = referenceRows(
    words,
    [
      edge({
        excerpt: paragraphs.join("\n\n"),
        id: "c-1",
        kind: "mentions",
        nodeKind: "comment",
        ref: "dispatch://LAY-2/comment/c-1",
      }),
    ],
    NO_KNOWN,
    undefined
  );
  expect(rows[0]?.words).toBe(paragraphs.join(" "));
});

// A citing comment's words cite two nodes by their bare addresses: a comment the page's batched
// read names, and a question it names nothing for, as for a node no outline node cites. The row
// draws every word of the citing comment, the named comment by its name, and the question by the
// page's word for a question, never by its address. It drew the citing comment by its kind's word
// alone where any reference in its words had no name.
test("a citing item whose words cite a named node and one with no name draws all its words, the second by its kind's word", () => {
  const named = buildDispatchReference({ id: "c-2", key: "LAY-2", kind: "comment" });
  const unnamed = buildDispatchReference({ id: "a-3", key: "LAY-2", kind: "ask" });
  const text = `Compare ${named} with ${unnamed} before the review.`;
  const words = commentWords({ "c-1": reading(text) });
  const rows = referenceRows(
    words,
    [
      edge({
        excerpt: text,
        id: "c-1",
        kind: "mentions",
        nodeKind: "comment",
        ref: buildDispatchReference({ id: "c-1", key: "LAY-2", kind: "comment" }),
      }),
    ],
    { edited: new Set(), names: new Map([[named, "The rail follows the statement"]]) },
    undefined
  );
  const line = rows[0]?.words ?? "";
  expect(line).toBe(
    `Compare The rail follows the statement with ${NODE_REFERENCES_WORDS.kinds.ask} before the review.`
  );
  expect(line, "the row draws the unnamed question's address").not.toContain(unnamed);
  expect(line, "the row draws the unnamed question's id").not.toContain("a-3");
});

// A citing comment's words cite a comment its author edited since. The outline's row names that
// comment by its words and then ` · edited`, inside the reference's name (`rowReferenceWords`),
// and so does Details' row of what references the focused node.
test("a citing item's words name a reference whose post its author edited with its edited mark, as the outline's row does", () => {
  const named = buildDispatchReference({ id: "c-2", key: "LAY-2", kind: "comment" });
  const text = `Compare ${named} before the review.`;
  const rows = referenceRows(
    commentWords({ "c-1": reading(text) }),
    [
      edge({
        excerpt: text,
        id: "c-1",
        kind: "mentions",
        nodeKind: "comment",
        ref: buildDispatchReference({ id: "c-1", key: "LAY-2", kind: "comment" }),
      }),
    ],
    { edited: new Set([named]), names: new Map([[named, "The rail follows the statement"]]) },
    "LAY-2"
  );
  expect(rows[0]?.words, "the row names the edited comment without its edited mark").toBe(
    "Compare The rail follows the statement · edited before the review."
  );
});

// A citing comment's words cite a question of a project document by its bare address, which the
// page's batched read names nothing for. The row draws the question by the page's word for a
// question, as it draws an issue's question, never by its address. It drew it by the word for a
// document, the document the question is asked on.
test("a citing item whose words cite a project document's question with no name draws it by the word for a question", () => {
  const unnamed = buildDispatchReference({
    item: { id: "a-4", kind: "ask" },
    kind: "document",
    project: "LAY",
    slug: "design",
  });
  const text = `Answer ${unnamed} before the review.`;
  const rows = referenceRows(
    commentWords({ "c-1": reading(text) }),
    [
      edge({
        excerpt: text,
        id: "c-1",
        kind: "mentions",
        nodeKind: "comment",
        ref: buildDispatchReference({ id: "c-1", key: "LAY-2", kind: "comment" }),
      }),
    ],
    NO_KNOWN,
    undefined
  );
  expect(rows[0]?.words).toBe(`Answer ${NODE_REFERENCES_WORDS.kinds.ask} before the review.`);
});

test("a node the read carries no words and no address of is still one row", () => {
  const rows = referenceRows(
    NO_WORDS,
    [edge({ id: "s-1", kind: "mentions", nodeKind: "session" })],
    NO_KNOWN,
    undefined
  );
  expect(rows).toEqual([
    {
      at: AT,
      key: "session:s-1",
      kind: "mentions",
      nodeKind: "session",
      ref: undefined,
      source: undefined,
      words: "",
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
  const rows = referenceRows(
    words,
    [
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
    ],
    NO_KNOWN,
    undefined
  );
  expect(rows.map((row) => [row.key, row.nodeKind, row.ref, row.words])).toEqual([
    [
      "comment:c-1",
      "comment",
      "dispatch://LAY-2/comment/c-1",
      "It names the row here. And here again.",
    ],
    ["ask:a-1", "ask", "dispatch://LAY-2/ask/a-1", "Which statement is this about?"],
    ["message:m-2", "message", "dispatch://LAY-2/message/m-2", "Its reply names the row."],
  ]);
});

// A block of a document keeps its own kind and its own address: its parent is the document.
test("a block of a document is not read as an item", () => {
  const rows = referenceRows(
    NO_WORDS,
    [
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
    ],
    NO_KNOWN,
    undefined
  );
  expect(rows.map((row) => [row.key, row.nodeKind, row.ref])).toEqual([
    ["block:v-1", "block", "dispatch://LAY-2/artifact/v#b-c1"],
  ]);
});

// Each source in the old panel carried the citing node's issue or project, and each edge
// its time; a row carries both, and keeps the time of its first edge.
test("a row carries where the citing node is from and when it referenced the row", () => {
  const rows = referenceRows(
    NO_WORDS,
    [
      edge({
        at: "2026-11-14T09:00:00Z",
        id: "c-9",
        issueKey: "LAY-9",
        kind: "mentions",
        nodeKind: "comment",
      }),
      edge({ id: "a-1", kind: "mentions", nodeKind: "artifact", project: "LAY" }),
    ],
    NO_KNOWN,
    undefined
  );
  expect(rows.map((row) => [row.source, row.at])).toEqual([
    ["LAY-9", "2026-11-14T09:00:00Z"],
    ["LAY", AT],
  ]);
});

// Decision 560: a citing node is named by the first line of its own words, as the outline names
// a row. Two tasks of one plan that serve the row each carry the first line of the server's
// reading of its excerpt, cut at its first sentence end as R1's rule cuts a row's one line, so
// the two rows read apart. They carried no words, and both were named "served by" alone.
test("two tasks of one plan that serve the row each carry the first line of its own words", () => {
  const rows = referenceRows(
    NO_WORDS,
    [
      servingTask("t1", "Draw the history in Details. Each entry keeps its day."),
      servingTask("t2", "Read the history from the event log. It pages two hundred at a time."),
    ],
    NO_KNOWN,
    "LAY-2"
  );
  expect(
    rows.map((row) => [row.key, row.words]),
    "each task is not named by its own first line"
  ).toEqual([
    ["block:plan-md#t1", "Draw the history in Details."],
    ["block:plan-md#t2", "Read the history from the event log."],
  ]);
});

// The line is the outline's one line of a node (`lineReading`, `firstLineOf`, `drawnLine`): read
// from the first block of the reading that holds a word, a rule before it passed over, and cut at
// a word within the hundred characters a line holds, with an ellipsis where the cut fell inside a
// sentence.
test("a citing block's line is cut as the outline cuts a row's one line", () => {
  const sentence =
    "Read every row of the outline at each width the page draws at, from the phone up to the widest screen it holds today";
  const rows = referenceRows(
    NO_WORDS,
    [
      servingTask("t1", `---\n\n${sentence}`, [
        { type: "horizontal_rule" },
        { content: [run(sentence)], type: "paragraph" },
        { content: [run("A second paragraph.")], type: "paragraph" },
      ]),
    ],
    NO_KNOWN,
    "LAY-2"
  );
  expect(rows[0]?.words).toBe(
    "Read every row of the outline at each width the page draws at, from the phone up to the widest…"
  );
});

// A citing block's words cite two nodes by their bare addresses: a comment the page's batched read
// names, and a question it names nothing for. Its line names the first by its name and the second
// by the page's word for a question, as an item's words name theirs, never by an address.
test("a citing block's line names each reference it writes, never by its address", () => {
  const named = buildDispatchReference({ id: "c-2", key: "LAY-2", kind: "comment" });
  const unnamed = buildDispatchReference({ id: "a-3", key: "LAY-2", kind: "ask" });
  const rows = referenceRows(
    NO_WORDS,
    [servingTask("t1", `Answer ${unnamed} as ${named} asks. Then draw the panel.`)],
    { edited: new Set(), names: new Map([[named, "The rail follows the statement"]]) },
    "LAY-2"
  );
  const line = rows[0]?.words ?? "";
  expect(line).toBe(
    `Answer ${NODE_REFERENCES_WORDS.kinds.ask} as The rail follows the statement asks.`
  );
  expect(line, "the row draws an address").not.toContain("dispatch://");
});

// A comment of another issue reaches the list as the block of it that cites the row, and is named
// by the first line of that block's words, with its issue. A comment of this issue whose words the
// page holds is named by all of them, whatever reading the read sent of its excerpt.
test("an item of another issue is named by the first line of its words, and one of this issue by all of them", () => {
  const said = "It cites the statement from another issue. Then it moves on.";
  const rows = referenceRows(
    commentWords({ "c-1": reading("Thread 1 opens here.", "Its second paragraph follows.") }),
    [
      edge({
        excerpt: said,
        id: "c-9#b1",
        issueKey: "LAY-9",
        kind: "mentions",
        nodeKind: "block",
        parent: { id: "c-9", kind: "comment", ref: "dispatch://LAY-9/comment/c-9" },
        reading: reading(said),
        ref: "dispatch://LAY-9/comment/c-9#b1",
      }),
      edge({
        excerpt: "Thread 1 opens here.",
        id: "c-1",
        issueKey: "LAY-2",
        kind: "mentions",
        nodeKind: "comment",
        reading: reading("Thread 1 opens here."),
        ref: "dispatch://LAY-2/comment/c-1",
      }),
    ],
    NO_KNOWN,
    "LAY-2"
  );
  expect(rows.map((row) => [row.key, row.source, row.words])).toEqual([
    ["comment:c-9", "LAY-9", "It cites the statement from another issue."],
    ["comment:c-1", "LAY-2", "Thread 1 opens here. Its second paragraph follows."],
  ]);
});

// A node with no words of its own carries none, and the component names it by its word: a task
// whose reading holds no word, one whose excerpt the read sent no reading of, and an issue and a
// document, whose excerpt is the issue's title and the document's file name, a reading the
// server sends and no page reads.
test("a node with no words of its own carries none", () => {
  const rows = referenceRows(
    NO_WORDS,
    [
      servingTask("t1", "---", [{ type: "horizontal_rule" }]),
      edge({
        excerpt: "Draw the panel.",
        id: "plan-md#t2",
        issueKey: "LAY-2",
        kind: "serves",
        nodeKind: "block",
        ref: "dispatch://LAY-2/artifact/plan-md#b-t2",
      }),
      edge({
        excerpt: "The rail becomes an icon rail.",
        id: "LAY-9",
        kind: "child_of",
        nodeKind: "issue",
        reading: reading("The rail becomes an icon rail."),
        ref: "dispatch://LAY-9",
      }),
      edge({
        excerpt: "notes.md",
        id: "a-1",
        issueKey: "LAY-2",
        kind: "mentions",
        nodeKind: "artifact",
        reading: reading("notes.md"),
        ref: "dispatch://LAY-2/artifact/notes-md",
      }),
    ],
    NO_KNOWN,
    "LAY-2"
  );
  expect(rows.map((row) => [row.key, row.words])).toEqual([
    ["block:plan-md#t1", ""],
    ["block:plan-md#t2", ""],
    ["issue:LAY-9", ""],
    ["artifact:a-1", ""],
  ]);
});
