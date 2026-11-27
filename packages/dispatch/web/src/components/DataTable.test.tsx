import { afterEach, expect, spyOn, test } from "bun:test";
import type {
  ColumnDef,
  ColumnFiltersState,
  GroupingState,
  PaginationState,
  SortingState,
} from "@tanstack/react-table";
// biome-ignore lint/style/noRestrictedImports: a column's drag and drop, which user-event has no form for.
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MOUSE_CSS, SIZE_CSS, sizeUnder } from "../__tests__/control-size";
import { DEPLOY_NAMES, DEPLOY_NOUN } from "../__tests__/deploy-table";
import { openList, optionNames, rows as optionsOf, pick, pickNamed } from "../__tests__/list-picks";
import { endWindowScroll } from "../__tests__/scroll-box";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { useKeymapScope } from "../features/shell/keymap";
import { selectedCardBg } from "../theme/classes";
import type { ControlSize } from "./control-names";
import {
  type ColumnFilterRenderer,
  DataTable,
  DataTableCells,
  DataTableGroupRow,
  type DataTableNames,
  type DataTableProps,
  type RowNoun,
  useDataTable,
} from "./DataTable";
import type { PagerOptions } from "./Pager";
import { type LayoutColumns, SORT_MARK_WIDTH, useTableLayout } from "./table-layout";

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
/** The same columns with Region and State filtered by picking their values, which the shared
 *  filter offers with a count beside each. */
const PICKED_COLUMNS: ColumnDef<Deploy>[] = COLUMNS.map((column) =>
  column.id === "region" || column.id === "state"
    ? { ...column, enableColumnFilter: true, filterFn: "arrIncludesSome" }
    : column
);
/** How many of `DEPLOYS` each value of Region and State lists, by column. */
const PICKED_COUNTS: Readonly<Record<string, ReadonlyMap<string, number>>> = {
  region: new Map([
    ["eu", 2],
    ["us", 1],
  ]),
  state: new Map([
    ["live", 2],
    ["rolled back", 1],
  ]),
};
/** The table's own column ids, which its stored layout keeps and no other, and the attribute
 *  its rows carry: a deploy's row, never an issue's. */
const LAYOUT_COLUMNS: LayoutColumns = {
  ids: COLUMNS.flatMap((column) => (column.id === undefined ? [] : [column.id])),
  rowAttribute: "data-deploy-row",
};

/** The line under the deploys table, with the page sizes the caller offers. */
const DEPLOY_PAGER: PagerOptions = {
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

/** The same deploys on a page that pins: Deploy and Service stay at the box's left edge while
 *  the other columns scroll under them, Service giving up width first. Every rule here is this
 *  caller's — the starting widths, the smallest each column is drawn at, and which columns are
 *  held on a wide screen and on a phone. */
const PINNED_LAYOUT: LayoutColumns = {
  ids: LAYOUT_COLUMNS.ids,
  minimums: { deploy: 80, service: 120 },
  pins: {
    flexible: "service",
    phone: {
      columns: ["deploy", "service"],
      flexibleMin: 110,
      share: 0.8,
      widths: { deploy: 100 },
    },
    wide: { columns: ["deploy", "service"], share: 0.6 },
  },
  rowAttribute: "data-deploy-row",
  sizes: { deploy: 120, minutes: 100, region: 100, service: 240, state: 100 },
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
  counts,
  fillColumn,
  filterCounted,
  fixedColumns,
  focusTarget,
  groupSlot,
  grouping: initialGrouping = [],
  groupOrder,
  highlighted,
  layoutColumns = LAYOUT_COLUMNS,
  layoutKey = "deploys",
  names = DEPLOY_NAMES,
  noun = DEPLOY_NOUN,
  pageSize = 25,
  pager = DEPLOY_PAGER,
  renderFilter,
  rowHeight,
  rows = DEPLOYS,
  serverPage,
  size,
  stackColumns,
  trailingWidth,
}: {
  columns?: ColumnDef<Deploy>[];
  /** By column id, how many deploys each of that column's values lists, which its filter counts
   *  with while it is open. */
  counts?: Readonly<Record<string, ReadonlyMap<string, number>>>;
  /** The column that takes what the box has over the others' widths. */
  fillColumn?: string;
  /** Which values of the open filter's column the counts have counted. */
  filterCounted?: DataTableProps<Deploy>["filterCounted"];
  /** The columns this caller's order keeps in their place without pinning them. */
  fixedColumns?: readonly string[];
  /** An undrawn row whose trailing control this caller asks to show whole. */
  focusTarget?: string;
  /** What this caller draws beside a group's heading, in place of the count of its deploys. */
  groupSlot?: ReactNode;
  grouping?: GroupingState;
  groupOrder?: (columnId: string, a: string, b: string) => number;
  /** The deploy whose row this caller is pointing at, painted in its own class. */
  highlighted?: string;
  layoutColumns?: LayoutColumns;
  layoutKey?: string;
  /** The caller's own words, which name the keys and where they are answered from. */
  names?: DataTableNames;
  /** What this caller calls its rows, which its filters and its pager count in. */
  noun?: RowNoun;
  pageSize?: number;
  /** The line under the table; `null` draws a table with no pager at all. */
  pager?: PagerOptions | null;
  renderFilter?: ColumnFilterRenderer<Deploy>;
  /** The one height this caller draws every row at. */
  rowHeight?: number;
  rows?: Deploy[];
  /** The rows are one page a server answered, of this many rows in all. */
  serverPage?: { readonly rowCount: number };
  /** The size the page draws its controls at, which the table hands its own. */
  size?: ControlSize;
  /** The columns whose headers stack their sort over their filter in a phone's box. */
  stackColumns?: readonly string[];
  /** The width of the action cell each row draws after its columns. */
  trailingWidth?: number;
}): ReactNode {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [grouping, setGrouping] = useState<GroupingState>(initialGrouping);
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([]);
  const [globalFilter, setGlobalFilter] = useState("");
  const [pagination, setPagination] = useState<PaginationState>({ pageIndex: 0, pageSize });
  const [openFilter, setOpenFilter] = useState<string>();
  const [rolledBack, setRolledBack] = useState<ReadonlySet<string>>(new Set());
  const layout = { ...useTableLayout(layoutKey, layoutColumns, sorting), fixedColumns };
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
    names,
    onColumnFiltersChange: setColumnFilters,
    onGlobalFilterChange: setGlobalFilter,
    onGroupingChange: setGrouping,
    onPaginationChange: setPagination,
    onSortingChange: setSorting,
    rowHeight,
    rows,
    serverPage,
    state,
    viewKey: JSON.stringify(state),
  });
  return (
    <section aria-label={layoutKey}>
      {focusTarget === undefined ? null : (
        <button type="button" onClick={() => model.focusRow(focusTarget, "button", "nearest")}>
          Return to row
        </button>
      )}
      <DataTable
        empty="No deploys match."
        fillColumn={fillColumn}
        filterCounted={filterCounted}
        filterCounts={counts?.[openFilter ?? ""] ?? NO_COUNTS}
        label={`Deploys ${layoutKey}`}
        model={model}
        noun={noun}
        onOpenFilterChange={setOpenFilter}
        openFilter={openFilter}
        pager={pager ?? undefined}
        renderFilter={renderFilter}
        renderGroup={({ entry: { continued, row }, index, measure }) => (
          <DataTableGroupRow
            columns={model.table.getVisibleLeafColumns().length}
            expanded={row.getIsExpanded()}
            heading={<em>{String(row.groupingValue)}</em>}
            index={index}
            label={`${row.getIsExpanded() ? "Collapse" : "Expand"} ${String(row.groupingValue)}${continued ? ", continued" : ""}`}
            measure={measure}
            onToggle={row.getToggleExpandedHandler()}
            slot={groupSlot ?? <span>{row.getLeafRows().length} deploys</span>}
            width={300}
          />
        )}
        renderRow={({ row, index, measure }) => (
          <tr data-deploy-row={row.id} data-index={index} ref={measure} tabIndex={-1}>
            <DataTableCells
              highlight={row.id === highlighted ? selectedCardBg : undefined}
              row={row}
              // A deploy rolled back has nothing left to roll back: the button goes, and the row
              // stays.
              trailing={
                trailingWidth === undefined ? undefined : rolledBack.has(row.id) ? null : (
                  <button
                    onClick={() => setRolledBack((done) => new Set(done).add(row.id))}
                    type="button"
                  >
                    Roll back {row.id}
                  </button>
                )
              }
            />
          </tr>
        )}
        scrollTestId={`${layoutKey}-scroll`}
        size={size}
        stackColumns={stackColumns}
        trailingWidth={trailingWidth}
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

// A caller whose counts read something besides the rows: it says, by column and by value, which
// counts are counted yet. The shared filter draws a count beside each value that is and none
// beside one that is not, never a 0, and what it says of one column's values leaves another's.
test("the shared filter draws a count beside each value its caller says is counted, in that column alone", async () => {
  const user = userEvent.setup();
  draw(
    <Deploys
      columns={PICKED_COLUMNS}
      counts={PICKED_COUNTS}
      filterCounted={(column, value) => column !== "region" || value === "eu"}
    />
  );

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  /** One header filter's options, by the name each is read out as, with its panel shut after. */
  const options = async (header: string) => {
    await user.click(within(table).getByRole("button", { name: `Filter ${header}` }));
    const names = optionNames(screen.getByRole("listbox", { name: `${header} options` }));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
    return names;
  };
  expect(await options("Region")).toEqual(["eu, 2 deploys", "us"]);
  expect(await options("State")).toEqual(["live, 2 deploys", "rolled back, 1 deploy"]);
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

// A server's page: the rows are the page on screen, so the table slices none of them and counts
// its pages by the server's row count; a step asks the caller for the next page.
test("a server's page is drawn whole and paged by the server's row count", async () => {
  const user = userEvent.setup();
  const rows = Array.from({ length: 10 }, (_, index) => ({
    id: `d${index + 1}`,
    minutes: index,
    region: "eu",
    service: `service ${index + 1}`,
    state: "live",
  }));
  // Pages of 10 rows: the server holds 23, so 3 pages, and it answered the first.
  draw(<Deploys pageSize={10} rows={rows} serverPage={{ rowCount: 23 }} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(listed(table)).toHaveLength(10);
  expect(screen.getByText("Page 1 of 3")).toBeInTheDocument();
  expect(screen.getByText("Rows 1–10 of 23")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Next ›" }));

  // The page index moved; the rows are the caller's until it hands the next page in. The range
  // is counted against the server's rows, not the one page the table holds.
  expect(screen.getByText("Page 2 of 3")).toBeInTheDocument();
  expect(screen.getByText("Rows 11–20 of 23")).toBeInTheDocument();
  expect(listed(table)).toEqual(rows.map((row) => row.id));
});

// A caller that counts its rows elsewhere (the Issues tab, on its tabs) asks for no total: the
// line keeps the range, the page, its steps and the page size. Every other caller keeps it.
test("a pager with no total draws the range, the page, its steps and the page size", async () => {
  const user = userEvent.setup();
  const rows = Array.from({ length: 12 }, (_, index) => ({
    id: `d${index + 1}`,
    minutes: index,
    region: "eu",
    service: `service ${index + 1}`,
    state: "live",
  }));
  draw(
    <Deploys
      pageSize={10}
      pager={{ allRows: 50, pageSizes: [10, 50], testId: "deploys-pager", totals: false }}
      rows={rows}
    />
  );

  await screen.findByRole("table", { name: "Deploys deploys" });
  const pager = screen.getByTestId("deploys-pager");
  expect(within(pager).getByText("Rows 1–10")).toBeInTheDocument();
  expect(within(pager).getByText("Page 1 of 2")).toBeInTheDocument();
  expect(within(pager).getByRole("button", { name: "‹ Previous" })).toBeInTheDocument();
  expect(within(pager).getByRole("button", { name: "Next ›" })).toBeInTheDocument();
  expect(pager.textContent).not.toMatch(/12|deploys/);

  await pick(user, "Rows per page", "All rows");
  expect(within(pager).getAllByText("All rows")).toHaveLength(2);
  expect(pager.textContent).not.toMatch(/12|deploys/);
});

// The press that reaches the last page, or the first, leaves the reader on the button they
// pressed: a step with no page for it is refused with `aria-disabled` rather than `disabled`,
// which the browser would take the focus off, and a press on it then does nothing.
test("a pager step that reaches the last page or the first keeps the focus, refused rather than disabled", async () => {
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
  const previous = screen.getByRole<HTMLButtonElement>("button", { name: "‹ Previous" });
  const next = screen.getByRole<HTMLButtonElement>("button", { name: "Next ›" });
  expect(previous).toHaveAttribute("aria-disabled", "true");
  expect(previous.disabled).toBe(false);
  expect(next).not.toHaveAttribute("aria-disabled");

  act(() => next.focus());
  await user.keyboard("{Enter}");
  expect(screen.getByText("Page 2 of 2")).toBeInTheDocument();
  expect(next).toHaveAttribute("aria-disabled", "true");
  expect(next.disabled).toBe(false);
  expect(next).toHaveFocus();
  await user.keyboard("{Enter}");
  expect(listed(table)).toEqual(["d11", "d12"]);
  expect(next).toHaveFocus();

  act(() => previous.focus());
  await user.keyboard("{Enter}");
  expect(screen.getByText("Page 1 of 2")).toBeInTheDocument();
  expect(previous).toHaveAttribute("aria-disabled", "true");
  expect(previous.disabled).toBe(false);
  expect(previous).toHaveFocus();
  await user.keyboard("{Enter}");
  expect(listed(table)).toHaveLength(10);
  expect(previous).toHaveFocus();
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

// k18 Three (R14): a page that starts with a leaf draws its group's heading first, and that
// heading reads continued only when the page before held a leaf of the group. A heading that was
// the page before's last row, with none of its rows there, opens its group on the next page.
test.each([
  // Page 1: the eu heading, 8 eu rows, then the us heading with no us row.
  ["first leaf opens page 2", 8, "Collapse us"],
  // Page 1: the eu heading and 9 of eu's 12 rows; its other 3 rows open page 2.
  ["leaves run onto page 2", 12, "Collapse eu, continued"],
])("the heading at page 2's top of the group whose %s", async (_name, eu, heading) => {
  const user = userEvent.setup();
  const rows = [
    ...Array.from({ length: eu }, (_, index) => ({
      id: `eu${index + 1}`,
      minutes: index,
      region: "eu",
      service: `service ${index + 1}`,
      state: "live",
    })),
    ...Array.from({ length: 4 }, (_, index) => ({
      id: `us${index + 1}`,
      minutes: index,
      region: "us",
      service: `service ${index + 1}`,
      state: "live",
    })),
  ];
  draw(
    <Deploys
      groupOrder={(_column, a, b) => a.localeCompare(b)}
      grouping={["region"]}
      pageSize={10}
      rows={rows}
    />
  );
  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  await user.click(screen.getByRole("button", { name: "Next ›" }));
  // The page's first line, which the box draws at index 0 below its spacer row.
  const first = table.querySelector('tbody tr[data-index="0"]');
  if (!(first instanceof HTMLElement)) throw new Error("Page 2 draws no line");
  const toggle = within(first).getByRole("button", { name: /^Collapse / });
  // The words the caller wrote, then the name a screen reader reads from them.
  expect(toggle.getAttribute("aria-label")).toBe(heading);
  expect(toggle).toHaveAccessibleName(heading);
});

test("says the caller's own words when no row is listed", async () => {
  draw(<Deploys rows={[]} />);

  await screen.findByRole("table", { name: "Deploys deploys" });
  expect(screen.getByText("No deploys match.")).toBeInTheDocument();
});

// The caller that asked for it: the project List stores under `dispatch.project-table.<project>`,
// and the priorities page of dispatch://INFRA-2947 under a key of its own. The key is the
// caller's whole string, which the core prefixes nothing to and reads nothing out of.
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

// A phone stands a header's filter under its words only in the column its caller names, as the
// issues table names its title: another table's `title` stands on one line there.
test("a phone stacks the header its caller names, and no other table's title", async () => {
  draw(
    <>
      <Deploys
        columns={NAMESAKE_COLUMNS}
        layoutColumns={NAMESAKE_LAYOUT}
        layoutKey="stacked"
        stackColumns={["title"]}
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

// The caller that asked for it: the priorities page of dispatch://INFRA-2947, whose fault lists
// fit one page and show no line counting them.
test("a table given no pager draws none", async () => {
  draw(<Deploys pager={null} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(listed(table)).toEqual(["d1", "d2", "d3"]);
  expect(screen.queryByTestId("deploys-pager")).toBeNull();
  expect(screen.queryByRole("button", { name: pickNamed("Rows per page") })).toBeNull();
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

/** The same words for a table whose next and previous answer from its own rows alone (`moveFrom:
 *  "row"`), for keys the page answers itself everywhere else, such as the arrows, which scroll it -
 *  the Delivery page's pull requests are walked that way (`features/delivery/PRList.tsx`): the same
 *  keys under the same ids, answered only while the focus stands in one of this table's rows. */
const BESIDE_NAMES: DataTableNames = {
  ...DEPLOY_NAMES,
  keymap: { ...DEPLOY_NAMES.keymap, moveFrom: "row" },
};

// INFRA-3135's review round 1, R10: a page that draws several tables over the same keys answers
// each key from the table the focus is in. A table beside the main one walks its own rows from one of
// them; the main one walks its rows from anywhere the focus is in no table, and from its own.
// The keymap answers a key with the first registration whose `when` holds, so this holds in
// either order the tables register in, with rows of the same ids under the same attribute.
for (const order of [
  ["main", "beside"],
  ["beside", "main"],
]) {
  test(`each of two tables on a page answers its keys from its own rows, and the main one from outside every table (${order.join(" before ")})`, async () => {
    const user = userEvent.setup();
    draw(
      <KeymapProvider>
        <Scope />
        {order.map((key) => (
          <Deploys key={key} layoutKey={key} names={key === "main" ? DEPLOY_NAMES : BESIDE_NAMES} />
        ))}
      </KeymapProvider>
    );
    await screen.findByRole("table", { name: "Deploys beside" });
    const [main, beside] = ["main", "beside"].map((key) => [
      ...screen
        .getByRole("table", { name: `Deploys ${key}` })
        .querySelectorAll<HTMLElement>("[data-deploy-row]"),
    ]);

    // Nowhere in a table: the main table's first row.
    await user.keyboard("j");
    expect(main?.[0], "j from the page").toHaveFocus();
    // In a row of the table beside it: that table's own next and previous rows.
    act(() => beside?.[0]?.focus());
    await user.keyboard("j");
    expect(beside?.[1], "j from the table beside").toHaveFocus();
    await user.keyboard("k");
    expect(beside?.[0], "k from the table beside").toHaveFocus();
    // Back in a row of the main table: its own.
    act(() => main?.[1]?.focus());
    await user.keyboard("j");
    expect(main?.[2], "j from the main table").toHaveFocus();
    // On a control of the table beside that is no row: neither table takes the key.
    const sort = within(screen.getByRole("table", { name: "Deploys beside" })).getByRole("button", {
      name: "Minutes",
    });
    act(() => sort.focus());
    await user.keyboard("j");
    expect(sort, "j from the header of the table beside").toHaveFocus();
  });
}

// R10, one test per thing a caller of the core decides. Each names the caller that asked for
// it: the priorities page of dispatch://INFRA-2947, the evals tab of dispatch://INFRA-2918, or
// the project List, which is the page this change moves onto the shared controls.

// The caller that asked for it: the priorities page of dispatch://INFRA-2947, whose column
// filter reads a registry of facets with a count against each value, and the evals tab of
// dispatch://INFRA-2918, whose filter reads the column's own values. The header cell draws what
// it is given and chooses nothing.
test("a header cell draws the filter its caller gives it, not one of its own", async () => {
  const user = userEvent.setup();
  draw(
    <Deploys
      renderFilter={({ column }) => (
        <button onClick={() => column.setFilterValue("led")} type="button">
          Facets for {String(column.columnDef.header)}
        </button>
      )}
    />
  );

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  // The shared filter the core would otherwise have chosen is nowhere in the header.
  expect(screen.queryByRole("button", { name: "Filter Service" })).toBeNull();

  await user.click(within(table).getByRole("button", { name: "Facets for Service" }));

  expect(listed(table)).toEqual(["d3"]);
});

const SORT_HINT = "Sort (shift-click to add a second sort)";

// INFRA-3221's R9: the sort hint moved from each sort button's native title into the shared
// tooltip, which a keyboard reaches too, so two tooltips never show at once. A pinned column
// cannot be dragged, so its hint says nothing of a drag.
test("a sort button carries no native title and opens its hint in the tooltip, on hover and on focus", async () => {
  const user = userEvent.setup();
  draw(<Deploys layoutColumns={PINNED_LAYOUT} layoutKey="pinned" />, 1400);

  const table = await screen.findByRole("table", { name: "Deploys pinned" });
  const sort = (name: string) => within(table).getByRole("button", { name });
  for (const name of ["Deploy", "Service", "State", "Minutes", "Region"])
    expect(sort(name)).not.toHaveAttribute("title");
  await user.hover(sort("Minutes"));
  expect(screen.getByRole("tooltip")).toHaveTextContent(`${SORT_HINT}; drag to move the column`, {
    normalizeWhitespace: true,
  });
  expect(sort("Minutes")).toHaveAccessibleDescription(`${SORT_HINT}; drag to move the column`);
  await user.hover(sort("Deploy"));
  expect(screen.getAllByRole("tooltip")).toHaveLength(1);
  expect(screen.getByRole("tooltip").textContent).toBe(SORT_HINT);
  await user.unhover(sort("Deploy"));
  expect(screen.queryByRole("tooltip")).toBeNull();
  act(() => sort("State").focus());
  expect(screen.getByRole("tooltip").textContent).toBe(`${SORT_HINT}; drag to move the column`);
  await user.keyboard("{Escape}");
  expect(screen.queryByRole("tooltip")).toBeNull();
  expect(sort("State")).toHaveFocus();
  // The filter trigger keeps the native title that names it.
  expect(screen.getByRole("button", { name: "Filter Service" })).toHaveAttribute(
    "title",
    "Filter Service"
  );
});

test("a column that says how its value is taken says it first in its sort tooltip, the hint under it", async () => {
  const user = userEvent.setup();
  const method = "The minutes from the first push to the rollout's end.";
  const columns = COLUMNS.map((column) =>
    column.id === "minutes" ? { ...column, meta: { describe: method } } : column
  );
  draw(<Deploys columns={columns} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  await user.hover(within(table).getByRole("button", { name: "Minutes" }));
  const lines = [...screen.getByRole("tooltip").querySelectorAll("p")];
  expect(lines.map((line) => line.textContent)).toEqual([
    method,
    `${SORT_HINT}; drag to move the column`,
  ]);
  // A column with no method has the hint alone.
  await user.hover(within(table).getByRole("button", { name: "Region" }));
  const hint = [...screen.getByRole("tooltip").querySelectorAll("p")];
  expect(hint.map((line) => line.textContent)).toEqual([`${SORT_HINT}; drag to move the column`]);
});

test("a column drag shuts the sort tooltip, and no hover opens one until the drop", async () => {
  const user = userEvent.setup();
  draw(<Deploys />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const sort = (name: string) => within(table).getByRole("button", { name });
  await user.hover(sort("Minutes"));
  expect(screen.getByRole("tooltip")).toBeDefined();
  // A header's drag start: HTML5 drag and drop, which user-event has no form for.
  fireEvent.dragStart(sort("Minutes"));
  expect(screen.queryByRole("tooltip")).toBeNull();
  await user.hover(sort("Region"));
  expect(screen.queryByRole("tooltip")).toBeNull();
  // The drop that ends that drag, which user-event has no form for either.
  fireEvent.drop(within(table).getByRole("columnheader", { name: /Region/ }));
  expect(screen.queryByRole("tooltip")).toBeNull();
  await user.unhover(sort("Region"));
  await user.hover(sort("Region"));
  expect(screen.getByRole("tooltip").textContent).toBe(`${SORT_HINT}; drag to move the column`);
});

// The caller that asked for it: the Issues tab on a phone, whose order keeps Title right after Key
// without pinning it. A column the caller's order keeps in its place is not dragged, and its hint
// names no drag, since no drag would move it; a column dropped onto it still moves before it.
test("a column the caller's order keeps in place is not dragged and names no drag, and takes a drop", async () => {
  const user = userEvent.setup();
  draw(<Deploys fixedColumns={["service"]} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const sort = (name: string) => within(table).getByRole("button", { name });
  expect(sort("Service")).toHaveAttribute("draggable", "false");
  expect(sort("Minutes")).toHaveAttribute("draggable", "true");
  await user.hover(sort("Service"));
  expect(screen.getByRole("tooltip").textContent).toBe(SORT_HINT);
  await user.unhover(sort("Service"));

  const service = within(table).getByRole("columnheader", { name: /Service/ });
  // The drag start, drag over and drop of one header onto another: HTML5 drag and drop, which
  // user-event has no form for.
  fireEvent.dragStart(sort("Minutes"));
  fireEvent.dragOver(service);
  fireEvent.drop(service);
  expect(
    within(table)
      .getAllByRole("columnheader")
      .map((header) => header.querySelector("button")?.textContent)
  ).toEqual(["Deploy", "Minutes", "Service", "State", "Region"]);
});

test("a column's own control stands in its header beside the filter, outside the sort button", async () => {
  const columns = COLUMNS.map((column) =>
    column.id === "service"
      ? {
          ...column,
          meta: {
            text: true,
            headerControl: (
              <button aria-pressed={false} type="button">
                Bars
              </button>
            ),
          },
        }
      : column
  );
  draw(<Deploys columns={columns} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const control = within(table).getByRole("button", { name: "Bars" });
  const cell: HTMLElement | null = control.closest("th");
  expect(cell).toBe(within(table).getByRole("columnheader", { name: /Service/ }));
  expect(within(table).getByRole("button", { name: "Service" })).not.toContainElement(control);
  expect(control.parentElement).toContainElement(
    within(table).getByRole("button", { name: "Filter Service" })
  );
});

// The caller that asked for it: the Delivery list, whose Title took what its box had over the
// other columns before the list was drawn here, so its rows reach the box's right edge. The table
// is at least as wide as the box, and the named column's header carries no width, which a fixed
// table gives the rest. A pinned column keeps its own width, since the pinned ones after it are
// held at the sum of theirs, and a table that names none draws every column at its own width.
test("the column the caller names takes what the box has over the other columns' widths", async () => {
  const widths = (table: HTMLElement) =>
    within(table)
      .getAllByRole("columnheader")
      .map((header) => header.style.width);
  draw(<Deploys fillColumn="service" />, 1400);

  const filled = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(filled.style.minWidth).toBe("100%");
  expect(widths(filled)).toEqual(["150px", "", "150px", "150px", "150px"]);

  for (const restore of restores.splice(0)) restore();
  draw(<Deploys fillColumn="service" layoutColumns={PINNED_LAYOUT} layoutKey="pinned" />, 1400);
  const pinned = await screen.findByRole("table", { name: "Deploys pinned" });
  expect(pinned.style.minWidth).toBe("");
  expect(widths(pinned).slice(0, 2)).toEqual(["120px", "240px"]);

  for (const restore of restores.splice(0)) restore();
  draw(<Deploys />, 1400);
  const plain = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(plain.style.minWidth).toBe("");
  expect(widths(plain)).toEqual(["150px", "150px", "150px", "150px", "150px"]);
});

// dispatch://INFRA-3221/comment/141bbad2-b75e-4bb4-b0a7-b5b1af541a3c, on every table the core
// draws: a column's minimum holds its header with its filter button, and while the table is
// sorted by the column its sort mark too. Only a sorted column grows, by the mark, and only while
// it is sorted, a second sort's column too; one whose caller names a sorted minimum that holds
// the mark already does not.
test("a sorted column at its minimum grows by the sort mark, and gives it back once the sort moves", async () => {
  const user = userEvent.setup();
  const atMinimums: LayoutColumns = {
    ids: LAYOUT_COLUMNS.ids,
    minimums: { deploy: 100, minutes: 100, region: 100, service: 150, state: 100 },
    rowAttribute: "data-deploy-row",
    sizes: { deploy: 100, minutes: 100, region: 100, service: 150, state: 100 },
    sortedMinimums: { region: 100 },
  };
  draw(<Deploys layoutColumns={atMinimums} layoutKey="marks" />, 1400);
  const table = await screen.findByRole("table", { name: "Deploys marks" });
  const widths = () =>
    within(table)
      .getAllByRole("columnheader")
      .map((header) => header.style.width);
  const sort = (name: string) =>
    within(table).getByRole("button", { name: new RegExp(`^${name}`) });
  const marked = `${100 + SORT_MARK_WIDTH}px`;
  expect(widths()).toEqual(["100px", "150px", "100px", "100px", "100px"]);

  await user.click(sort("Minutes"));
  expect(widths()).toEqual(["100px", "150px", "100px", marked, "100px"]);
  await user.click(sort("State"));
  expect(widths()).toEqual(["100px", "150px", marked, "100px", "100px"]);
  // A second sort draws its mark too.
  await user.keyboard("{Shift>}");
  await user.click(sort("Minutes"));
  await user.keyboard("{/Shift}");
  expect(widths()).toEqual(["100px", "150px", marked, marked, "100px"]);
  // Region's minimum holds its mark already, as its caller says.
  await user.click(sort("Region"));
  expect(widths()).toEqual(["100px", "150px", "100px", "100px", "100px"]);
});

// The caller that asked for it: the project List's Start next, which sits in a last cell with no
// header of its own, and the priorities page's row actions.
test("a trailing action cell is drawn after the columns, under a head cell of its width", async () => {
  draw(<Deploys trailingWidth={104} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const headers = within(table).getAllByRole("columnheader");
  expect(headers).toHaveLength(COLUMNS.length + 1);
  expect(headers.at(-1)?.textContent).toBe("");
  expect(headers.at(-1)?.style.width).toBe("104px");

  const row = table.querySelector<HTMLElement>('[data-deploy-row="d2"]');
  expect(row?.querySelectorAll("td")).toHaveLength(COLUMNS.length + 1);
  expect(
    within(row as HTMLElement).getByRole("button", { name: "Roll back d2" })
  ).toBeInTheDocument();
});

// The List's Start next goes once its issue starts next, and the row stays: the focus the press
// left on it goes to the control now at its place in the row, else the one before it, else the
// row itself, never the page's body.
test("a row's control that takes itself away hands the focus to the row", async () => {
  const user = userEvent.setup();
  draw(<Deploys trailingWidth={104} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const row = table.querySelector<HTMLElement>('[data-deploy-row="d2"]');
  if (row === null) throw new Error("no row d2 is drawn");
  act(() => within(row).getByRole("button", { name: "Roll back d2" }).focus());
  await user.keyboard("{Enter}");
  expect(within(row).queryByRole("button", { name: "Roll back d2" })).toBeNull();
  expect(row).toHaveFocus();
});

// The caller that asked for it: the project List paints a row its last action moved, and the
// priorities page a row whose rank it has just changed. A pinned cell takes the paint too, so the
// row is painted to the box's left edge rather than from the first scrolling column.
test("a painted row is painted to its left edge, pinned cells included", async () => {
  draw(<Deploys highlighted="d2" layoutColumns={PINNED_LAYOUT} />, 1400);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const cells = (id: string) => [
    ...(table.querySelector(`[data-deploy-row="${id}"]`)?.querySelectorAll("td") ?? []),
  ];
  const painted = cells("d2");
  expect(painted[0]).toHaveClass("sticky", ...selectedCardBg.split(" "));
  expect(painted[1]).toHaveClass("sticky", ...selectedCardBg.split(" "));
  // The columns that scroll are painted by the row the caller drew around them.
  expect(painted[2]).not.toHaveClass(...selectedCardBg.split(" "));
  // A row the caller is not pointing at keeps the surface under its pinned cells.
  expect(cells("d1")[0]).not.toHaveClass(...selectedCardBg.split(" "));
});

// The caller that asked for it: the project List's rows carry `data-issue-row`, the priorities
// page's carry its own word. The core reads the attribute out of `names` and writes none.
test("the rows carry the caller's own data attribute and no word of another page", async () => {
  draw(<Deploys />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  expect(table.querySelector("[data-issue-row]")).toBeNull();
  expect(table.querySelector("[data-pr-row]")).toBeNull();
  expect(listed(table)).toEqual(["d1", "d2", "d3"]);
  expect(DEPLOY_NAMES.rowAttribute).toBe("data-deploy-row");
});

// The caller that asked for it: the project List draws a group's issues by status beside its
// heading, and the priorities page a "Start all" beside a priority's. The heading itself is a
// node, since a group can stand for a row elsewhere.
test("a group's heading is the caller's node, with the caller's slot beside it", async () => {
  const user = userEvent.setup();
  draw(
    <Deploys
      groupOrder={(_column, a, b) => a.localeCompare(b)}
      groupSlot={<button type="button">Roll back the group</button>}
      grouping={["region"]}
    />
  );

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const toggle = within(table).getByRole("button", { name: "Collapse eu" });
  expect(toggle.querySelector("em")?.textContent).toBe("eu");
  const slot = within(table).getAllByRole("button", { name: "Roll back the group" })[0];
  // The slot sits beside the heading's toggle, never inside it.
  expect(toggle.contains(slot as Node)).toBe(false);
  expect(listed(table)).toEqual(["d1", "d3", "d2"]);

  await user.click(toggle);
  expect(listed(table)).toEqual(["d2"]);
});

// The caller that asked for it: the evals tab counts evals over rows, the project List issues in
// groups over rows, and each offers its own page sizes.
test("the pager counts in the caller's nouns and offers the caller's page sizes", async () => {
  const user = userEvent.setup();
  draw(
    <Deploys
      noun={{
        group: { one: "region", many: "regions" },
        line: { one: "line", many: "lines" },
        many: "deploys",
        one: "deploy",
      }}
      pager={{ pageSizes: [5, 20], testId: "deploys-pager" }}
    />
  );

  await screen.findByRole("table", { name: "Deploys deploys" });
  const pager = screen.getByTestId("deploys-pager");
  expect(within(pager).getByText("3 of 3 deploys")).toBeInTheDocument();
  expect(within(pager).getByText("Lines 1–3 of 3")).toBeInTheDocument();
  expect(optionsOf(await openList(user, "Lines per page"))).toEqual(["5 per page", "20 per page"]);
});

// The page's size reaches the controls the table draws and nothing else: the rows keep one
// height at every size.
test("a table hands its size to its pager and its header's filter, and draws its cells the same at both sizes", async () => {
  const view = draw(<Deploys size="compact" />);
  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  // Read from `md` up, where the two sizes part: each control's height, and the classes of a
  // header cell and of a body cell, with the body cell's padding.
  const read = () => {
    const css = `${SIZE_CSS}${MOUSE_CSS}`;
    const cell = within(table).getByText("billing").closest("td");
    if (cell === null) throw new Error("billing is drawn in no cell");
    return {
      cell: `${cell.className} | ${sizeUnder(cell, css).padding}`,
      controls: [
        screen.getByRole("button", { name: "Next ›" }),
        screen.getByRole("button", { name: pickNamed("Rows per page") }),
        within(table).getByRole("button", { name: "Filter Service" }),
      ].map((control) => sizeUnder(control, css).minHeight),
      header: within(table).getByRole("columnheader", { name: /^Service/ }).className,
    };
  };
  const compact = read();
  expect(compact.controls).toEqual(["32px", "32px", "32px"]);
  expect(compact.cell).toEndWith("| 8px 8px 8px 8px");
  view.rerender(<Deploys size="comfortable" />);
  const comfortable = read();
  expect(comfortable.controls).toEqual(["44px", "44px", "44px"]);
  expect(comfortable.cell).toBe(compact.cell);
  expect(comfortable.header).toBe(compact.header);
});

/** A row of the priorities page of dispatch://INFRA-2947: a priority, who owns it and how much
 *  work stands under it. Nothing here is an issue, and nothing selects. */
interface Priority {
  id: string;
  name: string;
  owner: string;
  inFlight: number;
  waiting: number;
}

/** That page's columns: no `select`, no `key`, no `title` among them. */
const PRIORITY_COLUMNS: ColumnDef<Priority>[] = [
  { accessorFn: (row) => row.name, enableColumnFilter: false, header: "Priority", id: "priority" },
  { accessorFn: (row) => row.owner, enableColumnFilter: false, header: "Owner", id: "owner" },
  {
    accessorFn: (row) => row.inFlight,
    enableColumnFilter: false,
    header: "In flight",
    id: "inFlight",
  },
  {
    accessorFn: (row) => row.waiting,
    enableColumnFilter: false,
    header: "Waiting",
    id: "waiting",
  },
];

/** The layout that page hands the core, as one object: its own column ids, the column it opens
 *  hidden, each column's starting width and the smallest it is drawn at, and no pin at all. */
const PRIORITY_LAYOUT: LayoutColumns = {
  hiddenByDefault: { waiting: false },
  ids: ["priority", "owner", "inFlight", "waiting"],
  minimums: { priority: 200 },
  rowAttribute: "data-priority-row",
  sizes: { inFlight: 90, owner: 160, priority: 140, waiting: 90 },
};

/** That page's own words: its rows, its groups, its links and the keys that walk them. */
const PRIORITY_NAMES: DataTableNames = {
  groupAttribute: "data-priority-group",
  keymap: {
    next: { id: "priorities-next", keys: "j", label: "Next priority" },
    open: { id: "priorities-open", keys: "o", label: "Open priority" },
    openOnEnter: { id: "priorities-open-enter", keys: "Enter", label: "Open the focused priority" },
    previous: { id: "priorities-previous", keys: "k", label: "Previous priority" },
    scope: "agents",
    select: { id: "priorities-select", keys: "x", label: "Select or deselect the priority" },
  },
  linkAttribute: "data-priority-link",
  rowAttribute: "data-priority-row",
};

const PRIORITIES: Priority[] = [
  { id: "p1", inFlight: 4, name: "Nothing waits on a person", owner: "sami", waiting: 2 },
  { id: "p2", inFlight: 1, name: "One table in Dispatch", owner: "ash", waiting: 0 },
];

/** The priorities page as a caller of the core, with no selection of any kind. */
function Priorities({ layoutKey = "priorities" }: { layoutKey?: string }): ReactNode {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [grouping, setGrouping] = useState<GroupingState>([]);
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([]);
  const [globalFilter, setGlobalFilter] = useState("");
  const [pagination, setPagination] = useState<PaginationState>({ pageIndex: 0, pageSize: 25 });
  const layout = useTableLayout(layoutKey, PRIORITY_LAYOUT, sorting);
  const state = { columnFilters, globalFilter, grouping, pagination, sorting };
  const model = useDataTable<Priority>({
    columns: PRIORITY_COLUMNS,
    getRowId: (row) => row.id,
    layout,
    names: PRIORITY_NAMES,
    onColumnFiltersChange: setColumnFilters,
    onGlobalFilterChange: setGlobalFilter,
    onGroupingChange: setGrouping,
    onPaginationChange: setPagination,
    onSortingChange: setSorting,
    rows: PRIORITIES,
    state,
    viewKey: JSON.stringify(state),
  });
  return (
    <section aria-label={layoutKey}>
      <p>{model.table.getSelectedRowModel().rows.length} picked</p>
      <button onClick={() => model.table.getRowModel().rows[0]?.toggleSelected(true)} type="button">
        Pick the first priority
      </button>
      <DataTable
        empty="No priority matches."
        filterCounts={NO_COUNTS}
        label={`Priorities ${layoutKey}`}
        model={model}
        noun={{
          group: { one: "group", many: "groups" },
          line: { one: "row", many: "rows" },
          many: "priorities",
          one: "priority",
        }}
        onOpenFilterChange={() => undefined}
        openFilter={undefined}
        renderGroup={() => null}
        renderRow={({ row, index, measure }) => (
          <tr data-priority-row={row.id} data-index={index} ref={measure} tabIndex={-1}>
            <DataTableCells row={row} />
          </tr>
        )}
        scrollTestId={`${layoutKey}-scroll`}
      />
    </section>
  );
}

// The caller that asked for it: the priorities page of dispatch://INFRA-2947, which hands the
// core one layout object — its own column ids, the column it opens hidden, each column's
// starting width and the smallest it is drawn at — and pins nothing.
test("the layout is the one object the caller gives, pins and all", async () => {
  draw(<Priorities />, 1400);

  const table = await screen.findByRole("table", { name: "Priorities priorities" });
  const headers = within(table).getAllByRole("columnheader");
  // Waiting opens hidden, as that page's layout says; the reader is the one who shows it.
  expect(headers.map((header) => header.querySelector("button")?.textContent)).toEqual([
    "Priority",
    "Owner",
    "In flight",
  ]);
  // Each column at the width the caller gave it, and Priority at its minimum rather than the
  // narrower starting width.
  expect(headers.map((header) => header.style.width)).toEqual(["200px", "160px", "90px"]);
  // A table whose layout pins nothing holds no column at the box's left edge.
  expect(headers.map((header) => header.style.left)).toEqual(["", "", ""]);
});

// The caller that asked for it: the project List pins Select, Priority, Key and Title on a wide
// screen and Select, Key and Title on a phone, with Title giving up width first. Every one of
// those rules is the caller's object, and the core holds no column name.
test("the pins the caller names are held on a wide screen and on a phone", async () => {
  draw(<Deploys layoutColumns={PINNED_LAYOUT} layoutKey="pinned" />, 1400);

  const wide = within(await screen.findByRole("table", { name: "Deploys pinned" })).getAllByRole(
    "columnheader"
  );
  expect(wide.slice(0, 3).map((header) => [header.style.width, header.style.left])).toEqual([
    ["120px", "0px"],
    ["240px", "120px"],
    ["100px", ""],
  ]);

  for (const restore of restores.splice(0)) restore();
  draw(<Deploys layoutColumns={PINNED_LAYOUT} layoutKey="pinned" />, 390);

  const phone = within(await screen.findByRole("table", { name: "Deploys pinned" })).getAllByRole(
    "columnheader"
  );
  // On a phone each pinned column is drawn at the width that layout names, and the flexible one
  // takes what is left of the box's share: 390 × 0.8, less the 100 the first one holds.
  expect(phone.slice(0, 3).map((header) => [header.style.width, header.style.left])).toEqual([
    ["100px", "0px"],
    ["212px", "100px"],
    ["100px", ""],
  ]);
});

// The caller that asked for it: the priorities page of dispatch://INFRA-2947, which lists one
// row per priority and acts on none of them in bulk. A table given no selection has no column
// of tick boxes and selects nothing when asked.
test("a table given no selection draws no selection column and selects nothing", async () => {
  const user = userEvent.setup();
  draw(<Priorities />);

  const table = await screen.findByRole("table", { name: "Priorities priorities" });
  expect(within(table).queryAllByRole("checkbox")).toEqual([]);
  expect(
    within(table)
      .getAllByRole("columnheader")
      .map((header) => header.textContent)
  ).not.toContain("Select");

  await user.click(screen.getByRole("button", { name: "Pick the first priority" }));
  expect(screen.getByText("0 picked")).toBeInTheDocument();
});

/** How tall the unit DOM takes every drawn line to be, which the virtualizer measures it at. */
const LINE_PX = 40;

/** The unit DOM lays nothing out: give every drawn line - a row or a group's heading, each
 *  carrying `data-index` - a height of `LINE_PX`, so a 600 px box draws the lines near it and
 *  no more. */
function linesTall(boxHeight?: number): void {
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetHeight");
  Object.defineProperty(HTMLElement.prototype, "offsetHeight", {
    configurable: true,
    get() {
      if (boxHeight !== undefined && this.dataset.testid?.endsWith("-scroll")) return boxHeight;
      return this.dataset.index === undefined ? (descriptor?.get?.call(this) ?? 0) : LINE_PX;
    },
  });
  restores.push(() => {
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "offsetHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "offsetHeight");
  });
}

/** `count` deploys, `d1` to `d<count>`, the first and every other one after it in `eu` and the
 *  rest in `us`. */
function manyDeploys(count: number): Deploy[] {
  return Array.from({ length: count }, (_, index) => ({
    id: `d${index + 1}`,
    minutes: index,
    region: index % 2 === 0 ? "eu" : "us",
    service: `service ${index + 1}`,
    state: "live",
  }));
}

/** The scroll a browser makes and reports: happy-dom scrolls no box, so the test moves it and
 *  fires the event the virtualizer reads its window from. */
function scrollBoxTo(box: HTMLElement, top: number): void {
  act(() => {
    box.scrollTop = top;
    box.dispatchEvent(new Event("scroll"));
  });
}

/** The place each drawn line of `table` says it holds among the table's rows. */
const places = (table: HTMLElement) =>
  [...table.querySelectorAll("tbody tr[data-index]")].map((line) =>
    line.getAttribute("aria-rowindex")
  );

// INFRA-3135's review round 1 (thread PRRT_kwDORFy7ds63p6zF): the table draws only the lines near
// its box, so it says how many rows it has, drawn or not, and each drawn line where it sits among
// them with the header's row first, as the Delivery list did before it was drawn here: a screen
// reader hears "row 40 of 61" rather than the drawn rows alone. Every table takes them, the
// project List and the priorities tables too. A grouped table counts its headings among its
// rows, and a paged one the lines of the page it shows, with the heading a group carried over
// from the page before.
test("the table counts every row of its page for a screen reader, drawn or not, and each drawn line says its place", async () => {
  const user = userEvent.setup();
  linesTall();
  draw(
    <>
      <Deploys layoutKey="flat" pageSize={100} pager={null} rows={manyDeploys(60)} />
      <Deploys
        groupOrder={(_column, a, b) => a.localeCompare(b)}
        grouping={["region"]}
        layoutKey="grouped"
        pageSize={10}
        rows={manyDeploys(12)}
      />
    </>
  );

  // Sixty rows, of which the box draws the first few dozen: `d<n>` is the row in place n + 1.
  const flat = await screen.findByRole("table", { name: "Deploys flat" });
  expect(flat).toHaveAttribute("aria-rowcount", "61");
  expect(flat.querySelector("thead tr")).toHaveAttribute("aria-rowindex", "1");
  const owned = () => listed(flat).map((id) => String(Number(id?.slice(1)) + 1));
  expect(listed(flat).length).toBeLessThan(60);
  expect(places(flat)).toEqual(owned());
  expect(places(flat)[0]).toBe("2");
  scrollBoxTo(screen.getByTestId("flat-scroll"), 10_000);
  // The scroll ends on the table's own timer: inside `act`, before the row reads on.
  await endWindowScroll();
  expect(listed(flat)).toContain("d60");
  expect(places(flat)).toEqual(owned());
  expect(places(flat).at(-1)).toBe("61");

  // Page one: the eu heading and its six rows, then the us heading and two of its rows. Page
  // two: the us heading carried over, and its last four rows.
  const grouped = screen.getByRole("table", { name: "Deploys grouped" });
  expect(grouped).toHaveAttribute("aria-rowcount", "11");
  expect(places(grouped)).toEqual(["2", "3", "4", "5", "6", "7", "8", "9", "10", "11"]);
  await user.click(screen.getByRole("button", { name: "Next ›" }));
  expect(grouped).toHaveAttribute("aria-rowcount", "6");
  expect(places(grouped)).toEqual(["2", "3", "4", "5", "6"]);
  expect(
    within(grouped).getByRole("button", { name: "Collapse us, continued" }).closest("tr")
  ).toHaveAttribute("aria-rowindex", "2");
});

// R11: a table that does not ask to hold a reader at the top (`holdAtTop`, the Delivery page's
// list) keeps main's rule there, and a row added above lands in view. A commit that only scrolls -
// the same rows, the virtualizer's window moved - is the reader's own move: the anchor measures
// nothing for it, so a scroll alone reads no row's place. A commit that changes the rows is
// anchored, and finds the row nearest the middle of the box.
test("a table holds no row at the top unless it asks, and a commit that only scrolls measures no place", async () => {
  linesTall();
  const view = draw(
    <Deploys layoutKey="flat" pageSize={100} pager={null} rows={manyDeploys(60)} />
  );
  const table = await screen.findByRole("table", { name: "Deploys flat" });
  const box = screen.getByTestId("flat-scroll");
  const point = spyOn(document, "elementFromPoint").mockReturnValue(null);
  restores.push(() => point.mockRestore());
  await act(async () => {
    const { promise, resolve } = Promise.withResolvers<void>();
    setTimeout(resolve, 50);
    await promise;
  });
  const arrival = (id: string): Deploy => ({
    id,
    minutes: 1,
    region: "eu",
    service: "arrival",
    state: "live",
  });
  const arrived = [arrival("d0"), ...manyDeploys(60)];
  view.rerender(<Deploys layoutKey="flat" pageSize={100} pager={null} rows={arrived} />);
  expect(listed(table)[0]).toBe("d0");
  expect(box.scrollTop).toBe(0);
  expect(point).not.toHaveBeenCalled();

  scrollBoxTo(box, 1000);
  expect(listed(table)).toContain("d40");
  expect(point).not.toHaveBeenCalled();
  // The virtualizer reads the scroll as over 150 ms after its last event (`isScrollingResetDelay`
  // in @tanstack/virtual-core) and draws again then: let that commit land here, inside `act`,
  // rather than wherever the row has got to by then, which a slow run reached before the
  // rerender below ended (seed 2 at 5a9a6b66). It is a commit that only scrolls too.
  await act(async () => {
    const { promise, resolve } = Promise.withResolvers<void>();
    setTimeout(resolve, 200);
    await promise;
  });
  expect(point).not.toHaveBeenCalled();
  view.rerender(
    <Deploys layoutKey="flat" pageSize={100} pager={null} rows={[...arrived, arrival("d61")]} />
  );
  expect(point).toHaveBeenCalled();
});

// The caller that asked for it: the Delivery list, whose every row is the 36 px it was before the
// list was drawn here. A row the box has not drawn is counted at the caller's height rather than
// guessed at, so the space under the drawn window is what the rows it stands for take, and the
// box scrolls the whole list's height from the first draw.
test("a table whose rows are all one height counts the rows it has not drawn at that height", async () => {
  linesTall();
  draw(<Deploys pageSize={100} pager={null} rowHeight={LINE_PX} rows={manyDeploys(60)} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const drawn = listed(table).length;
  expect(drawn).toBeLessThan(60);
  expect(table.querySelector<HTMLElement>("tbody > tr[aria-hidden]:last-child")?.style.height).toBe(
    `${(60 - drawn) * LINE_PX}px`
  );
});

/** A ResizeObserver the test delivers by hand: the unit DOM has none, and the virtualizer measures
 *  a drawn line again only when its observer reports the line's new size. */
class HandResizeObserver {
  static readonly made: HandResizeObserver[] = [];
  readonly targets = new Set<Element>();
  constructor(readonly callback: ResizeObserverCallback) {
    HandResizeObserver.made.push(this);
  }
  observe(target: Element, _options?: ResizeObserverOptions): void {
    this.targets.add(target);
  }
  unobserve(target: Element): void {
    this.targets.delete(target);
  }
  disconnect(): void {
    this.targets.clear();
  }
}

/** Reports `line` at `height` px to the observer that watches it, as a browser does once the
 *  line has been laid out at that height. */
function reportHeight(line: Element, height: number): void {
  const observer = HandResizeObserver.made.find((made) => made.targets.has(line));
  if (observer === undefined) throw new Error("no observer watches that line");
  const size = { blockSize: height, inlineSize: 600 };
  const entry: ResizeObserverEntry = {
    borderBoxSize: [size],
    contentBoxSize: [size],
    contentRect: new DOMRect(0, 0, 600, height),
    devicePixelContentBoxSize: [size],
    target: line,
  };
  act(() => observer.callback([entry], observer));
}

// R11 (k13, "a title above R changed"): a row the box has drawn that then changes height is a
// change the anchor answers in the commit that draws it (`ViewportAnchor`), so the virtualizer
// moves the box for it no more. The two together moved it twice: at 1440 px a title that wrapped
// a row 12 px taller left the row under the pointer 12 px above where the reader saw it. A row
// the virtualizer measures for the first time, drawn as the reader scrolls, is still put where
// its height says: no commit of the anchor's answers that one.
test("a drawn row that changes height above the box's top leaves the box to the anchor, and a row first measured there still moves it", async () => {
  linesTall();
  HandResizeObserver.made.length = 0;
  const native = Object.getOwnPropertyDescriptor(window, "ResizeObserver");
  Object.defineProperty(window, "ResizeObserver", {
    configurable: true,
    value: HandResizeObserver,
    writable: true,
  });
  restores.push(() => {
    if (native) Object.defineProperty(window, "ResizeObserver", native);
    else Reflect.deleteProperty(window, "ResizeObserver");
  });
  // The unit DOM lays nothing out, so the box would hold nothing to scroll, and a correction past
  // its end would be held rather than written: give it more height than the test scrolls.
  const scrollHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
    configurable: true,
    get() {
      return this.dataset.testid?.endsWith("-scroll")
        ? 10_000
        : (scrollHeight?.get?.call(this) ?? 0);
    },
  });
  restores.push(() => {
    if (scrollHeight) Object.defineProperty(HTMLElement.prototype, "scrollHeight", scrollHeight);
    else Reflect.deleteProperty(HTMLElement.prototype, "scrollHeight");
  });
  const point = spyOn(document, "elementFromPoint").mockReturnValue(null);
  restores.push(() => point.mockRestore());
  draw(<Deploys pageSize={100} pager={null} rowHeight={LINE_PX} rows={manyDeploys(60)} />);
  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const box = screen.getByTestId("deploys-scroll");
  const line = (index: number) => {
    const found = table.querySelector(`tbody tr[data-index="${index}"]`);
    if (found === null) throw new Error(`line ${index} is not drawn`);
    return found;
  };

  // Line 20 was drawn and measured at the first draw, and lies wholly above the box's top once
  // the box is 1000 px down. It grows by 20 px, which the anchor answers in its own commit.
  scrollBoxTo(box, 1000);
  reportHeight(line(20), LINE_PX + 20);
  expect(box.scrollTop).toBe(1000);

  // Line 45 is first drawn by the scroll to 1000 and measured for the first time once the box
  // is 2000 px down, above its top: the box moves by what it takes over its estimate.
  scrollBoxTo(box, 2000);
  reportHeight(line(45), LINE_PX + 20);
  expect(box.scrollTop).toBe(2020);
});

test("a nearest-edge hand-off waits for an undrawn row and places its measured control whole", async () => {
  linesTall(600);
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this.matches("[data-testid='deploys-scroll']")) return new DOMRect(0, 0, 390, 600);
    if (this.matches("thead, th")) return new DOMRect(0, 0, 100, 60);
    if (this.textContent === "Roll back d54" && this.matches("button")) {
      const box = this.closest("[data-testid='deploys-scroll']");
      return new DOMRect(10, 60 + 53 * LINE_PX + 10 - (box?.scrollTop ?? 0), 60, 13);
    }
    return new DOMRect();
  });
  restores.push(() => rect.mockRestore());
  draw(
    <Deploys
      focusTarget="d54"
      pageSize={100}
      pager={null}
      rowHeight={LINE_PX}
      rows={manyDeploys(60)}
      trailingWidth={104}
    />
  );
  const box = screen.getByTestId("deploys-scroll");
  box.style.overflowY = "auto";
  Object.defineProperties(box, {
    clientWidth: { value: 390 },
    scrollHeight: { value: 60 + 60 * LINE_PX },
  });
  const drawRow = spyOn(box, "scrollTo").mockImplementation(() => {});
  const correct = spyOn(box, "scrollBy").mockImplementation(
    (options?: ScrollToOptions | number, y?: number) => {
      box.scrollTop += (typeof options === "number" ? y : options?.top) ?? 0;
    }
  );
  const focus = spyOn(HTMLElement.prototype, "focus");
  restores.push(() => {
    drawRow.mockRestore();
    correct.mockRestore();
    focus.mockRestore();
  });
  expect(screen.queryByRole("button", { name: "Roll back d54" })).not.toBeInTheDocument();
  act(() => screen.getByRole("button", { name: "Return to row" }).click());
  // Before the scroll event the virtualizer still uses the old range. Its index jump aligns the
  // measured row as if there were no header, leaving the control below the box.
  expect(drawRow).toHaveBeenCalledWith(expect.objectContaining({ top: 1560 }));
  expect(screen.queryByRole("button", { name: "Roll back d54" })).not.toBeInTheDocument();
  scrollBoxTo(box, 1560);
  const target = screen.getByRole("button", { name: "Roll back d54" });
  expect(target).toHaveFocus();
  expect(focus).toHaveBeenLastCalledWith({ preventScroll: true });
  expect<readonly unknown[]>(correct.mock.calls).toEqual([[{ top: 43 }]]);
  expect(target.getBoundingClientRect().bottom).toBe(600);
  await endWindowScroll();
  expect(box.scrollTop).toBe(1603);
});
