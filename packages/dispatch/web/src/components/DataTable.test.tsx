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
import { selectedCardBg } from "../theme/classes";
import {
  type ColumnFilterRenderer,
  DataTable,
  DataTableCells,
  DataTableGroupRow,
  type DataTableNames,
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
  groupSlot,
  grouping: initialGrouping = [],
  groupOrder,
  highlighted,
  layoutColumns = LAYOUT_COLUMNS,
  layoutKey = "deploys",
  pageSize = 25,
  pager = DEPLOY_PAGER,
  renderFilter,
  rows = DEPLOYS,
  stackColumn,
  trailingWidth,
}: {
  columns?: ColumnDef<Deploy>[];
  /** What this caller draws beside a group's heading; with it the headings are the core's
   *  `DataTableGroupRow` rather than a line of this test's own. */
  groupSlot?: ReactNode;
  grouping?: GroupingState;
  groupOrder?: (columnId: string, a: string, b: string) => number;
  /** The deploy whose row this caller is pointing at, painted in its own class. */
  highlighted?: string;
  layoutColumns?: LayoutColumns;
  layoutKey?: string;
  pageSize?: number;
  /** The line under the table; `null` draws a table with no pager at all. */
  pager?: PagerOptions | null;
  renderFilter?: ColumnFilterRenderer<Deploy>;
  rows?: Deploy[];
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
        renderFilter={renderFilter}
        renderGroup={({ entry, index, measure }) =>
          groupSlot === undefined ? (
            <tr data-group={String(entry.row.groupingValue)} data-index={index} ref={measure}>
              <td>{String(entry.row.groupingValue)}</td>
            </tr>
          ) : (
            <DataTableGroupRow
              columns={model.table.getVisibleLeafColumns().length}
              expanded={entry.row.getIsExpanded()}
              heading={<em>{String(entry.row.groupingValue)}</em>}
              index={index}
              label={`${entry.row.getIsExpanded() ? "Collapse" : "Expand"} ${String(entry.row.groupingValue)}`}
              measure={measure}
              onToggle={entry.row.getToggleExpandedHandler()}
              slot={groupSlot}
              width={400}
            />
          )
        }
        renderRow={({ row, index, measure }) => (
          <tr data-deploy-row={row.id} data-index={index} ref={measure} tabIndex={-1}>
            <DataTableCells
              highlight={row.id === highlighted ? selectedCardBg : undefined}
              row={row}
              trailing={
                trailingWidth === undefined ? undefined : (
                  <button type="button">Roll back {row.id}</button>
                )
              }
            />
          </tr>
        )}
        scrollTestId={`${layoutKey}-scroll`}
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
  draw(
    <Deploys
      pager={{
        nouns: {
          group: { one: "region", many: "regions" },
          item: { one: "deploy", many: "deploys" },
          row: { one: "line", many: "lines" },
        },
        pageSizes: [5, 20],
        testId: "deploys-pager",
      }}
    />
  );

  await screen.findByRole("table", { name: "Deploys deploys" });
  const pager = screen.getByTestId("deploys-pager");
  expect(within(pager).getByText("3 of 3 deploys")).toBeInTheDocument();
  expect(within(pager).getByText("Lines 1–3 of 3")).toBeInTheDocument();
  const sizes = within(pager).getByRole("combobox", { name: "Lines per page" });
  expect([...sizes.querySelectorAll("option")].map((option) => option.textContent)).toEqual([
    "5 per page",
    "20 per page",
  ]);
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
  filterCountNoun: { one: "issue", many: "issues" },
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
