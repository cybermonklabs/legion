import { useCallback } from "react";

import { DETAILS_ATTRIBUTE, useMarginDrawer } from "./margin-slot";
import { firstUsableIn, usableControl } from "./usable-control";

/** The attribute each row of Details carries, by the property it holds (`IssueMargin.tsx`), so
 *  a control elsewhere on the page can open Details at that row. */
export const DETAILS_ROW_ATTRIBUTE = "data-details-row";

/** The attribute that marks, in a row whose editor Details draws itself (Parent, Messages go
 *  to), the control that opens that editor, with the value `"control"`, and, while the editor is
 *  open in that control's place, the editor, with `"editor"`. A press at the row
 *  (`useOpenDetailsAt`) reaches the one or the other and no other control of the row, such as
 *  the parent's link, which stands before the editor. */
export const DETAILS_OPENER_ATTRIBUTE = "data-details-opener";

/** The rows of Details a control elsewhere on the page opens it at: the labels the header's
 *  count leaves over, the two properties whose editor opens in their row, and the issue's
 *  collaborators, which `t o` brings the reader to (the key table). */
export type DetailsRow = "collaborators" | "labels" | "parent" | "route";

/** The event the opener sends on Details' `aside` before it looks for the row, with the row as
 *  its `detail`: the section of Details that holds the row opens on it, so a row inside a
 *  closed section, which is hidden, is shown and can take the focus. */
export const DETAILS_ROW_REQUEST_EVENT = "details-row-request";

/**
 * Opens Details at one of its rows: the drawer opens, it is asked for the row
 * (`DETAILS_ROW_REQUEST_EVENT`), the row is brought into its view, and the focus goes to the
 * row's first control that takes a press (`usableControl`), so the reader is where the control
 * that sent them said. With `press` a control is pressed instead, as the labels key presses
 * Edit labels. In a row that marks the control that opens its editor (`DETAILS_OPENER_ATTRIBUTE`)
 * that control is pressed, and nothing while it takes no press. Where that row draws its open
 * editor instead, which Details keeps open while it is closed, nothing is pressed and the focus
 * goes to the editor's first control that takes a press, its box: a script's click puts no focus
 * in a box, and the row's other controls lead elsewhere, the parent's link to the parent issue.
 * A row that marks nothing, as Labels, whose control is a shared one, has its first control
 * that takes a press pressed. The focus is placed two frames on, after the drawer has given it
 * to its own Close as it opens (`Margin.tsx`) and after the row's editor, where one opens, has
 * mounted. Outside a shell that draws Details the panel is in the page's own column, and the
 * row is reached there.
 */
export function useOpenDetailsAt(): (row: DetailsRow, press?: boolean) => void {
  const drawer = useMarginDrawer();
  return useCallback(
    (row: DetailsRow, press = false) => {
      drawer?.setOpen(true);
      if (drawer !== undefined) {
        document
          .querySelector<HTMLElement>(`aside[${DETAILS_ATTRIBUTE}]`)
          ?.dispatchEvent(new CustomEvent<DetailsRow>(DETAILS_ROW_REQUEST_EVENT, { detail: row }));
      }
      requestAnimationFrame(() =>
        requestAnimationFrame(() => {
          const found = document.querySelector<HTMLElement>(
            `${drawer === undefined ? "" : `aside[${DETAILS_ATTRIBUTE}] `}[${DETAILS_ROW_ATTRIBUTE}="${row}"]`
          );
          if (found === null) return;
          found.scrollIntoView({ block: "nearest" });
          if (!press) {
            firstUsableIn(found)?.focus({ preventScroll: true });
            return;
          }
          const opener = found.querySelector<HTMLElement>(`[${DETAILS_OPENER_ATTRIBUTE}]`);
          if (opener === null) {
            firstUsableIn(found)?.click();
          } else if (opener.getAttribute(DETAILS_OPENER_ATTRIBUTE) === "editor") {
            firstUsableIn(opener)?.focus({ preventScroll: true });
          } else if (usableControl(opener)) {
            opener.click();
          }
        })
      );
    },
    [drawer]
  );
}
