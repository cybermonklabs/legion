import { afterEach, expect, spyOn, test } from "bun:test";
import type {
  ColumnDef,
  ColumnFiltersState,
  GroupingState,
  PaginationState,
  SortingState,
} from "@tanstack/react-table";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MOUSE_CSS, SIZE_CSS, sizeUnder } from "../__tests__/control-size";
import { DEPLOY_NAMES, DEPLOY_NOUN } from "../__tests__/deploy-table";
import { openList, optionNames, rows as optionsOf, pickNamed } from "../__tests__/list-picks";
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
  size,
  stackColumn,
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
  /** The size the page draws its controls at, which the table hands its own. */
  size?: ControlSize;
  stackColumn?: string;
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
    names,
    onColumnFiltersChange: setColumnFilters,
    onGlobalFilterChange: setGlobalFilter,
    onGroupingChange: setGrouping,
    onPaginationChange: setPagination,
    onSortingChange: setSorting,
    rowHeight,
    rows,
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
        renderGroup={({ entry: { row }, index, measure }) => (
          <DataTableGroupRow
            columns={model.table.getVisibleLeafColumns().length}
            expanded={row.getIsExpanded()}
            heading={<em>{String(row.groupingValue)}</em>}
            index={index}
            label={`${row.getIsExpanded() ? "Collapse" : "Expand"} ${String(row.groupingValue)}`}
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
        stackColumn={stackColumn}
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

// Firefox makes a box that scrolls a Tab stop of its own, which here would read as nothing: the
// header's controls and the rows are the stops, so the box is out of the Tab order.
test("the box that scrolls is no Tab stop of its own", async () => {
  draw(<Deploys />);

  await screen.findByRole("table", { name: "Deploys deploys" });
  expect(screen.getByTestId("deploys-scroll")).toHaveAttribute("tabindex", "-1");
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

/** The deploys with bounds on one column's width: Minutes is drawn at TanStack's 150 px until a
 *  reader changes it, never narrower than this caller's 120 px, nor wider than its own 200 px. */
const BOUNDED_COLUMNS: ColumnDef<Deploy>[] = COLUMNS.map((column) =>
  column.id === "minutes" ? { ...column, maxSize: 200 } : column
);
const BOUNDED_LAYOUT: LayoutColumns = { ...LAYOUT_COLUMNS, minimums: { minutes: 120 } };

// R12 of dispatch://INFRA-2160 and the architect's ruling on it: a column's width changes by keys
// on its grip as well as by a drag. The grip is a Tab stop, a separator whose value is the width
// and whose bounds are the ones the drawn width obeys; ArrowRight and ArrowLeft step the width,
// Shift takes a larger step, and Enter gives the column back its first width, as a double-click
// does. The width is the one the layout stores, under the table's key.
test("a column's grip takes the focus, and its keys change, bound and reset the column's width", async () => {
  const user = userEvent.setup();
  draw(<Deploys columns={BOUNDED_COLUMNS} layoutColumns={BOUNDED_LAYOUT} />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const header = within(table).getByRole("columnheader", { name: /Minutes/ });
  const grip = within(table).getByRole("separator", { name: "Resize Minutes" });
  expect(grip).toHaveAttribute("tabindex", "0");
  expect(grip).toHaveAttribute("aria-orientation", "vertical");
  expect([grip.getAttribute("aria-valuemin"), grip.getAttribute("aria-valuemax")]).toEqual([
    "120",
    "200",
  ]);
  const width = () => [header.style.width, grip.getAttribute("aria-valuenow")];
  const stored = () => JSON.parse(localStorage.getItem("deploys") ?? "{}").sizing;
  expect(width()).toEqual(["150px", "150"]);

  act(() => grip.focus());
  await user.keyboard("{ArrowRight}");
  expect(width(), "ArrowRight widens the column by a step").toEqual(["160px", "160"]);
  await user.keyboard("{Shift>}{ArrowRight}{/Shift}");
  expect(width(), "Shift+ArrowRight stops at the column's widest").toEqual(["200px", "200"]);
  await user.keyboard("{Shift>}{ArrowLeft}{/Shift}");
  expect(width(), "Shift+ArrowLeft narrows the column by a larger step").toEqual(["150px", "150"]);
  await user.keyboard("{Shift>}{ArrowLeft}{/Shift}{ArrowLeft}");
  expect(width(), "the arrows stop at the caller's narrowest").toEqual(["120px", "120"]);
  expect(stored()).toEqual({ minutes: 120 });

  await user.keyboard("{Enter}");
  expect(width(), "Enter gives the column back its first width").toEqual(["150px", "150"]);
  expect(stored()).toEqual({});
  expect(grip, "the grip keeps the focus through its keys").toHaveFocus();
});

/** The deploys with a colour dot before them, as the Delivery list draws one: a column that does
 *  not sort or filter, drawn at 20 px, under the 36 px the stored layout keeps
 *  (`STORED_WIDTH.min`), headed by `header`. */
const dotColumns = (header: string): ColumnDef<Deploy>[] => [
  {
    accessorFn: (row) => row.region,
    enableColumnFilter: false,
    enableSorting: false,
    header,
    id: "color",
    minSize: 20,
    size: 20,
  },
  ...COLUMNS,
];
const DOT_LAYOUT: LayoutColumns = {
  ...LAYOUT_COLUMNS,
  ids: ["color", ...LAYOUT_COLUMNS.ids],
  minimums: { color: 20 },
};

// INFRA-2160's review round 1 (thread PRRT_kwDORFy7ds66U3XP): a column drawn narrower than the
// grip's narrowest - the Delivery list's colour dot, at 20 px under the 36 px the stored layout
// keeps - keeps its width on ArrowLeft rather than grow to that bound, and the range its grip
// reports holds the width it is drawn at. ArrowRight still sets only a width the stored layout
// keeps.
test("a column drawn under its grip's narrowest keeps its width on ArrowLeft, and its grip's range holds that width", async () => {
  const user = userEvent.setup();
  draw(<Deploys columns={dotColumns("Dot")} layoutColumns={DOT_LAYOUT} layoutKey="dots" />);

  const table = await screen.findByRole("table", { name: "Deploys dots" });
  const grip = within(table).getByRole("separator", { name: "Resize Dot" });
  const header = grip.closest("th");
  const width = () => [header?.style.width, grip.getAttribute("aria-valuenow")];
  const range = () => [grip.getAttribute("aria-valuemin"), grip.getAttribute("aria-valuemax")];
  const stored = () => JSON.parse(localStorage.getItem("dots") ?? "{}").sizing;
  expect(width()).toEqual(["20px", "20"]);
  expect(range(), "the grip's range holds the 20 px the column is drawn at").toEqual([
    "20",
    "2000",
  ]);

  act(() => grip.focus());
  await user.keyboard("{ArrowLeft}");
  expect(width(), "ArrowLeft leaves the column at its 20 px").toEqual(["20px", "20"]);
  expect(stored(), "ArrowLeft writes no width").toEqual({});
  await user.keyboard("{ArrowRight}");
  expect(width(), "ArrowRight sets the narrowest width the stored layout keeps").toEqual([
    "36px",
    "36",
  ]);
  expect(range()).toEqual(["36", "2000"]);
  await user.keyboard("{ArrowLeft}");
  expect(width(), "ArrowLeft at that narrowest keeps the column there").toEqual(["36px", "36"]);
  expect(stored()).toEqual({ color: 36 });

  await user.keyboard("{Enter}");
  expect(width(), "Enter gives the column back its first width").toEqual(["20px", "20"]);
  expect(stored()).toEqual({});
});

// INFRA-2160's review round 1 (thread PRRT_kwDORFy7ds66U3XR): a column whose header draws no
// text, as the Delivery list's colour dot, names its grip and its move menu by its id, so
// neither Tab stop is heard as a bare "Resize" or "Move".
test("a column whose header draws no text names its grip and its move menu by its id", async () => {
  draw(<Deploys columns={dotColumns("")} layoutColumns={DOT_LAYOUT} layoutKey="dots" />);

  const table = await screen.findByRole("table", { name: "Deploys dots" });
  expect(within(table).getByRole("separator", { name: "Resize color" })).toBeInTheDocument();
  expect(within(table).getByRole("button", { name: "Move color" })).toBeInTheDocument();
});

// INFRA-2160's review round 1 (thread PRRT_kwDORFy7ds66U3XJ): the move menu's trigger is drawn
// out of the header's flow, and keeps its place among the header's Tab stops: after the sort,
// before the filter and the grip.
test("a header's Tab stops run from its sort to its move menu, its filter and its grip", async () => {
  const user = userEvent.setup();
  draw(<Deploys />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  act(() => within(table).getByRole("button", { name: "Service" }).focus());
  for (const stop of [
    within(table).getByRole("button", { name: "Move Service" }),
    within(table).getByRole("button", { name: "Filter Service" }),
    within(table).getByRole("separator", { name: "Resize Service" }),
  ]) {
    await user.tab();
    expect(stop).toHaveFocus();
  }
});

// R12: a column moves by its header's menu as well as by a drag. Move left and Move right are
// offered only where a column that is not pinned stands on that side; a move writes the order a
// drop writes, under the table's key; and the menu leaves the focus on its trigger in the moved
// column's header.
test("a column's move menu moves it left or right, offering only the moves the column can make", async () => {
  const user = userEvent.setup();
  draw(<Deploys />);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const headings = () =>
    within(table)
      .getAllByRole("columnheader")
      .map((header) => header.querySelector("button")?.textContent);
  const offered = async (name: string) => {
    await user.click(within(table).getByRole("button", { name: `Move ${name}` }));
    const choices = within(screen.getByRole("menu", { name: `Move ${name}` }))
      .getAllByRole("menuitem")
      .map((choice) => choice.textContent);
    await user.keyboard("{Escape}");
    return choices;
  };
  expect(await offered("Deploy")).toEqual(["Move right"]);
  expect(await offered("State")).toEqual(["Move left", "Move right"]);
  expect(await offered("Region")).toEqual(["Move left"]);

  const trigger = () => within(table).getByRole("button", { name: "Move State" });
  act(() => trigger().focus());
  await user.keyboard("{Enter}{Enter}");
  expect(headings(), "Move left").toEqual(["Deploy", "State", "Service", "Minutes", "Region"]);
  expect(trigger(), "the focus stays on the moved column's menu").toHaveFocus();
  await user.keyboard("{Enter}{ArrowDown}{Enter}");
  await user.keyboard("{Enter}{ArrowDown}{Enter}");
  expect(headings(), "Move right, twice").toEqual([
    "Deploy",
    "Service",
    "Minutes",
    "State",
    "Region",
  ]);
  expect(trigger(), "the focus stays on the moved column's menu").toHaveFocus();
  expect(JSON.parse(localStorage.getItem("deploys") ?? "{}").order).toEqual([
    "deploy",
    "service",
    "minutes",
    "state",
    "region",
  ]);
});

// A pinned column has no drag, and no drop takes a column past one: it has no move menu, and the
// column beside it is offered no move onto it.
test("a pinned column has no move menu, and no move passes one", async () => {
  draw(<Deploys layoutColumns={PINNED_LAYOUT} layoutKey="pinned" />, 1400);

  const table = await screen.findByRole("table", { name: "Deploys pinned" });
  expect(within(table).queryByRole("button", { name: "Move Deploy" })).toBeNull();
  expect(within(table).queryByRole("button", { name: "Move Service" })).toBeNull();
  await userEvent.setup().click(within(table).getByRole("button", { name: "Move State" }));
  expect(
    within(screen.getByRole("menu", { name: "Move State" }))
      .getAllByRole("menuitem")
      .map((choice) => choice.textContent)
  ).toEqual(["Move right"]);
});

// The page's keymap reads a key the grip and the menu do not take as it reads it on the header's
// sort: `j` from either walks the rows of the table it is in.
test("the grip and the move menu leave j to the rows, as the header's sort does", async () => {
  const user = userEvent.setup();
  draw(
    <KeymapProvider>
      <Scope />
      <Deploys />
    </KeymapProvider>
  );

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const [first] = table.querySelectorAll<HTMLElement>("[data-deploy-row]");
  for (const control of [
    within(table).getByRole("separator", { name: "Resize Minutes" }),
    within(table).getByRole("button", { name: "Move Minutes" }),
  ]) {
    act(() => control.focus());
    await user.keyboard("j");
    expect(first, `j from ${control.getAttribute("aria-label")}`).toHaveFocus();
  }
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
        stackColumn="title"
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

// The caller that asked for it: the project List stacks `title`, and the priorities page stacks
// a column of its own. The core holds no column name of any page.
test("the column the caller names stacks its controls in a box under 640 px", async () => {
  draw(<Deploys stackColumn="service" />, 390);

  const table = await screen.findByRole("table", { name: "Deploys deploys" });
  const controls = (name: RegExp) =>
    within(table).getByRole("columnheader", { name }).querySelector("span")?.className ?? "";
  expect(controls(/Service/)).toContain("flex-col");
  expect(controls(/Minutes/)).not.toContain("flex-col");
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

// The caller that asked for it: the project List draws an epic's actions beside its heading, and
// the priorities page a "Start all" beside a priority's. The heading itself is a node, since a
// group can stand for a row elsewhere.
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

/** That page's own words: its rows, its groups, its links and the actions that walk them. */
const PRIORITY_NAMES: DataTableNames = {
  groupAttribute: "data-priority-group",
  keymap: {
    next: "project.next",
    open: "project.open",
    openOnEnter: "project.open-enter",
    previous: "project.previous",
    scope: "agents",
    select: "project.select",
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
  const layout = useTableLayout(layoutKey, PRIORITY_LAYOUT);
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
    within(grouped).getByRole("button", { name: "Collapse us" }).closest("tr")
  ).toHaveAttribute("aria-rowindex", "2");
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
