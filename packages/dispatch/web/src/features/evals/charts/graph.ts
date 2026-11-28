// What a `network` and an `architecture` draw from (dispatch://INFRA-2918/artifact/plan-md, D10,
// "How each kind reads the readings write"): the newest reading of each node or part a range
// holds, and the edges between them, each an `edge` row that names its two ends in
// `attributes.from` and `attributes.to` (its row key is `<from>→<to>`); and the change of a value
// from the compare range's to the window's, which both kinds' tables hold beside each value. Pure.
import { type ChartPoint, type ChartValue, NO_DATA, writerOf } from "../chart-model";
import { type DeclaredReading, newestByCell } from "./readings";

/** No reading of the node or part in the range. */
const ABSENT: ChartValue = { label: NO_DATA, note: null, state: "absent", value: null };

/** Which range of the header a reading is of. */
type ReadingRange = DeclaredReading["range"];

/** The row kind of the readings that are a graph's edges. */
export const EDGE_ROW_KIND = "edge";

/** A number as a graph writes it: whole, or to two decimals. */
export function writeNumber(value: number): string {
  return writerOf("", "value")(value)(0);
}

/** "1 edge", "2 edges". */
export function plural(count: number, one: string): string {
  return `${count} ${one}${count === 1 ? "" : "s"}`;
}

/** A spec field that is a whole number at least 0, as a number or as its digits; else null. */
export function wholeOf(value: unknown): number | null {
  if (typeof value === "number") return Number.isInteger(value) && value >= 0 ? value : null;
  return typeof value === "string" && /^\d+$/.test(value) ? Number(value) : null;
}

/** The newest reading of each row (its row key) among the readings of `range` that `keep` keeps,
 *  in the order of their rows' first readings. */
export function newestByRow(
  readings: readonly DeclaredReading[],
  range: ReadingRange,
  keep: (reading: DeclaredReading) => boolean
): Map<string, DeclaredReading> {
  return newestByCell(
    readings.filter((reading) => reading.range === range && reading.row !== "" && keep(reading)),
    (reading) => reading.row
  );
}

/** One edge of a graph: its two ends, and the kind and the words its reading's attributes give
 *  it ("" where they give none). */
export interface GraphEdge {
  readonly from: string;
  readonly to: string;
  readonly kind: string;
  readonly label: string;
}

function attributeText(attributes: Readonly<Record<string, unknown>>, name: string): string {
  const value = attributes[name];
  return typeof value === "string" ? value : "";
}

/** The edges of `range` among the edge readings of `series`, the series a graph's nodes or parts
 *  are readings of: the newest `edge` reading of each pair of ends, in the order of their first
 *  readings; and how many of those edge readings name no two ends, which no graph can draw. An
 *  eval's charts read one `edge` view, so the series keeps a network and an architecture of one
 *  eval from drawing or counting each other's edges. */
export function edgesOf(
  readings: readonly DeclaredReading[],
  range: ReadingRange,
  series: ReadonlySet<string>
): { readonly edges: readonly GraphEdge[]; readonly unnamed: number } {
  let unnamed = 0;
  const named = readings.filter((reading) => {
    if (
      reading.range !== range ||
      reading.rowKind !== EDGE_ROW_KIND ||
      !series.has(reading.series)
    ) {
      return false;
    }
    const ends =
      attributeText(reading.attributes, "from") !== "" &&
      attributeText(reading.attributes, "to") !== "";
    if (!ends) unnamed++;
    return ends;
  });
  const newest = newestByCell(
    named,
    (reading) =>
      `${attributeText(reading.attributes, "from")}\u2192${attributeText(reading.attributes, "to")}`
  );
  const edges = [...newest.values()].map((reading) => ({
    from: attributeText(reading.attributes, "from"),
    kind: attributeText(reading.attributes, "kind"),
    label: attributeText(reading.attributes, "label"),
    to: attributeText(reading.attributes, "to"),
  }));
  return { edges, unnamed };
}

/** The change from `then`, the compare range's value, to `now`, the window's: for two numbers
 *  their difference, written with its sign by `write`; for two words, whether they differ;
 *  nothing where either range has nothing to compare. */
function changeOf(
  now: ChartValue,
  then: ChartValue,
  write: (value: number) => string = writeNumber
): ChartValue {
  if (now.label === NO_DATA || then.label === NO_DATA) return ABSENT;
  if (now.value !== null && then.value !== null) {
    const change = now.value - then.value;
    const label =
      change === 0 ? "no change" : `${change > 0 ? "+" : "\u2212"}${write(Math.abs(change))}`;
    return { label, note: null, state: "plain", value: change };
  }
  return {
    label: now.label === then.label ? "no change" : "changed",
    note: null,
    state: "plain",
    value: null,
  };
}

/** A place of a graph's table: its own values, and with a compare range the compare range's and
 *  their change (each number written by `write`), every statistic "no data" where the compare
 *  range holds no reading of it. */
export function graphPoint(
  x: string,
  label: string,
  values: Readonly<Record<string, ChartValue>>,
  compare: Readonly<Record<string, ChartValue>> | null,
  comparing: boolean,
  write: (value: number) => string = writeNumber
): ChartPoint {
  const ids = Object.keys(values);
  const then = comparing ? (compare ?? Object.fromEntries(ids.map((id) => [id, ABSENT]))) : null;
  return {
    change:
      then === null
        ? null
        : Object.fromEntries(
            ids.map((id) => [id, changeOf(values[id] ?? ABSENT, then[id] ?? ABSENT, write)])
          ),
    compare: then,
    label,
    partial: false,
    values,
    x,
  };
}

/** A series' figures over the whole window, which a graph draws none of. */
export const NO_WINDOW_FIGURES: ChartPoint = {
  change: null,
  compare: null,
  label: "Window",
  partial: false,
  values: {},
  x: "window",
};
