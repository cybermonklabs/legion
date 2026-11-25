import type { Column } from "@tanstack/react-table";
import type { ReactNode } from "react";
import {
  borderDefault,
  surfaceBg,
  surfaceMutedHoverBg,
  tableFilterActive,
  textMutedOnCanvas,
} from "../theme/classes";
import { FUNNEL_PATH } from "./bar-look";
import { CONTROL, type ControlSize, glyphTarget } from "./control-names";
import type { RowNoun } from "./DataTable";
import { MultiSelect } from "./MultiSelect";
import { PanelTrigger } from "./PanelTrigger";

function FunnelIcon(): ReactNode {
  return (
    <svg aria-hidden="true" className="size-3.5" fill="currentColor" viewBox="0 0 16 16">
      <title>Filter</title>
      <path d={FUNNEL_PATH} />
    </svg>
  );
}

const filterTrigger = (active: boolean, size: ControlSize) =>
  `${glyphTarget[size]} rounded p-1 ${active ? tableFilterActive : `${textMutedOnCanvas} ${surfaceMutedHoverBg}`}`;

export interface ColumnFilterProps<TData> {
  readonly column: Column<TData, unknown>;
  readonly counts: ReadonlyMap<string, number>;
  /** Whether `counts` counts the rows the reader's narrowing lists: false while they are the
   *  previous answer's, kept on screen while the read for a new narrowing is out, and then no
   *  value draws a count beside it, since nothing has counted it yet; never a 0 that would say no
   *  row holds it. A filter given none counts. */
  readonly counted?: boolean;
  /** What one row of the table is, which a count of rows is read in: `3 issues` on the project
   *  List, `3 priorities` on the priorities table. */
  readonly noun: RowNoun;
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  /** How much room the funnel takes (`ControlSize`): the table's own `size`, which is the
   *  page's. */
  readonly size?: ControlSize;
  /** The values whose rows are not loaded yet: each says `load` rather than count them as
   *  none, as Status does for Done before the closed issues are read. */
  readonly unloaded: readonly string[];
}

/**
 * The one filter a table's header holds: the funnel that opens either a search over the column's
 * values or, for a column of free text, a box to type what it must contain. Which of the two it
 * is comes from the column's `meta`, so a column says once how it filters and every table filters
 * it the same way.
 */
export function ColumnFilter<TData>({
  column,
  counted = true,
  counts,
  noun,
  open,
  onOpenChange,
  size = "comfortable",
  unloaded,
}: ColumnFilterProps<TData>): ReactNode {
  const meta = column.columnDef.meta;
  const name = String(column.columnDef.header);
  if (meta?.text) {
    const value = (column.getFilterValue() as string | undefined) ?? "";
    return (
      <PanelTrigger
        align="right"
        control={CONTROL.columnFilter}
        label={<FunnelIcon />}
        onOpenChange={onOpenChange}
        open={open}
        title={`Filter ${name}`}
        triggerClassName={filterTrigger(value !== "", size)}
      >
        {() => (
          <input
            aria-label={`Filter ${name}`}
            className={`m-1 rounded-md border px-2 py-1 text-sm ${borderDefault} ${surfaceBg}`}
            // biome-ignore lint/a11y/noAutofocus: the panel opens to type a filter
            autoFocus
            onChange={(event) => column.setFilterValue(event.target.value || undefined)}
            placeholder={`${name} contains…`}
            value={value}
          />
        )}
      </PanelTrigger>
    );
  }
  const picks = (column.getFilterValue() as string[] | undefined) ?? [];
  const order = meta?.order;
  const options = [...new Set([...counts.keys(), ...picks, ...(meta?.always ?? [])])].sort(
    (a, b) =>
      order === undefined
        ? (counts.get(b) ?? 0) - (counts.get(a) ?? 0)
        : order.indexOf(a) - order.indexOf(b)
  );
  const show = meta?.show ?? ((value: string) => value);
  const countOf = (value: string) => counts.get(value) ?? 0;
  return (
    <MultiSelect
      // The funnel is the sign that it opens a panel, as it is on the text filter's `PanelTrigger`
      // above: the header's one filter keeps one look, with no chevron after it.
      chevron={false}
      control={CONTROL.columnFilter}
      emptyMessage="No matching value."
      label={name}
      onChange={(next) => column.setFilterValue(next.length === 0 ? undefined : next)}
      onOpenChange={onOpenChange}
      open={open}
      // An option whose rows are not read yet says so rather than claim a count of zero, and one
      // that nothing has counted yet draws no count at all.
      optionDetail={
        counted
          ? (value) => (unloaded.includes(value) ? "load" : String(countOf(value)))
          : undefined
      }
      optionDetailLabel={
        counted
          ? (value) =>
              unloaded.includes(value)
                ? "not loaded"
                : `${countOf(value)} ${countOf(value) === 1 ? noun.one : noun.many}`
          : undefined
      }
      optionLabel={show}
      options={options}
      searchLabel={`Search ${name}`}
      selected={picks}
      triggerAriaLabel={
        picks.length === 0 ? `Filter ${name}` : `Filter ${name}: ${picks.length} picked`
      }
      triggerClassName={filterTrigger(picks.length > 0, size)}
    >
      <FunnelIcon />
    </MultiSelect>
  );
}
