import { useMemo } from "react";

import { compareTimestamps } from "../../lib/timestamps";
import type { TreeNode } from "./issue-tree";
import { conversationItem, type OutlineWriter, useOutlineWriter } from "./outline-writer";
import type { NodeType } from "./page-words";
import type { DrawnRow, PageRow } from "./tree-fold";
import { rowKeyOf } from "./tree-keys";

/**
 * The conversation's current point on the issue page's outline: the one node the reader's next
 * turn answers, and the drawn rows that carry its mark. Pure but for `useCurrentPoint`, over the
 * tree `issue-tree.ts` builds and the rows `tree-fold.ts` draws, so the rows and a unit test ask
 * the same question of the same model.
 *
 * The point is, in this order: what the line at the foot is aimed at, while the reader has aimed
 * it (`OutlineWriter`): the entry a reply answers, or the statement a comment is on; else the
 * first open question that waits on the reader, which is the row the page opens on
 * (`IssueTree.tsx`); else the node of the newest act on the issue. A reader who has aimed the
 * line has said what their next turn answers, so nothing else is the point while they hold it,
 * not even a question that waits on them; with nothing aimed, a question that waits on them is
 * the turn that is theirs, whatever was said after it; and with neither, the conversation stands
 * where it last moved. An answer the writer holds aims nothing (`OutlineWriter.answer`).
 *
 * A node drawn under several parents is one placement per parent: every placement but its edited
 * place is transcluded, and the branch under a transcluded placement repeats the edited place's
 * own nodes. Every walk here passes a transcluded placement by, with all it holds, so the point is
 * found once and its path ends at its edited place: the mark is on one row, never on a copy.
 */

/** How a drawn row carries the current point: its own row (`here`), or, while its own row is
 *  folded away or stands past its parent's first page of children, a row above it on its path or
 *  the row that pages those children (`path`). A folded branch or a page still says where the
 *  conversation is, so the reader can open or page their way down to it. */
export type PointMark = "here" | "path";

/** The writer's two targets as `OutlineWriter` holds them (outline-writer.tsx): the entry the
 *  line replies to and the statement it comments on. Nothing else the writer holds - its kind, an
 *  answer, an edit, a landing - says what the reader's next turn answers. */
export type PointAim = Pick<OutlineWriter, "reply" | "statement">;

/** One edited placement of the tree, with the ids from the root down to it, its own id last. */
interface Placement {
  readonly node: TreeNode;
  readonly path: readonly string[];
}

/** Every edited placement of `root` in reading order, the root first: a parent before its
 *  children, the children in their own order, and no transcluded placement nor anything under
 *  it, since all of that repeats nodes whose edited place is elsewhere in the tree. */
function editedPlacements(root: TreeNode): Placement[] {
  const found: Placement[] = [];
  const walk = (node: TreeNode, above: readonly string[]): void => {
    if (node.transcluded) return;
    const path = [...above, node.id];
    found.push({ node, path });
    for (const child of node.children) walk(child, path);
  };
  walk(root, []);
  return found;
}

/**
 * Whether a node is what the line is aimed at, where the reader has aimed it; undefined while
 * they have not. The line holds one target at a time - setting either ends the other
 * (`OutlineWriter.statement`) - so the order they are read in here never decides between two.
 */
function aimedAt(aim: PointAim): ((node: TreeNode) => boolean) | undefined {
  const { reply, statement } = aim;
  // The statement's row, by the node id a held send keeps of it as well, so a line that mounted
  // again over a refused comment on a statement still points at that statement's row.
  if (statement !== undefined) return (node) => node.id === statement.row;
  // The entry by its own id and kind, which is all a reply carries of its row: a reply taken up
  // from a held send names no node id, and a comment and a message may share an id.
  if (reply !== null) {
    return (node) => {
      const item = conversationItem(node);
      return item !== undefined && item.id === reply.id && item.kind === reply.parentKind;
    };
  }
  return undefined;
}

/** The kinds whose node is an act on the issue among the rows the conversation draws: a comment,
 *  a question asked or answered, and a message, at the time the node carries. A block of a
 *  document is no turn of the conversation, and neither is a check's result, a decision's last
 *  change nor the issue's own creation. */
const ACTS: Readonly<Partial<Record<NodeType, true>>> = {
  ask: true,
  comment: true,
  message: true,
};

/**
 * The placement of the newest act on the issue: of the nodes `ACTS` names, the one made last, or
 * the ask answered last, by its answer's time. A tie goes to the node later in reading order,
 * which is the one drawn lower, nearer the line the reader writes in. Undefined where no act
 * carries a time.
 */
function newestAct(placements: readonly Placement[]): Placement | undefined {
  let newest: { readonly at: string; readonly placement: Placement } | undefined;
  const consider = (placement: Placement, at: string | undefined): void => {
    if (at !== undefined && (newest === undefined || compareTimestamps(at, newest.at) >= 0)) {
      newest = { at, placement };
    }
  };
  for (const placement of placements) {
    if (ACTS[placement.node.type] !== true) continue;
    consider(placement, placement.node.at);
    // An answer is part of its ask and no node of its own (`NodeAnswer`), so giving it is an act
    // on the ask's node; only an ask carries one.
    consider(placement, placement.node.answer?.at);
  }
  return newest?.placement;
}

/**
 * The ids from the root down to the current point, the root first; undefined where there is none.
 *
 * While the reader has aimed the line, the point is the node it is aimed at, and undefined where
 * the tree holds no such node - an entry not read yet, a statement a new version took out -
 * rather than a node the reader did not aim at. Else it is the first question in reading order
 * that waits on the reader, which is where the page opens (`IssueTree.tsx`); else the node of the
 * newest act on the issue (`newestAct`). The path never runs through a transcluded placement.
 */
export function currentPath(root: TreeNode, aim: PointAim): readonly string[] | undefined {
  const placements = editedPlacements(root);
  const aimed = aimedAt(aim);
  if (aimed !== undefined) return placements.find(({ node }) => aimed(node))?.path;
  const question = placements.find(({ node }) => node.type === "ask" && node.waiting);
  return (question ?? newestAct(placements))?.path;
}

/**
 * The depth on `path` of the node `row` stands for: its own id at that depth, and the ids it sits
 * under the path's ids above it, since one node is drawn under several parents and only its edited
 * placement is on the path. Undefined for a row that stands for none of them: a row of another
 * branch, a transcluded placement of a node on the path, and the row that pages a node's
 * children, which is no node's own row (`pagesPoint`).
 */
function depthOnPath(row: DrawnRow, path: readonly string[]): number | undefined {
  if (row.kind !== "node") return undefined;
  const depth = row.trail.length;
  if (row.node.id !== path[depth]) return undefined;
  return row.trail.every((id, index) => id === path[index]) ? depth : undefined;
}

/** Whether the point stands among the rows `row` pages: its node is on `path` at the placement
 *  the path runs through, as `depthOnPath` matches a node's row, and the path's next id is one of
 *  the children no row draws yet, so the point is that child or under it. */
function pagesPoint(row: PageRow, path: readonly string[]): boolean {
  const depth = row.trail.length;
  if (row.of.id !== path[depth] || !row.trail.every((id, index) => id === path[index])) {
    return false;
  }
  const { children } = row.of;
  return children.slice(children.length - row.left).some((child) => child.id === path[depth + 1]);
}

/**
 * Each drawn row that carries the mark, by its row key (`rowKeyOf` of tree-keys.ts). The point's
 * own row, where it is drawn, carries `here` and is the only row marked. Where it is folded away
 * or not drawn, every drawn row above it on its path carries `path` instead, and so does the row
 * that pages its parent's children where the point stands among them (`pagesPoint`), so the
 * reader sees which branch to open or which page to draw; a row of the path that is not drawn
 * carries nothing.
 *
 * The rows are matched by their trails, never by their place in the list, so the rows of a zoomed
 * outline match as the whole tree's do and the issue's own row may be missing (`IssueTree.tsx`
 * draws none for it).
 */
export function pointMarks(
  rows: readonly DrawnRow[],
  path: readonly string[] | undefined
): ReadonlyMap<string, PointMark> {
  const marks = new Map<string, PointMark>();
  if (path === undefined) return marks;
  const point = rows.find((row) => depthOnPath(row, path) === path.length - 1);
  if (point !== undefined) return marks.set(rowKeyOf(point), "here");
  for (const row of rows) {
    const marked =
      row.kind === "page" ? pagesPoint(row, path) : depthOnPath(row, path) !== undefined;
    if (marked) marks.set(rowKeyOf(row), "path");
  }
  return marks;
}

/** `currentPath` over `root` for the writer the page holds (`useOutlineWriter`), memoised on the
 *  root and the writer's two targets. The writer is a new object whenever any of its fields
 *  changes - its kind, an answer, an edit, a landing - and none of those moves the point, so the
 *  aim is built inside from the two targets alone. Outside an issue page's body there is no
 *  writer, and the point is what it is with nothing aimed. */
export function useCurrentPoint(root: TreeNode): readonly string[] | undefined {
  const writer = useOutlineWriter();
  const reply = writer?.reply ?? null;
  const statement = writer?.statement;
  return useMemo(() => currentPath(root, { reply, statement }), [root, reply, statement]);
}
