// The model of a `network` (dispatch://INFRA-2918/artifact/plan-md, D10; the validator's rules,
// internal/dispatch/evals/charts `networkFields`): each node the window's `node` readings name
// (its row key), at its newest reading, with the attributes that reading carries, and the window's
// `edge` readings between those nodes. The spec names the attribute that sizes a node (`size`),
// the one that colours it (`color`) and the others a reader may colour by instead (`selector`),
// the one that outlines it (`outline`) and the one that names it (`label`, else its id), and the
// words each attribute and value reads as (`labels`, `empty_label`, `not_reported`,
// `value_labels`, `ratio`). A node stands one column right of the furthest node whose edge reaches
// it, so the edges run left to right where they make no cycle. With a compare range, a node's
// values there are its compare values, which the renderer draws as a dashed ring of its size and
// colour there and the table lists beside its own. `graph`, the eval platform's bucket of graph
// files, is accepted and draws nothing: Dispatch reads the network from readings (plan D10). Pure.
import type { ChartAxis, ChartModel, ChartSeries, ChartValue } from "../chart-model";
import {
  EDGE_ROW_KIND,
  edgesOf,
  type GraphEdge,
  graphPoint,
  NO_WINDOW_FIGURES,
  newestByRow,
  plural,
  writeNumber,
} from "./graph";
import {
  type ChartRead,
  type DeclaredInput,
  type DeclaredSpec,
  declaredChartName,
  PALETTE_SIZE,
  specList,
  specObject,
  specText,
  type Tone,
} from "./kind";
import type { DeclaredReading } from "./readings";

/** The row kind of the readings that are a network's nodes. */
export const NODE_ROW_KIND = "node";

/** The statistic of a node's row that lists the nodes its edges reach; `@` begins no attribute's
 *  name, so it is no attribute's. */
export const LINKS = "@links";

/** A node's least and most radius, in pixels: a node of no size, or of the least, draws at the
 *  least; the largest size of the window and the compare range draws at the most. */
export const NODE_RADIUS = { least: 6, most: 20 } as const;

/** How many nodes no edge touches stand in one row, under the nodes the edges place, where the
 *  edges place fewer columns. */
const LOOSE_PER_ROW = 6;

const NETWORK_AXIS: ChartAxis = { label: "Node", many: "nodes", one: "node" };

/** One value of a colouring, with its colour: its place among the colouring's values, or the grey
 *  of "other" past the palette's and for a node that holds none. Two values that read as the same
 *  words (true and "yes" under one `value_labels.yes`) are one value, as a reader sees them. */
export interface NetworkCategory {
  /** The value's words, "" for a node that holds none. */
  readonly key: string;
  readonly label: string;
  readonly tone: Tone;
}

/** An attribute the nodes may be coloured by, with its words and each value the drawn nodes hold
 *  of it, in either range: in order, and the value of a node that holds none last. */
export interface Colouring {
  readonly attribute: string;
  readonly label: string;
  readonly categories: readonly NetworkCategory[];
}

/** One node as the graph draws it. */
export interface NetworkNode {
  readonly id: string;
  readonly label: string;
  readonly column: number;
  readonly row: number;
  /** Its radius from its size, and whether its size attribute holds a number of 0 or more. */
  readonly radius: number;
  readonly sized: boolean;
  /** Its radius from its size in the compare range, the least where it has no size there; null
   *  where that range holds no reading of it. */
  readonly compareRadius: number | null;
  readonly outlined: boolean;
  /** Its value's key under each colouring, by attribute; the compare range's, null where that
   *  range holds no reading of it. */
  readonly categories: Readonly<Record<string, string>>;
  readonly compareCategories: Readonly<Record<string, string>> | null;
}

/** An attribute of the spec and the words it reads as. */
export interface NamedAttribute {
  readonly attribute: string;
  readonly label: string;
}

/** Everything about the network's drawing that is not a value of its table. */
export interface NetworkDrawing {
  readonly nodes: readonly NetworkNode[];
  /** The edges between two nodes of the window. */
  readonly edges: readonly GraphEdge[];
  readonly columns: number;
  readonly rows: number;
  readonly size: NamedAttribute;
  readonly outline: NamedAttribute | null;
  /** The spec's `color` first, then each of its `selector`. */
  readonly colourings: readonly Colouring[];
  /** The nodes the compare range holds and the window does not, by name. */
  readonly compareOnly: readonly string[];
  /** The lines under the graph: the edges it cannot draw. */
  readonly notes: readonly string[];
}

export interface NetworkModel extends ChartModel {
  readonly network: NetworkDrawing;
}

export function isNetworkModel(model: ChartModel): model is NetworkModel {
  return "network" in model;
}

/** A network reads every node and edge reading of its eval: its spec names no series. */
export function networkReads(): ChartRead[] {
  return [{ rowKind: NODE_ROW_KIND }, { rowKind: EDGE_ROW_KIND }];
}

/** The words a network's spec gives its attributes and values. */
interface Words {
  readonly labels: DeclaredSpec;
  readonly empty: string;
  readonly notReported: readonly string[];
  readonly yes: string | null;
  readonly no: string | null;
  readonly partial: string | null;
  /** `ratio`, which writes the colour attribute's value as "<of> of <out_of> <noun>". */
  readonly ratio: { readonly of: string; readonly outOf: string; readonly noun: string } | null;
  readonly colour: string;
}

function textList(value: unknown): string[] {
  return specList(value).filter((one): one is string => typeof one === "string" && one !== "");
}

function nonBlank(value: unknown): string | null {
  const text = specText(value);
  return text === null || text.trim() === "" ? null : text;
}

function wordsOf(spec: DeclaredSpec): Words {
  const values = specObject(spec.value_labels);
  const ratio = spec.ratio === null ? null : specObject(spec.ratio);
  const of = specText(ratio?.of);
  const outOf = specText(ratio?.out_of);
  return {
    colour: specText(spec.color) ?? "",
    empty: nonBlank(spec.empty_label) ?? "no value",
    labels: specObject(spec.labels),
    no: nonBlank(values.no),
    notReported: textList(spec.not_reported),
    partial: nonBlank(values.partial),
    ratio: of === null || outOf === null ? null : { noun: specText(ratio?.noun) ?? "", of, outOf },
    yes: nonBlank(values.yes),
  };
}

/** A value no node holds: none, blank words, or a list or an object, which reads as none. */
function missing(value: unknown): boolean {
  return (
    value === null ||
    value === undefined ||
    typeof value === "object" ||
    (typeof value === "string" && value.trim() === "")
  );
}

/** An attribute's number: a finite number, or words that are one; else null. */
function numberOf(value: unknown): number | null {
  if (typeof value === "number") return Number.isFinite(value) ? value : null;
  if (typeof value !== "string" || value.trim() === "") return null;
  const number = Number(value);
  return Number.isFinite(number) ? number : null;
}

function plainText(value: unknown): string {
  return typeof value === "number" ? writeNumber(value) : String(value);
}

/** An attribute's words: the spec's `labels`, else its name with its first letter raised and each
 *  underscore a space. */
function fieldLabel(words: Words, attribute: string): string {
  const declared = nonBlank(words.labels[attribute]);
  if (declared !== null) return declared;
  const spaced = attribute.replaceAll("_", " ");
  return `${spaced.charAt(0).toUpperCase()}${spaced.slice(1)}`;
}

/** A value's words: `value_labels`' yes for true or "yes", its no for false or "no", and its
 *  partial before a fraction between 0 and 1; else the value as written. Attributes are JSON, so
 *  a true attribute is true, never the 1 or 0 a count may hold, which the eval platform's graph
 *  files, read from CSV, would also read as yes or no. */
function valueText(words: Words, value: unknown): string {
  if (words.yes !== null && (value === true || value === "yes")) return words.yes;
  if (words.no !== null && (value === false || value === "no")) return words.no;
  if (words.partial !== null && typeof value === "number" && value > 0 && value < 1) {
    return `${words.partial} (${writeNumber(value)})`;
  }
  return plainText(value);
}

/** What a node of no value of `attribute` reads: "not reported" for an attribute `not_reported`
 *  names, else `empty_label`; never 0. */
function emptyText(words: Words, attribute: string): string {
  return words.notReported.includes(attribute) ? "not reported" : words.empty;
}

/** A node's value of `attribute`, as its table and its graph write it. */
function attributeValue(
  words: Words,
  attributes: Readonly<Record<string, unknown>>,
  attribute: string
): ChartValue {
  const raw = attributes[attribute];
  if (attribute === words.colour && words.ratio !== null) {
    const of = attributes[words.ratio.of];
    const outOf = attributes[words.ratio.outOf];
    if (!missing(of) && !missing(outOf)) {
      return {
        label: `${plainText(of)} of ${plainText(outOf)} ${words.ratio.noun}`,
        note: null,
        state: "plain",
        value: missing(raw) ? null : numberOf(raw),
      };
    }
  }
  if (missing(raw)) {
    return { label: emptyText(words, attribute), note: null, state: "absent", value: null };
  }
  return { label: valueText(words, raw), note: null, state: "plain", value: numberOf(raw) };
}

/** Whether a node's outline attribute says yes: true, 1, or the words yes or true. */
function saysYes(value: unknown): boolean {
  const said = typeof value === "string" ? value.trim().toLowerCase() : value;
  return said === true || said === 1 || said === "yes" || said === "true";
}

/** Each node's column and row. A node an edge reaches stands one column right of the furthest
 *  node whose edge reaches it, the nodes placed in the order of their first readings and, where
 *  the edges make a cycle, from its first node not yet placed; each column's nodes stand in that
 *  order. The nodes no edge touches stand under them, as many to a row as the columns the edges
 *  place, or `LOOSE_PER_ROW` where they place fewer. */
function layout(
  ids: readonly string[],
  edges: readonly GraphEdge[]
): {
  readonly places: ReadonlyMap<string, { readonly column: number; readonly row: number }>;
  readonly columns: number;
  readonly rows: number;
} {
  const joining = edges.filter((edge) => edge.from !== edge.to);
  const into = new Map(ids.map((id) => [id, [] as string[]]));
  for (const edge of joining) into.get(edge.to)?.push(edge.from);
  const linked = new Set(joining.flatMap((edge) => [edge.from, edge.to]));
  const columnOf = new Map<string, number>();
  const waiting = ids.filter((id) => linked.has(id));
  while (waiting.length > 0) {
    const ready = waiting.findIndex((id) =>
      (into.get(id) ?? []).every((from) => columnOf.has(from))
    );
    const [id] = waiting.splice(Math.max(ready, 0), 1);
    if (id === undefined) break;
    const after = (into.get(id) ?? []).flatMap((from) => {
      const column = columnOf.get(from);
      return column === undefined ? [] : [column + 1];
    });
    columnOf.set(id, Math.max(0, ...after));
  }
  const places = new Map<string, { column: number; row: number }>();
  const taken: number[] = [];
  for (const id of ids) {
    const column = columnOf.get(id);
    if (column === undefined) continue;
    const row = taken[column] ?? 0;
    taken[column] = row + 1;
    places.set(id, { column, row });
  }
  const linkedRows = Math.max(0, ...taken.map((rows) => rows ?? 0));
  const loose = ids.filter((id) => !columnOf.has(id));
  const width = Math.max(taken.length, Math.min(loose.length, LOOSE_PER_ROW), 1);
  loose.forEach((id, index) => {
    places.set(id, { column: index % width, row: linkedRows + Math.floor(index / width) });
  });
  return {
    columns: loose.length === 0 ? taken.length : width,
    places,
    rows: linkedRows + Math.ceil(loose.length / width),
  };
}

/**
 * The model of a `network` (plan D10) from its spec and its eval's node and edge readings. Its one
 * series is the nodes: one place per node of the window, in the order of their first readings,
 * named by its label attribute (else its id), whose values are its size, colour, `selector` and
 * outline attributes and the nodes its edges reach, each in the words the spec gives it; with a
 * compare range, each node's values in that range and their change, "no data" where that range
 * holds no reading of it.
 */
export function networkModel(spec: DeclaredSpec, input: DeclaredInput): NetworkModel {
  const words = wordsOf(spec);
  const size = specText(spec.size) ?? "";
  const outline = specText(spec.outline);
  const labelAttribute = specText(spec.label);
  const colourAttributes = [...new Set([words.colour, ...textList(spec.selector)])].filter(
    (attribute) => attribute !== ""
  );
  const attributes = [
    ...new Set([size, ...colourAttributes, ...(outline === null ? [] : [outline])]),
  ].filter((attribute) => attribute !== "");

  const isNode = (reading: DeclaredReading) => reading.rowKind === NODE_ROW_KIND;
  const comparing = input.compare !== null;
  const window = newestByRow(input.readings, "window", isNode);
  const compare = comparing
    ? newestByRow(input.readings, "compare", isNode)
    : new Map<string, DeclaredReading>();
  const ids = [...window.keys()];
  const nameOf = (id: string, reading: DeclaredReading | undefined): string => {
    const raw = labelAttribute === null ? undefined : reading?.attributes[labelAttribute];
    return missing(raw) ? id : plainText(raw);
  };
  const names = new Map(ids.map((id) => [id, nameOf(id, window.get(id))]));

  // Its edges are the edge readings of the series its nodes are readings of, not another graph's.
  const nodeSeries = new Set(input.readings.filter(isNode).map((reading) => reading.series));
  const windowEdges = edgesOf(input.readings, "window", nodeSeries);
  const compareEdges = comparing ? edgesOf(input.readings, "compare", nodeSeries).edges : [];
  const drawn = windowEdges.edges.filter((edge) => window.has(edge.from) && window.has(edge.to));
  const strays = windowEdges.edges.length - drawn.length;
  const { columns, places, rows } = layout(ids, drawn);

  const sizeOf = (reading: DeclaredReading | undefined): number | null => {
    const value = reading === undefined ? null : numberOf(reading.attributes[size]);
    return value !== null && value >= 0 ? value : null;
  };
  const largest = Math.max(
    0,
    ...ids.flatMap((id) => [sizeOf(window.get(id)), sizeOf(compare.get(id))]).map((v) => v ?? 0)
  );
  const radiusOf = (value: number | null): number =>
    value === null || largest === 0
      ? NODE_RADIUS.least
      : Math.round(
          (NODE_RADIUS.least +
            (NODE_RADIUS.most - NODE_RADIUS.least) * Math.sqrt(value / largest)) *
            10
        ) / 10;

  // A value's key among a colouring's values: its words, "" where the node holds none.
  const keyOf = (value: unknown): string => (missing(value) ? "" : valueText(words, value));
  const colourings: Colouring[] = colourAttributes.map((attribute) => {
    const keys = new Set(
      ids.flatMap((id) =>
        [window.get(id), compare.get(id)].flatMap((reading) =>
          reading === undefined ? [] : [keyOf(reading.attributes[attribute])]
        )
      )
    );
    const held = [...keys]
      .filter((key) => key !== "")
      .sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
    return {
      attribute,
      categories: [
        ...held.map((key, index) => ({
          key,
          label: key,
          tone: index < PALETTE_SIZE ? index : ("other" as const),
        })),
        ...(keys.has("")
          ? [{ key: "", label: emptyText(words, attribute), tone: "other" as const }]
          : []),
      ],
      label: fieldLabel(words, attribute),
    };
  });
  const categoriesOf = (reading: DeclaredReading): Record<string, string> =>
    Object.fromEntries(
      colourAttributes.map((attribute) => [attribute, keyOf(reading.attributes[attribute])])
    );

  const nodes = [...window].map(([id, own]): NetworkNode => {
    const then = compare.get(id);
    const place = places.get(id) ?? { column: 0, row: 0 };
    return {
      categories: categoriesOf(own),
      column: place.column,
      compareCategories: then === undefined ? null : categoriesOf(then),
      compareRadius: then === undefined ? null : radiusOf(sizeOf(then)),
      id,
      label: names.get(id) ?? id,
      outlined: outline !== null && saysYes(own.attributes[outline]),
      radius: radiusOf(sizeOf(own)),
      row: place.row,
      sized: sizeOf(own) !== null,
    };
  });

  const linksOf = (
    edges: readonly GraphEdge[],
    id: string,
    labels: ReadonlyMap<string, string>
  ): ChartValue => {
    const reached = edges
      .filter((edge) => edge.from === id)
      .map((edge) => labels.get(edge.to) ?? edge.to);
    return reached.length === 0
      ? { label: "none", note: null, state: "absent", value: null }
      : { label: reached.join(", "), note: null, state: "plain", value: null };
  };
  const compareNames = new Map([
    ...[...compare].map(([id, reading]): [string, string] => [id, nameOf(id, reading)]),
    ...names,
  ]);
  const valuesOf = (
    reading: DeclaredReading,
    edges: readonly GraphEdge[],
    labels: ReadonlyMap<string, string>
  ): Record<string, ChartValue> =>
    Object.fromEntries([
      ...attributes.map((attribute) => [
        attribute,
        attributeValue(words, reading.attributes, attribute),
      ]),
      [LINKS, linksOf(edges, reading.row, labels)],
    ]);
  const points = [...window].map(([id, own]) => {
    const then = compare.get(id);
    return graphPoint(
      id,
      names.get(id) ?? id,
      valuesOf(own, windowEdges.edges, names),
      then === undefined ? null : valuesOf(then, compareEdges, compareNames),
      comparing
    );
  });

  const series: ChartSeries = {
    dataNote: null,
    headline: [],
    label: "Nodes",
    name: "nodes",
    noun: "nodes",
    points,
    repositoryNote: null,
    statistics: [
      ...attributes.map((attribute) => ({ id: attribute, label: fieldLabel(words, attribute) })),
      { id: LINKS, label: "Links to" },
    ],
    target: null,
    unit: "",
    window: NO_WINDOW_FIGURES,
  };
  const notes = [
    ...(strays === 0
      ? []
      : [`${plural(strays, "edge")} not drawn: an end names no node of the window.`]),
    ...(windowEdges.unnamed === 0
      ? []
      : [
          `${plural(windowEdges.unnamed, "edge reading")} not drawn: no from and to in its attributes.`,
        ]),
  ];

  return {
    axis: NETWORK_AXIS,
    comparable: true,
    compareRange: input.compare,
    kind: "network",
    name: declaredChartName(input.slug, spec),
    network: {
      colourings,
      columns,
      compareOnly: [...compare.keys()]
        .filter((id) => !window.has(id))
        .map((id) => nameOf(id, compare.get(id))),
      edges: drawn,
      nodes,
      notes,
      outline: outline === null ? null : { attribute: outline, label: fieldLabel(words, outline) },
      rows,
      size: { attribute: size, label: fieldLabel(words, size) },
    },
    series: [series],
    title: specText(spec.title) ?? specText(spec.id) ?? "",
  };
}
