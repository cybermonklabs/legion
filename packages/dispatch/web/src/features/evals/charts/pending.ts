// The `pending` kind (dispatch://INFRA-2918/artifact/plan-md, D4, D10): an approved graph whose
// kind is not built yet. It draws its owner's mock-up, a line through each series' points or a bar
// for each, on axes of its own that name no scale, under a hatched MOCK-UP; or, with none, a
// placeholder that names the chart. Its `builder`, the issue that builds the kind, stands in an
// attribute, never in the text, as the eval platform (at commit 4f9223b9) keeps it. It reads no
// readings and takes no compare range (`comparable: false`, spec R20): its table holds the
// mock-up's points. Pure.
import {
  type ChartModel,
  type ChartPoint,
  type ChartSeries,
  type ChartValue,
  NO_DATA,
  writerOf,
} from "../chart-model";
import {
  type ChartKind,
  type DeclaredInput,
  type DeclaredSpec,
  declaredChartName,
  groupTones,
  specList,
  specNumber,
  specObject,
  specText,
  type Tone,
} from "./kind";
import { PendingRenderer } from "./PendingRenderer";

/** A mock-up's form, as its `shape` names it: x a number on a line, a category under a bar. */
export type SketchShape = "line" | "bar";

/** One place on a mock-up's x axis: a number of a line, else a category, in the order it draws. */
export interface SketchPlace {
  readonly x: string;
  readonly label: string;
  /** Where it stands: the number on a line, the category's index under bars. */
  readonly at: number;
}

/** What a mock-up draws beside its points: its form, its axes' titles, who drew it and when. */
export interface Sketch {
  readonly shape: SketchShape;
  readonly xTitle: string;
  readonly yTitle: string;
  /** The line over the drawing that says it is a mock-up, and whose. */
  readonly line: string;
  readonly places: readonly SketchPlace[];
}

export interface PendingSeries extends ChartSeries {
  readonly tone: Tone;
}

export interface PendingModel extends ChartModel {
  readonly series: readonly PendingSeries[];
  readonly pending: {
    /** The `dispatch://` link to the issue that builds this kind, null where the spec names none. */
    readonly builder: string | null;
    /** The mock-up, null where the owner has drawn none. */
    readonly sketch: Sketch | null;
    /** What stands in the chart's place with no mock-up, naming the chart; null with one. */
    readonly placeholder: string | null;
  };
}

/** A mock-up axis's title: its label and unit, and that it names no scale. */
function sketchTitle(axis: DeclaredSpec, fallback: string): string {
  const label = specText(axis.label) || fallback;
  const unit = specText(axis.unit) ?? "";
  return `${label}${unit === "" ? "" : ` (${unit})`} (mock-up, no scale)`;
}

/** A point's x as its place's name: a line's number as written, a bar's category as given. */
function placeName(shape: SketchShape, x: unknown): string | null {
  if (shape === "line") {
    const number = specNumber(x);
    return number === null ? null : writerOf("", "value")(number)(0);
  }
  return typeof x === "string" && x !== "" ? x : null;
}

/**
 * The model of a `pending` spec. With a mock-up (`shape` line or bar, the only form a pending
 * chart takes): one series for each series its points name, in the order they first name it, and
 * one place for each x, a line's in the order of its number and a bar's in the order the points
 * first name it; a series' second point at one x takes a place of its own, "<x> (2)". Each series
 * holds a value at every place, "no data" where it has no point. With none: no series, and the
 * placeholder that names the chart.
 */
export function pendingModel(spec: DeclaredSpec, input: DeclaredInput): PendingModel {
  const title = specText(spec.title) ?? specText(spec.id) ?? "";
  const builder = specText(spec.builder);
  const mockup = spec.mockup === undefined ? null : specObject(spec.mockup);
  const shape: SketchShape | null =
    mockup?.shape === "line" || mockup?.shape === "bar" ? mockup.shape : null;
  const base = {
    comparable: false,
    compareRange: input.compare,
    kind: "pending",
    name: declaredChartName(input.slug, spec),
    title,
  } as const;
  if (mockup === null || shape === null) {
    return {
      ...base,
      axis: { label: "Point", many: "points", one: "point" },
      pending: {
        builder,
        placeholder: `${title}: this graph is not built yet, and its owner has drawn no mock-up of it.`,
        sketch: null,
      },
      series: [],
    };
  }

  const xAxis = specObject(mockup.x);
  const yAxis = specObject(mockup.y);
  const unit = specText(yAxis.unit) ?? "";
  const write = writerOf(unit, "value");
  // Each series' points, by the place each takes, in the order the points name them.
  const bySeries = new Map<string, Map<string, number>>();
  const places = new Map<string, SketchPlace & { readonly order: number }>();
  specList(mockup.points).forEach((item) => {
    const point = specObject(item);
    const series = specText(point.series) ?? "";
    const name = placeName(shape, point.x);
    const y = specNumber(point.y);
    if (series === "" || name === null || y === null) return;
    const own = bySeries.get(series) ?? new Map<string, number>();
    bySeries.set(series, own);
    let x = name;
    for (let count = 2; own.has(x); count++) x = `${name} (${count})`;
    own.set(x, y);
    if (!places.has(x)) {
      const at = shape === "line" ? (specNumber(point.x) as number) : places.size;
      places.set(x, { at, label: x, order: places.size, x });
    }
  });
  // A line's places in the order of their numbers, a second point after the first; a bar's as
  // they came.
  const ordered = [...places.values()].sort((a, b) =>
    shape === "line" ? a.at - b.at || a.order - b.order : a.order - b.order
  );
  const sketchPlaces = ordered.map(
    (place, index): SketchPlace => ({
      at: shape === "line" ? place.at : index,
      label: place.label,
      x: place.x,
    })
  );
  const absent: ChartValue = { label: NO_DATA, note: null, state: "absent", value: null };
  const yLabel = specText(yAxis.label) || "y";
  const tones = groupTones(spec, [...bySeries.keys()]);
  const series = tones.map(({ name, tone }): PendingSeries => {
    const own = bySeries.get(name) ?? new Map<string, number>();
    const points = sketchPlaces.map((place): ChartPoint => {
      const y = own.get(place.x);
      return {
        change: null,
        compare: null,
        label: place.label,
        partial: false,
        values: {
          y:
            y === undefined
              ? absent
              : { label: write(y)(0), note: "mock-up", state: "plain", value: y },
        },
        x: place.x,
      };
    });
    return {
      dataNote: null,
      headline: [],
      label: name,
      name,
      noun: "points",
      points,
      repositoryNote: null,
      statistics: [{ id: "y", label: unit === "" ? yLabel : `${yLabel} (${unit})` }],
      target: null,
      tone,
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
  const by = specText(mockup.submitted_by) || "its owner";
  const at = specText(mockup.submitted_at) ?? "";
  const xLabel = specText(xAxis.label) || "x";
  const xUnit = specText(xAxis.unit) ?? "";
  return {
    ...base,
    axis: {
      label: xUnit === "" ? xLabel : `${xLabel} (${xUnit})`,
      many: "points",
      one: "point",
    },
    pending: {
      builder,
      placeholder: null,
      sketch: {
        line: `Mock-up: nothing is measured yet; ${by} drew it${at === "" ? "" : ` on ${at.slice(0, 10)}`}.`,
        places: sketchPlaces,
        shape,
        xTitle: sketchTitle(xAxis, "x"),
        yTitle: sketchTitle(yAxis, "y"),
      },
    },
    series,
  };
}

export const pendingKind: ChartKind = {
  model: pendingModel,
  reads: () => [],
  renderer: PendingRenderer,
};
