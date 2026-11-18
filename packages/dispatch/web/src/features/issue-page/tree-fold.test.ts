import { expect, test } from "bun:test";

import type { TreeNode } from "./issue-tree";
import { heldBy } from "./issue-tree";
import type { NodeType } from "./page-words";
import {
  DRAWN_LEVELS,
  type DrawnRow,
  frontier,
  NARROW_LEVELS,
  NOTHING_OPENED,
  treeRows,
  zoomBreadcrumb,
} from "./tree-fold";

/**
 * The fold, over a tree built by hand: the page opens folded to the frontier and not to a fixed
 * depth, and a question's path costs no other branch a row.
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
  // A node's line of overtaken children reads as `line:<id>`.
  if (!rows.every((row) => row.kind !== "overtaken"))
    return rows.flatMap((row) => (row.kind === "overtaken" ? `line:${row.of.id}` : idsOf([row])));
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
  for (const state of [
    "failed",
    "no-result",
    "older",
    "retracted",
    "unmarked",
    "unreadable",
  ] as const) {
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

// A decision sits where the tree read parents it; wherever it is drawn, what opens the path to it
// is its waiting for the reader's review.
test("a decision waiting for the reader's review is open and a settled one is not", () => {
  const rows = treeRows(
    node("CORE-1", "issue", [
      node(
        "spec#b-q1",
        "ask",
        [node("decisions-md#b-d1", "decision", [], { state: "waiting_review", waiting: true })],
        { state: "answered" }
      ),
      node("spec#b-q2", "ask", [node("decisions-md#b-d2", "decision", [], { state: "kept" })], {
        state: "answered",
      }),
    ])
  );
  expect(idsOf(rows)).toEqual(["CORE-1", "spec#b-q1", "decisions-md#b-d1", "spec#b-q2"]);
  const settled = rowOf(rows, "spec#b-q2");
  expect(settled?.kind === "node" ? settled.folded : undefined).toBe(1);
});

// Dispatch records no completion for a plan task, so a task nobody can mark done is no
// evidence that anything is open: the frontier passes it by and the branch it sits in folds
// like any other (`unanswered`). What the work leaves open the issue can show - the check that
// has not passed, the requirement nothing proves - and that carries the path to it.
test("a plan task is not open, and the check beside it is what opens their branch", () => {
  const settled = treeRows(
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
  expect(idsOf(settled)).toEqual(["CORE-1", "spec#b-r1", "spec#b-r2"]);
  const folded = rowOf(settled, "spec#b-r1");
  expect(folded?.kind === "node" ? folded.expanded : undefined).toBe(false);
  expect(folded?.kind === "node" ? folded.folded : undefined).toBe(2);

  const open = treeRows(
    node("CORE-1", "issue", [
      node(
        "spec#b-r1",
        "requirement",
        [node("plan-md#b-t1", "plan_task"), node("v#b-c1", "check", [], { state: "failed" })],
        { counts: { checks: 1, planTasks: 1 } }
      ),
      proved("spec#b-r2"),
    ])
  );
  expect(idsOf(open)).toEqual(["CORE-1", "spec#b-r1", "plan-md#b-t1", "v#b-c1", "spec#b-r2"]);
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

/** A chain of documents `length` long under the issue, each holding the next, the last holding
 *  one note: a branch as deep as a reader can open, with nothing on it open. */
function chain(length: number): TreeNode {
  let deepest = node(`doc-${length}`, "document", [node("note", "callout")]);
  for (let at = length - 1; at > 0; at -= 1) {
    deepest = node(`doc-${at}`, "document", [deepest]);
  }
  return node("CORE-1", "issue", [deepest]);
}

/** Every branch of `chain(length)` opened by the reader, and none opened in place. */
function openedChain(length: number) {
  return {
    branches: new Set(Array.from({ length }, (_, at) => `doc-${at + 1}`)),
    closed: new Set<string>(),
    deeper: new Set<string>(),
  };
}

// R16: from 1024 px up the outline draws three levels below the node it is rooted at, and a
// branch the reader opens deeper than that is one row saying how many nodes it holds, which
// opens them in place.
test("a branch the reader opens below the drawn levels is one row saying how many it holds", () => {
  const root = chain(4);
  const opened = treeRows(root, openedChain(4));
  // doc-1, doc-2 and doc-3 sit one, two and three levels below the issue, and are drawn; doc-3's
  // own nodes would sit a fourth level down, so its branch is one row of its own.
  expect(idsOf(opened)).toEqual(["CORE-1", "doc-1", "doc-2", "doc-3", "more:doc-3"]);
  const more = opened.at(-1);
  expect(more?.kind === "deeper" ? more.held : 0).toBe(2);
  expect(more?.kind === "deeper" ? more.level : 0).toBe(DRAWN_LEVELS + 2);
  // The row above it says how many nodes it holds that no row of their own draws, as a row
  // whose own fold is closed does: a reader of the tree pattern - c-keyboard's among them -
  // takes a row carrying `aria-expanded` to hold children, and these are drawn as one line.
  const branch = rowOf(opened, "doc-3");
  expect(branch?.kind === "node" ? branch.folded : undefined).toBe(2);
  expect(branch?.kind === "node" ? branch.expanded : undefined).toBe(true);
  // Opened in place, the branch is drawn where it stands.
  const inPlace = treeRows(root, { ...openedChain(4), deeper: new Set(["doc-3"]) });
  expect(idsOf(inPlace)).toContain("doc-4");
});

// R16: below 1024 px the outline draws one level below the node it is rooted at, so a branch the
// reader opens there is one row with its count straight away.
test("below 1024 px one level is drawn below the node the outline is on", () => {
  const opened = treeRows(chain(2), openedChain(2), [], NARROW_LEVELS);
  expect(idsOf(opened)).toEqual(["CORE-1", "doc-1", "more:doc-1"]);
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
// the branches the frontier unfolds: the path to a check that has not passed and the path to an
// open question. A branch the reader cannot close is a branch they cannot read past.
test("a branch the reader folds shut stays shut, whatever drew it open", () => {
  const root = node("CORE-1", "issue", [
    node("spec#b-r1", "requirement", [
      node("verification-md#b-c1", "check", [], { state: "no-result" }),
    ]),
    node("doc-1", "document", [node("question", "ask", [], { question: true, state: "open" })]),
  ]);
  // Nothing folded by hand: the requirement is open, because its check has no result and so has
  // not passed, and so is the path down to the open question.
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

// R16: below 1024 px the outline draws one level below the node it is on, except a path the
// frontier holds open, which is drawn whatever its depth.
test("below 1024 px the frontier's paths are drawn whatever their depth", () => {
  const root = issueWithQuestionAt(3);
  // Nothing chosen is the issue itself, so every branch of it is one row away; the path down
  // to the open question is unfolded under it, a level a node.
  const rows = treeRows(root, NOTHING_OPENED, [], NARROW_LEVELS);
  expect(idsOf(rows)).toEqual(["CORE-1", "doc-1", "doc-2", "question", "other"]);
  expect(zoomBreadcrumb(root, undefined)).toEqual([]);
  expect(rowOf(rows, "question")?.level).toBe(4);
  const other = rowOf(rows, "other");
  expect(other?.kind === "node" ? other.folded : undefined).toBe(3);
  // Zoomed into a node, the outline is that node and what it holds, under a breadcrumb that
  // walks back to the issue.
  const doc = rowOf(rows, "doc-1");
  const inside = treeRows(
    doc?.kind === "node" ? doc.node : root,
    NOTHING_OPENED,
    ["CORE-1"],
    NARROW_LEVELS
  );
  expect(idsOf(inside)).toEqual(["doc-1", "doc-2", "question"]);
  expect(zoomBreadcrumb(root, "doc-1").map((one) => one.id)).toEqual(["CORE-1"]);
});

/** The line a node's overtaken children fold to, where the rows draw one under `id`. */
function lineOf(rows: readonly DrawnRow[], id: string) {
  const line = rows.find((row) => row.kind === "overtaken" && row.of.id === id);
  return line?.kind === "overtaken" ? line : undefined;
}

/** What the model says of a node a later one took the place of. */
const OVERTAKEN = { closed: true, life: "overtaken" } as const;

/** An issue whose one document holds a live requirement between two overtaken ones, the second
 *  holding a note, and counts what it holds as the tree read does (`heldBy`). */
function withOvertaken(): TreeNode {
  const children = [
    node("spec#b-r1", "requirement", [], OVERTAKEN),
    node("spec#b-r2", "requirement"),
    node("spec#b-r3", "requirement", [node("spec#b-r3-note", "callout")], OVERTAKEN),
  ];
  return node("CORE-1", "issue", [node("spec", "document", children, { held: heldBy(children) })]);
}

// Under a parent that holds overtaken nodes, one line after its other children says how many it
// holds directly, and is a row of the tree, which the keys reach.
// Opened, each overtaken node is a row one level below the line, folded with its subtree.
test("overtaken children are one focusable line that counts them", () => {
  const root = withOvertaken();
  const opened = { ...NOTHING_OPENED, branches: new Set(["spec"]) };
  const closed = treeRows(root, opened);
  expect(idsOf(closed)).toEqual(["CORE-1", "spec", "spec#b-r2", "line:spec"]);
  const line = lineOf(closed, "spec");
  expect(line?.held).toBe(2);
  expect(line?.level).toBe(3);
  expect(line?.open).toBe(false);
  expect(line?.trail).toEqual(["CORE-1"]);

  const open = treeRows(root, { ...opened, overtaken: new Set(["spec"]) });
  expect(idsOf(open)).toEqual([
    "CORE-1",
    "spec",
    "spec#b-r2",
    "line:spec",
    "spec#b-r1",
    "spec#b-r3",
  ]);
  expect(lineOf(open, "spec")?.open).toBe(true);
  for (const id of ["spec#b-r1", "spec#b-r3"]) {
    expect(rowOf(open, id)?.level).toBe(4);
    expect(rowOf(open, id)?.trail).toEqual(["CORE-1", "spec"]);
  }
  const holding = rowOf(open, "spec#b-r3");
  expect(holding?.kind === "node" ? holding.expanded : undefined).toBe(false);
  expect(holding?.kind === "node" ? holding.folded : undefined).toBe(1);
});

// An open question under an overtaken node opens no path, so neither that node nor its parent
// unfolds because of it, and opened by the reader the overtaken node still arrives folded.
test("the frontier never passes into an overtaken node", () => {
  const question = node("spec#b-q1", "ask", [], { question: true, state: "open" });
  const issueWith = (life: Partial<TreeNode>): TreeNode =>
    node("CORE-1", "issue", [
      node("spec", "document", [node("spec#b-r1", "requirement", [question], life)]),
    ]);
  // The control: under a live requirement, the question opens the path down to it.
  expect([...frontier(issueWith({}))].sort()).toEqual(["CORE-1", "spec", "spec#b-r1"]);

  const root = issueWith(OVERTAKEN);
  expect([...frontier(root)]).toEqual([]);
  expect(idsOf(treeRows(root))).toEqual(["CORE-1", "spec"]);
  const opened = treeRows(root, {
    ...NOTHING_OPENED,
    branches: new Set(["spec"]),
    overtaken: new Set(["spec"]),
  });
  expect(idsOf(opened)).toEqual(["CORE-1", "spec", "line:spec", "spec#b-r1"]);
  const overtaken = rowOf(opened, "spec#b-r1");
  expect(overtaken?.kind === "node" ? overtaken.expanded : undefined).toBe(false);
});

// A folded branch's count counts its line of overtaken children as one row and nothing under it,
// which is the number of rows the branch draws opened, its line closed.
test("a folded count counts the line as one row", () => {
  const root = withOvertaken();
  const folded = rowOf(treeRows(root), "spec");
  expect(folded?.kind === "node" ? folded.expanded : undefined).toBe(false);
  expect(folded?.kind === "node" ? folded.folded : undefined).toBe(2);
  const opened = treeRows(root, { ...NOTHING_OPENED, branches: new Set(["spec"]) });
  expect(idsOf(opened).slice(2)).toEqual(["spec#b-r2", "line:spec"]);
});

// A proposed node is only put forward until a person settles it, so a proposed statement no check
// serves and a proposed check that has not passed open no path; a proposed question and a proposed
// decision waiting for review still ask the reader.
test("a proposed statement no check serves and a proposed check that has not passed open no path", () => {
  const issueWith = (life: Partial<TreeNode>): TreeNode =>
    node("CORE-1", "issue", [
      node("spec", "document", [
        node("spec#b-r1", "requirement", [], { ...life, counts: { checks: 0, planTasks: 0 } }),
      ]),
      node("verification", "document", [
        node("verification#b-c1", "check", [], { ...life, state: "failed" }),
      ]),
      node("asks", "document", [node("q", "ask", [], { ...life, question: true, state: "open" })]),
      node("decisions", "document", [node("d", "decision", [], { ...life, waiting: true })]),
    ]);
  // The control: live, each of the four opens the path down to it.
  expect([...frontier(issueWith({}))].sort()).toEqual([
    "CORE-1",
    "asks",
    "decisions",
    "spec",
    "verification",
  ]);
  expect([...frontier(issueWith({ life: "proposed" }))].sort()).toEqual([
    "CORE-1",
    "asks",
    "decisions",
  ]);
});
