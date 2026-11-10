import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
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

function draw(tree: ReactNode) {
  // The unit DOM has no layout engine, and the grid's rows are virtual: supply the box's own
  // height, which is all the virtualizer measures.
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "issue-grid-scroll" ? 600 : (descriptor?.get?.call(this) ?? 0);
    },
  });
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
