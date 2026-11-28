import { isViewer } from "../refs/actor";
import type { TreeNode } from "./issue-tree";

/**
 * Whether `child`, drawn under `parent`, is an entry of the Contents list: an ask, or a comment
 * or message the reader wrote that begins a thread, which a comment or a message it answers does
 * not. A transcluded copy is never one: a node drawn under a second parent is an entry at its
 * own place alone. The list that draws the entries and the count below read this one rule.
 */
export function isContentsEntry(
  parent: TreeNode,
  child: TreeNode,
  viewer: string | undefined
): boolean {
  if (child.transcluded) return false;
  if (child.type === "ask") return true;
  return (
    (child.type === "comment" || child.type === "message") &&
    parent.type !== "comment" &&
    parent.type !== "message" &&
    child.author !== undefined &&
    isViewer(child.author, viewer)
  );
}

/**
 * How many entries the Contents list draws for `node`'s branch (`isContentsEntry`), at any
 * depth. A transcluded copy and the branch under it are the same nodes as at the edited place,
 * so neither is counted again. Conversation's count in the view row is this number
 * (`slots/view-row.tsx`).
 */
export function contentsEntries(node: TreeNode, viewer: string | undefined): number {
  let entries = 0;
  for (const child of node.children) {
    if (child.transcluded) continue;
    if (isContentsEntry(node, child, viewer)) entries += 1;
    entries += contentsEntries(child, viewer);
  }
  return entries;
}
