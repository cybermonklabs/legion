import { expect, test } from "bun:test";

import type { TreeNode } from "./issue-tree";
import { heldBy } from "./issue-tree";
import { stampStates } from "./node-states/node-states";
import type { NodeType } from "./page-words";
import {
  CHILD_PAGE,
  type DrawnRow,
  foldedLine,
  indentOf,
  NOTHING_OPENED,
  type OpenedBranches,
  pathTo,
  treeRows,
  withEveryFoldUndone,
  withFold,
  withNextPage,
  zoomBreadcrumb,
} from "./tree-fold";

/**
 * The fold, over a tree built by hand: the page opens with every node unfolded and folds none
 * itself, the reader folds a node to its first line and a count, `z` unfolds what they folded,
 * a node draws its children a page of 50 at a time, and every level is drawn (R11, R15).
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

/** The ids of every row drawn, in reading order; the row that pages a node's children reads as
 *  `page:<id>:<how many are left>`. */
function idsOf(rows: readonly DrawnRow[]): string[] {
  return rows.map((row) => (row.kind === "page" ? `page:${row.of.id}:${row.left}` : row.node.id));
}

function rowOf(rows: readonly DrawnRow[], id: string): Extract<DrawnRow, { kind: "node" }> {
  const found = rows.find((row) => row.kind === "node" && row.node.id === id);
  if (found?.kind !== "node") throw new Error(`no row for ${id}`);
  return found;
}

/** An issue as the frontier's fold read it: an open question deep in a chain of documents, a
 *  requirement whose one check passed, a check that failed, an answered question with the
 *  decision that settled it, and a replaced decision over the question it settled. */
function issue(): TreeNode {
  let deepest = node("question", "ask", [], { question: true, state: "open" });
  for (let level = 4; level > 0; level -= 1) deepest = node(`doc-${level}`, "document", [deepest]);
  return node("CORE-1", "issue", [
    deepest,
    node("spec#b-r1", "requirement", [node("v#b-c1", "check", [], { state: "passed" })], {
      counts: { checks: 1, planTasks: 0 },
    }),
    node("spec#b-r2", "requirement", [node("v#b-c2", "check", [], { state: "failed" })]),
    node("ask-1", "ask", [node("spec#b-d1", "decision", [], { state: "kept" })], {
      state: "answered",
    }),
    node("spec#b-d2", "decision", [node("ask-2", "ask", [], { state: "answered" })], {
      replaced: true,
    }),
  ]);
}

/** Every id of `root`, the root first, in reading order. */
function everyId(root: TreeNode): string[] {
  return [root.id, ...root.children.flatMap(everyId)];
}

/** A node holding `count` children, `<id>-1` up to `<id>-<count>`. */
function holding(id: string, count: number): TreeNode {
  return node(
    id,
    "comment",
    Array.from({ length: count }, (_, at) => node(`${id}-${at + 1}`, "comment"))
  );
}

// R11: Conversation and Graph open with no node folded, at any depth, whatever the node holds:
// a path to nothing open, a passed check, an answered question, a replaced decision.
test("no node opens folded", () => {
  const root = issue();
  const rows = treeRows(root);
  expect(idsOf(rows)).toEqual(everyId(root));
  for (const row of rows) {
    if (row.kind !== "node") continue;
    expect(row.folded, `${row.node.id} opened folded`).toBeUndefined();
    expect(row.expanded, `${row.node.id} opened folded`).toBe(
      row.node.children.length > 0 ? true : undefined
    );
  }
});

// R11: a node the reader folds shows its first line - to its first sentence end, or to 100
// characters cut at a word - and how many nodes it holds, at every level, and its branch is
// drawn as no rows; unfolded, the branch is back.
test("a folded node shows its first line and how many nodes it holds", () => {
  const long = `${"word ".repeat(30)}with no end`;
  const deep = node("doc-3", "document", [node("note", "callout")], { text: long });
  const root = node("CORE-1", "issue", [
    node(
      "comment-1",
      "comment",
      [node("reply-1", "comment", [node("reply-2", "comment")]), node("reply-3", "comment")],
      { text: "The first sentence ends here. The second does not show." }
    ),
    node("doc-1", "document", [node("doc-2", "document", [deep])]),
  ]);
  let opened = withFold(NOTHING_OPENED, "comment-1", false);
  opened = withFold(opened, "doc-3", false);
  const rows = treeRows(root, opened);
  expect(idsOf(rows)).toEqual(["CORE-1", "comment-1", "doc-1", "doc-2", "doc-3"]);
  const comment = rowOf(rows, "comment-1");
  expect(comment).toMatchObject({ expanded: false, folded: 3 });
  expect(foldedLine(comment.node)).toBe("The first sentence ends here.");
  const document = rowOf(rows, "doc-3");
  expect(document).toMatchObject({ expanded: false, folded: 1, level: 4 });
  const line = foldedLine(document.node);
  expect(line.endsWith("…")).toBe(true);
  expect(line.length).toBeLessThanOrEqual(101);
  expect(long.startsWith(line.slice(0, -1))).toBe(true);
  expect(line.slice(0, -1).endsWith(" ")).toBe(false);
  // Unfolded by the same control, the branch is back.
  const unfolded = treeRows(root, withFold(opened, "comment-1", true));
  expect(idsOf(unfolded)).toContain("reply-2");
  expect(idsOf(unfolded)).not.toContain("note");
});

// R11: `z` unfolds every node the reader folded, and leaves the pages of children they drew.
test("z unfolds every node the reader folded", () => {
  const root = node("CORE-1", "issue", [
    holding("thread", CHILD_PAGE + 7),
    node("spec#b-r1", "requirement", [node("v#b-c1", "check", [node("result-1", "result")])]),
  ]);
  const page = treeRows(root).find((row) => row.kind === "page");
  if (page?.kind !== "page") throw new Error("no page row");
  let opened: OpenedBranches = withNextPage(NOTHING_OPENED, page);
  for (const id of ["spec#b-r1", "v#b-c1", "thread"]) opened = withFold(opened, id, false);
  expect(idsOf(treeRows(root, opened))).toEqual(["CORE-1", "thread", "spec#b-r1"]);
  const undone = withEveryFoldUndone(opened);
  expect(undone.closed.size).toBe(0);
  expect(idsOf(treeRows(root, undone))).toEqual(everyId(root));
  // With nothing folded, `z` changes nothing.
  expect(withEveryFoldUndone(NOTHING_OPENED)).toBe(NOTHING_OPENED);
});

// R15: a node draws its first 50 children, in the read's order, and one row after them that says
// how many are left; its press draws the next 50, and a page is no fold.
test("a node draws its first 50 children and the count of the rest", () => {
  const root = node("CORE-1", "issue", [holding("post", 120)]);
  const first = treeRows(root);
  expect(idsOf(first)).toEqual([
    "CORE-1",
    "post",
    ...Array.from({ length: 50 }, (_, at) => `post-${at + 1}`),
    "page:post:70",
  ]);
  expect(rowOf(first, "post")).toMatchObject({ expanded: true });
  expect(rowOf(first, "post").folded).toBeUndefined();
  const page = first.at(-1);
  if (page?.kind !== "page") throw new Error("no page row");
  expect(page.level).toBe(3);
  const second = treeRows(root, withNextPage(NOTHING_OPENED, page));
  expect(idsOf(second).at(-2)).toBe("post-100");
  expect(idsOf(second).at(-1)).toBe("page:post:20");
  const last = second.at(-1);
  if (last?.kind !== "page") throw new Error("no page row");
  const third = treeRows(root, withNextPage(withNextPage(NOTHING_OPENED, page), last));
  expect(idsOf(third).at(-1)).toBe("post-120");
  expect(third.some((row) => row.kind === "page")).toBe(false);
  // A node of 50 children or fewer draws no page row.
  expect(treeRows(node("CORE-1", "issue", [holding("post", 50)])).at(-1)?.kind).toBe("node");
});

// R15: a child the focus, an address or a write names is drawn with every child before it, at
// every level of the path down to it, so a new row past the first 50 lands in view.
test("the child the focus names is drawn with every child before it", () => {
  const reply = holding("post-52", 60);
  const root = node("CORE-1", "issue", [
    node(
      "post",
      "comment",
      Array.from({ length: 57 }, (_, at) => (at === 51 ? reply : node(`post-${at + 1}`, "comment")))
    ),
  ]);
  const named = treeRows(root, NOTHING_OPENED, [], ["post-55"]);
  expect(idsOf(named).slice(-3)).toEqual(["post-54", "post-55", "page:post:2"]);
  // Deeper down, every parent on the path draws up to the child the path goes through.
  const deep = idsOf(treeRows(root, NOTHING_OPENED, [], ["post-52-58"]));
  expect(deep).toContain("post-52");
  expect(deep).toContain("post-52-58");
  expect(deep).toContain("page:post-52:2");
  expect(deep.at(-1)).toBe("page:post:5");
  // Named by nothing, it is behind its parent's page again.
  expect(idsOf(treeRows(root))).not.toContain("post-52");
});

// R11: every level is drawn, and past six levels a row's indent stops growing, so its words keep
// their width at 390 px. The issue is no row, so the outline's first row is level 1.
test("the indent stops growing past six levels", () => {
  let deepest = node("reply-9", "comment");
  for (let at = 8; at > 0; at -= 1) deepest = node(`reply-${at}`, "comment", [deepest]);
  const rows = treeRows(node("CORE-1", "issue", [deepest]))
    .slice(1)
    .map((row) => ({ ...row, level: row.level - 1 }));
  expect(idsOf(rows)).toEqual(Array.from({ length: 9 }, (_, at) => `reply-${at + 1}`));
  expect(rows.map((row) => indentOf(row.level))).toEqual([0, 1, 2, 3, 4, 5, 5, 5, 5]);
});

// R11: `.` zooms into a node under the breadcrumb of its ancestors from the issue down, and the
// rows drawn from it keep the keys of their path from the issue.
test("a zoom draws its node's branch under the breadcrumb of its ancestors", () => {
  const root = issue();
  expect(zoomBreadcrumb(root, undefined)).toEqual([]);
  expect(zoomBreadcrumb(root, "doc-3").map((one) => one.id)).toEqual(["CORE-1", "doc-1", "doc-2"]);
  const doc = rowOf(treeRows(root), "doc-3").node;
  const inside = treeRows(doc, NOTHING_OPENED, ["CORE-1", "doc-1", "doc-2"]);
  expect(idsOf(inside)).toEqual(["doc-3", "doc-4", "question"]);
  expect(rowOf(inside, "question").trail).toEqual(["CORE-1", "doc-1", "doc-2", "doc-3", "doc-4"]);
});

/** What each row draws but its node's own facts: the node's id, or the paged node's, its level,
 *  its twisty and its count. */
function shapeOf(rows: readonly DrawnRow[]): string[] {
  return rows.map((row) =>
    row.kind === "node"
      ? `${row.node.id}@${row.level}:${String(row.expanded)}:${String(row.folded)}`
      : `page:${row.of.id}@${row.level}:${row.left}`
  );
}

/** What the model says of a node a later one took the place of. */
const OVERTAKEN = { closed: true, life: "overtaken" } as const;

/** An issue whose one document holds a live requirement between two requirements with `life`,
 *  overtaken unless a case gives another, the second holding a note, and counts what it holds as
 *  the tree read does (`heldBy`). */
function withOvertaken(life: Partial<TreeNode> = OVERTAKEN): TreeNode {
  const children = [
    node("spec#b-r1", "requirement", [], life),
    node("spec#b-r2", "requirement"),
    node("spec#b-r3", "requirement", [node("spec#b-r3-note", "callout")], life),
  ];
  return node("CORE-1", "issue", [node("spec", "document", children, { held: heldBy(children) })]);
}

// An overtaken node is a row at its place among its parent's children, in the tree's order, at
// their level, and its subtree is drawn open as any branch's is; the reader folds it as any row,
// to one row counting what it holds. The same tree with nothing overtaken draws the same rows,
// as the page opens, with the node folded by the reader, and with that fold undone by `z`.
test("an overtaken node is a row at its place with its subtree open", () => {
  const root = withOvertaken();
  const rows = treeRows(root);
  expect(idsOf(rows)).toEqual([
    "CORE-1",
    "spec",
    "spec#b-r1",
    "spec#b-r2",
    "spec#b-r3",
    "spec#b-r3-note",
  ]);
  for (const id of ["spec#b-r1", "spec#b-r3"]) {
    const row = rowOf(rows, id);
    expect(row.level, id).toBe(3);
    expect(row.trail, id).toEqual(["CORE-1", "spec"]);
  }
  expect(rowOf(rows, "spec#b-r3")).toMatchObject({ expanded: true });
  expect(rowOf(rows, "spec#b-r3").folded).toBeUndefined();
  expect(rowOf(rows, "spec#b-r3-note").level).toBe(4);

  // Folded by the reader, it is one row counting what it holds, as any row is.
  const shut = withFold(NOTHING_OPENED, "spec#b-r3", false);
  expect(rowOf(treeRows(root, shut), "spec#b-r3")).toMatchObject({ expanded: false, folded: 1 });

  const live = withOvertaken({});
  for (const opening of [NOTHING_OPENED, shut, withEveryFoldUndone(shut)]) {
    expect(shapeOf(treeRows(root, opening))).toEqual(shapeOf(treeRows(live, opening)));
  }
});

// A folded branch's count counts an overtaken node and the nodes under it as it counts every node:
// it is the count of the same tree with nothing overtaken, and the number of rows the branch draws
// open.
test("a folded count counts an overtaken node as any node", () => {
  const root = withOvertaken();
  const live = withOvertaken({});
  expect(root.held).toBe(live.held);
  expect(treeRows(root).slice(2)).toHaveLength(4);
  const shut = withFold(NOTHING_OPENED, "spec", false);
  for (const tree of [root, live]) {
    expect(rowOf(treeRows(tree, shut), "spec")).toMatchObject({ expanded: false, folded: 4 });
  }
});

// An overtaken node is drawn open and whole, folded only by the reader (spec R5): as the page
// opens, with nothing folded, the outline draws an overtaken node and every node under it open,
// the same rows it draws for the same tree with nothing overtaken, whether the overtaken node
// stands under a live node or under another overtaken one.
test("an overtaken node arrives as open on load as its live self", () => {
  // Each node's read id is its own id, so a case names the nodes the stamp makes overtaken by id.
  const read = (id: string, type: NodeType, children: readonly TreeNode[] = []): TreeNode =>
    node(id, type, children, { readId: id });
  const live = node("CORE-1", "issue", [
    read("spec", "document", [
      read("spec#b-r1", "requirement", [
        read("spec#b-r1-note", "callout", [read("spec#b-r1-ask", "ask")]),
      ]),
      read("spec#b-r2", "requirement", [read("spec#b-r2-note", "callout")]),
    ]),
  ]);
  const liveShape = shapeOf(treeRows(live, NOTHING_OPENED));
  const cases = [["spec#b-r1"], ["spec#b-r1-note"], ["spec#b-r1", "spec#b-r1-note"], ["spec#b-r2"]];
  for (const overtaken of cases) {
    const label = `${overtaken.join(" and ")} overtaken`;
    const states = new Map(overtaken.map((id) => [id, { life: "overtaken" }] as const));
    const root = stampStates(live, states);
    const rows = treeRows(root, NOTHING_OPENED);
    for (const id of overtaken) {
      // The case reads an overtaken node that holds a branch, which its live self holds too.
      const placed = pathTo(root, id)?.at(-1);
      expect(placed?.life, `${label}: ${id}`).toBe("overtaken");
      expect(pathTo(live, id)?.at(-1)?.life, `${label}: ${id} live`).toBeUndefined();
      expect(placed?.children.length, `${label}: ${id} holds`).toBeGreaterThan(0);
      // Its row is open, and every node under it is a row.
      expect(rowOf(rows, id), `${label}: ${id}`).toMatchObject({ expanded: true });
      expect(rowOf(rows, id).folded, `${label}: ${id}`).toBeUndefined();
      for (const under of everyId(placed ?? root).slice(1)) {
        expect(idsOf(rows), `${label}: ${under} under ${id}`).toContain(under);
      }
    }
    expect(idsOf(rows), label).toEqual(everyId(root));
    expect(shapeOf(rows), label).toEqual(liveShape);
  }
});
