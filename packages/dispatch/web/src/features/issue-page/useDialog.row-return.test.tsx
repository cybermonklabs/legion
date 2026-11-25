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

/**
 * A row whose act is drawn only while the focus is inside the row, as a focused row of the new
 * page draws its Copy reference, and a palette that Ctrl+K opens and Escape closes. `marked`
 * puts the dialog's return mark on the row, as `NodeRow` does on its focusable element.
 */
function Page({ marked }: { marked: boolean }) {
  const [open, setOpen] = useState(false);
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
      </div>
      {open ? <Palette onClose={() => setOpen(false)} /> : null}
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
