import { useLocation } from "react-router-dom";

import { inboxItemOf } from "../refs/page-routes";
import { READING_PANE_QUERY, useMediaQuery } from "../shell/useDialog";
import { inboxEntryState } from "./entry-state";

/**
 * Whether the Inbox page fills the viewport, rather than scrolling as one document, for the
 * shell and the page to size themselves by: from the reading column's width, where the list and
 * the column each scroll in themselves, and below it while the address names a row the reader
 * opened (`?item=`), whose reading view scrolls in a box of its own - a decision block is
 * scrolled to its section there, and an approval's Next change steps that box. A row the column
 * chose by itself is not one the reader opened (`inboxDefaultPick` on its history entry): below
 * the column's width the Inbox drops it from the address and shows the list.
 *
 * The shell reads this in the entry chunk, so it reads the address and the history entry through
 * the modules the entry holds (`refs/page-routes.ts`, `entry-state.ts`), and not through
 * `refs/routes.ts` or `pane-selection.ts`, which would bring the reference grammar and the
 * Inbox's selection into the entry (`MAX_ENTRY_BYTES` in `web/src/asset-sizes-in-build.ts`).
 */
export function useInboxFillsViewport(): boolean {
  const wide = useMediaQuery(READING_PANE_QUERY);
  const { search, state } = useLocation();
  return (
    wide ||
    (inboxItemOf(new URLSearchParams(search)) !== undefined &&
      inboxEntryState(state).inboxDefaultPick !== true)
  );
}
