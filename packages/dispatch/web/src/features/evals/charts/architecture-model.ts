// The model of an `architecture` (dispatch://INFRA-2918/artifact/plan-md, D10; the validator's
// rules, internal/dispatch/evals/charts `architectureFields`): a diagram of a system's parts, each
// part a `part` reading of `value_series` (its row key the part's id) whose attributes place and
// name it: `label`, `column` and `row` (whole numbers from 0), `kind` (`place`, which holds the
// parts whose `contained_in` names it; `kernel`; else a part) and `fault_shown`. A part's value is
// its newest reading with one; the bin of `bins` it falls in colours its box, the higher the
// darker, and it prints with `decimals` decimals. The `numbered` heaviest parts draw a numbered
// badge and list their notes under the diagram. `columns` titles the columns, `bands` the rows
// above the first place and below the last, and `key` gives the key's words for a fault shown and
// a kernel's line. `second_reading`, a second series of the same parts, is the table's second
// series and draws nothing on the diagram. With a compare range, each value prints its compare
// value and its change beside it. `graph`, the eval platform's ledger folder, is accepted and draws
// nothing: Dispatch reads the parts and their edges from readings (plan D10). Pure.
import type { EvalLink } from "../../../api/types";
import {
  type ChartAxis,
  type ChartModel,
  type ChartSeries,
  type ChartValue,
  NO_DATA,
} from "../chart-model";
import {
  EDGE_ROW_KIND,
  edgesOf,
  type GraphEdge,
  graphPoint,
  NO_WINDOW_FIGURES,
  newestByRow,
  plural,
  wholeOf,
  writeNumber,
} from "./graph";
import {
  type ChartRead,
  type DeclaredInput,
  type DeclaredSpec,
  declaredChartName,
  specList,
  specNumber,
  specObject,
  specText,
} from "./kind";
import type { DeclaredReading } from "./readings";

/** The row kind of the readings that are an architecture's parts. */
export const PART_ROW_KIND = "part";

const ARCHITECTURE_AXIS: ChartAxis = { label: "Part", many: "parts", one: "part" };

/** A place holds parts and has no value; a kernel and a part are boxes with one. */
export type PartKind = "place" | "part" | "kernel";

/** The cells a part covers: a place, from its own to the last column and row of the parts it
 *  holds; any other part, its own cell. */
export interface PartSpan {
  readonly column: number;
  readonly row: number;
  readonly lastColumn: number;
  readonly lastRow: number;
}

/** One part of the diagram as it draws. */
export interface ArchitecturePart {
  readonly id: string;
  readonly label: string;
  readonly kind: PartKind;
  /** Its own cell, where its box or a place's title stands. */
  readonly column: number;
  readonly row: number;
  readonly span: PartSpan;
  /** The place that holds it, "" for none. */
  readonly within: string;
  /** Its value's bin among `bins`, 0 the lightest; null where it has no value. */
  readonly bin: number | null;
  /** Its number among the `numbered` heaviest parts, a part tied with one taking its number; null
   *  for none. */
  readonly badge: number | null;
  /** Whether a fault through it has been shown (`fault_shown`). */
  readonly fault: boolean;
  /** A place's: whether an edge reaches it or a part it holds. */
  readonly reached: boolean;
}

/** A band of rows above the first place or below the last, and its title, "" for none. */
export interface ArchitectureBand {
  readonly title: string;
  readonly first: number;
  readonly last: number;
}

/** One of the heaviest parts under the diagram: its badge, its id and the first clause of its
 *  label. Its value and note are its table's. */
export interface HeavyPart {
  readonly badge: number;
  readonly id: string;
  readonly name: string;
}

/** Everything about the diagram's drawing that is not a value of its table. */
export interface ArchitectureDrawing {
  /** Every part of the window, in the order of their cells: column by column, row by row. */
  readonly parts: readonly ArchitecturePart[];
  /** The edges between two parts of the window. */
  readonly edges: readonly GraphEdge[];
  /** Each column's title, "" for none, one for each column. */
  readonly columns: readonly string[];
  readonly rows: number;
  readonly above: ArchitectureBand | null;
  readonly below: ArchitectureBand | null;
  /** The key's words for each bin, lightest first: "up to 1", "1 to 5", …, "over 25". */
  readonly bins: readonly string[];
  /** The key's words for a fault shown, where a part shows one, and for a kernel's line, where an
   *  edge to a kernel draws. */
  readonly fault: string | null;
  readonly kernel: string | null;
  readonly heaviest: readonly HeavyPart[];
  /** The sources its values name (the ledger document's version), each once. */
  readonly sources: readonly string[];
  /** The links its values' readings carry (D10's `links`), which its sources line draws. */
  readonly links: readonly EvalLink[];
  /** The parts the compare range holds and the window does not, by label. */
  readonly compareOnly: readonly string[];
  /** The lines under the diagram: the edges it cannot draw. */
  readonly notes: readonly string[];
}

export interface ArchitectureModel extends ChartModel {
  readonly architecture: ArchitectureDrawing;
}

export function isArchitectureModel(model: ChartModel): model is ArchitectureModel {
  return "architecture" in model;
}

/** An architecture reads its value series' parts, its second reading's, and its eval's edges. */
export function architectureReads(spec: DeclaredSpec): ChartRead[] {
  return [
    ...[specText(spec.value_series), specText(spec.second_reading)].flatMap((series) =>
      series === null ? [] : [{ rowKind: PART_ROW_KIND, series }]
    ),
    { rowKind: EDGE_ROW_KIND },
  ];
}

function attributeText(reading: DeclaredReading, name: string): string {
  const value = reading.attributes[name];
  return typeof value === "string" ? value : "";
}

/** How the diagram writes a value: with `decimals` decimals where the spec gives them, a trailing
 *  zero kept; else, and for 0, a value under 0.001 and one of a million or more, as a chart
 *  writes any number. */
function valueWriter(decimals: number | null): (value: number) => string {
  return (value) =>
    decimals === null ||
    value === 0 ||
    Math.abs(value) < 0.001 ||
    Math.abs(Number(value.toFixed(decimals))) >= 1e6
      ? writeNumber(value)
      : value.toFixed(decimals);
}

/** Each part's own cell. A part takes the column its `column` names (else the first) and the row
 *  its `row` names, or the next free row below it where another part holds that cell; a part that
 *  names no row takes the first free row of its column. The parts that name a row are placed
 *  first, in the order of their readings, so a part that names none never takes a named cell. */
function cellsOf(
  readings: ReadonlyMap<string, DeclaredReading>
): Map<string, { readonly column: number; readonly row: number }> {
  const taken = new Set<string>();
  const cells = new Map<string, { column: number; row: number }>();
  const entries = [...readings];
  const named = entries.filter(([, reading]) => wholeOf(reading.attributes.row) !== null);
  const unnamed = entries.filter(([, reading]) => wholeOf(reading.attributes.row) === null);
  for (const [id, reading] of [...named, ...unnamed]) {
    const column = wholeOf(reading.attributes.column) ?? 0;
    let row = wholeOf(reading.attributes.row) ?? 0;
    while (taken.has(`${column}:${row}`)) row++;
    taken.add(`${column}:${row}`);
    cells.set(id, { column, row });
  }
  return cells;
}

/**
 * The model of an `architecture` (plan D10) from its spec and the readings of its value series,
 * its second reading and its eval's edges. Its first series is the value series: one row of the
 * table for each part and kernel of the window (a place has no value, so no row), in the order of
 * their cells, named by its label (else its id), whose value is its newest reading's with one,
 * with that reading's note; "no data" with the reason its reading gives where it has none. Its
 * second, where the spec names a second reading, is that series at the same parts. With a compare
 * range, each part's values there and their change, "no data" where that range holds no value of
 * it.
 */
export function architectureModel(spec: DeclaredSpec, input: DeclaredInput): ArchitectureModel {
  const valueSeries = specText(spec.value_series) ?? "";
  const secondSeries = specText(spec.second_reading);
  const decimals = wholeOf(spec.decimals);
  const write = valueWriter(decimals);
  const limits = specList(spec.bins).flatMap((limit) => {
    const number = specNumber(limit);
    return number === null ? [] : [number];
  });
  const numbered = wholeOf(spec.numbered) ?? 0;
  const comparing = input.compare !== null;

  const partsOf = (series: string, range: "window" | "compare", valued: boolean) =>
    newestByRow(
      input.readings,
      range,
      (reading) =>
        reading.rowKind === PART_ROW_KIND &&
        reading.series === series &&
        (!valued || reading.value !== null)
    );
  const layout = partsOf(valueSeries, "window", false);
  const values = partsOf(valueSeries, "window", true);
  const compareValues = comparing
    ? partsOf(valueSeries, "compare", true)
    : new Map<string, DeclaredReading>();

  const kindOf = (reading: DeclaredReading): PartKind => {
    const kind = attributeText(reading, "kind");
    return kind === "place" || kind === "kernel" ? kind : "part";
  };
  const labelOf = (id: string, reading: DeclaredReading | undefined): string => {
    const label = reading === undefined ? "" : attributeText(reading, "label");
    return label === "" ? id : label;
  };
  const cells = cellsOf(layout);
  // Its edges are the edge readings of the series its parts are readings of, not another graph's.
  const windowEdges = edgesOf(
    input.readings,
    "window",
    new Set(secondSeries === null ? [valueSeries] : [valueSeries, secondSeries])
  );
  const drawn = windowEdges.edges.filter((edge) => layout.has(edge.from) && layout.has(edge.to));
  const strays = windowEdges.edges.length - drawn.length;

  const binOf = (value: number): number => {
    const index = limits.findIndex((limit) => value <= limit);
    return index < 0 ? limits.length : index;
  };
  const weights = [...layout]
    .filter(([, reading]) => kindOf(reading) !== "place")
    .flatMap(([id]) => {
      const value = values.get(id)?.value;
      return value === undefined || value === null ? [] : [value];
    });
  const badgeOf = (value: number | null): number | null => {
    if (value === null) return null;
    const rank = 1 + weights.filter((weight) => weight > value).length;
    return rank <= numbered ? rank : null;
  };

  const parts = [...layout].map(([id, reading]): ArchitecturePart => {
    const kind = kindOf(reading);
    const cell = cells.get(id) ?? { column: 0, row: 0 };
    const value = kind === "place" ? null : (values.get(id)?.value ?? null);
    const held = [...layout].filter(([, part]) => attributeText(part, "contained_in") === id);
    const heldCells = held.flatMap(([heldId]) => {
      const heldCell = cells.get(heldId);
      return heldCell === undefined ? [] : [heldCell];
    });
    const fault = reading.attributes.fault_shown;
    return {
      badge: kind === "place" ? null : badgeOf(value),
      bin: value === null ? null : binOf(value),
      column: cell.column,
      fault: fault === true || fault === "true",
      id,
      kind,
      label: labelOf(id, reading),
      reached:
        kind === "place" &&
        drawn.some((edge) => edge.to === id || held.some(([heldId]) => heldId === edge.to)),
      row: cell.row,
      span:
        kind === "place"
          ? {
              column: Math.min(cell.column, ...heldCells.map((one) => one.column)),
              lastColumn: Math.max(cell.column, ...heldCells.map((one) => one.column)),
              lastRow: Math.max(cell.row, ...heldCells.map((one) => one.row)),
              row: cell.row,
            }
          : { column: cell.column, lastColumn: cell.column, lastRow: cell.row, row: cell.row },
      within: attributeText(reading, "contained_in"),
    };
  });
  parts.sort((a, b) => a.column - b.column || a.row - b.row);

  const rows = Math.max(0, ...parts.map((part) => part.span.lastRow + 1));
  const titles = specList(spec.columns).map((title) => specText(title) ?? "");
  const columnCount = Math.max(titles.length, ...parts.map((part) => part.span.lastColumn + 1));
  const places = parts.filter((part) => part.kind === "place");
  const bands = specObject(spec.bands);
  const firstPlace = Math.min(...places.map((place) => place.row));
  const lastPlace = Math.max(...places.map((place) => place.span.lastRow));
  const key = specObject(spec.key);

  const drawnValue = (
    reading: DeclaredReading | undefined,
    newest: DeclaredReading | undefined
  ): ChartValue =>
    reading === undefined || reading.value === null
      ? {
          label: NO_DATA,
          note: newest === undefined || newest.absentReason === "" ? null : newest.absentReason,
          state: "absent",
          value: null,
        }
      : {
          label: write(reading.value),
          note: reading.note === "" ? null : reading.note,
          state: "plain",
          value: reading.value,
        };
  const boxes = parts.filter((part) => part.kind !== "place");
  const seriesOf = (
    name: string,
    window: ReadonlyMap<string, DeclaredReading>,
    latest: ReadonlyMap<string, DeclaredReading>,
    compare: ReadonlyMap<string, DeclaredReading>
  ): ChartSeries => {
    const unit = [...window.values()].find((reading) => reading.unit !== "")?.unit ?? "";
    return {
      dataNote: null,
      headline: [],
      label: name,
      name,
      noun: "parts",
      points: boxes.map((part) => {
        const then = compare.get(part.id);
        return graphPoint(
          part.id,
          part.label,
          { value: drawnValue(window.get(part.id), latest.get(part.id)) },
          then === undefined ? null : { value: drawnValue(then, then) },
          comparing,
          write
        );
      }),
      repositoryNote: null,
      statistics: [{ id: "value", label: unit === "" ? "Value" : `Value (${unit})` }],
      target: null,
      unit,
      window: NO_WINDOW_FIGURES,
    };
  };
  const series = [
    seriesOf(valueSeries, values, layout, compareValues),
    ...(secondSeries === null
      ? []
      : [
          seriesOf(
            secondSeries,
            partsOf(secondSeries, "window", true),
            partsOf(secondSeries, "window", false),
            comparing ? partsOf(secondSeries, "compare", true) : new Map<string, DeclaredReading>()
          ),
        ]),
  ];

  const compareParts = comparing
    ? partsOf(valueSeries, "compare", false)
    : new Map<string, DeclaredReading>();
  const highest = limits.at(-1);
  return {
    architecture: {
      above:
        places.length > 0 && firstPlace > 0
          ? { first: 0, last: firstPlace - 1, title: specText(bands.above) ?? "" }
          : null,
      below:
        places.length > 0 && lastPlace < rows - 1
          ? { first: lastPlace + 1, last: rows - 1, title: specText(bands.below) ?? "" }
          : null,
      bins:
        highest === undefined
          ? []
          : [
              ...limits.map((limit, index) => {
                const below = limits[index - 1];
                return below === undefined
                  ? `up to ${writeNumber(limit)}`
                  : `${writeNumber(below)} to ${writeNumber(limit)}`;
              }),
              `over ${writeNumber(highest)}`,
            ],
      columns: Array.from({ length: columnCount }, (_, index) => titles[index] ?? ""),
      compareOnly: [...compareParts]
        .filter(([id]) => !layout.has(id))
        .map(([id, reading]) => labelOf(id, reading)),
      edges: drawn,
      fault: parts.some((part) => part.fault)
        ? (specText(key.fault_shown) ?? "a fault through this part has been shown")
        : null,
      heaviest: boxes
        .flatMap((part) =>
          part.badge === null
            ? []
            : [{ badge: part.badge, id: part.id, name: part.label.split(/[,;]/)[0]?.trim() ?? "" }]
        )
        .sort((a, b) => a.badge - b.badge),
      kernel: drawn.some((edge) => edge.kind === "kernel")
        ? (specText(key.kernel) ?? "a kernel's line")
        : null,
      links: [...values.values()].flatMap((reading) => reading.links),
      notes: [
        ...(strays === 0
          ? []
          : [`${plural(strays, "edge")} not drawn: an end names no part of the window.`]),
        ...(windowEdges.unnamed === 0
          ? []
          : [
              `${plural(windowEdges.unnamed, "edge reading")} not drawn: no from and to in its attributes.`,
            ]),
      ],
      parts,
      rows,
      sources: [...new Set([...values.values()].map((reading) => reading.source))]
        .filter((source) => source !== "")
        .sort(),
    },
    axis: ARCHITECTURE_AXIS,
    comparable: true,
    compareRange: input.compare,
    kind: "architecture",
    name: declaredChartName(input.slug, spec),
    series,
    title: specText(spec.title) ?? specText(spec.id) ?? "",
  };
}
