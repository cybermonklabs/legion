import type { Column } from "@tanstack/react-table";
import { type ReactNode, useState } from "react";
import { CONTROL } from "../../components/control-names";
import { PanelTrigger } from "../../components/PanelTrigger";
import {
  borderDefault,
  card,
  surfaceBg,
  surfaceMutedHoverBg,
  tableFilterActive,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";

/** The windows the panel offers with one press. */
const PRESETS: readonly { readonly value: string; readonly label: string }[] = [
  { value: "1d", label: "1 day" },
  { value: "7d", label: "7 days" },
  { value: "28d", label: "28 days" },
  { value: "90d", label: "90 days" },
  { value: "all", label: "All" },
];

const PANEL_CLASS = `z-50 flex w-64 max-w-[calc(100vw-16px)] flex-col gap-2 rounded-lg border p-2 text-sm font-normal shadow-lg ${card}`;

/** The window a value names, in the reader's words, for the trigger's title. */
function describe(value: string): string {
  const preset = PRESETS.find((option) => option.value === value);
  if (preset !== undefined) return preset.value === "all" ? "Any time" : `Last ${preset.label}`;
  const range = /^(.*)\.\.(.*)$/.exec(value);
  if (range === null) return value;
  return `${range[1] || "start"} to ${range[2] || "now"}`;
}

/** A `<from>..<to>` end as a date input holds it: the UTC date of a day or of a time. */
const asDate = (end: string | undefined) => (end ?? "").slice(0, 10);

/**
 * The date range filter of a column of moments (Closed): the last 1, 7, 28 or 90 days, all of
 * them, or a from and to date, `from` included and `to` excluded. It writes `f.<column>` as
 * `<N>d`, `<from>..<to>` or `all`.
 */
export function RangeFilter<TData>({
  column,
  open,
  onOpenChange,
}: {
  column: Column<TData, unknown>;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}): ReactNode {
  const value = (column.getFilterValue() as string | undefined) ?? "";
  const range = /^(.*)\.\.(.*)$/.exec(value);
  const [from, setFrom] = useState(asDate(range?.[1]));
  const [to, setTo] = useState(asDate(range?.[2]));
  const name = String(column.columnDef.header);
  const active = value !== "" && value !== "all";
  return (
    <PanelTrigger
      align="right"
      // A column's own filter, found by the name every header's filter has.
      control={CONTROL.columnFilter}
      label={
        <svg aria-hidden="true" className="size-3.5" fill="currentColor" viewBox="0 0 16 16">
          <title>Dates</title>
          <path d="M4 1h1.5v1.5h5V1H12v1.5h2.5V15h-13V2.5H4zm-1 5V13.5h10V6z" />
        </svg>
      }
      onOpenChange={onOpenChange}
      open={open}
      panelClassName={PANEL_CLASS}
      title={`Filter ${name}: ${active ? describe(value) : "any time"}`}
      triggerClassName={`min-h-11 min-w-11 rounded p-1 md:min-h-8 md:min-w-8 ${active ? tableFilterActive : `${textMutedOnCanvas} ${surfaceMutedHoverBg}`}`}
    >
      {(close) => (
        <>
          <fieldset className="flex flex-wrap gap-1">
            <legend className={`mb-1 text-xs ${textMutedOnCanvas}`}>{name} in the last</legend>
            {PRESETS.map((preset) => (
              <button
                aria-pressed={value === preset.value}
                className={`min-h-9 rounded-md border px-2 text-xs ${borderDefault} ${
                  value === preset.value ? tableFilterActive : surfaceMutedHoverBg
                }`}
                key={preset.value}
                onClick={() => {
                  column.setFilterValue(preset.value);
                  close();
                }}
                type="button"
              >
                {preset.label}
              </button>
            ))}
          </fieldset>
          <form
            className="flex flex-col gap-2"
            onSubmit={(event) => {
              event.preventDefault();
              if (from === "" && to === "") return;
              column.setFilterValue(`${from}..${to}`);
              close();
            }}
          >
            <span className={`text-xs ${textMutedOnCanvas}`}>Or from and to (UTC)</span>
            <span className="flex items-center gap-1">
              <input
                aria-label={`${name} from`}
                className={`min-w-0 flex-1 rounded-md border px-1 py-1 text-xs ${borderDefault} ${surfaceBg}`}
                onChange={(event) => setFrom(event.target.value)}
                type="date"
                value={from}
              />
              <span className={textSecondaryOnCanvas}>to</span>
              <input
                aria-label={`${name} to`}
                className={`min-w-0 flex-1 rounded-md border px-1 py-1 text-xs ${borderDefault} ${surfaceBg}`}
                onChange={(event) => setTo(event.target.value)}
                type="date"
                value={to}
              />
            </span>
            <span className="flex gap-2">
              <button
                className={`min-h-9 rounded-md border px-2 text-xs ${borderDefault} ${surfaceMutedHoverBg}`}
                type="submit"
              >
                Apply
              </button>
              {value === "" ? null : (
                <button
                  className={`min-h-9 rounded-md border px-2 text-xs ${borderDefault} ${surfaceMutedHoverBg}`}
                  onClick={() => {
                    column.setFilterValue(undefined);
                    close();
                  }}
                  type="button"
                >
                  Clear
                </button>
              )}
            </span>
          </form>
        </>
      )}
    </PanelTrigger>
  );
}
