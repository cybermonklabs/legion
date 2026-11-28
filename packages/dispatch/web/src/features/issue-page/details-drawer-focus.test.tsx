import { expect, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { DESKTOP_WIDTH, issue, stubViewport } from "../margin/margin-fixture";
import { buildIssuePath } from "../refs/routes";
import { DetailsDrawerShell, detailsDrawerClient } from "./details-drawer-fixture";
import { IssueIdentityRow } from "./IssueIdentityRow";
import { useMarginDrawer } from "./margin-slot";

/** The new page's Details control (`IssueIdentityRow.tsx`: the request, then the open), and an
 *  open with no request, as `Shift+M` makes one. */
function Controls(): ReactNode {
  const details = useMarginDrawer();
  return (
    <>
      <IssueIdentityRow actions={[]} issueKey={issue.key} parent={null} project={issue.project} />
      <button
        onClick={() => details?.setOpen(!details.open)}
        onMouseDown={(event) => event.preventDefault()}
        type="button"
      >
        Open via key
      </button>
    </>
  );
}

function renderBesideDetails(): RenderResult {
  return render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={detailsDrawerClient()}>
        <DetailsDrawerShell>
          <Controls />
          <textarea aria-label="Your answer" />
          <button type="button">Elsewhere</button>
        </DetailsDrawerShell>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// R14 and decision 584: the new page's Details control opens Details and leaves the focus where
// it is, since Details follows the focus there. Without `leaveFocusOnNextOpen` the drawer put
// the focus on its Close as it opened, taking the focus off Dashboard's answer box, so the
// browser row "Details on a focused ask reads that ask and its references" failed on "opening
// Details took the focus off the question". The request is the control's open alone: an open
// after it by anything else focuses Close again. Closing gives the focus back as main does, which
// happy-dom cannot show (a hidden button keeps the focus there), so part D's browser row
// "the frame draws an icon rail, Details over the page and the Contents pane @phone" holds it.
test("the Details control leaves the focus where it is as Details opens, and asks it of that open alone", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const view = renderBesideDetails();
  try {
    const details = await screen.findByTestId("desktop-margin-shell");
    const control = screen.getByTestId("issue-details-control");
    const answer = screen.getByRole("textbox", { name: "Your answer" });
    await user.click(answer);
    await user.click(control);
    expect(details.hasAttribute("hidden"), "the Details control opened no Details").toBe(false);
    expect(document.activeElement, "opening Details took the focus off the field").toBe(answer);
    await user.click(control);
    expect(details.hasAttribute("hidden"), "the Details control left Details open").toBe(true);
    expect(document.activeElement, "closing Details took the focus off the field").toBe(answer);

    const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
    act(() => elsewhere.focus());
    await user.click(control);
    expect(document.activeElement, "opening Details took the focus off the button").toBe(elsewhere);
    await user.click(control);
    expect(details.hasAttribute("hidden"), "the Details control left Details open").toBe(true);

    // The control's request was its own open's: an open by a key after it focuses Close.
    await user.click(screen.getByRole("button", { name: "Open via key" }));
    expect(details.hasAttribute("hidden"), "the key opened no Details").toBe(false);
    expect(document.activeElement, "an open after the control's kept the focus where it was").toBe(
      screen.getByTestId("details-close")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

// R14: opening Details on the new page by anything but the Details control - `Shift+M`, which
// calls `setOpen(true)` with no request - puts the focus on its Close, as main does.
test("opening Details on the new page without the control's request puts the focus on its Close", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const view = renderBesideDetails();
  try {
    const details = await screen.findByTestId("desktop-margin-shell");
    act(() => screen.getByRole("button", { name: "Elsewhere" }).focus());
    await user.click(screen.getByRole("button", { name: "Open via key" }));
    expect(details.hasAttribute("hidden"), "opening Details left it hidden").toBe(false);
    expect(document.activeElement, "opening Details left the focus off its Close").toBe(
      screen.getByTestId("details-close")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});
