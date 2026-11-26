// The `lanes` kind (dispatch://INFRA-2918/artifact/plan-md, D10; the validator's rules,
// internal/dispatch/evals/charts): one lane for each row of its y series over measured time, its
// readings parting the lane's time into the states `value_states` maps their numbers to. Each
// state runs from the reading where it began to the reading where the next began, and the newest
// to the newest reading; the newest is a bar in its colour (a dot where it began at the newest
// reading), an earlier one a thin grey line, and a partial one a dashed line. Each lane ends in
// where its item stands: its newest reading's note, else its state's label. `marks` adds the
// readings of a second series in a lane as dated dots with their notes, `row_columns` the newest
// reading of other series in each lane's row between the lanes' names and the lanes, and
// `annotations` dated rules across the lanes. The compare range's readings shift onto the window's
// axis as a muted second series under each lane (`LanesRenderer`). The model's first series is
// the lanes' newest values, each lane one point, and each row column a series after it, so the
// table, one row per lane, holds every number the chart draws, and its compare value and change.
// Pure.
import {
  type ChartAxis,
  type ChartModel,
  type ChartSeries,
  type ChartStatistic,
  type ChartValue,
  NO_DATA,
  writerOf,
} from "../chart-model";
import {
  ABSENT,
  changeOf,
  entryOf,
  mockupLine,
  timeLabel,
  type ValueStateEntry,
  valueStatesOf,
  valueText,
} from "./grid";
import {
  type ChartKind,
  type ChartRead,
  type DeclaredInput,
  type DeclaredSpec,
  declaredChartName,
  type MarkState,
  specList,
  specNumber,
  specObject,
  specText,
} from "./kind";
import { LanesRenderer } from "./LanesRenderer";
import { type DeclaredReading, newestByCell } from "./readings";

/** A lane chart draws at most this many lanes; the table holds the rest. */
export const MAX_LANES = 35;

const DAY_MS = 24 * 60 * 60 * 1000;

/** One state of a lane: from the reading where it began to the reading where the next began (the
 *  newest, to the newest reading), in epoch milliseconds on the window's axis. */
export interface LaneSpan {
  readonly from: number;
  readonly to: number;
  readonly state: MarkState;
  readonly label: string;
  /** The lane's newest state: a bar, or a dot where it began at the newest reading. */
  readonly newest: boolean;
  readonly dot: boolean;
}

/** One reading of `marks` in a lane: a dot at its measured time, labelled with its note. */
export interface LaneMark {
  readonly id: string;
  readonly at: number;
  readonly note: string;
}

/** What a lane draws of one range. */
export interface LaneRange {
  readonly spans: readonly LaneSpan[];
  readonly marks: readonly LaneMark[];
}

export interface Lane {
  readonly row: string;
  /** The label of the group whose first lane it is; null where it starts none. */
  readonly group: string | null;
  readonly window: LaneRange;
  readonly compare: LaneRange;
  /** Where its item stands: its newest reading's note, else its state's label; null where the
   *  window holds no reading of it. */
  readonly end: { readonly text: string; readonly state: MarkState } | null;
}

/** One row column: the series of the model it is, what its cell prints, and what a lane with no
 *  such reading prints. */
export interface LaneColumn {
  readonly series: string;
  readonly label: string;
  readonly field: "value" | "note";
  readonly emptyLabel: string;
}

/** A dated rule across the lanes. */
export interface LaneRule {
  readonly at: number;
  readonly label: string;
  readonly detail: string | null;
}

/** Everything about the lanes' drawing that is not a value. */
export interface LanesPlot {
  /** Every lane, in the order the series' points hold them; the first `drawn` draw. */
  readonly lanes: readonly Lane[];
  readonly drawn: number;
  /** The time axis's ends, in epoch milliseconds. */
  readonly from: number;
  readonly to: number;
  readonly weekly: boolean;
  /** The time axis's title; null for none. */
  readonly xTitle: string | null;
  /** The headers over the groups' names, the lanes' names and the end texts; null for none. */
  readonly groupTitle: string | null;
  readonly namesTitle: string | null;
  readonly endTitle: string | null;
  /** The lanes stand in groups: a rule between groups in place of a line through each lane. */
  readonly grouped: boolean;
  readonly columns: readonly LaneColumn[];
  readonly rules: readonly LaneRule[];
  /** The lines under the chart: the lanes and readings it does not draw. */
  readonly notes: readonly string[];
  readonly mockup: string | null;
  readonly notStartedGrey: boolean;
}

export interface LanesModel extends ChartModel {
  readonly lanes: LanesPlot;
}

function plural(count: number, one: string): string {
  return `${count} ${one}${count === 1 ? "" : "s"}`;
}

/** A date (its first moment) or a time, in epoch milliseconds; null for neither. */
function timeOf(value: unknown): number | null {
  const text = specText(value);
  if (text === null) return null;
  const at = Date.parse(text);
  return Number.isFinite(at) ? at : null;
}

/** A mock-up's points as readings of the chart's y series, each point's series its lane. */
function mockReadings(mockup: DeclaredSpec, series: string): DeclaredReading[] {
  const ciLevel = specNumber(mockup.ci_level);
  return specList(mockup.points).flatMap((item, index) => {
    const point = specObject(item);
    const at = timeOf(point.x);
    if (at === null) return [];
    const ciHigh = specNumber(point.ci_high);
    return [
      {
        absentReason: "",
        attributes: {},
        bound: null,
        ciHigh,
        ciLevel: ciHigh === null ? null : ciLevel,
        ciLow: specNumber(point.ci_low),
        col: "",
        id: `mockup-${String(index).padStart(3, "0")}`,
        links: [],
        measuredAt: new Date(at).toISOString(),
        measuredMs: at,
        n: specNumber(point.n),
        nUnit: "",
        note: "",
        range: "window",
        recordedAt: "",
        replaces: "",
        row: specText(point.series) ?? "",
        rowGroup: "",
        rowKind: "mockup",
        series,
        source: "",
        unit: "",
        value: specNumber(point.y),
      },
    ];
  });
}

/** The readings of one range, oldest first, by their row. */
function byRow(
  readings: readonly DeclaredReading[],
  range: "window" | "compare"
): Map<string, DeclaredReading[]> {
  const rows = new Map<string, DeclaredReading[]>();
  for (const reading of readings) {
    if (reading.range !== range) continue;
    const held = rows.get(reading.row);
    if (held === undefined) rows.set(reading.row, [reading]);
    else held.push(reading);
  }
  return rows;
}

/** One range's lanes: each lane's readings that draw, one at each time, its states and its
 *  marks, by row. */
type LaneReadings = Map<string, { kept: DeclaredReading[]; spans: LaneSpan[]; marks: LaneMark[] }>;

/** A lane's readings with a value, oldest first, one at each measured time: of those measured at
 *  one time, the newest written. */
function onePerTime(readings: readonly DeclaredReading[]): DeclaredReading[] {
  const kept: DeclaredReading[] = [];
  for (const reading of readings) {
    if (reading.value === null) continue;
    if (kept.at(-1)?.measuredMs === reading.measuredMs) kept.pop();
    kept.push(reading);
  }
  return kept;
}

/** A lane's states from its readings, each run of readings whose entries draw alike one state,
 *  `shift` milliseconds on. */
function spansOf(
  readings: readonly DeclaredReading[],
  entries: readonly ValueStateEntry[],
  shift: number
): LaneSpan[] {
  const runs: { from: number; entry: ValueStateEntry }[] = [];
  for (const reading of readings) {
    const entry = entryOf(entries, reading.value as number);
    if (entry === null) continue;
    const last = runs.at(-1)?.entry;
    if (last !== undefined && last.state === entry.state && last.label === entry.label) continue;
    runs.push({ entry, from: reading.measuredMs + shift });
  }
  const newest = (readings.at(-1)?.measuredMs ?? 0) + shift;
  return runs.map((run, index) => {
    const to = runs[index + 1]?.from ?? newest;
    const last = index === runs.length - 1;
    return {
      dot: last && run.from === to,
      from: run.from,
      label: run.entry.label,
      newest: last,
      state: run.entry.state,
      to,
    };
  });
}

/** A row column's number as a cell prints it: with its decimals, out of its reading's `n`, and
 *  its unit unless the column's label names it. */
function columnNumber(
  reading: DeclaredReading,
  value: number,
  decimals: number | null,
  unitInLabel: boolean
): string {
  const plain = (number: number) => writerOf("", "value")(number)(0);
  const number = decimals === null ? plain(value) : value.toFixed(decimals);
  const base = reading.n === null ? number : `${number} of ${plain(reading.n)}`;
  const unit = reading.n !== null && reading.nUnit !== "" ? reading.nUnit : reading.unit;
  if (unitInLabel || unit === "") return base;
  return `${base} ${unit === "hours" ? "h" : unit === "minutes" ? "min" : unit}`;
}

/** The series a lane chart reads: its y series, its marks and each row column's series. */
export function lanesReads(spec: DeclaredSpec): ChartRead[] {
  const names = [
    specText(specObject(spec.y).series),
    specText(spec.marks),
    ...specList(spec.row_columns).map((column) => specText(specObject(column).series)),
  ].filter((name): name is string => name !== null);
  return [...new Set(names)].map((series) => ({ series }));
}

/**
 * The model of a `lanes` chart (plan D10) from its spec and the readings of its series: one lane
 * for each row of its y series, in the order its `groups` list them (only those), else `order`'s
 * first and then the rest as they came; each lane's states, marks and end of the window, and of
 * the compare range shifted onto the window's axis; each lane's newest value with its state, and
 * each row column's newest reading in the lane's row, with the compare range's and the change. A
 * mock-up's points stand for readings where the y series holds no reading with a value.
 */
export function lanesModel(spec: DeclaredSpec, input: DeclaredInput): LanesModel {
  const y = specObject(spec.y);
  const x = specObject(spec.x);
  const ySeries = specText(y.series) ?? "";
  const marksSeries = specText(spec.marks);
  const entries = valueStatesOf(spec);
  const comparing = input.compare !== null;
  // With no compare range the chart draws no compare reading, whatever the views still hold.
  const drawable = input.readings.filter((reading) => comparing || reading.range === "window");
  const measured = drawable.filter((reading) => reading.series === ySeries);
  const mockup = spec.mockup === undefined ? null : specObject(spec.mockup);
  const mocked = mockup !== null && !measured.some((reading) => reading.value !== null);
  const readings = mocked && mockup !== null ? mockReadings(mockup, ySeries) : measured;
  const shift =
    input.compare === null ? 0 : Date.parse(input.window.from) - Date.parse(input.compare.from);
  const notes: string[] = [];

  // The lanes: the window's rows first, then those only the compare range holds.
  const byRange = [
    ...readings.filter((reading) => reading.range === "window"),
    ...readings.filter((reading) => reading.range === "compare"),
  ];
  const read = [...new Set(byRange.map((reading) => reading.row))];
  const groups = specList(spec.groups).map((item) => {
    const group = specObject(item);
    return {
      label: specText(group.label) ?? "",
      rows: specList(group.rows).filter((row): row is string => typeof row === "string"),
    };
  });
  const order = specList(spec.order).filter((row): row is string => typeof row === "string");
  let rows: { row: string; group: string | null }[];
  if (groups.length > 0) {
    rows = groups.flatMap((group) =>
      group.rows
        .filter((row) => read.includes(row))
        .map((row, index) => ({ group: index === 0 ? group.label : null, row }))
    );
    const listed = new Set(groups.flatMap((group) => group.rows));
    const left = read.filter((row) => !listed.has(row));
    if (rows.length === 0 && read.length > 0) {
      notes.push("No lane drawn: the groups list no row that has readings.");
    }
    if (left.length > 0) notes.push(`Not drawn, as no group lists them: ${left.join(", ")}.`);
  } else {
    const named = [...new Set(order.filter((row) => read.includes(row)))];
    rows = [...named, ...read.filter((row) => !named.includes(row))].map((row) => ({
      group: null,
      row,
    }));
  }
  const drawn = Math.min(rows.length, MAX_LANES);
  if (rows.length > drawn) {
    notes.push(
      `${plural(rows.length - drawn, "more lane")} not drawn: a lane chart draws at most ${MAX_LANES}; the table holds them.`
    );
  }
  const drawnRows = new Set(rows.slice(0, drawn).map(({ row }) => row));
  const unvalued = readings.filter(
    (reading) => reading.value === null && drawnRows.has(reading.row)
  ).length;
  if (unvalued > 0) notes.push(`${plural(unvalued, "reading")} with no value not drawn.`);

  // Each lane's states and marks of each range, the compare range's `shift` on.
  const marks =
    marksSeries === null ? [] : drawable.filter((reading) => reading.series === marksSeries);
  const [window, compare] = (["window", "compare"] as const).map((range) => {
    const moved = range === "compare" ? shift : 0;
    const own = byRow(readings, range);
    const ownMarks = byRow(marks, range);
    return new Map(
      rows.map(({ row }) => {
        const kept = onePerTime(own.get(row) ?? []);
        const lane = {
          kept,
          marks: (ownMarks.get(row) ?? []).map(
            (reading): LaneMark => ({
              at: reading.measuredMs + moved,
              id: reading.id,
              note: reading.note,
            })
          ),
          spans: spansOf(kept, entries, moved),
        };
        return [row, lane];
      })
    );
  }) as [LaneReadings, LaneReadings];

  // The lanes' newest values, the model's first series.
  const newestValue = (
    kept: readonly DeclaredReading[],
    spans: readonly LaneSpan[],
    moved: number
  ) => {
    const reading = kept.at(-1);
    const span = spans.at(-1);
    if (reading === undefined || reading.value === null || span === undefined) return ABSENT;
    const note = mocked ? "mock-up" : reading.note;
    return {
      label: `${valueText(reading.value, reading.unit)} (${span.label})`,
      note: `since ${timeLabel(span.from - moved)}${note === "" ? "" : `; ${note}`}`,
      state: "plain",
      value: reading.value,
    } satisfies ChartValue;
  };
  const laneStatistics: ChartStatistic[] = [{ id: "value", label: "Value" }];
  const lanePoints = rows.map(({ row }) => {
    const own = window.get(row);
    const then = compare.get(row);
    const values = { value: newestValue(own?.kept ?? [], own?.spans ?? [], 0) };
    const compareValues = comparing
      ? { value: newestValue(then?.kept ?? [], then?.spans ?? [], shift) }
      : null;
    const unit = own?.kept.at(-1)?.unit ?? then?.kept.at(-1)?.unit ?? "";
    return {
      change: compareValues === null ? null : changeOf(values, compareValues, { value: unit }),
      compare: compareValues,
      label: row,
      partial: false,
      values,
      x: row,
    };
  });
  const laneSeries: ChartSeries = {
    dataNote: null,
    headline: [],
    label: ySeries,
    name: ySeries,
    noun: "lanes",
    points: lanePoints,
    repositoryNote: null,
    statistics: laneStatistics,
    target: null,
    unit: readings.find((reading) => reading.unit !== "")?.unit ?? "",
    window: {
      change: null,
      compare: null,
      label: "Window",
      partial: false,
      values: {},
      x: "window",
    },
  };

  // Each row column: the newest reading of its series in each lane's row.
  const columnSpecs = specList(spec.row_columns).map(specObject);
  const columns: LaneColumn[] = [];
  const columnSeries = columnSpecs.flatMap((column): ChartSeries[] => {
    const series = specText(column.series);
    if (series === null) return [];
    const field = column.field === "note" ? "note" : "value";
    const label = specText(column.label) ?? series;
    const emptyLabel = specText(column.empty_label) ?? "";
    const decimals = specNumber(column.decimals);
    const unitInLabel = column.unit_in_label === true;
    const name = field === "note" ? `${series} \u00b7 note` : series;
    columns.push({ emptyLabel, field, label, series: name });
    const own = drawable.filter((reading) => reading.series === series);
    const newest = (range: "window" | "compare") =>
      newestByCell(
        own.filter((reading) => reading.range === range),
        (reading) => reading.row
      );
    const windowCells = newest("window");
    const compareCells = newest("compare");
    const empty: ChartValue = {
      label: emptyLabel === "" ? NO_DATA : emptyLabel,
      note: null,
      state: "absent",
      value: null,
    };
    const cellOf = (reading: DeclaredReading | undefined): ChartValue => {
      if (reading === undefined) return empty;
      if (field === "note") {
        return reading.note === ""
          ? empty
          : { label: reading.note, note: null, state: "plain", value: null };
      }
      return reading.value === null
        ? empty
        : {
            label: columnNumber(reading, reading.value, decimals, unitInLabel),
            note: reading.note === "" ? null : reading.note,
            state: "plain",
            value: reading.value,
          };
    };
    const points = rows.map(({ row }) => {
      const reading = windowCells.get(row);
      const then = compareCells.get(row);
      const values = { [field]: cellOf(reading) };
      const compareValues = comparing ? { [field]: cellOf(then) } : null;
      return {
        change:
          compareValues === null
            ? null
            : changeOf(values, compareValues, {
                [field]: unitInLabel ? "" : (reading?.unit ?? ""),
              }),
        compare: compareValues,
        label: row,
        partial: false,
        values,
        x: row,
      };
    });
    return [
      {
        dataNote: null,
        headline: [],
        label,
        name,
        noun: "lanes",
        points,
        repositoryNote: null,
        statistics: [{ id: field, label: field === "note" ? "Note" : "Value" }],
        target: null,
        unit: "",
        window: {
          change: null,
          compare: null,
          label: "Window",
          partial: false,
          values: {},
          x: "window",
        },
      },
    ];
  });

  const lanes: Lane[] = rows.map(({ group, row }) => {
    const own = window.get(row);
    const then = compare.get(row);
    const newest = own?.kept.at(-1);
    const span = own?.spans.at(-1);
    return {
      compare: { marks: then?.marks ?? [], spans: then?.spans ?? [] },
      end:
        newest === undefined || span === undefined
          ? null
          : { state: span.state, text: !mocked && newest.note !== "" ? newest.note : span.label },
      group,
      row,
      window: { marks: own?.marks ?? [], spans: own?.spans ?? [] },
    };
  });

  // The time axis: the window, or a mock-up's own times.
  const times = readings.map((reading) => reading.measuredMs);
  const from = mocked ? Math.min(...times) : Date.parse(input.window.from);
  const to = mocked ? Math.max(...times) : Date.parse(input.window.to);
  const [low, high] =
    Number.isFinite(from) && Number.isFinite(to) && to > from
      ? [from, to]
      : [
          (Number.isFinite(from) ? from : 0) - DAY_MS / 2,
          (Number.isFinite(from) ? from : 0) + DAY_MS / 2,
        ];

  const rules = specList(spec.annotations).flatMap((item): LaneRule[] => {
    const note = specObject(item);
    const at = timeOf(note.x);
    const label = specText(note.label);
    return at === null || label === null ? [] : [{ at, detail: specText(note.detail), label }];
  });
  const xLabel = specText(x.label);
  const namesTitle = specText(spec.row_label) ?? (specText(y.label) || null);
  const axis: ChartAxis = { label: namesTitle ?? "Lane", many: "lanes", one: "lane" };
  return {
    axis,
    comparable: true,
    compareRange: input.compare,
    kind: "lanes",
    lanes: {
      columns,
      drawn,
      endTitle: specText(spec.end_heading),
      from: low,
      groupTitle: specText(spec.group_label),
      grouped: groups.length > 0,
      lanes,
      mockup: mocked && mockup !== null ? mockupLine(mockup) : null,
      namesTitle,
      notStartedGrey: spec.not_started_outline === "grey",
      notes,
      rules,
      to: high,
      weekly: x.ticks === "week",
      xTitle: xLabel === null ? "Measured time (UTC)" : xLabel === "" ? null : xLabel,
    },
    name: declaredChartName(input.slug, spec),
    series: [laneSeries, ...columnSeries],
    title: specText(spec.title) ?? specText(spec.id) ?? "",
  };
}

export const lanesKind: ChartKind = {
  model: lanesModel,
  reads: lanesReads,
  renderer: LanesRenderer,
};
