import { expect, test } from "bun:test";

import type { TreeNode } from "./issue-tree";
import type { NodeType, Preset } from "./page-words";
import { filterSignals, presetView } from "./presets";
import { type DrawnRow, NOTHING_OPENED, treeRows } from "./tree-fold";
import { everythingOpened } from "./tree-keys";

/**
 * The page with no filter on and the four filters as views of the one tree (dispatch://INFRA-1196
 * R1, R2): with no filter on the whole tree, folded to the frontier; under a filter its own
 * kinds as rows and no other, each other parent a line on its child's row.
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
    if (at.parentLine !== undefined) lines[at.id] = `${at.parentLine.type}: ${at.parentLine.line}`;
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
        node("spec#b-r1", "requirement", [check(false), node("plan#b-t1", "plan_task")]),
        node("decision-1", "decision", [], { state: "kept" }),
      ],
      { state: "answered", text: "Which layout does the page take? The three pictures show it." }
    ),
    node("spec#b-r2", "requirement", [
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

// R26, R18: the parent a filter draws as no row is one line on its child's row, and carries the
// parent's own address where the model gives it one, so the line is the link to that node.
test("a parent line carries the parent's own address", () => {
  const root = node("CORE-1", "issue", [
    node("spec#b-r1", "requirement", [node("plan#b-t1", "plan_task")], {
      reference: "dispatch://CORE-1/spec#b-r1",
    }),
    node("comment-1", "comment", [node("plan#b-t2", "plan_task")]),
  ]);
  const lines = presetView(root, "plan", "").root.children.map((task) => task.parentLine);
  expect(lines).toEqual([
    {
      id: "spec#b-r1",
      line: "spec#b-r1",
      reference: "dispatch://CORE-1/spec#b-r1",
      type: "requirement",
    },
    { id: "comment-1", line: "comment-1", type: "comment" },
  ]);
});
