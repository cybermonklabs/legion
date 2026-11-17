import { useMemo } from "react";

import { type AddressTarget, type IssueAddress, pathToTarget } from "./issue-address";
import type { TreeNode } from "./issue-tree";

/** The node an address names, read against the tree (`useAddressTarget`). Each field is one
 *  value for as long as the address and the tree say the same; the object holding them is new
 *  on every render. */
export interface AddressedNode {
  /** The id of the node the address names, or nothing: what a view keeps whatever its filter
   *  or its words match (`presets.ts`'s `presetView`). */
  readonly named: ReadonlySet<string>;
  /** The chain down to the node the address names, the issue first, which the tree unfolds and
   *  puts the focus on the last node of; undefined where the address names no node or the
   *  tree holds none it names. */
  readonly reveal: readonly TreeNode[] | undefined;
  /** The node the address names, as the address says it. */
  readonly target: AddressTarget | undefined;
}

/**
 * The node `address` names, resolved against the whole tree `root` - the page's and every
 * filter's - so a view keeps it even where its filter or its words would not have matched it.
 * Every target an address names, `/children` among them, is one a tree unfolds and focuses.
 *
 * The target is read by what it says rather than by the value `issueAddress` built, which is a
 * new one on every render: the tree is walked again only when the address or the tree changes.
 */
export function useAddressTarget(
  root: TreeNode,
  address: Pick<IssueAddress, "target">
): AddressedNode {
  const targetKind = address.target?.kind;
  const targetId = address.target?.id;
  // Which document a block target belongs to, where its address names one: a block id alone
  // names a block of any document of the issue (`pathToTarget`).
  const targetDocument = address.target?.kind === "block" ? address.target.document : undefined;
  const target = useMemo<AddressTarget | undefined>(() => {
    if (targetKind === undefined || targetId === undefined) return undefined;
    if (targetKind === "block") return { document: targetDocument, id: targetId, kind: "block" };
    return { id: targetId, kind: targetKind } as AddressTarget;
  }, [targetDocument, targetId, targetKind]);
  const reveal = useMemo(() => pathToTarget(root, target), [root, target]);
  const named = useMemo(() => {
    const node = reveal?.at(-1);
    return new Set(node === undefined ? [] : [node.id]);
  }, [reveal]);
  return { named, reveal, target };
}
