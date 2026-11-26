import { expect, test } from "bun:test";

import type { TreeNode } from "./issue-tree";
import type { NodeType } from "./page-words";
import {
  CHILD_PAGE,
  type DrawnRow,
  indentOf,
  NOTHING_OPENED,
  type OpenedBranches,
  placeOf,
  treeRows,
  withEveryFoldUndone,
  withFold,
  withNextPage,
  zoomBreadcrumb,
} from "./tree-fold";

/**
 * The fold, over a tree built by hand: the page opens with every node unfolded and folds none
 * itself, the reader folds a row to its node's first line and a count, `z` unfolds what they
 * folded, a node draws its children a page of 50 at a time, and every level is drawn (R11, R15).
 * A fold and a page are the reader's act on one row, kept by the row's place.
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

/** An issue with an open question deep in a chain of documents, a
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

/** An issue whose one node `served` holds is built under two requirements, as a check that
 *  serves both is (`issue-tree.ts`): its rows are two, one under each requirement. */
function servedTwice(served: () => TreeNode): TreeNode {
  return node("CORE-1", "issue", [
    node("spec#b-r1", "requirement", [served()]),
    node("spec#b-r2", "requirement", [served()]),
  ]);
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

// R11: a node the reader folds counts how many nodes it holds, at every level, and its branch is
// drawn as no rows; unfolded, the branch is back. The line a folded row draws is the row's
// (`NodeRow.drawn.test.tsx`).
test("a folded node counts the nodes it holds and draws no row of its branch", () => {
  const deep = node("doc-3", "document", [node("note", "callout")]);
  const root = node("CORE-1", "issue", [
    node("comment-1", "comment", [
      node("reply-1", "comment", [node("reply-2", "comment")]),
      node("reply-3", "comment"),
    ]),
    node("doc-1", "document", [node("doc-2", "document", [deep])]),
  ]);
  let opened = withFold(NOTHING_OPENED, "CORE-1>comment-1", false);
  opened = withFold(opened, "CORE-1>doc-1>doc-2>doc-3", false);
  const rows = treeRows(root, opened);
  expect(idsOf(rows)).toEqual(["CORE-1", "comment-1", "doc-1", "doc-2", "doc-3"]);
  expect(rowOf(rows, "comment-1")).toMatchObject({ expanded: false, folded: 3 });
  expect(rowOf(rows, "doc-3")).toMatchObject({ expanded: false, folded: 1, level: 4 });
  // Unfolded by the same control, the branch is back.
  const unfolded = treeRows(root, withFold(opened, "CORE-1>comment-1", true));
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
  for (const place of ["CORE-1>spec#b-r1", "CORE-1>spec#b-r1>v#b-c1", "CORE-1>thread"]) {
    opened = withFold(opened, place, false);
  }
  expect(idsOf(treeRows(root, opened))).toEqual(["CORE-1", "thread", "spec#b-r1"]);
  const undone = withEveryFoldUndone(opened);
  expect(undone.closed.size).toBe(0);
  expect(idsOf(treeRows(root, undone))).toEqual(everyId(root));
  // With nothing folded, `z` changes nothing.
  expect(withEveryFoldUndone(NOTHING_OPENED)).toBe(NOTHING_OPENED);
});

// R11: a fold is the reader's act on one row. A check that serves two requirements is drawn under
// each, and the reader's fold of one of those rows folds that row alone: the other row of the
// check stays as it was, and unfolding the row draws back exactly the rows its count said.
test("a fold on one row of a node drawn under two parents folds that row alone", () => {
  const root = servedTwice(() =>
    node("v#b-c1", "check", [
      node("result-1", "result", [node("note", "callout")]),
      node("result-2", "result"),
    ])
  );
  const place = "CORE-1>spec#b-r2>v#b-c1";
  const folded = treeRows(root, withFold(NOTHING_OPENED, place, false));
  const checks = folded.filter((row) => row.kind === "node" && row.node.id === "v#b-c1");
  expect(checks.map((row) => row.trail.at(-1))).toEqual(["spec#b-r1", "spec#b-r2"]);
  expect(checks[0], "the fold of the check's row under r2 folded its row under r1").toMatchObject({
    expanded: true,
  });
  expect(checks[0]).not.toHaveProperty("folded");
  expect(checks[1]).toMatchObject({ expanded: false, folded: 3 });
  expect(idsOf(folded)).toEqual([
    "CORE-1",
    "spec#b-r1",
    "v#b-c1",
    "result-1",
    "note",
    "result-2",
    "spec#b-r2",
    "v#b-c1",
  ]);
  // Unfolded by the same control, the row draws back the three rows its count said, and no more.
  const unfolded = treeRows(root, withFold(withFold(NOTHING_OPENED, place, false), place, true));
  expect(unfolded.length - folded.length).toBe(3);
  expect(idsOf(unfolded)).toEqual(idsOf(treeRows(root)));
});

// R11: the reader's fold is kept by the row's place from the issue, which the rows drawn from a
// zoomed node are seeded with, so a fold holds through a zoom into a branch above it and back
// out, a fold made inside the zoom holds once zoomed out, and both hold through a read drawn
// again with the same nodes.
test("a fold holds through a zoom in and back out, and through a read drawn again", () => {
  const build = (): TreeNode =>
    node("CORE-1", "issue", [
      node("doc-1", "document", [
        node("doc-2", "document", [node("comment-1", "comment", [node("reply-1", "comment")])]),
      ]),
    ]);
  const root = build();
  const whole = rowOf(treeRows(root), "comment-1");
  const opened = withFold(NOTHING_OPENED, placeOf(whole.trail, whole.node.id), false);
  expect(rowOf(treeRows(root, opened), "comment-1")).toMatchObject({ expanded: false, folded: 1 });
  // Zoomed into doc-1, the row is still folded.
  const doc = rowOf(treeRows(root), "doc-1").node;
  const inside = treeRows(doc, opened, ["CORE-1"]);
  expect(rowOf(inside, "comment-1"), "the zoom into doc-1 drew the folded row open").toMatchObject({
    expanded: false,
    folded: 1,
  });
  expect(idsOf(inside)).not.toContain("reply-1");
  // Zoomed back out, and drawn again from a new read of the same nodes, it is still folded.
  expect(rowOf(treeRows(root, opened), "comment-1")).toMatchObject({ expanded: false });
  expect(idsOf(treeRows(build(), opened))).toEqual(["CORE-1", "doc-1", "doc-2", "comment-1"]);
  // A fold made inside the zoom is the same row's once zoomed back out.
  const zoomed = rowOf(treeRows(doc, NOTHING_OPENED, ["CORE-1"]), "comment-1");
  const madeInside = withFold(NOTHING_OPENED, placeOf(zoomed.trail, zoomed.node.id), false);
  expect(
    rowOf(treeRows(root, madeInside), "comment-1"),
    "the fold made inside the zoom was lost on the zoom out"
  ).toMatchObject({ expanded: false, folded: 1 });
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

// R15: the row that pages a node's children draws the next page under its own row. A node drawn
// under two parents has a page row under each, and a press on one draws the rest under that row
// alone.
test("the next page of a node's children is drawn under the row pressed alone", () => {
  const root = servedTwice(() => holding("thread", CHILD_PAGE + 7));
  const first = treeRows(root);
  const pages = first.filter((row) => row.kind === "page");
  expect(idsOf(pages)).toEqual(["page:thread:7", "page:thread:7"]);
  const pressed = pages[1];
  if (pressed?.kind !== "page") throw new Error("no page row under r2");
  const second = treeRows(root, withNextPage(NOTHING_OPENED, pressed));
  const left = second.filter((row) => row.kind === "page");
  expect(idsOf(left), "the press under r2 drew the next page under r1 too").toEqual([
    "page:thread:7",
  ]);
  expect(left[0]?.trail).toEqual(["CORE-1", "spec#b-r1"]);
  // The seven rows drawn take the place of the row that paged them.
  expect(second.length - first.length).toBe(6);
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

// R15: a row the page names by its place - the row the tab stop rests on - is drawn with every
// child before it at that place alone, and the same node's other placement keeps its first page.
// A node named by its id alone is drawn so where `pathTo` finds it, its first placement.
test("a row named past its parent's first 50 is drawn with every child before it, under that place alone", () => {
  const root = servedTwice(() => holding("thread", CHILD_PAGE + 7));
  const placed = treeRows(root, NOTHING_OPENED, [], [], ["CORE-1>spec#b-r2>thread>thread-55"]);
  const pages = placed.filter((row) => row.kind === "page");
  expect(idsOf(pages), "the row named under r2 drew more children under r1").toEqual([
    "page:thread:7",
    "page:thread:2",
  ]);
  expect(pages.map((row) => row.trail.at(-1))).toEqual(["spec#b-r1", "spec#b-r2"]);
  // Named by its id alone, the node is drawn at its first placement, under r1.
  const named = treeRows(root, NOTHING_OPENED, [], ["thread-55"]);
  expect(idsOf(named.filter((row) => row.kind === "page"))).toEqual([
    "page:thread:2",
    "page:thread:7",
  ]);
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
