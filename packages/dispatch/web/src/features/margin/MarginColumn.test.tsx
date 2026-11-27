import { expect, test } from "bun:test";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { type MarginDrawer, MarginSlotProvider } from "../issue-page/margin-slot";
import { buildIssuePath } from "../refs/routes";
import type { MarginProps } from "./Margin";
import { MarginColumnFallback } from "./MarginColumn";
import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "./margin-fixture";
import { MARGIN_COLUMN_CLASS } from "./useMarginLayout";

const ISSUE_PATH = buildIssuePath({ key: "CORE-1", kind: "issue" });

/** Details as the shell hands it to the margin on each load of the new issue page: closed. */
const closedDetails: MarginDrawer = { open: false, setOpen: () => {}, setPage: () => {} };

/** What `MarginColumnFallback` draws on `path` in a window `viewport` px wide, with the shell's
 *  `props` and, where one is given, the new issue page's `drawer`: a copy of its element, or
 *  null where it draws none. */
function drawnFallback(
  viewport: number,
  path: string,
  props: MarginProps = {},
  drawer?: MarginDrawer
): Element | null {
  const restoreViewport = stubViewport(viewport);
  const innerWidth = Object.getOwnPropertyDescriptor(window, "innerWidth");
  Object.defineProperty(window, "innerWidth", { configurable: true, value: viewport });
  try {
    const view = render(
      <MemoryRouter initialEntries={[path]}>
        <MarginSlotProvider drawer={drawer}>
          <MarginColumnFallback {...props} />
        </MarginSlotProvider>
      </MemoryRouter>
    );
    const drawn = view.container.firstElementChild?.cloneNode(true) ?? null;
    view.unmount();
    return drawn as Element | null;
  } finally {
    restoreViewport();
    if (innerWidth === undefined) {
      Reflect.deleteProperty(window, "innerWidth");
    } else {
      Object.defineProperty(window, "innerWidth", innerWidth);
    }
  }
}

// The margin's code arrives after the shell's first paint (`MarginColumn.tsx`). Where the margin
// will draw its desktop column, the frames before it hold the column's place at the width the
// margin will draw it, so the page beside it has its final width from the first: the default, or
// the reader's own clamped as the margin clamps it. The place is empty and read by no one.
test("the fallback holds the desktop column's place at the width the margin draws it", () => {
  for (const [width, drawnWidth] of [
    [undefined, "384px"],
    [408, "408px"],
    [200, "280px"],
  ] as const) {
    const place = drawnFallback(DESKTOP_WIDTH, ISSUE_PATH, { width });
    expect(place, `a width of ${width}`).not.toBeNull();
    expect(place?.getAttribute("aria-hidden"), `a width of ${width}`).toBe("true");
    expect(place?.getAttribute("class"), `a width of ${width}`).toBe(MARGIN_COLUMN_CLASS);
    expect((place as HTMLElement | null)?.style.width, `a width of ${width}`).toBe(drawnWidth);
    expect(place?.childNodes.length, `a width of ${width}`).toBe(0);
  }
});

// Everywhere else the margin takes no width from the page, so neither does the fallback: a
// collapsed margin is a fixed rail over the gutter the shell keeps for it, the new issue page's
// Details opens over the page, below the compact layout the margin is a sheet at the foot of the
// screen, and a route with no margin has none.
test("the fallback draws nothing where the margin draws no desktop column", () => {
  for (const [where, drawn] of [
    ["collapsed to its rail", drawnFallback(DESKTOP_WIDTH, ISSUE_PATH, { collapsed: true })],
    ["below the compact layout", drawnFallback(PHONE_WIDTH, ISSUE_PATH)],
    ["as Details", drawnFallback(DESKTOP_WIDTH, ISSUE_PATH, {}, closedDetails)],
    ["on a route with no margin", drawnFallback(DESKTOP_WIDTH, "/")],
  ] as const) {
    expect(drawn, where).toBeNull();
  }
});
