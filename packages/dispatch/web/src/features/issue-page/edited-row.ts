import { createContext, useContext, useSyncExternalStore } from "react";

import { createHeld, type Held } from "./held-value";
import type { EditedRow } from "./useNodeEditing";

/** No row of a tree is edited: what a row drawn outside a tree reads (`useEditedRow`). */
const NO_EDITED_ROW: Held<EditedRow | undefined> = createHeld<EditedRow | undefined>(undefined);

/**
 * The row of the tree the reader is editing in, which the tree holds below its own state
 * (`IssueTree.tsx`'s `RowEditing`) and each of its rows reads for itself (`useEditedRow`), so the
 * edit starting, moving and ending draws the rows it changes and no other.
 */
export const EditedRowContext = createContext<Held<EditedRow | undefined>>(NO_EDITED_ROW);

/** The document the row `rowKey` is being edited in, or undefined while it is not the row being
 *  edited: the reading of the nearest tree's edited row that changes for this row alone. */
export function useEditedRow(rowKey: string): string | undefined {
  const edited = useContext(EditedRowContext);
  return useSyncExternalStore(edited.subscribe, () => {
    const row = edited.get();
    return row?.row === rowKey ? row.artifactId : undefined;
  });
}
