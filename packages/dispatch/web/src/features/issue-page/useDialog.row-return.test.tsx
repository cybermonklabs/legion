import { expect, test } from "bun:test";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useEffect, useState } from "react";

import { KeymapProvider } from "../shell/KeymapProvider";
import { DIALOG_RETURN_ATTRIBUTE, useDialog } from "../shell/useDialog";

/** The search palette's stand-in: a dialog drawn beside the page, outside every row. */
function Palette({ onClose }: { onClose: () => void }) {
  const dialog = useDialog<HTMLDivElement>({ onClose, open: true });
  return (
    <div aria-label="Search" ref={dialog.containerRef} role="dialog">
      <button type="button">Search</button>
    </div>
  );
}

/** The navigation sheet's stand-in: a dialog whose Open search closes it and opens the palette in
 *  one handler, as the navigation sheet's Search and New issue do (`app.tsx`). */
function Sheet({ onClose, onSearch }: { onClose: () => void; onSearch: () => void }) {
  const dialog = useDialog<HTMLDivElement>({ onClose, open: true });
  return (
    <div aria-label="Sheet" ref={dialog.containerRef} role="dialog">
      <button onClick={onSearch} type="button">
        Open search
      </button>
    </div>
  );
}

/**
 * A row whose acts are drawn only while the focus is inside the row, as a focused row of the new
 * page draws its Copy reference, a palette that Ctrl+K opens and Escape closes, and a sheet that
 * the row's Menu opens. `marked` puts the dialog's return mark on the row, as `NodeRow` does on
 * its focusable element.
 */
function Page({ marked }: { marked: boolean }) {
  const [open, setOpen] = useState(false);
  const [sheet, setSheet] = useState(false);
  const [within, setWithin] = useState(false);
  useEffect(() => {
    const openOnK = (event: KeyboardEvent) => {
      if (event.ctrlKey && event.key === "k") setOpen(true);
    };
    document.addEventListener("keydown", openOnK);
    return () => document.removeEventListener("keydown", openOnK);
  }, []);
  return (
    <>
      <div
        {...{ [DIALOG_RETURN_ATTRIBUTE]: marked ? "" : undefined }}
        onBlur={(event) => {
          if (!event.currentTarget.contains(event.relatedTarget)) setWithin(false);
        }}
        onFocus={() => setWithin(true)}
        role="treeitem"
        tabIndex={-1}
      >
        The row's words
        {within ? <button type="button">Copy reference</button> : null}
        {within ? (
          <button onClick={() => setSheet(true)} type="button">
            Menu
          </button>
        ) : null}
      </div>
      {open ? <Palette onClose={() => setOpen(false)} /> : null}
      {sheet ? (
        <Sheet
          onClose={() => setSheet(false)}
          onSearch={() => {
            setSheet(false);
            setOpen(true);
          }}
        />
      ) : null}
    </>
  );
}

/** Draws the page, focuses the row's act, opens the palette from it and closes it with Escape,
 *  once the palette has taken the focus and the act has gone with the row's focus. */
async function closeAfterTheActWent(marked: boolean): Promise<() => void> {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <Page marked={marked} />
    </KeymapProvider>
  );
  act(() => screen.getByRole("treeitem").focus());
  act(() => screen.getByRole("button", { name: "Copy reference" }).focus());
  await user.keyboard("{Control>}k{/Control}");
  await waitFor(() => expect(screen.getByRole("button", { name: "Search" })).toHaveFocus());
  expect(
    screen.queryByRole("button", { name: "Copy reference" }),
    "the act stayed drawn once the row lost the focus to the palette"
  ).toBeNull();
  await user.keyboard("{Escape}");
  expect(screen.queryByRole("dialog"), "Escape left the palette open").toBeNull();
  return () => view.unmount();
}

test("a dialog closed after its act has gone gives the focus to the act's row", async () => {
  const unmount = await closeAfterTheActWent(true);
  try {
    expect(
      screen.getByRole("treeitem"),
      `the palette's close left the focus on ${document.activeElement?.tagName}`
    ).toHaveFocus();
  } finally {
    unmount();
  }
});

test("with no marked ancestor the focus goes to the body, as before", async () => {
  const unmount = await closeAfterTheActWent(false);
  try {
    expect(
      document.body,
      `the palette's close left the focus on ${document.activeElement?.tagName}`
    ).toHaveFocus();
  } finally {
    unmount();
  }
});

// The row's Menu opens the sheet, whose Open search closes it and opens the palette in one
// handler: the sheet hands its target, the Menu, to the palette (`handOverFocus`). The Menu went
// with the row's focus as the sheet opened, so the palette gives the focus to the Menu's row.
test("a dialog that the act's dialog hands its target to gives the focus to the act's row", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <Page marked />
    </KeymapProvider>
  );
  try {
    act(() => screen.getByRole("treeitem").focus());
    act(() => screen.getByRole("button", { name: "Menu" }).focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(screen.getByRole("button", { name: "Open search" })).toHaveFocus());
    expect(
      screen.queryByRole("button", { name: "Menu" }),
      "the act stayed drawn once the row lost the focus to the sheet"
    ).toBeNull();
    await user.click(screen.getByRole("button", { name: "Open search" }));
    expect(
      screen.queryByRole("dialog", { name: "Sheet" }),
      "Open search left the sheet open"
    ).toBeNull();
    await waitFor(() => expect(screen.getByRole("button", { name: "Search" })).toHaveFocus());
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog"), "Escape left the palette open").toBeNull();
    expect(
      screen.getByRole("treeitem"),
      `the palette's close left the focus on ${document.activeElement?.tagName}`
    ).toHaveFocus();
  } finally {
    view.unmount();
  }
});
