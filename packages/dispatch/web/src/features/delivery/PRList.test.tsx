import { expect, spyOn, test } from "bun:test";
import { act, cleanup, render, screen } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";

import type { DeliveryPR } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap, useKeymapScope } from "../shell/keymap";
import { OVERSCAN_ROWS, ROW_HEIGHT_PX, scrollTopToShowRow, visibleRowRange } from "./lib/prList";
import { PRList } from "./PRList";

const base: DeliveryPR = {
  additions: 1,
  author: "octocat",
  components: [],
  created_at: "2024-06-01T00:00:00Z",
  deletions: 1,
  deploy_run: null,
  deployed_at: null,
  deployed_status: "waiting",
  first_commit_at: null,
  id: "acme/widgets#1",
  issue: null,
  issue_title: null,
  merged_at: "2024-06-01T01:00:00Z",
  number: 1,
  parent_agent: null,
  partial: false,
  priority: null,
  repo: "acme/widgets",
  rework: false,
  sessions: [],
  title: "feat: widget 1",
  unfetchable_reason: null,
  url: "https://github.com/acme/widgets/pull/1",
};

/** `count` pull requests, newest merge first by number, so the default sort (merged, newest
 *  first) lists them as #1, #2, ... */
function prs(count: number): DeliveryPR[] {
  return Array.from({ length: count }, (_, i) => ({
    ...base,
    id: `acme/widgets#${i + 1}`,
    merged_at: new Date(Date.parse("2024-06-02T00:00:00Z") - (i + 1) * 60_000).toISOString(),
    number: i + 1,
    title: `feat: widget ${i + 1}`,
    url: `https://github.com/acme/widgets/pull/${i + 1}`,
  }));
}

/** The unit DOM has no layout engine, so the test supplies the two measurements the table reads:
 *  the height of the box the rows scroll in, which is the window it draws, and the height of a
 *  drawn row, which is what the virtualizer measures each one to be. */
function measureBoxAt(height: number): () => void {
  const box = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  const row = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "delivery-pr-list" ? height : (box?.get?.call(this) ?? 0);
    },
  });
  Object.defineProperty(HTMLElement.prototype, "offsetHeight", {
    configurable: true,
    get() {
      return this.dataset.prRow === undefined ? (row?.get?.call(this) ?? 0) : ROW_HEIGHT_PX;
    },
  });
  return () => {
    if (box !== undefined) Object.defineProperty(HTMLElement.prototype, "clientHeight", box);
    if (row !== undefined) Object.defineProperty(HTMLElement.prototype, "offsetHeight", row);
  };
}

/** The `delivery` scope, which `DeliveryPage` pushes around the list in both of its views. */
function DeliveryScope({ children }: { children: ReactNode }): ReactNode {
  useKeymapScope("delivery");
  return children;
}

function renderList(
  rows: DeliveryPR[],
  onSelect = (_id: string) => {},
  onSelectRun = (_id: number) => {}
) {
  return render(
    <KeymapProvider>
      <DeliveryScope>
        <PRList
          allRepos={["acme/widgets"]}
          colorBy="repo"
          colorScale={new Map()}
          columnFilters={[]}
          facetCounts={{}}
          issueTitles={{}}
          onColumnFiltersChange={() => {}}
          onSelect={onSelect}
          onSelectRun={onSelectRun}
          onSortingChange={() => {}}
          prs={rows}
          selectedId={undefined}
          sessionGroups={[]}
          sorting={[{ id: "merged", desc: true }]}
        />
      </DeliveryScope>
    </KeymapProvider>
  );
}

/** The rows drawn, by the id each carries. */
function drawnRows(): string[] {
  return [...document.querySelectorAll<HTMLElement>("[data-pr-row]")].map(
    (row) => row.dataset.prRow ?? ""
  );
}

/** The scroll a browser would do and report: happy-dom lays nothing out and scrolls no box, so
 *  the test moves the box where the table asked to be taken and fires the event that follows,
 *  which is what draws the rows at that offset. */
function scrolledTo(top: number): void {
  act(() => {
    const box = screen.getByTestId("delivery-pr-list");
    box.scrollTop = top;
    box.dispatchEvent(new Event("scroll"));
  });
}

test("the window holds the rows on screen plus the overscan, clamped at both ends", () => {
  const viewport = 10 * ROW_HEIGHT_PX;
  expect(visibleRowRange(0, viewport, 1000)).toEqual({ first: 0, last: 10 + OVERSCAN_ROWS });
  // Scrolled to the very end: the last row is inside the window, and nothing past it.
  const end = (1000 - 10) * ROW_HEIGHT_PX;
  expect(visibleRowRange(end, viewport, 1000)).toEqual({ first: 990 - OVERSCAN_ROWS, last: 1000 });
  // A taller viewport after a resize widens the window by the rows it now shows.
  expect(visibleRowRange(0, 20 * ROW_HEIGHT_PX, 1000).last).toBe(20 + OVERSCAN_ROWS);
  expect(visibleRowRange(0, viewport, 5)).toEqual({ first: 0, last: 5 });
});

test("scrolling to a row moves only as far as it takes to show it below the header", () => {
  const viewport = 11 * ROW_HEIGHT_PX; // the header plus ten body rows
  expect(scrollTopToShowRow(3, 0, viewport)).toBe(0);
  expect(scrollTopToShowRow(10, 0, viewport)).toBe(ROW_HEIGHT_PX);
  expect(scrollTopToShowRow(2, 5 * ROW_HEIGHT_PX, viewport)).toBe(2 * ROW_HEIGHT_PX);
});

test("the list renders only its first window of rows, and a taller viewport renders more", () => {
  const restore = measureBoxAt(10 * ROW_HEIGHT_PX);
  try {
    renderList(prs(200));
    const shown = drawnRows();
    expect(shown[0]).toBe("acme/widgets#1");
    expect(shown.length).toBeLessThan(200);
    cleanup();
    restore();

    const taller = measureBoxAt(40 * ROW_HEIGHT_PX);
    try {
      renderList(prs(200));
      expect(drawnRows().length).toBeGreaterThan(shown.length);
    } finally {
      cleanup();
      taller();
    }
  } finally {
    cleanup();
    restore();
  }
});

test("arrowing past the last rendered row renders and focuses the next, and Home and End jump to the ends", async () => {
  const user = userEvent.setup();
  const restore = measureBoxAt(10 * ROW_HEIGHT_PX);
  const opened: string[] = [];
  try {
    renderList(prs(200), (id) => opened.push(id));
    const drawn = drawnRows().length;
    expect(drawn).toBeLessThan(200);
    const rows = () => [...document.querySelectorAll<HTMLElement>("[data-pr-row]")];
    // No row is a Tab stop, as no row of the application's one table is: a row takes the focus
    // from a press on it, and the arrows walk the rows from the row that holds it.
    expect(rows().filter((row) => row.tabIndex === 0).length).toBe(0);
    act(() => rows()[0]?.focus());
    expect(document.activeElement).toBe(rows()[0] as HTMLElement);
    await user.keyboard("{ArrowDown}");
    await user.keyboard("{ArrowUp}");
    expect(document.activeElement).toBe(rows()[0] as HTMLElement);

    for (let i = 0; i < drawn; i += 1) {
      await user.keyboard("{ArrowDown}");
      scrolledTo((i + 1) * ROW_HEIGHT_PX);
    }
    // The row after the last one of the first window: the table scrolled to it, drew it, and
    // put focus there.
    expect(document.activeElement?.getAttribute("data-pr-row")).toBe(`acme/widgets#${drawn + 1}`);
    expect(document.activeElement?.textContent).toContain(`feat: widget ${drawn + 1}`);

    await user.keyboard("{End}");
    // The foot of the list: a row the window never held, so only the scroll that follows the
    // key draws it. Every row past the drawn window is still an estimate, so the offset is
    // past the whole list rather than an arithmetic of measured heights.
    scrolledTo(200 * 64);
    expect(document.activeElement?.getAttribute("data-pr-row")).toBe("acme/widgets#200");
    await user.keyboard("{Home}");
    scrolledTo(0);
    expect(document.activeElement?.getAttribute("data-pr-row")).toBe("acme/widgets#1");

    await user.keyboard("{Enter}");
    expect(opened).toEqual(["acme/widgets#1"]);
  } finally {
    cleanup();
    restore();
  }
});

test("a deployed row's 'deployed' opens its deploy run, and the row's own click opens the PR", async () => {
  const user = userEvent.setup();
  const restore = measureBoxAt(10 * ROW_HEIGHT_PX);
  const opened: string[] = [];
  const runs: number[] = [];
  try {
    renderList(
      [
        {
          ...base,
          deploy_run: 500,
          deployed_at: "2024-06-01T01:40:00Z",
          deployed_status: "deployed",
        },
      ],
      (id) => opened.push(id),
      (id) => runs.push(id)
    );
    await user.click(screen.getByRole("button", { name: "Open deploy run #500" }));
    expect(runs).toEqual([500]);
    expect(opened).toEqual([]);
  } finally {
    cleanup();
    restore();
  }
});

/** Presses `keys` (user-event's spelling) where the focus is, and answers whether the page took
 *  the press: a key nothing prevents is the browser's, which scrolls the page with an arrow. The
 *  listener that reads it is added to the window after the keymap's own, so it sees what the
 *  keymap did. */
async function taken(user: UserEvent, keys: string): Promise<boolean> {
  let prevented = false;
  const read = (event: KeyboardEvent) => {
    prevented = event.defaultPrevented;
  };
  window.addEventListener("keydown", read);
  try {
    await user.keyboard(keys);
  } finally {
    window.removeEventListener("keydown", read);
  }
  return prevented;
}

// INFRA-3135's review round 1 (thread PRRT_kwDORFy7ds63p6zE): the arrows were the list's while
// it held any row, so with the focus on the page's body, a header or a control outside the list
// they moved the focus into it and the page no longer scrolled. A row that holds the focus is the
// only place they are the list's, as before the shared table.
test("Up and Down are the list's only while a row holds the focus, and the page's everywhere else", async () => {
  const user = userEvent.setup();
  const restore = measureBoxAt(10 * ROW_HEIGHT_PX);
  try {
    renderList(prs(3));
    const rows = () => [...document.querySelectorAll<HTMLElement>("[data-pr-row]")];
    // The page's body, then a header's sort button: neither key is taken, and no row is focused.
    expect(document.activeElement).toBe(document.body);
    expect(await taken(user, "{ArrowDown}")).toBe(false);
    expect(await taken(user, "{ArrowUp}")).toBe(false);
    expect(document.activeElement).toBe(document.body);
    const header = screen.getByRole("button", { name: /^Merged/ });
    act(() => header.focus());
    expect(await taken(user, "{ArrowDown}")).toBe(false);
    expect(await taken(user, "{ArrowUp}")).toBe(false);
    expect(document.activeElement).toBe(header);

    // A row's own link, which Tab reaches: the focus is in a row, so the arrows walk the rows.
    act(() => rows()[0]?.querySelector("a")?.focus());
    expect(await taken(user, "{ArrowDown}")).toBe(true);
    expect(document.activeElement).toBe(rows()[1] as HTMLElement);
    expect(await taken(user, "{ArrowUp}")).toBe(true);
    expect(document.activeElement).toBe(rows()[0] as HTMLElement);
  } finally {
    cleanup();
    restore();
  }
});

// The same thread: `x` was registered for this list and listed in the shortcut help as "Select
// or deselect the focused row", though the list selects nothing, so the key did nothing.
test("the list registers no select key, so the shortcut help lists no x for it", async () => {
  const user = userEvent.setup();
  const restore = measureBoxAt(10 * ROW_HEIGHT_PX);
  try {
    renderList(prs(3));
    expect(
      appKeymap
        .describe()
        .filter((entry) => entry.scope === "delivery")
        .map((entry) => [entry.id, entry.keys])
    ).toEqual([
      ["next", ["ArrowDown"]],
      ["previous", ["ArrowUp"]],
      ["open", ["o"]],
      ["open-enter", ["Enter"]],
    ]);
    // And on a focused row, `x` is the browser's.
    act(() => document.querySelector<HTMLElement>("[data-pr-row]")?.focus());
    expect(await taken(user, "x")).toBe(false);
  } finally {
    cleanup();
    restore();
  }
});

// INFRA-3135's review round 1, R10: the List and the priorities table name their title column as
// the one whose header stacks in a box under 640 px; the Delivery list named none, so on a phone
// its Title header was drawn as no other table's is. It names its `title` too.
test("the list stacks its title's header in a box under 640 px, and no other", () => {
  const restore = measureBoxAt(10 * ROW_HEIGHT_PX);
  // On `Element.prototype`, which owns the method: bun's `mockRestore` leaves a copy on a
  // prototype that only inherits it, which would hide every later test's spy on `Element`.
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return new DOMRect(0, 0, this.dataset.testid === "delivery-pr-list" ? 390 : 100, 40);
  });
  try {
    renderList(prs(3));
    const header = (name: RegExp) =>
      screen.getByRole("columnheader", { name }).querySelector("span");
    expect(header(/^Title/)).toHaveClass("flex-col");
    expect(header(/^Author/)).not.toHaveClass("flex-col");
  } finally {
    cleanup();
    rect.mockRestore();
    restore();
  }
});
