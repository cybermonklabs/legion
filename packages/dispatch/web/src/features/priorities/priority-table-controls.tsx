import type { Table } from "@tanstack/react-table";
import { type ReactNode, useState } from "react";
import { ColumnsMenu } from "../../components/ColumnsMenu";
import { ControlRow } from "../../components/ControlRow";
import { DisplayMenu } from "../../components/DisplayMenu";
import { SearchBox } from "../../components/SearchBox";

/**
 * The one row a table of these pages draws its controls in, as the project List draws its own
 * (`features/project/issues-table-page.tsx`): the search box at the head of the row, the page's
 * own controls beside it with the columns menu, and the display menu at the right end carrying
 * the grouping. The table has no narrowing that is not one of its columns, so the row draws no
 * filter trigger and no chip: every column's filter is on its own header (`ColumnFilter`).
 */
export function TableToolbar<Row>({
  children,
  searchLabel,
  searchPlaceholder,
  table,
}: {
  /** The page's own controls, such as the priorities table's **New priority**. */
  children?: ReactNode;
  searchLabel: string;
  searchPlaceholder: string;
  table: Table<Row>;
}): ReactNode {
  const [viewOpen, setViewOpen] = useState(false);
  const { globalFilter: search, grouping } = table.getState();
  return (
    <ControlRow
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
          size="compact"
        />
      }
      search={
        <SearchBox
          className="w-full"
          label={searchLabel}
          onChange={(value) => table.setGlobalFilter(value)}
          placeholder={searchPlaceholder}
          size="compact"
          value={search}
        />
      }
      size="compact"
    >
      {children}
      <ColumnsMenu columns={table.getAllLeafColumns()} size="compact" />
    </ControlRow>
  );
}
