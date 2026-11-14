import type { Column } from "@tanstack/react-table";
import type { ReactNode } from "react";
import {
  borderDefault,
  surfaceBg,
  surfaceMutedHoverBg,
  tableFilterActive,
  textMutedOnCanvas,
} from "../theme/classes";
import { CONTROL } from "./control-names";
import { MultiSelect } from "./MultiSelect";
import { PanelTrigger } from "./PanelTrigger";

/** What a count of rows is called where a filter option says how many it would list: the
 *  project List counts issues, another table counts its own rows. */
export interface FilterCountNoun {
  readonly one: string;
  readonly many: string;
}

function FunnelIcon(): ReactNode {
  return (
    <svg aria-hidden="true" className="size-3.5" fill="currentColor" viewBox="0 0 16 16">
      <title>Filter</title>
      <path d="M1.5 2h13l-5 6.2V13l-3 1.5V8.2z" />
    </svg>
  );
}

const filterTrigger = (active: boolean) =>
  `min-h-11 min-w-11 rounded p-1 md:min-h-8 md:min-w-8 ${active ? tableFilterActive : `${textMutedOnCanvas} ${surfaceMutedHoverBg}`}`;

export interface ColumnFilterProps<TData> {
  readonly column: Column<TData, unknown>;
  readonly counts: ReadonlyMap<string, number>;
  /** What a count of rows is read as: `3 issues` on the project List. */
  readonly countNoun: FilterCountNoun;
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
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
  counts,
  countNoun,
  open,
  onOpenChange,
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
        triggerClassName={filterTrigger(value !== "")}
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
  return (
    <MultiSelect
      control={CONTROL.columnFilter}
      emptyMessage="No matching value."
      label={name}
      onChange={(next) => column.setFilterValue(next.length === 0 ? undefined : next)}
      onOpenChange={onOpenChange}
      open={open}
      // An option whose rows are not read yet says so rather than claim a count of zero.
      optionDetail={(value) => (unloaded.includes(value) ? "load" : String(counts.get(value) ?? 0))}
      optionDetailLabel={(value) =>
        unloaded.includes(value)
          ? "not loaded"
          : `${counts.get(value) ?? 0} ${(counts.get(value) ?? 0) === 1 ? countNoun.one : countNoun.many}`
      }
      optionLabel={show}
      options={options}
      searchLabel={`Search ${name}`}
      selected={picks}
      triggerAriaLabel={
        picks.length === 0 ? `Filter ${name}` : `Filter ${name}: ${picks.length} picked`
      }
      triggerClassName={filterTrigger(picks.length > 0)}
    >
      <FunnelIcon />
    </MultiSelect>
  );
}
