// The model of a declared `number` (dispatch://INFRA-2918/artifact/plan-md, D10; the validator's
// rules, internal/dispatch/evals/charts/number.go): an eval's headline and its small numbers, each
// the newest reading with a value of one of the declaration's series. At the top stand its
// `tiles`, else its first series (the headline) and, beside it, each next series that no section
// owns; then each of its `sections`, a titled row of tiles with a line of `counts`, one count for
// each row of one series. The model is one series whose points are the small numbers in the order
// they draw, so its table holds one row for each: its value, its total (`of`), its interval and its
// count; with the compare range, each one's compare value (the compare range's newest reading with
// a value of the same series, and row for a count) and its change, which the drawing prints beside
// each value (D10). The platform's `adapter` is stored and nothing is drawn from it (the plan's
// decision 5d): Dispatch holds no adapter. Pure.
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
  groupTones,
  type MarkState,
  markState,
  specList,
  specObject,
  specText,
  type Tone,
} from "./kind";
import { type DeclaredReading, newestByCell } from "./readings";
import {
  changeValue,
  figureValue,
  intervalWords,
  measuredDay,
  NOTHING,
  notMeasured,
} from "./small-number";

/** Where a small number stands: the headline, beside it, a tile, or a count of a section. */
export type NumberPart = "headline" | "beside" | "tile" | "count";

export interface NumberPoint extends ChartPoint {
  readonly part: NumberPart;
  /** The section it stands in, by its place among the sections; null at the top. */
  readonly section: number | null;
  /** The series it is the newest reading of, and a count's row ("" for the rest). */
  readonly series: string;
  readonly row: string;
  /** A tile's `state`, which colours it. */
  readonly state: MarkState | null;
  /** A count's dot: its row's colour by its place in the counts' `order`. */
  readonly tone: Tone | null;
  /** Its reading's day: always beside the headline and a number beside it, and on a tile or a
   *  count where it is not the day of the declaration's newest reading with a value. */
  readonly date: string | null;
  /** The words after its interval, where its reading has one. */
  readonly interval: string | null;
}

export interface NumberSeries extends ChartSeries {
  readonly points: readonly NumberPoint[];
}

/** A titled group of small numbers. On the platform's page, as on the tab, it stands after the
 *  chart `after` names (`numberParts`), and names that chart (`data-after`). */
export interface NumberSection {
  readonly title: string;
  readonly subtitle: string | null;
  readonly after: string;
  /** The series its counts are of, and the line that stands in their place where no reading of
   *  it has a value. */
  readonly counts: string | null;
  readonly countsEmpty: string | null;
}

export interface NumberModel extends ChartModel {
  readonly number: {
    /** Whether the top draws `tiles`, rather than the headline and the numbers beside it. */
    readonly tiles: boolean;
    readonly sections: readonly NumberSection[];
  };
  readonly series: readonly NumberSeries[];
}

export function isNumberModel(model: ChartModel): model is NumberModel {
  return "number" in model;
}

/** A section of a number drawn alone: the chart its `after` names, and its model. */
export interface NumberSectionPart {
  readonly after: string;
  readonly model: NumberModel;
}

/**
 * A number in the parts the platform's page draws apart (spec R20): its top (its tiles, or its
 * headline and the numbers beside it), which stands in the number's own place, and each section,
 * which the declared eval draws after the chart its `after` names. Each part is a model of its
 * own, so each draws through `EvalChart` and its table holds the numbers it draws: the top keeps
 * the number's name and title, and a section takes its own title and the name
 * `<the number's name>/<its place among the sections>`.
 */
export function numberParts(model: NumberModel): {
  top: NumberModel;
  sections: NumberSectionPart[];
} {
  const [series] = model.series;
  const points = series?.points ?? [];
  const part = (
    name: string,
    title: string,
    own: readonly NumberPoint[],
    sections: readonly NumberSection[]
  ): NumberModel => ({
    ...model,
    name,
    number: { ...model.number, sections },
    series:
      series === undefined
        ? []
        : [
            {
              ...series,
              points: own,
              statistics: series.statistics.filter(({ id }) =>
                own.some((point) => id in point.values)
              ),
            },
          ],
    title,
  });
  return {
    sections: model.number.sections.map((section, index) => ({
      after: section.after,
      model: part(
        `${model.name}/${index}`,
        section.title,
        points
          .filter((point) => point.section === index)
          .map((point) => ({ ...point, section: 0 })),
        [section]
      ),
    })),
    top: part(
      model.name,
      model.title,
      points.filter((point) => point.section === null),
      []
    ),
  };
}

/** Each text of a spec field that is a list. */
function names(value: unknown): string[] {
  return specList(value).filter((name): name is string => typeof name === "string");
}

/** The series a number reads: every series its declaration lists. */
export function numberReads(spec: DeclaredSpec): ChartRead[] {
  return [...new Set(names(spec.series))].map((series) => ({ series }));
}

/** A small number as the declaration names it. */
interface Place {
  readonly part: NumberPart;
  readonly section: number | null;
  readonly series: string;
  readonly row: string;
  readonly label: string;
  readonly state: MarkState | null;
  readonly tone: Tone | null;
  readonly signed: boolean;
  /** Its total: its reading's own n, or another series' newest value. */
  readonly of: string | null;
  /** Whether it prints its reading's n first on its grey line ("from 90 tasks"). */
  readonly count: boolean;
}

/** A tile of `tiles` or of a section's. */
function tilesOf(value: unknown, section: number | null): Place[] {
  return specList(value).map((item) => {
    const tile = specObject(item);
    const series = specText(tile.series) ?? "";
    return {
      count: tile.count === true,
      label: specText(tile.label) ?? series,
      of: specText(tile.of),
      part: "tile",
      row: "",
      section,
      series,
      signed: tile.signed === true,
      state: markState(tile.state),
      tone: null,
    };
  });
}

/** One range's newest readings: with a value by series, with a value by series and row, and of
 *  any kind by series (a series whose readings have no value names its reason). */
interface Newest {
  readonly bySeries: Map<string, DeclaredReading>;
  readonly byRow: Map<string, DeclaredReading>;
  readonly blank: Map<string, DeclaredReading>;
}

function newestOf(readings: readonly DeclaredReading[], range: "window" | "compare"): Newest {
  const own = readings.filter((reading) => reading.range === range);
  const valued = own.filter((reading) => reading.value !== null);
  return {
    blank: newestByCell(own, (reading) => reading.series),
    byRow: newestByCell(valued, (reading) => `${reading.series}\n${reading.row}`),
    bySeries: newestByCell(valued, (reading) => reading.series),
  };
}

/** What a place draws of one range: each of its figures by statistic, the unit each figure's
 *  change is written in, and the reading it draws. */
interface Figures {
  readonly values: Record<string, ChartValue>;
  readonly units: Record<string, string>;
  readonly reading: DeclaredReading | null;
}

function figuresOf(place: Place, newest: Newest): Figures {
  const reading =
    place.part === "count"
      ? newest.byRow.get(`${place.series}\n${place.row}`)
      : newest.bySeries.get(place.series);
  const values: Record<string, ChartValue> = {};
  const units: Record<string, string> = {};
  if (reading === undefined) {
    const blank = place.part === "count" ? undefined : newest.blank.get(place.series);
    values.value = blank === undefined ? NOTHING : notMeasured(blank);
    if (place.of !== null) values.of = NOTHING;
    if (place.count) values.n = NOTHING;
    return { reading: null, units, values };
  }
  // A tile with a total prints its value bare, and its total with the unit: "9 of 28 statements";
  // a count prints both with theirs: "21 unfixed of 42".
  units.value = reading.unit;
  values.value = figureValue(
    reading.value,
    reading.unit,
    {
      bare: place.part === "tile" && place.of !== null,
      bound: reading.bound,
      signed: place.signed,
    },
    place.part === "tile" && reading.note !== "" ? reading.note : null
  );
  if (place.of === "n") {
    units.of = place.part === "count" ? reading.nUnit : reading.nUnit || reading.unit;
    values.of = figureValue(reading.n, units.of);
  } else if (place.of !== null) {
    const total = newest.bySeries.get(place.of);
    units.of = total?.unit ?? "";
    values.of =
      total === undefined ? NOTHING : figureValue(total.value, total.unit, { bound: total.bound });
  }
  if (reading.ciLow !== null && reading.ciHigh !== null) {
    units.ci_low = reading.unit;
    units.ci_high = reading.unit;
    values.ci_low = figureValue(reading.ciLow, reading.unit, { bare: true });
    values.ci_high = figureValue(reading.ciHigh, reading.unit, { bare: true });
  }
  if (place.count) {
    units.n = reading.nUnit;
    values.n = figureValue(reading.n, reading.nUnit);
  }
  return { reading, units, values };
}

/** A range's figure of `id` where the other range has one and this one has none. */
function missing(id: string): ChartValue {
  return id === "ci_low" || id === "ci_high"
    ? { label: "no interval", note: null, state: "absent", value: null }
    : NOTHING;
}

/** The statistics of the number's table, in their columns' order. */
const STATISTICS: readonly ChartStatistic[] = [
  { id: "value", label: "Value" },
  { id: "of", label: "Of" },
  { id: "ci_low", label: "Interval low" },
  { id: "ci_high", label: "Interval high" },
  { id: "n", label: "Count" },
];

/**
 * The model of a `number` (plan D10) from its declaration and the readings of its series. Each
 * small number is the newest reading with a value of its series (a count's, of its row) in the
 * window, and its compare value the compare range's newest one; a series with readings but none
 * with a value draws "not measured" and its reason, and one with none "no data". A section's
 * counts are its series' rows that hold a value, those `order` names first, each dot in its row's
 * colour by that order (`groupTones`, as an `xy` that declares the same order draws each row).
 */
export function numberModel(spec: DeclaredSpec, input: DeclaredInput): NumberModel {
  const series = names(spec.series);
  const intervalLabel = specText(spec.interval_label);
  const window = newestOf(input.readings, "window");
  const compare = input.compare === null ? null : newestOf(input.readings, "compare");
  const valued = input.readings.filter(
    (reading) => reading.range === "window" && reading.value !== null
  );
  const newestReading = valued[valued.length - 1];
  const newestDay = newestReading === undefined ? null : measuredDay(newestReading);

  const drawnSections = specList(spec.sections).map((item, index) => {
    const section = specObject(item);
    const counts = specObject(section.counts);
    const countsSeries = specText(counts.series);
    const rows =
      countsSeries === null
        ? []
        : [
            ...newestByCell(
              valued.filter((reading) => reading.series === countsSeries),
              (reading) => reading.row
            ).keys(),
          ];
    const subtitle = specText(section.subtitle);
    const drawn: NumberSection = {
      after: specText(section.after) ?? "",
      counts: countsSeries,
      countsEmpty:
        countsSeries !== null && rows.length === 0
          ? `${countsSeries}: no reading with a value in this window.`
          : null,
      subtitle: subtitle === "" ? null : subtitle,
      title: specText(section.title) ?? "",
    };
    const countPlaces = groupTones({ order: counts.order }, rows).map(
      ({ name, tone }): Place => ({
        count: false,
        label: name,
        of: counts.of === "n" ? "n" : null,
        part: "count",
        row: name,
        section: index,
        series: countsSeries ?? "",
        signed: false,
        state: null,
        tone,
      })
    );
    return { drawn, places: [...tilesOf(section.tiles, index), ...countPlaces] };
  });
  // A series a section draws, as a tile, a tile's total or its counts, never draws at the top.
  const owned = new Set(
    drawnSections.flatMap((section) => [
      ...section.places.flatMap((place) =>
        place.of === null || place.of === "n" ? [place.series] : [place.series, place.of]
      ),
      ...(section.drawn.counts === null ? [] : [section.drawn.counts]),
    ])
  );

  const topTiles = tilesOf(spec.tiles, null);
  const plain = (part: NumberPart, name: string): Place => ({
    count: false,
    label: name,
    of: null,
    part,
    row: "",
    section: null,
    series: name,
    signed: false,
    state: null,
    tone: null,
  });
  const top: Place[] =
    topTiles.length > 0
      ? topTiles
      : series.flatMap((name, index) =>
          index === 0 ? [plain("headline", name)] : owned.has(name) ? [] : [plain("beside", name)]
        );

  const places = [...top, ...drawnSections.flatMap((section) => section.places)];
  const points = places.map((place): NumberPoint => {
    const now = figuresOf(place, window);
    const then = compare === null ? null : figuresOf(place, compare);
    const ids = STATISTICS.map(({ id }) => id).filter(
      (id) => id in now.values || (then !== null && id in then.values)
    );
    const values = Object.fromEntries(ids.map((id) => [id, now.values[id] ?? missing(id)]));
    const compareValues =
      then === null
        ? null
        : Object.fromEntries(ids.map((id) => [id, then.values[id] ?? missing(id)]));
    const reading = now.reading;
    const day = reading === null ? null : measuredDay(reading);
    const dated = place.part === "headline" || place.part === "beside" || day !== newestDay;
    return {
      change:
        compareValues === null
          ? null
          : Object.fromEntries(
              ids.map((id) => [
                id,
                changeValue(values[id], compareValues[id], now.units[id] ?? then?.units[id] ?? ""),
              ])
            ),
      compare: compareValues,
      date: dated ? day : null,
      interval:
        reading !== null && reading.ciLow !== null && reading.ciHigh !== null
          ? intervalWords(reading.ciLevel, intervalLabel)
          : null,
      label: place.label,
      part: place.part,
      partial: false,
      row: place.row,
      section: place.section,
      series: place.series,
      state: place.state,
      tone: place.tone,
      values,
      x: place.part === "count" ? `${place.series} \u00b7 ${place.row}` : place.series,
    };
  });

  const title = specText(spec.title) ?? specText(spec.id) ?? "";
  return {
    axis: { label: "Number", many: "numbers", one: "number" },
    comparable: true,
    compareRange: input.compare,
    kind: "number",
    name: declaredChartName(input.slug, spec),
    number: { sections: drawnSections.map((section) => section.drawn), tiles: topTiles.length > 0 },
    series: [
      {
        dataNote: null,
        headline: [],
        label: title,
        name: specText(spec.id) ?? "",
        noun: "readings",
        points,
        repositoryNote: null,
        statistics: STATISTICS.filter(({ id }) => points.some((point) => id in point.values)),
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
    ],
    title,
  };
}
