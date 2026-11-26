import type { Table } from "@tanstack/react-table";
import { type ReactNode, useState } from "react";
import { ColumnsMenu } from "../../components/ColumnsMenu";
import { ControlRow } from "../../components/ControlRow";
import { DisplayMenu } from "../../components/DisplayMenu";
import { columnFilterChips, filterChips } from "../../components/filter-chips";
import { SearchBox } from "../../components/SearchBox";

/**
 * The one row a table of these pages draws its controls in, as the project List draws its own
 * (`features/project/issues-table-page.tsx`): the search box at the head of the row, the page's
 * own controls beside it with the columns menu, the display menu at the right end carrying the
 * grouping, and the chips of the column filters that are on. The table has no narrowing that is
 * not one of its columns, so the row draws no filter trigger: every column's filter is set on
 * its own header (`ColumnFilter`). Its chip is drawn here as well, whether the column is shown
 * or hidden, so a column the reader hid with the columns menu keeps its narrowing in sight, and
 * the chip's press takes it off. Every control stands at the one height of a toolbar's controls
 * from 1280 px up (the `toolbar` size). A phone's box (`compact`) draws no column control: its
 * one cell names every field whatever the reader shows (`PRIORITY_PHONE_HIDDEN`), so a choice of
 * columns there would change nothing the reader sees.
 */
export function TableToolbar<Row>({
  children,
  compact,
  searchLabel,
  searchPlaceholder,
  table,
}: {
  /** The page's own controls, such as the priorities table's **New priority**. */
  children?: ReactNode;
  /** A phone's box (`useTableLayout`'s `compact`), which draws no column control. */
  compact: boolean;
  searchLabel: string;
  searchPlaceholder: string;
  table: Table<Row>;
}): ReactNode {
  const [viewOpen, setViewOpen] = useState(false);
  const { globalFilter: search, grouping } = table.getState();
  return (
    <ControlRow
      chips={filterChips(columnFilterChips(table.getAllLeafColumns()), "toolbar")}
      display={
        <DisplayMenu
          label="View"
          onOpenChange={setViewOpen}
          open={viewOpen}
          sections={[
            {
              id: "group",
              label: "Group by",
              onChange: (next) => {
                table.setGrouping(next ? [next] : []);
                table.setExpanded(true);
              },
              options: [
                { label: "Nothing", value: "" },
                ...table
                  .getAllLeafColumns()
                  .filter((column) => column.getCanGroup())
                  .map((column) => ({ label: String(column.columnDef.header), value: column.id })),
              ],
              value: grouping[0] ?? "",
            },
          ]}
          size="toolbar"
        />
      }
      search={
        <SearchBox
          className="w-full"
          label={searchLabel}
          onChange={(value) => table.setGlobalFilter(value)}
          placeholder={searchPlaceholder}
          size="toolbar"
          value={search}
        />
      }
      size="toolbar"
    >
      {children}
      {compact ? null : <ColumnsMenu columns={table.getAllLeafColumns()} size="toolbar" />}
    </ControlRow>
  );
}
