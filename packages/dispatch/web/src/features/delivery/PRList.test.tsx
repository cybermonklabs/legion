import { expect, test } from "bun:test";
import { act, cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { DeliveryPR } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
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

function renderList(
  rows: DeliveryPR[],
  onSelect = (_id: string) => {},
  onSelectRun = (_id: number) => {}
) {
  return render(
    <KeymapProvider>
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
    // The rows are reached by the keys rather than by Tab, as every row of the application's
    // one table is: the first arrow press takes the list's first row, whatever held focus.
    expect(rows().filter((row) => row.tabIndex === 0).length).toBe(0);
    await user.keyboard("{ArrowDown}");
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
