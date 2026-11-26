import { isViewer } from "../refs/actor";
import type { TreeNode } from "./issue-tree";
import type { NodeThreads } from "./text-comments/text-threads";
import { threadCommentNode } from "./text-comments/thread-node";
import { nodesInOrder } from "./tree-keys";

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

/** The Contents list's entries for the comments on text the reader wrote (R8, root R14): the tree
 *  with those entries in it, and what each entry opens. */
export interface ThreadEntryTree {
  /** The kind each entry of a thread's root is, by its id: what a press on it opens
   *  (`/comments/<id>` or `/messages/<id>`, the address of R8). */
  readonly kinds: ReadonlyMap<string, "comment" | "message">;
  readonly root: TreeNode;
}

/** What one node gives the walk of `withThreadEntries`: the node, with its entries drawn in, and
 *  the entries that stand after it, in its parent's place, where it is a comment or a message. */
interface WithEntries {
  readonly after: readonly TreeNode[];
  readonly node: TreeNode;
}

/**
 * The tree `root` with an entry for each comment on text the reader wrote - the root of a thread
 * of `threads`, by node (`textThreadsOf`), which no row of the outline draws (R1) - as a node
 * under the node its thread stands by, first among its children, in the order of the threads'
 * words: so the list, which reads the rule above, draws it in the tree's order under the nearest
 * entry above that node, and none for a reply in a thread. A thread that stands by a node `root`
 * holds none of - Conversation draws no row of a plan task with no step - has its entry under the
 * node it hangs from (`NodeThreads.parent`) instead. A node that is a comment or a message takes
 * no entry as a child, which the rule refuses as a reply: its entries follow it, in its parent's
 * place, under the nearest entry above it. The entries are no row: `held` stays as it was, and
 * `root` is the same object where no entry is added.
 *
 * `viewer` is the reader's login, and `key` the issue, which an entry's address names where its
 * comment names none.
 */
export function withThreadEntries(
  root: TreeNode,
  threads: ReadonlyMap<string, NodeThreads>,
  viewer: string | undefined,
  key: string
): ThreadEntryTree {
  const kinds = new Map<string, "comment" | "message">();
  const entries = new Map<string, TreeNode[]>();
  const present =
    threads.size === 0 ? new Set<string>() : new Set(nodesInOrder(root).map((node) => node.id));
  for (const [nodeId, { parent, threads: held }] of threads) {
    const host = present.has(nodeId) ? nodeId : parent;
    if (!present.has(host)) continue;
    for (const { kind, thread } of held) {
      const comment = thread.root.comment;
      if (!isViewer(comment.author, viewer)) continue;
      kinds.set(comment.id, kind);
      const own = entries.get(host);
      const entry = threadCommentNode(comment, kind, key);
      if (own === undefined) entries.set(host, [entry]);
      else own.push(entry);
    }
  }
  if (entries.size === 0) return { kinds, root };
  const place = (node: TreeNode): WithEntries => {
    const own = entries.get(node.id) ?? [];
    const item = node.type === "comment" || node.type === "message";
    const children: TreeNode[] = [];
    const bubbled: TreeNode[] = [];
    let changed = false;
    for (const child of node.children) {
      const placed = place(child);
      children.push(placed.node);
      changed ||= placed.node !== child || placed.after.length > 0;
      if (item) bubbled.push(...placed.after);
      else children.push(...placed.after);
    }
    if (item) return { after: [...own, ...bubbled], node: changed ? { ...node, children } : node };
    if (own.length === 0 && !changed) return { after: [], node };
    return { after: [], node: { ...node, children: [...own, ...children] } };
  };
  return { kinds, root: place(root).node };
}
