import { afterEach, expect, test } from "bun:test";
import type {
  ColumnDef,
  ColumnFiltersState,
  GroupingState,
  PaginationState,
  SortingState,
} from "@tanstack/react-table";
import { render } from "@testing-library/react";
import { type ComponentType, type ReactNode, useState } from "react";

import { ColumnFilter, type ColumnFilterProps } from "./ColumnFilter";
import type { ControlSize } from "./control-names";
import {
  DataTable,
  type DataTableModel,
  type DataTableNames,
  type DataTableProps,
  type RowNoun,
  useDataTable,
} from "./DataTable";
import { Pager, type PagerProps } from "./Pager";
import { SelectionBar, type SelectionBarProps } from "./SelectionBar";
import { type LayoutColumns, useTableLayout } from "./table-layout";

// INFRA-3135, round 24 (the tester's, from the architect's goal red team of 2026-11-22T11:06Z):
// spec v8, R2, "Each control takes its size from the page, comfortable or compact, as a value the
// page passes and never as a copy of the component". `SearchBox`, `PanelTrigger`, `Chip`,
// `ColumnsMenu` and `ControlRow` take `size` and draw `controlPadding[size]`; at 3fce6150
// `Pager`, `SelectionBar`, `ColumnFilter` and `DataTable` take no size, so a page cannot pass one.
// Each is given a size here through a cast, because its props have no `size` at 3fce6150, so this
// file type-checks there and each test fails on what the control draws: the classes of every
// element it draws at `compact` and at `comfortable`, which are the same where the size is not
// read.

/** Each control as a page would draw it with a size, which its props do not name yet. */
const SizedPager = Pager as unknown as ComponentType<
  PagerProps<Deploy> & { readonly size?: ControlSize }
>;
const SizedSelectionBar = SelectionBar as unknown as ComponentType<
  SelectionBarProps & { readonly size?: ControlSize }
>;
const SizedColumnFilter = ColumnFilter as unknown as ComponentType<
  ColumnFilterProps<Deploy> & { readonly size?: ControlSize }
>;
const SizedDataTable = DataTable as unknown as ComponentType<
  DataTableProps<Deploy> & { readonly size?: ControlSize }
>;

/** A row that is not an issue. */
interface Deploy {
  id: string;
  region: string;
  service: string;
}

/** A column of free text, whose filter is a text box, and a column of values, whose filter is
 *  a list of them. */
const COLUMNS: ColumnDef<Deploy>[] = [
  { accessorFn: (row) => row.id, enableColumnFilter: false, header: "Deploy", id: "deploy" },
  {
    accessorFn: (row) => row.service,
    filterFn: "includesString",
    header: "Service",
    id: "service",
    meta: { text: true },
  },
  { accessorFn: (row) => row.region, filterFn: "arrIncludesSome", header: "Region", id: "region" },
];

const LAYOUT_COLUMNS: LayoutColumns = {
  ids: ["deploy", "service", "region"],
  rowAttribute: "data-deploy-row",
};

const NAMES: DataTableNames = {
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

const NOUN: RowNoun = {
  group: { many: "groups", one: "group" },
  line: { many: "rows", one: "row" },
  many: "deploys",
  one: "deploy",
};

const DEPLOYS: Deploy[] = [
  { id: "d1", region: "eu", service: "billing" },
  { id: "d2", region: "us", service: "search" },
  { id: "d3", region: "eu", service: "ledger" },
];

const COUNTS: ReadonlyMap<string, number> = new Map([
  ["eu", 2],
  ["us", 1],
]);

/** The one table every control here is drawn over, paged two rows at a time. */
function useDeploys(): DataTableModel<Deploy> {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [grouping, setGrouping] = useState<GroupingState>([]);
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([]);
  const [globalFilter, setGlobalFilter] = useState("");
  const [pagination, setPagination] = useState<PaginationState>({ pageIndex: 0, pageSize: 2 });
  const layout = useTableLayout("sized-controls", LAYOUT_COLUMNS);
  const state = { columnFilters, globalFilter, grouping, pagination, sorting };
  return useDataTable<Deploy>({
    columns: COLUMNS,
    getRowId: (row) => row.id,
    layout,
    names: NAMES,
    onColumnFiltersChange: setColumnFilters,
    onGlobalFilterChange: setGlobalFilter,
    onGroupingChange: setGrouping,
    onPaginationChange: setPagination,
    onSortingChange: setSorting,
    rows: DEPLOYS,
    state,
    viewKey: JSON.stringify(state),
  });
}

function PagerAt({ size }: { size: ControlSize }): ReactNode {
  const model = useDeploys();
  return (
    <SizedPager
      noun={NOUN}
      pageSizes={[2, 10]}
      size={size}
      table={model.table}
      testId="deploys-pager"
    />
  );
}

function ColumnFiltersAt({ size }: { size: ControlSize }): ReactNode {
  const model = useDeploys();
  return (
    <>
      {(["service", "region"] as const).map((id) => {
        const column = model.table.getColumn(id);
        if (column === undefined) throw new Error(`the deploys table has no ${id} column`);
        return (
          <SizedColumnFilter
            column={column}
            counts={COUNTS}
            key={id}
            noun={NOUN}
            onOpenChange={() => undefined}
            open={false}
            size={size}
            unloaded={[]}
          />
        );
      })}
    </>
  );
}

function DataTableAt({ size }: { size: ControlSize }): ReactNode {
  const model = useDeploys();
  const [openFilter, setOpenFilter] = useState<string>();
  return (
    <SizedDataTable
      empty="No deploys match."
      filterCounts={COUNTS}
      label="Deploys"
      model={model}
      noun={NOUN}
      onOpenFilterChange={setOpenFilter}
      openFilter={openFilter}
      pager={{ pageSizes: [2, 10], testId: "deploys-pager" }}
      renderGroup={({ entry: { row }, index, measure }) => (
        <tr data-index={index} ref={measure}>
          <td>{String(row.groupingValue)}</td>
        </tr>
      )}
      renderRow={({ row, index, measure }) => (
        <tr data-deploy-row={row.id} data-index={index} ref={measure} tabIndex={-1}>
          <td>{row.original.service}</td>
        </tr>
      )}
      scrollTestId="deploys-scroll"
      size={size}
    />
  );
}

afterEach(() => {
  localStorage.clear();
});

/** Every element `tree` draws, in order, by its tag and its classes. Read from the render's own
 *  container, which is taken out of the document afterwards, so a second render reads none of
 *  the first's. */
function drawnClasses(tree: ReactNode): string[] {
  const view = render(tree);
  try {
    return [...view.container.querySelectorAll("*")].map(
      (element) => `${element.tagName.toLowerCase()} ${element.getAttribute("class") ?? ""}`
    );
  } finally {
    view.unmount();
    view.container.remove();
  }
}

test("R2: the Pager takes its size from the page - compact and comfortable draw different classes", () => {
  expect(drawnClasses(<PagerAt size="compact" />)).not.toEqual(
    drawnClasses(<PagerAt size="comfortable" />)
  );
});

test("R2: the SelectionBar takes its size from the page - compact and comfortable draw different classes", () => {
  const bar = (size: ControlSize) => (
    <SizedSelectionBar count={2} label="Selected rows" size={size}>
      <button type="button">Clear selection</button>
    </SizedSelectionBar>
  );
  expect(drawnClasses(bar("compact"))).not.toEqual(drawnClasses(bar("comfortable")));
});

test("R2: the ColumnFilter takes its size from the page - compact and comfortable draw different classes", () => {
  expect(drawnClasses(<ColumnFiltersAt size="compact" />)).not.toEqual(
    drawnClasses(<ColumnFiltersAt size="comfortable" />)
  );
});

test("R2: the DataTable takes its size from the page - compact and comfortable draw different classes", () => {
  expect(drawnClasses(<DataTableAt size="compact" />)).not.toEqual(
    drawnClasses(<DataTableAt size="comfortable" />)
  );
});
