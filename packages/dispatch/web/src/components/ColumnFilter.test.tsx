import { afterEach, expect, test } from "bun:test";
import type {
  ColumnDef,
  ColumnFiltersState,
  FilterFn,
  GroupingState,
  PaginationState,
  SortingState,
} from "@tanstack/react-table";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useMemo, useState } from "react";

import { MOUSE_CSS, SIZE_CSS, sizeUnder } from "../__tests__/control-size";
import { DEPLOY_NAMES, DEPLOY_NOUN } from "../__tests__/deploy-table";
import { optionNames } from "../__tests__/list-picks";
import { ColumnFilter, type ColumnFilterProps } from "./ColumnFilter";
import type { ControlSize } from "./control-names";
import {
  DataTable,
  DataTableCells,
  DataTableGroupRow,
  type DataTableNames,
  type RowNoun,
  useDataTable,
} from "./DataTable";
import { fold } from "./MultiSelect";
import { type LayoutColumns, useTableLayout } from "./table-layout";

/** A row that is not an issue. */
interface Deploy {
  id: string;
  region: string;
  service: string;
}

/** A column of free text, whose funnel opens a text box (`PanelTrigger`), and a column of
 *  values, whose funnel opens a list of them (`MultiSelect`). */
const COLUMNS: ColumnDef<Deploy>[] = [
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
  ids: ["service", "region"],
  rowAttribute: "data-deploy-row",
};

const DEPLOYS: Deploy[] = [
  { id: "d1", region: "eu", service: "billing" },
  { id: "d2", region: "us", service: "search" },
];

const COUNTS: ReadonlyMap<string, number> = new Map([
  ["eu", 1],
  ["us", 1],
]);

/** Each column's funnel as a table's header draws it, at `size` where one is given, with the
 *  column `open` names opened, and `counted` handed on where it is given. */
function Funnels({
  counted,
  open,
  size,
}: {
  counted?: ColumnFilterProps<Deploy>["counted"];
  open?: string;
  size?: ControlSize;
}): ReactNode {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [grouping, setGrouping] = useState<GroupingState>([]);
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([]);
  const [globalFilter, setGlobalFilter] = useState("");
  const [pagination, setPagination] = useState<PaginationState>({ pageIndex: 0, pageSize: 25 });
  const layout = useTableLayout("funnels", LAYOUT_COLUMNS, sorting);
  const state = { columnFilters, globalFilter, grouping, pagination, sorting };
  const { table } = useDataTable<Deploy>({
    columns: COLUMNS,
    getRowId: (row) => row.id,
    layout,
    names: DEPLOY_NAMES,
    onColumnFiltersChange: setColumnFilters,
    onGlobalFilterChange: setGlobalFilter,
    onGroupingChange: setGrouping,
    onPaginationChange: setPagination,
    onSortingChange: setSorting,
    rows: DEPLOYS,
    state,
    viewKey: JSON.stringify(state),
  });
  return table
    .getAllLeafColumns()
    .map((column) => (
      <ColumnFilter
        column={column}
        counted={counted}
        counts={COUNTS}
        key={column.id}
        noun={DEPLOY_NOUN}
        onOpenChange={() => undefined}
        open={column.id === open}
        size={size}
        unloaded={[]}
      />
    ));
}

/** A row that is not an issue, with a status the column knows and labels its rows name. */
interface Ticket {
  id: string;
  status: string;
  labels: string[];
}

const STATUSES = ["todo", "done"];
const STATUS_WORDS: Readonly<Record<string, string>> = { todo: "Todo", done: "Done" };
const TICKETS: Ticket[] = [
  { id: "t1", status: "todo", labels: ["legion"] },
  { id: "t2", status: "done", labels: ["legion", "web"] },
  { id: "t3", status: "done", labels: [] },
];

const valuesOf = (ticket: Ticket, id: string) =>
  id === "status" ? [ticket.status] : ticket.labels;
/** A pick matches a value by its `fold`, as the filter ticks it. */
const picked: FilterFn<Ticket> = (row, id, value: string[]) =>
  value.some((pick) => valuesOf(row.original, id).some((entry) => fold(entry) === fold(pick)));

const TICKET_COLUMNS: ColumnDef<Ticket>[] = [
  { accessorFn: (row) => row.id, enableColumnFilter: false, header: "Ticket", id: "ticket" },
  {
    accessorFn: (row) => row.status,
    filterFn: picked,
    header: "Status",
    id: "status",
    meta: {
      always: STATUSES,
      order: STATUSES,
      show: (value: string) => STATUS_WORDS[value] ?? value,
    },
  },
  {
    accessorFn: (row) => row.labels.join(", "),
    filterFn: picked,
    header: "Labels",
    id: "labels",
  },
];
const TICKET_LAYOUT: LayoutColumns = {
  ids: ["ticket", "status", "labels"],
  rowAttribute: "data-ticket-row",
};
const TICKET_NAMES: DataTableNames = {
  groupAttribute: "data-ticket-group",
  keymap: {
    next: { id: "tickets-next", keys: "j", label: "Next ticket" },
    open: { id: "tickets-open", keys: "o", label: "Open ticket" },
    openOnEnter: { id: "tickets-open-enter", keys: "Enter", label: "Open the focused ticket" },
    previous: { id: "tickets-previous", keys: "k", label: "Previous ticket" },
    scope: "agents",
    select: { id: "tickets-select", keys: "x", label: "Select or deselect the focused ticket" },
  },
  linkAttribute: "data-ticket-link",
  rowAttribute: "data-ticket-row",
};
const TICKET_NOUN: RowNoun = {
  group: { one: "group", many: "groups" },
  line: { one: "row", many: "rows" },
  many: "tickets",
  one: "ticket",
};
/** The address parameter each filter is read from. */
const PARAMS: Readonly<Record<string, string>> = { status: "status", labels: "label" };

/**
 * A table that reads its picks from `address` as the address spells them, with the shared
 * header filter (`ColumnFilter`) on Status and Labels. The picks it holds now are written out
 * under it, so a test reads what a press left.
 */
function Tickets({ address }: { address: string }): ReactNode {
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>(() => {
    const params = new URLSearchParams(address);
    return Object.entries(PARAMS).flatMap(([id, param]) => {
      const value = params.getAll(param);
      return value.length === 0 ? [] : [{ id, value }];
    });
  });
  const [openFilter, setOpenFilter] = useState<string>();
  const layout = useTableLayout("tickets", TICKET_LAYOUT, []);
  const model = useDataTable<Ticket>({
    columns: TICKET_COLUMNS,
    getRowId: (row) => row.id,
    layout,
    names: TICKET_NAMES,
    onColumnFiltersChange: setColumnFilters,
    onGlobalFilterChange: () => undefined,
    onGroupingChange: () => undefined,
    onPaginationChange: () => undefined,
    onSortingChange: () => undefined,
    rows: TICKETS,
    state: {
      columnFilters,
      globalFilter: "",
      grouping: [],
      pagination: { pageIndex: 0, pageSize: 25 },
      sorting: [],
    },
    viewKey: "tickets",
  });
  // What each value of the open filter's column would list.
  const counts = useMemo(() => {
    const counted = new Map<string, number>();
    if (openFilter !== undefined)
      for (const ticket of TICKETS)
        for (const value of valuesOf(ticket, openFilter))
          counted.set(value, (counted.get(value) ?? 0) + 1);
    return counted;
  }, [openFilter]);
  return (
    <>
      <DataTable
        empty="No tickets match."
        filterCounts={counts}
        label="Tickets"
        model={model}
        noun={TICKET_NOUN}
        onOpenFilterChange={setOpenFilter}
        openFilter={openFilter}
        renderGroup={({ entry: { row }, index, measure }) => (
          <DataTableGroupRow
            columns={TICKET_COLUMNS.length}
            expanded={row.getIsExpanded()}
            heading={String(row.groupingValue)}
            index={index}
            label={String(row.groupingValue)}
            measure={measure}
            onToggle={row.getToggleExpandedHandler()}
            width={300}
          />
        )}
        renderRow={({ row, index, measure }) => (
          <tr data-index={index} data-ticket-row={row.id} ref={measure} tabIndex={-1}>
            <DataTableCells row={row} />
          </tr>
        )}
        scrollTestId="tickets-scroll"
      />
      <output data-testid="picks">{JSON.stringify(columnFilters)}</output>
    </>
  );
}

const undraws: (() => void)[] = [];
afterEach(() => {
  for (const undraw of undraws.splice(0)) undraw();
  localStorage.clear();
});

test("the funnel keeps the 44 px target at every width at the comfortable size, and gives it up from md only at the compact size", () => {
  const view = render(<Funnels />);
  // What each funnel draws under `css`: a window below `md` (`SIZE_CSS` alone), or from it.
  const drawn = (css: string) =>
    [
      screen.getByRole("button", { name: "Filter Service" }),
      screen.getByRole("button", { name: "Filter Region" }),
    ].map((funnel) => sizeUnder(funnel, css));
  try {
    const target = { minHeight: "44px", minWidth: "44px", padding: "4px 4px 4px 4px" };
    expect(drawn(SIZE_CSS)).toEqual([target, target]);
    expect(drawn(`${SIZE_CSS}${MOUSE_CSS}`)).toEqual([target, target]);
    view.rerender(<Funnels size="comfortable" />);
    expect(drawn(`${SIZE_CSS}${MOUSE_CSS}`)).toEqual([target, target]);
    view.rerender(<Funnels size="compact" />);
    expect(drawn(SIZE_CSS)).toEqual([target, target]);
    const mouse = { minHeight: "32px", minWidth: "32px", padding: "4px 4px 4px 4px" };
    expect(drawn(`${SIZE_CSS}${MOUSE_CSS}`)).toEqual([mouse, mouse]);
  } finally {
    view.unmount();
  }
});

// The Delivery page's column filters, while the read for a new narrowing is out: the counts on
// screen are the previous answer's, so nothing has counted the values yet. Each value is drawn
// with no count beside it, never a 0, and the values those counts name are still offered.
test("a value filter whose counts nothing has counted yet draws no count beside a value, and the counts once they are counted", () => {
  const regions = () => optionNames(screen.getByRole("listbox", { name: "Region options" }));
  const view = render(<Funnels counted={false} open="region" />);
  try {
    expect(regions()).toEqual(["eu", "us"]);
    view.rerender(<Funnels counted open="region" />);
    expect(regions()).toEqual(["eu, 1 deploy", "us, 1 deploy"]);
  } finally {
    view.unmount();
  }
});

// A filter some of whose values count a read that has not answered yet: each of those values is
// drawn with no count beside it, never a 0, and every value whose count is counted draws it.
test("a value filter draws a count beside each value it is told is counted, and none beside the rest", () => {
  const view = render(<Funnels counted={(value) => value === "eu"} open="region" />);
  try {
    expect(optionNames(screen.getByRole("listbox", { name: "Region options" }))).toEqual([
      "eu, 1 deploy",
      "us",
    ]);
  } finally {
    view.unmount();
  }
});

// k18 Four (R14): a picked value outside a column's own options, in another case than an
// option's own, is that option: offered once, ticked, and one press unpicks it, which leaves the
// filter with no pick, as unpicking a filter's last value does.
// Each option is matched by its name in any case, so a second option of another case is found.
test.each([
  ["?status=Done", "Status", /^done,/i],
  ["?label=Legion", "Labels", /^legion,/i],
])("%s is offered once in its filter, ticked, and one press unpicks it", async (address, column, option) => {
  const user = userEvent.setup();
  const view = render(<Tickets address={address} />);
  undraws.push(() => view.unmount());
  await screen.findByRole("table", { name: "Tickets" });

  await user.click(screen.getByRole("button", { name: `Filter ${column}: 1 picked` }));
  const list = screen.getByRole("listbox", { name: `${column} options` });
  const offered = within(list).getAllByRole("option", { name: option });
  expect(offered).toHaveLength(1);
  expect(offered[0]).toHaveAttribute("aria-selected", "true");

  await user.click(offered[0] as HTMLElement);
  expect(within(list).getByRole("option", { name: option })).toHaveAttribute(
    "aria-selected",
    "false"
  );
  expect(screen.getByRole("button", { name: `Filter ${column}` })).toBeInTheDocument();
  expect(screen.getByTestId("picks").textContent).toBe("[]");
});
