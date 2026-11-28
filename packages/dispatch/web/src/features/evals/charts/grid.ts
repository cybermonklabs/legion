// The `grid` kind (dispatch://INFRA-2918/artifact/plan-md, D10; the validator's rules,
// internal/dispatch/evals/charts): the readings matrix of its y series, one row for each reading's
// row and one column for each col, each cell the newest reading of its row and col in the window.
// A cell prints what its reading supplies: its value with its unit (with `of`, out of its total),
// its interval on the next line, a mark for the earlier readings of the cell, `—` and the reason
// of a reading with no value, and `not read` where the cell has no reading. Its shade is its value
// across the grid's values, its share of its total with `of`, or with `value_states` the state
// its value takes; a grid of more than 1,000 cells draws each as a square, and one of more than
// 10,000 is not drawn. The compare range's newest reading of each cell stands in the cell as a
// muted ghost with its change (`GridRenderer`). Each column of the grid is one series of the model
// (D4), each holding a point at every row, so the table, one row per row of the grid, holds every
// number a cell draws, and its compare value and change. Pure.
//
// The helpers `lanes.ts` shares with it, `value_states` and the words of a value and of a mock-up,
// are exported here: a lane chart maps each reading's number to a state as a grid does.
import {
  type ChartAxis,
  type ChartModel,
  type ChartPoint,
  type ChartSeries,
  type ChartStatistic,
  type ChartValue,
  NO_DATA,
  writerOf,
} from "../chart-model";
import { GridRenderer } from "./GridRenderer";
import {
  type ChartKind,
  type ChartRead,
  type DeclaredInput,
  type DeclaredSpec,
  declaredChartName,
  type MarkState,
  markState,
  specList,
  specNumber,
  specObject,
  specText,
} from "./kind";
import { rangesLine } from "./plot";
import { type Bound, type DeclaredReading, newestByCell } from "./readings";

/** One entry of a chart's `value_states`: the state a value takes from `from` up (the first entry
 *  has none: it covers every value below the second's), and the words its key gives it. */
export interface ValueStateEntry {
  readonly state: MarkState;
  readonly label: string;
  readonly from: number | null;
}

export function valueStatesOf(spec: DeclaredSpec): ValueStateEntry[] {
  return specList(spec.value_states).flatMap((item) => {
    const entry = specObject(item);
    const state = markState(entry.state);
    const label = specText(entry.label);
    return state === null || label === null ? [] : [{ from: specNumber(entry.from), label, state }];
  });
}

/** The entry `value` takes: the last whose `from` is at or below it, else the first. */
export function entryOf(
  entries: readonly ValueStateEntry[],
  value: number
): ValueStateEntry | null {
  let held = entries[0] ?? null;
  for (const entry of entries.slice(1)) {
    if (entry.from !== null && entry.from <= value) held = entry;
  }
  return held;
}

/** The units a chart writes as its own words: minutes and hours as durations, a ratio as a share,
 *  and none; any other unit follows the number. */
const WRITTEN_UNITS: ReadonlySet<string> = new Set(["", "minutes", "hours", "ratio"]);

/** `value` as a chart writes it, its bound's sign before it, and its unit after it where `unit`
 *  is true and the writer does not already name it. */
export function valueText(value: number, unit: string, bound: Bound | null = null): string {
  const number = writerOf(unit, "value")(value)(0);
  const text = WRITTEN_UNITS.has(unit) ? number : `${number} ${unit}`;
  return bound === null ? text : `${bound === ">=" ? "\u2265" : "\u2264"} ${text}`;
}

/** The number alone, as `valueText` writes it before its unit. */
function numberText(value: number, unit: string, bound: Bound | null): string {
  const number = writerOf(unit, "value")(value)(0);
  return bound === null ? number : `${bound === ">=" ? "\u2265" : "\u2264"} ${number}`;
}

/** A change from a compare value to a value, with its sign and its unit. */
export function changeText(change: number, unit: string): string {
  return change === 0
    ? "no change"
    : `${change > 0 ? "+" : "\u2212"}${valueText(Math.abs(change), unit)}`;
}

/** A time as a chart's table names it. */
export function timeLabel(ms: number): string {
  return `${new Date(ms).toISOString().slice(0, 16).replace("T", " ")} UTC`;
}

/** The line that says a chart draws its owner's mock-up, as the xy's says it (`plotModel`). */
export function mockupLine(mockup: DeclaredSpec): string {
  const by = specText(mockup.submitted_by) ?? "its owner";
  const at = specText(mockup.submitted_at) ?? "";
  return `Mock-up: nothing is measured yet; ${by} drew it${at === "" ? "" : ` on ${at.slice(0, 10)}`}.`;
}

/** A value of nothing: no reading at the place. */
export const ABSENT: ChartValue = { label: NO_DATA, note: null, state: "absent", value: null };

/** The change from the compare range's value to the window's of each statistic, in `units`. */
export function changeOf(
  values: Readonly<Record<string, ChartValue>>,
  compare: Readonly<Record<string, ChartValue>>,
  units: Readonly<Record<string, string>>
): Record<string, ChartValue> {
  return Object.fromEntries(
    Object.keys(values).map((id) => {
      const now = values[id]?.value ?? null;
      const then = compare[id]?.value ?? null;
      if (now === null || then === null) return [id, ABSENT];
      const change = now - then;
      return [
        id,
        { label: changeText(change, units[id] ?? ""), note: null, state: "plain", value: change },
      ];
    })
  );
}

/** More cells than this draw as squares with no text. */
export const GRID_TEXT_CELLS = 1000;
/** More cells than this are not drawn. */
export const GRID_MAX_CELLS = 10000;

/** How the grid draws: a cell of text each, a square each, or not at all. */
export type GridForm = "text" | "squares" | "none";

/** One part of a line a cell prints: words, or the value of one statistic of one range, which
 *  the renderer names as the table names it. */
export interface GridPart {
  readonly text: string;
  readonly statistic: string | null;
  readonly range: "window" | "compare" | "change";
}

export type GridLine = readonly GridPart[];

export interface GridPoint extends ChartPoint {
  /** Whether the cell has a reading of the window, and of the compare range. */
  readonly held: boolean;
  readonly compareHeld: boolean;
  /** The state its value takes (`value_states`), null for none. */
  readonly state: MarkState | null;
  readonly stateLabel: string | null;
  /** Its shade from 0 to 1: of its value across the grid's, or of its share of its total; null
   *  for a white cell. */
  readonly shade: number | null;
  /** The compare range's reading's state, which its ghost takes. */
  readonly compareState: MarkState | null;
  /** The lines the cell prints, and its ghost's lines while the compare range is shown. */
  readonly lines: readonly GridLine[];
  readonly compareLines: readonly GridLine[];
  /** What the cell's hover text says: its row, column, value with its total, state and note. */
  readonly tip: string;
}

export interface GridSeries extends ChartSeries {
  /** The col the column draws ("" for a series of rows, which draws as one column). */
  readonly col: string;
  readonly points: readonly GridPoint[];
}

/** One row of the grid: its name, and the row group it draws in. */
export interface GridRow {
  readonly key: string;
  readonly label: string;
  readonly group: string;
}

/** One entry of the key over the grid: a state's colour and label, the two ends of its shade, or
 *  the cell with no value. */
export type GridKeyEntry =
  | { readonly kind: "state"; readonly state: MarkState; readonly label: string }
  | { readonly kind: "shade"; readonly low: string; readonly high: string | null }
  | { readonly kind: "empty"; readonly label: string };

/** Everything about the grid's drawing that is not a value. */
export interface Grid {
  readonly form: GridForm;
  /** Rows times columns. */
  readonly cells: number;
  /** The title of the rows (`y.label`) and of the columns (`x.label`); null for none. */
  readonly rowTitle: string | null;
  readonly colTitle: string | null;
  /** The rows, in the order every series' points hold them. */
  readonly rows: readonly GridRow[];
  readonly key: readonly GridKeyEntry[];
  /** The units of the grid's values where they are more than one: then no cell is shaded. */
  readonly mixedUnits: readonly string[];
  /** The lines under the grid: the levels of its intervals. */
  readonly notes: readonly string[];
  readonly mockup: string | null;
  readonly notStartedGrey: boolean;
}

export interface GridModel extends ChartModel {
  readonly grid: Grid;
  readonly series: readonly GridSeries[];
}

/** A mock-up's points as readings of the grid's y series at their row and col. */
function mockReadings(mockup: DeclaredSpec, series: string): DeclaredReading[] {
  const at = Date.parse(specText(mockup.submitted_at) ?? "");
  const measuredMs = Number.isFinite(at) ? at : 0;
  const ciLevel = specNumber(mockup.ci_level);
  return specList(mockup.points).map((item, index) => {
    const point = specObject(item);
    const ciHigh = specNumber(point.ci_high);
    return {
      absentReason: "",
      attributes: {},
      bound: null,
      ciHigh,
      ciLevel: ciHigh === null ? null : ciLevel,
      ciLow: specNumber(point.ci_low),
      col: specText(point.col) ?? "",
      id: `mockup-${String(index).padStart(3, "0")}`,
      links: [],
      measuredAt: new Date(measuredMs).toISOString(),
      measuredMs,
      n: specNumber(point.n),
      nUnit: "",
      note: "mock-up",
      range: "window",
      recordedAt: "",
      replaces: "",
      row: specText(point.row) ?? "",
      rowGroup: "",
      rowKind: "mockup",
      series,
      source: "",
      unit: "",
      value: specNumber(point.y),
    };
  });
}

/** One range's cells: each cell's newest reading, its readings' count, and its total's reading. */
interface RangeCells {
  readonly newest: Map<string, DeclaredReading>;
  readonly counts: Map<string, number>;
  readonly totals: Map<string, DeclaredReading>;
}

function cellKey(row: string, col: string): string {
  return `${row}\n${col}`;
}

function rangeCells(
  readings: readonly DeclaredReading[],
  totals: readonly DeclaredReading[]
): RangeCells {
  const counts = new Map<string, number>();
  for (const reading of readings) {
    const key = cellKey(reading.row, reading.col);
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  return {
    counts,
    newest: newestByCell(readings, (reading) => cellKey(reading.row, reading.col)),
    totals: newestByCell(totals, (reading) => cellKey(reading.row, reading.col)),
  };
}

/** A cell's total: its reading's own `n` with `of: "n"`, else the newest reading of the `of`
 *  series in the cell; null where it has none. */
function totalOf(
  of: string | null,
  reading: DeclaredReading | undefined,
  total: DeclaredReading | undefined
): { value: number; unit: string } | null {
  if (of === null || reading === undefined) return null;
  if (of === "n") return reading.n === null ? null : { unit: reading.nUnit, value: reading.n };
  return total === undefined || total.value === null
    ? null
    : { unit: total.unit, value: total.value };
}

/** A cell's value of statistic `id`. */
function cellValue(
  id: string,
  reading: DeclaredReading | undefined,
  total: { value: number; unit: string } | null,
  count: number
): ChartValue {
  if (reading === undefined) return ABSENT;
  switch (id) {
    case "value": {
      if (reading.value === null) {
        const reason = reading.absentReason;
        return {
          label: reason === "" ? "not measured" : `not measured: ${reason}`,
          note: reason === "" ? null : reason,
          state: "absent",
          value: null,
        };
      }
      return {
        label: valueText(reading.value, reading.unit, reading.bound),
        note: reading.note === "" ? null : reading.note,
        state: "plain",
        value: reading.value,
      };
    }
    case "total":
      return total === null
        ? { label: "no total", note: null, state: "absent", value: null }
        : {
            label: valueText(total.value, total.unit),
            note: null,
            state: "plain",
            value: total.value,
          };
    case "earlier":
      return { label: `${count - 1}`, note: null, state: "plain", value: count - 1 };
    default: {
      const value = id === "ci_low" ? reading.ciLow : reading.ciHigh;
      return value === null
        ? { label: "no interval", note: null, state: "absent", value: null }
        : { label: valueText(value, reading.unit), note: null, state: "plain", value };
    }
  }
}

/** The lines a cell prints: its value (with a total, as `<value> of <total>`) or `—` and the
 *  reason its reading has none, its interval, and the mark of its earlier readings; `not read`
 *  where it has no reading. Each number names the statistic of `values` it prints. */
function cellLines(
  reading: DeclaredReading | undefined,
  totalled: boolean,
  values: Readonly<Record<string, ChartValue>>,
  count: number
): GridLine[] {
  const words = (text: string): GridPart => ({ range: "window", statistic: null, text });
  const number = (statistic: string, text: string): GridPart => ({
    range: "window",
    statistic,
    text,
  });
  if (reading === undefined) return [[words("not read")]];
  const lines: GridLine[] = [
    reading.value === null
      ? [words(reading.absentReason === "" ? "\u2014" : `\u2014 ${reading.absentReason}`)]
      : totalled
        ? [
            number("value", numberText(reading.value, reading.unit, reading.bound)),
            words(" of "),
            number("total", values.total?.label ?? ""),
          ]
        : [number("value", values.value?.label ?? NO_DATA)],
  ];
  if (reading.ciLow !== null && reading.ciHigh !== null) {
    lines.push([
      number("ci_low", numberText(reading.ciLow, reading.unit, null)),
      words(" to "),
      number("ci_high", values.ci_high?.label ?? ""),
    ]);
  }
  if (count > 1) lines.push([number("earlier", `+${count - 1} earlier`)]);
  return lines;
}

/** The order of the rows: those `order` names first, in its order, then the rest in the order of
 *  their first readings; each row group then draws in the order of its first drawn row. */
function rowsOf(
  spec: DeclaredSpec,
  readings: readonly DeclaredReading[],
  label: string
): GridRow[] {
  const groups = new Map<string, string>();
  for (const reading of readings) {
    if (!groups.has(reading.row)) groups.set(reading.row, reading.rowGroup);
  }
  const order = specList(spec.order).filter(
    (row): row is string => typeof row === "string" && groups.has(row)
  );
  const named = [...new Set(order)];
  const keys = [...named, ...[...groups.keys()].filter((row) => !named.includes(row))];
  const groupOrder = [...new Set(keys.map((row) => groups.get(row) ?? ""))];
  return keys
    .map((key) => ({ group: groups.get(key) ?? "", key, label: key === "" ? label : key }))
    .sort((a, b) => groupOrder.indexOf(a.group) - groupOrder.indexOf(b.group));
}

/** The series a grid reads: its y series, and the series of its totals. */
export function gridReads(spec: DeclaredSpec): ChartRead[] {
  const names = [specText(specObject(spec.y).series), specText(spec.of)].filter(
    (name): name is string => name !== null && name !== "n"
  );
  return [...new Set(names)].map((series) => ({ series }));
}

/**
 * The model of a `grid` (plan D10) from its spec and the readings of its series: one row for each
 * row of its y series' readings (`order`'s first, then the rest as they came, each row group
 * together) and one column for each col, as they came; each cell's newest reading of the window,
 * with its total (`of`), its state (`value_states`) or its shade, and the compare range's newest
 * reading of the cell with its change. A mock-up's points stand for readings where the y series
 * holds no reading with a value.
 */
export function gridModel(spec: DeclaredSpec, input: DeclaredInput): GridModel {
  const y = specObject(spec.y);
  const x = specObject(spec.x);
  const ySeries = specText(y.series) ?? "";
  const of = specText(spec.of);
  const entries = valueStatesOf(spec);
  const comparing = input.compare !== null;
  // With no compare range the chart draws no compare reading, whatever the views still hold.
  const drawable = input.readings.filter((reading) => comparing || reading.range === "window");
  const measured = drawable.filter((reading) => reading.series === ySeries);
  const mockup = spec.mockup === undefined ? null : specObject(spec.mockup);
  const mocked = mockup !== null && !measured.some((reading) => reading.value !== null);
  const readings = mocked && mockup !== null ? mockReadings(mockup, ySeries) : measured;
  const totals =
    of === null || of === "n" || mocked ? [] : drawable.filter((reading) => reading.series === of);
  const [window, compare] = (["window", "compare"] as const).map((range) =>
    rangeCells(
      readings.filter((reading) => reading.range === range),
      totals.filter((reading) => reading.range === range)
    )
  ) as [RangeCells, RangeCells];

  // The window's rows and columns first, then those only the compare range holds.
  const byRange = [
    ...readings.filter((reading) => reading.range === "window"),
    ...readings.filter((reading) => reading.range === "compare"),
  ];
  const rows = rowsOf(spec, byRange, ySeries);
  const cols = [...new Set(byRange.map((reading) => reading.col))];
  const cellCount = rows.length * cols.length;
  const form: GridForm =
    cellCount > GRID_MAX_CELLS ? "none" : cellCount > GRID_TEXT_CELLS ? "squares" : "text";

  // What shades a cell: its state, its share of its total, or its value across the grid's.
  const drawn = [...window.newest.values()];
  const valued = drawn.filter((reading) => reading.value !== null);
  const units = [...new Set(valued.map((reading) => reading.unit))].filter((unit) => unit !== "");
  const mixedUnits = units.length > 1 ? units.sort() : [];
  const shading: "state" | "share" | "value" | "none" =
    entries.length > 0 ? "state" : of !== null ? "share" : mixedUnits.length > 0 ? "none" : "value";
  const values = valued.map((reading) => reading.value as number);
  const low = Math.min(...values);
  const high = Math.max(...values);
  const shadeOf = (reading: DeclaredReading | undefined, total: { value: number } | null) => {
    if (reading === undefined || reading.value === null) return null;
    if (shading === "share") {
      return total === null || total.value <= 0
        ? null
        : Math.min(1, Math.max(0, reading.value / total.value));
    }
    if (shading !== "value") return null;
    return high === low ? 0 : (reading.value - low) / (high - low);
  };

  const series = cols.map((col): GridSeries => {
    const keys = rows.map((row) => cellKey(row.key, col));
    // The column's newest readings of each range, which say what its table holds.
    const held = [window, compare].flatMap((cells) =>
      keys.flatMap((key) => {
        const reading = cells.newest.get(key);
        return reading === undefined ? [] : [{ count: cells.counts.get(key) ?? 0, reading }];
      })
    );
    const statistics: ChartStatistic[] = [
      { id: "value", label: "Value" },
      ...(held.some(({ reading }) => reading.ciLow !== null || reading.ciHigh !== null)
        ? [
            { id: "ci_low", label: "Interval low" },
            { id: "ci_high", label: "Interval high" },
          ]
        : []),
      ...(of === null ? [] : [{ id: "total", label: "Total" }]),
      ...(held.some(({ count }) => count > 1)
        ? [{ id: "earlier", label: "Earlier readings" }]
        : []),
    ];
    const colLabel = col === "" ? ySeries : col;
    const points = rows.map((row, index): GridPoint => {
      const key = keys[index] as string;
      const reading = window.newest.get(key);
      const then = compare.newest.get(key);
      const total = totalOf(of, reading, window.totals.get(key));
      const thenTotal = totalOf(of, then, compare.totals.get(key));
      const count = window.counts.get(key) ?? 0;
      const own = Object.fromEntries(
        statistics.map(({ id }) => [id, cellValue(id, reading, total, count)])
      );
      const compareValues = comparing
        ? Object.fromEntries(
            statistics.map(({ id }) => [
              id,
              cellValue(id, then, thenTotal, compare.counts.get(key) ?? 0),
            ])
          )
        : null;
      const unit = reading?.unit ?? then?.unit ?? "";
      const statisticUnits = Object.fromEntries(
        statistics.map(({ id }) => [
          id,
          id === "total" ? (total?.unit ?? thenTotal?.unit ?? "") : id === "earlier" ? "" : unit,
        ])
      );
      const change = compareValues === null ? null : changeOf(own, compareValues, statisticUnits);
      const entry =
        reading === undefined || reading.value === null ? null : entryOf(entries, reading.value);
      const thenEntry =
        then === undefined || then.value === null ? null : entryOf(entries, then.value);
      const changed = change?.value;
      const note = reading?.note ?? "";
      const written =
        reading === undefined
          ? "not read"
          : `${own.value?.label ?? NO_DATA}${total === null ? "" : ` of ${own.total?.label ?? ""}`}`;
      return {
        change,
        compare: compareValues,
        compareHeld: then !== undefined,
        compareLines:
          then === undefined || compareValues === null
            ? []
            : [
                [
                  { range: "compare", statistic: null, text: "was " },
                  {
                    range: "compare",
                    statistic: "value",
                    text: compareValues.value?.label ?? NO_DATA,
                  },
                  ...(changed === undefined || changed.value === null
                    ? []
                    : [
                        { range: "change" as const, statistic: null, text: " \u00b7 " },
                        { range: "change" as const, statistic: "value", text: changed.label },
                      ]),
                ],
              ],
        compareState: thenEntry?.state ?? null,
        held: reading !== undefined,
        label: row.label,
        lines: cellLines(reading, total !== null, own, count),
        partial: false,
        shade: shadeOf(reading, total),
        state: entry?.state ?? null,
        stateLabel: entry?.label ?? null,
        tip: `${row.label}, ${colLabel}: ${written}${entry === null ? "" : `, ${entry.label}`}${
          note === "" ? "" : ` (${note})`
        }`,
        values: own,
        x: row.key,
      };
    });
    return {
      col,
      dataNote: null,
      headline: [],
      label: colLabel,
      name: col === "" ? ySeries : `${ySeries} \u00b7 ${col}`,
      noun: "readings",
      points,
      repositoryNote: null,
      statistics,
      target: null,
      unit: held[0]?.reading.unit ?? "",
      window: {
        change: null,
        compare: null,
        label: "Window",
        partial: false,
        values: {},
        x: "window",
      },
    };
  });

  // The key: each state's colour and label, or the two ends of the shade, and the cell with no
  // value where the grid has one (on squares, a cell with no reading too).
  const unvalued =
    drawn.some((reading) => reading.value === null) ||
    (form === "squares" && window.newest.size < cellCount);
  const empty: GridKeyEntry[] = unvalued ? [{ kind: "empty", label: "no value" }] : [];
  const unit = units[0] ?? "";
  const key: GridKeyEntry[] =
    shading === "state"
      ? [
          ...entries.map(({ label, state }): GridKeyEntry => ({ kind: "state", label, state })),
          ...empty,
        ]
      : shading === "share"
        ? [{ high: "100% of its total", kind: "shade", low: "0%" }, ...empty]
        : shading === "value" && values.length > 0
          ? [
              {
                high: high === low ? null : valueText(high, unit),
                kind: "shade",
                low: valueText(low, unit),
              },
              ...empty,
            ]
          : [];

  const ranges = rangesLine(spec, [...window.newest.values(), ...compare.newest.values()]);
  const axis: ChartAxis = {
    label: specText(y.label) || "Row",
    many: "rows",
    one: "row",
  };
  return {
    axis,
    comparable: true,
    compareRange: input.compare,
    grid: {
      cells: cellCount,
      colTitle: specText(x.label) || null,
      form,
      key,
      mixedUnits,
      mockup: mocked && mockup !== null ? mockupLine(mockup) : null,
      notStartedGrey: spec.not_started_outline === "grey",
      notes: ranges === null ? [] : [ranges],
      rowTitle: specText(y.label) || null,
      rows,
    },
    kind: "grid",
    name: declaredChartName(input.slug, spec),
    series,
    title: specText(spec.title) ?? specText(spec.id) ?? "",
  };
}

export const gridKind: ChartKind = {
  model: gridModel,
  reads: gridReads,
  renderer: GridRenderer,
};
