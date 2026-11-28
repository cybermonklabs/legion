// The model of a `runs` chart (dispatch://INFRA-2918/artifact/plan-md, D10; spec R20): the
// eval's runs, as the eval platform (at commit 4f9223b9) draws its run view. In Dispatch each run
// is a row of the readings, its `row_key` the run's id: a reading of a value series with no `col`
// is the run's final number of that series, a reading whose `col` is a whole number is its number
// of that series at that step, and each reading's `attributes.settings` and `attributes.tags` are
// the run's settings and tags (the newest reading's value of each key). The chart reads the
// series `name` and each `{final}` column's name: the run table's columns are the spec's `columns`
// first, then every other setting, tag and final number the runs hold, and the start; the picked
// runs draw over their steps (`step_label`), starting at `name`. A run started in the compare
// range when its first reading is of the compare range: it is marked, and its numbers are compare
// values. A run is in one range, so no run has a change. Pure.
import type { EvalLink } from "../../../api/types";
import {
  type ChartModel,
  type ChartPoint,
  type ChartSeries,
  type ChartStatistic,
  type ChartValue,
  writerOf,
} from "../chart-model";
import {
  type ChartRead,
  type DeclaredInput,
  type DeclaredSpec,
  declaredChartName,
  groupTones,
  specList,
  specObject,
  specText,
  type Tone,
} from "./kind";
import type { DeclaredReading } from "./readings";

/** The series of the run table, one point at each run's row: no eval series, whose names hold no
 *  space, is called this. */
export const RUN_COLUMNS = "run columns";

/** The statistic of a step's number in each value series. */
export const STEP_VALUE = "value";

/** The statistic of a run's start. */
export const STARTED = "started";

/** Where a run column reads its value: the run's settings, its tags, or its final numbers. */
export type RunField = "setting" | "tag" | "final";
const RUN_FIELDS: readonly RunField[] = ["setting", "tag", "final"];

/** One column of the run table. */
export interface RunColumn {
  /** `<field>:<key>`, the statistic its values name. */
  readonly id: string;
  readonly field: RunField;
  readonly key: string;
  /** Its header: a setting's key, a tag's and a final number's with what it is, so a setting and
   *  a tag of one key read apart. */
  readonly label: string;
  /** Whether each run that holds it holds a number, so it sorts as numbers. */
  readonly numeric: boolean;
  /** Whether the spec's `columns` name it, so the table shows it at first. */
  readonly declared: boolean;
}

/** One run's number of one series at one step: its row's place on the chart's axis. */
export interface RunStep {
  readonly step: number;
  readonly x: string;
  readonly value: ChartValue;
}

/** One run, as the run view draws it. */
export interface Run {
  readonly id: string;
  /** The range it started in. */
  readonly range: "window" | "compare";
  readonly startedMs: number;
  readonly started: ChartValue;
  readonly tone: Tone;
  /** Its value of each column, by the column's id. */
  readonly cells: Readonly<Record<string, ChartValue>>;
  /** Its numbers over steps, by series, in the order of their steps. */
  readonly steps: Readonly<Record<string, readonly RunStep[]>>;
  /** The links its readings carry (D10's `links`), which its row draws. */
  readonly links: readonly EvalLink[];
}

/** A series a run holds numbers of at steps, and its unit. */
export interface RunName {
  readonly name: string;
  readonly unit: string;
}

export interface RunsModel extends ChartModel {
  readonly runs: {
    readonly columns: readonly RunColumn[];
    /** Newest start first, as the platform lists them. */
    readonly list: readonly Run[];
    /** The series the runs hold numbers of at steps: `name` first, then in the spec's order. */
    readonly names: readonly RunName[];
    /** The series the picked runs draw over steps at first. */
    readonly name: string;
    /** The title of the steps' axis. */
    readonly stepLabel: string;
  };
}

export function isRunsModel(model: ChartModel): model is RunsModel {
  return "runs" in model;
}

/** The spec's columns, each once, in order. */
function columnsOf(spec: DeclaredSpec): { field: RunField; key: string }[] {
  const seen = new Set<string>();
  return specList(spec.columns).flatMap((item) => {
    const column = specObject(item);
    const field = RUN_FIELDS.find((one) => column[one] !== undefined);
    const key = field === undefined ? null : specText(column[field]);
    if (field === undefined || key === null || key === "" || seen.has(`${field}:${key}`)) return [];
    seen.add(`${field}:${key}`);
    return [{ field, key }];
  });
}

/** The series a runs spec reads: `name`, then each final column's name. */
export function runsReads(spec: DeclaredSpec): ChartRead[] {
  const names = [
    specText(spec.name),
    ...columnsOf(spec).flatMap(({ field, key }) => (field === "final" ? [key] : [])),
  ].filter((name): name is string => name !== null && name !== "");
  return [...new Set(names)].map((series) => ({ series }));
}

/** A setting's text as a run holds it: text, or a number written as JSON writes it. */
function settingText(value: unknown): string | null {
  if (typeof value === "string") return value;
  if (typeof value === "number" && Number.isFinite(value)) return String(value);
  return null;
}

/** A plain decimal number, as a setting held as text may be one: `8`, `-3`, `0.5`, `1e3`. */
const DECIMAL = /^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/;

/** A step: the whole number a reading's `col` is. */
const STEP = /^-?\d+$/;

/** What a reading of a number draws: its value with its unit, or that it was not measured. */
function readingValue(reading: DeclaredReading): ChartValue {
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
    label: writerOf(reading.unit, "value")(reading.value)(0),
    note: reading.note === "" ? null : reading.note,
    state: "plain",
    value: reading.value,
  };
}

/** A value a run does not hold. */
const NOT_SET: ChartValue = { label: "not set", note: null, state: "absent", value: null };

/** No value at all: a run's column at a step's row, a step's number at a run's row, a run's
 *  other range, and every change. */
const BLANK: ChartValue = { label: "", note: null, state: "absent", value: null };

/** One run's readings, gathered: its start, settings, tags, final numbers and steps. */
interface Gathered {
  readonly id: string;
  readonly first: DeclaredReading;
  readonly settings: Map<string, string>;
  readonly tags: Map<string, string>;
  readonly finals: Map<string, DeclaredReading>;
  readonly steps: Map<string, Map<number, DeclaredReading>>;
  readonly links: EvalLink[];
}

function gather(readings: readonly DeclaredReading[], series: ReadonlySet<string>): Gathered[] {
  const runs = new Map<string, Gathered>();
  // Oldest first, so each later reading's setting, tag, final number and step stands.
  for (const reading of readings) {
    if (reading.row === "" || !series.has(reading.series)) continue;
    let run = runs.get(reading.row);
    if (run === undefined) {
      run = {
        finals: new Map(),
        first: reading,
        id: reading.row,
        links: [],
        settings: new Map(),
        steps: new Map(),
        tags: new Map(),
      };
      runs.set(reading.row, run);
    }
    for (const [field, held] of [
      ["settings", run.settings],
      ["tags", run.tags],
    ] as const) {
      for (const [key, value] of Object.entries(specObject(reading.attributes[field]))) {
        const text = settingText(value);
        if (text !== null) held.set(key, text);
      }
    }
    run.links.push(...reading.links);
    if (reading.col === "") run.finals.set(reading.series, reading);
    else if (STEP.test(reading.col)) {
      const steps = run.steps.get(reading.series) ?? new Map<number, DeclaredReading>();
      run.steps.set(reading.series, steps);
      steps.set(Number(reading.col), reading);
    }
  }
  return [...runs.values()];
}

/**
 * The model of a `runs` spec. Its table has a row for each run, newest start first, each followed
 * by a row for each step it holds a number at; the run table's series (`RUN_COLUMNS`) holds each
 * run's columns and start at its row, and each series the runs hold steps of holds each run's
 * number at each of its step rows. A run started in the compare range holds its values as compare
 * values, and a run of the window as values; the other range and every change are blank.
 */
export function runsModel(spec: DeclaredSpec, input: DeclaredInput): RunsModel {
  const name = specText(spec.name) ?? "";
  const stepLabel = specText(spec.step_label) || "Step";
  const read = runsReads(spec).flatMap((one) => (one.series === undefined ? [] : [one.series]));
  const comparing = input.compare !== null;
  const gathered = gather(input.readings, new Set(read)).sort(
    (a, b) => b.first.measuredMs - a.first.measuredMs || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0)
  );

  // The spec's columns first, then every other setting, tag and final number the runs hold.
  const declared = columnsOf(spec);
  const others = RUN_FIELDS.flatMap((field) =>
    [
      ...new Set(
        gathered.flatMap((run) => [
          ...(field === "setting" ? run.settings : field === "tag" ? run.tags : run.finals).keys(),
        ])
      ),
    ]
      .sort()
      .filter((key) => !declared.some((one) => one.field === field && one.key === key))
      .map((key) => ({ field, key }))
  );
  const columns = [...declared, ...others].map(
    ({ field, key }, index): RunColumn => ({
      declared: index < declared.length,
      field,
      id: `${field}:${key}`,
      key,
      label: field === "setting" ? key : `${key} (${field === "tag" ? "tag" : "final"})`,
      numeric:
        field === "final" ||
        (field === "setting" &&
          gathered.some((run) => run.settings.has(key)) &&
          gathered.every((run) => {
            const text = run.settings.get(key);
            return text === undefined || DECIMAL.test(text);
          })),
    })
  );
  const cellOf = (run: Gathered, column: RunColumn): ChartValue => {
    if (column.field === "final") {
      const reading = run.finals.get(column.key);
      return reading === undefined ? NOT_SET : readingValue(reading);
    }
    const text = (column.field === "setting" ? run.settings : run.tags).get(column.key);
    if (text === undefined) return NOT_SET;
    return { label: text, note: null, state: "plain", value: column.numeric ? Number(text) : null };
  };

  // The series the runs hold steps of: `name` first, then in the order the spec reads them.
  const names = read.flatMap((series): RunName[] => {
    const steps = gathered.map((run) => run.steps.get(series)).find((own) => own !== undefined);
    const first = steps === undefined ? undefined : [...steps.values()][0];
    return first === undefined ? [] : [{ name: series, unit: first.unit }];
  });

  const tones = new Map(
    groupTones(
      spec,
      gathered.map((run) => run.id)
    ).map(({ name: id, tone }) => [id, tone])
  );
  const list = gathered.map((run): Run => {
    const steps = Object.fromEntries(
      names.map(({ name: series }) => [
        series,
        [...(run.steps.get(series) ?? new Map<number, DeclaredReading>()).entries()]
          .sort((a, b) => a[0] - b[0])
          .map(([step, reading]) => ({
            step,
            value: readingValue(reading),
            x: `${run.id}@${step}`,
          })),
      ])
    );
    return {
      cells: Object.fromEntries(columns.map((column) => [column.id, cellOf(run, column)])),
      id: run.id,
      links: run.links,
      range: comparing ? run.first.range : "window",
      started: {
        // A time as the run table writes it, to the minute.
        label: `${new Date(run.first.measuredMs).toISOString().slice(0, 16).replace("T", " ")} UTC`,
        note: null,
        state: "plain",
        value: null,
      },
      startedMs: run.first.measuredMs,
      steps,
      tone: tones.get(run.id) ?? "other",
    };
  });

  // Each run's row, then a row for each step it holds a number at.
  const places = list.flatMap((run) => {
    const steps = [
      ...new Set(Object.values(run.steps).flatMap((own) => own.map(({ step }) => step))),
    ].sort((a, b) => a - b);
    return [
      {
        label: run.range === "compare" ? `${run.id} \u00b7 started in the compare range` : run.id,
        run,
        step: null,
        x: run.id,
      },
      ...steps.map((step) => ({
        label: `${run.id} \u00b7 ${stepLabel} ${step}`,
        run,
        step,
        x: `${run.id}@${step}`,
      })),
    ];
  });

  const seriesOf = (
    seriesName: string,
    label: string,
    unit: string,
    statistics: ChartStatistic[],
    own: (place: (typeof places)[number]) => Readonly<Record<string, ChartValue>> | null
  ): ChartSeries => {
    const blank = Object.fromEntries(statistics.map(({ id }) => [id, BLANK]));
    return {
      dataNote: null,
      headline: [],
      label,
      name: seriesName,
      noun: "runs",
      points: places.map((place): ChartPoint => {
        const values = own(place) ?? blank;
        const window = place.run.range === "window";
        return {
          change: comparing ? blank : null,
          compare: comparing ? (window ? blank : values) : null,
          label: place.label,
          partial: false,
          values: window ? values : blank,
          x: place.x,
        };
      }),
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
  };

  const series = [
    seriesOf(
      RUN_COLUMNS,
      "Runs",
      "",
      [
        ...columns.map((column) => ({ id: column.id, label: column.label })),
        { id: STARTED, label: "Started" },
      ],
      (place) => (place.step === null ? { ...place.run.cells, [STARTED]: place.run.started } : null)
    ),
    ...names.map(({ name: series, unit }) =>
      seriesOf(
        series,
        series,
        unit,
        [{ id: STEP_VALUE, label: unit === "" ? series : `${series} (${unit})` }],
        (place) => {
          const step = place.run.steps[series]?.find((one) => one.step === place.step);
          return step === undefined ? null : { [STEP_VALUE]: step.value };
        }
      )
    ),
  ];

  return {
    axis: { label: "Run", many: "rows", one: "row" },
    comparable: true,
    compareRange: input.compare,
    kind: "runs",
    name: declaredChartName(input.slug, spec),
    runs: { columns, list, name, names, stepLabel },
    series,
    title: specText(spec.title) ?? specText(spec.id) ?? "",
  };
}
