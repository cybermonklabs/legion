import { afterEach, expect, mock, test } from "bun:test";
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

import { MOUSE_CSS, SIZE_CSS, sizeUnder } from "../__tests__/control-size";
import { DEPLOY_NAMES, DEPLOY_NOUN } from "../__tests__/deploy-table";
import { openList, pickNamed, rows } from "../__tests__/list-picks";
import type { ControlSize } from "./control-names";
import { useDataTable } from "./DataTable";
import { Pager } from "./Pager";
import { type LayoutColumns, useTableLayout } from "./table-layout";

/** A row that is not an issue: the pager counts whatever its table holds. */
interface Deploy {
  id: string;
  service: string;
}

const COLUMNS: ColumnDef<Deploy>[] = [
  { accessorFn: (row) => row.id, header: "Deploy", id: "deploy" },
  { accessorFn: (row) => row.service, header: "Service", id: "service" },
];

const LAYOUT_COLUMNS: LayoutColumns = {
  ids: ["deploy", "service"],
  rowAttribute: "data-deploy-row",
};

/** The page size that means every row, as the project List's is. */
const ALL_ROWS = Number.MAX_SAFE_INTEGER;

/** Twelve deploys: three pages of five, two of ten, or one of every row. */
const DEPLOYS: Deploy[] = Array.from({ length: 12 }, (_, index) => ({
  id: `d${index + 1}`,
  service: `service ${index + 1}`,
}));

/** The pager under a table of `DEPLOYS` paged five at a time, drawn at `size` where one is
 *  given. `onPaged` hears each change the table makes to its pages. */
function PagedDeploys({ onPaged, size }: { onPaged?: () => void; size?: ControlSize }): ReactNode {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [grouping, setGrouping] = useState<GroupingState>([]);
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([]);
  const [globalFilter, setGlobalFilter] = useState("");
  const [pagination, setPagination] = useState<PaginationState>({ pageIndex: 0, pageSize: 5 });
  const layout = useTableLayout("paged-deploys", LAYOUT_COLUMNS, sorting);
  const state = { columnFilters, globalFilter, grouping, pagination, sorting };
  const model = useDataTable<Deploy>({
    columns: COLUMNS,
    getRowId: (row) => row.id,
    layout,
    names: DEPLOY_NAMES,
    onColumnFiltersChange: setColumnFilters,
    onGlobalFilterChange: setGlobalFilter,
    onGroupingChange: setGrouping,
    onPaginationChange: (update) => {
      onPaged?.();
      setPagination(update);
    },
    onSortingChange: setSorting,
    rows: DEPLOYS,
    state,
    viewKey: JSON.stringify(state),
  });
  return (
    <Pager
      allRows={ALL_ROWS}
      noun={DEPLOY_NOUN}
      pageSizes={[5, 10, ALL_ROWS]}
      size={size}
      table={model.table}
      testId="deploys-pager"
    />
  );
}

afterEach(() => {
  localStorage.clear();
});

test("the pager's steps and its Rows per page take the page's size: the comfortable padding and 44 px by default, the compact padding with 44 px given up from md", () => {
  const view = render(<PagedDeploys />);
  // What each of the pager's controls draws under `css`: a window below `md` (`SIZE_CSS`
  // alone), or from it.
  const drawn = (css: string) =>
    [
      screen.getByRole("button", { name: "‹ Previous" }),
      screen.getByRole("button", { name: "Next ›" }),
      screen.getByRole("button", { name: pickNamed("Rows per page") }),
    ].map((control) => sizeUnder(control, css));
  try {
    const comfortable = { minHeight: "44px", padding: "8px 12px 8px 12px" };
    expect(drawn(SIZE_CSS)).toMatchObject([comfortable, comfortable, comfortable]);
    expect(drawn(`${SIZE_CSS}${MOUSE_CSS}`)).toMatchObject([comfortable, comfortable, comfortable]);
    view.rerender(<PagedDeploys size="compact" />);
    const finger = { minHeight: "44px", padding: "4px 8px 4px 8px" };
    expect(drawn(SIZE_CSS)).toMatchObject([finger, finger, finger]);
    const mouse = { minHeight: "32px", padding: "4px 8px 4px 8px" };
    expect(drawn(`${SIZE_CSS}${MOUSE_CSS}`)).toMatchObject([mouse, mouse, mouse]);
  } finally {
    view.unmount();
  }
});

test("Rows per page opens a list whose text box narrows the page sizes, and a pick sets how many rows a page holds", async () => {
  const user = userEvent.setup();
  const view = render(<PagedDeploys />);
  try {
    const pager = screen.getByTestId("deploys-pager");
    expect(within(pager).getByText("Rows 1–5 of 12")).toBeInTheDocument();
    expect(within(pager).getByText("Page 1 of 3")).toBeInTheDocument();
    const trigger = screen.getByRole("button", { name: pickNamed("Rows per page") });
    expect(trigger).toHaveTextContent("5 per page");

    // Every size in the pager's words, the one a page holds now ticked.
    const list = await openList(user, "Rows per page");
    expect(rows(list)).toEqual(["✓5 per page", "10 per page", "All rows"]);
    const search = screen.getByRole("combobox", { name: "Search page sizes" });
    expect(search).toHaveFocus();
    await user.type(search, "10");
    expect(rows(list)).toEqual(["10 per page"]);
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(within(pager).getByText("Rows 1–10 of 12")).toBeInTheDocument();
    expect(within(pager).getByText("Page 1 of 2")).toBeInTheDocument();
    expect(trigger).toHaveTextContent("10 per page");
    expect(trigger).toHaveFocus();

    // The size that means every row is the largest safe integer. The list holds it as a string,
    // and the pick hands the table the same number back.
    const sizes = await openList(user, "Rows per page");
    await user.click(within(sizes).getByRole("option", { name: "All rows" }));
    expect(within(pager).getByText("All 12 rows")).toBeInTheDocument();
    expect(within(pager).getByText("Page 1 of 1")).toBeInTheDocument();
    expect(trigger).toHaveTextContent("All rows");
  } finally {
    view.unmount();
  }
});

// A `<select>` set to the value it holds fires no change when that value is picked again. The
// list opens on the size a page holds, so Enter at once picks that size, and the table is asked
// for no new pagination: it neither builds new pages nor tells its page to write them.
test("a pick of the size a page holds sets nothing", async () => {
  const user = userEvent.setup();
  const paged = mock();
  const view = render(<PagedDeploys onPaged={paged} />);
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Rows per page") });
    await openList(user, "Rows per page");
    paged.mockClear();
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(paged).not.toHaveBeenCalled();
    expect(trigger).toHaveTextContent("5 per page");
    expect(trigger).toHaveFocus();
  } finally {
    view.unmount();
  }
});
