import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { api } from "../../api/client";
import type { IssueSummary } from "../../api/types";
import { type GridColumn, IssueGrid } from "./IssueGrid";

function issue(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    assignee: null,
    blocked_by: [],
    claim: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-09-10T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    key,
    labels: [],
    last_seq: 2,
    open_asks: 0,
    parent: null,
    primary_artifact_id: "spec",
    priority: null,
    progress: { children: null, tasks: null },
    rank: key,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    status: "todo",
    title: `Work ${key}`,
    updated_at: "2026-09-10T00:00:00Z",
    ...overrides,
  };
}

interface Row {
  key: string;
  hours: number;
}
const hoursColumn: GridColumn<Row> = {
  cell: (row) => <span data-testid={`hours-${row.key}`}>{row.hours} h</span>,
  header: "Hours",
  id: "hours",
  value: (row) => row.hours,
};

const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});

/** The listing every grid in a test joins to: two issues of CORE, and no Legion daemon. */
function harness(issues: IssueSummary[] = [issue("CORE-1"), issue("CORE-2")]) {
  const listing = spyOn(api, "listIssues").mockResolvedValue(issues);
  const state = spyOn(api, "getMyState").mockResolvedValue({});
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const legion = spyOn(api, "listLegionSettings").mockResolvedValue([]);
  restores.push(() => {
    listing.mockRestore();
    state.mockRestore();
    agents.mockRestore();
    legion.mockRestore();
  });
}

/** What the box measures now. A test changes it through `remeasure`, which is the window
 *  resize the hook listens for; happy-dom lays nothing out, so this is the only width there is. */
let measuredWidth = 1000;

/** The box's new width, and the resize that makes the table read it. */
async function remeasure(width: number) {
  measuredWidth = width;
  await act(async () => {
    window.dispatchEvent(new Event("resize"));
  });
}

function draw(tree: ReactNode, boxWidth = 1000) {
  measuredWidth = boxWidth;
  // The unit DOM has no layout engine, and the grid's rows are virtual: supply the box's own
  // height, which is all the virtualizer measures, and its width, which is what decides whether
  // the table is drawn for a phone.
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "issue-grid-scroll" ? 600 : (descriptor?.get?.call(this) ?? 0);
    },
  });
  const rect = HTMLElement.prototype.getBoundingClientRect;
  HTMLElement.prototype.getBoundingClientRect = function bounds(this: HTMLElement) {
    const measured = rect.call(this) as DOMRect;
    if (this.dataset.testid !== "issue-grid-scroll") return measured;
    return { ...measured.toJSON(), width: measuredWidth, height: 600 } as DOMRect;
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/projects/CORE/legion"]}>
      <QueryClientProvider client={client}>{tree}</QueryClientProvider>
    </MemoryRouter>
  );
  restores.push(() => {
    view.unmount();
    client.clear();
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
    HTMLElement.prototype.getBoundingClientRect = rect;
  });
  return view;
}

test("a grid column sorts by its value and draws its own cell", async () => {
  const user = userEvent.setup();
  harness();
  draw(
    <IssueGrid<Row>
      columns={["key", "title", hoursColumn]}
      label="Finished issues"
      layoutKey="CORE.finished"
      project="CORE"
      rows={[
        { hours: 9, key: "CORE-1" },
        { hours: 2, key: "CORE-2" },
      ]}
    />
  );
  const grid = await screen.findByRole("table", { name: "Finished issues" });
  expect((await within(grid).findByTestId("hours-CORE-1")).textContent).toBe("9 h");
  const keysNow = () =>
    within(grid)
      .getAllByRole("row")
      .slice(1)
      .map((row) => row.getAttribute("data-issue-row"));
  expect(keysNow()).toEqual(["CORE-1", "CORE-2"]);
  await user.click(within(grid).getByRole("button", { name: "Sort by hours" }));
  expect(keysNow()).toEqual(["CORE-2", "CORE-1"]);
  await user.click(within(grid).getByRole("button", { name: "Sort by hours" }));
  expect(keysNow()).toEqual(["CORE-1", "CORE-2"]);
});

test("a row whose key the listing does not hold still draws its own columns", async () => {
  harness([issue("CORE-1")]);
  draw(
    <IssueGrid<Row>
      columns={["key", hoursColumn]}
      label="Finished issues"
      layoutKey="CORE.finished"
      project="CORE"
      rows={[
        { hours: 9, key: "CORE-1" },
        { hours: 4, key: "CORE-404" },
      ]}
    />
  );
  const grid = await screen.findByRole("table", { name: "Finished issues" });
  expect((await within(grid).findByTestId("hours-CORE-404")).textContent).toBe("4 h");
  expect(within(grid).getByText("CORE-404")).toBeDefined();
});

test("three grids on one page keep their own sort and their own saved layout", async () => {
  const user = userEvent.setup();
  harness();
  // A width the reader dragged on the first grid before this render; the other two never had it.
  localStorage.setItem(
    "dispatch.project-table.CORE.one",
    JSON.stringify({ order: [], sizing: { title: 320 }, visibility: {} })
  );
  draw(
    <>
      {["one", "two", "three"].map((name) => (
        <IssueGrid<Row>
          columns={["key", hoursColumn]}
          key={name}
          label={`Grid ${name}`}
          layoutKey={`CORE.${name}`}
          project="CORE"
          rows={[
            { hours: 9, key: "CORE-1" },
            { hours: 2, key: "CORE-2" },
          ]}
        />
      ))}
    </>
  );
  const first = await screen.findByRole("table", { name: "Grid one" });
  const second = screen.getByRole("table", { name: "Grid two" });
  const keysOf = (grid: HTMLElement) =>
    within(grid)
      .getAllByRole("row")
      .slice(1)
      .map((row) => row.getAttribute("data-issue-row"));

  await user.click(within(first).getByRole("button", { name: "Sort by hours" }));
  expect(keysOf(first)).toEqual(["CORE-2", "CORE-1"]);
  // The sort is this grid's: the one beside it is still in the order it was given.
  expect(keysOf(second)).toEqual(["CORE-1", "CORE-2"]);

  // And each grid keeps its widths, its order and what it shows under a key of its own: the
  // first grid's saved width is still its own, and the third saved none.
  expect(
    JSON.parse(localStorage.getItem("dispatch.project-table.CORE.one") ?? "{}").sizing
  ).toEqual({ title: 320 });
  expect(
    JSON.parse(localStorage.getItem("dispatch.project-table.CORE.three") ?? "{}").sizing
  ).toEqual({});
});

test("the Key link goes where keyHref sends it", async () => {
  harness();
  draw(
    <IssueGrid<Row>
      columns={["key"]}
      keyHref={(row) => `/projects/CORE/legion/${row.key}`}
      label="Finished issues"
      layoutKey="CORE.finished"
      project="CORE"
      rows={[{ hours: 9, key: "CORE-1" }]}
    />
  );
  const grid = await screen.findByRole("table", { name: "Finished issues" });
  expect(within(grid).getByRole("link", { name: "CORE-1" }).getAttribute("href")).toBe(
    "/projects/CORE/legion/CORE-1"
  );
});

// The Issues tab opens with Time per phase hidden; that is the tab's rule, not the table's, so
// a grid that asks for the column gets it.
test("a grid shows a registry column the Issues tab hides", async () => {
  harness();
  draw(
    <IssueGrid<Row>
      columns={["key", "phase_hours"]}
      label="Finished issues"
      layoutKey="CORE.finished"
      project="CORE"
      rows={[{ hours: 9, key: "CORE-1" }]}
    />
  );
  const grid = await screen.findByRole("table", { name: "Finished issues" });
  expect(within(grid).getByRole("columnheader", { name: /Time per phase/ })).toBeDefined();
});

test("a column the reader hides stays hidden there and nowhere else", async () => {
  const user = userEvent.setup();
  harness();
  draw(
    <>
      {["one", "two"].map((name) => (
        <IssueGrid<Row>
          columns={["key", hoursColumn]}
          key={name}
          label={`Grid ${name}`}
          layoutKey={`CORE.${name}`}
          project="CORE"
          rows={[{ hours: 9, key: "CORE-1" }]}
        />
      ))}
    </>
  );
  const first = await screen.findByRole("table", { name: "Grid one" });
  expect(within(first).getByRole("columnheader", { name: /Hours/ })).toBeDefined();

  await user.click(screen.getAllByRole("button", { name: "Columns" })[0] as HTMLElement);
  await user.click(await screen.findByRole("checkbox", { name: "Hours" }));

  expect(within(first).queryByRole("columnheader", { name: /Hours/ })).toBeNull();
  const second = screen.getByRole("table", { name: "Grid two" });
  expect(within(second).getByRole("columnheader", { name: /Hours/ })).toBeDefined();
  expect(
    JSON.parse(localStorage.getItem("dispatch.project-table.CORE.one") ?? "{}").visibility
  ).toEqual({ hours: false });
  expect(
    JSON.parse(localStorage.getItem("dispatch.project-table.CORE.two") ?? "{}").visibility
  ).toEqual({});
});

// happy-dom lays nothing out, so what is asserted is the box itself: no height to scroll inside
// and no vertical overflow, and every row of the page in the table rather than a virtual window.
test("fit=rows draws every row of the page with no box to scroll inside", async () => {
  harness();
  const rows = Array.from({ length: 25 }, (_, index) => ({
    hours: index,
    key: `CORE-${index + 1}`,
  }));
  const view = draw(
    <IssueGrid<Row>
      columns={["key", hoursColumn]}
      fit="rows"
      label="Finished issues"
      layoutKey="CORE.finished"
      pageSize={25}
      project="CORE"
      rows={rows}
    />
  );
  const grid = await screen.findByRole("table", { name: "Finished issues" });
  expect(within(grid).getAllByRole("row").slice(1)).toHaveLength(25);
  const box = view.container.querySelector<HTMLElement>("[data-testid=issue-grid-scroll]");
  expect(box?.style.height).toBe("");
  expect(box?.className).toContain("overflow-x-auto");
  expect(box?.className).not.toContain("overflow-auto");
});

test("a phone keeps the primary columns and reads the title under the key", async () => {
  harness([issue("CORE-1", { title: "The pager reads the rows it shows" })]);
  const view = draw(
    <IssueGrid<Row>
      columns={["key", "title", { ...hoursColumn, primary: true }]}
      label="Finished issues"
      layoutKey="CORE.finished"
      project="CORE"
      primary={["ci"]}
      rows={[{ hours: 9, key: "CORE-1" }]}
    />,
    390
  );
  const grid = await screen.findByRole("table", { name: "Finished issues" });
  // The primary column stays; Title has no column of its own.
  expect(within(grid).getByRole("columnheader", { name: /Hours/ })).toBeDefined();
  expect(within(grid).queryByRole("columnheader", { name: /Title/ })).toBeNull();
  // The title is in the Key cell, and the key is written in full.
  const keyCell = view.container
    .querySelector("[data-grid-key-title]")
    ?.closest("td") as HTMLElement;
  expect(within(keyCell).getByText("CORE-1")).toBeDefined();
  expect(within(keyCell).getByText("The pager reads the rows it shows")).toBeDefined();
});

// happy-dom lays nothing out, so what is asserted is what decides the wrap in a browser: the
// row may shrink below the width of its contents, and wraps rather than widening the page.
test("the pager wraps inside a 390 box instead of widening the page", async () => {
  harness();
  const view = draw(
    <IssueGrid<Row>
      columns={["key", hoursColumn]}
      label="Finished issues"
      layoutKey="CORE.finished"
      project="CORE"
      rows={[{ hours: 9, key: "CORE-1" }]}
    />,
    390
  );
  await screen.findByRole("table", { name: "Finished issues" });
  const pager = view.container.querySelector<HTMLElement>("[data-testid=issues-table-pager]");
  if (pager === null) throw new Error("the grid drew no pager");
  expect(pager.className).toContain("flex-wrap");
  expect(pager.className).toContain("min-w-0");
  expect(pager.className).not.toContain("shrink-0");
  for (const piece of pager.querySelectorAll("span")) {
    expect(piece.className).toContain("min-w-0");
  }
});

test("at 390 the key and two primary columns fit the box between them", async () => {
  harness([issue("CORE-1", { title: "A title long enough to need both of its lines" })]);
  const view = draw(
    <IssueGrid<Row>
      columns={[
        "key",
        "title",
        { ...hoursColumn, primary: true },
        { ...hoursColumn, header: "Rounds", id: "rounds", primary: true },
        { ...hoursColumn, header: "Extra", id: "extra" },
      ]}
      label="Finished issues"
      layoutKey="CORE.finished"
      project="CORE"
      rows={[{ hours: 9, key: "CORE-1" }]}
    />,
    390
  );
  const grid = await screen.findByRole("table", { name: "Finished issues" });
  expect(Number.parseFloat(grid.style.width)).toBeLessThanOrEqual(390);
  // The two primaries are drawn, each wide enough to say something; the rest is in the picker.
  const widths = [...grid.querySelectorAll("thead th")].map((head) =>
    Number.parseFloat((head as HTMLElement).style.width)
  );
  expect(widths).toHaveLength(3);
  expect(Math.min(...widths.slice(1))).toBeGreaterThanOrEqual(96);
  expect(widths.reduce((sum, width) => sum + width, 0)).toBeLessThanOrEqual(390);
  expect(view.container.querySelectorAll("[data-grid-key-title]")).toHaveLength(1);
});

// A box inside a container that is not shown, and the instant before a full-page capture's
// first paint, both measure 0 wide. Reading that as a phone drew the phone's table at 1280.
test("a measured width of zero leaves the layout as it was", async () => {
  harness([issue("CORE-1")]);
  const view = draw(
    <IssueGrid<Row>
      columns={["key", "title", { ...hoursColumn, primary: true }]}
      label="Finished issues"
      layoutKey="CORE.finished"
      project="CORE"
      rows={[{ hours: 9, key: "CORE-1" }]}
    />,
    1280
  );
  const grid = await screen.findByRole("table", { name: "Finished issues" });
  const wide = grid.style.width;
  expect(within(grid).getByRole("columnheader", { name: /Title/ })).toBeDefined();

  await remeasure(0);

  expect(within(grid).getByRole("columnheader", { name: /Title/ })).toBeDefined();
  expect(view.container.querySelector("[data-grid-key-title]")).toBeNull();
  expect(grid.style.width).toBe(wide);
});
