import { afterEach, expect, spyOn, test } from "bun:test";
import type {
  ColumnDef,
  ColumnFiltersState,
  GroupingState,
  PaginationState,
  SortingState,
} from "@tanstack/react-table";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { useKeymapScope } from "../features/shell/keymap";
import { DataTable, type DataTableNames, flexRender, useDataTable } from "./DataTable";
import type { PagerOptions } from "./Pager";
import { type LayoutColumns, useTableLayout } from "./table-layout";

/** A row that is not an issue: the core knows nothing of what its rows are. */
interface Deploy {
  id: string;
  service: string;
  state: string;
  minutes: number;
  region: string;
}

const COLUMNS: ColumnDef<Deploy>[] = [
  { accessorFn: (row) => row.id, enableColumnFilter: false, header: "Deploy", id: "deploy" },
  {
    accessorFn: (row) => row.service,
    filterFn: "includesString",
    header: "Service",
    id: "service",
    meta: { text: true },
  },
  { accessorFn: (row) => row.state, enableColumnFilter: false, header: "State", id: "state" },
  {
    accessorFn: (row) => row.minutes,
    enableColumnFilter: false,
    header: "Minutes",
    id: "minutes",
    sortDescFirst: false,
  },
  { accessorFn: (row) => row.region, enableColumnFilter: false, header: "Region", id: "region" },
];
/** The table's own column ids, which its stored layout keeps and no other, and the attribute
 *  its rows carry: a deploy's row, never an issue's. */
const LAYOUT_COLUMNS: LayoutColumns = {
  ids: COLUMNS.flatMap((column) => (column.id === undefined ? [] : [column.id])),
  rowAttribute: "data-deploy-row",
};

/** The caller's own words for its deploys, which the core reads: a scope of its own, the keys
 *  of the page that draws it, and the attributes its rows and links carry. */
const DEPLOY_NAMES: DataTableNames = {
  filterCountNoun: { one: "deploy", many: "deploys" },
  groupAttribute: "data-deploy-group",
  keymap: {
    next: { id: "deploys-next", keys: "j", label: "Next deploy" },
    open: { id: "deploys-open", keys: "o", label: "Open deploy" },
    openOnEnter: { id: "deploys-open-enter", keys: "Enter", label: "Open the focused deploy" },
    previous: { id: "deploys-previous", keys: "k", label: "Previous deploy" },
    scope: "agents",
    select: { id: "deploys-select", keys: "x", label: "Select or deselect the focused deploy" },
  },
  linkAttribute: "data-deploy-link",
  rowAttribute: "data-deploy-row",
};

/** The line under the deploys table, counting what the caller counts. */
const DEPLOY_PAGER: PagerOptions = {
  nouns: {
    group: { one: "group", many: "groups" },
    item: { one: "deploy", many: "deploys" },
    row: { one: "row", many: "rows" },
  },
  pageSizes: [10, 25, 50],
  testId: "deploys-pager",
};

/** A table whose columns share the issue table's names, `title` and `status`, and none of its
 *  layout rules: no pin, no minimum, no starting width it did not give. */
const NAMESAKE_COLUMNS: ColumnDef<Deploy>[] = [
  { accessorFn: (row) => row.id, enableColumnFilter: false, header: "Deploy", id: "deploy" },
  { accessorFn: (row) => row.service, enableColumnFilter: false, header: "Service", id: "title" },
  { accessorFn: (row) => row.state, enableColumnFilter: false, header: "State", id: "status" },
  { accessorFn: (row) => row.region, enableColumnFilter: false, header: "Region", id: "region" },
];
const NAMESAKE_LAYOUT: LayoutColumns = {
  ids: ["deploy", "title", "status", "region"],
  rowAttribute: "data-deploy-row",
};

const DEPLOYS: Deploy[] = [
  { id: "d1", minutes: 9, region: "eu", service: "billing", state: "live" },
  { id: "d2", minutes: 2, region: "us", service: "search", state: "live" },
  { id: "d3", minutes: 5, region: "eu", service: "ledger", state: "rolled back" },
];

const NO_COUNTS: ReadonlyMap<string, number> = new Map();

/** A caller of the core: it holds the view itself, as the issue table holds it in the URL. */
function Deploys({
  columns = COLUMNS,
  grouping: initialGrouping = [],
  groupOrder,
  layoutColumns = LAYOUT_COLUMNS,
  layoutKey = "deploys",
  pageSize = 25,
  pager = DEPLOY_PAGER,
  rows = DEPLOYS,
}: {
  columns?: ColumnDef<Deploy>[];
  grouping?: GroupingState;
  groupOrder?: (columnId: string, a: string, b: string) => number;
  layoutColumns?: LayoutColumns;
  layoutKey?: string;
  pageSize?: number;
  /** The line under the table; `null` draws a table with no pager at all. */
  pager?: PagerOptions | null;
  rows?: Deploy[];
}): ReactNode {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [grouping, setGrouping] = useState<GroupingState>(initialGrouping);
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([]);
  const [globalFilter, setGlobalFilter] = useState("");
  const [pagination, setPagination] = useState<PaginationState>({ pageIndex: 0, pageSize });
  const [openFilter, setOpenFilter] = useState<string>();
  const layout = useTableLayout(layoutKey, layoutColumns);
  const state = { columnFilters, globalFilter, grouping, pagination, sorting };
  const model = useDataTable<Deploy>({
    columns,
    getRowId: (row) => row.id,
    ...(groupOrder === undefined
      ? {}
      : {
          groupOrder: {
            compare: groupOrder,
            heading: (columnId: string, row: Deploy) => (columnId === "region" ? row.region : ""),
          },
        }),
    layout,
    names: DEPLOY_NAMES,
    onColumnFiltersChange: setColumnFilters,
    onGlobalFilterChange: setGlobalFilter,
    onGroupingChange: setGrouping,
    onPaginationChange: setPagination,
    onSortingChange: setSorting,
    rows,
    state,
    viewKey: JSON.stringify(state),
  });
  return (
    <section aria-label={layoutKey}>
      <DataTable
        empty="No deploys match."
        filterCounts={NO_COUNTS}
        label={`Deploys ${layoutKey}`}
        model={model}
        onOpenFilterChange={setOpenFilter}
        openFilter={openFilter}
        pager={pager ?? undefined}
        renderGroup={({ entry, index, measure }) => (
          <tr data-group={String(entry.row.groupingValue)} data-index={index} ref={measure}>
            <td>{String(entry.row.groupingValue)}</td>
          </tr>
        )}
        renderRow={({ row, index, measure }) => (
          <tr data-deploy-row={row.id} data-index={index} ref={measure} tabIndex={-1}>
            {row.getVisibleCells().map((cell) => (
              <td key={cell.id}>{flexRender(cell.column.columnDef.cell, cell.getContext())}</td>
            ))}
          </tr>
        )}
        scrollTestId={`${layoutKey}-scroll`}
      />
    </section>
  );
}

/** The page the table sits on, which pushes the scope its keys are registered in. */
function Scope(): ReactNode {
  useKeymapScope("agents");
  return null;
}

const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});

/** The unit DOM lays nothing out: give each table's box the height a window would, and the
 *  width `boxWidth` where a test names one. */
function draw(tree: ReactNode, boxWidth?: number) {
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid?.endsWith("-scroll") ? 600 : (descriptor?.get?.call(this) ?? 0);
    },
  });
  if (boxWidth !== undefined) {
    const rect = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement
    ) {
      return new DOMRect(0, 0, this.dataset.testid?.endsWith("-scroll") ? boxWidth : 100, 40);
    });
    restores.push(() => rect.mockRestore());
  }
  const view = render(tree);
  restores.push(() => {
    view.unmount();
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
  });
  return view;
}

const listed = (table: HTMLElement) =>
  [...table.querySelectorAll("[data-deploy-row]")].map((row) =>
    row.getAttribute("data-deploy-row")
  );

test("draws the rows it is given, of a type that is not an issue, under its label", async () => {
  draw(<Deploys />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(listed(table)).toEqual(["d1", "d2", "d3"]);
  expect(within(table).getByText("billing")).toBeInTheDocument();
  expect(screen.getByText("3 of 3 deploys")).toBeInTheDocument();
});

test("sorts by a column's value when its header is pressed", async () => {
  const user = userEvent.setup();
  draw(<Deploys />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  await user.click(within(table).getByRole("button", { name: "Minutes" }));

  expect(listed(table)).toEqual(["d2", "d3", "d1"]);
  expect(within(table).getByRole("columnheader", { name: /Minutes/ })).toHaveAttribute(
    "aria-sort",
    "ascending"
  );
});

test("filters by the text typed into a column's filter", async () => {
  const user = userEvent.setup();
  draw(<Deploys />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  await user.click(within(table).getByRole("button", { name: "Filter Service" }));
  await user.type(screen.getByRole("textbox", { name: "Filter Service" }), "led");

  expect(listed(table)).toEqual(["d3"]);
  expect(screen.getByText("1 of 3 deploys")).toBeInTheDocument();
});

test("pages its rows and moves to the next page", async () => {
  const user = userEvent.setup();
  const rows = Array.from({ length: 12 }, (_, index) => ({
    id: `d${index + 1}`,
    minutes: index,
    region: "eu",
    service: `service ${index + 1}`,
    state: "live",
  }));
  draw(<Deploys pageSize={10} rows={rows} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(listed(table)).toHaveLength(10);
  expect(screen.getByText("Rows 1–10 of 12")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Next ›" }));

  expect(listed(table)).toEqual(["d11", "d12"]);
  expect(screen.getByText("Page 2 of 2")).toBeInTheDocument();
});

test("groups its rows with the headings in the caller's order", async () => {
  draw(<Deploys groupOrder={(_column, a, b) => b.localeCompare(a)} grouping={["region"]} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(
    [...table.querySelectorAll("[data-group]")].map((row) => row.getAttribute("data-group"))
  ).toEqual(["us", "eu"]);
  expect(listed(table)).toEqual(["d2", "d1", "d3"]);
});

test("says the caller's own words when no row is listed", async () => {
  draw(<Deploys rows={[]} />);

  await screen.findByRole("table", { name: "Deploys deploys" });
  expect(screen.getByText("No deploys match.")).toBeInTheDocument();
});

test("keeps its own columns' widths, order and visibility under the whole key it is given", async () => {
  const stored = {
    order: ["region", "deploy"],
    sizing: { minutes: 210 },
    visibility: { state: false },
  };
  localStorage.setItem("deploys", JSON.stringify(stored));
  draw(
    <>
      <Deploys layoutKey="deploys" />
      <Deploys layoutKey="releases" />
    </>
  );

  const deploys = await screen.findByRole("table", { name: "Deploys deploys" });
  const releases = screen.getByRole("table", { name: "Deploys releases" });
  const headings = (table: HTMLElement) =>
    within(table)
      .getAllByRole("columnheader")
      .map((header) => header.querySelector("button")?.textContent);
  expect(headings(deploys)).toEqual(["Region", "Deploy", "Service", "Minutes"]);
  expect(within(deploys).getByRole("columnheader", { name: /Minutes/ })).toHaveStyle({
    width: "210px",
  });
  expect(headings(releases)).toEqual(["Deploy", "Service", "State", "Minutes", "Region"]);
  expect(JSON.parse(localStorage.getItem("deploys") ?? "{}")).toEqual(stored);
  expect(JSON.parse(localStorage.getItem("releases") ?? "{}")).toEqual({
    order: [],
    sizing: {},
    visibility: {},
  });
});

// The issue table's layout rules are the issue table's, not a column name's: a table with its own
// `title` and `status` keeps the hidden state and the widths its reader stored, at a desktop box
// and at a phone's, and pins nothing.
for (const boxWidth of [1400, 390]) {
  test(`a table with title and status columns keeps its own layout rules in a ${boxWidth} px box`, async () => {
    const stored = { order: [], sizing: { deploy: 40, status: 50 }, visibility: { title: false } };
    localStorage.setItem("namesakes", JSON.stringify(stored));
    draw(
      <Deploys columns={NAMESAKE_COLUMNS} layoutColumns={NAMESAKE_LAYOUT} layoutKey="namesakes" />,
      boxWidth
    );

    const table = await screen.findByRole("table", { name: "Deploys namesakes" });
    const headers = within(table).getAllByRole("columnheader");
    expect(headers.map((header) => header.querySelector("button")?.textContent)).toEqual([
      "Deploy",
      "State",
      "Region",
    ]);
    expect(headers.map((header) => header.style.width)).toEqual(["40px", "50px", "150px"]);
    expect(headers.map((header) => header.style.left)).toEqual(["", "", ""]);
    expect(JSON.parse(localStorage.getItem("namesakes") ?? "{}")).toEqual(stored);
  });
}

// The reader moving the view - a sort, a filter, a page - starts the box at its top; the rows
// changing under a view the reader kept leaves the box where the reader scrolled it.
test("a view change scrolls the box to its top and a data change keeps it where it was", async () => {
  const user = userEvent.setup();
  const rows = (service: string) =>
    Array.from({ length: 20 }, (_, index) => ({
      id: `d${index + 1}`,
      minutes: index,
      region: "eu",
      service: `${service} ${index + 1}`,
      state: "live",
    }));
  const view = draw(<Deploys rows={rows("billing")} />);
  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const box = screen.getByTestId("deploys-scroll");

  box.scrollTop = 200;
  view.rerender(<Deploys rows={rows("ledger")} />);
  await screen.findByText("ledger 1");
  expect(box.scrollTop).toBe(200);

  await user.click(within(table).getByRole("button", { name: "Minutes" }));
  expect(box.scrollTop).toBe(0);
});

test("a table given no pager draws none", async () => {
  draw(<Deploys pager={null} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(listed(table)).toEqual(["d1", "d2", "d3"]);
  expect(screen.queryByTestId("deploys-pager")).toBeNull();
  expect(screen.queryByRole("combobox", { name: "Rows per page" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Next ›" })).toBeNull();
});

// The core knows nothing of what its rows are called: a page gives it the attribute its rows
// carry and the scope and labels its keys are registered under, and those are what it uses.
test("walks the rows by the attribute and the keymap scope the caller gives", async () => {
  const user = userEvent.setup();
  draw(
    <KeymapProvider>
      <Scope />
      <Deploys />
    </KeymapProvider>
  );

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(table.querySelector("[data-issue-row]")).toBeNull();
  const rows = [...table.querySelectorAll<HTMLElement>("[data-deploy-row]")];

  await user.keyboard("j");
  expect(rows[0]).toHaveFocus();
  await user.keyboard("j");
  expect(rows[1]).toHaveFocus();
  await user.keyboard("k");
  expect(rows[0]).toHaveFocus();
});
