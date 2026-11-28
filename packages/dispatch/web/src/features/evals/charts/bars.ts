// The model of a declared chart that draws its readings by category or as stacks: a `bar`, an
// `interval` and a `stacked-bar` (dispatch://INFRA-2918/artifact/plan-md, D10; the validator's
// rules, internal/dispatch/evals/charts). Each group of the chart (a col of a grouped bar, a part of
// a stack) is one series of the model (D4), and every series holds a point at every place the
// chart's table has a row for: each category, each measured date of a stack over time, or each
// week and row of a stack by week; a place a series has no reading at reads "no data". The compare
// range's readings are each point's compare values: by category, each category's newest reading
// of the compare range; over time, at the date the compare range's shift onto the window's axis
// puts them; so a value's change is its value less the compare range's there. Pure.
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
import { rangesLine } from "./plot";
import { type Bound, type DeclaredReading, newestByCell } from "./readings";

export type BarsKind = "bar" | "interval" | "stacked-bar";

/**
 * How a chart lays out its places:
 * - `upright`: one bar or point per category along x, the value axis up; a grouped bar or
 *   interval draws a cluster per row, one mark per col;
 * - `horizontal`: one row per category, the value along x (a bar with `orientation`);
 * - `facet`: a table of short bars, one row per row and one column per col (a bar with `facet`);
 * - `stack`: one stacked bar per place, one segment per group (a stacked bar by row or by time);
 * - `area`: a stacked area over time (a stacked bar with `layout: "area"`).
 */
export type BarsLayout = "upright" | "horizontal" | "facet" | "stack" | "area";

/** What marks a value: a bar, an interval's point, a dot, or a dot for each unit of its total. */
export type BarsMark = "bar" | "point" | "dot" | "dots";

/** One place of the chart, a row of its table. */
export interface BarsPlace {
  /** Its name in the page (`data-x`): a category, a UTC date, or a week and a row. */
  readonly x: string;
  readonly label: string;
  /** On a stack over time, the first moment of its UTC date in epoch milliseconds. */
  readonly at: number | null;
  /** With `frames`, the Monday (YYYY-MM-DD) its week starts on. */
  readonly week: string | null;
  /** The category it draws, a row or a col of the readings ("" on a stack over time). */
  readonly category: string;
  /** On a horizontal bar with `groups`, the heading above it, the first row of its group. */
  readonly heading: string | null;
  /** On a facet table, the line `row_subtitles` writes under its name. */
  readonly subtitle: string | null;
  /** On a facet table, the unit of its readings where its name does not say it, else "". */
  readonly unit: string;
  /** On a facet table, its readings are in more than one unit: each value prints its unit and
   *  draws no bar. */
  readonly mixed: boolean;
}

/** A reference: a rule at y, in a state's colour where it names one, or the band from y to yEnd. */
export type BarsReference =
  | {
      readonly type: "rule";
      readonly y: number;
      readonly label: string;
      readonly state: MarkState | null;
    }
  | { readonly type: "band"; readonly y: number; readonly yEnd: number; readonly label: string };

/** A labelled mark: at a category (`category`, which a place of the chart draws) or at a time
 *  (`at`), a dot at y where it gives one, else a label over its category or a dashed rule at its
 *  time. */
export interface BarsAnnotation {
  readonly category: string | null;
  readonly at: number | null;
  readonly y: number | null;
  readonly label: string;
  readonly detail: string | null;
  readonly state: MarkState | null;
}

/** One entry of `value_states`: the state of each value from `from` up to the next entry's. */
export interface ValueState {
  readonly state: MarkState;
  readonly label: string;
  readonly from: number | null;
}

/** One week of a stacked bar by row with `frames`: its places, by their index in `places`. */
export interface BarsFrame {
  readonly week: string;
  readonly label: string;
  readonly places: readonly number[];
}

/** A column right of a dot chart's plot (`row_columns`): its header, the statistic of the chart's
 *  one series that holds its cells, and whether they print a number or a note. */
export interface BarsColumn {
  readonly header: string;
  readonly statistic: string;
  readonly field: "value" | "note";
}

/** The value axis: its title and unit, log scale, whole-number ticks and the ends it fixes; a
 *  share axis from 0 to 100% for a horizontal bar against its totals. */
export interface BarsValueAxis {
  readonly title: string;
  readonly unit: string;
  readonly log: boolean;
  readonly whole: boolean;
  readonly min: number | null;
  readonly max: number | null;
  readonly share: boolean;
}

/** Everything about the chart's drawing that is not a value. */
export interface Bars {
  readonly kind: BarsKind;
  readonly layout: BarsLayout;
  /** A bar or an interval of clusters: one cluster per row, one mark per col. */
  readonly grouped: boolean;
  readonly mark: BarsMark;
  /** A stack over time: its places are measured dates on a time axis, not categories. */
  readonly overTime: boolean;
  readonly places: readonly BarsPlace[];
  /** The title of the axis of places: a category axis takes one only from `x.label`. */
  readonly placeTitle: string;
  readonly valueAxis: BarsValueAxis;
  /** Each value stands against its total (`of`): `19 of 28 tasks`. */
  readonly of: boolean;
  /** The plot's height in pixels; on a horizontal bar, only where the spec declares one. */
  readonly height: number | null;
  readonly reference: BarsReference | null;
  readonly annotations: readonly BarsAnnotation[];
  /** The key over the plot: each group's colour, or each value state's (`valueStates`). */
  readonly legend: "key" | "direct" | "states" | "none";
  readonly key: readonly { readonly name: string; readonly tone: Tone }[];
  readonly valueStates: readonly ValueState[];
  /** An interval prints each point's value beside it (`labels: "value"`). */
  readonly labels: boolean;
  /** A horizontal bar writes each row's note in its row (`row_notes`). */
  readonly rowNotes: boolean;
  readonly columns: readonly BarsColumn[];
  readonly plotHeader: string | null;
  readonly frames: readonly BarsFrame[] | null;
  /** On a facet table, the parts `stack_order` names, in its order, each with the state `states`
   *  gives it, which its segments take before their row's and their col's. */
  readonly parts: readonly { readonly name: string; readonly state: MarkState | null }[];
  /** On a stacked area, a tick each Monday (`x.ticks: "week"`). */
  readonly weekTicks: boolean;
  /** The units its readings are in, where they are in more than one: it then draws no number. */
  readonly mixedUnits: readonly string[];
  /** The lines under the plot: its ranges' levels, its bounds, and the readings it does not draw. */
  readonly notes: readonly string[];
  /** Where the chart draws its owner's mock-up, the line that says so. */
  readonly mockup: string | null;
  /** A shape mock-up's drawing of its own: its value axis has no tick values. */
  readonly noScale: boolean;
  readonly notStartedGrey: boolean;
}

export interface BarsPoint extends ChartPoint {
  /** Whether the series has a reading of the window here, and of the compare range. */
  readonly held: boolean;
  readonly compareHeld: boolean;
  readonly bound: Bound | null;
  readonly compareBound: Bound | null;
  /** The colour of its own mark where its value, its category or its row names one; else its
   *  series'. */
  readonly tone: Tone | null;
  /** Its reading's note (`row_notes`, a facet cell's note). */
  readonly note: string;
  /** Why its reading has no value, where it has none. */
  readonly reason: string;
}

export interface BarsSeries extends ChartSeries {
  /** Its group's name, "" for the one series of a chart with no groups. */
  readonly group: string;
  readonly tone: Tone;
  readonly points: readonly BarsPoint[];
}

export interface BarsModel extends ChartModel {
  readonly bars: Bars;
  readonly series: readonly BarsSeries[];
}

export function isBarsModel(model: ChartModel): model is BarsModel {
  return "bars" in model;
}

/** A value of nothing: no reading of the series at the place. */
const ABSENT: ChartValue = { label: NO_DATA, note: null, state: "absent", value: null };

/** A chart's height where its spec names none, in pixels. */
const DEFAULT_HEIGHT = 340;

/** A facet table draws at most this many rows. */
const FACET_ROWS = 5;

/** What joins a facet row's name and its part's (`<row> · <part>`), and a week and a row. */
const JOIN = " \u00b7 ";

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const DAY = 24 * 60 * 60 * 1000;

const BOUND_WORDS: Readonly<Record<Bound, string>> = {
  "<=": "upper bound: the true value is at most this",
  ">=": "lower bound: the true value is at least this",
};

function plural(count: number, one: string): string {
  return `${count} ${one}${count === 1 ? "" : "s"}`;
}

/** `value` as a chart writes a number of `unit`, its bound's sign before it. */
export function writeNumber(value: number, unit: string, bound: Bound | null = null): string {
  const text = writerOf(unit, "value")(value)(0);
  return bound === null ? text : `${bound === ">=" ? "\u2265" : "\u2264"} ${text}`;
}

/** The UTC date of an instant, YYYY-MM-DD. */
function dayOf(ms: number): string {
  return new Date(ms).toISOString().slice(0, 10);
}

/** The Monday (YYYY-MM-DD) of the week, Monday 00:00 UTC to the next, that holds an instant. */
function weekOf(ms: number): string {
  const days = Math.floor(ms / DAY);
  // The epoch began on a Thursday: a day's place in its week from Monday is (days + 3) mod 7.
  return dayOf((days - ((days + 3) % 7)) * DAY);
}

function weekLabel(week: string): string {
  const day = new Date(`${week}T00:00:00Z`);
  return `Week of ${day.getUTCDate()} ${MONTHS[day.getUTCMonth()]}`;
}

/** A spec's time as its first moment (a UTC date) or its instant, else null. */
function timeOf(value: unknown): number | null {
  if (typeof value !== "string") return null;
  const ms = Date.parse(/^\d{4}-\d{2}-\d{2}$/.test(value) ? `${value}T00:00:00Z` : value);
  return Number.isNaN(ms) ? null : ms;
}

/** What the chart's readings draw as: its layout, whether its marks cluster, whether its places
 *  are dates or weeks, and which field names its groups and its places. */
interface Form {
  readonly layout: BarsLayout;
  readonly grouped: boolean;
  readonly overTime: boolean;
  readonly frames: boolean;
  /** One series, a mark per category (a bar or an interval with no clusters and no facet). */
  readonly single: boolean;
  /** One series whose categories are cols (`group: "col"`), each in its own colour. */
  readonly byCol: boolean;
  /** On a stack over time, the field each segment is a value of. */
  readonly segment: "col" | "row_group";
  readonly facet: boolean;
}

/** The form of a chart; a mock-up's points draw in one frame, and a shape mock-up's on categories
 *  of their own. */
function formOf(kind: BarsKind, spec: DeclaredSpec, mocked: boolean, shaped: boolean): Form {
  const groupCol = spec.group === "col";
  if (kind === "stacked-bar") {
    const byRow = shaped || spec.stack === "row";
    return {
      byCol: false,
      facet: false,
      frames: byRow && !mocked && spec.frames === "week",
      grouped: false,
      layout: !byRow && spec.layout === "area" ? "area" : "stack",
      overTime: !byRow,
      segment: spec.group === "row_group" ? "row_group" : "col",
      single: false,
    };
  }
  // A shape mock-up's points are categories, each series a col of a cluster.
  const grouped = shaped || (spec.layout === "grouped" && groupCol);
  const facet = !shaped && kind === "bar" && spec.facet === "row" && groupCol;
  const horizontal = !shaped && kind === "bar" && spec.orientation === "horizontal";
  return {
    byCol: !grouped && !facet && groupCol,
    facet,
    frames: false,
    grouped,
    layout: facet ? "facet" : horizontal ? "horizontal" : "upright",
    overTime: false,
    segment: "col",
    single: !grouped && !facet,
  };
}

/** Where a reading stands: its place, its group and, on a facet table, its part. */
interface Keyed {
  readonly place: string;
  readonly group: string;
  readonly part: string;
  readonly at: number | null;
  readonly week: string | null;
  readonly category: string;
}

function keyOf(
  form: Form,
  parts: readonly string[],
  reading: DeclaredReading,
  shift: number
): Keyed {
  if (form.overTime) {
    const day = dayOf(reading.measuredMs + shift);
    return {
      at: Date.parse(`${day}T00:00:00Z`),
      category: "",
      group: form.segment === "row_group" ? reading.rowGroup : reading.col,
      part: "",
      place: day,
      week: null,
    };
  }
  if (form.frames) {
    const week = weekOf(reading.measuredMs + shift);
    return {
      at: null,
      category: reading.row,
      group: reading.col,
      part: "",
      place: `${week}${JOIN}${reading.row}`,
      week,
    };
  }
  if (form.single) {
    const category = form.byCol ? reading.col : reading.row;
    return { at: null, category, group: "", part: "", place: category, week: null };
  }
  // A facet row named `<row> · <part>`, where stack_order names `<part>`, is that part of `<row>`.
  const split = reading.row.lastIndexOf(JOIN);
  const part = split < 0 ? "" : reading.row.slice(split + JOIN.length);
  if (form.facet && split > 0 && parts.includes(part)) {
    const row = reading.row.slice(0, split);
    return { at: null, category: row, group: reading.col, part, place: row, week: null };
  }
  return {
    at: null,
    category: reading.row,
    group: reading.col,
    part: "",
    place: reading.row,
    week: null,
  };
}

/** One range's readings at one place of one group: its own reading, and its parts' readings on a
 *  facet table. */
interface Slot {
  own: DeclaredReading | null;
  readonly parts: Map<string, DeclaredReading>;
}

type Range = "window" | "compare";

/** One range's slots, by group and then by place. */
type Slots = Map<string, Map<string, Slot>>;

/** A mock-up point as a reading of the chart's y series, standing where the chart's form puts its
 *  x: at a category (a row, or with `group: "col"` a col) and its group, or at its date. */
function mockReading(
  form: Form,
  point: DeclaredSpec,
  index: number,
  ciLevel: number | null,
  ySeries: string
): DeclaredReading | null {
  const group = specText(point.series) ?? "";
  const x = point.x;
  const at = form.overTime ? timeOf(x) : 0;
  if (at === null) return null;
  const category = typeof x === "number" ? String(x) : (specText(x) ?? "");
  const ciHigh = specNumber(point.ci_high);
  return {
    absentReason: "",
    attributes: {},
    bound: null,
    ciHigh,
    ciLevel: ciHigh === null ? null : ciLevel,
    ciLow: specNumber(point.ci_low),
    col: form.byCol ? category : group,
    id: `mockup-${index}`,
    links: [],
    measuredAt: form.overTime ? new Date(at).toISOString() : "",
    measuredMs: at,
    n: specNumber(point.n),
    nUnit: "",
    note: "mock-up",
    range: "window",
    recordedAt: "",
    replaces: "",
    row: form.overTime ? "" : category,
    rowGroup: group,
    rowKind: "mockup",
    series: ySeries,
    source: "",
    unit: "",
    value: specNumber(point.y),
  };
}

/** The state `value_states` gives a value: the last entry whose `from` is at or below it. */
function stateOfValue(states: readonly ValueState[], value: number | null): MarkState | null {
  if (value === null || states.length === 0) return null;
  let state = (states[0] as ValueState).state;
  for (const entry of states) {
    if (entry.from !== null && value >= entry.from) state = entry.state;
  }
  return state;
}

function valueStatesOf(value: unknown): ValueState[] {
  return specList(value).flatMap((item): ValueState[] => {
    const entry = specObject(item);
    const state = markState(entry.state);
    return state === null
      ? []
      : [{ from: specNumber(entry.from), label: specText(entry.label) ?? "", state }];
  });
}

function referenceOf(value: unknown): BarsReference | null {
  if (value === undefined) return null;
  const reference = specObject(value);
  const y = specNumber(reference.y);
  if (y === null) return null;
  const label = specText(reference.label) ?? "";
  const yEnd = specNumber(reference.y_end);
  return yEnd === null
    ? { label, state: markState(reference.state), type: "rule", y }
    : { label, type: "band", y, yEnd };
}

/** What one slot draws: its value (on a facet table, the sum of its parts), its bound and
 *  interval, its unit, note and reason, and the readings it draws. */
interface Drawn {
  readonly value: number | null;
  /** On a facet table, its parts cut off on both sides: the cell prints each part. */
  readonly split: string | null;
  readonly bound: Bound | null;
  readonly ciLow: number | null;
  readonly ciHigh: number | null;
  readonly unit: string;
  readonly note: string;
  readonly reason: string;
  readonly n: number | null;
  readonly nUnit: string;
  readonly measured: DeclaredReading;
  readonly parts: ReadonlyMap<string, DeclaredReading>;
}

function drawnOf(slot: Slot, parts: readonly string[]): Drawn | null {
  const own = slot.own;
  if (slot.parts.size === 0) {
    if (own === null) return null;
    return {
      bound: own.bound,
      ciHigh: own.ciHigh,
      ciLow: own.ciLow,
      measured: own,
      n: own.n,
      nUnit: own.nUnit,
      note: own.note,
      parts: slot.parts,
      reason: own.absentReason,
      split: null,
      unit: own.unit,
      value: own.value,
    };
  }
  // A facet bar of parts: their sum, cut off on the side its parts are cut off on.
  const ordered = parts.flatMap((part) => {
    const reading = slot.parts.get(part);
    return reading === undefined ? [] : [reading];
  });
  const first = ordered[0] as DeclaredReading;
  const valued = ordered.filter((reading) => reading.value !== null);
  const sides = new Set(
    valued.flatMap((reading) => (reading.bound === null ? [] : [reading.bound]))
  );
  const unit = first.unit;
  return {
    bound: sides.size === 1 ? ([...sides][0] as Bound) : null,
    ciHigh: null,
    ciLow: null,
    measured: ordered.reduce((newest, reading) =>
      reading.measuredMs > newest.measuredMs ? reading : newest
    ),
    n: null,
    nUnit: "",
    note: (valued.length > 0 ? valued : ordered)
      .map((reading) => reading.note)
      .filter((note) => note !== "")
      .join("; "),
    parts: slot.parts,
    reason:
      valued.length > 0
        ? ""
        : ordered
            .map((reading) => reading.absentReason)
            .filter((reason) => reason !== "")
            .join("; "),
    split:
      sides.size > 1
        ? valued
            .map((reading) => writeNumber(reading.value as number, unit, reading.bound))
            .join(" + ")
        : null,
    unit,
    value:
      valued.length === 0
        ? null
        : valued.reduce((sum, reading) => sum + (reading.value as number), 0),
  };
}

/** The ids of what each group's table holds: its value, its interval where the chart draws one,
 *  its total against `of`, and on a facet table each part. */
function statisticsOf(
  kind: BarsKind,
  axis: BarsValueAxis,
  intervals: boolean,
  of: boolean,
  parts: readonly string[]
): ChartStatistic[] {
  return [
    { id: "value", label: axis.unit === "" ? axis.title : `${axis.title} (${axis.unit})` },
    ...(kind === "interval" || intervals
      ? [
          { id: "ci_low", label: "Interval low" },
          { id: "ci_high", label: "Interval high" },
        ]
      : []),
    ...(of ? [{ id: "total", label: "Total" }] : []),
    ...parts.map((part) => ({ id: `part:${part}`, label: part })),
  ];
}

/** A drawn slot's value of statistic `id`, with its total (`of`), noting when it was measured
 *  where it is the compare range's, and writing its unit where its facet row mixes units;
 *  nothing where the slot is empty. */
function cellValue(
  drawn: Drawn | null,
  id: string,
  context: {
    readonly total: { value: number; unit: string } | null;
    readonly compare: boolean;
    readonly withUnit: boolean;
  }
): ChartValue {
  if (drawn === null) return ABSENT;
  const { total } = context;
  const measured =
    context.compare && drawn.measured.measuredAt !== ""
      ? `measured ${drawn.measured.measuredAt.slice(0, 10)}`
      : null;
  if (id === "value") {
    if (drawn.value === null) {
      const reason = drawn.reason;
      return {
        // A reading's own words, which the chart draws as plain text: `eval-reading-reason`
        label: reason === "" ? "not measured" : `not measured: ${reason}`,
        note: reason === "" ? measured : reason,
        state: "absent",
        value: null,
      };
    }
    const notes = [
      drawn.bound === null ? "" : BOUND_WORDS[drawn.bound],
      drawn.note,
      measured ?? "",
    ].filter((part) => part !== "");
    const written = writeNumber(drawn.value, drawn.unit, drawn.bound);
    return {
      label:
        drawn.split ??
        (context.withUnit && drawn.unit !== "" ? `${written} ${drawn.unit}` : written),
      note: notes.length === 0 ? null : notes.join("; "),
      state: "plain",
      value: drawn.split === null ? drawn.value : null,
    };
  }
  if (id === "total") {
    return total === null
      ? { label: "no total", note: null, state: "absent", value: null }
      : {
          label: `${writeNumber(total.value, "")}${total.unit === "" ? "" : ` ${total.unit}`}`,
          note: null,
          state: "plain",
          value: total.value,
        };
  }
  if (id.startsWith("part:")) {
    const part = drawn.parts.get(id.slice("part:".length));
    if (part === undefined) return ABSENT;
    return part.value === null
      ? {
          label: part.absentReason === "" ? "not measured" : `not measured: ${part.absentReason}`,
          note: part.absentReason === "" ? null : part.absentReason,
          state: "absent",
          value: null,
        }
      : {
          label: writeNumber(part.value, part.unit, part.bound),
          note: part.note === "" ? null : part.note,
          state: "plain",
          value: part.value,
        };
  }
  const value = id === "ci_low" ? drawn.ciLow : drawn.ciHigh;
  return value === null
    ? { label: "no interval", note: null, state: "absent", value: null }
    : { label: writeNumber(value, drawn.unit), note: null, state: "plain", value };
}

/** The change from the compare range's value to the window's, by statistic. */
function changeOf(
  values: Readonly<Record<string, ChartValue>>,
  compare: Readonly<Record<string, ChartValue>>,
  unit: string
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
          : `${change > 0 ? "+" : "\u2212"}${writeNumber(Math.abs(change), unit)}`;
      return [id, { label, note: null, state: "plain", value: change }];
    })
  );
}

/** A row column's cell (`row_columns`): its number as a table cell prints it, with "of" its `n`
 *  and its unit (none where the column's label names it), with the column's decimals; or its
 *  note; or the column's `empty_label` where the row has neither, else nothing. */
function columnValue(column: DeclaredSpec, reading: DeclaredReading | undefined): ChartValue {
  const empty = specText(column.empty_label);
  const blank: ChartValue = {
    label: empty ?? "",
    note: null,
    state: "absent",
    value: null,
  };
  if (reading === undefined) return blank;
  if (column.field === "note") {
    return reading.note === ""
      ? blank
      : { label: reading.note, note: null, state: "plain", value: null };
  }
  if (reading.value === null) return blank;
  const decimals = specNumber(column.decimals);
  const number = (value: number) =>
    decimals === null ? writeNumber(value, "") : value.toFixed(decimals);
  const of = reading.n === null ? "" : ` of ${number(reading.n)}`;
  const unit = column.unit_in_label === true || reading.unit === "" ? "" : ` ${reading.unit}`;
  return {
    label: `${number(reading.value)}${of}${unit}`,
    note: null,
    state: reading.value === 0 ? "absent" : "plain",
    value: reading.value,
  };
}

/** The series a bar, an interval or a stacked bar reads: its y series, the series of its totals
 *  (`of`) and of its states (`state_of`), and each row column's. */
export function barsReads(spec: DeclaredSpec): ChartRead[] {
  const names = [
    specObject(spec.y).series,
    spec.of === "n" ? null : spec.of,
    spec.state_of,
    ...specList(spec.row_columns).map((column) => specObject(column).series),
  ].filter((name): name is string => typeof name === "string");
  return [...new Set(names)].map((series) => ({ series }));
}

/**
 * The model of a `bar`, an `interval` or a `stacked-bar` (plan D10) from its spec and the readings
 * of its series:
 * - a bar or an interval draws each category's newest reading: a row's, or with `group: "col"` a
 *   col's, in its own colour; by value, largest first, where `order` names none; grouped, a cluster
 *   per row and a mark per col; horizontal, a row per category, against its total with `of`; with
 *   `facet`, a table of a row per row and a column per col, each cell's bar the sum of its parts;
 * - a stacked bar draws a bar per measured date (its UTC date), a segment per col or row group,
 *   or with `stack: "row"` a bar per row and a segment per col, each its cell's newest reading;
 *   with `frames`, a frame per week of measured time, the newest first;
 * - the compare range's readings are each place's compare values, by category its newest; a
 *   mock-up's points stand for readings where the chart has no reading with a value.
 * Each series is a group, in the order of its colour; its points are every place of the chart, in
 * the order the chart draws them.
 */
export function barsModel(kind: BarsKind, spec: DeclaredSpec, input: DeclaredInput): BarsModel {
  const y = specObject(spec.y);
  const ySeries = specText(y.series) ?? "";
  const measured = input.readings.filter((reading) => reading.series === ySeries);

  // A mock-up draws where the chart's y series holds no reading with a value.
  const mockup = spec.mockup === undefined ? null : specObject(spec.mockup);
  const mocked = mockup !== null && !measured.some((reading) => reading.value !== null);
  const shaped = mocked && mockup.shape !== undefined;
  const form = formOf(kind, spec, mocked, shaped);
  const parts = form.facet
    ? specList(spec.stack_order).filter((part): part is string => typeof part === "string")
    : [];
  const ciLevel = mockup === null ? null : specNumber(mockup.ci_level);
  const ys: DeclaredReading[] =
    mocked && mockup !== null
      ? specList(mockup.points).flatMap((point, index) => {
          const reading = mockReading(form, specObject(point), index, ciLevel, ySeries);
          return reading === null ? [] : [reading];
        })
      : measured;
  const shift =
    input.compare === null ? 0 : Date.parse(input.window.from) - Date.parse(input.compare.from);
  const shiftOf = (range: Range) => (range === "compare" ? shift : 0);
  const notes: string[] = [];

  // Each range's newest reading of each place, group and part, and the places in the order of
  // their first readings: the window's, then the compare range's.
  const places = new Map<string, Keyed>();
  const cells: Record<Range, Slots> = { compare: new Map(), window: new Map() };
  const ranges: Range[] = input.compare === null || mocked ? ["window"] : ["window", "compare"];
  for (const range of ranges) {
    const own = ys.filter((reading) => reading.range === range);
    const keyed = new Map(
      own.map((reading) => [reading, keyOf(form, parts, reading, shiftOf(range))])
    );
    const keyFor = (reading: DeclaredReading) => keyed.get(reading) as Keyed;
    const newest = newestByCell(own, (reading) => {
      const key = keyFor(reading);
      return `${key.place}\n${key.group}\n${key.part}`;
    });
    for (const reading of newest.values()) {
      const key = keyFor(reading);
      if (!places.has(key.place)) places.set(key.place, key);
      let byPlace = cells[range].get(key.group);
      if (byPlace === undefined) {
        byPlace = new Map();
        cells[range].set(key.group, byPlace);
      }
      const slot = byPlace.get(key.place) ?? { own: null, parts: new Map() };
      if (key.part === "") slot.own = reading;
      else slot.parts.set(key.part, reading);
      byPlace.set(key.place, slot);
    }
    if (range !== "window") continue;
    // The readings of a cell another reading of its place stands for: on a bar of one series,
    // another col's (row's) of its row (col); on a stack over time, another cell's of its date
    // and segment.
    const cellsOfReadings = new Set(
      own.map((reading) => {
        const key = keyFor(reading);
        return `${key.place}\n${key.group}\n${key.part}\n${reading.row}\n${reading.col}`;
      })
    );
    const unplotted = cellsOfReadings.size - newest.size;
    if (unplotted > 0 && form.single) {
      notes.push(
        `${plural(unplotted, "reading")} not plotted: a newer reading of another ${
          form.byCol ? "row" : "col"
        } stands for its ${form.byCol ? "col" : "row"}.`
      );
    } else if (unplotted > 0 && form.overTime) {
      notes.push(
        `${plural(unplotted, "reading")} not plotted: a newer reading stands for its date and ${
          form.segment === "row_group" ? "row group" : "col"
        }.`
      );
    }
  }
  // A facet bar of parts draws in place of its row's own reading in that col.
  let hidden = 0;
  for (const range of ranges) {
    for (const byPlace of cells[range].values()) {
      for (const slot of byPlace.values()) {
        if (slot.parts.size === 0 || slot.own === null) continue;
        if (range === "window") hidden++;
        slot.own = null;
      }
    }
  }
  if (hidden > 0) {
    notes.push(
      `${plural(hidden, "reading")} not plotted: its row's parts stand for it in its col.`
    );
  }

  // The newest reading of another series at each place (on a cluster or a stack, each cell):
  // the totals of `of`, the values `state_of` colours by, and each row column's cells.
  const otherSeries = (series: string, range: Range): Map<string, DeclaredReading> =>
    newestByCell(
      input.readings.filter((reading) => reading.series === series && reading.range === range),
      (reading) => {
        const key = keyOf(form, parts, reading, shiftOf(range));
        return `${key.place}\n${form.single ? "" : key.group}`;
      }
    );
  const ofField = kind === "bar" && (form.layout === "horizontal" || form.grouped) ? spec.of : null;
  const of = typeof ofField === "string";
  const ofSeries = of && ofField !== "n" ? ofField : null;
  const totals: Record<Range, Map<string, DeclaredReading>> = {
    compare:
      ofSeries === null || !ranges.includes("compare")
        ? new Map()
        : otherSeries(ofSeries, "compare"),
    window: ofSeries === null ? new Map() : otherSeries(ofSeries, "window"),
  };
  const totalOf = (
    range: Range,
    place: string,
    group: string,
    drawn: Drawn | null
  ): { value: number; unit: string } | null => {
    if (!of || drawn === null) return null;
    if (ofSeries === null) {
      return drawn.n === null ? null : { unit: drawn.nUnit || drawn.unit, value: drawn.n };
    }
    const total = totals[range].get(`${place}\n${form.single ? "" : group}`);
    return total === undefined || total.value === null
      ? null
      : { unit: total.unit || drawn.unit, value: total.value };
  };
  const valueStates = kind === "interval" ? [] : valueStatesOf(spec.value_states);
  const stateOfSeries = valueStates.length === 0 ? null : specText(spec.state_of);
  const stateValues: Map<string, DeclaredReading> =
    stateOfSeries === null ? new Map() : otherSeries(stateOfSeries, "window");

  const drawnAt = (range: Range, group: string, place: string): Drawn | null => {
    const slot = cells[range].get(group)?.get(place);
    return slot === undefined ? null : drawnOf(slot, parts);
  };
  const groupsSeen = [...new Set([...cells.window.keys(), ...cells.compare.keys()])];
  // On a horizontal bar against its totals, a row with no total is left out.
  if (form.layout === "horizontal" && of) {
    const left = [...places.keys()].filter(
      (place) => totalOf("window", place, "", drawnAt("window", "", place)) === null
    );
    for (const place of left) places.delete(place);
    if (left.length > 0) {
      notes.push(
        `${plural(left.length, "row")} not drawn, with no total (${
          ofSeries === null ? "no n" : `no reading of ${ofSeries}`
        }): ${left.join(", ")}.`
      );
    }
  }

  // The places in the order the chart draws them.
  const firstSeen = [...places.keys()];
  const categoryOf = (place: string) => (places.get(place) as Keyed).category;
  const order = spec.order;
  const orderList = specList(order).filter((name): name is string => typeof name === "string");
  // Each of `names` (categories) in the order it draws, where `placeOf` names its place: a bar and
  // an interval of one series by value, largest first; clusters and stacks in the order of their
  // first readings; `order: "total"` by the sum of each place's values; then the categories
  // `groups` or `order` names first.
  const categoryOrder = (names: string[], placeOf: (name: string) => string): string[] => {
    const totalAt = (name: string) => {
      const values = groupsSeen.flatMap((group) => {
        const value = drawnAt("window", group, placeOf(name))?.value ?? null;
        return value === null ? [] : [value];
      });
      return values.length === 0 ? Number.NEGATIVE_INFINITY : values.reduce((a, b) => a + b, 0);
    };
    const base =
      form.single || order === "total"
        ? names
            .map((name, index) => ({ index, name, total: totalAt(name) }))
            .sort((a, b) => b.total - a.total || a.index - b.index)
            .map(({ name }) => name)
        : names;
    const named =
      form.layout === "horizontal" && spec.groups !== undefined
        ? specList(spec.groups).flatMap((item) => specList(specObject(item).rows))
        : orderList;
    const first = named.filter(
      (name): name is string => typeof name === "string" && names.includes(name)
    );
    return [...first, ...base.filter((name) => !first.includes(name))];
  };
  let placeOrder: string[];
  if (form.overTime) {
    placeOrder = [...firstSeen].sort();
  } else if (form.frames) {
    // Each week's bars, the newest week first.
    const weeks = [
      ...new Set(firstSeen.map((place) => (places.get(place) as Keyed).week as string)),
    ]
      .sort()
      .reverse();
    placeOrder = weeks.flatMap((week) => {
      const rows = firstSeen.flatMap((place) =>
        (places.get(place) as Keyed).week === week ? [categoryOf(place)] : []
      );
      return categoryOrder(rows, (row) => `${week}${JOIN}${row}`).map(
        (row) => `${week}${JOIN}${row}`
      );
    });
  } else {
    placeOrder = categoryOrder(firstSeen, (place) => place);
  }
  if (form.facet && placeOrder.length > FACET_ROWS) {
    const left = placeOrder.slice(FACET_ROWS);
    placeOrder = placeOrder.slice(0, FACET_ROWS);
    notes.push(
      `${plural(left.length, "row")} not drawn: a facet table draws ${FACET_ROWS}: ${left.join(", ")}.`
    );
  }

  // Each group in the order it draws, with its colour: a stack's parts by `stack_order` (or, over
  // time, `order`), the rest as they came; a cluster's and a facet table's cols as they came.
  const toneSpec: DeclaredSpec = {
    ...spec,
    order:
      kind !== "stacked-bar"
        ? undefined
        : spec.stack_order !== undefined || !form.overTime
          ? spec.stack_order
          : spec.order,
  };
  const groupTone = new Map<string, Tone>(
    form.single ? [["", 0]] : groupTones(toneSpec, groupsSeen).map(({ name, tone }) => [name, tone])
  );
  const groups = [...groupTone.keys()];
  // A bar of one series colours its categories: each col in its own, by its place in `order`;
  // each row by its state or the highlight; a facet row by its state.
  const states = specObject(spec.states);
  const highlight = specText(spec.highlight);
  const categoryTones = form.byCol ? groupTones(spec, firstSeen.map(categoryOf)) : [];
  const categoryTone = new Map(categoryTones.map(({ name, tone }) => [name, tone]));
  const placeTone = (category: string): Tone | null => {
    if (form.byCol) return categoryTone.get(category) ?? null;
    if (!form.single && !form.facet) return null;
    const state = markState(states[category]);
    if (state !== null) return state;
    return form.single && highlight !== null ? (category === highlight ? 0 : "other") : null;
  };

  // What the value axis draws, and the units the readings are in.
  const windowDrawn = groups.flatMap((group) =>
    placeOrder.flatMap((place) => {
      const drawn = drawnAt("window", group, place);
      return drawn === null ? [] : [drawn];
    })
  );
  const unit = ys.find((reading) => reading.unit !== "")?.unit ?? "";
  const units = new Set(windowDrawn.map((drawn) => drawn.unit).filter((one) => one !== ""));
  const unitsMayDiffer = form.facet || (form.layout === "horizontal" && of);
  const yLabel = specText(y.label);
  const valueAxis: BarsValueAxis = {
    log: y.log === true,
    max: specNumber(y.max),
    min: specNumber(y.min),
    share: form.layout === "horizontal" && of,
    title: yLabel === null || yLabel === "" ? ySeries : yLabel,
    unit: units.size > 1 ? "" : unit,
    whole: y.ticks === "whole",
  };
  // A stacked bar draws no interval; a bar draws each reading's own, where any has one.
  const intervals =
    kind !== "stacked-bar" &&
    ys.some((reading) => reading.ciLow !== null || reading.ciHigh !== null);
  // The columns right of a dot chart's plot, each a statistic of its one series: the newest
  // reading of the column's series in each row.
  const columns = (form.layout === "horizontal" ? specList(spec.row_columns) : []).map(
    (item, index) => {
      const column = specObject(item);
      const name = specText(column.series) ?? "";
      const cells: Record<Range, Map<string, DeclaredReading>> = {
        compare: ranges.includes("compare") ? otherSeries(name, "compare") : new Map(),
        window: otherSeries(name, "window"),
      };
      return {
        cells,
        column,
        field: column.field === "note" ? ("note" as const) : ("value" as const),
        header: specText(column.label) ?? name,
        statistic: `column:${index}`,
      };
    }
  );
  const statistics = [
    ...statisticsOf(kind, valueAxis, intervals, of, parts),
    ...columns.map(({ header, statistic }) => ({ id: statistic, label: header })),
  ];
  const comparing = input.compare !== null;
  const nothing = Object.fromEntries(statistics.map(({ id }) => [id, ABSENT]));

  const placeRecords: BarsPlace[] = placeOrder.map((place) => {
    const key = places.get(place) as Keyed;
    const rowUnits = form.facet
      ? new Set(
          groups.flatMap((group) => {
            const drawn = drawnAt("window", group, place);
            return drawn === null || drawn.unit === "" ? [] : [drawn.unit];
          })
        )
      : new Set<string>();
    const rowUnit = rowUnits.size === 1 ? ([...rowUnits][0] as string) : "";
    const subtitle = form.facet ? specText(specObject(spec.row_subtitles)[key.category]) : null;
    return {
      at: key.at,
      category: key.category,
      heading: null,
      label: form.frames ? `${weekLabel(key.week as string)}${JOIN}${key.category}` : place,
      mixed: rowUnits.size > 1,
      subtitle,
      unit:
        rowUnit !== "" && !key.category.toLowerCase().includes(rowUnit.toLowerCase())
          ? rowUnit
          : "",
      week: key.week,
      x: place,
    };
  });
  // Each group of a horizontal bar heads its first drawn row.
  if (form.layout === "horizontal") {
    for (const item of specList(spec.groups)) {
      const group = specObject(item);
      const rows = specList(group.rows);
      const first = placeRecords.findIndex((place) => rows.includes(place.category));
      const record = placeRecords[first];
      if (record !== undefined) {
        placeRecords[first] = { ...record, heading: specText(group.label) ?? "" };
      }
    }
  }

  const series: BarsSeries[] = groups.map((group) => {
    const points = placeRecords.map((place): BarsPoint => {
      const own = drawnAt("window", group, place.x);
      const then = comparing ? drawnAt("compare", group, place.x) : null;
      const empty = own === null && then === null;
      const valuesOf = (range: Range, drawn: Drawn | null): Record<string, ChartValue> =>
        Object.fromEntries(
          statistics.map(({ id }) => {
            const column = columns.find((one) => one.statistic === id);
            return [
              id,
              column === undefined
                ? cellValue(drawn, id, {
                    compare: range === "compare",
                    total: totalOf(range, place.x, group, drawn),
                    withUnit: place.mixed,
                  })
                : columnValue(column.column, column.cells[range].get(`${place.x}\n`)),
            ];
          })
        );
      const values = empty ? nothing : valuesOf("window", own);
      const compareValues = !comparing ? null : empty ? nothing : valuesOf("compare", then);
      const valueState = stateOfValue(
        valueStates,
        stateOfSeries === null
          ? (own?.value ?? null)
          : (stateValues.get(`${place.x}\n${form.single ? "" : group}`)?.value ?? null)
      );
      return {
        bound: own?.bound ?? null,
        change:
          compareValues === null ? null : empty ? nothing : changeOf(values, compareValues, unit),
        compare: compareValues,
        compareBound: then?.bound ?? null,
        compareHeld: then !== null,
        held: own !== null,
        label: place.label,
        note: own?.note ?? "",
        partial: false,
        reason: own !== null && own.value === null ? own.reason : "",
        tone: valueState ?? placeTone(place.category),
        values,
        x: place.x,
      };
    });
    return {
      dataNote: null,
      group,
      headline: [],
      label: group === "" ? valueAxis.title : group,
      name: group === "" ? ySeries : `${ySeries}${JOIN}${group}`,
      noun: "readings",
      points,
      repositoryNote: null,
      statistics,
      target: null,
      tone: groupTone.get(group) ?? "other",
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

  // The annotations at a category the chart draws, or at a time over time.
  const annotations: BarsAnnotation[] = [];
  for (const item of specList(spec.annotations)) {
    const note = specObject(item);
    const label = specText(note.label) ?? "";
    const base = {
      detail: specText(note.detail),
      label,
      state: markState(note.state),
      y: specNumber(note.y),
    };
    if (form.overTime) {
      const at = timeOf(note.x);
      if (at !== null) annotations.push({ ...base, at, category: null });
      continue;
    }
    const category = specText(note.x) ?? "";
    if (placeRecords.some((record) => record.category === category)) {
      annotations.push({ ...base, at: null, category });
    } else {
      notes.push(`The note \u201c${label}\u201d is not drawn: the chart draws no ${category}.`);
    }
  }

  // The lines under the plot.
  const everyReading = groups.flatMap((group) =>
    placeOrder.flatMap((place) =>
      ranges.flatMap((range) => {
        const drawn = drawnAt(range, group, place);
        return drawn === null
          ? []
          : drawn.parts.size > 0
            ? [...drawn.parts.values()]
            : [drawn.measured];
      })
    )
  );
  const ranged = kind === "stacked-bar" ? null : rangesLine(spec, everyReading);
  const bounds = (["<=", ">="] as const).filter((bound) =>
    everyReading.some((reading) => reading.bound === bound)
  );
  const values = windowDrawn.flatMap((drawn) => (drawn.value === null ? [] : [drawn.value]));
  const unitText = valueAxis.unit === "" ? "" : ` ${valueAxis.unit}`;
  if (!valueAxis.share && valueAxis.max !== null) {
    const above = values.filter((value) => value > (valueAxis.max as number)).length;
    if (above > 0) notes.push(`\u25b2 ${plural(above, "value")} above ${valueAxis.max}${unitText}`);
  }
  if (!valueAxis.share && valueAxis.min !== null) {
    const below = values.filter((value) => value < (valueAxis.min as number)).length;
    if (below > 0) notes.push(`\u25bc ${plural(below, "value")} below ${valueAxis.min}${unitText}`);
  }
  if (valueAxis.log && !valueAxis.share) {
    const zero = values.filter((value) => value <= 0).length;
    if (zero > 0) notes.push(`${plural(zero, "value")} at or below 0 not drawn on the log axis.`);
  }
  if (form.facet) {
    const negative = values.filter((value) => value < 0).length;
    if (negative > 0) {
      notes.push(
        `${plural(negative, "value")} below 0 drawn as a number alone: a bar from 0 has no length for it.`
      );
    }
  }
  const lines = [
    ...(ranged === null ? [] : [ranged]),
    ...bounds.map((bound) =>
      bound === ">="
        ? "Lower bound: the true value is at least this."
        : "Upper bound: the true value is at most this."
    ),
    ...notes,
  ];

  // The key over the plot: each value state, else each group's colour where it names more than
  // one, unless the chart names its groups directly; a facet table names its cols in its header.
  const keyed: { name: string; tone: Tone }[] = form.byCol
    ? categoryTones
    : form.single
      ? []
      : groups.map((group) => ({ name: group, tone: groupTone.get(group) ?? "other" }));
  const legend: Bars["legend"] = form.facet
    ? "none"
    : valueStates.length > 0
      ? "states"
      : spec.legend === "direct" && !form.single
        ? "direct"
        : keyed.length >= 2
          ? "key"
          : "none";

  const frames: BarsFrame[] | null = form.frames
    ? [...new Set(placeRecords.map((place) => place.week as string))].map((week) => ({
        label: weekLabel(week),
        places: placeRecords.flatMap((place, index) => (place.week === week ? [index] : [])),
        week,
      }))
    : null;

  const mockupBy = mocked ? (specText(mockup?.submitted_by) ?? "its owner") : null;
  const mockupAt = mocked ? (specText(mockup?.submitted_at) ?? "") : "";
  const x = specObject(spec.x);
  const xLabel = specText(x.label);
  const placeTitle = form.overTime
    ? xLabel === null || xLabel === ""
      ? "Measured time (UTC)"
      : xLabel
    : shaped
      ? `${specText(specObject(mockup?.x).label) ?? "x"} (mock-up, no scale)`
      : (xLabel ?? "");
  const axis: ChartAxis = form.overTime
    ? { label: "Measured date (UTC)", many: "dates", one: "date" }
    : form.frames
      ? { label: "Week \u00b7 row", many: "weeks and rows", one: "week and row" }
      : { label: placeTitle || "Category", many: "categories", one: "category" };
  const shapeAxis = specObject(mockup?.y);
  const shapeUnit = specText(shapeAxis.unit) ?? "";

  return {
    axis,
    bars: {
      annotations,
      columns: columns.map(({ field, header, statistic }) => ({ field, header, statistic })),
      frames,
      grouped: form.grouped,
      height:
        specNumber(spec.height) ??
        (form.layout === "horizontal" || form.layout === "facet" ? null : DEFAULT_HEIGHT),
      key: legend === "key" || legend === "direct" ? keyed : [],
      kind,
      labels: kind === "interval" && spec.labels === "value",
      layout: form.layout,
      legend,
      mark:
        kind === "interval"
          ? "point"
          : form.layout === "horizontal" && spec.mark === "dot"
            ? "dot"
            : form.layout === "horizontal" && spec.mark === "dots" && of
              ? "dots"
              : "bar",
      mixedUnits: !unitsMayDiffer && units.size > 1 ? [...units].sort() : [],
      mockup:
        mockupBy === null
          ? null
          : `Mock-up: nothing is measured yet; ${mockupBy} drew it${mockupAt === "" ? "" : ` on ${mockupAt.slice(0, 10)}`}.`,
      noScale: shaped,
      notStartedGrey: spec.not_started_outline === "grey",
      notes: lines,
      of,
      overTime: form.overTime,
      parts: parts.map((name) => ({ name, state: markState(states[name]) })),
      placeTitle,
      places: placeRecords,
      plotHeader: form.layout === "horizontal" ? specText(spec.plot_header) : null,
      reference: form.facet ? null : referenceOf(spec.reference),
      rowNotes: form.layout === "horizontal" && spec.row_notes === "note",
      valueAxis: shaped
        ? {
            ...valueAxis,
            title: `${specText(shapeAxis.label) ?? "y"}${shapeUnit === "" ? "" : ` (${shapeUnit})`} (mock-up, no scale)`,
            unit: "",
          }
        : valueAxis,
      valueStates,
      weekTicks: form.layout === "area" && x.ticks === "week",
    },
    comparable: true,
    compareRange: input.compare,
    kind,
    name: declaredChartName(input.slug, spec),
    series,
    title: specText(spec.title) ?? specText(spec.id) ?? "",
  };
}
