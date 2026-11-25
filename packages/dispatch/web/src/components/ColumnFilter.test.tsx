import { afterEach, expect, test } from "bun:test";
import type {
  ColumnDef,
  ColumnFiltersState,
  GroupingState,
  PaginationState,
  SortingState,
} from "@tanstack/react-table";
import { render, screen } from "@testing-library/react";
import { type ReactNode, useState } from "react";

import { MOUSE_CSS, SIZE_CSS, sizeUnder } from "../__tests__/control-size";
import { DEPLOY_NAMES, DEPLOY_NOUN } from "../__tests__/deploy-table";
import { optionNames } from "../__tests__/list-picks";
import { ColumnFilter } from "./ColumnFilter";
import type { ControlSize } from "./control-names";
import { useDataTable } from "./DataTable";
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
  counted?: boolean;
  open?: string;
  size?: ControlSize;
}): ReactNode {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [grouping, setGrouping] = useState<GroupingState>([]);
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([]);
  const [globalFilter, setGlobalFilter] = useState("");
  const [pagination, setPagination] = useState<PaginationState>({ pageIndex: 0, pageSize: 25 });
  const layout = useTableLayout("funnels", LAYOUT_COLUMNS);
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

afterEach(() => {
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
