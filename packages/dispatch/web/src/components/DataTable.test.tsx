import { afterEach, expect, spyOn, test } from "bun:test";
import {
  type ColumnDef,
  type ColumnFiltersState,
  flexRender,
  type GroupingState,
  type PaginationState,
  type SortingState,
} from "@tanstack/react-table";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { type LayoutColumns, useTableLayout } from "../features/project/issues-table-layout";
import { tableStorageKey } from "../features/project/issues-table-storage";
import { appKeymap } from "../features/shell/keymap";
import { DataTable, DataTableGroupRow, SORT_MARK_WIDTH, useDataTable } from "./DataTable";

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
/** The table's own column ids, which its stored layout keeps and no other. */
const LAYOUT_COLUMNS: LayoutColumns = {
  ids: COLUMNS.flatMap((column) => (column.id === undefined ? [] : [column.id])),
};

/** A table whose columns share the issue table's names, `title` and `status`, and none of its
 *  layout rules: no pin, no minimum, no starting width it did not give. */
const NAMESAKE_COLUMNS: ColumnDef<Deploy>[] = [
  { accessorFn: (row) => row.id, enableColumnFilter: false, header: "Deploy", id: "deploy" },
  { accessorFn: (row) => row.service, enableColumnFilter: false, header: "Service", id: "title" },
  { accessorFn: (row) => row.state, enableColumnFilter: false, header: "State", id: "status" },
  { accessorFn: (row) => row.region, enableColumnFilter: false, header: "Region", id: "region" },
];
const NAMESAKE_LAYOUT: LayoutColumns = { ids: ["deploy", "title", "status", "region"] };

const DEPLOYS: Deploy[] = [
  { id: "d1", minutes: 9, region: "eu", service: "billing", state: "live" },
  { id: "d2", minutes: 2, region: "us", service: "search", state: "live" },
  { id: "d3", minutes: 5, region: "eu", service: "ledger", state: "rolled back" },
];

const NO_COUNTS: ReadonlyMap<string, number> = new Map();

/** A caller of the core: it holds the view itself, as the issue table holds it in the URL. A
 *  `plain` caller's rows are no issues: they carry `data-row`, every one is drawn, and the table
 *  has no pager and no row keys. */
function Deploys({
  columns = COLUMNS,
  grouping: initialGrouping = [],
  groupOrder,
  layoutColumns = LAYOUT_COLUMNS,
  layoutKey = "deploys",
  pageSize = 25,
  plain = false,
  rows = DEPLOYS,
  stackedHeader,
}: {
  columns?: ColumnDef<Deploy>[];
  grouping?: GroupingState;
  groupOrder?: (columnId: string, a: string, b: string) => number;
  layoutColumns?: LayoutColumns;
  layoutKey?: string;
  pageSize?: number;
  plain?: boolean;
  rows?: Deploy[];
  stackedHeader?: string;
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
    onColumnFiltersChange: setColumnFilters,
    onGlobalFilterChange: setGlobalFilter,
    onGroupingChange: setGrouping,
    onPaginationChange: setPagination,
    onSortingChange: setSorting,
    rowKeys: !plain,
    rows,
    state,
    viewKey: JSON.stringify(state),
    virtual: !plain,
  });
  return (
    <section aria-label={layoutKey}>
      <DataTable
        empty="No deploys match."
        filterCounts={NO_COUNTS}
        label={`Deploys ${layoutKey}`}
        model={model}
        noun={{ many: "deploys", one: "deploy" }}
        onOpenFilterChange={setOpenFilter}
        openFilter={openFilter}
        pager={!plain}
        renderGroup={({ entry: { row }, index, measure }) => (
          <DataTableGroupRow
            columns={model.table.getVisibleLeafColumns().length}
            expanded={row.getIsExpanded()}
            heading={String(row.groupingValue)}
            index={index}
            label={`${row.getIsExpanded() ? "Collapse" : "Expand"} ${String(row.groupingValue)}`}
            measure={measure}
            onToggle={row.getToggleExpandedHandler()}
            slot={<span>{row.getLeafRows().length} deploys</span>}
            width={300}
          />
        )}
        renderRow={({ row, index, measure }) => (
          <tr
            data-index={index}
            {...{ [plain ? "data-row" : "data-issue-row"]: row.id }}
            ref={measure}
            tabIndex={-1}
          >
            {row.getVisibleCells().map((cell) => (
              <td key={cell.id}>{flexRender(cell.column.columnDef.cell, cell.getContext())}</td>
            ))}
          </tr>
        )}
        rowAttribute={plain ? "data-row" : undefined}
        scrollTestId={`${layoutKey}-scroll`}
        stackedHeader={stackedHeader}
      />
    </section>
  );
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
    // Stubbed on Element, the prototype that owns the method: patching HTMLElement would leave an
    // own property behind that shadows every later spy on Element.
    const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
      this: Element
    ) {
      const box = this.getAttribute("data-testid")?.endsWith("-scroll") === true;
      return new DOMRect(0, 0, box ? boxWidth : 100, 40);
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
  [...table.querySelectorAll("[data-issue-row]")].map((row) => row.getAttribute("data-issue-row"));

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

/** Sixty deploys: more rows than a 600 px box draws at once. */
const MANY = Array.from({ length: 60 }, (_, index) => ({
  id: `d${index + 1}`,
  minutes: index,
  region: "eu",
  service: `service ${index + 1}`,
  state: "live",
}));

test("by default it draws the issue tables' rows: the ones in view, each with data-issue-row, the pager under them and the row keys bound", async () => {
  draw(<Deploys pageSize={100} rows={MANY} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(listed(table).length).toBeGreaterThan(0);
  expect(listed(table).length).toBeLessThan(MANY.length);
  expect(screen.getByTestId("issues-table-pager")).toBeInTheDocument();
  expect(appKeymap.describe().map((binding) => binding.id)).toContain("list-next");
});

test("a table whose rows are no issues draws every row under its own attribute, with no pager and no row keys", async () => {
  draw(<Deploys pageSize={100} plain rows={MANY} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(table.querySelectorAll("[data-issue-row]")).toHaveLength(0);
  expect(
    [...table.querySelectorAll("[data-row]")].map((row) => row.getAttribute("data-row"))
  ).toEqual(MANY.map((row) => row.id));
  expect(screen.queryByTestId("issues-table-pager")).toBeNull();
  expect(appKeymap.describe().map((binding) => binding.id)).not.toContain("list-next");
  // As tall as its rows: the box takes no height of its own.
  expect(screen.getByTestId("deploys-scroll").style.height).toBe("");
});

test("groups its rows with the headings in the caller's order", async () => {
  draw(<Deploys groupOrder={(_column, a, b) => b.localeCompare(a)} grouping={["region"]} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(
    within(table)
      .getAllByRole("button", { name: /^(Collapse|Expand) / })
      .map((toggle) => toggle.getAttribute("aria-label"))
  ).toEqual(["Collapse us", "Collapse eu"]);
  expect(listed(table)).toEqual(["d2", "d1", "d3"]);
});

test("a group's heading row folds its rows away and opens them again, with the caller's slot beside it", async () => {
  const user = userEvent.setup();
  draw(<Deploys groupOrder={(_column, a, b) => a.localeCompare(b)} grouping={["region"]} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const eu = within(table).getByRole("button", { name: "Collapse eu" });
  expect(eu).toHaveAttribute("aria-expanded", "true");
  expect(eu.closest("td")).toHaveAttribute("colspan", "5");
  expect(within(table).getByText("2 deploys")).toBeInTheDocument();
  await user.click(eu);
  expect(listed(table)).toEqual(["d2"]);
  const folded = within(table).getByRole("button", { name: "Expand eu" });
  expect(folded).toHaveAttribute("aria-expanded", "false");
  await user.click(folded);
  expect(listed(table)).toEqual(["d1", "d3", "d2"]);
});

test("says the caller's own words when no row is listed", async () => {
  draw(<Deploys rows={[]} />);

  await screen.findByRole("table", { name: "Deploys deploys" });
  expect(screen.getByText("No deploys match.")).toBeInTheDocument();
});

test("keeps its own columns' widths, order and visibility under its own key", async () => {
  const stored = {
    order: ["region", "deploy"],
    sizing: { minutes: 210 },
    visibility: { state: false },
  };
  localStorage.setItem(tableStorageKey("deploys"), JSON.stringify(stored));
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
  expect(JSON.parse(localStorage.getItem(tableStorageKey("deploys")) ?? "{}")).toEqual(stored);
  expect(JSON.parse(localStorage.getItem(tableStorageKey("releases")) ?? "{}")).toEqual({
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
    localStorage.setItem(tableStorageKey("namesakes"), JSON.stringify(stored));
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
    expect(JSON.parse(localStorage.getItem(tableStorageKey("namesakes")) ?? "{}")).toEqual(stored);
  });
}

// A phone stands a header's filter under its words only in the column its caller names, as the
// issues table names its title: another table's `title` stands on one line there.
test("a phone stacks the header its caller names, and no other table's title", async () => {
  draw(
    <>
      <Deploys
        columns={NAMESAKE_COLUMNS}
        layoutColumns={NAMESAKE_LAYOUT}
        layoutKey="stacked"
        stackedHeader="title"
      />
      <Deploys columns={NAMESAKE_COLUMNS} layoutColumns={NAMESAKE_LAYOUT} layoutKey="unstacked" />
    </>,
    390
  );
  await screen.findByRole("table", { name: "Deploys stacked" });
  const stacked = (name: string) =>
    within(screen.getByRole("table", { name }))
      .getByRole("columnheader", { name: /^Service/ })
      .firstElementChild?.classList.contains("flex-col");
  expect([stacked("Deploys stacked"), stacked("Deploys unstacked")]).toEqual([true, false]);
});

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

// dispatch://INFRA-2918/spec#b-r7: every header holds its label, its filter button and, while
// its column is sorted, its sort mark, with nothing cut. A column is never narrower than the
// larger of its declared minimum and what its header draws, the mark's slot counted sorted or
// not, so a sort neither widens a column nor takes room from a label. happy-dom lays nothing out:
// each label and the filter button are given a width where the header cell reads them, a range
// over what each holds.
test("a column's minimum width is at least its header's label, filter and sort mark", async () => {
  const user = userEvent.setup();
  const labels: Readonly<Record<string, number>> = {
    Deploy: 60,
    Minutes: 120,
    Region: 50,
    Service: 70,
    State: 30,
  };
  const filter = 32;
  const range = spyOn(Range.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Range
  ) {
    const holder = this.startContainer;
    const width =
      holder instanceof Element && holder.querySelector("button") !== null
        ? filter
        : (labels[holder.textContent ?? ""] ?? 0);
    return new DOMRect(0, 0, width, 16);
  });
  restores.push(() => range.mockRestore());
  // Every starting width but State's is under its header, Region's declared minimum is over its
  // header, and the reader dragged Minutes under its header.
  localStorage.setItem(
    tableStorageKey("headers"),
    JSON.stringify({ order: [], sizing: { minutes: 30 }, visibility: {} })
  );
  const narrow: LayoutColumns = {
    ids: LAYOUT_COLUMNS.ids,
    minimums: { region: 90 },
    sizes: { deploy: 40, minutes: 50, region: 40, service: 50, state: 200 },
  };
  draw(<Deploys layoutColumns={narrow} layoutKey="headers" />);

  const table = await screen.findByRole("table", { name: "Deploys headers" });
  const widths = () =>
    within(table)
      .getAllByRole("columnheader")
      .map((header) => header.style.width);
  const minutes = () => within(table).getByRole("columnheader", { name: /^Minutes/ });
  const mark = () => minutes().querySelector("button")?.lastElementChild;
  const drawn = [
    `${60 + SORT_MARK_WIDTH}px`,
    `${70 + SORT_MARK_WIDTH + filter}px`,
    "200px",
    `${120 + SORT_MARK_WIDTH}px`,
    "90px",
  ];
  expect(widths()).toEqual(drawn);
  expect(mark()?.textContent).toBe("");
  expect(mark()).toHaveStyle({ width: `${SORT_MARK_WIDTH}px` });

  await user.click(within(table).getByRole("button", { name: /^Minutes/ }));
  expect(minutes()).toHaveAttribute("aria-sort", "ascending");
  expect(mark()?.textContent).toBe("▲");
  expect(mark()).toHaveStyle({ width: `${SORT_MARK_WIDTH}px` });
  expect(widths()).toEqual(drawn);

  await user.click(within(table).getByRole("button", { name: /^Minutes/ }));
  expect(minutes()).toHaveAttribute("aria-sort", "descending");
  expect(widths()).toEqual(drawn);

  await user.click(within(table).getByRole("button", { name: /^Service/ }));
  expect(within(table).getByRole("columnheader", { name: /^Service/ })).toHaveAttribute(
    "aria-sort",
    "ascending"
  );
  expect(widths()).toEqual(drawn);
});
