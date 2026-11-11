import { useEffect, useState } from "react";

const PRESETS = [
  ["1 day", "1d"],
  ["7 days", "7d"],
  ["28 days", "28d"],
  ["90 days", "90d"],
  ["All", "all"],
] as const;

interface Dates {
  readonly from: string;
  readonly to: string;
}

function datesOf(value: string | undefined): Dates {
  const range = /^(\d{4}-\d{2}-\d{2})\.\.(\d{4}-\d{2}-\d{2})$/u.exec(value ?? "");
  return range === null ? { from: "", to: "" } : { from: range[1], to: range[2] };
}

export function RangeFilter({
  onChange,
  value,
}: {
  readonly onChange: (value: string | undefined) => void;
  readonly value: string | undefined;
}) {
  const [dates, setDates] = useState(() => datesOf(value));
  useEffect(() => setDates(datesOf(value)), [value]);
  const changeDate = (field: keyof Dates, nextValue: string) => {
    const next = { ...dates, [field]: nextValue };
    setDates(next);
    onChange(next.from && next.to ? `${next.from}..${next.to}` : undefined);
  };

  return (
    <div className="flex flex-wrap items-end gap-2 p-2">
      {PRESETS.map(([label, preset]) => (
        <button
          aria-pressed={value === preset}
          className="rounded border px-2 py-1 text-sm"
          key={preset}
          onClick={() => onChange(preset)}
          type="button"
        >
          {label}
        </button>
      ))}
      <label className="grid gap-1 text-sm">
        From
        <input
          aria-label="From"
          onChange={(event) => changeDate("from", event.target.value)}
          type="date"
          value={dates.from}
        />
      </label>
      <label className="grid gap-1 text-sm">
        To
        <input
          aria-label="To"
          onChange={(event) => changeDate("to", event.target.value)}
          type="date"
          value={dates.to}
        />
      </label>
    </div>
  );
}
