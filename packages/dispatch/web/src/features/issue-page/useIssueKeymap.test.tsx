import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { type RenderResult, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation, useNavigationType } from "react-router-dom";

import type { IssueDetails } from "../../api/types";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { parseIssuePath } from "../refs/routes";
import { artifact, issueDetails } from "./decisions-filter/decisions-fixtures";
import { ProvidersSlot } from "./slots/providers";
import { AppProviders, stubReads } from "./slots/stage-fixtures";
import { ViewRowSlot } from "./slots/view-row";

/**
 * The view row's chords of part N (its R4): `t a` opens the node menu, which `/artifacts`
 * opens, with the focus in it, and `t f` opens the filter by words with the caret in it. Each
 * case draws the page's own view row, which binds them (`useIssueKeymap.ts`), at 1440 px, where
 * the row draws its small controls at its end.
 */

/** The issue's spec and a document of no role, which the node menu lists, the spec first. */
const ISSUE: IssueDetails = {
  ...issueDetails(),
  artifacts: [artifact("artifact-notes", "notes-md"), artifact("artifact-spec", "spec", true)],
};

/** Where the router is, and how it got there. */
function Where(): ReactNode {
  const { pathname, search } = useLocation();
  return (
    <output data-move={useNavigationType()} data-testid="where">
      {`${pathname}${search}`}
    </output>
  );
}

/** The view row at the address the router holds, under the page's own providers. */
function Row(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={ISSUE} route={route}>
      <ViewRowSlot issue={ISSUE} route={route} />
      <Where />
    </ProvidersSlot>
  );
}

let restore: () => void = () => {};

afterEach(() => restore());

/** The view row at `address`, at 1440 px, with the focus on the page's body. */
function drawRow(address: string): RenderResult {
  const reads = stubReads();
  const restoreViewport = stubViewport(DESKTOP_WIDTH);
  restore = () => {
    restoreViewport();
    for (const read of reads) read.mockRestore();
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter initialEntries={[address]}>
      <AppProviders client={client}>
        <Row />
      </AppProviders>
    </MemoryRouter>
  );
}

test("t a opens the node menu with the focus on its first item", async () => {
  const user = userEvent.setup();
  const view = drawRow("/issues/CORE-1/spec");
  try {
    await screen.findByRole("button", { name: "Files and documents" });
    expect(screen.queryByRole("menu")).toBeNull();
    await user.keyboard("ta");
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe("/issues/CORE-1/artifacts")
    );
    const menu = await screen.findByRole("menu");
    const [first] = within(menu).getAllByRole("menuitem");
    await waitFor(() => expect(first).toHaveFocus());
    // The spec is the menu's first item, whichever order the issue holds its documents in.
    expect(within(first as HTMLElement).getByRole("button", { name: "spec" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Files and documents" })).toHaveAttribute(
      "aria-expanded",
      "true"
    );
  } finally {
    view.unmount();
  }
});

test("t f opens the words box with the caret in it", async () => {
  const user = userEvent.setup();
  for (const address of ["/issues/CORE-1", "/issues/CORE-1/spec"]) {
    const view = drawRow(address);
    try {
      await screen.findByRole("button", { name: "Filter" });
      await user.keyboard("tf");
      await waitFor(() =>
        expect(screen.getByRole("searchbox", { name: "Filter" }), address).toHaveFocus()
      );
      expect(screen.queryByRole("button", { name: "Filter" }), address).toBeNull();
      // The words narrow the view the reader is on: the chord opens no address of its own.
      expect(screen.getByTestId("where").textContent, address).toBe(address);
      expect(screen.getByTestId("where"), address).toHaveAttribute("data-move", "POP");
      await user.keyboard("ab");
      await waitFor(() =>
        expect(screen.getByTestId("where").textContent, address).toBe(`${address}?q=ab`)
      );
    } finally {
      view.unmount();
      restore();
      restore = () => {};
    }
  }
});

// A chord's target the reader already sees whole takes the focus where it stands, and the page
// stays where it was (part N's R1): a chord back to the outline finds the row the reader was on
// at the place they left it. One the reader does not see is brought on screen as before.
// happy-dom lays nothing out, so the node menu's items are given a box, on screen and then below
// it, and every other box is empty.
test("a chord leaves a target already on screen where it is", async () => {
  const user = userEvent.setup();
  for (const [where, top, moves] of [
    ["on screen", 200, false],
    ["below the screen", 2_000, true],
  ] as const) {
    const view = drawRow("/issues/CORE-1/spec");
    const boxes = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
      this: Element
    ) {
      return this.matches('[role="menuitem"]') ? new DOMRect(0, top, 320, 40) : new DOMRect();
    });
    const intoView = spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {});
    const scrolled = spyOn(window, "scrollBy").mockImplementation(() => {});
    try {
      await screen.findByRole("button", { name: "Files and documents" });
      await user.keyboard("ta");
      const [first] = within(await screen.findByRole("menu")).getAllByRole("menuitem");
      await waitFor(() => expect(first, where).toHaveFocus());
      expect(
        intoView.mock.calls.length + scrolled.mock.calls.length > 0,
        `the chord's target ${where} ${moves ? "was not brought on screen" : "moved the page"}`
      ).toBe(moves);
    } finally {
      boxes.mockRestore();
      intoView.mockRestore();
      scrolled.mockRestore();
      view.unmount();
      restore();
      restore = () => {};
    }
  }
});
