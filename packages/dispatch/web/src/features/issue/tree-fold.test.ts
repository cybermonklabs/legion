import { expect, test } from "bun:test";

import type { TreeNode } from "./issue-tree";
import type { NodeType } from "./page-words";
import {
  DRAWN_LEVELS,
  type DrawnRow,
  frontier,
  treeRows,
  zoomBreadcrumb,
  zoomedRows,
} from "./tree-fold";

/**
 * The fold, over a tree built by hand (dispatch://INFRA-1196 R1, R6,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t14`): the page opens folded to the frontier and not
 * to a fixed depth, and a question's path costs no other branch a row.
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

function rowOf(rows: readonly DrawnRow[], id: string): DrawnRow | undefined {
  return rows.find((row) => row.kind === "node" && row.node.id === id);
}

/** An issue whose open question sits `depth` levels below it, inside a chain of documents, with
 *  one other document of three blocks beside it. */
function issueWithQuestionAt(depth: number): TreeNode {
  let deepest = node("question", "ask", [], { question: true, state: "open" });
  for (let level = depth - 1; level > 0; level -= 1) {
    deepest = node(`doc-${level}`, "document", [deepest]);
  }
  const other = node("other", "document", [
    node("other#b-1", "requirement"),
    node("other#b-2", "requirement"),
    node("other#b-3", "requirement"),
  ]);
  return node("CORE-1", "issue", [deepest, other]);
}

test("the path to an open question is unfolded whatever its depth", () => {
  for (const depth of [1, 2, 6]) {
    const root = issueWithQuestionAt(depth);
    const rows = treeRows(root);
    const question = rowOf(rows, "question");
    expect(question, `no row for a question ${depth} levels down`).toBeDefined();
    expect(question?.kind === "node" ? question.level : 0).toBe(depth + 1);
    for (let level = 1; level < depth; level += 1) {
      const above = rowOf(rows, `doc-${level}`);
      expect(above?.kind === "node" ? above.expanded : undefined).toBe(true);
    }
  }
});

test("every other branch is folded to its first level with a count of what it holds", () => {
  const rows = treeRows(issueWithQuestionAt(2));
  const other = rowOf(rows, "other");
  expect(other?.kind === "node" ? other.expanded : undefined).toBe(false);
  expect(other?.kind === "node" ? other.folded : undefined).toBe(3);
  expect(idsOf(rows)).not.toContain("other#b-1");
});

test("a question's rows are not counted against the fold", () => {
  const withQuestion = issueWithQuestionAt(2);
  const answered = node("CORE-1", "issue", [
    node("doc-1", "document", [node("question", "ask", [], { state: "answered" })]),
    withQuestion.children[1] as TreeNode,
  ]);
  const before = idsOf(treeRows(withQuestion));
  const after = idsOf(treeRows(answered));
  expect(before).toContain("question");
  expect(after).not.toContain("question");
  // Nothing outside the question's own path moved: the other branch is folded either way.
  expect(after.filter((id) => !before.includes(id))).toEqual([]);
  expect(after.length).toBeLessThan(before.length);
});

/** A requirement with one check that passed and nothing else: a branch holding nothing open. */
function proved(id: string): TreeNode {
  return node(id, "requirement", [node(`${id}-check`, "check", [], { state: "passed" })], {
    counts: { checks: 1, planTasks: 0 },
  });
}

test("a branch that holds nothing open arrives folded, with a count of what it holds", () => {
  const rows = treeRows(node("CORE-1", "issue", [proved("spec#b-r1")]));
  expect(idsOf(rows)).toEqual(["CORE-1", "spec#b-r1"]);
  const branch = rowOf(rows, "spec#b-r1");
  expect(branch?.kind === "node" ? branch.expanded : undefined).toBe(false);
  expect(branch?.kind === "node" ? branch.folded : undefined).toBe(1);
});

test("the path to an open question arrives unfolded and nothing beside it does", () => {
  const rows = treeRows(
    node("CORE-1", "issue", [
      node("spec", "document", [node("spec#b-q1", "ask", [], { question: true, state: "open" })]),
      proved("spec#b-r1"),
    ])
  );
  expect(idsOf(rows)).toEqual(["CORE-1", "spec", "spec#b-q1", "spec#b-r1"]);
  const above = rowOf(rows, "spec");
  expect(above?.kind === "node" ? above.expanded : undefined).toBe(true);
});

// One row per kind the frontier holds (`unanswered`), each read against the same kind settled.

test("an open question is open and an answered one is not", () => {
  const rows = treeRows(
    node("CORE-1", "issue", [
      node("asked", "document", [node("asked#b-q", "ask", [], { question: true, state: "open" })]),
      node("answered", "document", [node("answered#b-q", "ask", [], { state: "answered" })]),
    ])
  );
  expect(idsOf(rows)).toEqual(["CORE-1", "asked", "asked#b-q", "answered"]);
  const settled = rowOf(rows, "answered");
  expect(settled?.kind === "node" ? settled.folded : undefined).toBe(1);
});

test("a check that has not passed is open and a passed one is not", () => {
  for (const state of ["failed", "no-result", "older", "unmarked"] as const) {
    const rows = treeRows(
      node("CORE-1", "issue", [
        node("spec#b-r1", "requirement", [node("v#b-c1", "check", [], { state })], {
          counts: { checks: 1, planTasks: 0 },
        }),
      ])
    );
    expect(idsOf(rows), `a ${state} check is drawn nowhere`).toEqual([
      "CORE-1",
      "spec#b-r1",
      "v#b-c1",
    ]);
  }
  expect(idsOf(treeRows(node("CORE-1", "issue", [proved("spec#b-r1")])))).toEqual([
    "CORE-1",
    "spec#b-r1",
  ]);
});

// A decision sits under the question it settles the day the model holds that edge
// (`tree-drafts.ts`'s `addDecisions`); wherever it is drawn, what opens the path to it is its
// waiting for the reader's review.
test("a decision waiting for the reader's review is open and a settled one is not", () => {
  const rows = treeRows(
    node("CORE-1", "issue", [
      node(
        "spec#b-q1",
        "ask",
        [node("decisions-md#b-d1", "decision", [], { state: "waiting_review", waiting: true })],
        { state: "answered" }
      ),
      node("spec#b-q2", "ask", [node("decisions-md#b-d2", "decision", [], { state: "yours" })], {
        state: "answered",
      }),
    ])
  );
  expect(idsOf(rows)).toEqual(["CORE-1", "spec#b-q1", "decisions-md#b-d1", "spec#b-q2"]);
  const settled = rowOf(rows, "spec#b-q2");
  expect(settled?.kind === "node" ? settled.folded : undefined).toBe(1);
});

// Dispatch records no completion for a plan task, so none can be read as done and the path to
// every one of them is drawn (`unanswered`).
test("a plan task is open while nothing records it done", () => {
  const rows = treeRows(
    node("CORE-1", "issue", [
      node(
        "spec#b-r1",
        "requirement",
        [node("plan-md#b-t1", "plan_task"), node("v#b-c1", "check", [], { state: "passed" })],
        { counts: { checks: 1, planTasks: 1 } }
      ),
      proved("spec#b-r2"),
    ])
  );
  expect(idsOf(rows)).toEqual(["CORE-1", "spec#b-r1", "plan-md#b-t1", "v#b-c1", "spec#b-r2"]);
});

test("a requirement no check serves is open, and one outside the coverage read is not", () => {
  const rows = treeRows(
    node("CORE-1", "issue", [
      node("spec", "document", [
        node("spec#b-r1", "requirement", [], { counts: { checks: 0, planTasks: 0 } }),
      ]),
      node("branch", "document", [node("branch#b-r1", "requirement")]),
    ])
  );
  expect(idsOf(rows)).toEqual(["CORE-1", "spec", "spec#b-r1", "branch"]);
  const outside = rowOf(rows, "branch");
  expect(outside?.kind === "node" ? outside.folded : undefined).toBe(1);
});

test("a branch the reader opens below the drawn levels is one row saying how many it holds", () => {
  const deep = node("CORE-1", "issue", [
    node(
      "spec#b-r1",
      "requirement",
      [
        node(
          "verification-md#b-c1",
          "check",
          [node("result-1", "result"), node("result-2", "result")],
          { state: "failed" }
        ),
      ],
      { counts: { checks: 1, planTasks: 0 } }
    ),
  ]);
  // The check is on the frontier's path and drawn; what it holds is a set the reader opens,
  // and the row says how many nodes that is.
  const rows = treeRows(deep);
  expect(idsOf(rows)).toEqual(["CORE-1", "spec#b-r1", "verification-md#b-c1"]);
  expect(rowOf(rows, "verification-md#b-c1")).toMatchObject({ expanded: false, folded: 2 });
  // Opened by the reader at the third level, the branch is one row saying how many nodes it
  // holds rather than the nodes themselves.
  const opened = treeRows(deep, {
    branches: new Set(["verification-md#b-c1"]),
    closed: new Set(),
    deeper: new Set(),
  });
  expect(idsOf(opened)).toEqual([
    "CORE-1",
    "spec#b-r1",
    "verification-md#b-c1",
    "more:verification-md#b-c1",
  ]);
  const more = opened.at(-1);
  expect(more?.kind === "deeper" ? more.held : 0).toBe(2);
  expect(more?.kind === "deeper" ? more.level : 0).toBe(DRAWN_LEVELS + 1);
  // The row above it says how many nodes it holds that no row of their own draws, as a row
  // whose own twisty is closed does: a reader of the tree pattern - c-keyboard's among them -
  // takes a row carrying `aria-expanded` to hold children, and these are drawn as one line.
  const branch = rowOf(opened, "verification-md#b-c1");
  expect(branch?.kind === "node" ? branch.folded : undefined).toBe(2);
  expect(branch?.kind === "node" ? branch.expanded : undefined).toBe(true);
  // Opened in place, the branch is drawn where it stands.
  const inPlace = treeRows(deep, {
    branches: new Set(["verification-md#b-c1"]),
    closed: new Set(),
    deeper: new Set(["verification-md#b-c1"]),
  });
  expect(idsOf(inPlace)).toContain("result-2");
});

// R1: the reader's own hand outranks the frontier both ways. This is the open; the fold is the
// row below.
test("a branch the reader opens stays open though nothing in it is open", () => {
  const root = node("CORE-1", "issue", [proved("spec#b-r1")]);
  expect(idsOf(treeRows(root))).toEqual(["CORE-1", "spec#b-r1"]);
  const opened = treeRows(root, {
    branches: new Set(["spec#b-r1"]),
    closed: new Set(),
    deeper: new Set(),
  });
  expect(idsOf(opened)).toEqual(["CORE-1", "spec#b-r1", "spec#b-r1-check"]);
  const branch = rowOf(opened, "spec#b-r1");
  expect(branch?.kind === "node" ? branch.expanded : undefined).toBe(true);
});

test("a replaced decision folds to one line with a count, and is not deleted", () => {
  const root = node("CORE-1", "issue", [
    node("decisions-md#b-d1", "decision", [node("question", "ask", [], { question: true })], {
      replaced: true,
    }),
  ]);
  const rows = treeRows(root);
  const replaced = rowOf(rows, "decisions-md#b-d1");
  expect(replaced?.kind === "node" ? replaced.folded : undefined).toBe(1);
  expect(idsOf(rows)).not.toContain("question");
  // The reader opens it by hand and it is all there.
  const opened = treeRows(root, {
    branches: new Set(["decisions-md#b-d1"]),
    closed: new Set(),
    deeper: new Set(),
  });
  expect(idsOf(opened)).toContain("question");
});

// R1: the twisty says Fold and ArrowLeft folds, so every branch with children folds, including
// the reasoning the fold draws of its own accord and the path to an open question the frontier
// unfolds. A branch the reader cannot close is a branch they cannot read past.
test("a branch the reader folds shut stays shut, whatever drew it open", () => {
  const root = node("CORE-1", "issue", [
    node("spec#b-r1", "requirement", [node("verification-md#b-c1", "check")]),
    node("doc-1", "document", [node("question", "ask", [], { question: true, state: "open" })]),
  ]);
  // Nothing folded by hand: the requirement's own reasoning is drawn, and so is the path down
  // to the open question.
  expect(idsOf(treeRows(root))).toEqual([
    "CORE-1",
    "spec#b-r1",
    "verification-md#b-c1",
    "doc-1",
    "question",
  ]);
  const folded = treeRows(root, {
    branches: new Set(),
    closed: new Set(["spec#b-r1", "doc-1"]),
    deeper: new Set(),
  });
  expect(idsOf(folded)).toEqual(["CORE-1", "spec#b-r1", "doc-1"]);
  const requirement = rowOf(folded, "spec#b-r1");
  expect(requirement?.kind === "node" ? requirement.expanded : undefined).toBe(false);
  expect(requirement?.kind === "node" ? requirement.folded : undefined).toBe(1);
  // Opened again by the same control, both branches are back.
  const reopened = treeRows(root, {
    branches: new Set(["spec#b-r1", "doc-1"]),
    closed: new Set(),
    deeper: new Set(),
  });
  expect(idsOf(reopened)).toContain("verification-md#b-c1");
  expect(idsOf(reopened)).toContain("question");
});

test("the frontier holds every node above an open question and no question itself", () => {
  expect([...frontier(issueWithQuestionAt(2))].sort()).toEqual(["CORE-1", "doc-1"]);
});

test("the zoomed tree opens on the issue with the question's path unfolded under it", () => {
  const root = issueWithQuestionAt(2);
  // Nothing chosen is the issue itself, so every branch of it is one row away; the path down
  // to the open question is unfolded under it, at the one level the narrow tree has.
  const rows = zoomedRows(root, undefined);
  expect(idsOf(rows)).toEqual(["CORE-1", "doc-1", "question", "other"]);
  expect(zoomBreadcrumb(root, undefined)).toEqual([]);
  expect(rows.every((row) => row.level <= 2)).toBe(true);
  const unfolded = rowOf(rows, "doc-1");
  expect(unfolded?.kind === "node" ? unfolded.expanded : undefined).toBe(true);
  const other = rowOf(rows, "other");
  expect(other?.kind === "node" ? other.folded : undefined).toBe(3);
  // Zoomed into a node, the tree is that node and its children, under a breadcrumb that walks
  // back to the issue.
  const inside = zoomedRows(root, "doc-1");
  expect(idsOf(inside)).toEqual(["doc-1", "question"]);
  expect(zoomBreadcrumb(root, "doc-1").map((one) => one.id)).toEqual(["CORE-1"]);
  expect(inside.every((row) => row.level <= 2)).toBe(true);
});
