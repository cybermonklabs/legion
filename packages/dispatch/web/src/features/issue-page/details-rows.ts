import { useCallback } from "react";

import { placeInView } from "../doc/readers-place";
import { DETAILS_ATTRIBUTE, useMarginDrawer } from "./margin-slot";

/** The attribute each row of Details carries, by the property it holds (`IssueMargin.tsx`), so
 *  a control elsewhere on the page can open Details at that row. */
export const DETAILS_ROW_ATTRIBUTE = "data-details-row";

/** The rows of Details a control elsewhere on the page opens it at: the labels the header's
 *  count leaves over, the two properties whose editor opens in their row, and the issue's
 *  collaborators, which `t o` brings the reader to (the key table). */
export type DetailsRow = "collaborators" | "labels" | "parent" | "route";

/** The event the opener sends on Details' `aside` before it looks for the row, with the row as
 *  its `detail`: the section of Details that holds the row opens on it, so a row inside a
 *  closed section is there to be found. */
export const DETAILS_ROW_REQUEST_EVENT = "details-row-request";

/**
 * Opens Details at one of its rows: the drawer opens, it is asked for the row
 * (`DETAILS_ROW_REQUEST_EVENT`), the row is brought into its view, and the
 * focus goes to the row's first control - or, with `press`, that control is pressed, as the
 * labels key presses Edit labels - so the reader is where the control that sent them said
 * (R13). The focus is placed two frames on, after the drawer has given it
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
          placeInView(found, { block: "nearest" });
          const control = found.querySelector<HTMLElement>(
            "input, textarea, select, button:not(:disabled), a[href]"
          );
          if (press) control?.click();
          else control?.focus({ preventScroll: true });
        })
      );
    },
    [drawer]
  );
}
