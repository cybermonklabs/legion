import { afterEach, expect, test } from "bun:test";
import type { ColumnDef } from "@tanstack/react-table";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import {
  type TableAddressGrammar,
  tableAddressGrammar,
  useTableAddress,
} from "../lib/page-address";
import { ColumnsMenu } from "./ColumnsMenu";
import { DataTable, DataTableCells, type DataTableNames, useDataTable } from "./DataTable";
import type { PagerOptions } from "./Pager";
import { type LayoutColumns, useTableLayout } from "./table-layout";

/**
 * Two of the one table drawn together, each with its own address prefix, its own stored layout
 * key and its own columns — the case the priorities page of dispatch://INFRA-2947 meets, where
 * the two fault lists stand above the priorities themselves. The address and the stored layout
 * are this test's own: it reads what each table wrote and draws both again from it.
 */

/** A line of either table: what it is, where it went and how long it took. */
interface Line {
  id: string;
  what: string;
  where: string;
  minutes: number;
}

const DEPLOY_COLUMNS: ColumnDef<Line>[] = [
  { accessorFn: (row) => row.id, enableColumnFilter: false, header: "Deploy", id: "deploy" },
  {
    accessorFn: (row) => row.what,
    filterFn: "includesString",
    header: "Service",
    id: "service",
    meta: { text: true },
  },
  {
    accessorFn: (row) => row.minutes,
    enableColumnFilter: false,
    header: "Minutes",
    id: "minutes",
    sortDescFirst: false,
  },
];

const RELEASE_COLUMNS: ColumnDef<Line>[] = [
  { accessorFn: (row) => row.id, enableColumnFilter: false, header: "Release", id: "release" },
  { accessorFn: (row) => row.where, enableColumnFilter: false, header: "Channel", id: "channel" },
];

const DEPLOY_LAYOUT: LayoutColumns = {
  ids: ["deploy", "service", "minutes"],
  rowAttribute: "data-deploy-row",
};

const RELEASE_LAYOUT: LayoutColumns = {
  ids: ["release", "channel"],
  rowAttribute: "data-release-row",
};

const DEPLOY_NAMES: DataTableNames = {
  filterCountNoun: { one: "deploy", many: "deploys" },
  groupAttribute: "data-deploy-group",
  keymap: {
    next: { id: "deploys-next", keys: "j", label: "Next deploy" },
    open: { id: "deploys-open", keys: "o", label: "Open deploy" },
    openOnEnter: { id: "deploys-open-enter", keys: "Enter", label: "Open the focused deploy" },
    previous: { id: "deploys-previous", keys: "k", label: "Previous deploy" },
    scope: "agents",
    select: { id: "deploys-select", keys: "x", label: "Select or deselect the deploy" },
  },
  linkAttribute: "data-deploy-link",
  rowAttribute: "data-deploy-row",
};

const RELEASE_NAMES: DataTableNames = {
  filterCountNoun: { one: "release", many: "releases" },
  groupAttribute: "data-release-group",
  keymap: {
    next: { id: "releases-next", keys: "ArrowDown", label: "Next release" },
    open: { id: "releases-open", keys: "o", label: "Open release" },
    openOnEnter: { id: "releases-open-enter", keys: "Enter", label: "Open the focused release" },
    previous: { id: "releases-previous", keys: "ArrowUp", label: "Previous release" },
    scope: "agents",
    select: { id: "releases-select", keys: "x", label: "Select or deselect the release" },
  },
  linkAttribute: "data-release-link",
  rowAttribute: "data-release-row",
};

/** Each table's own grammar, under its own prefix: neither reads a parameter of the other. A
 *  module-level constant, since `useTableAddress` memoizes the view it reads on its identity. */
const DEPLOY_ADDRESS: TableAddressGrammar = tableAddressGrammar({ pageSize: 10, prefix: "d" });
const RELEASE_ADDRESS: TableAddressGrammar = tableAddressGrammar({ pageSize: 10, prefix: "r" });

const DEPLOYS: Line[] = [
  ...Array.from({ length: 12 }, (_, index) => ({
    id: `d${index + 1}`,
    minutes: 12 - index,
    what: `billing ${index + 1}`,
    where: "eu",
  })),
  ...Array.from({ length: 3 }, (_, index) => ({
    id: `l${index + 1}`,
    minutes: index,
    what: `ledger ${index + 1}`,
    where: "us",
  })),
];

const RELEASES: Line[] = [
  { id: "r1", minutes: 3, what: "web", where: "stable" },
  { id: "r2", minutes: 4, what: "api", where: "beta" },
  { id: "r3", minutes: 5, what: "worker", where: "stable" },
];

const NO_COUNTS: ReadonlyMap<string, number> = new Map();

/** One table of the page, reading and writing its view under its own prefix and keeping its own
 *  widths, order and visibility under its own whole key. */
function PrefixedTable({
  columns,
  grammar,
  label,
  layoutColumns,
  layoutKey,
  names,
  pager,
  rows,
}: {
  columns: ColumnDef<Line>[];
  grammar: TableAddressGrammar;
  label: string;
  layoutColumns: LayoutColumns;
  layoutKey: string;
  names: DataTableNames;
  pager: PagerOptions;
  rows: Line[];
}): ReactNode {
  const { params, view, setFilters, setGrouping, setPagination, setSearch, setSorting } =
    useTableAddress(grammar);
  const layout = useTableLayout(layoutKey, layoutColumns);
  const [openFilter, setOpenFilter] = useState<string>();
  const model = useDataTable<Line>({
    columns,
    getRowId: (row) => row.id,
    layout,
    names,
    onColumnFiltersChange: setFilters,
    onGlobalFilterChange: setSearch,
    onGroupingChange: setGrouping,
    onPaginationChange: setPagination,
    onSortingChange: setSorting,
    rows,
    state: {
      columnFilters: view.filters,
      globalFilter: view.search,
      grouping: view.grouping,
      pagination: { pageIndex: view.page, pageSize: view.pageSize },
      sorting: view.sorting,
    },
    viewKey: params.toString(),
  });
  return (
    <section aria-label={label}>
      <ColumnsMenu columns={model.table.getAllLeafColumns()} />
      <DataTable
        empty={`No ${label} match.`}
        filterCounts={NO_COUNTS}
        label={label}
        model={model}
        onOpenFilterChange={setOpenFilter}
        openFilter={openFilter}
        pager={pager}
        renderGroup={() => null}
        renderRow={({ row, index, measure }) => (
          <tr data-index={index} ref={measure} tabIndex={-1} {...{ [names.rowAttribute]: row.id }}>
            <DataTableCells row={row} />
          </tr>
        )}
        scrollTestId={`${layoutKey}-scroll`}
      />
    </section>
  );
}

/** The page the two tables sit on, with the address it is at drawn where the test can read it. */
function TwoTables(): ReactNode {
  const location = useLocation();
  return (
    <>
      <p data-testid="address">{location.search}</p>
      <PrefixedTable
        columns={DEPLOY_COLUMNS}
        grammar={DEPLOY_ADDRESS}
        label="deploys"
        layoutColumns={DEPLOY_LAYOUT}
        layoutKey="two.deploys"
        names={DEPLOY_NAMES}
        pager={{
          nouns: {
            group: { one: "region", many: "regions" },
            item: { one: "deploy", many: "deploys" },
            row: { one: "row", many: "rows" },
          },
          pageSizes: [10, 25],
          testId: "deploys-pager",
        }}
        rows={DEPLOYS}
      />
      <PrefixedTable
        columns={RELEASE_COLUMNS}
        grammar={RELEASE_ADDRESS}
        label="releases"
        layoutColumns={RELEASE_LAYOUT}
        layoutKey="two.releases"
        names={RELEASE_NAMES}
        pager={{
          nouns: {
            group: { one: "channel", many: "channels" },
            item: { one: "release", many: "releases" },
            row: { one: "row", many: "rows" },
          },
          pageSizes: [10, 25],
          testId: "releases-pager",
        }}
        rows={RELEASES}
      />
    </>
  );
}

const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});

/** The unit DOM lays nothing out: give each table's box the height a window would. */
function draw(entry: string) {
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid?.endsWith("-scroll") ? 600 : (descriptor?.get?.call(this) ?? 0);
    },
  });
  const view = render(
    <MemoryRouter initialEntries={[entry]}>
      <TwoTables />
    </MemoryRouter>
  );
  restores.push(() => {
    view.unmount();
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
  });
  return view;
}

const address = () => screen.getByTestId("address").textContent ?? "";

const listed = (label: string, attribute: string) =>
  [...screen.getByRole("region", { name: label }).querySelectorAll(`[${attribute}]`)].map((row) =>
    row.getAttribute(attribute)
  );

const headings = (label: string) =>
  within(screen.getByRole("table", { name: label }))
    .getAllByRole("columnheader")
    .map((header) => header.querySelector("button")?.textContent);

test("sorting, narrowing, paging and hiding a column on one table leave the other as it was", async () => {
  const user = userEvent.setup();
  draw("/page");

  const deploys = await screen.findByRole("table", { name: "deploys" });
  const releases = screen.getByRole("table", { name: "releases" });
  const before = listed("releases", "data-release-row");

  await user.click(within(deploys).getByRole("button", { name: "Minutes" }));
  await user.click(within(deploys).getByRole("button", { name: "Filter Service" }));
  await user.type(screen.getByRole("textbox", { name: "Filter Service" }), "billing");
  await user.keyboard("{Escape}");
  await user.click(
    within(screen.getByTestId("deploys-pager")).getByRole("button", { name: "Next ›" })
  );
  await user.click(
    within(screen.getByRole("region", { name: "deploys" })).getByRole("button", { name: "Columns" })
  );
  await user.click(screen.getByRole("checkbox", { name: "Minutes" }));
  await user.keyboard("{Escape}");

  // Every one of the four is written under this table's own prefix, and nothing of the other's.
  expect(address()).toContain("d.sort=minutes");
  expect(address()).toContain("d.f.service=billing");
  expect(address()).toContain("d.page=2");
  expect(address()).not.toContain("r.");

  // The deploys table is sorted, narrowed, on its second page and short of a column.
  expect(headings("deploys")).toEqual(["Deploy", "Service"]);
  expect(listed("deploys", "data-deploy-row")).toEqual(["d2", "d1"]);

  // The releases table is exactly as it was drawn.
  expect(headings("releases")).toEqual(["Release", "Channel"]);
  expect(listed("releases", "data-release-row")).toEqual(before);
  expect(within(releases).getAllByText("stable")).toHaveLength(2);
  expect(screen.getByText("3 of 3 releases")).toBeInTheDocument();

  // Each table's stored layout is under its own whole key: the hidden column is the deploys
  // table's alone.
  expect(JSON.parse(localStorage.getItem("two.deploys") ?? "{}")).toEqual({
    order: [],
    sizing: {},
    visibility: { minutes: false },
  });
  expect(JSON.parse(localStorage.getItem("two.releases") ?? "{}")).toEqual({
    order: [],
    sizing: {},
    visibility: {},
  });
});

test("both tables are drawn again from the address and the layout they wrote", async () => {
  localStorage.setItem(
    "two.deploys",
    JSON.stringify({ order: [], sizing: {}, visibility: { minutes: false } })
  );
  draw("/page?d.sort=minutes&d.f.service=billing&d.page=2");

  await screen.findByRole("table", { name: "deploys" });
  // The deploys table comes back sorted, narrowed, on its second page, with Minutes still hidden.
  expect(headings("deploys")).toEqual(["Deploy", "Service"]);
  expect(listed("deploys", "data-deploy-row")).toEqual(["d2", "d1"]);
  expect(screen.getByText("12 of 15 deploys")).toBeInTheDocument();

  // The releases table comes back as it opens: nothing of the other table's address reached it.
  expect(headings("releases")).toEqual(["Release", "Channel"]);
  expect(listed("releases", "data-release-row")).toEqual(["r1", "r2", "r3"]);
  expect(screen.getByText("3 of 3 releases")).toBeInTheDocument();
});
