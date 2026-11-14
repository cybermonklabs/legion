import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import type { Table } from "@tanstack/react-table";
import type { ReactNode } from "react";
import {
  borderDefault,
  surfaceBg,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ColumnsMenu } from "../project/issues-table-controls";

/** The search, the group-by and the column control every table on these pages carries, with
 *  whatever else its own page puts beside them. */
export function TableToolbar<Row>({
  children,
  searchLabel,
  searchPlaceholder,
  table,
}: {
  children?: ReactNode;
  searchLabel: string;
  searchPlaceholder: string;
  table: Table<Row>;
}): ReactNode {
  const { globalFilter: search, grouping } = table.getState();
  return (
    <div className="flex flex-wrap items-center gap-2">
      <input
        aria-label={searchLabel}
        className={`w-full min-w-0 flex-none rounded-lg border px-3 py-1.5 text-sm sm:w-auto sm:min-w-64 sm:flex-1 ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas}`}
        maxLength={SEARCH_QUERY_MAX}
        onChange={(event) => table.setGlobalFilter(event.target.value)}
        placeholder={searchPlaceholder}
        type="search"
        value={search}
      />
      <label className={`flex items-center gap-2 text-sm ${textSecondaryOnCanvas}`}>
        Group by
        <select
          className={`rounded-lg border px-2 py-1.5 ${borderDefault} ${surfaceBg}`}
          onChange={(event) => {
            table.setGrouping(event.target.value ? [event.target.value] : []);
            table.setExpanded(true);
          }}
          value={grouping[0] ?? ""}
        >
          <option value="">Nothing</option>
          {table
            .getAllLeafColumns()
            .filter((column) => column.getCanGroup())
            .map((column) => (
              <option key={column.id} value={column.id}>
                {String(column.columnDef.header)}
              </option>
            ))}
        </select>
      </label>
      <ColumnsMenu columns={table.getAllLeafColumns()} />
      {children}
    </div>
  );
}
