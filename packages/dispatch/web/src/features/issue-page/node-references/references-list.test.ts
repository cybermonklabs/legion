import { expect, test } from "bun:test";

import { graphNode } from "../../../__tests__/graph-fixture";
import type { DisplayTree, GraphEdge, GraphReferences, ReferenceTarget } from "../../../api/types";
import { buildDispatchReference } from "../../refs/routes";
import { NODE_REFERENCES_WORDS } from "../page-words";
import { issueItemWords, referenceRows } from "./references-list";
import { type ServingTask, servingTask, statementsReaching } from "./serving-task-fixture";

const AT = "2026-11-15T10:00:00Z";

/** No item of the issue has words the page holds. */
const NO_WORDS: ReadonlyMap<string, string> = new Map();

/** The page's batched names read has answered for no citing block. */
const NO_TARGETS: readonly ReferenceTarget[] = [];

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

/** The page's batched names read's member for the block at `ref`: the server's reading of the
 *  block's own content (`text_tree`), one element of `content` per top-level block, and its
 *  stored excerpt as its `name`, which no row reads. */
function blockTarget(ref: string, content: readonly DisplayTree[], name = ""): ReferenceTarget {
  return { gone: false, kind: "block", name, ref, text_tree: { content, type: "doc" } };
}

/** The rows of the tasks `tasks`, with the names read's member of each, for a statement of
 *  `LAY-2`: no item of the issue holds words the page holds. */
function taskRows(
  tasks: readonly ServingTask[],
  known: Parameters<typeof referenceRows>[3] = NO_KNOWN
) {
  return referenceRows(
    NO_WORDS,
    tasks.map((task) => task.edge),
    tasks.map((task) => task.target),
    known,
    "LAY-2"
  );
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
    NO_TARGETS,
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
    NO_TARGETS,
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
    NO_TARGETS,
    NO_KNOWN,
    undefined
  );
  expect(rows.map((row) => `${row.key} ${row.kind}`)).toEqual([
    "block:p-8 needs",
    "comment:c-1 mentions",
  ]);
});

// A node that is no comment, message or question of this issue - another issue, a block of a
// document, a comment of another issue - that the page's batched names read has named nothing for
// is named by its word: the read's excerpt and the node's own text are its stored text, and no row
// draws them.
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
    NO_TARGETS,
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
    NO_TARGETS,
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
    NO_TARGETS,
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
    NO_TARGETS,
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
    NO_TARGETS,
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
    NO_TARGETS,
    NO_KNOWN,
    undefined
  );
  expect(rows[0]?.words).toBe(`Answer ${NODE_REFERENCES_WORDS.kinds.ask} before the review.`);
});

test("a node the read carries no words and no address of is still one row", () => {
  const rows = referenceRows(
    NO_WORDS,
    [edge({ id: "s-1", kind: "mentions", nodeKind: "session" })],
    NO_TARGETS,
    NO_KNOWN,
    undefined
  );
  expect(rows).toEqual([
    {
      at: AT,
      code: false,
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
    NO_TARGETS,
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
    NO_TARGETS,
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
    NO_TARGETS,
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
// reading of its content, which the page's batched names read answers for its address, cut at its
// first sentence end as R1's rule cuts a row's one line, so the two rows read apart. They carried
// no words, and both were named "served by" alone.
test("two tasks of one plan that serve the row each carry the first line of its own words", () => {
  const rows = taskRows([
    servingTask("t1", "Draw the history in Details. Each entry keeps its day."),
    servingTask("t2", "Read the history from the event log. It pages two hundred at a time."),
  ]);
  expect(
    rows.map((row) => [row.key, row.words]),
    "each task is not named by its own first line"
  ).toEqual([
    ["block:plan-md#t1", "Draw the history in Details."],
    ["block:plan-md#t2", "Read the history from the event log."],
  ]);
});

// Decision 565 (1): the references read's excerpt of a block is its first 480 runes, its opening
// line included (`refs.BlockExcerpt`), and the opening line of a task that serves many statements
// is longer than that, so its excerpt is markup alone. The task is named by the first line of its
// own content, which the names read answers with no opening line. Named from the excerpt, it read
// `:::plan_task{#t1 serves="document document…`.
test("a task whose opening line is longer than the excerpt is named by its first line", () => {
  const task = servingTask(
    "t1",
    "Draw the history in Details. Each entry keeps its day.",
    undefined,
    statementsReaching("t1", 520)
  );
  expect(
    task.edge.excerpt?.text,
    "the excerpt holds the task's words, so the case proves nothing"
  ).not.toContain("Draw the history");
  const [row] = taskRows([task]);
  expect(
    row?.words,
    "the task whose opening line passes the excerpt is not named by its first line"
  ).toBe("Draw the history in Details.");
  expect(row?.code).toBe(false);
});

// Decision 565 (1): a task whose first sentence crosses the excerpt's 480th rune has an excerpt cut
// inside that sentence, with no ellipsis. The task is named by its own first sentence, which is
// longer than a row's one line, cut as the outline cuts it, with the outline's ellipsis. Named from
// the excerpt, it read "Read the history from the event log one pa".
test("a task whose first sentence is longer than a row's line is named with the outline's ellipsis", () => {
  const sentence =
    "Read the history from the event log one page of two hundred events at a time, newest first, and keep the read mark where the reader left it.";
  const task = servingTask(
    "t1",
    `${sentence} It pages on.`,
    undefined,
    statementsReaching("t1", 420)
  );
  const excerpt = task.edge.excerpt?.text ?? "";
  expect(
    excerpt.includes("Read the history") && !excerpt.includes(sentence),
    "the excerpt is not cut inside the task's first sentence, so the case proves nothing"
  ).toBe(true);
  const [row] = taskRows([task]);
  expect(row?.words, "the task's first sentence is not cut with the outline's ellipsis").toBe(
    "Read the history from the event log one page of two hundred events at a time, newest first, and keep…"
  );
});

// The line is the outline's one line of a node (`lineReading`, `firstLineOf`, `drawnLine`): read
// from the first block of the reading that holds a word, a rule before it passed over, and cut at
// a word within the hundred characters a line holds, with an ellipsis where the cut fell inside a
// sentence.
test("a citing block's line is cut as the outline cuts a row's one line", () => {
  const sentence =
    "Read every row of the outline at each width the page draws at, from the phone up to the widest screen it holds today";
  const rows = taskRows([
    servingTask("t1", `---\n\n${sentence}`, [
      { type: "horizontal_rule" },
      { content: [run(sentence)], type: "paragraph" },
      { content: [run("A second paragraph.")], type: "paragraph" },
    ]),
  ]);
  expect(rows[0]?.words).toBe(
    "Read every row of the outline at each width the page draws at, from the phone up to the widest…"
  );
});

// The names read answers a task with the server's reading of its own content, whose top-level
// blocks are the task's heading and the paragraph under it, as the outline reads a typed block
// (`tree-drafts.ts`'s `TreeContext.treeOf`). The outline draws its line from the first block of it
// that holds a word (`lineReading`), so the row names the task by its heading's words alone. It ran
// the heading into the paragraph: "Draw the history Each entry keeps its day."; and a task whose
// opening line passes the excerpt was named by its markup.
test("a task whose words open on a heading is named by the heading's words alone, as the outline names it", () => {
  const rows = taskRows([
    servingTask(
      "t1",
      "## Draw the history\n\nEach entry keeps its day.",
      [
        { attrs: { level: 2 }, content: [run("Draw the history")], type: "heading" },
        { content: [run("Each entry keeps its day.")], type: "paragraph" },
      ],
      statementsReaching("t1", 520)
    ),
  ]);
  expect(rows[0]?.words, "the task's line runs its heading into the paragraph under it").toBe(
    "Draw the history"
  );
});

// A citing block's words cite two nodes by their bare addresses: a comment the page's batched read
// names, and a question it names nothing for. Its line names the first by its name and the second
// by the page's word for a question, as an item's words name theirs, never by an address.
test("a citing block's line names each reference it writes, never by its address", () => {
  const named = buildDispatchReference({ id: "c-2", key: "LAY-2", kind: "comment" });
  const unnamed = buildDispatchReference({ id: "a-3", key: "LAY-2", kind: "ask" });
  const rows = taskRows(
    [servingTask("t1", `Answer ${unnamed} as ${named} asks. Then draw the panel.`)],
    {
      edited: new Set(),
      names: new Map([[named, "The rail follows the statement"]]),
    }
  );
  const line = rows[0]?.words ?? "";
  expect(line).toBe(
    `Answer ${NODE_REFERENCES_WORDS.kinds.ask} as The rail follows the statement asks.`
  );
  expect(line, "the row draws an address").not.toContain("dispatch://");
});

// Decision 565 (2): a code block of another document that quotes the task by its address is a node
// that references it. Code is literal, so the row names the code block by the first line of its
// code as typed, the address in it written as its author wrote it, and no name of the task in its
// place; the component draws it as code. Named as words are, it read "See Draw the history in
// Details.", the task's own words in the code's place.
test("a code block that quotes the row is named by its code's first line, as typed", () => {
  const task = "dispatch://LAY-2/artifact/plan-md#b-t1";
  const ref = "dispatch://LAY-2/artifact/notes-md#b-c9";
  const code = `See ${task} for the history.\nThen read the log.`;
  const [row, ...more] = referenceRows(
    NO_WORDS,
    [
      edge({
        excerpt: `\`\`\`\n${code}\n\`\`\``,
        id: "notes-md#c9",
        issueKey: "LAY-2",
        kind: "mentions",
        nodeKind: "block",
        ref,
      }),
    ],
    [blockTarget(ref, [{ content: [run(code)], type: "code_block" }], `\`\`\`\n${code}\n\`\`\``)],
    { edited: new Set(), names: new Map([[task, "Draw the history in Details."]]) },
    "LAY-2"
  );
  expect(more).toHaveLength(0);
  expect(
    [row?.code, row?.words],
    "the code block is not named by its code's first line as typed, as code"
  ).toEqual([true, `See ${task} for the history.`]);
});

// A comment of another issue reaches the list as the block of it that cites the row, and is named
// by the first line of that block's words, which the names read answers for the block's address,
// with its issue. A comment of this issue whose words the page holds is named by all of them,
// whatever the names read answers.
test("an item of another issue is named by the first line of its words, and one of this issue by all of them", () => {
  const said = "It cites the statement from another issue. Then it moves on.";
  const block = "dispatch://LAY-9/comment/c-9#b1";
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
        ref: block,
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
    [blockTarget(block, [{ content: [run(said)], type: "paragraph" }], said)],
    NO_KNOWN,
    "LAY-2"
  );
  expect(rows.map((row) => [row.key, row.source, row.words])).toEqual([
    ["comment:c-9", "LAY-9", "It cites the statement from another issue."],
    ["comment:c-1", "LAY-2", "Thread 1 opens here. Its second paragraph follows."],
  ]);
});

// A node with no words of its own carries none, and the component names it by its word: a task
// whose reading holds no word, one the names read has not answered for, one it answered gone, and
// an issue and a document, whose excerpt is the issue's title and the document's file name, a
// reading the server sends and no page reads.
test("a node with no words of its own carries none", () => {
  const wordless = servingTask("t1", "---", [{ type: "horizontal_rule" }]);
  const unanswered = servingTask("t2", "Draw the panel.");
  const gone = servingTask("t3", "Read the panel.");
  const rows = referenceRows(
    NO_WORDS,
    [
      wordless.edge,
      unanswered.edge,
      gone.edge,
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
    [wordless.target, { gone: true, kind: "block", name: "", ref: gone.target.ref }],
    NO_KNOWN,
    "LAY-2"
  );
  expect(rows.map((row) => [row.key, row.words])).toEqual([
    ["block:plan-md#t1", ""],
    ["block:plan-md#t2", ""],
    ["block:plan-md#t3", ""],
    ["issue:LAY-9", ""],
    ["artifact:a-1", ""],
  ]);
});
