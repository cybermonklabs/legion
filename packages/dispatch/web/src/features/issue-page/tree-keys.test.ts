import { expect, test } from "bun:test";

import type { TreeNode } from "./issue-tree";
import type { NodeType } from "./page-words";
import { NOTHING_OPENED, pathTo, treeRows, withFold } from "./tree-fold";
import { decisionBehind, nextQuestion, nodesInOrder, rowKeyOf } from "./tree-keys";

/**
 * What the tree's keyboard asks of the model, over a tree built by hand.
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
 *  reader has not opened, with two decision blocks of the spec naming the first requirement. */
function issue(): TreeNode {
  return node("CORE-1", "issue", [
    node("spec", "document", [
      node("spec#b-q1", "ask", [], { question: true, reference: "dispatch://CORE-1/ask/one" }),
      node("spec#b-r1", "requirement", [], { reference: "dispatch://CORE-1/spec#b-r1" }),
      node("spec#b-q2", "ask", [], { question: true }),
      node("spec#b-answered", "ask"),
    ]),
    node("spec#b-d1", "decision", [], {
      chips: [
        {
          answered: true,
          gone: false,
          reading: { words: "R1" },
          ref: "dispatch://CORE-1/spec#b-r1@v3",
        },
      ],
    }),
    node("spec#b-d2", "decision", [], {
      chips: [
        {
          answered: true,
          gone: false,
          reading: { words: "R1" },
          ref: "dispatch://CORE-1/spec#b-r1",
        },
      ],
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
    "spec#b-d1",
    "spec#b-d2",
  ]);
});

test("the next-question key walks the open questions and reports past the last", () => {
  const root = issue();
  // From nowhere, and from a node that is no question, the key enters at the first question.
  expect(nextQuestion(root, undefined)?.id).toBe("spec#b-q1");
  expect(nextQuestion(root, "spec#b-d1")?.id).toBe("spec#b-q1");
  // From a question, the next one; from the last, nothing, which the key reports.
  expect(nextQuestion(root, "spec#b-q1")?.id).toBe("spec#b-q2");
  expect(nextQuestion(root, "spec#b-q2")).toBeUndefined();
  // An answered question is no stop on the walk.
  expect(nextQuestion(node("CORE-1", "issue", [node("a", "ask")]), undefined)).toBeUndefined();
});

test("the decision behind a node is the first that names it, whatever version it pins", () => {
  const root = issue();
  expect(decisionBehind(root, "spec#b-r1")?.id).toBe("spec#b-d1");
  expect(decisionBehind(root, "spec#b-q2")).toBeUndefined();
  expect(decisionBehind(root, "nothing-of-this-issue")).toBeUndefined();
});

// A person's decision is the answered question itself, whose node carries a chip for each node
// its `shaped` tuples point at (`tree-read/tree-nodes.ts`' `draftAsk`): the decision behind what
// it shaped is that question. A question still open, or one closed without an answer, decided
// nothing, so a chip it carries puts no decision behind anything.
test("the decision behind a node may be the answered question that shaped it", () => {
  const shaped = {
    answered: true,
    gone: false,
    reading: { words: "R1" },
    ref: "dispatch://CORE-1/spec#b-r1",
  };
  const root = node("CORE-1", "issue", [
    node("spec#b-open", "ask", [], { chips: [shaped], question: true, state: "open" }),
    node("spec#b-declined", "ask", [], { chips: [shaped], state: "declined" }),
    node("spec#b-r1", "requirement", [], { reference: "dispatch://CORE-1/spec#b-r1" }),
    node("ask-1", "ask", [], { chips: [shaped], state: "answered" }),
  ]);
  expect(decisionBehind(root, "spec#b-r1")?.id).toBe("ask-1");
});

// R10: a check reaches back through the chain it sits in. No decision names the check itself,
// so the decision key on it moves to the decision that informed the requirement it serves; a
// check under a requirement no decision informed, and a node of any other kind no decision
// names, have nowhere to go.
test("the decision behind a check is the one that informed the requirement it serves", () => {
  const check = node("verification-md#b-c1", "check", [node("result-1", "result")]);
  const unserved = node("verification-md#b-c2", "check");
  const root = node("CORE-1", "issue", [
    node("spec#b-r1", "requirement", [check], { reference: "dispatch://CORE-1/spec#b-r1" }),
    node("spec#b-r2", "requirement", [unserved], { reference: "dispatch://CORE-1/spec#b-r2" }),
    node("spec#b-d1", "decision", [], {
      chips: [
        {
          answered: true,
          gone: false,
          reading: { words: "R1" },
          ref: "dispatch://CORE-1/spec#b-r1",
        },
      ],
    }),
  ]);
  expect(decisionBehind(root, "verification-md#b-c1")?.id).toBe("spec#b-d1");
  expect(decisionBehind(root, "verification-md#b-c2")).toBeUndefined();
  expect(decisionBehind(root, "result-1")).toBeUndefined();
});

test("a row is told from another by the path from the root down to it", () => {
  const one = node("spec#b-r1", "requirement");
  expect(rowKeyOf({ kind: "node", level: 2, node: one, trail: ["CORE-1"] })).toBe(
    "CORE-1>spec#b-r1"
  );
  expect(rowKeyOf({ kind: "node", level: 1, node: one, trail: [] })).toBe("spec#b-r1");
  expect(
    rowKeyOf({ kind: "page", left: 7, level: 4, of: one, trail: ["CORE-1", "spec#b-r2"] })
  ).toBe("page:CORE-1>spec#b-r2>spec#b-r1");
});

// R9: a check that serves two requirements is one node built under both (`issue-tree.ts`), so
// the rows below each copy draw the same nodes. Keyed by their direct parent those rows shared
// one key: React warned, the focus and the editing matched both copies at once, and the tree
// had two rows carrying `tabIndex=0` instead of its one tab stop.
test("a node drawn under two requirements gives every row below it a key of its own", () => {
  const check = (): TreeNode =>
    node("verification-md#b-c1", "check", [node("res1", "result", [node("note", "callout")])], {
      state: "passed",
    });
  const root = node("CORE-1", "issue", [
    node("spec#b-r1", "requirement", [check()]),
    node("spec#b-r2", "requirement", [check()]),
  ]);

  const open = treeRows(root).map(rowKeyOf);
  expect(new Set(open).size).toBe(open.length);
  expect(open).toContain("CORE-1>spec#b-r1>verification-md#b-c1>res1");
  expect(open).toContain("CORE-1>spec#b-r2>verification-md#b-c1>res1");
  // Folded by the reader, the one node is folded under each copy, and each copy's row keeps a
  // key of its own.
  const folded = treeRows(root, withFold(NOTHING_OPENED, "res1", false)).map(rowKeyOf);
  expect(new Set(folded).size).toBe(folded.length);
  expect(folded.filter((key) => key.endsWith(">res1"))).toEqual([
    "CORE-1>spec#b-r1>verification-md#b-c1>res1",
    "CORE-1>spec#b-r2>verification-md#b-c1>res1",
  ]);
  expect(folded.some((key) => key.endsWith(">note"))).toBe(false);
});

// R11: a reader who zooms into a node reads the tree from that node (`IssueTree.tsx`), and what
// a key holds - the editing, the tree's one tab stop - outlives the zoom in both directions. So
// the rows drawn from a node are keyed by the path from the issue, which the fold is seeded
// with, and not by the path from the zoom.
test("a row keeps its key when the tree is drawn from the node a zoom is on", () => {
  const root = node("CORE-1", "issue", [
    node("spec#b-r1", "requirement", [node("verification-md#b-c1", "check")]),
  ]);
  const above = pathTo(root, "spec#b-r1") ?? [];

  const whole = treeRows(root).map(rowKeyOf);
  const zoomed = treeRows(
    above.at(-1) ?? root,
    NOTHING_OPENED,
    above.slice(0, -1).map((one) => one.id)
  ).map(rowKeyOf);
  expect(zoomed).toEqual(["CORE-1>spec#b-r1", "CORE-1>spec#b-r1>verification-md#b-c1"]);
  expect(whole).toEqual(["CORE-1", ...zoomed]);
});

// R9: one node placed under two parents is reached both ways, and each of those rows carries the
// path it is drawn under: two placements are two keys, never one key drawn twice.
test("every row of a node drawn under two parents has a key of its own", () => {
  const under = node("spec#b-r1", "requirement", [
    node("spec#b-q1", "ask", [], { question: true, state: "open" }),
  ]);
  const root = node("CORE-1", "issue", [node("spec", "document", [under]), under]);

  const keys = treeRows(root).map(rowKeyOf);
  expect(new Set(keys).size).toBe(keys.length);
  expect(keys).toEqual([
    "CORE-1",
    "CORE-1>spec",
    "CORE-1>spec>spec#b-r1",
    "CORE-1>spec>spec#b-r1>spec#b-q1",
    "CORE-1>spec#b-r1",
    "CORE-1>spec#b-r1>spec#b-q1",
  ]);
});
