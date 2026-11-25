import { type RefObject, useEffect, useMemo, useRef, useState } from "react";

import type { TreeNode } from "../issue-tree";
import { rowSelectorOf } from "../tree-focus";
import { rowKeyOf } from "../tree-keys";
import { abortOnReaderAct } from "../useNodeEditing";
import { moveToRow } from "../useTreeKeymap";
import type { RowReveal } from "./row-edits";

/** A move of the node menu that was taken, until the outline draws its node under its new
 *  parent; then the key of the row that takes the focus once it is drawn. */
type Moved = { readonly id: string; readonly parent: string } | { readonly key: string };

/** The path from `node` down to the own row of the node `id`, its placement that is no
 *  transclusion (part T's second rows), or undefined where the tree under `node` holds none. */
function ownPathTo(node: TreeNode, id: string): TreeNode[] | undefined {
  if (node.id === id) return [node];
  for (const child of node.children) {
    if (child.transcluded) continue;
    const below = ownPathTo(child, id);
    if (below !== undefined) return [node, ...below];
  }
  return undefined;
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
 * new parent; in that render every branch above the node's own row is unfolded, a folded new
 * parent among them (`unfoldPath`, the reader's folds otherwise as they left them), and in the
 * next, which draws the row, the row takes the focus as a key's move gives it (`moveToRow`). The
 * row a move takes out of the tree drops the focus meanwhile, which the outline's drop rule gives
 * the nearest row; the moved row takes it back. The reader's first act since the move lets it go,
 * so a reader who moved on stays where they went.
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
  const [moved, setMoved] = useState<Moved | undefined>(undefined);
  const acts = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => acts.current?.abort(), []);
  useEffect(() => {
    if (moved === undefined) return;
    if ("key" in moved) {
      acts.current?.abort();
      setMoved(undefined);
      const row = body.current?.querySelector<HTMLElement>(rowSelectorOf(moved.key));
      if (row !== null && row !== undefined) moveToRow(row);
      return;
    }
    const path = ownPathTo(root, moved.id);
    const node = path?.at(-1);
    const parent = path?.at(-2);
    if (path === undefined || node === undefined || parent === undefined) return;
    // The issue is the tree's root, which the read names by its key, its `TreeNode.id`.
    if ((parent.readId ?? parent.id) !== moved.parent) return;
    const trail = path.slice(0, -1).map((above) => above.id);
    unfoldPath(trail);
    setMoved({ key: rowKeyOf({ kind: "node", level: path.length, node, trail }) });
  }, [body, moved, root, unfoldPath]);
  const latest = useRef({ reach, root });
  latest.current = { reach, root };
  return useMemo(
    (): RowReveal => ({
      moved: (id, parent) => {
        acts.current?.abort();
        acts.current = abortOnReaderAct(() => setMoved(undefined));
        setMoved({ id, parent });
      },
      toRow: (node) => {
        const found = nodeOfRead(latest.current.root, node);
        if (found !== undefined) latest.current.reach(found.id);
      },
    }),
    []
  );
}
