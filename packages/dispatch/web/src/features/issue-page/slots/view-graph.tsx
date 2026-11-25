import { type ReactNode, useEffect } from "react";

import { useOutlineReads } from "../outline-reads";
import { BodySlot } from "./body";
import type { ViewSlotProps } from "./stage";

/**
 * Graph's slot (R10): what the stage draws in place of the outline with Graph on, the last
 * control of the view row. It draws Graph's read - the tree read with no view, which every reader
 * of the outline takes in Graph (`tree-read/tree-read.ts`' `outlineOf`) - whole, as one outline,
 * through the outline's own rows (`BodySlot`, `IssueTree.tsx`, `TreeRows.tsx`, `NodeRow.tsx`):
 * every node of the read the page draws as a row, each under the parent the model stores, and
 * each check and plan task also under each other requirement it serves, the one node
 * (`presets.ts`' `presetView` of `graph`). The memo's callout is no row, and a comment on a
 * node's text is none either (`tree-read/tree-nodes.ts`). It keeps the editing every outline has,
 * Enter and each node's reply control, and draws no version control: Graph is a view and no
 * filter. `/graph/<node id>` opens it zoomed to that node (`IssueAddress.zoom`).
 *
 * The outline lands on the node an address names, or says in its place that the issue holds
 * none, as the outline does, so once its reads have settled it tells the stage its tree draws
 * the address (`ViewSlotProps.onAddressDrawn`), and the stage keeps it.
 */
export function ViewGraphSlot({ onAddressDrawn, ...props }: ViewSlotProps): ReactNode {
  const { readsSettled } = useOutlineReads(props.issue);
  useEffect(() => {
    if (readsSettled) onAddressDrawn(true);
  }, [onAddressDrawn, readsSettled]);
  return <BodySlot {...props} />;
}
