import { expect, mock, test } from "bun:test";
import { render } from "@testing-library/react";
import type { ReactNode } from "react";

import {
  DETAILS_ROW_ATTRIBUTE,
  DETAILS_ROW_REQUEST_EVENT,
  type DetailsRow,
  useOpenDetailsAt,
} from "./details-rows";
import { DETAILS_ATTRIBUTE, type MarginDrawer, MarginSlotProvider } from "./margin-slot";

/** A control elsewhere on the page that opens Details at `row`, as the header's label count does. */
function Opener({ row }: { row: DetailsRow }): ReactNode {
  const openAt = useOpenDetailsAt();
  return (
    <button onClick={() => openAt(row)} type="button">
      Open at {row}
    </button>
  );
}

// A row inside a closed section of Details is not drawn until its section opens, so the opener
// asks Details for the row - an event on the drawer's aside, with the row as its detail - before
// it looks for the row two frames on.
test("the opener asks Details for its row before it looks for the row", () => {
  const requested: { row: unknown; rowDrawn: boolean }[] = [];
  const setOpen = mock((_open: boolean) => {});
  const drawer: MarginDrawer = { open: false, setOpen, setPage: () => {} };
  const view = render(
    <MarginSlotProvider drawer={drawer}>
      <aside {...{ [DETAILS_ATTRIBUTE]: "" }} />
      <Opener row="labels" />
    </MarginSlotProvider>
  );
  try {
    const aside = view.container.querySelector(`aside[${DETAILS_ATTRIBUTE}]`);
    if (aside === null) throw new Error("the case drew no Details aside");
    aside.addEventListener(DETAILS_ROW_REQUEST_EVENT, (event) => {
      requested.push({
        row: (event as CustomEvent<DetailsRow>).detail,
        rowDrawn: aside.querySelector(`[${DETAILS_ROW_ATTRIBUTE}]`) !== null,
      });
    });
    view.getByRole("button", { name: "Open at labels" }).click();
    expect(setOpen).toHaveBeenCalledWith(true);
    // Sent at the press itself, before the two frames in which the opener looks for the row.
    expect(requested, "the opener sent Details no request for its row").toEqual([
      { row: "labels", rowDrawn: false },
    ]);
  } finally {
    view.unmount();
  }
});
