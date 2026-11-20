import type { ReactNode } from "react";

import { type DetailsRow, useOpenDetailsAt } from "./details-rows";

// What `details-rows.test.tsx` and `IssueMargin.test.tsx` both reach a row of Details with.

/** A control elsewhere on the page that opens Details at `row`, as the header's label count does,
 *  or with `press` presses the row's control there, as the labels key and the header's Set or
 *  Edit parent issue do. It is named "Open at" or "Press at", then the row. */
export function Opener({ press = false, row }: { press?: boolean; row: DetailsRow }): ReactNode {
  const openAt = useOpenDetailsAt();
  return (
    <button onClick={() => openAt(row, press)} type="button">
      {press ? "Press at" : "Open at"} {row}
    </button>
  );
}
