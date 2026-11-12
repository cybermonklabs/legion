import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import type { Table } from "@tanstack/react-table";
import type { ReactNode } from "react";
import type { DisplayRow, LineProps } from "../../components/DataTable";
import {
  borderDefault,
  checkboxAccent,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { Popover } from "../project/issues-table-controls";

/** The column control of a table of any row type: the issues table's own is typed to its row,
 *  and these tables hold priorities and fault rows. */
export function ColumnsMenu<Row>({ table }: { table: Table<Row> }): ReactNode {
  return (
    <Popover align="right" label="Columns">
      {() =>
        table
          .getAllLeafColumns()
          .filter((column) => column.getCanHide())
          .map((column) => (
            <label
              className={`flex items-center gap-2 rounded px-2 py-1 ${surfaceMutedHoverBg}`}
              key={column.id}
            >
              <input
                checked={column.getIsVisible()}
                className={checkboxAccent}
                onChange={column.getToggleVisibilityHandler()}
                type="checkbox"
              />
              {String(column.columnDef.header)}
            </label>
          ))
      }
    </Popover>
  );
}

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
      <ColumnsMenu table={table} />
      {children}
    </div>
  );
}

/** One group's heading row: what the rows under it have in common, and how many there are
 *  here, with the control that folds them away. */
export function GroupHeadingRow<Row>({
  columns,
  entry,
  index,
  measure,
  noun,
  show,
}: LineProps & {
  columns: number;
  entry: DisplayRow<Row>;
  /** What a row of this table is called, in the singular. */
  noun: string;
  /** The heading as the reader sees it, where the column spells it differently. */
  show?: (value: string) => string;
}): ReactNode {
  const { continued, row, shown } = entry;
  const value = String(row.groupingValue ?? "");
  const heading = show?.(value) ?? value;
  const leaves = row.getLeafRows().filter((leaf) => !leaf.getIsGrouped()).length;
  const here = shown ?? leaves;
  const count =
    here === leaves
      ? `${leaves} ${leaves === 1 ? noun : `${noun}s`}`
      : `${here} of ${leaves} ${noun}s`;
  return (
    <tr className={surfaceMutedBg} data-index={index} ref={measure}>
      <td className="px-2 py-2" colSpan={columns}>
        <button
          aria-expanded={row.getIsExpanded()}
          aria-label={`${row.getIsExpanded() ? "Collapse" : "Expand"} ${heading}, ${count}${continued ? ", continued" : ""}`}
          className={`flex min-w-0 items-center gap-2 text-left font-semibold ${textPrimaryOnCanvas}`}
          onClick={row.getToggleExpandedHandler()}
          type="button"
        >
          <span className={`w-4 ${textMutedOnSurfaceMuted}`}>
            {row.getIsExpanded() ? "▾" : "▸"}
          </span>
          {heading}
          {continued ? (
            <span className={`font-normal ${textMutedOnSurfaceMuted}`}>(continued)</span>
          ) : null}
          <span className={`font-normal ${textMutedOnSurfaceMuted}`}>{count}</span>
        </button>
      </td>
    </tr>
  );
}
