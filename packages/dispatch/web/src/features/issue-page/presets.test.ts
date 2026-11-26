import { beforeAll, expect, test } from "bun:test";

import type { DisplayTree, IssueTreeParent } from "../../api/types";
import { actorLabelText } from "../refs/AgentName";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { deletedLineText } from "../refs/post-marks";
import type { AddressFallback } from "./address-target";
import type { AddressTarget } from "./issue-address";
import type { TreeNode } from "./issue-tree";
import { type NodeStates, stampStates } from "./node-states/node-states";
import { NODE_TYPE_NAMES, type NodeType, PRESET_ORDER, type Preset } from "./page-words";
import {
  answerHolds,
  filterHolding,
  filterOfUndrawn,
  filterSignals,
  isFilter,
  presetView,
  tagFilter,
} from "./presets";
import { nameOf, parentLinkWords } from "./projection-fixture";
import { addressView } from "./slots/body";
import { type DrawnRow, treeRows } from "./tree-fold";
import { type TreeNodeExtra, treeAnswer, treeNode, treeTuple } from "./tree-read/tree-fixtures";
import type { TreeAnswer, TreeAnswerNode } from "./tree-read/tree-read";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/**
 * The page with no filter on and the four filters as views of the one tree: with no filter on
 * the whole tree, every node of it unfolded; under a filter its own kinds as rows and no other, each
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

/** The ids of every row drawn, in reading order; the row that pages a node's children reads as
 *  `page:<id>`. */
function idsOf(rows: readonly DrawnRow[]): string[] {
  return rows.map((row) => (row.kind === "page" ? `page:${row.of.id}` : row.node.id));
}

/** The rows the page draws for `preset` under `query`: every node the view kept, unfolded, which
 *  is what `IssueTree.tsx` draws, and the issue's own row dropped where the view draws none. */
function drawn(root: TreeNode, preset: Preset, query = ""): string[] {
  const view = presetView(root, preset, query);
  const rows = treeRows(view.root);
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

// R9, R10, R11: with no filter on - Conversation, and Graph, which is no filter - the view is the
// whole tree the outline read answers, every node of it unfolded. Graph draws every node;
// Conversation draws Conversation's read, which holds no result and no plan task, so a result or
// a plan task the tree holds - every one, before the read takes `?kind=` - is no row of it.
test("with no filter on the view is the whole tree, every node of it unfolded", () => {
  const root = issue();
  for (const preset of ["conversation", "graph"] as const) {
    const view = presetView(root, preset, "");
    expect(view.rootRow).toBe(true);
    expect(parentLines(view.root)).toEqual({});
  }
  // Exactly the rows the page draws with nothing narrowing it: every node.
  expect(drawn(root, "graph")).toEqual(nodesUnder(root).map((one) => one.id));
  expect(drawn(root, "graph")).toEqual(idsOf(treeRows(root)));
  const conversation = presetView(root, "conversation", "").root;
  expect([...new Set(nodesUnder(conversation).map((one) => one.type))].sort()).toEqual([
    "ask",
    "check",
    "comment",
    "decision",
    "issue",
    "requirement",
  ]);
});

/** Every node under `root`, `root` first, each before its children. */
function nodesUnder(root: TreeNode): TreeNode[] {
  return [root, ...root.children.flatMap(nodesUnder)];
}

// R10, R12: Graph is a view and no filter: it draws the whole tree as the page with no filter on
// does, the issue its first row, every branch as the fold leaves it and every node of every kind,
// and it has no count of its own among the filters' signals.
test("Graph is no filter", () => {
  const root = issue();
  expect(isFilter("graph")).toBe(false);
  expect(isFilter("conversation")).toBe(false);
  expect(PRESET_ORDER.filter(isFilter)).toEqual(["spec", "verification", "plan", "decisions"]);
  const view = presetView(root, "graph", "");
  expect(view).toMatchObject({ lines: new Map(), rootRow: true });
  expect(nodesUnder(view.root).map((one) => one.id)).toEqual(nodesUnder(root).map((one) => one.id));
  expect(Object.keys(filterSignals(root, "")).sort()).toEqual([
    "decisions",
    "plan",
    "spec",
    "verification",
  ]);
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

// Graph, which draws every node, finds a plan task by its words with the chain above it as
// context; Conversation, which draws no plan task, finds none.
test("a filter by words with no filter on draws the path down to what it matched", () => {
  const root = issue();
  const view = presetView(root, "graph", "b-t2");
  expect([...view.context]).toEqual(["comment-1", "CORE-1"]);
  expect(drawn(root, "graph", "b-t2")).toEqual(["CORE-1", "comment-1", "plan#b-t2"]);
  expect(drawn(root, "conversation", "b-t2")).toEqual(["CORE-1"]);
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
  expect(idsOf(treeRows(view.root)).slice(1)).toEqual(["spec#b-r1", "spec#b-r2", "comment-1"]);
  // It is drawn and not counted: the count is the filter's own kind.
  expect(view.matches).toBe(2);
});

// R9: an address that names a node Conversation draws no row of opens the filter of its kind at
// it, and a row's kind tag opens a criterion or a statement in Spec, a check in Verification and
// a decision in Decisions.
test("a node Conversation draws no row of opens its filter, and a kind tag its own view", () => {
  const result = node("result-1", "result");
  expect(filterOfUndrawn(node("plan#b-t1", "plan_task"))).toBe("plan");
  expect(filterOfUndrawn(result)).toBe("verification");
  // A plan task Conversation draws as the ancestor of a row is a row of it.
  expect(filterOfUndrawn(node("plan#b-t1", "plan_task", [node("c1", "comment")]))).toBeUndefined();
  expect(filterOfUndrawn(node("comment-1", "comment"))).toBeUndefined();
  expect(tagFilter(node("spec#b-k1", "criterion"))).toBe("spec");
  expect(tagFilter(statement("spec#b-r1"))).toBe("spec");
  expect(tagFilter(node("plan#b-r1", "requirement"))).toBeUndefined();
  expect(tagFilter(node("v#b-c1", "check", [], { state: "passed" }))).toBe("verification");
  expect(tagFilter(node("plan#b-c1", "check"))).toBeUndefined();
  expect(tagFilter(node("d1", "decision"))).toBe("decisions");
  expect(tagFilter(node("ask-1", "ask"))).toBeUndefined();
  expect(tagFilter(result)).toBeUndefined();
});

// R9: a block Conversation's read holds no node of opens the filter whose held view answer holds
// it as one of that filter's own kinds, in the document its address names where it names one;
// where no held view holds it, no filter does.
test("a block Conversation's read does not hold opens the filter whose view answer holds it", () => {
  const placed = (slug: string) => ({ artifact_id: `artifact-${slug}`, position: 0, slug });
  const views = {
    plan: treeAnswer([
      treeNode("r1", "requirement", null, { document: placed("spec-md"), in: "spec-md" }),
      treeNode("t1", "plan_task", null, { document: placed("plan-md"), in: "plan-md" }),
    ]),
  };
  expect(filterHolding(views, { id: "t1" })).toBe("plan");
  expect(filterHolding(views, { document: "plan-md", id: "t1" })).toBe("plan");
  expect(filterHolding(views, { document: "spec-md", id: "t1" })).toBeUndefined();
  // A node the view holds as context of its own kinds is no node of that filter's.
  expect(filterHolding(views, { id: "r1" })).toBeUndefined();
  expect(filterHolding(undefined, { id: "t1" })).toBeUndefined();
  // Graph draws every node of its read, so a read the page holds answers a block of any kind.
  expect(answerHolds(views.plan, { id: "r1" })).toBe(true);
  expect(answerHolds(views.plan, { document: "plan-md", id: "r1" })).toBe(false);
  expect(answerHolds(undefined, { id: "r1" })).toBe(false);
});

/** Where an address whose `target` the tree holds no node of lands once its reads are held. */
function absent(target: Extract<AddressFallback, { kind: "absent" }>["target"]): AddressFallback {
  return { kind: "absent", target };
}

/** Where an address whose node the outline draws no row of opens: Graph at that node. */
const GRAPH_AT_NODE = { keepsNode: true, preset: "graph" } as const;

// R9: the outline alone reads a node address (`?node=`), Conversation's and Graph's, so one
// whose node Conversation draws no row of - a plan task Conversation's tree holds, or a node
// Conversation's read holds none of where the page holds Graph's read and it holds the node -
// goes to Graph's `?node=` of that node, which draws every node of its read (R10); a block
// address goes to its filter, else to Graph where the page holds Graph's read and it holds the
// block. A node Conversation draws a row of stays, and so does a node on Graph's own address
// that Graph's tree holds.
test("a node address Conversation draws no row of goes to Graph's ?node= of that node", () => {
  const placed = { artifact_id: "artifact-plan-md", position: 0, slug: "plan-md" };
  const graphRead = treeAnswer([
    treeNode("t9", "plan_task", null, { document: placed, in: "plan-md" }),
  ]);
  const nodeTarget = { id: "CORE-1/plan-md#t9", kind: "node" } as const;
  const blockTarget = { id: "t9", kind: "block" } as const;
  const on = (target: AddressTarget) => ({ preset: "conversation", target }) as const;
  const root = issue();
  for (const read of [undefined, graphRead]) {
    expect(
      addressView(on(nodeTarget), undefined, root, absent(nodeTarget), read),
      `a node Conversation's read holds none of, with Graph's read ${read === undefined ? "not " : ""}held`
    ).toEqual(read === undefined ? { keepsNode: false, preset: "conversation" } : GRAPH_AT_NODE);
  }
  expect(addressView(on(blockTarget), undefined, root, absent(blockTarget), graphRead)).toEqual(
    GRAPH_AT_NODE
  );
  expect(
    addressView(on(blockTarget), undefined, root, absent(blockTarget), undefined)
  ).toBeUndefined();
  // A plan task Conversation's tree holds is no row of it: its block's address opens Plan, and
  // its node's address Graph.
  const task = node("plan#b-t1", "plan_task", [], { readId: "CORE-1/plan#t1" });
  const toTask = [node("CORE-1", "issue", [task]), task];
  expect(addressView(on(blockTarget), toTask, root, undefined, graphRead)).toEqual({
    keepsNode: true,
    preset: "plan",
  });
  expect(addressView(on(nodeTarget), toTask, root, undefined, graphRead)).toEqual(GRAPH_AT_NODE);
  // A comment is a row of Conversation, which lands on it.
  const comment = node("comment-1", "comment", [], { readId: "comment-1" });
  const toComment = [node("CORE-1", "issue", [comment]), comment];
  expect(addressView(on(nodeTarget), toComment, root, undefined, graphRead)).toBeUndefined();
  // Graph's own address draws Graph, which lands on a node its tree holds.
  const graph = { preset: "graph", target: nodeTarget } as const;
  expect(addressView(graph, toTask, root, undefined, graphRead)).toBeUndefined();
});

// R9 in a filter's place: where a filter's view draws no row for the address's node, the stage
// draws the outline, Conversation's, in its place, and a node Conversation draws no row of opens
// the filter of its kind as on Conversation's address: a plan task under Verification or Spec
// opens Plan at it. The filter the outline stands in drew none of it, so that filter is never the
// one opened, and a node address, which the outline alone reads, opens nothing there.
test("in a filter's place an address of a node Conversation draws no row of opens the filter of its kind", () => {
  const placed = { artifact_id: "artifact-plan-md", position: 0, slug: "plan-md" };
  const views = {
    plan: treeAnswer([treeNode("t9", "plan_task", null, { document: placed, in: "plan-md" })]),
  };
  const root = node("CORE-1", "issue", [], { views });
  const block = { id: "t9", kind: "block" } as const;
  const on = (preset: Preset, target: AddressTarget) => ({ preset, target }) as const;
  const plan = { keepsNode: true, preset: "plan" } as const;
  expect(
    addressView(on("verification", block), undefined, root, absent(block), undefined),
    "a block only Plan's view answer holds, in Verification's place"
  ).toEqual(plan);
  expect(
    addressView(on("plan", block), undefined, root, absent(block), undefined),
    "a block only Plan's view answer holds, in Plan's own place"
  ).toBeUndefined();
  const task = node("plan#b-t1", "plan_task", [], { readId: "CORE-1/plan#t1" });
  const toTask = [node("CORE-1", "issue", [task]), task];
  expect(
    addressView(on("spec", block), toTask, root, undefined, undefined),
    "a plan task the tree holds, in Spec's place"
  ).toEqual(plan);
  expect(
    addressView(on("plan", block), toTask, root, undefined, undefined),
    "a plan task the tree holds, in Plan's own place"
  ).toBeUndefined();
  const unheld = { id: "CORE-1/plan-md#t404", kind: "node" } as const;
  expect(
    addressView(on("verification", unheld), undefined, root, absent(unheld), undefined),
    "a node address no read holds, in Verification's place"
  ).toBeUndefined();
});

// R9: an id no read holds opens the outline's own address without it, with no line: on
// Conversation's own address, Conversation's, whether the page holds no Graph read or holds one
// that lacks the node, and on Graph's own address Graph's. A node Conversation's read holds none
// of that Graph's read, as the page holds it, holds opens Graph at that node.
test("a node address no read holds opens the issue's own address with no line", () => {
  const placed = { artifact_id: "artifact-plan-md", position: 0, slug: "plan-md" };
  const graphRead = treeAnswer([
    treeNode("t9", "plan_task", null, { document: placed, in: "plan-md" }),
  ]);
  const graphHolds = { id: "CORE-1/plan-md#t9", kind: "node" } as const;
  const nobodyHolds = { id: "CORE-1/plan-md#t404", kind: "node" } as const;
  const on = (preset: Preset, target: AddressTarget) => ({ preset, target }) as const;
  const root = issue();
  const ownAddress = { keepsNode: false, preset: "conversation" } as const;
  expect(
    addressView(on("conversation", nobodyHolds), undefined, root, absent(nobodyHolds), undefined),
    "a node no read holds, with no Graph read held"
  ).toEqual(ownAddress);
  expect(
    addressView(on("conversation", nobodyHolds), undefined, root, absent(nobodyHolds), graphRead),
    "a node no read holds, with a Graph read held that lacks it"
  ).toEqual(ownAddress);
  expect(
    addressView(on("conversation", graphHolds), undefined, root, absent(graphHolds), graphRead),
    "a node Conversation's read lacks, with a Graph read held that holds it"
  ).toEqual(GRAPH_AT_NODE);
  expect(
    addressView(on("graph", nobodyHolds), undefined, root, absent(nobodyHolds), graphRead),
    "a node no read holds, on Graph's own address"
  ).toEqual({ keepsNode: false, preset: "graph" });
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

// R15: a count the page draws over the outline or a view counts the nodes loaded, and carries "+"
// while the read it is read from names a next page, since the page's `remaining` is not split by
// filter: a count read off the tree by the outline read's mark on the issue, and a held view's
// count, and the root the view draws, by that view's own read, whatever the outline read says.
// Decisions' flag of what waits for review is a count over the same read as its count, and
// carries the same "+" (INFRA-1196's architect, 13:39Z); the failing checks' flag carries none.
// Once no page is left, no count carries it.
test("a count carries + while a cursor remains", () => {
  expect(filterSignals({ ...issue(), moreToRead: true }, "")).toEqual({
    decisions: { count: 4, flag: { count: 1, more: true, word: "to_review" }, more: true },
    plan: { count: 2, more: true, unit: "steps" },
    spec: { count: 2, more: true },
    verification: { count: 1, flag: { count: 1, word: "failing" }, more: true },
  });
  const paged = treeAnswer([viewNode("t1", "plan_task", "query")], [], {
    converted: true,
    cursor: "page-2",
    view: "plan",
  });
  const whole: TreeAnswer = { ...paged, cursor: null };
  const outlineDone = { ...issue(), views: { plan: paged } };
  expect(filterSignals(outlineDone, "").plan).toEqual({ count: 1, more: true, unit: "steps" });
  expect(filterSignals(outlineDone, "").spec).toEqual({ count: 2 });
  expect(presetView(outlineDone, "plan", "").root.moreToRead).toBe(true);
  const viewDone = { ...issue(), moreToRead: true as const, views: { plan: whole } };
  expect(filterSignals(viewDone, "").plan).toEqual({ count: 1, unit: "steps" });
  expect(presetView(viewDone, "plan", "").root.moreToRead).toBeUndefined();
  expect(presetView(viewDone, "conversation", "").root.moreToRead).toBe(true);
  expect(filterSignals(issue(), "").spec).toEqual({ count: 2 });
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

// R2, R9: a person's decision is the answered question itself, so no decision node restates it,
// and a decision under an answered question is an agent's own that settles it: the Decisions
// count holds the answer and that decision each as an entry, as it holds a decision under a
// question nobody answered and one under the issue.
test("the Decisions count holds an agent's decision under an answered question as its own entry", () => {
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
  expect(filterSignals(root, "").decisions).toEqual({ count: 5 });
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

/** `root` with the tree read's states put on it by the read's own id, as the page puts them
 *  (`node-states.ts`): an overtaken node and every node under it are `closed`. Each node is read
 *  as the node of the tree read whose id is its own, where it carries no read id. */
function stamped(root: TreeNode, states: Readonly<Record<string, NodeStates>>): TreeNode {
  return stampStates(readAsIds(root), new Map(Object.entries(states)));
}

/** `root` with every node that carries no read id given its own id as one. */
function readAsIds(root: TreeNode): TreeNode {
  return { readId: root.id, ...root, children: root.children.map(readAsIds) };
}

// A filter control's count counts its query's own nodes, a proposed one and one at or under an
// overtaken node among them, under a words filter too. Only what asks something of the reader
// leaves them out (the next row).
test("a filter's count counts proposed and overtaken query nodes", () => {
  const root = stamped(
    node("CORE-1", "issue", [
      node("ask-1", "ask", [
        statement("spec#b-r1", [node("verification#b-c1", "check", [], { state: "failed" })]),
        node("plan#b-t1", "plan_task"),
      ]),
      statement("spec#b-r2", [
        node("verification#b-c2", "check", [], { state: "failed" }),
        node("plan#b-t2", "plan_task"),
      ]),
      node("decision-1", "decision", [], { state: "waiting_review", waiting: true }),
      node("decision-2", "decision", [], { state: "kept" }),
    ]),
    {
      "ask-1": { life: "overtaken" },
      "decision-1": { life: "proposed" },
      "decision-2": { life: "overtaken" },
      "plan#b-t2": { life: "proposed" },
      "spec#b-r2": { life: "proposed" },
      "verification#b-c2": { life: "proposed" },
    }
  );

  expect(presetView(root, "spec", "").matches).toBe(2);
  expect(presetView(root, "verification", "").matches).toBe(2);
  expect(presetView(root, "plan", "").matches).toBe(2);
  expect(presetView(root, "decisions", "").matches).toBe(3);
  const signals = filterSignals(root, "");
  expect(signals.spec.count).toBe(2);
  expect(signals.verification.count).toBe(2);
  expect(signals.plan.count).toBe(2);
  expect(signals.decisions.count).toBe(3);
  // The query's own nodes: the proposed statement alone, and the one under the overtaken ask.
  expect(filterSignals(root, "b-r2").spec.count).toBe(1);
  expect(filterSignals(root, "b-r1").spec.count).toBe(1);
  expect(filterSignals(root, "b-t").plan.count).toBe(2);
});

// No count treats a proposed check as failing, nor a node at or under an overtaken one as open:
// the failing flag counts neither a proposed failed check nor one under an overtaken statement,
// and the to-review flag no overtaken decision that waits, nor one under an overtaken comment,
// which the Decisions filter lifts out from under it. The same tree with no states raises both
// flags, so the states are what take them away.
test("no flag counts them", () => {
  const live = node("CORE-1", "issue", [
    statement("spec#b-r1", [node("verification#b-c1", "check", [], { state: "failed" })]),
    statement("spec#b-r2", [node("verification#b-c2", "check", [], { state: "failed" })]),
    node("decision-1", "decision", [], { state: "waiting_review", waiting: true }),
    node("comment-1", "comment", [
      node("decision-2", "decision", [], { state: "waiting_review", waiting: true }),
    ]),
  ]);
  expect(filterSignals(live, "")).toEqual({
    decisions: { count: 2, flag: { count: 2, word: "to_review" } },
    plan: { count: 0, unit: "steps" },
    spec: { count: 2 },
    verification: { count: 2, flag: { count: 2, word: "failing" } },
  });

  const root = stamped(live, {
    "comment-1": { life: "overtaken" },
    "decision-1": { life: "overtaken" },
    "spec#b-r2": { life: "overtaken" },
    "verification#b-c1": { life: "proposed" },
  });
  expect(filterSignals(root, "")).toEqual({
    decisions: { count: 2 },
    plan: { count: 0, unit: "steps" },
    spec: { count: 2 },
    verification: { count: 2 },
  });
});

// The failing flag counts the checks the Verification summary counts: a live failed check that
// serves only a proposed statement, which no count reads, is no failing check of the flag, while
// one that serves a live statement still is.
test("the failing flag leaves out a failed check that serves only a proposed statement", () => {
  const root = stamped(
    node("CORE-1", "issue", [
      statement("spec#b-r1", [node("verification#b-c1", "check", [], { state: "failed" })]),
      statement("spec#b-r2", [node("verification#b-c2", "check", [], { state: "failed" })]),
    ]),
    { "spec#b-r1": { life: "proposed" } }
  );
  expect(filterSignals(root, "").verification.flag).toEqual({ count: 1, word: "failing" });
});

// What asks the reader stays. A proposed decision that waits for review is on the review list as
// today, so the to-review flag counts it.
test("a proposed decision waiting for review is still on the to-review flag", () => {
  const root = stamped(
    node("CORE-1", "issue", [
      node("decision-1", "decision", [], { state: "waiting_review", waiting: true }),
      node("decision-2", "decision", [], { state: "waiting_review", waiting: true }),
    ]),
    { "decision-1": { life: "proposed" }, "decision-2": { life: "overtaken" } }
  );
  expect(filterSignals(root, "").decisions).toEqual({
    count: 2,
    flag: { count: 1, word: "to_review" },
  });
});

// A filter that draws a node's parent as one line on its row, with no view read held, draws that
// parent's marks there too, as its own row would carry them. A parent with no mark carries none.
test("a filter's parent line carries the marks of the parent it names", () => {
  const root = stamped(
    node("CORE-1", "issue", [
      node("ask-1", "ask", [statement("spec#b-r1")]),
      node("ask-2", "ask", [statement("spec#b-r2")]),
      node("ask-3", "ask", [statement("spec#b-r3")]),
    ]),
    { "ask-1": { life: "proposed" }, "ask-2": { onlyYou: true } }
  );
  const lines = new Map<string, TreeNode["parentLine"]>();
  const walk = (at: TreeNode): void => {
    if (at.parentLine !== undefined) lines.set(at.id, at.parentLine);
    for (const child of at.children) walk(child);
  };
  walk(presetView(root, "spec", "").root);
  expect(lines.get("spec#b-r1")?.life).toBe("proposed");
  expect(lines.get("spec#b-r1")?.onlyYou).toBeUndefined();
  expect(lines.get("spec#b-r2")?.onlyYou).toBe(true);
  expect(lines.get("spec#b-r2")?.life).toBeUndefined();
  expect(lines.get("spec#b-r3")).toBeDefined();
  expect("life" in (lines.get("spec#b-r3") ?? {})).toBe(false);
  expect("onlyYou" in (lines.get("spec#b-r3") ?? {})).toBe(false);
});

/** One node of a view read, the query's or context, under the issue unless the case names
 *  another parent, its words its name. */
function viewNode(
  id: string,
  supertag: string,
  role: TreeAnswerNode["role"],
  {
    parent = "CORE-1",
    ...extra
  }: TreeNodeExtra & { readonly parent?: string | IssueTreeParent | null } = {}
): TreeAnswerNode {
  return treeNode(id, supertag, parent, { role, text: id, ...extra });
}

/** The spec's statement a1 as a view names the parent of a node the view holds but not that
 *  parent: by its store id, kind, supertag and first line. */
const SPEC_STATEMENT: IssueTreeParent = {
  first_line: "The spec's statement.",
  id: "CORE-1/spec#a1",
  kind: "block",
  supertag: "requirement",
};

// Once the Spec view read is held on the issue's node, Spec draws the nodes its query took of
// R2's kinds, each as the tree's node of that id. The memo callout the
// view answers first is no row, and a decision and a constraint the view answers as context are
// never rows: each is a line of the row its tie names, a decision by its `shaped` tuple and a
// constraint by the row's `constrained_by` tuple. The control counts the view's own statements.
test("a view answer's decisions and constraints are lines, never rows", () => {
  const spec: TreeAnswer = treeAnswer(
    [
      viewNode("summary", "callout", "query", { in: "spec" }),
      viewNode("r1", "requirement", "query", { in: "spec" }),
      viewNode("r2", "requirement", "query", { in: "spec" }),
      viewNode("d1", "decision", "context", { in: "decisions-md", text: "Keep the model pure." }),
      viewNode("k1", "constraint", "context", { text: "Ship on Fridays only." }),
    ],
    [treeTuple("shaped", "d1", "r1"), treeTuple("constrained_by", "r1", "k1")],
    { converted: true, tree: "artifact-tree", view: "spec" }
  );
  const root = node(
    "CORE-1",
    "issue",
    [
      node("spec#b-summary", "callout", [], { readId: "CORE-1/spec#summary" }),
      node("spec#b-r1", "requirement", [], { readId: "CORE-1/spec#r1" }),
      node("spec#b-r2", "requirement", [], { readId: "CORE-1/spec#r2" }),
      node("decisions-md#b-d1", "decision", [], { readId: "CORE-1/decisions-md#d1" }),
    ],
    { views: { spec } }
  );

  expect(drawn(root, "spec")).toEqual(["spec#b-r1", "spec#b-r2"]);
  expect([...presetView(root, "spec", "").lines]).toEqual([
    [
      "spec#b-r1",
      [
        { id: "d1", text: "Keep the model pure.", type: "decision" },
        { id: "k1", text: "Ship on Fridays only.", type: "constraint" },
      ],
    ],
  ]);
  expect(filterSignals(root, "").spec.count).toBe(2);
});

/** The Spec view read holding the statements r1 and r2, and the tree its own answer builds
 *  (`TreeViews.trees`), each statement worded as the view's. */
function specViewOf(): { readonly answer: TreeAnswer; readonly tree: TreeNode } {
  return {
    answer: treeAnswer(
      [
        viewNode("r1", "requirement", "query", { in: "spec" }),
        viewNode("r2", "requirement", "query", { in: "spec" }),
      ],
      [],
      { view: "spec" }
    ),
    tree: node("CORE-1", "issue", [
      statement("spec#b-r1", [], { readId: "CORE-1/spec#r1", text: "The view's r1." }),
      statement("spec#b-r2", [], { readId: "CORE-1/spec#r2", text: "The view's r2." }),
    ]),
  };
}

/** Each row `preset` draws of `root`, by its id, with its words. */
function rowWords(root: TreeNode, preset: Preset): [string, string][] {
  return treeRows(presetView(root, preset, "").root)
    .slice(1)
    .flatMap((row): [string, string][] =>
      row.kind === "node" ? [[row.node.id, row.node.text]] : []
    );
}

// R4, R5: a filter draws its view read's rows whatever the outline read holds. A node of its view
// answer the outline's tree holds no node of is drawn as the node of the tree the view's answer
// builds alone (`TreeViews.trees`): with the outline read not sent, as on Spec loaded cold below
// 1024 px (R13), the outline's tree is the issue alone and Spec still draws each statement.
test("a filter draws a node its view answer holds that the outline's tree lacks", () => {
  const { answer, tree } = specViewOf();
  const cold = node("CORE-1", "issue", [], { views: { spec: answer, trees: { spec: tree } } });
  expect(rowWords(cold, "spec")).toEqual([
    ["spec#b-r1", "The view's r1."],
    ["spec#b-r2", "The view's r2."],
  ]);
});

// R4: where the outline's tree and the tree the view's answer builds both hold a node, the filter
// draws the outline's, so a row reads as it does in the outline; the view's own tree gives the
// node the outline's lacks.
test("a filter draws the outline's node where the outline's tree and its view's both hold it", () => {
  const { answer, tree } = specViewOf();
  const root = node(
    "CORE-1",
    "issue",
    [statement("spec#b-r1", [], { readId: "CORE-1/spec#r1", text: "The outline's r1." })],
    { views: { spec: answer, trees: { spec: tree } } }
  );
  expect(rowWords(root, "spec")).toEqual([
    ["spec#b-r1", "The outline's r1."],
    ["spec#b-r2", "The view's r2."],
  ]);
});

// R4 and R9: Conversation's read holds no plan task, so the Plan filter draws each task from the
// tree its own view answer builds, where the names read gave no words to the chip of a statement
// the task also serves: the row names that statement by its own words, from the tree that holds
// it.
test("a view's row drawn from its own tree names each other parent it serves by that parent's words", () => {
  const second = "dispatch://CORE-1/spec#b-r2";
  const task = node("plan#b-t1", "plan_task", [], {
    alsoUnder: [{ answered: false, gone: false, ref: second }],
    readId: "CORE-1/plan#t1",
  });
  const tree = node("CORE-1", "issue", [
    statement("spec#b-r1", [task], { readId: "CORE-1/spec#r1", text: "The first statement." }),
    statement("spec#b-r2", [], {
      readId: "CORE-1/spec#r2",
      reference: second,
      text: "The second statement.",
    }),
  ]);
  const answer = treeAnswer([viewNode("t1", "plan_task", "query", { in: "plan" })], [], {
    view: "plan",
  });
  const root = node("CORE-1", "issue", [], { views: { plan: answer, trees: { plan: tree } } });
  const drawn = presetView(root, "plan", "").root.children.find((row) => row.id === "plan#b-t1");
  expect(
    drawn?.alsoUnder.map(nameOf),
    "the task does not name the second statement it serves"
  ).toEqual(["The second statement."]);
});

// R19: the Plan view's read answers each task's size in whole minutes, from the task's `size`
// attribute (`plan.size`), and on an issue with issues under it the tasks of every issue under it
// beside the issue's own, each marked with its issue, as context. Each is a row of Plan, with its
// size where it holds one and its issue where it is another issue's, and the Plan control counts
// them all; a size that is no whole number of minutes is none. The view hands the issue's own
// tasks first, then each other issue's in the order its document writes them, the written order
// the plan's walk starts from. No other view takes another issue's node as a row.
test("a plan task's size and issue come from the plan view's answer", () => {
  const plan = (size: string | null) => ({
    cycle: false,
    docs: null,
    needs: [],
    position: 0,
    size,
    skills: null,
  });
  const answer = treeAnswer(
    [
      viewNode("t1", "plan_task", "query", { in: "plan", plan: plan("90") }),
      viewNode("x1", "plan_task", "context", {
        document: { artifact_id: "artifact-child-plan", position: 1, slug: "plan" },
        issue: "CORE-2",
        parent: "CORE-2",
        plan: plan("45"),
      }),
      viewNode("x0", "plan_task", "context", {
        document: { artifact_id: "artifact-child-plan", position: 0, slug: "plan" },
        issue: "CORE-2",
        parent: "CORE-2",
        plan: plan(null),
      }),
      viewNode("t2", "plan_task", "query", { in: "plan", plan: plan(null) }),
      viewNode("t3", "plan_task", "query", { in: "plan", plan: plan("soon") }),
    ],
    [],
    { view: "plan" }
  );
  const tree = node("CORE-1", "issue", [
    node("plan#b-t1", "plan_task", [], { readId: "CORE-1/plan#t1" }),
    node("plan#b-t2", "plan_task", [], { readId: "CORE-1/plan#t2" }),
    node("plan#b-t3", "plan_task", [], { readId: "CORE-1/plan#t3" }),
    node("x1", "plan_task", [], { readId: "CORE-2/plan#x1" }),
    node("x0", "plan_task", [], { readId: "CORE-2/plan#x0" }),
  ]);
  const root = node("CORE-1", "issue", [], { views: { plan: answer, trees: { plan: tree } } });
  expect(
    presetView(root, "plan", "").root.children.map((row) => [row.id, row.size, row.issue])
  ).toEqual([
    ["plan#b-t1", 90, undefined],
    ["plan#b-t2", undefined, undefined],
    ["plan#b-t3", undefined, undefined],
    ["x0", undefined, "CORE-2"],
    ["x1", 45, "CORE-2"],
  ]);
  expect(filterSignals(root, "").plan.count).toBe(5);
  const verification = treeAnswer(
    [viewNode("c9", "check", "context", { in: "verification", issue: "CORE-2", parent: "CORE-2" })],
    [],
    { view: "verification" }
  );
  const checks = node("CORE-1", "issue", [], {
    views: {
      trees: {
        verification: node("CORE-1", "issue", [
          node("c9", "check", [], { readId: "CORE-2/verification#c9", state: "no-result" }),
        ]),
      },
      verification,
    },
  });
  expect(presetView(checks, "verification", "").root.children).toEqual([]);
});

// Two documents of an issue that has not converted can hold one block id, and each block is a row
// of its own document's id: a filter draws the view's node as the tree's node of that same
// document, never as the other document's block that shares its read id, though the tree holds
// that one first.
test("a view draws its own document's block where two documents share a block id", () => {
  const view = (supertag: string, slug: string): TreeAnswer =>
    treeAnswer(
      [
        viewNode("a1", supertag, "query", {
          document: { artifact_id: `artifact-${slug}`, position: 0, slug },
        }),
      ],
      [],
      { view: slug === "spec" ? "spec" : "plan" }
    );
  const tree = (views: TreeNode["views"]) =>
    node(
      "CORE-1",
      "issue",
      [
        node("plan-md#b-a1", "plan_task", [], {
          readId: "CORE-1/plan-md#a1",
          text: "The plan's step.",
        }),
        node("spec#b-a1", "requirement", [], {
          readId: "CORE-1/spec#a1",
          text: "The spec's statement.",
        }),
      ],
      { views }
    );

  expect(drawn(tree({ spec: view("requirement", "spec") }), "spec")).toEqual(["spec#b-a1"]);
  expect(drawn(tree({ plan: view("plan_task", "plan-md") }), "plan")).toEqual(["plan-md#b-a1"]);
});

// Two documents share the block id a1: a plan task t1 hangs under the spec's statement a1, and
// the plan holds a task a1 of its own. The Plan view names t1's parent by its store id, kind and
// first line (`parent`), so Plan draws t1 as a row of its own with the statement as its line, as
// the outline places it, never nested under the plan's task a1.
test("a view places a node by the parent kind its view names where two documents share the parent's id", () => {
  const plan: TreeAnswer = treeAnswer(
    [
      viewNode("a1", "plan_task", "query", {
        document: { artifact_id: "artifact-plan-md", position: 0, slug: "plan-md" },
      }),
      viewNode("t1", "plan_task", "query", {
        document: { artifact_id: "artifact-plan-md", position: 1, slug: "plan-md" },
        parent: SPEC_STATEMENT,
      }),
    ],
    [],
    { view: "plan" }
  );
  const root = node(
    "CORE-1",
    "issue",
    [
      node(
        "spec#b-a1",
        "requirement",
        [
          node("plan-md#b-t1", "plan_task", [], {
            readId: "CORE-1/plan-md#t1",
            text: "The second step.",
          }),
        ],
        { readId: SPEC_STATEMENT.id, text: "The spec's statement." }
      ),
      node("plan-md#b-a1", "plan_task", [], {
        readId: "CORE-1/plan-md#a1",
        text: "The plan's step.",
      }),
    ],
    { views: { plan } }
  );

  const rows = presetView(root, "plan", "").root.children;
  expect({
    line: rows.find((row) => row.id === "plan-md#b-t1")?.parentLine?.type,
    rows: rows.map((row) => [row.id, row.children.map((child) => child.id)]),
  }).toEqual({
    line: "requirement",
    rows: [
      ["plan-md#b-a1", []],
      ["plan-md#b-t1", []],
    ],
  });
});

// The Plan view holds no node of t1's parent, the spec's statement a1, and the tree holds the
// plan's task a1 before it. The view names that parent by its store id beside its kind
// (`parent`), so t1's row carries the statement's line, never the line of the plan's task the
// tree holds first.
test("a view's row carries the line of the parent of the kind its view names where the tree holds another node of its id first", () => {
  const plan: TreeAnswer = treeAnswer(
    [
      viewNode("t1", "plan_task", "query", {
        document: { artifact_id: "artifact-plan-md", position: 1, slug: "plan-md" },
        parent: SPEC_STATEMENT,
      }),
    ],
    [],
    { view: "plan" }
  );
  const root = node(
    "CORE-1",
    "issue",
    [
      node("plan-md#b-a1", "plan_task", [], {
        readId: "CORE-1/plan-md#a1",
        text: "The plan's step.",
      }),
      node(
        "spec#b-a1",
        "requirement",
        [
          node("plan-md#b-t1", "plan_task", [], {
            readId: "CORE-1/plan-md#t1",
            text: "The second step.",
          }),
        ],
        { readId: SPEC_STATEMENT.id, text: "The spec's statement." }
      ),
    ],
    { views: { plan } }
  );

  expect(
    presetView(root, "plan", "").root.children.map((row) => [row.id, row.parentLine?.id])
  ).toEqual([["plan-md#b-t1", "spec#b-a1"]]);
});

// The page holds the tree read and a filter's view read as each answers, in either order, so a
// filter is drawn first from either, and what it draws for the node an address names must not
// turn on which answered first. A node the address names that is no row of the filter stands
// under the nearest row of the filter above it in the tree, and at the top only where no row
// stands above it, whether the filter is drawn from its view read or off the tree: under
// Verification a plan task the tree places under a statement stands under that statement, where
// the view draws the statement's checks alone, never at the top as a row of its own.
test("a node the address names stands where the tree places it, whether its filter's view read is held or not", () => {
  const verification: TreeAnswer = treeAnswer(
    [
      viewNode("r1", "requirement", "context", { in: "spec" }),
      viewNode("c1", "check", "query", { in: "verification" }),
    ],
    [treeTuple("serves", "c1", "r1")],
    { view: "verification" }
  );
  const tree = (views: TreeNode["views"]) =>
    node(
      "CORE-1",
      "issue",
      [
        statement(
          "spec#b-r1",
          [
            node("verification#b-c1", "check", [], {
              readId: "CORE-1/verification#c1",
              state: "failed",
            }),
            node("plan#b-t1", "plan_task", [], { readId: "CORE-1/plan#t1" }),
          ],
          { readId: "CORE-1/spec#r1" }
        ),
        node("comment-1", "comment", [
          node("plan#b-t2", "plan_task", [], { readId: "CORE-1/plan#t2" }),
        ]),
      ],
      views === undefined ? {} : { views }
    );
  // Each top row by its id, the ids of the rows under it, and the id of its parent's line.
  const placed = (root: TreeNode, id: string) =>
    presetView(root, "verification", "", new Set([id])).root.children.map((row) => [
      row.id,
      row.children.map((child) => child.id),
      row.parentLine?.id,
    ]);

  expect(placed(tree({ verification }), "plan#b-t1")).toEqual([
    ["spec#b-r1", ["verification#b-c1", "plan#b-t1"], undefined],
  ]);
  expect(placed(tree({ verification }), "plan#b-t2")).toEqual([
    ["spec#b-r1", ["verification#b-c1"], undefined],
    ["plan#b-t2", [], "comment-1"],
  ]);
  for (const id of ["plan#b-t1", "plan#b-t2"]) {
    expect(placed(tree({ verification }), id)).toEqual(placed(tree(undefined), id));
  }
});

// A node the address names that no row of the view stands above is a row of the view's top, at
// its place in the tree's order, whichever read lands first: the view must not move it after
// the view's own rows once its read lands. Here the tree walks an ask holding the named comment
// before the statement r1.
test("a node the address names with no row above it keeps the tree's order among the view's top rows, whether its filter's view read is held or not", () => {
  const spec: TreeAnswer = treeAnswer(
    [viewNode("r1", "requirement", "query", { in: "spec" })],
    [],
    {
      view: "spec",
    }
  );
  const tree = (views: TreeNode["views"]) =>
    node(
      "CORE-1",
      "issue",
      [
        node("ask-1", "ask", [node("comment-x", "comment", [], { readId: "comment-x" })], {
          readId: "ask-1",
        }),
        statement("spec#b-r1", [], { readId: "CORE-1/spec#r1" }),
      ],
      views === undefined ? {} : { views }
    );
  const top = (root: TreeNode) =>
    presetView(root, "spec", "", new Set(["comment-x"])).root.children.map((row) => [
      row.id,
      row.parentLine?.id,
    ]);

  expect(top(tree(undefined))).toEqual([
    ["comment-x", "ask-1"],
    ["spec#b-r1", undefined],
  ]);
  expect(top(tree({ spec })), "the named comment's place once the Spec view read is held").toEqual(
    top(tree(undefined))
  );
});

/** Each top row `preset` draws with the address naming `id`, nested: its id, the id of its
 *  parent's line, and the rows under it. */
type Shape = [string, string | undefined, Shape[]];
function shapeOf(root: TreeNode, preset: Preset, id: string): Shape[] {
  const shape = (row: TreeNode): Shape => [row.id, row.parentLine?.id, row.children.map(shape)];
  return presetView(root, preset, "", new Set([id])).root.children.map(shape);
}

// Verification draws a node at every placement the tree has, so a node the address names that
// the tree places under two statements, the second a transclusion, stands under both whichever
// read lands first: the view read must not drop its second placement.
test("under Verification a node the address names stands under each statement the tree places it under, whether its view read is held or not", () => {
  const verification: TreeAnswer = treeAnswer(
    [
      viewNode("r1", "requirement", "context", { in: "spec" }),
      viewNode("r2", "requirement", "context", { in: "spec" }),
      viewNode("c1", "check", "query", { in: "verification" }),
    ],
    [treeTuple("serves", "c1", "r1"), treeTuple("serves", "c1", "r2")],
    { view: "verification" }
  );
  const check = (transcluded: boolean) =>
    node("verification#b-c1", "check", [], {
      readId: "CORE-1/verification#c1",
      state: "failed",
      transcluded,
    });
  const task = (transcluded: boolean) =>
    node("plan#b-t1", "plan_task", [], { readId: "CORE-1/plan#t1", transcluded });
  const tree = (views: TreeNode["views"]) =>
    node(
      "CORE-1",
      "issue",
      [
        statement("spec#b-r1", [check(false), task(false)], { readId: "CORE-1/spec#r1" }),
        statement("spec#b-r2", [check(true), task(true)], { readId: "CORE-1/spec#r2" }),
      ],
      views === undefined ? {} : { views }
    );

  expect(shapeOf(tree(undefined), "verification", "plan#b-t1")).toEqual([
    [
      "spec#b-r1",
      undefined,
      [
        ["verification#b-c1", undefined, []],
        ["plan#b-t1", undefined, []],
      ],
    ],
    [
      "spec#b-r2",
      undefined,
      [
        ["verification#b-c1", undefined, []],
        ["plan#b-t1", undefined, []],
      ],
    ],
  ]);
  expect(
    shapeOf(tree({ verification }), "verification", "plan#b-t1"),
    "the named task's places once the Verification view read is held"
  ).toEqual(shapeOf(tree(undefined), "verification", "plan#b-t1"));
});

// A node the address names stands at each place the tree has it, whichever read lands first.
// Here the tree places the plan task t1 first under a comment, with no row above it, and again
// under the statement r2: Verification draws it at the top with the comment as its line, and
// under r2 after r2's check.
test("a node the address names stands at each place the tree has it, the first with no row above it, whether its filter's view read is held or not", () => {
  const verification: TreeAnswer = treeAnswer(
    [
      viewNode("r2", "requirement", "context", { in: "spec" }),
      viewNode("c1", "check", "query", { in: "verification" }),
    ],
    [treeTuple("serves", "c1", "r2")],
    { view: "verification" }
  );
  const tree = (views: TreeNode["views"]) =>
    node(
      "CORE-1",
      "issue",
      [
        node("comment-1", "comment", [
          node("plan#b-t1", "plan_task", [], { readId: "CORE-1/plan#t1" }),
        ]),
        statement(
          "spec#b-r2",
          [
            node("verification#b-c1", "check", [], {
              readId: "CORE-1/verification#c1",
              state: "failed",
            }),
            node("plan#b-t1", "plan_task", [], { readId: "CORE-1/plan#t1", transcluded: true }),
          ],
          { readId: "CORE-1/spec#r2" }
        ),
      ],
      views === undefined ? {} : { views }
    );

  expect(shapeOf(tree(undefined), "verification", "plan#b-t1")).toEqual([
    ["plan#b-t1", "comment-1", []],
    [
      "spec#b-r2",
      undefined,
      [
        ["verification#b-c1", undefined, []],
        ["plan#b-t1", undefined, []],
      ],
    ],
  ]);
  expect(
    shapeOf(tree({ verification }), "verification", "plan#b-t1"),
    "the named task's places once the Verification view read is held"
  ).toEqual(shapeOf(tree(undefined), "verification", "plan#b-t1"));
});

// Under a row, a node the address names keeps the tree's order among that row's rows, whichever
// read lands first. Here the tree walks the plan task t1 before the check c1 under the statement
// r1, so Verification draws t1 before c1, not after the rows the view read places there.
test("a node the address names under a row keeps the tree's order among that row's rows, whether its filter's view read is held or not", () => {
  const verification: TreeAnswer = treeAnswer(
    [
      viewNode("r1", "requirement", "context", { in: "spec" }),
      viewNode("c1", "check", "query", { in: "verification" }),
    ],
    [treeTuple("serves", "c1", "r1")],
    { view: "verification" }
  );
  const tree = (views: TreeNode["views"]) =>
    node(
      "CORE-1",
      "issue",
      [
        statement(
          "spec#b-r1",
          [
            node("plan#b-t1", "plan_task", [], { readId: "CORE-1/plan#t1" }),
            node("verification#b-c1", "check", [], {
              readId: "CORE-1/verification#c1",
              state: "failed",
            }),
          ],
          { readId: "CORE-1/spec#r1" }
        ),
      ],
      views === undefined ? {} : { views }
    );

  expect(shapeOf(tree(undefined), "verification", "plan#b-t1")).toEqual([
    [
      "spec#b-r1",
      undefined,
      [
        ["plan#b-t1", undefined, []],
        ["verification#b-c1", undefined, []],
      ],
    ],
  ]);
  expect(
    shapeOf(tree({ verification }), "verification", "plan#b-t1"),
    "the named task's place once the Verification view read is held"
  ).toEqual(shapeOf(tree(undefined), "verification", "plan#b-t1"));
});

// Under Spec and Plan the tree's lift stops at a transclusion, and a row the view read draws
// stands at its first placement in the tree's order, which can be one. A node the address names
// under that row stands under it all the same, whichever read lands first. Here the tree
// transcludes the plan task t1 under the statement r1 before t1's own place under the issue, with
// the named comment under each placement.
test("a node the address names under a row whose first placement is a transclusion stands under that row, whether its filter's view read is held or not", () => {
  const plan: TreeAnswer = treeAnswer([viewNode("t1", "plan_task", "query", { in: "plan" })], [], {
    view: "plan",
  });
  const task = (transcluded: boolean) =>
    node(
      "plan#b-t1",
      "plan_task",
      [node("comment-x", "comment", [], { readId: "comment-x", transcluded })],
      { readId: "CORE-1/plan#t1", transcluded }
    );
  const tree = (views: TreeNode["views"]) =>
    node(
      "CORE-1",
      "issue",
      [statement("spec#b-r1", [task(true)], { readId: "CORE-1/spec#r1" }), task(false)],
      views === undefined ? {} : { views }
    );

  expect(shapeOf(tree(undefined), "plan", "comment-x")).toEqual([
    ["plan#b-t1", undefined, [["comment-x", undefined, []]]],
  ]);
  expect(
    shapeOf(tree({ plan }), "plan", "comment-x"),
    "the named comment's place once the Plan view read is held"
  ).toEqual(shapeOf(tree(undefined), "plan", "comment-x"));
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
