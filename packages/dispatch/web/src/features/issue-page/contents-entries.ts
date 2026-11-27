import { isViewer } from "../refs/actor";
import type { TreeNode } from "./issue-tree";

/**
 * Whether `child`, drawn under `parent`, is an entry of the Contents list (R2). An ask is one. A
 * comment the reader wrote that begins a thread is one; a reply inside a thread - a comment or a
 * message under a comment or a message - is not. A message the reader wrote that begins a thread
 * is one only where it is one of `producing`, the messages under which the whole tree places a
 * node that is no message (`producingMessages`): a thread of messages alone produced no node, so
 * it is no entry. That is read over the whole tree and never over the one a words filter left,
 * which drops the node a matching message produced. A transcluded copy is never one, and a copy
 * under a message thread does not make the thread one: it is drawn at a second place, and was
 * not produced there. The Contents list reads this one rule (`IssueContents.tsx`'s `contentsOf`).
 */
export function isContentsEntry(
  parent: TreeNode,
  child: TreeNode,
  viewer: string | undefined,
  producing: ReadonlySet<string>
): boolean {
  if (child.transcluded) return false;
  if (child.type === "ask") return true;
  if (child.type !== "comment" && child.type !== "message") return false;
  if (parent.type === "comment" || parent.type === "message") return false;
  if (child.author === undefined || !isViewer(child.author, viewer)) return false;
  return child.type === "comment" || producing.has(child.id);
}

/** The ids of the messages of `root`, the whole tree, that begin a thread - under no comment and
 *  no message, the only messages `isContentsEntry` reads - under which or under a reply in whose
 *  thread the tree places a node that is no message, at any depth (`placesNode`). Only a
 *  thread's first message is tested, so each thread is read once and the walk is linear in the
 *  tree. A transcluded copy and the branch under it were produced at their own place, so none of
 *  them is read. */
export function producingMessages(root: TreeNode): ReadonlySet<string> {
  const ids = new Set<string>();
  const walk = (node: TreeNode): void => {
    const inThread = node.type === "comment" || node.type === "message";
    for (const child of node.children) {
      if (child.transcluded) continue;
      if (!inThread && child.type === "message" && placesNode(child)) ids.add(child.id);
      walk(child);
    }
  };
  walk(root);
  return ids;
}

/** Whether the tree places a node that is no message under `message` or under a reply in its
 *  thread, at any depth. A transcluded copy, and the branch under it, were produced at their own
 *  place, so neither counts. */
function placesNode(message: TreeNode): boolean {
  return message.children.some(
    (child) => !child.transcluded && (child.type !== "message" || placesNode(child))
  );
}
