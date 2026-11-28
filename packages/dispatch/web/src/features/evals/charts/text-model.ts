// The model of a declared `text` (dispatch://INFRA-2918/artifact/plan-md, D10; the validator's
// rules, internal/dispatch/evals/charts/kinds.go `textFields`): a table of text and numbers from
// one grid series, one row for each row of its readings (those `order` names first, then the rest
// by name, 40 at most) and one column for each declared column, each cell the newest reading of
// its row and col. A value column prints the reading's value, with "of" its n, its bar or its
// dots, its interval and, under it, its note; a note column prints the note, large with `lead`,
// and under it the value; `value_states` give a value its state; a spark column draws another
// series' readings in the row over time, with the newest value beside the line. Each column is a
// series of the model whose points are the rows, so the chart's table is the same table, one
// column for each figure a cell prints; with the compare range each value has its compare value
// (the compare range's newest reading of the same cell) and its change, which the drawing prints
// beside it (D10). Pure.

import { pct } from "../../delivery/lib/format";
import type {
  ChartModel,
  ChartPoint,
  ChartSeries,
  ChartStatistic,
  ChartValue,
} from "../chart-model";
import {
  type ChartRead,
  type DeclaredInput,
  type DeclaredSpec,
  declaredChartName,
  type MarkState,
  markState,
  specLines,
  specList,
  specNumber,
  specObject,
  specText,
} from "./kind";
import { type DeclaredReading, newestByCell } from "./readings";
import { carriesUnit, changeValue, figureValue, NOTHING, notMeasured } from "./small-number";

/** A text table draws this many rows at most. */
export const TEXT_ROWS_MAX = 40;

/** A dots cell draws a total of at most this many dots; a larger one draws as a bar. */
const DOTS_MAX = 20;

/** One entry of a column's `value_states`: a value at or above `from` (the first entry, every
 *  value) takes its state, whose label names it. */
export interface ValueState {
  readonly state: MarkState;
  readonly label: string;
  readonly from: number | null;
}

export type TextColumnKind = "value" | "note" | "spark";

export interface TextColumn {
  readonly kind: TextColumnKind;
  /** The col its cells read, or a spark column's series. */
  readonly col: string;
  /** Its header: its label, and the unit every value it draws shares where its cells leave it
   *  out ("Proof work (agent hours)"). */
  readonly header: string;
  /** Its share of the table's width, null where it takes the width its text needs. */
  readonly width: number | null;
  readonly bar: boolean;
  readonly lead: boolean;
  readonly valueStates: readonly ValueState[];
  /** A share's spark line: the value its dotted done line stands at. */
  readonly done: number | null;
}

/** One cell: its figures, and what it draws of them. */
export interface TextCell extends ChartPoint {
  /** The state its reading's value takes from the column's `value_states`, and its label. */
  readonly state: { readonly state: MarkState; readonly label: string } | null;
  /** A bar cell's length, a share of the bar's room from 0 to 1. */
  readonly bar: number | null;
  /** A dots cell: one dot for each unit of its total, `filled` of them filled. */
  readonly dots: { readonly filled: number; readonly total: number } | null;
  /** A spark cell's line: its series' readings with a value in the row over the window, oldest
   *  first, each at its measured time. */
  readonly spark: readonly { readonly at: number; readonly value: number }[];
}

export interface TextSeries extends ChartSeries {
  readonly column: TextColumn;
  readonly points: readonly TextCell[];
}

export interface TextModel extends ChartModel {
  readonly text: {
    /** Each row's name and the lines `row_subtitles` write under it, in the order they draw. */
    readonly rows: readonly { readonly name: string; readonly subtitles: readonly string[] }[];
    /** The line drawn in the table's place where its series holds no reading of the window. */
    readonly empty: string | null;
    /** The rows past the table's 40, which it leaves out. */
    readonly notes: readonly string[];
  };
  readonly series: readonly TextSeries[];
}

export function isTextModel(model: ChartModel): model is TextModel {
  return "text" in model;
}

/** The series a text table reads: its own, and each spark column's. */
export function textReads(spec: DeclaredSpec): ChartRead[] {
  const sparks = specList(spec.columns).flatMap((item) => {
    const spark = specText(specObject(item).spark);
    return spark === null ? [] : [spark];
  });
  const own = specText(spec.series);
  return [...new Set([...(own === null ? [] : [own]), ...sparks])].map((series) => ({ series }));
}

function valueStatesOf(value: unknown): ValueState[] {
  return specList(value).flatMap((item) => {
    const entry = specObject(item);
    const state = markState(entry.state);
    return state === null
      ? []
      : [{ from: specNumber(entry.from), label: specText(entry.label) ?? "", state }];
  });
}

/** The state of the last entry whose `from` is at or below `value`. */
function stateOf(
  states: readonly ValueState[],
  value: number | null
): { state: MarkState; label: string } | null {
  if (value === null) return null;
  // The entries' `from` rise, so the last one at or below the value is its state (ES2022 has no
  // `findLast`).
  let entry: ValueState | undefined;
  for (const one of states) if (one.from === null || one.from <= value) entry = one;
  return entry === undefined ? null : { label: entry.label, state: entry.state };
}

/** A column's header and whether its cells print their numbers bare: where every value it draws
 *  has one unit that its cells do not keep against their numbers, the header names that unit once
 *  (unless its label names it already) and each cell leaves it out. */
function headerOf(label: string, units: ReadonlySet<string>): { header: string; bare: boolean } {
  const [unit] = units;
  if (units.size !== 1 || unit === undefined || unit === "" || carriesUnit(unit)) {
    return { bare: false, header: label };
  }
  const named = label.toLowerCase().includes(unit.toLowerCase());
  return { bare: true, header: named ? label : `${label} (${unit})` };
}

/** What a declared column holds, before its cells are read. */
interface ColumnSpec {
  readonly kind: TextColumnKind;
  readonly col: string;
  readonly label: string;
  readonly width: number | null;
  readonly bar: boolean;
  readonly dots: boolean;
  readonly lead: boolean;
  readonly valueStates: readonly ValueState[];
  /** It prints the part of its reading that no column of the same col prints: a value column
   *  the note under its value, a note column the value under its note. */
  readonly under: boolean;
}

function columnsOf(spec: DeclaredSpec): ColumnSpec[] {
  const columns = specList(spec.columns).map(specObject);
  const fields = columns.map((column) => `${specText(column.col)}\n${column.field ?? "value"}`);
  return columns.map((column): ColumnSpec => {
    const spark = specText(column.spark);
    const col = spark ?? specText(column.col) ?? "";
    const kind: TextColumnKind =
      spark !== null ? "spark" : column.field === "note" ? "note" : "value";
    const other = `${col}\n${kind === "note" ? "value" : "note"}`;
    return {
      bar: column.bar === true,
      col,
      dots: column.bar === true && column.mark === "dots",
      kind,
      label: specText(column.label) ?? col,
      lead: column.lead === true,
      under: kind !== "spark" && !fields.includes(other),
      valueStates: valueStatesOf(column.value_states),
      width: specNumber(column.width),
    };
  });
}

/** The figures of a cell's reading, by statistic: its value (bare where its column's header
 *  names the unit, and with a total, whose unit then follows the total: "19 of 28 statements"),
 *  its total (its n), its share of the total on a bar, its interval and its note. */
function figuresOf(
  column: ColumnSpec,
  bare: boolean,
  unit: string,
  reading: DeclaredReading | undefined
): Record<string, ChartValue> {
  const figures: Record<string, ChartValue> = {};
  const showsNote = column.kind === "note" || column.under;
  const showsValue = column.kind !== "note" || column.under;
  if (reading === undefined) {
    if (column.kind !== "note") figures.value = NOTHING;
    return figures;
  }
  if (showsNote && reading.note !== "") {
    figures.note = { label: reading.note, note: null, state: "plain", value: null };
  }
  if (!showsValue) return figures;
  // A note column prints its value as a value cell does, with its unit, under its note.
  const headed = bare && column.kind !== "note";
  if (reading.value === null) {
    if (column.kind !== "note") figures.value = notMeasured(reading);
    return figures;
  }
  // A spark prints its newest value alone, as a value cell with no total.
  const total = column.kind === "spark" ? null : reading.n;
  const totalUnit = reading.nUnit || reading.unit;
  figures.value = figureValue(reading.value, reading.unit, {
    bare: headed || total !== null,
    bound: reading.bound,
  });
  if (total !== null) {
    figures.n = figureValue(total, totalUnit, { bare: headed && totalUnit === unit });
    if (column.bar && total > 0) {
      const share = reading.value / total;
      figures.share = { label: pct(share, 1), note: null, state: "plain", value: share };
    }
  }
  if (reading.ciLow !== null && reading.ciHigh !== null) {
    figures.ci_low = figureValue(reading.ciLow, reading.unit, { bare: true });
    figures.ci_high = figureValue(reading.ciHigh, reading.unit, { bare: true });
  }
  return figures;
}

/** The statistics a column's table holds, in their columns' order. */
const STATISTICS: Readonly<Record<TextColumnKind, readonly ChartStatistic[]>> = {
  note: [
    { id: "note", label: "Note" },
    { id: "value", label: "Value" },
    { id: "n", label: "Of" },
    { id: "ci_low", label: "Interval low" },
    { id: "ci_high", label: "Interval high" },
  ],
  spark: [{ id: "value", label: "Newest" }],
  value: [
    { id: "value", label: "Value" },
    { id: "n", label: "Of" },
    { id: "share", label: "Share" },
    { id: "ci_low", label: "Interval low" },
    { id: "ci_high", label: "Interval high" },
    { id: "note", label: "Note" },
  ],
};

/** A range's figure of `id` where the other range has one and this one has none: a note is
 *  left out, an interval reads "no interval", and any other figure "no data". */
function missing(id: string): ChartValue | undefined {
  if (id === "note") return undefined;
  return id === "ci_low" || id === "ci_high"
    ? { label: "no interval", note: null, state: "absent", value: null }
    : NOTHING;
}

/** Each figure `ids` names of one range, the missing ones as `missing` reads them. */
function filled(
  figures: Readonly<Record<string, ChartValue>>,
  ids: readonly string[]
): Record<string, ChartValue> {
  return Object.fromEntries(
    ids.flatMap((id) => {
      const value = figures[id] ?? missing(id);
      return value === undefined ? [] : [[id, value]];
    })
  );
}

/**
 * The model of a `text` (plan D10) from its declaration and the readings of its series and its
 * spark columns' series. Each row is a row of the table's series in the window; a cell with no
 * reading reads "no data" (a note cell is empty), and one whose reading has no value "not
 * measured", for its reason.
 */
export function textModel(spec: DeclaredSpec, input: DeclaredInput): TextModel {
  const series = specText(spec.series) ?? "";
  const own = input.readings.filter((reading) => reading.series === series);
  const window = own.filter((reading) => reading.range === "window");
  const order = specList(spec.order).filter((name): name is string => typeof name === "string");
  const seen = [...new Set(window.map((reading) => reading.row))];
  const sorted = [
    ...order.filter((name) => seen.includes(name)),
    ...seen.filter((name) => !order.includes(name)).sort((a, b) => (a < b ? -1 : a > b ? 1 : 0)),
  ];
  const rows = sorted.slice(0, TEXT_ROWS_MAX);
  const left = sorted.length - rows.length;
  const subtitles = specObject(spec.row_subtitles);
  const comparing = input.compare !== null;

  const columnSeries = columnsOf(spec).map((column): TextSeries => {
    const spark = column.kind === "spark";
    const readings = spark
      ? input.readings.filter((reading) => reading.series === column.col && reading.value !== null)
      : own.filter((reading) => reading.col === column.col);
    const newest = (range: "window" | "compare") =>
      newestByCell(
        readings.filter((reading) => reading.range === range),
        (reading) => reading.row
      );
    const now = newest("window");
    const then = newest("compare");
    const drawn = rows.flatMap((row) => {
      const reading = now.get(row);
      return reading === undefined || reading.value === null ? [] : [reading];
    });
    const units = new Set(drawn.map((reading) => reading.unit));
    const { bare, header } =
      column.kind === "note"
        ? { bare: false, header: column.label }
        : headerOf(column.label, units);
    const [unit = ""] = units.size === 1 ? units : [];
    const largest = Math.max(0, ...drawn.map((reading) => reading.value as number));

    const points = rows.map((row): TextCell => {
      const reading = now.get(row);
      const before = then.get(row);
      const ownFigures = figuresOf(column, bare, unit, reading);
      const thenFigures = comparing ? figuresOf(column, bare, unit, before) : {};
      const ids = STATISTICS[column.kind]
        .map(({ id }) => id)
        .filter((id) => id in ownFigures || id in thenFigures);
      const values = filled(ownFigures, ids);
      const compareValues = comparing ? filled(thenFigures, ids) : null;
      const unitOf = (id: string) =>
        id === "n"
          ? ((reading?.nUnit || reading?.unit) ?? "")
          : id === "share"
            ? "ratio"
            : (reading?.unit ?? before?.unit ?? "");
      const value = reading?.value ?? null;
      const total = reading?.n ?? null;
      const dotted =
        column.dots &&
        value !== null &&
        total !== null &&
        Number.isInteger(total) &&
        total >= 0 &&
        total <= DOTS_MAX;
      return {
        bar:
          column.bar && !dotted && value !== null
            ? Math.min(
                1,
                Math.max(
                  0,
                  total !== null && total > 0 ? value / total : largest > 0 ? value / largest : 0
                )
              )
            : null,
        change:
          compareValues === null
            ? null
            : Object.fromEntries(
                ids
                  .filter((id) => id !== "note")
                  .map((id) => [id, changeValue(values[id], compareValues[id], unitOf(id))])
              ),
        compare: compareValues,
        dots: dotted ? { filled: Math.min(total, Math.max(0, Math.floor(value))), total } : null,
        label: row,
        partial: false,
        spark: spark
          ? readings
              .filter((one) => one.range === "window" && one.row === row)
              .map((one) => ({ at: one.measuredMs, value: one.value as number }))
          : [],
        state: stateOf(column.valueStates, value),
        values,
        x: row,
      };
    });

    const statistics = STATISTICS[column.kind].filter(({ id }) =>
      points.some((point) => id in point.values || (point.compare !== null && id in point.compare))
    );
    const named = column.kind === "spark" ? column.col : `${series} \u00b7 ${column.col}`;
    return {
      column: {
        bar: column.bar,
        col: column.col,
        done: spark && units.size === 1 ? (unit === "ratio" ? 1 : unit === "%" ? 100 : null) : null,
        header,
        kind: column.kind,
        lead: column.lead,
        valueStates: column.valueStates,
        width: column.width,
      },
      dataNote: null,
      headline: [],
      label: header,
      name: column.kind === "note" ? `${named} \u00b7 note` : named,
      noun: "readings",
      points,
      repositoryNote: null,
      statistics,
      target: null,
      unit,
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

  const rowLabel = specText(spec.row_label);
  return {
    axis: { label: rowLabel ?? "Row", many: "rows", one: "row" },
    comparable: true,
    compareRange: input.compare,
    kind: "text",
    name: declaredChartName(input.slug, spec),
    series: columnSeries,
    text: {
      empty: window.length === 0 ? `No reading of ${series} in this window.` : null,
      notes:
        left === 0
          ? []
          : [
              `${left} ${left === 1 ? "row is" : "rows are"} left out: a table draws ${TEXT_ROWS_MAX} rows at most.`,
            ],
      rows: rows.map((name) => ({ name, subtitles: specLines(subtitles[name]) })),
    },
    title: specText(spec.title) ?? specText(spec.id) ?? "",
  };
}
