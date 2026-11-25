import type { Table } from "@tanstack/react-table";
import { type ReactNode, useState } from "react";
import type { BarActionProps } from "../../components/BarAction";
import { ControlRow } from "../../components/ControlRow";
import { columnFilterChips } from "../../components/filter-chips";

/**
 * The control bar a table of these pages draws its controls in (`ControlRow`), handed each
 * control as its props, as the project List hands its own (`features/project/issues-table-page.tsx`):
 * the search box first, the chips of the column filters that are on, the page's actions, the
 * columns menu, and the display menu at the bar's end carrying the grouping. The table has no
 * narrowing that is not one of its columns, so the bar draws no filter trigger: every column's
 * filter is set on its own header (`ColumnFilter`). Its chip is drawn here as well, whether the
 * column is shown or hidden, so a column the reader hid with the columns menu keeps its narrowing
 * in sight, and the chip's press takes it off; below 640 px of the bar's width the chips stand
 * behind one **+N more** in the bar's line, which lists them. What an action opens - the
 * priorities table's **New priority** form - the page draws under the bar, outside it. Every
 * control stands at the one height of a toolbar's controls from 1280 px up (the `toolbar` size).
 * A phone's box (`compact`) draws no column control: its one cell names every field whatever the
 * reader shows (`PRIORITY_PHONE_HIDDEN`), so a choice of columns there would change nothing the
 * reader sees.
 */
export function TableToolbar<Row>({
  actions,
  compact,
  searchLabel,
  searchPlaceholder,
  table,
}: {
  /** The page's own actions, such as the priorities table's **New priority**. */
  actions?: readonly BarActionProps[];
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
      actions={actions}
      chips={columnFilterChips(table.getAllLeafColumns())}
      columns={compact ? undefined : { columns: table.getAllLeafColumns() }}
      display={{
        label: "View",
        onOpenChange: setViewOpen,
        open: viewOpen,
        sections: [
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
        ],
      }}
      search={{
        label: searchLabel,
        onChange: (value) => table.setGlobalFilter(value),
        placeholder: searchPlaceholder,
        value: search,
      }}
      size="toolbar"
    />
  );
}
