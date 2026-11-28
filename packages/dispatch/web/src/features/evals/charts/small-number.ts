// How a declared `number` writes its small numbers and a declared `text` table writes its cells
// (dispatch://INFRA-2918/artifact/plan-md, D10): a reading's number with its unit, its bound's sign
// and, where asked, a leading plus; the words of its interval; and the change from the compare
// range's value to the window's. Pure.
import { type ChartValue, NO_DATA, writerOf } from "../chart-model";
import type { Bound, DeclaredReading } from "./readings";

/** The units a written number carries itself: `writerOf` writes minutes, hours and a ratio with
 *  theirs ("45m", "3.5 h", "12.0%"), and a number keeps `$`, `%` and `×` against it. A table's
 *  header never names one of them, since each cell keeps it. */
const OWN_UNITS: ReadonlySet<string> = new Set(["minutes", "hours", "ratio", "$", "%", "\u00d7"]);

export function carriesUnit(unit: string): boolean {
  return OWN_UNITS.has(unit);
}

/** A value of nothing: no reading at all. */
export const NOTHING: ChartValue = { label: NO_DATA, note: null, state: "absent", value: null };

/** How a figure is written: its unit left out where a header names it (`bare`), a leading plus
 *  before a positive value (`signed`), and its bound's sign before it. */
export interface FigureStyle {
  readonly bare?: boolean;
  readonly signed?: boolean;
  readonly bound?: Bound | null;
}

/** `value` written with its unit, as a small number or a table cell prints it: "15.78 times",
 *  "+39.9 h", "$785", "≥ 12". */
export function writeFigure(value: number, unit: string, style: FigureStyle = {}): string {
  const number = writerOf(unit, "value")(Math.abs(value))(0);
  const withUnit =
    style.bare === true || unit === "" || unit === "minutes" || unit === "hours" || unit === "ratio"
      ? number
      : unit === "$"
        ? `$${number}`
        : unit === "%" || unit === "\u00d7"
          ? `${number}${unit}`
          : `${number} ${unit}`;
  const sign = value < 0 ? "-" : style.signed === true && value > 0 ? "+" : "";
  const bound = style.bound ?? null;
  return `${bound === null ? "" : `${bound === ">=" ? "\u2265" : "\u2264"} `}${sign}${withUnit}`;
}

/** A figure as a drawn value: `value` written in `unit`, or nothing to measure. */
export function figureValue(
  value: number | null,
  unit: string,
  style: FigureStyle = {},
  note: string | null = null
): ChartValue {
  return value === null
    ? NOTHING
    : { label: writeFigure(value, unit, style), note, state: "plain", value };
}

/** A reading with no value, as its cell prints it: "not measured", with its reason as its note. */
export function notMeasured(reading: DeclaredReading): ChartValue {
  return {
    label: "not measured",
    note: reading.absentReason === "" ? null : reading.absentReason,
    state: "absent",
    value: null,
  };
}

/** The words after an interval: its level's ("95% interval"), else the chart's `interval_label`
 *  for an interval with no level, else "recorded interval". */
export function intervalWords(level: number | null, label: string | null): string {
  if (level === null) return label ?? "recorded interval";
  return `${Number((level * 100).toFixed(1))}% interval`;
}

/** The change from the compare range's value to the window's, in `unit`; nothing where either
 *  has no value. */
export function changeValue(
  value: ChartValue | undefined,
  compare: ChartValue | undefined,
  unit: string
): ChartValue {
  const now = value?.value ?? null;
  const then = compare?.value ?? null;
  if (now === null || then === null) return NOTHING;
  const change = now - then;
  return {
    label:
      change === 0
        ? "no change"
        : `${change > 0 ? "+" : "\u2212"}${writeFigure(Math.abs(change), unit)}`,
    note: null,
    state: "plain",
    value: change,
  };
}

/** The UTC day a reading was measured on, as the page dates a small number. */
export function measuredDay(reading: DeclaredReading): string {
  return new Date(reading.measuredMs).toISOString().slice(0, 10);
}
