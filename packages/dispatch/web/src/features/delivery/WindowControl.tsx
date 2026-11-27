import { type ReactNode, useId, useState } from "react";

import type { ControlSize } from "../../components/control-names";
import { type ListMarks, MultiSelect } from "../../components/MultiSelect";
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
  isPreset,
  lastUtcDate,
  PRESET_LABELS,
  utcDate,
  WINDOW_PRESETS,
  type WindowChoice,
} from "./lib/window";

/** The look the Window list's trigger and the custom range's fields share. The trigger takes its
 *  room from the page's size (`MultiSelect`'s `size`); each field writes its own. */
const fieldLook = `rounded border text-sm ${surfaceMutedBg} ${borderStrong} ${textPrimaryOnCanvas}`;
const fieldClass = `${fieldLook} px-2 py-1`;

/** The option of the Window list that opens the custom range's fields. */
const CUSTOM = "custom";

/** The Window list's options: each preset, then the custom range. */
const WINDOW_OPTIONS: readonly string[] = [...WINDOW_PRESETS, CUSTOM];

const WINDOW_TRIGGER_ATTRIBUTE = "data-delivery-window";
const WINDOW_MARKS: ListMarks = { trigger: { [WINDOW_TRIGGER_ATTRIBUTE]: "" } };
/** The Window list's trigger, which the page finds by it: the shared list hands out no element
 *  of its own. */
export const WINDOW_TRIGGER_SELECTOR = `[${WINDOW_TRIGGER_ATTRIBUTE}]`;

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
 *  of whole UTC days, picked from the shared list's single pick (`MultiSelect`), whose text box
 *  narrows the options as the reader types. A pick of the option the list shows does nothing, as
 *  a `<select>` set to the value it holds fires nothing, so the page keeps its brush. A custom
 *  range is applied only once both dates are set and it spans at most 84 days, the server's
 *  limit; a longer one is refused here with the limit stated. The list's trigger takes `size`,
 *  the page's (`ControlSize`). */
export function WindowControl({
  size = "comfortable",
  value,
  onChange,
}: {
  size?: ControlSize;
  value: WindowChoice;
  onChange: (next: WindowChoice) => void;
}): ReactNode {
  const id = useId();
  const [listOpen, setListOpen] = useState(false);
  const [custom, setCustom] = useState(value.kind === "custom");
  const [dates, setDates] = useState(() => draftDates(value));
  const [refused, setRefused] = useState<string | null>(null);
  // A new window - the control's own pick, or the address changing under it - starts the draft
  // again from that window. It is set during render (React's pattern for state derived from
  // props) rather than by a key on the control: a new key would remount the Window list under the
  // reader who picked a window, and drop their focus on the body.
  const key = windowKey(value);
  const [seeded, setSeeded] = useState(key);
  if (key !== seeded) {
    setSeeded(key);
    setCustom(value.kind === "custom");
    setDates(draftDates(value));
    setRefused(null);
  }
  const shown = custom || value.kind === "custom" ? CUSTOM : value.preset;

  const apply = () => {
    const range = customRange(dates.from, dates.to);
    if ("refused" in range) {
      setRefused(range.refused);
      return;
    }
    setRefused(null);
    onChange({ kind: "custom", ...range });
  };

  return (
    <div className={`flex flex-wrap items-center gap-2 text-sm ${textMutedOnCanvas}`}>
      {/* The list is named for the field itself, so its words here are for the eye alone. */}
      <span aria-hidden="true">Window</span>
      <MultiSelect
        emptyMessage="No window matches that."
        label="Window"
        marks={WINDOW_MARKS}
        mode="single"
        onOpenChange={setListOpen}
        onPick={(next) => {
          if (next === shown) return;
          if (!isPreset(next)) {
            setCustom(true);
            return;
          }
          setCustom(false);
          setRefused(null);
          onChange({ kind: "preset", preset: next });
        }}
        open={listOpen}
        optionLabel={(option) => (isPreset(option) ? PRESET_LABELS[option] : "Custom range…")}
        options={WINDOW_OPTIONS}
        searchLabel="Search windows"
        size={size}
        triggerClassName={`${fieldLook} text-left`}
        value={shown}
      />
      {custom ? (
        <>
          <label className="flex items-center gap-1" htmlFor={`${id}-from`}>
            From
          </label>
          <input
            className={fieldClass}
            id={`${id}-from`}
            onChange={(event) => setDates({ ...dates, from: event.target.value })}
            type="date"
            value={dates.from}
          />
          <label className="flex items-center gap-1" htmlFor={`${id}-to`}>
            To
          </label>
          <input
            className={fieldClass}
            id={`${id}-to`}
            onChange={(event) => setDates({ ...dates, to: event.target.value })}
            type="date"
            value={dates.to}
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
