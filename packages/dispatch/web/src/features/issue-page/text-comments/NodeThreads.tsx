import type { ReactNode } from "react";

import type { TreeNode } from "../issue-tree";

/** The attribute of the sheet that holds one node's threads below 1024 px. Built by t7. */
export const TEXT_THREADS_ATTRIBUTE = "data-text-threads";

/**
 * The sheet of one node's threads below 1024 px (R3): a `div` with `role="group"` inside the
 * node's row after its text, facts and acts and before its children's rows, as wide as the row,
 * in the order of the threads' words. Built by t7, mounted by each row site at its part's gate
 * (t14); it draws nothing until then.
 */
export function NodeThreads(_props: { readonly node: TreeNode }): ReactNode {
  return null;
}
