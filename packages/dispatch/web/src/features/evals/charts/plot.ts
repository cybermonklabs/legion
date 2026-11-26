// The model of a declared chart that draws readings as points on two axes: an `xy` and a `band`
// (dispatch://INFRA-2918/artifact/plan-md, D10; the validator's rules,
// internal/dispatch/evals/charts). Each group of the chart is one series of the model (D4); every
// series holds a point at every place the chart's table has a row for, so the table, one row per
// place, holds each series' value there, and a place a series has no reading at reads "no data".
// The compare range's readings are each point's compare values: over time at the instant the
// compare range's shift onto the window's axis puts them, against an x series and by step at the
// same row or step, so a reading's change is its value less the compare range's there. Pure.
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
import {
  type ChartRead,
  type DeclaredInput,
  type DeclaredSpec,
  declaredChartName,
  groupTones,
  type MarkState,
  markState,
  specList,
  specNumber,
  specObject,
  specText,
  type Tone,
} from "./kind";
import { type Bound, type DeclaredReading, newestByCell } from "./readings";

export type PlotKind = "xy" | "band";

/** What an x value is: a measured time, a number of an x series, or a step (`x.steps`). */
export type XForm = "time" | "number" | "category";

/** One axis as the spec declares it: its title, log scale, whole-number or weekly ticks, and the
 *  ends it fixes (epoch milliseconds on a time axis). */
export interface PlotAxis {
  readonly title: string;
  readonly unit: string;
  readonly log: boolean;
  readonly ticks: "whole" | "week" | null;
  readonly min: number | null;
  readonly max: number | null;
}

/** A reference: a rule at y (in a state's colour where it names one), the band from y to yEnd, the
 *  diagonal y = x, or a target on each line of an xy with steps at its last step. */
export type Reference =
  | {
      readonly type: "rule";
      readonly y: number;
      readonly label: string;
      readonly state: MarkState | null;
    }
  | { readonly type: "band"; readonly y: number; readonly yEnd: number; readonly label: string }
  | { readonly type: "diagonal"; readonly label: string }
  | { readonly type: "from-start"; readonly by: number; readonly label: string };

/** A labelled mark at an x value: a dot at (at, y), a shaded span from at to end, else a dashed
 *  rule; on an xy a dotted line or a bracket from y to yEnd, a label with no dot, and at the
 *  declared end of a numeric x axis, plain text at the plot's edge. */
export interface Annotation {
  readonly at: number;
  readonly end: number | null;
  readonly y: number | null;
  readonly yEnd: number | null;
  readonly label: string;
  readonly detail: string | null;
  readonly state: MarkState | null;
  readonly dot: boolean;
  readonly bracket: boolean;
  readonly edge: boolean;
}

/** One plot of a chart: its one plot, one of `y_options` (one view at a time), or one of `panels`
 *  (side by side). */
export interface PlotView {
  /** A panel's title, or an option's label, which names its button. */
  readonly title: string | null;
  readonly x: PlotAxis;
  readonly y: PlotAxis;
  readonly reference: Reference | null;
  readonly annotations: readonly Annotation[];
  /** The units its readings are in, where they are in more than one: it then draws no number. */
  readonly mixedUnits: readonly string[];
}

/** One entry of the key over the plot. */
export interface KeyEntry {
  readonly name: string;
  readonly tone: Tone;
  readonly hollow: boolean;
}

/** One item that `items` lists under an x: its title (the readings' row), its group, its note and
 *  its parts (their cols). */
export interface PlotItem {
  readonly title: string;
  readonly group: string;
  readonly tone: Tone;
  readonly note: string;
  readonly parts: readonly string[];
}

/** The items of an xy over time, one list for each x that holds any. */
export interface PlotItems {
  readonly partsLabel: string;
  /** The architecture chart of the same eval whose parts an item outlines. */
  readonly outlines: string | null;
  readonly places: readonly {
    readonly x: string;
    readonly label: string;
    readonly at: number;
    readonly items: readonly PlotItem[];
  }[];
}

/** The second value axis of an xy: each y number times `scale`, in `unit`. */
export interface SecondAxis {
  readonly scale: number;
  readonly label: string;
  readonly unit: string;
}

/** Everything about the chart's drawing that is not a value. */
export interface Plot {
  readonly kind: PlotKind;
  readonly form: XForm;
  /** The steps of a category axis, in order. */
  readonly steps: readonly string[];
  /** An xy with steps and `points: "only"`: each series is a step, each reading a dot at it. */
  readonly strip: boolean;
  readonly layout: "one" | "options" | "panels";
  readonly views: readonly PlotView[];
  /** How a series' points join: straight, in steps, or not at all. */
  readonly line: "linear" | "step" | "none";
  /** Which points draw a dot: all, the last of each line, or all and no line. */
  readonly marks: "all" | "end" | "only";
  readonly legend: "key" | "direct" | "none";
  readonly key: readonly KeyEntry[];
  readonly height: number;
  readonly zoom: boolean;
  /** A shape mock-up's drawing of its own: its axes have no tick values. */
  readonly noScale: boolean;
  readonly secondAxis: SecondAxis | null;
  /** Against an x series: readings with no x reading draw in a strip right of the plot. */
  readonly missingX: boolean;
  /** The lines under the plot: its ranges' levels, its bounds, its rings and medians, the
   *  readings it cuts or does not plot. */
  readonly notes: readonly string[];
  /** Where the chart draws its owner's mock-up, the line that says so. */
  readonly mockup: string | null;
  readonly notStartedGrey: boolean;
  /** The chart names medians, so its other rows' points draw tinted. */
  readonly medians: boolean;
  /** A strip's thin line for each row group from step to step. */
  readonly joinRowGroups: boolean;
  readonly items: PlotItems | null;
}

/** What a series of the plot is: a group's line or points, a run under medians, a median row, or
 *  a named group's median at each x (`summary`). */
export type SeriesRole = "group" | "run" | "median" | "summary";

export interface PlotPoint extends ChartPoint {
  /** Its place on the x axis: epoch milliseconds, the x series' number, or the step's index;
   *  null where it has none (no x reading), or where the series has no reading here. */
  readonly at: number | null;
  /** Whether the series has a reading of the window here. */
  readonly held: boolean;
  readonly bound: Bound | null;
  /** The compare range's reading's place on the window's axis. */
  readonly compareAt: number | null;
  readonly compareHeld: boolean;
  readonly compareBound: Bound | null;
  readonly rowGroup: string;
  /** On a strip, a median row's bar rather than a dot; `overall`, the median over every run. */
  readonly median: boolean;
  readonly overall: boolean;
}

export interface PlotSeries extends ChartSeries {
  /** The index of its view among `plot.views`. */
  readonly view: number;
  readonly group: string;
  readonly tone: Tone;
  readonly hollow: boolean;
  readonly role: SeriesRole;
  /** A median row whose marks a thin line joins (`medians.join`), and whose range is shaded
   *  across x (`medians.band`). */
  readonly joined: boolean;
  readonly banded: boolean;
  /** On an xy with steps and `reference.from_start`: the target at its last step. */
  readonly fromStart: number | null;
  readonly points: readonly PlotPoint[];
}

export interface PlotModel extends ChartModel {
  readonly plot: Plot;
  readonly series: readonly PlotSeries[];
}

export function isPlotModel(model: ChartModel): model is PlotModel {
  return "plot" in model;
}

/** A value of nothing: no reading of the series at the place. */
const ABSENT: ChartValue = { label: NO_DATA, note: null, state: "absent", value: null };

/** A time axis's title where its spec names none. */
const MEASURED_TIME = "Measured time (UTC)";

/** A chart's height where its spec names none, in pixels. */
const DEFAULT_HEIGHT = 340;

/** Fewer readings than this at one x give a summary's median no 95% interval. */
const SUMMARY_MIN_READINGS = 6;

const BOUND_WORDS: Readonly<Record<Bound, string>> = {
  "<=": "upper bound: the true value is at most this",
  ">=": "lower bound: the true value is at least this",
};

function plural(count: number, one: string): string {
  return `${count} ${one}${count === 1 ? "" : "s"}`;
}

/** `value` as a chart writes a number of `unit`, its bound's sign before it. */
function writeNumber(value: number, unit: string, bound: Bound | null = null): string {
  const text = writerOf(unit, "value")(value)(0);
  return bound === null ? text : `${bound === ">=" ? "\u2265" : "\u2264"} ${text}`;
}

/** A time as a chart's table names it. */
function timeLabel(ms: number): string {
  return `${new Date(ms).toISOString().slice(0, 16).replace("T", " ")} UTC`;
}

/** A spec's x value as its axis places it: a UTC date's first moment or a UTC time, a number, or
 *  the index of its step. */
function placeOf(form: XForm, steps: readonly string[], value: unknown): number | null {
  if (form === "number") return specNumber(value);
  if (typeof value !== "string") return null;
  if (form === "category") return steps.includes(value) ? steps.indexOf(value) : null;
  const ms = Date.parse(/^\d{4}-\d{2}-\d{2}$/.test(value) ? `${value}T00:00:00Z` : value);
  return Number.isNaN(ms) ? null : ms;
}

/** One place of one series: the reading drawn there, the x reading it is joined to, and where it
 *  stands. */
interface Cell {
  readonly key: string;
  readonly label: string;
  readonly at: number | null;
  readonly reading: DeclaredReading;
  readonly x: DeclaredReading | null;
  readonly median: boolean;
  readonly overall: boolean;
}

/** One range's cells of one view, by group and then by place. */
type Cells = Map<string, Map<string, Cell>>;

function addCell(cells: Cells, group: string, cell: Cell): void {
  let places = cells.get(group);
  if (places === undefined) {
    places = new Map();
    cells.set(group, places);
  }
  // Two readings of one group at one place each keep a place of their own.
  let key = cell.key;
  let label = cell.label;
  for (let count = 2; places.has(key); count++) {
    key = `${cell.key} (${count})`;
    label = `${cell.label} (${count})`;
  }
  places.set(key, key === cell.key ? cell : { ...cell, key, label });
}

/** How the spec reads its readings into series and places. */
interface Reader {
  readonly form: XForm;
  readonly steps: readonly string[];
  readonly strip: boolean;
  /** The field each series is a value of; on a strip, the step. */
  readonly group: "col" | "row_group" | "row" | null;
  readonly medianRows: readonly string[];
  readonly overall: string | null;
  /** Milliseconds the compare range's readings move to stand on the window's axis. */
  readonly shift: number;
}

function groupOf(reader: Reader, reading: DeclaredReading): string {
  switch (reader.group) {
    case "col":
      return reading.col;
    case "row_group":
      return reading.rowGroup;
    case "row":
      return reading.row;
    default:
      return "";
  }
}

/** The name a reading's place against an x series takes: its row and col but the one its group
 *  is a value of. */
function cellName(reader: Reader, reading: DeclaredReading): string {
  const parts = [
    reader.group === "row" ? "" : reading.row,
    reader.group === "col" ? "" : reading.col,
  ].filter((part) => part !== "");
  return parts.length === 0 ? "all" : parts.join(" \u00b7 ");
}

/** One range's cells of the view that draws `ys` (and, against an x series, `xs`), and how many of
 *  its readings the chart does not plot and why. */
function cellsOf(
  reader: Reader,
  range: "window" | "compare",
  ys: readonly DeclaredReading[],
  xs: readonly DeclaredReading[]
): { cells: Cells; notAStep: number } {
  const cells: Cells = new Map();
  const own = ys.filter((reading) => reading.range === range);
  if (reader.form === "time") {
    const shift = range === "compare" ? reader.shift : 0;
    for (const reading of own) {
      const at = reading.measuredMs + shift;
      addCell(cells, groupOf(reader, reading), {
        at,
        key: new Date(at).toISOString(),
        label: timeLabel(at),
        median: false,
        overall: false,
        reading,
        x: null,
      });
    }
    return { cells, notAStep: 0 };
  }
  if (reader.form === "number") {
    const newest = newestByCell(own, (reading) => `${reading.row}\n${reading.col}`);
    const xOwn = xs.filter((reading) => reading.range === range);
    // An x series of one col joins by row alone; else by row and col.
    const byRow = new Set(xOwn.map((reading) => reading.col)).size <= 1;
    const xNewest = newestByCell(xOwn, (reading) =>
      byRow ? reading.row : `${reading.row}\n${reading.col}`
    );
    for (const reading of newest.values()) {
      const x = xNewest.get(byRow ? reading.row : `${reading.row}\n${reading.col}`) ?? null;
      const name = cellName(reader, reading);
      addCell(cells, groupOf(reader, reading), {
        at: x?.value ?? null,
        key: name,
        label: name,
        median: false,
        overall: false,
        reading,
        x,
      });
    }
    return { cells, notAStep: 0 };
  }
  const stepped = own.filter((reading) => reader.steps.includes(reading.col));
  for (const reading of newestByCell(stepped, (cell) => `${cell.row}\n${cell.col}`).values()) {
    const at = reader.steps.indexOf(reading.col);
    const median = reader.medianRows.includes(reading.row);
    const overall = reading.row === reader.overall;
    // A strip draws each step as a series of dots, one for each row; else each row is a line.
    addCell(cells, reader.strip ? reading.col : reading.row, {
      at,
      key: reader.strip ? reading.row : reading.col,
      label: reader.strip ? reading.row : reading.col,
      median,
      overall,
      reading,
      x: null,
    });
  }
  return { cells, notAStep: own.length - stepped.length };
}

/** A mock-up point as a reading of the view's y series, so the chart draws it as it draws one. */
function mockReading(
  point: DeclaredSpec,
  index: number,
  value: number | null,
  ciLevel: number | null
): DeclaredReading {
  return {
    absentReason: "",
    attributes: {},
    bound: null,
    ciHigh: specNumber(point.ci_high),
    ciLevel: specNumber(point.ci_high) === null ? null : ciLevel,
    ciLow: specNumber(point.ci_low),
    col: "",
    id: `mockup-${index}`,
    links: [],
    measuredAt: "",
    measuredMs: 0,
    n: specNumber(point.n),
    nUnit: "",
    note: "mock-up",
    range: "window",
    recordedAt: "",
    replaces: "",
    row: specText(point.series) ?? "",
    rowGroup: "",
    rowKind: "mockup",
    series: specText(point.series) ?? "",
    source: "",
    unit: "",
    value,
  };
}

/** A mock-up's points as the window's cells of the chart's one view. */
function mockCells(reader: Reader, mockup: DeclaredSpec, form: XForm, steps: string[]): Cells {
  const cells: Cells = new Map();
  const ciLevel = specNumber(mockup.ci_level);
  specList(mockup.points).forEach((item, index) => {
    const point = specObject(item);
    const group = specText(point.series) ?? "";
    const reading = mockReading(point, index, specNumber(point.y), ciLevel);
    const at = placeOf(form, steps, point.x);
    if (at === null) return;
    const name =
      form === "time"
        ? new Date(at).toISOString()
        : form === "number"
          ? `x = ${writeNumber(at, "")}`
          : String(point.x);
    const strip = reader.strip && form === "category";
    addCell(cells, strip ? name : group, {
      at,
      key: strip ? group : name,
      label: strip ? group : form === "time" ? timeLabel(at) : name,
      median: false,
      overall: false,
      reading,
      x: form === "number" ? mockReading(point, index, at, null) : null,
    });
  });
  return cells;
}

/** The median of sorted numbers, and its distribution-free 95% interval: the order statistics
 *  x(j) and x(n+1-j) of the largest j for which 2 P(Binomial(n, 1/2) < j) is at most 5%, none
 *  for fewer than six numbers. */
function medianInterval(sorted: readonly number[]): {
  median: number;
  low: number | null;
  high: number | null;
} {
  const n = sorted.length;
  const middle = Math.floor(n / 2);
  const median =
    n % 2 === 1
      ? (sorted[middle] as number)
      : ((sorted[middle - 1] as number) + (sorted[middle] as number)) / 2;
  if (n < SUMMARY_MIN_READINGS) return { high: null, low: null, median };
  // P(Binomial(n, 1/2) = k), summed from k = 0, in logs so a large n does not underflow.
  let logTerm = -n * Math.LN2;
  let below = 0;
  let j = 0;
  for (let k = 0; k < middle; k++) {
    below += Math.exp(logTerm);
    if (2 * below > 0.05) break;
    j = k + 1;
    logTerm += Math.log((n - k) / (k + 1));
  }
  if (j === 0) return { high: null, low: null, median };
  return { high: sorted[n - j] as number, low: sorted[j - 1] as number, median };
}

/** A named group's median of y at each x, from the plotted readings of the groups it names: a col,
 *  or a population, the part of a col before " · ". */
function summaryCells(name: string, cells: Cells): Map<string, Cell> {
  const byX = new Map<number, Cell[]>();
  for (const places of cells.values()) {
    for (const cell of places.values()) {
      const col = cell.reading.col;
      if (cell.at === null || cell.reading.value === null) continue;
      if (col !== name && col.split(" \u00b7 ")[0] !== name) continue;
      byX.set(cell.at, [...(byX.get(cell.at) ?? []), cell]);
    }
  }
  const summary = new Map<string, Cell>();
  for (const [at, group] of [...byX.entries()].sort((a, b) => a[0] - b[0])) {
    const first = group[0] as Cell;
    const values = group.map((cell) => cell.reading.value as number).sort((a, b) => a - b);
    const { high, low, median } = medianInterval(values);
    const bounds = new Set(group.map((cell) => cell.reading.bound));
    const bound = bounds.size === 1 ? first.reading.bound : null;
    const key = `median at x = ${writeNumber(at, first.x?.unit ?? "")}`;
    summary.set(key, {
      at,
      key,
      label: `${key} (${plural(group.length, "reading")})`,
      median: false,
      overall: false,
      reading: {
        ...first.reading,
        bound,
        ciHigh: high,
        ciLevel: high === null ? null : 0.95,
        ciLow: low,
        id: `summary-${name}-${at}`,
        n: group.length,
        note: "",
        value: median,
      },
      x: first.x,
    });
  }
  return summary;
}

/** The ids of what a series' table holds: x against an x series, then y, then its interval where
 *  any of its readings has one, then the count a strip's median prints. */
function statisticsOf(
  reader: Reader,
  view: ViewSpec,
  cells: readonly (Cell | undefined)[],
  plotForm: XForm
): ChartStatistic[] {
  const readings = cells.flatMap((cell) => (cell === undefined ? [] : [cell.reading]));
  const unit = (title: string, of: string) => (of === "" ? title : `${title} (${of})`);
  return [
    ...(plotForm === "number" ? [{ id: "x", label: unit(view.x.title, view.x.unit) }] : []),
    { id: "y", label: unit(view.y.title, view.y.unit) },
    ...(readings.some((reading) => reading.ciLow !== null || reading.ciHigh !== null)
      ? [
          { id: "ci_low", label: "Interval low" },
          { id: "ci_high", label: "Interval high" },
        ]
      : []),
    ...(reader.strip && cells.some((cell) => cell?.median === true && cell.reading.n !== null)
      ? [{ id: "n", label: "Count" }]
      : []),
  ];
}

/** A cell's value of statistic `id`; nothing where the series has no reading at the place. */
function cellValue(cell: Cell | undefined, id: string, compare: boolean): ChartValue {
  if (cell === undefined) return ABSENT;
  const { reading } = cell;
  const measured =
    compare && reading.measuredAt !== "" ? `measured ${timeLabel(reading.measuredMs)}` : null;
  switch (id) {
    case "x": {
      const x = cell.x;
      if (x === null)
        return { label: "no x reading", note: measured, state: "absent", value: null };
      if (x.value === null) {
        return { label: "x not measured", note: measured, state: "absent", value: null };
      }
      return {
        label: writeNumber(x.value, x.unit, x.bound),
        note: measured,
        state: "plain",
        value: x.value,
      };
    }
    case "y": {
      if (reading.value === null) {
        const reason = reading.absentReason;
        return {
          label: reason === "" ? "not measured" : `not measured: ${reason}`,
          note: reason === "" ? measured : reason,
          state: "absent",
          value: null,
        };
      }
      const notes = [
        reading.bound === null ? "" : BOUND_WORDS[reading.bound],
        reading.note,
        measured ?? "",
      ].filter((part) => part !== "");
      return {
        label: writeNumber(reading.value, reading.unit, reading.bound),
        note: notes.length === 0 ? null : notes.join("; "),
        state: "plain",
        value: reading.value,
      };
    }
    case "n":
      return reading.n === null
        ? ABSENT
        : {
            label: reading.nUnit === "" ? `${reading.n}` : `${reading.n} ${reading.nUnit}`,
            note: null,
            state: "plain",
            value: reading.n,
          };
    default: {
      const value = id === "ci_low" ? reading.ciLow : reading.ciHigh;
      return value === null
        ? { label: "no interval", note: null, state: "absent", value: null }
        : { label: writeNumber(value, reading.unit), note: null, state: "plain", value };
    }
  }
}

/** The change from the compare range's value to the window's, by statistic. */
function changeOf(
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
      const label =
        change === 0
          ? "no change"
          : `${change > 0 ? "+" : "\u2212"}${writeNumber(Math.abs(change), units[id] ?? "")}`;
      return [id, { label, note: null, state: "plain", value: change }];
    })
  );
}

/** A view as the spec declares it: its axes, reference and annotations, and its series. */
interface ViewSpec {
  readonly title: string | null;
  readonly ySeries: string;
  readonly xSeries: string | null;
  readonly x: PlotAxis;
  readonly y: PlotAxis;
  readonly reference: Reference | null;
  readonly annotations: readonly Annotation[];
}

function axisOf(
  value: unknown,
  fallback: string,
  unit: string,
  form: XForm | "value",
  steps: readonly string[]
): PlotAxis {
  const axis = specObject(value);
  const ticks = axis.ticks === "whole" || axis.ticks === "week" ? axis.ticks : null;
  const end = (field: string) =>
    form === "value" ? specNumber(axis[field]) : placeOf(form, steps, axis[field]);
  const label = specText(axis.label);
  return {
    log: axis.log === true,
    max: end("max"),
    min: end("min"),
    ticks,
    title: label === null || label === "" ? fallback : label,
    unit,
  };
}

function referenceOf(value: unknown): Reference | null {
  if (value === undefined) return null;
  const reference = specObject(value);
  const label = specText(reference.label) ?? "";
  if (reference.line === "y=x") return { label, type: "diagonal" };
  const by = specNumber(reference.from_start);
  if (by !== null) return { by, label, type: "from-start" };
  const y = specNumber(reference.y);
  if (y === null) return null;
  const yEnd = specNumber(reference.y_end);
  return yEnd === null
    ? { label, state: markState(reference.state), type: "rule", y }
    : { label, type: "band", y, yEnd };
}

function annotationsOf(
  value: unknown,
  form: XForm,
  steps: readonly string[],
  xMax: number | null
): Annotation[] {
  return specList(value).flatMap((item): Annotation[] => {
    const note = specObject(item);
    const at = placeOf(form, steps, note.x);
    if (at === null) return [];
    const detail = specText(note.detail);
    return [
      {
        at,
        bracket: note.bracket === true,
        detail,
        dot: note.dot !== false && note.bracket !== true,
        edge: form === "number" && xMax !== null && at === xMax && note.y === undefined,
        end: note.x_end === undefined ? null : placeOf(form, steps, note.x_end),
        label: specText(note.label) ?? "",
        state: markState(note.state),
        y: specNumber(note.y),
        yEnd: specNumber(note.y_end),
      },
    ];
  });
}

/** The unit of the first reading of `series` that names one. */
function unitOf(readings: readonly DeclaredReading[], series: string | null): string {
  return readings.find((reading) => reading.series === series && reading.unit !== "")?.unit ?? "";
}

/** The views the spec declares: each of its panels, each of its y options, else its one plot. */
function viewsOf(
  spec: DeclaredSpec,
  form: XForm,
  steps: readonly string[],
  readings: readonly DeclaredReading[]
): { layout: Plot["layout"]; views: ViewSpec[] } {
  const x = specObject(spec.x);
  const xSeries = specText(x.series);
  const timeTitle = (fallback: string) => (form === "time" ? MEASURED_TIME : fallback);
  const view = (
    y: unknown,
    xAxis: unknown,
    viewXSeries: string | null,
    reference: unknown,
    annotations: unknown,
    title: string | null
  ): ViewSpec => {
    const ySeries = specText(specObject(y).series) ?? "";
    const xAxisOf = axisOf(
      xAxis,
      viewXSeries ?? timeTitle(""),
      unitOf(readings, viewXSeries),
      form,
      steps
    );
    return {
      annotations: annotationsOf(annotations, form, steps, xAxisOf.max),
      reference: referenceOf(reference),
      title,
      x: xAxisOf,
      xSeries: viewXSeries,
      y: axisOf(y, ySeries, unitOf(readings, ySeries), "value", steps),
      ySeries,
    };
  };
  const panels = specList(spec.panels);
  if (panels.length > 0) {
    return {
      layout: "panels",
      views: panels.map((item) => {
        const panel = specObject(item);
        const panelX = specObject(panel.x);
        const own = panel.y !== undefined;
        return view(
          own ? panel.y : spec.y,
          panelX,
          specText(panelX.series),
          own ? undefined : spec.reference,
          panel.annotations,
          specText(panel.title) ?? specText(panelX.label)
        );
      }),
    };
  }
  const options = specList(spec.y_options);
  if (options.length > 0) {
    return {
      layout: "options",
      views: options.map((item) => {
        const option = specObject(item);
        return view(option, x, xSeries, option.reference, spec.annotations, specText(option.label));
      }),
    };
  }
  return {
    layout: "one",
    views: [view(spec.y, x, xSeries, spec.reference, spec.annotations, null)],
  };
}

/** The items `items` lists under each x of an xy over time: each reading of its series with a
 *  value names one part (its col) of one item (its row), under the x of its measured time, in the
 *  group its row group names, with its note; a newer reading of one x, row and col takes the older
 *  one's place. */
function itemsOf(
  spec: DeclaredSpec,
  readings: readonly DeclaredReading[],
  tones: ReadonlyMap<string, Tone>
): PlotItems | null {
  const items = specObject(spec.items);
  const series = specText(items.series);
  if (series === null) return null;
  const drawn = readings.filter(
    (reading) => reading.series === series && reading.range === "window" && reading.value !== null
  );
  if (drawn.length === 0) return null;
  const newest = newestByCell(
    drawn,
    (reading) => `${reading.measuredMs}\n${reading.row}\n${reading.col}`
  );
  const places = new Map<number, Map<string, DeclaredReading[]>>();
  for (const reading of newest.values()) {
    const place = places.get(reading.measuredMs) ?? new Map<string, DeclaredReading[]>();
    place.set(reading.row, [...(place.get(reading.row) ?? []), reading]);
    places.set(reading.measuredMs, place);
  }
  const groupOrder = [...tones.keys()];
  const rank = (group: string) =>
    groupOrder.includes(group) ? groupOrder.indexOf(group) : groupOrder.length;
  return {
    outlines: specText(items.outlines),
    partsLabel: specText(items.parts_label) ?? "Parts",
    places: [...places.entries()]
      .sort((a, b) => a[0] - b[0])
      .map(([at, rows]) => ({
        at,
        items: [...rows.entries()]
          .map(([title, parts]): PlotItem => {
            const first = parts[0] as DeclaredReading;
            return {
              group: first.rowGroup,
              note: first.note,
              parts: parts.map((part) => part.col).filter((col) => col !== ""),
              title,
              tone: tones.get(first.rowGroup) ?? "other",
            };
          })
          .sort((a, b) => rank(a.group) - rank(b.group)),
        label: timeLabel(at),
        x: new Date(at).toISOString(),
      })),
  };
}

/** "Ranges: ..." under a chart that draws a supplied interval (an xy, a band, a bar, an interval,
 *  a grid): the level of each, once, then the words `interval_label` gives an interval with no
 *  level ("recorded interval" without them); the levels left out where `interval_level_in` names
 *  the text that states them. */
export function rangesLine(
  spec: DeclaredSpec,
  readings: readonly DeclaredReading[]
): string | null {
  const intervals = readings.filter((reading) => reading.ciLow !== null && reading.ciHigh !== null);
  if (intervals.length === 0) return null;
  const label = specText(spec.interval_label);
  const levelled = spec.interval_level_in === undefined;
  const levels = levelled
    ? [
        ...new Set(
          intervals.flatMap((reading) =>
            reading.ciLevel === null ? [] : [`${Math.round(reading.ciLevel * 100)}-in-100`]
          )
        ),
      ]
    : [];
  const unlevelled = intervals.some((reading) => reading.ciLevel === null) || !levelled;
  const tail = unlevelled ? (label ?? (levelled ? "recorded interval" : null)) : null;
  if (levels.length === 0 && tail === null) return null;
  return `Ranges: ${[levels.join(", "), tail].filter((part) => part !== null && part !== "").join("; ")}.`;
}

/** The series an xy or a band reads: each y series of its views, each x series, and the series
 *  of its items. */
export function plotReads(spec: DeclaredSpec): ChartRead[] {
  const names = [
    specObject(spec.y).series,
    specObject(spec.x).series,
    ...specList(spec.y_options).map((option) => specObject(option).series),
    ...specList(spec.panels).flatMap((panel) => [
      specObject(specObject(panel).x).series,
      specObject(specObject(panel).y).series,
    ]),
    specObject(spec.items).series,
  ].filter((name): name is string => typeof name === "string");
  return [...new Set(names)].map((series) => ({ series }));
}

/**
 * The model of an `xy` or a `band` (plan D10) from its spec and the readings of its series:
 * - over time (no `x.series`), each reading is a point at its measured time, and each group (its
 *   col, row group or row, as `group` names, else one) joins its points in time; the compare
 *   range's readings stand at the instant their shift onto the window's axis gives them;
 * - against an x series, each row and col's newest reading is a point at the newest x reading of
 *   its row (and col, where the x series has more than one); a reading with no x reading has no
 *   place, and draws in the strip `missing_x` asks for or is named as not plotted; the compare
 *   range's points are its own readings joined the same way, at the same row;
 * - with `x.steps`, each row is a line through its newest reading at each step (a strip, with
 *   `points: "only"`, draws each step's readings as dots, and its median rows as bars);
 * - `y_options` and `panels` each draw a view of their own, whose series the model holds after
 *   the view before's; a mock-up's points stand for readings where the chart has no reading with a
 *   value.
 * Each series is a group, in the order its colour's place gives it (`groupTones`); its points are
 * every place of the chart, in the order of their x.
 */
export function plotModel(kind: PlotKind, spec: DeclaredSpec, input: DeclaredInput): PlotModel {
  const x = specObject(spec.x);
  const xSteps = specList(x.steps).filter((step): step is string => typeof step === "string");
  const ownForm: XForm =
    xSteps.length > 0 ? "category" : specText(x.series) === null ? "time" : "number";
  const pointsField = spec.points === "end" || spec.points === "only" ? spec.points : null;
  const medians = specObject(spec.medians);
  const medianRows = specList(medians.rows).filter((row): row is string => typeof row === "string");
  const group =
    spec.group === "col" || spec.group === "row_group" || spec.group === "row" ? spec.group : null;
  const drawnSeries = new Set(
    [
      specObject(spec.y).series,
      ...specList(spec.y_options).map((o) => specObject(o).series),
    ].concat(specList(spec.panels).map((panel) => specObject(specObject(panel).y).series))
  );
  const measured = input.readings.filter((reading) => drawnSeries.has(reading.series));

  // A mock-up draws where none of the chart's y series holds a reading with a value.
  const mockup = spec.mockup === undefined ? null : specObject(spec.mockup);
  const mocked = mockup !== null && !measured.some((reading) => reading.value !== null);
  const shaped = mocked && (mockup.shape === "line" || mockup.shape === "bar");
  const shapeSteps =
    shaped && mockup.shape === "bar"
      ? [...new Set(specList(mockup.points).map((point) => String(specObject(point).x)))]
      : [];
  const form: XForm = shaped ? (mockup.shape === "line" ? "number" : "category") : ownForm;
  const steps = shaped ? shapeSteps : xSteps;
  const strip = kind === "xy" && form === "category" && !shaped && pointsField === "only";

  const reader: Reader = {
    form,
    group: strip || form === "category" ? null : group,
    medianRows,
    overall: specText(medians.overall),
    shift:
      input.compare === null ? 0 : Date.parse(input.window.from) - Date.parse(input.compare.from),
    steps,
    strip,
  };

  // A shape mock-up draws on axes of its own, which name no scale.
  const declared = viewsOf(spec, form, steps, input.readings);
  const mockAxis = (field: string): PlotAxis => {
    const axis = specObject(mockup?.[field]);
    const unit = specText(axis.unit) ?? "";
    return {
      log: false,
      max: null,
      min: null,
      ticks: null,
      title: `${specText(axis.label) ?? field}${unit === "" ? "" : ` (${unit})`} (mock-up, no scale)`,
      unit: "",
    };
  };
  const layout = shaped ? "one" : declared.layout;
  const viewSpecs = shaped
    ? [{ ...(declared.views[0] as ViewSpec), x: mockAxis("x"), xSeries: null, y: mockAxis("y") }]
    : declared.views;

  // Each view's cells of each range, and its readings the chart does not plot.
  const notes: string[] = [];
  const viewCells = viewSpecs.map((view, index) => {
    if (mocked && index === 0 && mockup !== null) {
      return { compare: new Map() as Cells, window: mockCells(reader, mockup, form, steps) };
    }
    const ys = input.readings.filter((reading) => reading.series === view.ySeries);
    const xs = input.readings.filter((reading) => reading.series === view.xSeries);
    const window = cellsOf(reader, "window", ys, xs);
    const compare =
      input.compare === null
        ? { cells: new Map() as Cells, notAStep: 0 }
        : cellsOf(reader, "compare", ys, xs);
    if (window.notAStep > 0) {
      notes.push(`${plural(window.notAStep, "reading")} not plotted: its col is no step.`);
    }
    return { compare: compare.cells, window: window.cells };
  });

  // Each group's colour, by its place among the groups of every view, in the order of their first
  // readings; with color row_group, of the row groups, which each row's line takes.
  const byRowGroup = kind === "xy" && spec.color === "row_group" && reader.group === "row";
  const firstAt = new Map<string, number>();
  const seriesFirstAt = new Map<string, number>();
  const rowGroupOf = new Map<string, string>();
  const earliest = (map: Map<string, number>, name: string, at: number) =>
    map.set(name, Math.min(map.get(name) ?? at, at));
  for (const cells of viewCells) {
    for (const range of [cells.compare, cells.window]) {
      for (const [name, places] of range) {
        for (const cell of places.values()) {
          if (byRowGroup && !rowGroupOf.has(name)) rowGroupOf.set(name, cell.reading.rowGroup);
          const at = mocked ? 0 : cell.reading.measuredMs;
          earliest(firstAt, byRowGroup ? cell.reading.rowGroup : name, at);
          earliest(seriesFirstAt, name, at);
        }
      }
    }
  }
  const toneNames = strip
    ? steps.slice()
    : [...firstAt.entries()].sort((a, b) => a[1] - b[1]).map(([name]) => name);
  const tones = groupTones(spec, toneNames);
  const toneOf = new Map(tones.map(({ name, tone }) => [name, tone]));
  const toneRank = (name: string) => tones.findIndex((entry) => entry.name === name);

  // Every place of the chart, in the order of x, so each series' points line up with the table's
  // rows.
  const summaryNames =
    kind === "xy" && form === "number" ? specList(specObject(spec.summary).cols) : [];
  const summaries = viewCells.map((cells) =>
    summaryNames
      .filter((name): name is string => typeof name === "string")
      .map((name) => ({
        compare: summaryCells(name, cells.compare),
        name,
        window: summaryCells(name, cells.window),
      }))
  );
  const places = new Map<string, { at: number; label: string }>();
  const place = (cell: Cell) => {
    const held = places.get(cell.key);
    const at = cell.at ?? Number.POSITIVE_INFINITY;
    if (held === undefined || at < held.at) places.set(cell.key, { at, label: cell.label });
  };
  viewCells.forEach((cells, index) => {
    for (const range of [cells.window, cells.compare]) {
      for (const cellsOfGroup of range.values())
        for (const cell of cellsOfGroup.values()) place(cell);
    }
    for (const summary of summaries[index] ?? []) {
      for (const cell of [...summary.window.values(), ...summary.compare.values()]) place(cell);
    }
  });
  const keys = [...places.entries()]
    .sort((a, b) => a[1].at - b[1].at || (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0))
    .map(([key, { label }]) => ({ key, label }));

  const comparing = input.compare !== null;
  const rings = specObject(spec.rings);
  const ringGroups = specList(rings.groups);
  const listedOr = (value: unknown, row: string, overallOnly: boolean) =>
    value === true ? !overallOnly || row === reader.overall : specList(value).includes(row);
  const fromStart = (viewSpec: ViewSpec) =>
    viewSpec.reference?.type === "from-start" ? viewSpec.reference.by : null;

  const series: PlotSeries[] = [];
  viewSpecs.forEach((view, index) => {
    const cells = viewCells[index] as { window: Cells; compare: Cells };
    const names = [...new Set([...cells.window.keys(), ...cells.compare.keys()])].sort(
      (a, b) =>
        toneRank(byRowGroup ? (rowGroupOf.get(a) ?? "") : a) -
          toneRank(byRowGroup ? (rowGroupOf.get(b) ?? "") : b) ||
        (seriesFirstAt.get(a) ?? 0) - (seriesFirstAt.get(b) ?? 0)
    );
    const groups = [
      ...names.map((name) => ({
        compare: cells.compare.get(name) ?? new Map<string, Cell>(),
        name,
        role: (kind === "xy" && reader.group === "row" && medianRows.length > 0
          ? medianRows.includes(name)
            ? "median"
            : "run"
          : "group") as SeriesRole,
        window: cells.window.get(name) ?? new Map<string, Cell>(),
      })),
      ...(summaries[index] ?? []).map((summary) => ({ ...summary, role: "summary" as const })),
    ];
    for (const { compare, name, role, window } of groups) {
      const ownCells = [...window.values(), ...compare.values()];
      const statistics = statisticsOf(reader, view, ownCells, form);
      const units = Object.fromEntries(
        statistics.map(({ id }) => [id, id === "x" ? view.x.unit : id === "n" ? "" : view.y.unit])
      );
      // A place the series has no reading at, in either range, shares one record of nothing, so a
      // chart of many groups costs no object per value where it draws none.
      const nothing = Object.fromEntries(statistics.map(({ id }) => [id, ABSENT]));
      const points = keys.map(({ key, label }): PlotPoint => {
        const own = window.get(key);
        const then = compare.get(key);
        const empty = own === undefined && then === undefined;
        const values = empty
          ? nothing
          : Object.fromEntries(statistics.map(({ id }) => [id, cellValue(own, id, false)]));
        const compareValues = !comparing
          ? null
          : empty
            ? nothing
            : Object.fromEntries(statistics.map(({ id }) => [id, cellValue(then, id, true)]));
        return {
          at: own?.at ?? null,
          bound: own?.reading.bound ?? null,
          change:
            compareValues === null
              ? null
              : empty
                ? nothing
                : changeOf(values, compareValues, units),
          compare: compareValues,
          compareAt: then?.at ?? null,
          compareBound: then?.reading.bound ?? null,
          compareHeld: then !== undefined,
          held: own !== undefined,
          label,
          median: own?.median ?? false,
          overall: own?.overall ?? false,
          partial: false,
          rowGroup: own?.reading.rowGroup ?? then?.reading.rowGroup ?? "",
          values,
          x: key,
        };
      });
      // A line's target from its first step, drawn at its last; none where either has no value.
      const by = fromStart(view);
      const firstValue = points.find((point) => point.held && point.at === 0)?.values.y?.value;
      const lastHeld = points.some((point) => point.held && point.at === steps.length - 1);
      const target =
        form === "category" && !strip && by !== null && lastHeld && typeof firstValue === "number"
          ? firstValue + by
          : null;
      const tone =
        role === "summary"
          ? (toneOf.get(name) ??
            toneOf.get(names.find((candidate) => candidate.split(" \u00b7 ")[0] === name) ?? "") ??
            "other")
          : role === "median" && name === reader.overall
            ? "overall"
            : (toneOf.get(byRowGroup ? (rowGroupOf.get(name) ?? "") : name) ?? "other");
      const unit = ownCells[0]?.reading.unit ?? view.y.unit;
      const label = [
        layout === "one" ? null : view.title,
        role === "summary" ? `median of ${name}` : name === "" ? null : name,
      ]
        .filter((part): part is string => part !== null && part !== "")
        .join(" \u00b7 ");
      // The series' name in the page (`data-series`): its y series, a panel's x series after it,
      // and its group, so every series of one chart has a name of its own.
      const named = role === "summary" ? `median of ${name}` : name;
      series.push({
        banded: role === "median" && listedOr(medians.band, name, true),
        dataNote: null,
        fromStart: target,
        group: name,
        headline: [],
        hollow: ringGroups.includes(name),
        joined: role === "median" && listedOr(medians.join, name, false),
        label: label === "" ? (view.title ?? view.y.title) : label,
        name: `${view.ySeries}${layout === "panels" ? `@${view.xSeries ?? index}` : ""}${
          named === "" ? "" : ` \u00b7 ${named}`
        }`,
        noun: "readings",
        points,
        repositoryNote: null,
        role,
        statistics,
        target: null,
        tone,
        unit,
        view: index,
        window: {
          change: null,
          compare: null,
          label: "Window",
          partial: false,
          values: {},
          x: "window",
        },
      });
    }
  });

  const views: PlotView[] = viewSpecs.map((view, index) => {
    const own = series.filter((one) => one.view === index);
    const units = new Set(
      own.flatMap((one) =>
        one.role === "summary"
          ? []
          : [...(viewCells[index]?.window.get(one.group)?.values() ?? [])].map(
              (cell) => cell.reading.unit
            )
      )
    );
    units.delete("");
    const drawn = own.flatMap((one) =>
      one.role === "summary" ? [] : one.points.filter((point) => point.held)
    );
    const ys = drawn.flatMap((point) => {
      const value = point.values.y?.value ?? null;
      return value === null ? [] : [value];
    });
    const where = viewSpecs.length > 1 && view.title !== null ? ` (${view.title})` : "";
    const unit = view.y.unit === "" ? "" : ` ${view.y.unit}`;
    if (view.y.max !== null) {
      const above = ys.filter((value) => value > (view.y.max as number)).length;
      if (above > 0)
        notes.push(`\u25b2 ${plural(above, "reading")} above ${view.y.max}${unit}${where}`);
    }
    if (view.y.min !== null) {
      const below = ys.filter((value) => value < (view.y.min as number)).length;
      if (below > 0)
        notes.push(`\u25bc ${plural(below, "reading")} below ${view.y.min}${unit}${where}`);
    }
    if (view.y.log) {
      const zero = ys.filter((value) => value <= 0).length;
      if (zero > 0) {
        notes.push(`${plural(zero, "reading")} at or below 0 not drawn on the log axis${where}.`);
      }
    }
    if (form === "number" && !mocked) {
      const unplaced = drawn.filter((point) => point.at === null).length;
      if (unplaced > 0 && !(spec.missing_x === "band")) {
        notes.push(
          `${plural(unplaced, "reading")} not plotted: no reading of ${view.xSeries ?? "x"} in its row${where}.`
        );
      }
    }
    return {
      annotations: view.annotations,
      mixedUnits: units.size > 1 ? [...units].sort() : [],
      reference: view.reference,
      title: view.title,
      x: view.x,
      y: view.y,
    };
  });

  const everyReading = viewCells.flatMap((cells) =>
    [cells.window, cells.compare].flatMap((range) =>
      [...range.values()].flatMap((places) => [...places.values()].map((cell) => cell.reading))
    )
  );
  const ranges = rangesLine(spec, everyReading);
  const bounds = (["<=", ">="] as const).filter((bound) =>
    everyReading.some((reading) => reading.bound === bound)
  );
  const boundLines = bounds.map((bound) =>
    bound === ">="
      ? "Lower bound: the true value is at least this."
      : "Upper bound: the true value is at most this."
  );
  const ringLabel = specText(rings.label);
  const medianLabel = specText(medians.label);
  const keyNotes = [
    ...(ranges === null ? [] : [ranges]),
    ...boundLines,
    ...(ringLabel === null || ringGroups.length === 0 ? [] : [`\u25cb ${ringLabel}`]),
    ...(medianLabel === null || medianRows.length === 0 ? [] : [`\u25ac ${medianLabel}`]),
    ...(summaryNames.length === 0
      ? []
      : [
          "\u25c6 each named group's median at each x, with its 95% interval where the x holds 6 readings or more",
        ]),
    ...notes,
  ];

  const key: KeyEntry[] = (byRowGroup ? tones : tones.filter(({ name }) => name !== ""))
    .filter(({ name }) => !strip && firstAt.has(name))
    .map(({ name, tone }) => ({ hollow: ringGroups.includes(name), name, tone }));
  const legend = strip
    ? "none"
    : spec.legend === "direct"
      ? "direct"
      : key.length >= 2 && key.length <= 10
        ? "key"
        : "none";
  const curve = spec.curve === "step" || spec.curve === "linear" ? spec.curve : null;
  const line: Plot["line"] =
    pointsField === "only" || strip
      ? "none"
      : form === "number" && kind === "xy"
        ? (curve ?? "none")
        : curve === "step"
          ? "step"
          : "linear";
  const secondAxis = specObject(spec.y2);
  const scale = specNumber(secondAxis.scale);
  const mockupBy = mocked ? (specText(mockup.submitted_by) ?? "its owner") : null;
  const mockupAt = mocked ? (specText(mockup.submitted_at) ?? "") : "";

  const axis: ChartAxis =
    form === "time"
      ? { label: "Measured (UTC)", many: "measured times", one: "measured time" }
      : form === "number" || strip
        ? {
            label: strip ? "Row" : "Point",
            many: strip ? "rows" : "points",
            one: strip ? "row" : "point",
          }
        : { label: specText(x.label) || "Step", many: "steps", one: "step" };

  return {
    axis,
    comparable: true,
    compareRange: input.compare,
    kind,
    name: declaredChartName(input.slug, spec),
    plot: {
      form,
      height: specNumber(spec.height) ?? DEFAULT_HEIGHT,
      items: kind === "xy" && form === "time" ? itemsOf(spec, input.readings, toneOf) : null,
      joinRowGroups: strip && spec.join === "row_group",
      key,
      kind,
      layout,
      legend,
      line,
      marks: strip ? "only" : (pointsField ?? "all"),
      medians: kind === "xy" && medianRows.length > 0,
      missingX: form === "number" && spec.missing_x === "band",
      mockup:
        mockupBy === null
          ? null
          : `Mock-up: nothing is measured yet; ${mockupBy} drew it${mockupAt === "" ? "" : ` on ${mockupAt.slice(0, 10)}`}.`,
      noScale: shaped,
      notStartedGrey: spec.not_started_outline === "grey",
      notes: keyNotes,
      secondAxis:
        kind === "xy" && scale !== null
          ? {
              label: specText(secondAxis.label) ?? "",
              scale,
              unit: specText(secondAxis.unit) ?? "",
            }
          : null,
      steps,
      strip,
      views,
      zoom: spec.zoom === true && form !== "category",
    },
    series,
    title: specText(spec.title) ?? specText(spec.id) ?? "",
  };
}
