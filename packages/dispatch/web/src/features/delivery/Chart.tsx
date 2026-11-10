// The one component every chart that draws a measure over a range goes through (INFRA-1077,
// dispatch://INFRA-979): plots with a value axis in the series' unit, a time axis of the range's
// days, a legend naming every series, and the series with nothing in the range left out and named
// once. Series of one unit share a plot. It has the two controls the spec gives every such chart:
//
//   - Graph or table. The table holds every number its graph draws: a run of numbers is one
//     column, a median-and-p90 spread is three (the median, the p90 and the results they came
//     from), a target line is named in the caption, and a day with nothing measured is a dash,
//     never a zero.
//   - Compare. The reader picks a second range, whose default is the range immediately before
//     the window, of the same length (`precedingRange`). The graph draws its series dashed beside
//     the window's, each named in the legend with its range, and the table puts the two side by
//     side, each pair with the change between them.
//
// The compare range belongs to the page, not to the chart: the chart names it and the page reads
// it as a second request, each inside the server's 84-day cap, which `customRange` holds a
// reader's own range to.
import { Fragment, type ReactNode, useCallback, useId, useMemo, useState } from "react";

import {
  borderDefault,
  borderStrong,
  chartAxis,
  chartSeries,
  dangerText,
  surfaceBg,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { customRange, DAY_MS, lastUtcDate, utcDate, type WindowChoice } from "./lib/window";
import { type SegmentedOption, SegmentedToggle } from "./SegmentedToggle";
import { UtcRangeFields } from "./UtcRangeFields";

/** A chart's range: the custom member of the page's own `WindowChoice` (`lib/window.ts`), since
 *  a chart draws the range its answer resolved and a comparison names two exact ends, never a
 *  rolling preset. `liveWindow` turns one into the window a page reads it with. */
export type ChartRange = Extract<WindowChoice, { kind: "custom" }>;

/** One row of a run of numbers: its name (a UTC day, or an issue), its value, null where nothing
 *  was measured, and whether the range covers only part of it. */
export interface SparkbarPoint {
  label: string;
  value: number | null;
  partial: boolean;
}

/** One day of a median-and-p90 spread, and how many results it came from. */
export interface SpreadBarDay {
  day: string;
  n: number;
  median_minutes: number | null;
  p90_minutes: number | null;
  partial: boolean;
}

/** A run of numbers; `label` heads its table column and names it in the legend. */
interface ChartSparkSeries {
  readonly kind: "spark";
  readonly label: string;
  readonly points: SparkbarPoint[];
  readonly target?: number;
  readonly max: number;
  readonly met?: (value: number) => boolean;
  readonly format: (value: number) => string;
  /** What the values count, in words ("hours", "share of issues"): series of one unit share a
   *  plot and its value axis; a series with none plots alone, titled by its label. */
  readonly unit?: string;
  /** How the value axis writes a round number in this unit, where `format` would not ("0h", not
   *  "0.0m"). */
  readonly tickFormat?: (value: number) => string;
}

/** A run of per-day medians and p90s, as `SpreadBars` takes them; `label` heads its two duration
 *  columns and `noun` the column counting what they came from. */
interface ChartSpreadSeries {
  readonly kind: "spread";
  readonly label: string;
  readonly days: readonly SpreadBarDay[];
  readonly target?: number;
  readonly noun: string;
  readonly format: (minutes: number) => string;
  readonly unit?: string;
}

/** One run of numbers a chart draws. */
export type ChartSeries = ChartSparkSeries | ChartSpreadSeries;

/** The comparison a page offers a chart: the range it reads beside the window, how the reader
 *  names another, and that range's series once its read lands. */
export interface ChartCompare {
  /** The range the page is reading beside the window, or null while nothing is compared. */
  readonly range: ChartRange | null;
  /** The range the reader picked; null turns the comparison off. */
  readonly onRange: (next: ChartRange | null) => void;
  /** The compare range's series, in the chart's own order; undefined while its read is out. */
  readonly series: readonly ChartSeries[] | undefined;
}

/** Where a host's comparison comes from: the range it reads beside the window, how the reader
 *  names another, and that range's own answer once its read lands. A section holds one of these
 *  and `chartCompare` turns it into each chart's `ChartCompare`. */
export interface ChartCompareSource<T> {
  /** The range the host is reading beside the window, or null while nothing is compared. */
  readonly range: ChartRange | null;
  /** The range the reader picked; null turns the comparison off. */
  readonly onRange: (next: ChartRange | null) => void;
  /** The compare range's answer; undefined while its read is out. */
  readonly data: T | undefined;
}

/** One chart's comparison: the host's range and callback, with `series` as that chart reads the
 *  compare range's answer. A host that reads no second range has no source and the chart shows
 *  its graph-or-table control alone; a chart whose series the answer does not hold (a card the
 *  compare answer has nothing for) compares nothing. */
export function chartCompare<T>(
  source: ChartCompareSource<T> | undefined,
  series: readonly ChartSeries[] | null | undefined
): ChartCompare | undefined {
  if (source === undefined) return undefined;
  return {
    onRange: source.onRange,
    range: source.range,
    series: source.data === undefined ? undefined : (series ?? undefined),
  };
}

/** What the Delivery page gives each of its sections: its one window and its one compare range,
 *  so every chart on the page follows one window control and one comparison. */
export interface DeliverySectionProps {
  /** The range the page reads beside its window, or null while nothing is compared. */
  readonly compare: ChartRange | null;
  /** The range a chart's compare control named; null turns the comparison off. */
  readonly onCompare: (next: ChartRange | null) => void;
  /** The page's window, which every section follows. */
  readonly window: WindowChoice;
}

/** What one row of a chart's table is, where it is not a UTC day: the heading of the table's
 *  first column, the caption's own words for the run the series draw ("<label> by <over>"), and
 *  how a compare range's rows pair with the window's. The Legion section's cards draw a measure
 *  over the window by issue, by worker type and by UTC week; every other chart on the page draws
 *  one per UTC day. */
export interface ChartRows {
  readonly header: string;
  readonly over: string;
  /** How the table pairs a compare range's row with the window's, which is what its change
   *  column is the change between. Every chart says which it is:
   *
   *    - `"position"`: row *i* of each range is the *i*-th of its own run, which is what a run
   *      of days or of UTC weeks is — two ranges of one length cover the same number of them,
   *      and the pairs are the two ranges' first days, second days, and so on.
   *    - `"name"`: a row pairs with the compare range's row of the same name, and a name only
   *      one range holds is a row with nothing on the other side. A run whose rows are issues or
   *      worker types is this: row *i* of two ranges is two different issues, so a change
   *      between them would be the change between two unrelated numbers.
   */
  readonly pair: "position" | "name";
}

/** What a run of per-day bars is: the rows every chart has unless it names its own. */
const UTC_DAYS: ChartRows = { header: "Day", over: "UTC day", pair: "position" };

export interface ChartProps {
  /** What the chart draws, in the reader's words: it names both controls and heads the table. */
  readonly label: string;
  /** The range the series cover, as the answer resolved it. */
  readonly range: ChartRange;
  /** The series drawn, in order; the table holds every number all of them draw. */
  readonly series: readonly ChartSeries[];
  /** The comparison, from the page that reads its range; omitted where none is read. */
  readonly compare?: ChartCompare;
  /** What one row of its table is, where it is not a UTC day. */
  readonly rows?: ChartRows;
  /** Called with a table row's name, and the column's header for a number's cell (null for the
   *  row's own name): a host that filters its issues by the issues behind a number passes it,
   *  and the table's names and numbers become buttons. */
  readonly onSelect?: (row: string, column: string | null) => void;
  /** What the table view holds where it is not the graph's numbers: a graph drawn per day over
   *  a table of one row per issue. `compare` is the compare range's table series. */
  /** Which view opens first: a run of fewer than three rows reads better as its table. */
  readonly defaultMode?: "graph" | "table";
  readonly table?: {
    readonly series: readonly ChartSeries[];
    readonly rows: ChartRows;
    readonly compare: readonly ChartSeries[] | undefined;
  };
}

/** What a day with nothing measured reads as: never a zero. */
const NOTHING = "\u2014";

/** What a number whose read is still out reads as, as the measures panel's numbers do. */
export const PENDING = "\u2026";

type ChartMode = "graph" | "table";

const MODES: readonly SegmentedOption<ChartMode>[] = [
  { value: "graph", label: "Graph" },
  { value: "table", label: "Table" },
];

/** A small control beside a chart: the compare picker here, and the Legion section's project
 *  picker, so one shape serves every control the page's charts sit under. */
export const fieldClass = `rounded border px-1.5 py-0.5 text-xs ${surfaceMutedBg} ${borderStrong} ${textPrimaryOnCanvas}`;

const noteClass = `text-[11px] ${textMutedOnSurface}`;

/** The range immediately before `range`, of the same length: what a comparison starts from. It
 *  is as long as a range the server already answered, so it is inside the server's cap too. */
export function precedingRange(range: ChartRange): ChartRange {
  const from = Date.parse(range.from);
  const to = Date.parse(range.to);
  return {
    kind: "custom",
    from: new Date(from - (to - from)).toISOString(),
    to: new Date(from).toISOString(),
  };
}

/** The range a run of whole UTC days covers, read as the window control reads a reader's two
 *  dates: the first day's midnight to the midnight after the last. */
export function daysRange(first: string, last: string): ChartRange {
  const range = customRange(first, last);
  if ("refused" in range) throw new Error(`a chart's days are no range: ${range.refused}`);
  return { kind: "custom", ...range };
}

/** Whether two ranges name the same two instants. */
function sameRange(one: ChartRange, other: ChartRange): boolean {
  return (
    Date.parse(one.from) === Date.parse(other.from) && Date.parse(one.to) === Date.parse(other.to)
  );
}

/** A range in the reader's words: the UTC days it covers. A range ending at a midnight ends on
 *  the day before, as `lastUtcDate` reads a whole-day range's exclusive end; a rolling preset,
 *  whose end is the present, ends on the day it is in, which is the last day the bars draw. */
function rangeLabel(range: ChartRange): string {
  const to = Date.parse(range.to);
  return `${utcDate(range.from)} to ${to % DAY_MS === 0 ? lastUtcDate(range.to) : utcDate(range.to)}`;
}

/** How long a range is, in the reader's words; a brushed window may be hours, which no whole
 *  number of days names. */
function spanLabel(range: ChartRange): string {
  const days = (Date.parse(range.to) - Date.parse(range.from)) / DAY_MS;
  if (!Number.isInteger(days) || days < 1) return "period";
  return days === 1 ? "day" : `${days} days`;
}

/** One number the graph draws, as the table holds it. */
interface ChartColumn {
  readonly header: string;
  readonly format: (value: number) => string;
}

/** One day's cell in every column, in the columns' order; null is nothing measured. */
interface ChartRow {
  readonly day: string;
  readonly partial: boolean;
  readonly values: readonly (number | null)[];
}

interface ChartTable {
  readonly columns: readonly ChartColumn[];
  readonly rows: readonly ChartRow[];
}

const asCount = (value: number): string => `${value}`;

/** Every number a series set draws, by day: a run of bars fills one column, a spread fills its
 *  median, its p90 and the results they came from, and a day one series does not reach is null
 *  there. Days keep the order the series hold them in, oldest first. */
function tableOf(series: readonly ChartSeries[]): ChartTable {
  const columns: ChartColumn[] = [];
  for (const one of series) {
    if (one.kind === "spark") {
      columns.push({ format: one.format, header: one.label });
      continue;
    }
    columns.push({ format: one.format, header: `${one.label} median` });
    columns.push({ format: one.format, header: `${one.label} p90` });
    // The count the spread's two durations came from, headed by what the series counts.
    columns.push({
      format: asCount,
      header: one.noun.charAt(0).toUpperCase() + one.noun.slice(1),
    });
  }
  const order: string[] = [];
  const cells = new Map<string, (number | null)[]>();
  const partial = new Set<string>();
  const row = (day: string): (number | null)[] => {
    const held = cells.get(day);
    if (held !== undefined) return held;
    const fresh: (number | null)[] = columns.map(() => null);
    cells.set(day, fresh);
    order.push(day);
    return fresh;
  };
  let at = 0;
  for (const one of series) {
    if (one.kind === "spark") {
      for (const point of one.points) {
        row(point.label)[at] = point.value;
        if (point.partial) partial.add(point.label);
      }
      at += 1;
      continue;
    }
    for (const day of one.days) {
      const values = row(day.day);
      values[at] = day.median_minutes;
      values[at + 1] = day.p90_minutes;
      values[at + 2] = day.n;
      if (day.partial) partial.add(day.day);
    }
    at += 3;
  }
  return {
    columns,
    rows: order.map((day) => ({
      day,
      partial: partial.has(day),
      values: cells.get(day) ?? [],
    })),
  };
}

/** The compare range's table while its read is out: no row pairs with anything. */
const emptyTable: ChartTable = { columns: [], rows: [] };

/** One line of the table: the window's row and the compare range's it is read against, either
 *  of which is absent where that range holds no such row. */
interface ChartPair {
  readonly before: ChartRow | undefined;
  readonly row: ChartRow | undefined;
}

/** The table's lines: every row of the window, each against the compare range's row it pairs
 *  with, and then every row of the compare range that paired with none. `compared` is null where
 *  nothing is compared, which is one line per window row.
 *
 *  `pair` is the chart's own rule (`ChartRows`): by position for a run of days or weeks, where
 *  the *i*-th row of each range is the *i*-th of its own run, and by name for a run the rows
 *  name — the issues, the worker types — where the *i*-th rows of two ranges are two different
 *  things and a change between them would be no change at all. */
function pairedRows(
  table: ChartTable,
  compared: ChartTable | null,
  pair: "position" | "name"
): ChartPair[] {
  if (compared === null) return table.rows.map((row) => ({ before: undefined, row }));
  if (pair === "position") {
    return Array.from(
      { length: Math.max(table.rows.length, compared.rows.length) },
      (_, index) => ({
        before: compared.rows[index],
        row: table.rows[index],
      })
    );
  }
  const before = new Map(compared.rows.map((row) => [row.day, row]));
  const paired: ChartPair[] = table.rows.map((row) => ({ before: before.get(row.day), row }));
  const named = new Set(table.rows.map((row) => row.day));
  for (const row of compared.rows) {
    if (!named.has(row.day)) paired.push({ before: row, row: undefined });
  }
  return paired;
}

/** A number as the table reads it: nothing measured where there is none, and an ellipsis where
 *  the read that would hold it is still out. */
function numberText(
  value: number | null | undefined,
  format: (value: number) => string,
  pending: boolean
): string {
  if (pending) return PENDING;
  return value === null || value === undefined ? NOTHING : format(value);
}

/** The change from the compare range's number to the window's, signed. */
function changeText(
  value: number | null | undefined,
  before: number | null | undefined,
  format: (value: number) => string,
  pending: boolean
): string {
  if (pending) return PENDING;
  if (value === null || value === undefined || before === null || before === undefined) {
    return NOTHING;
  }
  const change = value - before;
  if (change > 0) return `+${format(change)}`;
  if (change < 0) return `-${format(-change)}`;
  return format(0);
}

/** What the reader compares the window with, once they have picked a range. */
interface Comparison {
  readonly range: ChartRange;
  readonly series: readonly ChartSeries[] | undefined;
}

/** One line of a plot: its name, its values by position, its format, and whether it is the
 *  compare range's. */
interface PlotLine {
  readonly label: string;
  readonly values: readonly (number | null)[];
  readonly partial: readonly boolean[];
  readonly format: (value: number) => string;
  readonly tickFormat?: (value: number) => string;
  readonly compare: boolean;
  readonly color: number;
}

/** A series as the lines it plots: a run of numbers is one line, a median-and-p90 spread two. */
function linesOf(series: ChartSeries, color: number, compare: boolean): PlotLine[] {
  if (series.kind === "spark") {
    return [
      {
        label: series.label,
        values: series.points.map((point) => point.value),
        partial: series.points.map((point) => point.partial),
        format: series.format,
        tickFormat: series.tickFormat,
        compare,
        color,
      },
    ];
  }
  return [
    {
      label: `${series.label} median`,
      values: series.days.map((day) => day.median_minutes),
      partial: series.days.map((day) => day.partial),
      format: series.format,
      compare,
      color,
    },
    {
      label: `${series.label} p90`,
      values: series.days.map((day) => day.p90_minutes),
      partial: series.days.map((day) => day.partial),
      format: series.format,
      compare,
      color: color + 1,
    },
  ];
}

/** The rows a series runs over, by position: its days or its points' names. */
function rowLabels(series: ChartSeries): string[] {
  return series.kind === "spark"
    ? series.points.map((point) => point.label)
    : series.days.map((day) => day.day);
}

/** Whether a series has anything to draw: a value that is neither missing nor zero. */
function hasData(series: ChartSeries): boolean {
  return series.kind === "spark"
    ? series.points.some((point) => point.value !== null && point.value !== 0)
    : series.days.some((day) => day.median_minutes !== null);
}

/** A row's value in a series: a number, or null where nothing was measured. */
function rowValue(series: ChartSeries, index: number): number | null {
  return series.kind === "spark"
    ? (series.points[index]?.value ?? null)
    : (series.days[index]?.median_minutes ?? null);
}

/** The rows a plot draws: the range less its leading and trailing days on which no series
 *  measured anything, so a window reaching past the data does not draw an empty third, and a note
 *  naming the days left out. */
function filledSpan(series: readonly ChartSeries[]): { from: number; to: number; note: string } {
  const first = series[0];
  if (first === undefined) return { from: 0, to: 0, note: "" };
  const rows = rowLabels(first);
  const filled = (index: number) =>
    series.some((one) => {
      const value = rowValue(one, index);
      return value !== null && value !== 0;
    });
  let from = 0;
  while (from < rows.length && !filled(from)) from++;
  let to = rows.length;
  while (to > from && !filled(to - 1)) to--;
  if (from >= to) return { from: 0, to: rows.length, note: "" };
  const parts: string[] = [];
  if (from > 0) parts.push(`${tickLabel(rows[0] ?? "")} to ${tickLabel(rows[from - 1] ?? "")}`);
  if (to < rows.length)
    parts.push(`${tickLabel(rows[to] ?? "")} to ${tickLabel(rows[rows.length - 1] ?? "")}`);
  return {
    from,
    to,
    note:
      parts.length === 0 ? "" : `Nothing measured ${parts.join(" or ")}; those days are not drawn.`,
  };
}

/** A series cut to its rows from `from` to `to`. */
function sliceSeries(series: ChartSeries, from: number, to: number): ChartSeries {
  return series.kind === "spark"
    ? { ...series, points: series.points.slice(from, to) }
    : { ...series, days: series.days.slice(from, to) };
}

/** A round top for a value axis over `max`, and its step, so ticks read 0, 5, 10, 15, 20. */
function niceScale(max: number): { top: number; step: number } {
  if (!(max > 0)) return { top: 1, step: 0.25 };
  const raw = max / 4;
  const power = 10 ** Math.floor(Math.log10(raw));
  const step = ([1, 2, 2.5, 5, 10].find((m) => m * power >= raw) ?? 10) * power;
  return { top: step * Math.ceil(max / step), step };
}

/** A row's name on the time axis: a UTC day as "Nov 3", anything else as it is. */
function tickLabel(label: string): string {
  const day = label.match(/^(\d{4})-(\d{2})-(\d{2})$/);
  if (day === null) return label;
  const month = [
    "Jan",
    "Feb",
    "Mar",
    "Apr",
    "May",
    "Jun",
    "Jul",
    "Aug",
    "Sep",
    "Oct",
    "Nov",
    "Dec",
  ][Number(day[2]) - 1];
  return `${month} ${Number(day[3])}`;
}

/** The plot's measured width, which its text is drawn at: a fixed viewBox would scale its labels
 *  with the page. */
function useWidth(): [(node: HTMLDivElement | null) => void, number] {
  const [width, setWidth] = useState(720);
  const ref = useCallback((node: HTMLDivElement | null) => {
    if (node === null || typeof ResizeObserver === "undefined") return;
    setWidth(node.clientWidth || 720);
    const observer = new ResizeObserver(() => setWidth(node.clientWidth || 720));
    observer.observe(node);
  }, []);
  return [ref, width];
}

const PLOT_HEIGHT = 170;
const MARGIN = { top: 8, right: 12, bottom: 26, left: 62 };

/** One plot: the lines of one unit over the rows, with a value axis of numbers in that unit, a
 *  time axis of the rows' days, and a dashed target where a series has one. */
function Plot({
  comparedRange,
  label,
  lines,
  rows,
  target,
  title,
  unit,
}: {
  comparedRange: string | null;
  label: string;
  lines: readonly PlotLine[];
  rows: readonly string[];
  target: number | undefined;
  title: string;
  unit: string;
}): ReactNode {
  const [ref, width] = useWidth();
  const format =
    lines[0]?.tickFormat ??
    ((value: number) => (lines[0]?.format ?? String)(value).replace(/\.0(?=\D*$)/, ""));
  const all = lines.flatMap((line) =>
    line.values.filter((value): value is number => value !== null)
  );
  const { top, step } = niceScale(Math.max(...all, target ?? 0, 0));
  const inner = {
    w: Math.max(width - MARGIN.left - MARGIN.right, 40),
    h: PLOT_HEIGHT - MARGIN.top - MARGIN.bottom,
  };
  const n = Math.max(rows.length, 1);
  const x = (index: number) => MARGIN.left + ((index + 0.5) * inner.w) / n;
  const y = (value: number) => MARGIN.top + inner.h - (value / top) * inner.h;
  const ticks: number[] = [];
  for (let at = 0; at <= top + step / 2; at += step) ticks.push(at);
  const every = Math.max(1, Math.ceil(n / Math.max(2, Math.floor(inner.w / 64))));
  return (
    <div className="min-w-0" ref={ref}>
      <div className="flex flex-wrap items-baseline gap-x-2" data-chart-plot-title>
        <span className={`text-sm font-medium ${textPrimaryOnCanvas}`}>{title}</span>
        <span className={`text-[11px] ${textMutedOnSurface}`} data-chart-unit>
          {unit}
        </span>
      </div>
      <svg
        aria-label={`${label}: ${unit} by ${rows.length > 0 && /^\d{4}-/.test(rows[0] ?? "") ? "day" : "row"}`}
        className="block"
        data-chart-plot={unit}
        height={PLOT_HEIGHT}
        role="img"
        width={width}
      >
        {ticks.map((tick) => (
          <g key={tick}>
            <line
              className={chartAxis.grid}
              x1={MARGIN.left}
              x2={MARGIN.left + inner.w}
              y1={y(tick)}
              y2={y(tick)}
            />
            <text
              className={`${chartAxis.text} text-[11px]`}
              dominantBaseline="middle"
              textAnchor="end"
              x={MARGIN.left - 6}
              y={y(tick)}
            >
              {format(Math.round(tick * 100) / 100)}
            </text>
          </g>
        ))}
        <line
          className={chartAxis.base}
          x1={MARGIN.left}
          x2={MARGIN.left + inner.w}
          y1={y(0)}
          y2={y(0)}
        />
        {rows.map((row, index) =>
          (index % every === 0 && rows.length - 1 - index > every / 2) ||
          index === rows.length - 1 ? (
            <text
              className={`${chartAxis.text} text-[11px]`}
              data-chart-tick={row}
              key={row}
              textAnchor="middle"
              x={x(index)}
              y={PLOT_HEIGHT - 8}
            >
              {tickLabel(row)}
            </text>
          ) : null
        )}
        {target === undefined ? null : (
          <line
            className={chartAxis.base}
            strokeDasharray="4 3"
            x1={MARGIN.left}
            x2={MARGIN.left + inner.w}
            y1={y(target)}
            y2={y(target)}
          />
        )}
        {lines.map((line) => {
          const palette = chartSeries[line.color % chartSeries.length] ?? chartSeries[0];
          // A day with no value breaks the line rather than drawing it to zero.
          const runs: string[][] = [[]];
          line.values.forEach((value, index) => {
            if (value === null) runs.push([]);
            else runs[runs.length - 1]?.push(`${x(index)},${y(value)}`);
          });
          return (
            <g
              data-chart-line={line.compare ? "compare" : "window"}
              data-chart-series={line.label}
              key={`${line.label}-${line.compare}`}
            >
              {runs.map((run) =>
                run.length > 1 ? (
                  <polyline
                    className={palette.stroke}
                    fill="none"
                    key={run[0]}
                    strokeDasharray={line.compare ? "5 4" : undefined}
                    strokeWidth={2}
                    points={run.join(" ")}
                  />
                ) : null
              )}
              {line.values.map((value, index) =>
                value === null ? null : (
                  <circle
                    className={palette.fill}
                    cx={x(index)}
                    cy={y(value)}
                    key={`${rows[index] ?? index}`}
                    opacity={line.partial[index] || line.compare ? 0.45 : 1}
                    r={3}
                  >
                    <title>{`${line.label}, ${rows[index] ?? ""}${line.partial[index] ? " (partial)" : ""}: ${line.format(value)}`}</title>
                  </circle>
                )
              )}
            </g>
          );
        })}
      </svg>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs" data-chart-legend-row>
        {lines.map((line) => {
          const palette = chartSeries[line.color % chartSeries.length] ?? chartSeries[0];
          return (
            <span
              className="flex items-center gap-1.5"
              data-chart-legend={line.compare ? "compare" : "window"}
              key={`${line.label}-${line.compare}`}
            >
              <span
                aria-hidden
                className={`inline-block h-0.5 w-4 ${palette.swatch} ${line.compare ? "opacity-50" : ""}`}
              />
              <span className={textMutedOnSurface}>
                {line.label}
                {line.compare ? `, ${comparedRange}` : ""}
              </span>
            </span>
          );
        })}
      </div>
    </div>
  );
}

/** The chart itself: one plot per unit, each with its axes, its series in a legend below, a
 *  compared range's series drawn dashed beside the window's and named in the legend, and the
 *  series with nothing in the range left out and named once. */
function Graph({
  against,
  label,
  range,
  series,
}: {
  against: Comparison | null;
  label: string;
  range: ChartRange;
  series: readonly ChartSeries[];
}): ReactNode {
  const span = filledSpan(series);
  const trimmedSeries = series.map((one) => sliceSeries(one, span.from, span.to));
  const trimmedCompare =
    against?.series === undefined
      ? undefined
      : against.series.map((one) => sliceSeries(one, span.from, span.to));
  const drawn = trimmedSeries.flatMap((one, index) => (hasData(one) ? [{ one, index }] : []));
  const empty = trimmedSeries.filter((one) => !hasData(one)).map((one) => one.label);
  const units = [...new Set(drawn.map(({ one }) => one.unit ?? one.label))];
  let color = 0;
  const colorOf = new Map<number, number>();
  for (const { one, index } of drawn) {
    colorOf.set(index, color);
    color += one.kind === "spread" ? 2 : 1;
  }
  const comparedRange = against === null ? null : rangeLabel(against.range);
  return (
    <div className="flex min-w-0 flex-col gap-1" data-chart-graph>
      {drawn.length === 0 ? (
        <p className={noteClass}>Nothing measured in this range.</p>
      ) : (
        units.map((unit) => {
          const members = drawn.filter(({ one }) => (one.unit ?? one.label) === unit);
          const lines = members.flatMap(({ one, index }) => [
            ...linesOf(one, colorOf.get(index) ?? 0, false),
            ...(trimmedCompare?.[index] === undefined
              ? []
              : linesOf(trimmedCompare[index] as ChartSeries, colorOf.get(index) ?? 0, true)),
          ]);
          const first = members[0]?.one;
          return (
            <Plot
              comparedRange={comparedRange}
              key={unit}
              label={label}
              title={members.map(({ one }) => one.label).join(" vs ")}
              lines={lines}
              rows={first === undefined ? [] : rowLabels(first)}
              target={members.find(({ one }) => one.target !== undefined)?.one.target}
              unit={unit}
            />
          );
        })
      )}
      <div className={`flex flex-wrap gap-x-3 ${noteClass}`}>
        <div data-chart-range="window">
          <span>{rangeLabel(range)}</span>
        </div>
        {against === null ? null : (
          <div data-chart-range="compare">
            <span>{`Compare ${rangeLabel(against.range)}`}</span>
          </div>
        )}
        {span.note === "" ? null : <span data-chart-trimmed>{span.note}</span>}
        {empty.length === 0 ? null : (
          <span data-chart-empty>{`Nothing in this range: ${empty.join(", ")}.`}</span>
        )}
      </div>
    </div>
  );
}

/** Every number the graph draws, in a table: one row a day (or whatever `naming` says a row is),
 *  one column each number, and with a comparison the two ranges side by side with the change
 *  between them. */
function NumberTable({
  against,
  label,
  naming,
  onSelect,
  series,
}: {
  against: Comparison | null;
  label: string;
  naming: ChartRows;
  onSelect?: (row: string, column: string | null) => void;
  series: readonly ChartSeries[];
}): ReactNode {
  // Every number the series draw, and every number the compare range's draw, built once per run
  // of series rather than on every render of the host: a live delta or a keystroke elsewhere on
  // the page re-renders the section around a chart whose numbers have not moved.
  const table = useMemo(() => tableOf(series), [series]);
  const pending = against !== null && against.series === undefined;
  const comparedSeries = against?.series;
  const compared = useMemo(
    () => (comparedSeries === undefined ? null : tableOf(comparedSeries)),
    [comparedSeries]
  );
  const comparing = against !== null;
  const notes = [`${label} by ${naming.over}`];
  if (against !== null) notes.push(`compared with ${rangeLabel(against.range)}`);
  for (const one of series) {
    if (one.target !== undefined) notes.push(`${one.label} target ${one.format(one.target)}`);
  }
  notes.push(`${NOTHING} is a ${naming.header.toLowerCase()} with nothing measured`);
  const rows = useMemo(
    () => pairedRows(table, comparing ? (compared ?? emptyTable) : null, naming.pair),
    [comparing, compared, naming.pair, table]
  );
  return (
    <div className={`max-h-48 overflow-auto rounded border ${borderDefault}`}>
      <table className="w-full text-xs tabular-nums" data-chart-table={label}>
        <caption className={`px-2 py-1 text-left ${noteClass}`}>{`${notes.join(". ")}.`}</caption>
        <thead className={`sticky top-0 ${surfaceBg} ${textMutedOnSurface}`}>
          <tr>
            <th className="px-2 py-1 text-left font-normal">{naming.header}</th>
            {against === null ? null : (
              <th className="px-2 py-1 text-left font-normal">{`Compare ${naming.header.toLowerCase()}`}</th>
            )}
            {table.columns.map((column) => (
              <Fragment key={column.header}>
                <th className="px-2 py-1 text-right font-normal">{column.header}</th>
                {against === null ? null : (
                  <>
                    <th className="px-2 py-1 text-right font-normal">{`${column.header} compare`}</th>
                    <th className="px-2 py-1 text-right font-normal">{`${column.header} change`}</th>
                  </>
                )}
              </Fragment>
            ))}
          </tr>
        </thead>
        <tbody className={textMutedOnCanvas}>
          {rows.map(({ before, row }, index) => (
            <tr className={`border-t ${borderDefault}`} key={row?.day ?? before?.day ?? `${index}`}>
              <td className="px-2 py-0.5">
                {row === undefined ? (
                  NOTHING
                ) : (
                  <Selectable
                    label={`Show the issues of ${row.day}`}
                    onSelect={onSelect === undefined ? undefined : () => onSelect(row.day, null)}
                  >
                    {`${row.day}${row.partial ? " (partial)" : ""}`}
                  </Selectable>
                )}
              </td>
              {against === null ? null : (
                <td className="px-2 py-0.5">
                  {pending ? PENDING : before === undefined ? NOTHING : before.day}
                </td>
              )}
              {table.columns.map((column, at) => (
                <Fragment key={column.header}>
                  <td className="px-2 py-0.5 text-right">
                    <Selectable
                      label={`Show the issues behind ${column.header} of ${row?.day ?? ""}`}
                      onSelect={
                        onSelect === undefined || row === undefined || !row.values[at]
                          ? undefined
                          : () => onSelect(row.day, column.header)
                      }
                    >
                      {numberText(row?.values[at], column.format, false)}
                    </Selectable>
                  </td>
                  {against === null ? null : (
                    <>
                      <td className="px-2 py-0.5 text-right">
                        {numberText(before?.values[at], column.format, pending)}
                      </td>
                      <td className="px-2 py-0.5 text-right">
                        {changeText(row?.values[at], before?.values[at], column.format, pending)}
                      </td>
                    </>
                  )}
                </Fragment>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** A table name or number: a button where the host filters by the issues behind it, plain text
 *  otherwise. */
function Selectable({
  children,
  label,
  onSelect,
}: {
  children: ReactNode;
  label: string;
  onSelect: (() => void) | undefined;
}): ReactNode {
  if (onSelect === undefined) return children;
  return (
    <button
      aria-label={label}
      className="underline decoration-dotted underline-offset-2 hover:decoration-solid"
      data-chart-select
      onClick={onSelect}
      type="button"
    >
      {children}
    </button>
  );
}

/** The compare control: no second range, the range just before the window, or one the reader
 *  names as two whole UTC days, which `customRange` holds to the server's 84-day cap. */
function CompareControl({
  compare,
  label,
  range,
}: {
  compare: ChartCompare;
  label: string;
  range: ChartRange;
}): ReactNode {
  const id = useId();
  const [picking, setPicking] = useState(false);
  const [draft, setDraft] = useState<{ from: string; to: string } | null>(null);
  const [refused, setRefused] = useState<string | null>(null);
  const preceding = precedingRange(range);
  // A range another chart on the page named: this control shows it as the reader's own, since
  // every chart offers the one comparison the page reads.
  const named =
    compare.range !== null && !sameRange(compare.range, preceding) ? compare.range : null;
  const chosen =
    compare.range === null
      ? picking
        ? "custom"
        : "none"
      : named === null
        ? "preceding"
        : "custom";
  const fromDate = draft?.from ?? (named === null ? "" : utcDate(named.from));
  const toDate = draft?.to ?? (named === null ? "" : lastUtcDate(named.to));

  const apply = () => {
    const picked = customRange(fromDate, toDate);
    if ("refused" in picked) {
      setRefused(picked.refused);
      return;
    }
    setRefused(null);
    compare.onRange({ kind: "custom", ...picked });
  };

  return (
    <div className={`flex flex-wrap items-center gap-1.5 text-xs ${textMutedOnCanvas}`}>
      <select
        aria-label={`${label} comparison`}
        className={fieldClass}
        data-chart-compare={label}
        onChange={(event) => {
          const next = event.target.value;
          if (next === "custom") {
            setPicking(true);
            return;
          }
          setPicking(false);
          setRefused(null);
          compare.onRange(next === "preceding" ? preceding : null);
        }}
        value={chosen}
      >
        <option value="none">No comparison</option>
        <option value="preceding">{`Previous ${spanLabel(range)}`}</option>
        <option value="custom">Custom range…</option>
      </select>
      {chosen === "custom" ? (
        <UtcRangeFields
          fieldClass={fieldClass}
          from={fromDate}
          idPrefix={id}
          onApply={apply}
          onFrom={(next) => setDraft({ from: next, to: toDate })}
          onTo={(next) => setDraft({ from: fromDate, to: next })}
          refusalClass={`basis-full ${dangerText}`}
          refused={refused}
          to={toDate}
        />
      ) : null}
    </div>
  );
}

/** One chart: its series as bars or as the table of every number they draw, under the two
 *  controls every chart of a measure over a range has. */
export function Chart({
  compare,
  label,
  onSelect,
  range,
  rows = UTC_DAYS,
  series,
  table,
  defaultMode = "graph",
}: ChartProps): ReactNode {
  const [mode, setMode] = useState<ChartMode>(defaultMode);
  const against: Comparison | null =
    compare === undefined || compare.range === null
      ? null
      : { range: compare.range, series: compare.series };
  return (
    <div className="flex min-w-0 flex-col gap-1" data-chart={label}>
      {mode === "graph" ? (
        <Graph against={against} label={label} range={range} series={series} />
      ) : (
        <NumberTable
          against={
            against === null || table === undefined
              ? against
              : { range: against.range, series: table.compare }
          }
          label={label}
          naming={table?.rows ?? rows}
          onSelect={onSelect}
          series={table?.series ?? series}
        />
      )}
      <div className="flex flex-wrap items-center gap-1.5">
        <SegmentedToggle label={`${label} view`} onChange={setMode} options={MODES} value={mode} />
        {compare === undefined ? null : (
          <CompareControl compare={compare} label={label} range={range} />
        )}
      </div>
    </div>
  );
}
