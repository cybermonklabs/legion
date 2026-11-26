import { useLocation } from "react-router-dom";

import { READING_PANE_QUERY, useMediaQuery } from "../shell/useDialog";
import { parseInboxSearch } from "./inbox-route";
import { inboxEntryState } from "./pane-selection";

/**
 * Whether the Inbox page fills the viewport, rather than scrolling as one document, for the
 * shell and the page to size themselves by: from the reading column's width, where the list and
 * the column each scroll in themselves, and below it while the address names a row the reader
 * opened (`?item=`), whose reading view scrolls in a box of its own - a decision block is
 * scrolled to its section there, and an approval's Next change steps that box. A row the column
 * chose by itself is not one the reader opened (`inboxDefaultPick` on its history entry): below
 * the column's width the Inbox drops it from the address and shows the list.
 */
export function useInboxFillsViewport(): boolean {
  const wide = useMediaQuery(READING_PANE_QUERY);
  const { search, state } = useLocation();
  return (
    wide ||
    (parseInboxSearch(search).item !== undefined &&
      inboxEntryState(state).inboxDefaultPick !== true)
  );
}
