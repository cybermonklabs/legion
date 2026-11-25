import type { TreeNode } from "./issue-tree";

/** The issue's summary: the first `callout` block of its spec, which is the nearest thing
 *  today's block schema holds to a summary node, which it does not hold yet. The outline
 *  draws no row of it (`slots/body.tsx`), and the Spec view draws it as the issue's three
 *  sentences. It is found in the outline's tree, and where that tree holds none - a Spec view
 *  loaded cold below 1024 px sends no outline read - in the tree the Spec view's own answer
 *  builds (`TreeViews.treeOf`), which holds the memo. */
export function specSummary(root: TreeNode, specSlug: string): TreeNode | undefined {
  const prefix = `${specSlug}#b-`;
  const walk = (node: TreeNode): TreeNode | undefined => {
    if (node.type === "callout" && node.id.startsWith(prefix)) return node;
    for (const child of node.children) {
      const found = walk(child);
      if (found !== undefined) return found;
    }
    return undefined;
  };
  if (specSlug === "") return undefined;
  const memo = walk(root);
  if (memo !== undefined) return memo;
  const spec = root.views?.treeOf?.("spec");
  return spec === undefined ? undefined : walk(spec);
}
