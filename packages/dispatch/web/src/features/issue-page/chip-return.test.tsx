import { expect, test } from "bun:test";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { Link, MemoryRouter, useLocation, useNavigate } from "react-router-dom";

import { type ChipRecord, useChipRecord } from "./chip-return";

/**
 * The page's record of a chip's follow to a document of the issue (part N's R1, R2, the plan's
 * location table): `RowText.tsx` follows a chip in place with no state, so the page's root hears
 * the press and records the row that held the chip, its `href` and the address it was pressed
 * at, for the document's way back.
 */

/** What the record reads at the location the router holds, as the case asks for it. */
let recorded: () => ChipRecord | undefined = () => undefined;

/** The page's root around a row of the outline that holds `to` as a chip, at the address the
 *  router holds, with that address drawn, and Back. */
function Page({ to }: { to: string }): ReactNode {
  recorded = useChipRecord("CORE-1");
  const { hash, pathname, search } = useLocation();
  const navigate = useNavigate();
  return (
    <div data-issue-page="">
      <output data-testid="where">{`${pathname}${search}${hash}`}</output>
      <div data-node-id="head-b" data-node-row="" data-row-key="head-b">
        The words of head-b, which cite <Link to={to}>the notes</Link>.
      </div>
      <button onClick={() => navigate(-1)} type="button">
        Back
      </button>
    </div>
  );
}

function drawPage(to: string) {
  return render(
    <MemoryRouter initialEntries={["/issues/CORE-1/conversation?comment=head-b"]}>
      <Page to={to} />
    </MemoryRouter>
  );
}

test("a chip's follow to a document records its row, its href and its address", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-1/artifacts/notes-md#b-intro");
  try {
    await user.click(screen.getByRole("link", { name: "the notes" }));
    await waitFor(() =>
      expect(screen.getByTestId("where")).toHaveTextContent(
        "/issues/CORE-1/artifacts/notes-md#b-intro"
      )
    );
    expect(recorded()).toEqual({
      address: "/issues/CORE-1/conversation?comment=head-b",
      href: "/issues/CORE-1/artifacts/notes-md#b-intro",
      row: "head-b",
    });
    // The record is the document's location's: Back to the address the chip was pressed at
    // reads none.
    await user.click(screen.getByRole("button", { name: "Back" }));
    await waitFor(() =>
      expect(screen.getByTestId("where")).toHaveTextContent(
        "/issues/CORE-1/conversation?comment=head-b"
      )
    );
    expect(recorded()).toBeUndefined();
  } finally {
    view.unmount();
  }
});

test("a link to another issue's document records nothing", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-2/artifacts/notes-md");
  try {
    await user.click(screen.getByRole("link", { name: "the notes" }));
    await waitFor(() =>
      expect(screen.getByTestId("where")).toHaveTextContent("/issues/CORE-2/artifacts/notes-md")
    );
    expect(recorded()).toBeUndefined();
  } finally {
    view.unmount();
  }
});
