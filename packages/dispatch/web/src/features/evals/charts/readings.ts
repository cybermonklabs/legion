// The readings a declared chart draws (dispatch://INFRA-2918/artifact/plan-md, D10, "How each kind
// reads the readings write"): each reading row of an eval's items views as one plain record, with
// the reading fields every kind reads named as the platform's chart rules name them, so a kind's
// model reads `rowGroup` and `bound` rather than the readings write's `attributes`. Pure.
import type { EvalItemRow, EvalLink } from "../../../api/types";
import { compareTimestamps, isTimestamp } from "../../../lib/timestamps";

/** A reading's bound: its value is at least (`>=`) or at most (`<=`) the true value's. */
export type Bound = ">=" | "<=";

/** One reading as a declared chart reads it. */
export interface DeclaredReading {
  readonly id: string;
  readonly series: string;
  /** The reading's `row_key`, "" for a reading over time alone. */
  readonly row: string;
  readonly rowKind: string;
  readonly col: string;
  /** `attributes.row_group`, "" where it names none. */
  readonly rowGroup: string;
  /** Null: not measured, for the reason `absentReason` gives. */
  readonly value: number | null;
  readonly absentReason: string;
  readonly unit: string;
  readonly n: number | null;
  readonly nUnit: string;
  readonly ciLow: number | null;
  readonly ciHigh: number | null;
  readonly ciLevel: number | null;
  /** `attributes.bound`, null where the value is the true value. */
  readonly bound: Bound | null;
  readonly note: string;
  readonly source: string;
  /** The reading's own links (D10's `source` and `links` row), which a kind draws where it draws
   *  the reading's row. */
  readonly links: readonly EvalLink[];
  /** When it was measured, as the items read answers it. */
  readonly measuredAt: string;
  /** The same instant in epoch milliseconds, the place a time axis draws it at. */
  readonly measuredMs: number;
  readonly recordedAt: string;
  /** Which range of the header the reading is of. */
  readonly range: "window" | "compare";
  /** The id of the reading it corrects, "" for none. */
  readonly replaces: string;
  readonly attributes: Readonly<Record<string, unknown>>;
}

function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function finite(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

/** The earlier of two readings: by measured time, then by recorded time, then by id. */
export function compareReadings(a: DeclaredReading, b: DeclaredReading): number {
  return (
    compareTimestamps(a.measuredAt, b.measuredAt) ||
    (a.recordedAt !== "" && b.recordedAt !== ""
      ? compareTimestamps(a.recordedAt, b.recordedAt)
      : 0) ||
    (a.id < b.id ? -1 : a.id > b.id ? 1 : 0)
  );
}

/**
 * The readings among an items view's rows, oldest first. A reading is the one row kind whose
 * columns carry its id (`readingItemRow`, api/evals_items.go); the day rows and the rows Dispatch
 * computes behind a number carry none, so they are left out. A reading that another names in
 * `replaces` draws nowhere: the correction draws in its place (the readings write's `replaces`,
 * the platform's `supersedes`). The items read leaves a superseded reading out already; this holds
 * where two pages of one series answer the reading and its correction apart.
 */
export function readingsOf(rows: readonly EvalItemRow[]): DeclaredReading[] {
  const readings = rows.flatMap((row): DeclaredReading[] => {
    const cols = row.cols ?? {};
    const id = cols.id;
    // A time the server did not write could not be ordered (`compareTimestamps`).
    if (typeof id !== "string" || !isTimestamp(row.measured_at)) return [];
    const recordedAt = text(cols.recorded_at);
    const attributes: Readonly<Record<string, unknown>> =
      typeof cols.attributes === "object" && cols.attributes !== null
        ? (cols.attributes as Record<string, unknown>)
        : {};
    const bound = attributes.bound;
    return [
      {
        absentReason: text(cols.value_absent_reason),
        attributes,
        bound: bound === ">=" || bound === "<=" ? bound : null,
        ciHigh: finite(cols.ci_high),
        ciLevel: finite(cols.ci_level),
        ciLow: finite(cols.ci_low),
        col: text(cols.col),
        id,
        links: row.links ?? [],
        measuredAt: row.measured_at,
        measuredMs: Date.parse(row.measured_at),
        n: finite(cols.n),
        nUnit: text(cols.n_unit),
        note: text(cols.note),
        range: row.range,
        recordedAt: isTimestamp(recordedAt) ? recordedAt : "",
        replaces: text(cols.replaces),
        row: row.row_key,
        rowGroup: text(attributes.row_group),
        rowKind: row.row_kind,
        series: text(cols.series),
        source: row.source,
        unit: text(cols.unit),
        value: finite(cols.value),
      },
    ];
  });
  const replaced = new Set(readings.map((reading) => reading.replaces).filter((id) => id !== ""));
  const seen = new Set<string>();
  return readings
    .filter((reading) => {
      if (replaced.has(reading.id) || seen.has(reading.id)) return false;
      seen.add(reading.id);
      return true;
    })
    .sort(compareReadings);
}

/** The newest reading of each cell of `readings`, keyed by `cellOf`, in the order of their cells'
 *  first readings. */
export function newestByCell(
  readings: readonly DeclaredReading[],
  cellOf: (reading: DeclaredReading) => string
): Map<string, DeclaredReading> {
  const newest = new Map<string, DeclaredReading>();
  for (const reading of readings) {
    const cell = cellOf(reading);
    const held = newest.get(cell);
    if (held === undefined || compareReadings(held, reading) <= 0) newest.set(cell, reading);
  }
  return newest;
}
