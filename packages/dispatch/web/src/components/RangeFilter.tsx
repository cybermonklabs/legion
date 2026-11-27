import type { Column } from "@tanstack/react-table";
import { type ReactNode, useState } from "react";
import { ALL_CLOSED, rangeDays, rangeValue } from "../lib/closed-window";
import {
  borderDefault,
  surfaceBg,
  surfaceMutedHoverBg,
  tableFilterActive,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../theme/classes";
import { CONTROL } from "./control-names";
import { PanelTrigger } from "./PanelTrigger";

/** The windows the panel sets with one press, each by the value it writes. */
const PRESETS: readonly { readonly value: string; readonly label: string }[] = [
  { value: "1d", label: "1 day" },
  { value: "7d", label: "7 days" },
  { value: "28d", label: "28 days" },
  { value: "90d", label: "90 days" },
  { value: ALL_CLOSED, label: "All" },
];

const BUTTON = `min-h-9 rounded-md border px-2 text-xs ${borderDefault}`;
const DATE_INPUT = `min-w-0 flex-1 rounded-md border px-1 py-1 text-xs ${borderDefault} ${surfaceBg}`;

/** A window as the reader reads it, which its chip names: `7d` is Last 7 days, `all` is All, and a
 *  range is its first and last day as the picker shows them. A value this cannot read stays as it
 *  is. */
export function rangeWords(value: string): string {
  if (value === ALL_CLOSED) return "All";
  const days = /^(\d+)d$/.exec(value)?.[1];
  if (days !== undefined) return days === "1" ? "Last 1 day" : `Last ${days} days`;
  const range = rangeDays(value);
  if (range === undefined) return value;
  if (range.from === "") return `To ${range.to}`;
  if (range.to === "") return `From ${range.from}`;
  return `${range.from} to ${range.to}`;
}

function CalendarIcon(): ReactNode {
  return (
    <svg aria-hidden="true" className="size-3.5" fill="currentColor" viewBox="0 0 16 16">
      <title>Dates</title>
      <path d="M4 1h1.5v1.5h5V1H12v1.5h2.5V15h-13V2.5H4zm-1 5V13.5h10V6z" />
    </svg>
  );
}

/** The panel's controls, made each time it opens, so its dates start at the window in force. The
 *  picker takes no To before its From: each date bounds the other (`min`, `max`), so the browser's
 *  calendar offers no such day and its check stops Apply, and `rangeValue` writes nothing for it. */
function RangePanel({
  close,
  name,
  onPick,
  value,
}: {
  close: () => void;
  name: string;
  onPick: (next: string | undefined) => void;
  value: string;
}): ReactNode {
  const shown = rangeDays(value);
  const [from, setFrom] = useState(shown?.from ?? "");
  const [to, setTo] = useState(shown?.to ?? "");
  const pick = (next: string | undefined) => {
    onPick(next);
    close();
  };
  return (
    <>
      <fieldset className="flex flex-wrap gap-1">
        <legend className={`mb-1 text-xs ${textMutedOnCanvas}`}>{name} in the last</legend>
        {PRESETS.map((preset) => (
          <button
            aria-pressed={value === preset.value}
            className={`${BUTTON} ${value === preset.value ? tableFilterActive : surfaceMutedHoverBg}`}
            key={preset.value}
            onClick={() => pick(preset.value)}
            type="button"
          >
            {preset.label}
          </button>
        ))}
      </fieldset>
      <form
        className="flex w-full flex-col gap-2"
        onSubmit={(event) => {
          event.preventDefault();
          const next = rangeValue(from, to);
          if (next !== undefined) pick(next);
        }}
      >
        <span className={`text-xs ${textMutedOnCanvas}`}>Or from and to (UTC)</span>
        <span className="flex items-center gap-1">
          <input
            aria-label={`${name} from`}
            className={DATE_INPUT}
            max={to === "" ? undefined : to}
            onChange={(event) => setFrom(event.target.value)}
            type="date"
            value={from}
          />
          <span className={textSecondaryOnCanvas}>to</span>
          <input
            aria-label={`${name} to`}
            className={DATE_INPUT}
            min={from === "" ? undefined : from}
            onChange={(event) => setTo(event.target.value)}
            type="date"
            value={to}
          />
        </span>
        <span className="flex gap-2">
          <button className={`${BUTTON} ${surfaceMutedHoverBg}`} type="submit">
            Apply
          </button>
          {value === "" ? null : (
            <button
              className={`${BUTTON} ${surfaceMutedHoverBg}`}
              onClick={() => pick(undefined)}
              type="button"
            >
              Clear
            </button>
          )}
        </span>
      </form>
    </>
  );
}

export interface RangeFilterProps<TData> {
  /** A column of moments whose filter holds a window (`readWindow` in `lib/closed-window.ts`). */
  readonly column: Column<TData, unknown>;
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  /** The trigger's look while a window is set and while none is: the header filter's own
   *  (`ColumnFilter`). */
  readonly triggerClass: (active: boolean) => string;
}

/**
 * The date range filter of a column of moments (Closed): the last 1, 7, 28 or 90 days, all of
 * them, or a From and a To date, both days included. The address keeps the list route's rule,
 * `from` included and `to` left out, so the picked To day is written as the next UTC day and an
 * address read back shows the day before its `to` (`rangeValue`, `rangeDays`): 2026-11-03 to
 * 2026-11-09 writes `2026-11-03..2026-11-10`. It writes the column's filter as `<N>d`,
 * `<from>..<to>` or `all`.
 */
export function RangeFilter<TData>({
  column,
  open,
  onOpenChange,
  triggerClass,
}: RangeFilterProps<TData>): ReactNode {
  const value = (column.getFilterValue() as string | undefined) ?? "";
  const name = String(column.columnDef.header);
  return (
    <PanelTrigger
      align="right"
      control={CONTROL.columnFilter}
      label={<CalendarIcon />}
      onOpenChange={onOpenChange}
      open={open}
      stacked
      title={`Filter ${name}`}
      triggerClassName={triggerClass(value !== "" && value !== ALL_CLOSED)}
    >
      {(close) => (
        <RangePanel
          close={close}
          name={name}
          onPick={(next) => column.setFilterValue(next)}
          value={value}
        />
      )}
    </PanelTrigger>
  );
}
