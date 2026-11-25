import { beforeAll, expect, test } from "bun:test";

import type { DisplayTree } from "../../api/types";
import { actorLabelText } from "../refs/AgentName";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { deletedLineText } from "../refs/post-marks";
import type { TreeNode } from "./issue-tree";
import { NODE_TYPE_NAMES, type NodeType, type Preset } from "./page-words";
import { filterSignals, presetView } from "./presets";
import { parentLinkWords } from "./projection-fixture";
import { type DrawnRow, NOTHING_OPENED, treeRows } from "./tree-fold";
import { everythingOpened } from "./tree-keys";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/**
 * The page with no filter on and the four filters as views of the one tree: with no filter on
 * the whole tree, folded to the frontier; under a filter its own kinds as rows and no other, each
 * other parent a line on its child's row.
 */

function node(
  id: string,
  type: NodeType,
  children: readonly TreeNode[] = [],
  extra: Partial<TreeNode> = {}
): TreeNode {
  return {
    alsoUnder: [],
    children,
    chips: [],
    facts: [],
    gaps: [],
    held: children.reduce((total, child) => total + child.held + 1, 0),
    id,
    question: false,
    replaced: false,
    text: id,
    transcluded: false,
    type,
    waiting: false,
    ...extra,
  };
}

/** A requirement of the spec, which the tree marks a statement (`isStatement`): the requirement
 *  the Spec filter draws as a row and the Verification filter as context. */
function statement(
  id: string,
  children: readonly TreeNode[] = [],
  extra: Partial<TreeNode> = {}
): TreeNode {
  return node(id, "requirement", children, { statement: true, ...extra });
}

/** The ids of every row drawn, in reading order; a branch's own row reads as `more:<id>`. */
function idsOf(rows: readonly DrawnRow[]): string[] {
  return rows.map((row) => (row.kind === "deeper" ? `more:${row.of.id}` : row.node.id));
}

/** The rows the page draws for `preset` under `query`: a filtered view with every branch it kept
 *  open, which is the fold `IssueTree.tsx` draws, and the issue's own row dropped where the view
 *  draws none. Each row reads as its id and its depth. */
function drawn(root: TreeNode, preset: Preset, query = ""): string[] {
  const view = presetView(root, preset, query);
  const fold = view.filtering ? everythingOpened(view.root) : NOTHING_OPENED;
  const rows = treeRows(view.root, fold);
  return idsOf(view.rootRow ? rows : rows.slice(1));
}

/** Every node the view draws, by id, with the line its parent is drawn as where it has one. */
function parentLines(root: TreeNode): Record<string, string> {
  const lines: Record<string, string> = {};
  const walk = (at: TreeNode): void => {
    if (at.parentLine !== undefined) {
      lines[at.id] = `${at.parentLine.type}: ${parentLinkWords(at.parentLine)}`;
    }
    for (const child of at.children) walk(child);
  };
  walk(root);
  return lines;
}

/**
 * An issue as today's reads place it: an ask the person answered, with the requirement it raised
 * and the decision that settled it under it; a second requirement under the issue; a check that
 * serves both requirements, drawn under each, with a failed result under it; a plan task under
 * the first requirement and drawn again under the second; a plan task under a comment; a
 * decision waiting for review under a comment; and an open question nobody answered.
 */
function issue(): TreeNode {
  const check = (transcluded: boolean) =>
    node("verification#b-c1", "check", [node("result-1", "result", [], { transcluded })], {
      state: "failed",
      transcluded,
    });
  return node("CORE-1", "issue", [
    node(
      "ask-1",
      "ask",
      [
        statement("spec#b-r1", [check(false), node("plan#b-t1", "plan_task")]),
        node("decision-1", "decision", [], { state: "kept" }),
      ],
      { state: "answered", text: "Which layout does the page take? The three pictures show it." }
    ),
    statement("spec#b-r2", [
      check(true),
      node("plan#b-t1", "plan_task", [], { transcluded: true }),
    ]),
    node("comment-1", "comment", [
      node("plan#b-t2", "plan_task"),
      // A decision of the review list: one that waits and that a tie names a statement for,
      // which is what the tree marks `waiting` (R35).
      node("decision-2", "decision", [], { state: "waiting_review", waiting: true }),
    ]),
    node("ask-2", "ask", [], { question: true, state: "open" }),
  ]);
}

test("with no filter on the view is the whole tree, folded to the frontier", () => {
  const root = issue();
  const view = presetView(root, "conversation", "");
  expect(view.filtering).toBe(false);
  expect(view.rootRow).toBe(true);
  // Exactly the fold the page draws with nothing narrowing it.
  expect(drawn(root, "conversation")).toEqual(idsOf(treeRows(root)));
  expect(parentLines(view.root)).toEqual({});
});

// R8, c-tree: the spec's summary callout is the head's three sentences and no row of the
// conversation, unless the address names it.
test("with no filter on the spec's memo callout is no row, unless the address names it", () => {
  const memo = node("spec#b-summary", "callout");
  const root = node("CORE-1", "issue", [memo, node("ask-1", "ask")]);
  const ids = (view: { root: TreeNode }) => view.root.children.map((child) => child.id);
  expect(ids(presetView(root, "conversation", "", new Set(), memo.id))).toEqual(["ask-1"]);
  expect(ids(presetView(root, "conversation", "", new Set([memo.id]), memo.id))).toEqual([
    "spec#b-summary",
    "ask-1",
  ]);
});

test("a filter by words with no filter on opens the path down to what it matched", () => {
  const root = issue();
  const view = presetView(root, "conversation", "b-t2");
  expect(view.filtering).toBe(true);
  expect([...view.context]).toEqual(["comment-1", "CORE-1"]);
  expect(drawn(root, "conversation", "b-t2")).toEqual(["CORE-1", "comment-1", "plan#b-t2"]);
});

test("Spec draws the requirements as rows, the issue as none, and the ask above one as its line", () => {
  const root = issue();
  const view = presetView(root, "spec", "");
  expect(view.rootRow).toBe(false);
  expect(view.matches).toBe(2);
  expect(drawn(root, "spec")).toEqual(["spec#b-r1", "spec#b-r2"]);
  // The ask the first requirement sits under is one line on its row, cut by R1's first-line
  // rule; the second sits under the issue, which is the page itself and no line.
  expect(parentLines(view.root)).toEqual({
    "spec#b-r1": "ask: Which layout does the page take?",
  });
});

// A criterion is a row of Spec with the statements, and in its control's count; a statement
// written under a criterion stands under it.
test("Spec counts criteria", () => {
  const root = node("CORE-1", "issue", [
    node("comment-1", "comment", [node("criterion-1", "criterion", [statement("spec#b-r1")])]),
  ]);
  expect(drawn(root, "spec")).toEqual(["criterion-1", "spec#b-r1"]);
  expect(filterSignals(root, "").spec.count).toBe(2);
});

test("Verification draws each requirement as context with its checks under it and their results", () => {
  const root = issue();
  const view = presetView(root, "verification", "");
  expect(drawn(root, "verification")).toEqual([
    "spec#b-r1",
    "verification#b-c1",
    "result-1",
    "spec#b-r2",
    "verification#b-c1",
    "result-1",
  ]);
  expect([...view.context].sort()).toEqual(["spec#b-r1", "spec#b-r2"]);
  // The check drawn under both requirements is one check.
  expect(view.matches).toBe(1);
  expect(parentLines(view.root)).toEqual({
    "spec#b-r1": "ask: Which layout does the page take?",
  });
});

test("Plan draws each plan task once, its requirement or comment one line on its row", () => {
  const root = issue();
  const view = presetView(root, "plan", "");
  expect(drawn(root, "plan")).toEqual(["plan#b-t1", "plan#b-t2"]);
  expect(view.matches).toBe(2);
  expect(view.context.size).toBe(0);
  expect(parentLines(view.root)).toEqual({
    "plan#b-t1": "requirement: spec#b-r1",
    "plan#b-t2": "comment: comment-1",
  });
});

test("Decisions draws the asks and decisions, a decision under its ask and another's parent a line", () => {
  const root = issue();
  const view = presetView(root, "decisions", "");
  expect(drawn(root, "decisions")).toEqual(["ask-1", "decision-1", "decision-2", "ask-2"]);
  expect(view.matches).toBe(4);
  expect(parentLines(view.root)).toEqual({ "decision-2": "comment: comment-1" });
});

test("a node the address names is drawn whatever the filter", () => {
  const root = issue();
  const view = presetView(root, "spec", "", new Set(["comment-1"]));
  expect(idsOf(treeRows(view.root, everythingOpened(view.root))).slice(1)).toEqual([
    "spec#b-r1",
    "spec#b-r2",
    "comment-1",
  ]);
  // It is drawn and not counted: the count is the filter's own kind.
  expect(view.matches).toBe(2);
});

test("each filter's signal is its count, with what of it asks something of the reader", () => {
  expect(filterSignals(issue(), "")).toEqual({
    decisions: { count: 4, flag: { count: 1, word: "to_review" } },
    plan: { count: 2, unit: "steps" },
    spec: { count: 2 },
    verification: { count: 1, flag: { count: 1, word: "failing" } },
  });
  // A filter by words narrows every count with it.
  expect(filterSignals(issue(), "b-t2").plan.count).toBe(1);
});

// R35: "to review" counts the review list alone. A decision that waits and is tied to no
// statement keeps its state word and is in no count of what waits on the reader.
test("a waiting decision outside the review list is not counted as to review", () => {
  const root = node("CORE-1", "issue", [
    node("decision-1", "decision", [], { state: "waiting_review", waiting: true }),
    node("decision-2", "decision", [], { state: "waiting_review" }),
  ]);
  expect(filterSignals(root, "").decisions).toEqual({
    count: 2,
    flag: { count: 1, word: "to_review" },
  });
});

// R2, R9: the Decisions filter draws a person's answer as the decision and the section of
// decisions.md that restates it as no entry, so its count holds the answer and not the section.
// A decision under a question nobody answered restates nothing, and is counted.
test("the Decisions count leaves out a section that restates a person's answer", () => {
  const answer = {
    at: "2026-11-14T08:00:00Z",
    by: { id: "alice", kind: "user" as const },
    selected: [],
    text: "Yes.",
  };
  const root = node("CORE-1", "issue", [
    node("ask-1", "ask", [node("decision-1", "decision")], { answer, state: "answered" }),
    node("ask-2", "ask", [node("decision-2", "decision")], { question: true, state: "open" }),
    node("decision-3", "decision"),
  ]);
  expect(filterSignals(root, "").decisions).toEqual({ count: 4 });
});

// Under Decisions an answered question draws no reading, since the question already draws the
// person's answer.
test("Decisions draws no reading of an answered ask", () => {
  const root = node("CORE-1", "issue", [
    node("ask-1", "ask", [node("reading-1", "reading")], { state: "answered" }),
  ]);
  expect(drawn(root, "decisions")).toEqual(["ask-1"]);
  expect(filterSignals(root, "").decisions).toEqual({ count: 1 });
});

// A reading of an answered question the address names is drawn under Decisions all the same, as
// every filter draws a node the address names.
test("Decisions draws a reading the address names", () => {
  const root = node("CORE-1", "issue", [
    node("ask-1", "ask", [node("reading-1", "reading")], { state: "answered" }),
  ]);
  const view = presetView(root, "decisions", "", new Set(["reading-1"]));
  expect(idsOf(treeRows(view.root, everythingOpened(view.root))).slice(1)).toEqual([
    "ask-1",
    "reading-1",
  ]);
});

// Under Decisions an agent decision's readings are rows under it, and no reading is in the count,
// which holds the asks and the decisions alone.
test("Decisions draws an agent decision's reading under it", () => {
  const planner = { id: "planner", kind: "session" as const };
  const root = node("CORE-1", "issue", [
    node("comment-1", "comment", [
      node("decision-1", "decision", [node("reading-1", "reading", [], { author: planner })], {
        author: planner,
        state: "waiting_review",
      }),
    ]),
  ]);
  const view = presetView(root, "decisions", "");
  expect(drawn(root, "decisions")).toEqual(["decision-1", "reading-1"]);
  expect(view.root.children.map((row) => row.children.map((child) => child.id))).toEqual([
    ["reading-1"],
  ]);
  expect(view.matches).toBe(1);
});

// R26, R18: the parent a filter draws as no row is one line on its child's row, and carries the
// parent's own address where the model gives it one, so the line is the link to that node, and
// the parent's own words, which the row projects its line from.
test("a parent line carries the parent's own address", () => {
  const root = node("CORE-1", "issue", [
    statement("spec#b-r1", [node("plan#b-t1", "plan_task")], {
      reference: "dispatch://CORE-1/spec#b-r1",
    }),
    node("comment-1", "comment", [node("plan#b-t2", "plan_task")]),
  ]);
  const lines = presetView(root, "plan", "").root.children.map((task) => task.parentLine);
  expect(lines).toEqual([
    {
      id: "spec#b-r1",
      reference: "dispatch://CORE-1/spec#b-r1",
      text: "spec#b-r1",
      type: "requirement",
    },
    { id: "comment-1", text: "comment-1", type: "comment" },
  ]);
});

// R2 and R3 of dispatch://INFRA-1523: a parent the filter draws as no row is a comment or a
// message its author may have corrected, so its line carries the parent's marks - when and by
// whom it was deleted, when it was edited - and its child's row draws them with the line. A
// parent nobody corrected carries none.
test("a parent line carries a deleted or an edited parent's marks", () => {
  const by = { id: "alice", kind: "user" };
  const root = node("CORE-1", "issue", [
    node("comment-1", "comment", [node("plan#b-t1", "plan_task")], {
      deletedAt: "2026-11-05T12:00:00Z",
      deletedBy: by,
      text: "Deleted by alice · 2026-11-05T12:00:00Z",
    }),
    node("message-1", "message", [node("plan#b-t2", "plan_task")], {
      editedAt: "2026-11-05T13:00:00Z",
    }),
    node("comment-2", "comment", [node("plan#b-t3", "plan_task")]),
  ]);
  const lines = presetView(root, "plan", "").root.children.map((task) => task.parentLine);
  expect(lines).toEqual([
    {
      deletedAt: "2026-11-05T12:00:00Z",
      deletedBy: by,
      id: "comment-1",
      text: "Deleted by alice · 2026-11-05T12:00:00Z",
      type: "comment",
    },
    { editedAt: "2026-11-05T13:00:00Z", id: "message-1", text: "message-1", type: "message" },
    { id: "comment-2", text: "comment-2", type: "comment" },
  ]);
});

// R1, R2: a parent's line is drawn as a row's first line is
// (`drawnLine`), with an ellipsis where the cut ended inside a sentence and none where the line
// is a whole sentence, whose own mark ends it.
test("a parent's line draws its ellipsis only where the cut ended inside a sentence", () => {
  // The words R1's limit keeps of one sentence of 114 characters: it is cut at the last word
  // that fits.
  const kept = [
    "Which of the three layouts does the page take when a reader opens",
    "an issue that holds many asks and",
  ].join(" ");
  const root = node("CORE-1", "issue", [
    node("ask-1", "ask", [statement("spec#b-r1")], {
      state: "answered",
      text: `${kept} many comments?`,
    }),
    node("ask-2", "ask", [statement("spec#b-r2")], {
      state: "answered",
      text: "Which layout does the page take? The three pictures show it.",
    }),
  ]);
  const view = presetView(root, "spec", "");
  expect(drawn(root, "spec")).toEqual(["spec#b-r1", "spec#b-r2"]);
  const [long, short] = view.root.children.map((row) =>
    row.parentLine === undefined ? undefined : parentLinkWords(row.parentLine)
  );
  // The one sentence runs past the limit, so the line stops inside it and says it carries on.
  expect(long).toEndWith("…");
  expect(long).toBe(`${kept}…`);
  // Two short sentences: the line is the first, whole, and its own mark ends it.
  expect(short).toBe("Which layout does the page take?");
});

// A parent's line is cut from the parent's first block (`lineReading`), so a parent whose reading
// opens with a heading draws that heading alone, and never the heading run into the paragraph
// after it. A parent with no reading is read the same way from its stored text, whose heading
// marker is markup.
test("a parent whose reading opens with a heading draws that heading alone as its line", () => {
  const heading = "Which layout the page takes";
  const reading: DisplayTree = {
    content: [
      { attrs: { level: 2 }, content: [{ text: heading, type: "text" }], type: "heading" },
      { content: [{ text: "The three pictures show it.", type: "text" }], type: "paragraph" },
    ],
    type: "doc",
  };
  const text = `## ${heading}\n\nThe three pictures show it.`;
  const root = node("CORE-1", "issue", [
    node("ask-1", "ask", [statement("spec#b-r1")], {
      state: "answered",
      text,
      tree: reading,
    }),
    node("ask-2", "ask", [statement("spec#b-r2")], { state: "answered", text }),
  ]);
  expect(parentLines(presetView(root, "spec", "").root)).toEqual({
    "spec#b-r1": `ask: ${heading}`,
    "spec#b-r2": `ask: ${heading}`,
  });
});

// R22: an id is no name a reader reads. A parent a filter draws as no row and that holds no
// words - no text at all, or a reading none of whose blocks holds a word - is drawn on its
// child's row as the page's word for its kind, never as its id.
test("a parent with no words is drawn on its child's row as its kind's word, never its id", () => {
  const ruled: DisplayTree = { content: [{ type: "horizontal_rule" }], type: "doc" };
  const root = node("CORE-1", "issue", [
    statement("spec#b-r1", [node("plan#b-t1", "plan_task")], { text: "" }),
    node("comment-1", "comment", [node("plan#b-t2", "plan_task")], { text: "---", tree: ruled }),
    node("ask-1", "ask", [node("plan#b-t3", "plan_task")], { state: "answered", text: "" }),
  ]);
  const view = presetView(root, "plan", "");
  expect(drawn(root, "plan")).toEqual(["plan#b-t1", "plan#b-t2", "plan#b-t3"]);
  expect(parentLines(view.root)).toEqual({
    "plan#b-t1": `requirement: ${NODE_TYPE_NAMES.requirement}`,
    "plan#b-t2": `comment: ${NODE_TYPE_NAMES.comment}`,
    "plan#b-t3": `ask: ${NODE_TYPE_NAMES.ask}`,
  });
  for (const row of view.root.children) {
    const line = row.parentLine;
    expect(line === undefined ? undefined : parentLinkWords(line)).not.toBe(line?.id);
  }
});

// The words filter read a node's stored source, so a comment drawn as "bold text" and stored as
// `**bold** text` was missed by `?q=bold text` and found by `?q=**`, which no word it draws holds.
// It reads the node's words (`nodeWords`, R4).
test("the words filter matches the words a node draws, never its stored source", () => {
  const reading: DisplayTree = {
    content: [
      {
        content: [
          { marks: [{ type: "strong" }], text: "bold", type: "text" },
          { text: " text", type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const root = node("CORE-1", "issue", [
    node("comment-1", "comment", [], { text: "**bold** text", tree: reading }),
  ]);
  expect(presetView(root, "conversation", "bold text").matches).toBe(1);
  expect(drawn(root, "conversation", "bold text")).toEqual(["CORE-1", "comment-1"]);
  const markup = presetView(root, "conversation", "**");
  expect(markup.matches).toBe(0);
  expect(markup.root.children).toEqual([]);
});

// The architect's ruling of 2026-11-20 07:52Z on dispatch://INFRA-1523: a deleted post's node holds
// its deleted line as its text, as the model writes it (`tree-drafts.ts`'s `deletedPostLine`): a
// session named by its short id and the time as stored, which its row draws as neither, since the
// row draws the page's own deleted line (`DeletedLine`). A deleted post has no words of its own
// left, so the words filter matches it by none: not by the session's id, the raw time or the
// line's words. A post nobody deleted still matches by its words.
test("the words filter matches a deleted post by no words", () => {
  const session = { id: "0f8e2c4a-1111-4222-8333-444455556666", kind: "session" } as const;
  const deletion = { at: "2026-11-05T12:00:00Z", by: session };
  const root = node("CORE-1", "issue", [
    node("comment-1", "comment", [], {
      deletedAt: deletion.at,
      deletedBy: session,
      text: `${deletedLineText(deletion, { label: actorLabelText })} · ${deletion.at}`,
    }),
    node("comment-2", "comment", [], { text: "Ship it on Friday." }),
  ]);
  const queries = [actorLabelText(session), "0f8e", "2026-11-05T12", "deleted by", "friday"];
  expect(
    Object.fromEntries(
      queries.map((query) => [
        query,
        drawn(root, "conversation", query).filter((id) => id.startsWith("comment")),
      ])
    )
  ).toEqual({
    [actorLabelText(session)]: [],
    "0f8e": [],
    "2026-11-05T12": [],
    "deleted by": [],
    friday: ["comment-2"],
  });
});
