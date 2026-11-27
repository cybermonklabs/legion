import { useQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";
import { useNavigate } from "react-router-dom";

import { whoAmIQuery } from "../../../api/queries";
import type { IssueDetails } from "../../../api/types";
import { isViewer } from "../../refs/actor";
import { buildIssuePath } from "../../refs/routes";
import type { TreeNode } from "../issue-tree";
import type { Preset } from "../page-words";
import { nodesInOrder } from "../tree-keys";
import { outlineOf } from "../tree-read/tree-read";
import type { NodeThreads } from "./text-threads";
import { threadCommentNode } from "./thread-node";
import { useTextThreads } from "./useTextThreads";

/**
 * What the Contents slot (`slots/contents.tsx`) adds for the reader's comments on text (R8, root
 * R14). The slot stands outside the stage, so outside the column's own answer: it builds the
 * threads of the outline read it already holds (`useTextThreads` watches the page's reads and
 * sends none), puts an entry for each thread the reader began in the tree it hands the list
 * (`withThreadEntries`), and sends a press on such an entry to the thread's address, as any
 * address of R8 opens it (`thread-address.ts`).
 */

/** What the slot passes the list besides what it passes it already: the tree with the entries in
 *  it. Optional in its type because it overrides the `root` the slot gave first. */
export interface ThreadEntryProps {
  readonly root?: TreeNode;
}

/** The entries the slot adds, and the way a press on one of them goes. */
export interface ThreadEntries {
  /** Opens the thread the entry `id` stands for, at its address, where `id` is such an entry:
   *  true. For every other entry it does nothing and answers false. */
  readonly go: (id: string) => boolean;
  readonly props: ThreadEntryProps;
}

/** The entries of the reader's comments on text of `issue` in `list`, the tree the Contents list
 *  is given (Conversation's view of `preset`'s outline read), and where a press on one goes. */
export function useThreadEntries(
  issue: IssueDetails,
  list: TreeNode,
  preset: Preset
): ThreadEntries {
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const view = useTextThreads(issue, outlineOf(preset) ?? "conversation");
  const key = issue.key;
  const { kinds, root } = useMemo(
    () => withThreadEntries(list, view.threads, viewer, key),
    [key, list, view.threads, viewer]
  );
  const navigate = useNavigate();
  const go = useCallback(
    (id: string): boolean => {
      const kind = kinds.get(id);
      if (kind === undefined) return false;
      navigate(buildIssuePath({ id, key, kind }));
      return true;
    },
    [key, kinds, navigate]
  );
  const props = useMemo(() => ({ root }), [root]);
  return { go, props };
}

/** The Contents list's entries for the comments on text the reader wrote (R8, root R14): the tree
 *  with those entries in it, and what each entry opens. */
interface ThreadEntryTree {
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
 * words: so the list, which reads the rule of its entries (`contents-entries.ts`'
 * `isContentsEntry`), draws it in the tree's order under the nearest entry above that node, and
 * none for a reply in a thread. A thread that stands by a node `root` holds none of -
 * Conversation draws no row of a plan task with no step - has its entry under the node it hangs
 * from (`NodeThreads.parent`) instead. A node that is a comment or a message takes no entry as a
 * child, which the rule refuses as a reply: its entries follow it, in its parent's place, under
 * the nearest entry above it. The entries are no row: `held` stays as it was, and `root` is the
 * same object where no entry is added.
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
