import { moveTableStore, renameStoredColumns } from "../project/issues-table-storage";
import { BACKLOG_LAYOUT_KEY, PRIORITY_FORMER_IDS, PRIORITY_LAYOUT_KEY } from "./priorities-model";

/** The key the list of priority work in backlog or icebox kept its layout under in the release
 *  before, which no table reads now (`BACKLOG_LAYOUT_KEY`). */
const FORMER_BACKLOG_LAYOUT_KEY = "priorities-parked";

/**
 * Moves the layouts the priorities page's two tables kept under the names of the release before
 * to the names they have now, so a reader's widths, places and visibility outlive the renames:
 * the list of priority work in backlog or icebox from the key it had to `BACKLOG_LAYOUT_KEY`
 * (`moveTableStore`), and the priorities table's backlog column, under `PRIORITY_LAYOUT_KEY`,
 * from the id it had to its id now (`PRIORITY_FORMER_IDS`, `renameStoredColumns`).
 *
 * Once, and before anything renders: a table reads its layout under the names it has now alone,
 * and its first write writes that layout whole, so a column stored under its old id would be
 * dropped as none of the table's, and a layout under the list's old key would never be read.
 * A second call finds nothing under the old names and writes nothing. `main.tsx` calls it before
 * the root renders, so the shared table hooks know no older name and write nothing while they
 * render.
 */
export function moveFormerPriorityLayouts(): void {
  moveTableStore(FORMER_BACKLOG_LAYOUT_KEY, BACKLOG_LAYOUT_KEY);
  renameStoredColumns(PRIORITY_LAYOUT_KEY, PRIORITY_FORMER_IDS);
}
