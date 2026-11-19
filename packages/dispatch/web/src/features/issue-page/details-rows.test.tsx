import { expect, mock, test } from "bun:test";
import { render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import {
  DETAILS_ROW_ATTRIBUTE,
  DETAILS_ROW_REQUEST_EVENT,
  type DetailsRow,
  useOpenDetailsAt,
} from "./details-rows";
import { DETAILS_ATTRIBUTE, type MarginDrawer, MarginSlotProvider } from "./margin-slot";

/** A control elsewhere on the page that opens Details at `row`, as the header's label count does,
 *  or with `press` presses the row's first control there, as the labels key does. */
function Opener({ press = false, row }: { press?: boolean; row: DetailsRow }): ReactNode {
  const openAt = useOpenDetailsAt();
  return (
    <button onClick={() => openAt(row, press)} type="button">
      {press ? "Press at" : "Open at"} {row}
    </button>
  );
}

// A row inside a closed section of Details is hidden, and takes no focus, until its section
// opens, so the opener asks Details for the row - an event on the drawer's aside, with the row as
// its detail - before it looks for the row two frames on.
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

// The page's controls refuse a press with `aria-disabled` while their write is out, rather than
// turn disabled, so such a control still takes the focus and a click. The opener passes over it
// as it does a disabled one, and focuses or presses the row's first control that takes a press.
test("the opener passes over a control of its row that refuses a press with aria-disabled", async () => {
  const pressed: string[] = [];
  const drawer: MarginDrawer = { open: false, setOpen: () => {}, setPage: () => {} };
  const view = render(
    <MarginSlotProvider drawer={drawer}>
      <aside {...{ [DETAILS_ATTRIBUTE]: "" }}>
        <div {...{ [DETAILS_ROW_ATTRIBUTE]: "labels" }}>
          <button aria-disabled="true" onClick={() => pressed.push("refused")} type="button">
            Saving labels
          </button>
          <button onClick={() => pressed.push("usable")} type="button">
            Retry
          </button>
        </div>
      </aside>
      <Opener row="labels" />
      <Opener press row="labels" />
    </MarginSlotProvider>
  );
  try {
    const usable = view.getByRole("button", { name: "Retry" });
    view.getByRole("button", { name: "Open at labels" }).click();
    await waitFor(() =>
      expect(document.activeElement, "the opener focused a control that refuses a press").toBe(
        usable
      )
    );
    view.getByRole("button", { name: "Press at labels" }).click();
    await waitFor(() =>
      expect(pressed, "the opener pressed a control that refuses a press").toEqual(["usable"])
    );
  } finally {
    view.unmount();
  }
});
