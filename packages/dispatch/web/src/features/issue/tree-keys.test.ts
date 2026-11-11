import { expect, test } from "bun:test";

import type { TreeNode } from "./issue-tree";
import type { NodeType } from "./page-words";
import { pathTo, treeRows, zoomedRows } from "./tree-fold";
import {
  decisionBehind,
  everythingOpened,
  nextQuestion,
  nodesInOrder,
  rowKeyOf,
} from "./tree-keys";

/**
 * What the tree's keyboard asks of the model (dispatch://INFRA-1196 R9,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t15`), over a tree built by hand.
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

/** An issue whose spec holds two open questions and one answered one, under a document the
 *  reader has not opened, with a decision naming the first requirement. */
function issue(): TreeNode {
  return node("CORE-1", "issue", [
    node("spec", "document", [
      node("spec#b-q1", "ask", [], { question: true, reference: "dispatch://CORE-1/ask/one" }),
      node("spec#b-r1", "requirement", [], { reference: "dispatch://CORE-1/spec#b-r1" }),
      node("spec#b-q2", "ask", [], { question: true }),
      node("spec#b-answered", "ask"),
    ]),
    node("decisions-md#b-d1", "decision", [], {
      chips: [{ answered: true, gone: false, name: "R1", ref: "dispatch://CORE-1/spec#b-r1@v3" }],
    }),
    node("decisions-md#b-d2", "decision", [], {
      chips: [{ answered: true, gone: false, name: "R1", ref: "dispatch://CORE-1/spec#b-r1" }],
    }),
  ]);
}

test("the nodes read in the order the rows are drawn, the root first", () => {
  expect(nodesInOrder(issue()).map((one) => one.id)).toEqual([
    "CORE-1",
    "spec",
    "spec#b-q1",
    "spec#b-r1",
    "spec#b-q2",
    "spec#b-answered",
    "decisions-md#b-d1",
    "decisions-md#b-d2",
  ]);
});

test("the next-question key walks the open questions and reports past the last", () => {
  const root = issue();
  // From nowhere, and from a node that is no question, the key enters at the first question.
  expect(nextQuestion(root, undefined)?.id).toBe("spec#b-q1");
  expect(nextQuestion(root, "decisions-md#b-d1")?.id).toBe("spec#b-q1");
  // From a question, the next one; from the last, nothing, which the key reports.
  expect(nextQuestion(root, "spec#b-q1")?.id).toBe("spec#b-q2");
  expect(nextQuestion(root, "spec#b-q2")).toBeUndefined();
  // An answered question is no stop on the walk.
  expect(nextQuestion(node("CORE-1", "issue", [node("a", "ask")]), undefined)).toBeUndefined();
});

test("the decision behind a node is the first that names it, whatever version it pins", () => {
  const root = issue();
  expect(decisionBehind(root, "spec#b-r1")?.id).toBe("decisions-md#b-d1");
  expect(decisionBehind(root, "spec#b-q2")).toBeUndefined();
  expect(decisionBehind(root, "nothing-of-this-issue")).toBeUndefined();
});

test("the last state of the cycle opens every branch of the tree", () => {
  const opened = everythingOpened(issue());
  expect(opened.branches.has("spec")).toBe(true);
  expect(opened.deeper.has("spec")).toBe(true);
  expect(opened.branches.size).toBe(nodesInOrder(issue()).length);
});

test("a row is told from another by the path from the root down to it", () => {
  const one = node("spec#b-r1", "requirement");
  expect(rowKeyOf({ kind: "node", level: 2, node: one, trail: ["CORE-1"] })).toBe(
    "CORE-1>spec#b-r1"
  );
  expect(rowKeyOf({ kind: "node", level: 1, node: one, trail: [] })).toBe("spec#b-r1");
  expect(
    rowKeyOf({ held: 3, kind: "deeper", level: 4, of: one, trail: ["CORE-1", "spec#b-r2"] })
  ).toBe("deeper:CORE-1>spec#b-r2>spec#b-r1");
});

// R9: a check that serves two requirements is one node built under both (`issue-tree.ts`), so
// the rows below each copy draw the same nodes. Keyed by their direct parent those rows shared
// one key: React warned, the focus, the body and the pin matched both copies at once, and the
// tree had two rows carrying `tabIndex=0` instead of its one tab stop.
test("a node drawn under two requirements gives every row below it a key of its own", () => {
  const check = (): TreeNode =>
    node("verification-md#b-c1", "check", [node("res1", "result")], { state: "passed" });
  const root = node("CORE-1", "issue", [
    node("spec#b-r1", "requirement", [check()]),
    node("spec#b-r2", "requirement", [check()]),
  ]);

  const open = treeRows(root, everythingOpened(root)).map(rowKeyOf);
  expect(new Set(open).size).toBe(open.length);
  expect(open).toContain("CORE-1>spec#b-r1>verification-md#b-c1>res1");
  expect(open).toContain("CORE-1>spec#b-r2>verification-md#b-c1>res1");
  // Folded as the page opens, each copy's branch below the drawn levels is its own row too.
  const folded = treeRows(root).map(rowKeyOf);
  expect(new Set(folded).size).toBe(folded.length);
  expect(folded.filter((key) => key.startsWith("deeper:"))).toEqual([
    "deeper:CORE-1>spec#b-r1>verification-md#b-c1",
    "deeper:CORE-1>spec#b-r2>verification-md#b-c1",
  ]);
});

// R9: a reader who zooms into a node at 1440 px reads the tree from that node
// (`IssueTree.tsx`), and what a key holds - the pin, the open body, the tree's one tab stop -
// outlives the zoom in both directions. So the rows drawn from a node are keyed by the path
// from the issue, which the fold is seeded with, and not by the path from the zoom.
test("a row keeps its key when the tree is drawn from the node a zoom is on", () => {
  const root = node("CORE-1", "issue", [
    node("spec#b-r1", "requirement", [node("verification-md#b-c1", "check")]),
  ]);
  const above = pathTo(root, "spec#b-r1") ?? [];

  const whole = treeRows(root, everythingOpened(root)).map(rowKeyOf);
  const zoomed = treeRows(
    above.at(-1) ?? root,
    everythingOpened(root),
    above.slice(0, -1).map((one) => one.id)
  ).map(rowKeyOf);
  expect(zoomed).toEqual(["CORE-1>spec#b-r1", "CORE-1>spec#b-r1>verification-md#b-c1"]);
  expect(whole).toEqual(["CORE-1", ...zoomed]);
});

// R9: the zoomed tree hoists a match out of a context ancestor and carries the trail down to
// the first open question after the child the trail runs through (`zoomedRows`), so one node
// is reached both ways. Given one trail apiece those rows shared a key: over the tree below,
// `zoomedRows(root, undefined, new Set(["spec"]))` drew `spec#b-r1` and `spec#b-q1` twice,
// keyed `CORE-1>spec#b-r1` and `CORE-1>spec#b-q1` both times. Each row carries the path it is
// drawn under instead, so two placements are two keys.
test("every row of a zoomed drawing has a key of its own", () => {
  const under = node("spec#b-r1", "requirement", [
    node("spec#b-q1", "ask", [], { question: true, state: "open" }),
  ]);
  const root = node("CORE-1", "issue", [node("spec", "document", [under]), under]);

  const keys = zoomedRows(root, undefined, new Set(["spec"])).map(rowKeyOf);
  expect(new Set(keys).size).toBe(keys.length);
  expect(keys).toEqual([
    "CORE-1",
    "CORE-1>spec>spec#b-r1",
    "CORE-1>spec>spec#b-r1>spec#b-q1",
    "CORE-1>spec#b-r1",
    "CORE-1>spec#b-r1>spec#b-q1",
  ]);
});
