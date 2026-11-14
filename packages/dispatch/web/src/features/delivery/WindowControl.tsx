import { type ReactNode, type RefObject, useId, useState } from "react";

import {
  borderStrong,
  dangerText,
  secondaryButtonCompact,
  surfaceMutedBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import {
  customRange,
  lastUtcDate,
  PRESET_LABELS,
  utcDate,
  WINDOW_PRESETS,
  type WindowChoice,
} from "./lib/window";

const fieldClass = `rounded border px-2 py-1 text-sm ${surfaceMutedBg} ${borderStrong} ${textPrimaryOnCanvas}`;

/** The window a choice names, as one string: two choices of the same window give the same one. */
function windowKey(value: WindowChoice): string {
  return value.kind === "custom" ? `${value.from}|${value.to}` : value.preset;
}

/** The dates a window puts in the custom range's fields: its own days, else none. */
function draftDates(value: WindowChoice): { from: string; to: string } {
  return value.kind === "custom"
    ? { from: utcDate(value.from), to: lastUtcDate(value.to) }
    : { from: "", to: "" };
}

/** The Delivery page's window: a rolling preset (the last 7, 14, 28 or 84 days) or a custom range
 *  of whole UTC days. A custom range is applied only once both dates are set and it spans at most
 *  84 days, the server's limit; a longer one is refused here with the limit stated. */
export function WindowControl({
  value,
  onChange,
  selectRef,
}: {
  value: WindowChoice;
  onChange: (next: WindowChoice) => void;
  /** The Window select, which the page hands the focus to when a control beside it goes. */
  selectRef?: RefObject<HTMLSelectElement | null>;
}): ReactNode {
  const id = useId();
  const [custom, setCustom] = useState(value.kind === "custom");
  const [fromDate, setFromDate] = useState(() => draftDates(value).from);
  const [toDate, setToDate] = useState(() => draftDates(value).to);
  const [refused, setRefused] = useState<string | null>(null);
  // A new window - the control's own pick, or the address changing under it - starts the draft
  // again from that window. It is set during render (React's pattern for state derived from
  // props) rather than by a key on the control: a new key remounted the Window select under the
  // keyboard reader who picked a preset with an arrow key, and dropped their focus on the body.
  const key = windowKey(value);
  const [seeded, setSeeded] = useState(key);
  if (key !== seeded) {
    const dates = draftDates(value);
    setSeeded(key);
    setCustom(value.kind === "custom");
    setFromDate(dates.from);
    setToDate(dates.to);
    setRefused(null);
  }

  const apply = () => {
    const range = customRange(fromDate, toDate);
    if ("refused" in range) {
      setRefused(range.refused);
      return;
    }
    setRefused(null);
    onChange({ kind: "custom", ...range });
  };

  return (
    <div className={`flex flex-wrap items-center gap-2 text-sm ${textMutedOnCanvas}`}>
      <label className="flex items-center gap-2" htmlFor={`${id}-window`}>
        Window
      </label>
      <select
        className={fieldClass}
        id={`${id}-window`}
        onChange={(event) => {
          const next = event.target.value;
          const preset = WINDOW_PRESETS.find((candidate) => candidate === next);
          if (preset === undefined) {
            setCustom(true);
            return;
          }
          setCustom(false);
          setRefused(null);
          onChange({ kind: "preset", preset });
        }}
        ref={selectRef}
        value={custom ? "custom" : value.kind === "preset" ? value.preset : "custom"}
      >
        {WINDOW_PRESETS.map((preset) => (
          <option key={preset} value={preset}>
            {PRESET_LABELS[preset]}
          </option>
        ))}
        <option value="custom">Custom range…</option>
      </select>
      {custom ? (
        <>
          <label className="flex items-center gap-1" htmlFor={`${id}-from`}>
            From
          </label>
          <input
            className={fieldClass}
            id={`${id}-from`}
            onChange={(event) => setFromDate(event.target.value)}
            type="date"
            value={fromDate}
          />
          <label className="flex items-center gap-1" htmlFor={`${id}-to`}>
            To
          </label>
          <input
            className={fieldClass}
            id={`${id}-to`}
            onChange={(event) => setToDate(event.target.value)}
            type="date"
            value={toDate}
          />
          <span className="text-xs">(UTC)</span>
          <button className={secondaryButtonCompact} onClick={apply} type="button">
            Apply
          </button>
          {refused === null ? null : (
            <p className={`basis-full text-xs ${dangerText}`} role="alert">
              {refused}
            </p>
          )}
        </>
      ) : null}
    </div>
  );
}
