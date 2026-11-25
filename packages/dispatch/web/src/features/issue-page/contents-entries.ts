import { isViewer } from "../refs/actor";
import type { TreeNode } from "./issue-tree";
import type { NodeThreads } from "./text-comments/text-threads";
import { threadCommentNode } from "./text-comments/thread-node";
import { nodesInOrder } from "./tree-keys";

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
