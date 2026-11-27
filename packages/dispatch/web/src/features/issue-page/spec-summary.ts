import type { TreeNode } from "./issue-tree";
import type { TreeAnswer } from "./tree-read/tree-read";

/** The issue's summary: the first `callout` block of its spec, which is the nearest thing
 *  today's block schema holds to a summary node, which it does not hold yet. The outline
 *  draws no row of it (`slots/body.tsx`), and the Spec view draws it as the issue's three
 *  sentences. It is found in the outline's tree, and where that tree holds none - a Spec view
 *  loaded cold below 1024 px sends no outline read - in the tree the Spec view's own answer
 *  builds (`TreeViews.trees`), which holds the memo. That tree is the Spec view's whole build,
 *  and the outline's tree is made anew at each read's answer, so the summary is found in it once
 *  for each answer of the Spec view's read (`summaries`), and not built again with every read. */
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
  const answer = root.views?.spec;
  const held = answer === undefined ? undefined : summaries.get(answer);
  if (held !== undefined && held.slug === specSlug) return held.node;
  const spec = root.views?.trees?.spec;
  if (spec === undefined) return undefined;
  const node = walk(spec);
  if (answer !== undefined) summaries.set(answer, { node, slug: specSlug });
  return node;
}

/** The summary found in the tree each answer of the Spec view's read builds, with the spec's
 *  slug it was found under. */
const summaries = new WeakMap<
  TreeAnswer,
  { readonly node: TreeNode | undefined; readonly slug: string }
>();
