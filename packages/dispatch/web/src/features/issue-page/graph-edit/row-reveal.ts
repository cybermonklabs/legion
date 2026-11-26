import { type RefObject, useEffect, useMemo, useRef, useState } from "react";

import type { TreeNode } from "../issue-tree";
import { focusIsLoose, rowSelectorOf } from "../tree-focus";
import { rowKeyOf } from "../tree-keys";
import { abortOnReaderAct } from "../useNodeEditing";
import { moveToRow } from "../useTreeKeymap";
import type { RowDelete, RowReveal } from "./row-edits";

/** What the outline waits for to take the reader to a row of a node `id` (`TreeNode.id`): a move
 *  that was taken, until the outline draws the node under its new parent (`moved`); a write of the
 *  node that was refused or dropped, until a read draws the node's own row under another key than
 *  `from`, the row the focus was on (`kept`); then the key of the row that takes the focus once
 *  it is drawn (`focus`). */
type Pending =
  | { readonly kind: "moved"; readonly id: string; readonly parent: string }
  | { readonly kind: "kept"; readonly id: string; readonly from: string }
  | { readonly kind: "focus"; readonly key: string };

interface DeleteWatch extends RowDelete {
  readonly update: (root: TreeNode) => void;
}

const NODE_ROW = "[data-node-row]";

/** The path from `node` down to the own row of the first node under it, in the walk's order, that
 *  `match` holds for: its placement that is no transclusion (part T's second rows). Undefined
 *  where the tree under `node` holds none. */
export function ownPathWhere(
  node: TreeNode,
  match: (node: TreeNode) => boolean
): TreeNode[] | undefined {
  if (match(node)) return [node];
  for (const child of node.children) {
    if (child.transcluded) continue;
    const below = ownPathWhere(child, match);
    if (below !== undefined) return [node, ...below];
  }
  return undefined;
}

/** The path from `node` down to the own row of the node `id`, its placement that is no
 *  transclusion (part T's second rows), or undefined where the tree under `node` holds none. */
export function ownPathTo(node: TreeNode, id: string): TreeNode[] | undefined {
  return ownPathWhere(node, (each) => each.id === id);
}

/** The node the tree read gives the read id `readId`, in the tree under `node`. */
function nodeOfRead(node: TreeNode, readId: string): TreeNode | undefined {
  if (node.readId === readId) return node;
  for (const child of node.children) {
    const found = nodeOfRead(child, readId);
    if (found !== undefined) return found;
  }
  return undefined;
}

/**
 * How Graph's node menu (`graph-edit/RowMenu.tsx`) takes the reader to a row of the outline
 * (`IssueTree.tsx`, whose `body`, `reach`, `root` and `unfoldPath` these are).
 *
 * After a taken move (R4), the outline is drawn from the held read the move's answer was written
 * into, in a render after the answer. So `moved` waits for the tree that draws the node under its
 * new parent. After a write of the node that was refused or dropped, with the focus on the node's
 * row (R8: the focus stays where it was), `kept` waits for a read that draws the node's own row
 * under another key than the one the focus was on - a `412 PRECONDITION_FAILED` whose read again
 * draws the node where the server holds it, under another parent - and never for a read that
 * lacks the node, whose row's focus the outline's drop rule gives the nearest row, as for every
 * drop. In the render that draws the node's row, the row takes the focus as a key's move gives it
 * (`moveToRow`), before the drop rule, which runs after this in the same commit and leaves a
 * focus a row holds where it is; where a folded branch above it keeps the row from being drawn,
 * every branch above the node's own row is unfolded, a folded new parent among them
 * (`unfoldPath`, the reader's folds otherwise as they left them), and the row takes the focus in
 * the next render, which draws it. The row a move takes out of the tree drops the focus meanwhile,
 * which the drop rule gives the nearest row; the node's row takes it back. The reader's first act
 * since lets it go, so a reader who moved on stays where they went.
 *
 * A delete confirm remembers where its row was drawn before it took focus. On success or a gone
 * refusal, it checks again after the dialog restores focus. If the row is still drawn, the same
 * check runs when the outline next changes. Neither path depends on the outline's `heldRow`.
 *
 * `toRow` takes the reader to the row of a node by its read id, as the outline's keys do
 * (`reach`: the row where it is drawn, else the path to it unfolded and drawn).
 */
export function useRowReveal({
  body,
  reach,
  root,
  unfoldPath,
}: {
  readonly body: RefObject<HTMLElement | null>;
  readonly reach: (id: string) => void;
  readonly root: TreeNode;
  readonly unfoldPath: (above: readonly string[]) => void;
}): RowReveal {
  const [pending, setPending] = useState<Pending | undefined>(undefined);
  const acts = useRef<AbortController | undefined>(undefined);
  const deleting = useRef<DeleteWatch | undefined>(undefined);
  useEffect(
    () => () => {
      acts.current?.abort();
      deleting.current?.cancel();
    },
    []
  );
  useEffect(() => {
    deleting.current?.update(root);
    if (pending === undefined) return;
    const rowOf = (key: string) => body.current?.querySelector<HTMLElement>(rowSelectorOf(key));
    if (pending.kind === "focus") {
      acts.current?.abort();
      setPending(undefined);
      const row = rowOf(pending.key);
      if (row !== null && row !== undefined) moveToRow(row);
      return;
    }
    const path = ownPathTo(root, pending.id);
    const node = path?.at(-1);
    const parent = path?.at(-2);
    if (path === undefined || node === undefined || parent === undefined) {
      // A read that lacks the node: its row leaves, and the drop rule gives the focus on.
      if (pending.kind === "kept") {
        acts.current?.abort();
        setPending(undefined);
      }
      return;
    }
    const trail = path.slice(0, -1).map((above) => above.id);
    const key = rowKeyOf({ kind: "node", level: path.length, node, trail });
    // The issue is the tree's root, which the read names by its key, its `TreeNode.id`.
    if (pending.kind === "moved" && (parent.readId ?? parent.id) !== pending.parent) return;
    if (pending.kind === "kept" && key === pending.from) return;
    const row = rowOf(key);
    if (row !== null && row !== undefined) {
      acts.current?.abort();
      setPending(undefined);
      moveToRow(row);
      return;
    }
    unfoldPath(trail);
    setPending({ key, kind: "focus" });
  }, [body, pending, root, unfoldPath]);
  const latest = useRef({ body, reach, root });
  latest.current = { body, reach, root };
  return useMemo((): RowReveal => {
    const wait = (next: Pending) => {
      acts.current?.abort();
      acts.current = abortOnReaderAct(() => setPending(undefined));
      setPending(next);
    };
    return {
      deleting: (readId) => {
        deleting.current?.cancel();
        const tree = latest.current.body.current;
        const row = document.activeElement?.closest<HTMLElement>(NODE_ROW);
        if (tree === null || row?.dataset.nodeReadId !== readId || !tree.contains(row)) return;
        const key = row.dataset.rowKey;
        let index = [...tree.querySelectorAll<HTMLElement>(NODE_ROW)].indexOf(row);
        let reader: AbortController | undefined;
        const watch: DeleteWatch = {
          cancel: () => {
            reader?.abort();
            if (deleting.current === watch) deleting.current = undefined;
          },
          gone: () => {
            if (deleting.current !== watch) return;
            reader = abortOnReaderAct(watch.cancel);
            watch.update(latest.current.root);
          },
          update: (drawnRoot) => {
            const rows = [...tree.querySelectorAll<HTMLElement>(NODE_ROW)];
            const at = rows.findIndex((row) => row.dataset.rowKey === key);
            if (at >= 0) index = at;
            if (reader === undefined) return;
            const loose = focusIsLoose(tree);
            if (
              !loose &&
              document.activeElement?.closest<HTMLElement>(NODE_ROW)?.dataset.nodeReadId !== readId
            ) {
              watch.cancel();
              return;
            }
            if (at >= 0 || nodeOfRead(drawnRoot, readId) !== undefined) return;
            watch.cancel();
            // As in IssueTree, use the row now at that index, or the last row if it was at the end.
            const nearest = rows[Math.min(index, rows.length - 1)];
            if (nearest !== undefined && loose) moveToRow(nearest);
          },
        };
        deleting.current = watch;
        return watch;
      },
      kept: (readId) => {
        const id = nodeOfRead(latest.current.root, readId)?.id;
        const focused = document.activeElement?.closest<HTMLElement>(NODE_ROW);
        const from = focused?.dataset.rowKey;
        // Only the focus on a row of the node is kept: a reader who moved it elsewhere keeps it
        // where they put it.
        if (id === undefined || from === undefined || focused?.dataset.nodeId !== id) return;
        if (latest.current.body.current?.contains(focused) !== true) return;
        wait({ from, id, kind: "kept" });
      },
      moved: (id, parent) => wait({ id, kind: "moved", parent }),
      toRow: (node) => {
        const found = nodeOfRead(latest.current.root, node);
        if (found !== undefined) latest.current.reach(found.id);
      },
    };
  }, []);
}
