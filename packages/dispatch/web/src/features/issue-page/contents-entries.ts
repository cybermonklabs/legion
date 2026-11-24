import { isViewer } from "../refs/actor";
import type { TreeNode } from "./issue-tree";

/**
 * Whether `child`, drawn under `parent`, is an entry of the Contents list (R2). An ask is one. A
 * comment the reader wrote that begins a thread is one; a reply inside a thread - a comment or a
 * message under a comment or a message - is not. A message the reader wrote that begins a thread
 * is one only where the tree places a node that is no message under it or under a reply in its
 * thread, at any depth (`placesNode`): a thread of messages alone produced no node, so it is no
 * entry. A transcluded copy is never one, and a copy under a message thread does not make the
 * thread one: it is drawn at a second place, and was not produced there. The Contents list reads
 * this one rule (`IssueContents.tsx`'s `contentsOf`).
 */
export function isContentsEntry(
  parent: TreeNode,
  child: TreeNode,
  viewer: string | undefined
): boolean {
  if (child.transcluded) return false;
  if (child.type === "ask") return true;
  if (child.type !== "comment" && child.type !== "message") return false;
  if (parent.type === "comment" || parent.type === "message") return false;
  if (child.author === undefined || !isViewer(child.author, viewer)) return false;
  return child.type === "comment" || placesNode(child);
}

/** Whether the tree places a node that is no message under `message` or under a reply in its
 *  thread, at any depth. A transcluded copy, and the branch under it, were produced at their own
 *  place, so neither counts. */
function placesNode(message: TreeNode): boolean {
  return message.children.some(
    (child) => !child.transcluded && (child.type !== "message" || placesNode(child))
  );
}
