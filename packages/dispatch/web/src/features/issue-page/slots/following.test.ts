import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { type RenderResult, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ComponentProps, createElement, type ReactNode } from "react";
import { Link, MemoryRouter, useLocation } from "react-router-dom";

import type { IssueDetails } from "../../../api/types";
import { parseIssuePath } from "../../refs/routes";
import { issueDetails } from "../decisions-filter/decisions-fixtures";
import { useFollowing } from "./following";
import { ProvidersSlot } from "./providers";
import { AppProviders, stubReads } from "./stage-fixtures";
import { ViewRowSlot } from "./view-row";

/**
 * The following of the address across the filter by words (part N's R4, decision 375): the
 * words the box types are written into the address in place, and are no new following of it,
 * while a link to other words is one. Each case draws the page's own view row, whose box is the
 * one the reader types in, beside the following the stage and the views read.
 */

/** The following, and the address with its words, as the stage and the views read them. */
function Following(): ReactNode {
  const { pathname, search } = useLocation();
  return createElement(
    "output",
    { "data-following": useFollowing(), "data-testid": "following" },
    `${pathname}${search}`
  );
}

/** The view row at the address the router holds, under the page's own providers, with the
 *  following beside it and a link to other words. */
function Row({ issue }: { issue: IssueDetails }): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  // A provider's children are the arguments after its props. Its props' type, written for JSX,
  // requires them among the props too, so the props are asserted to that type.
  return createElement(
    ProvidersSlot,
    { issue, route } as ComponentProps<typeof ProvidersSlot>,
    createElement(ViewRowSlot, { issue, route }),
    createElement(Following),
    createElement(Link, { to: "/issues/CORE-1/spec?q=other" }, "Other words")
  );
}

/** The page's view row under the Spec view. */
function drawSpecRow(): RenderResult {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    createElement(
      MemoryRouter,
      { initialEntries: ["/issues/CORE-1/spec"] },
      createElement(
        AppProviders,
        { client } as ComponentProps<typeof AppProviders>,
        createElement(Row, { issue: issueDetails() })
      )
    )
  );
}

/** The following the page is at. */
function following(): string {
  return screen.getByTestId("following").dataset.following ?? "";
}

test("typing in the words box keeps one following", async () => {
  const reads = stubReads();
  const user = userEvent.setup();
  const view = drawSpecRow();
  try {
    const first = following();
    await user.click(screen.getByRole("button", { name: "Filter" }));
    const box = screen.getByRole("searchbox", { name: "Filter" });
    for (const [typed, words] of [
      ["a", "?q=a"],
      ["b", "?q=ab"],
      ["c", "?q=abc"],
    ] as const) {
      await user.type(box, typed);
      await waitFor(() => expect(screen.getByTestId("following")).toHaveTextContent(words));
      expect(following(), `typing ${typed} began a new following`).toBe(first);
    }
    await user.clear(box);
    await waitFor(() =>
      expect(screen.getByTestId("following").textContent).toBe("/issues/CORE-1/spec")
    );
    expect(following(), "emptying the box began a new following").toBe(first);
  } finally {
    view.unmount();
    for (const read of reads) read.mockRestore();
  }
});

test("a link to new words starts a new following", async () => {
  const reads = stubReads();
  const user = userEvent.setup();
  const view = drawSpecRow();
  try {
    await user.click(screen.getByRole("button", { name: "Filter" }));
    await user.type(screen.getByRole("searchbox", { name: "Filter" }), "abc");
    await waitFor(() => expect(screen.getByTestId("following")).toHaveTextContent("?q=abc"));
    const typed = following();

    await user.click(screen.getByRole("link", { name: "Other words" }));
    await waitFor(() => expect(screen.getByTestId("following")).toHaveTextContent("?q=other"));
    const linked = following();
    expect(linked, "the link to other words is the following the box was typed in").not.toBe(typed);
    const box = screen.getByRole("searchbox", { name: "Filter" });
    expect(box).toHaveValue("other");

    await user.type(box, "s");
    await waitFor(() => expect(screen.getByTestId("following")).toHaveTextContent("?q=others"));
    expect(following(), "typing after the link began a following of its own").toBe(linked);
  } finally {
    view.unmount();
    for (const read of reads) read.mockRestore();
  }
});
