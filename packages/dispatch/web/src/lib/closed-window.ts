/**
 * The Closed window (`f.closed`): what the address names, which rows it holds, how a date picker
 * writes it and shows it, and what the list route is asked for it. The table model narrows rows
 * by it, the Closed filter (`components/RangeFilter.tsx`) writes it, and the Done reads
 * (`api/queries.ts`) send it.
 */

/** An hour in milliseconds. */
export const HOUR_MS = 3_600_000;

/** A day in milliseconds. */
const DAY_MS = 24 * HOUR_MS;

/** A Closed window (`f.closed`), as the Done reads take it: its first moment (`from`, included)
 *  and its end (`to`, left out), in milliseconds since the epoch, either one null where the
 *  window leaves it open. `rolling` is a window of the last N days (`f.closed=<N>d`), whose first
 *  moment moves with the clock; a read of every done row starts it on the hour (`hourStart`).
 *  `readWindow` reads the address into this. */
export interface ClosedWindow {
  readonly from: number | null;
  readonly to: number | null;
  readonly rolling: boolean;
}

/** Whether a Closed window holds no moment: its end at or before its first moment, as an address
 *  whose to day comes before its from day names. No close falls in it, and the list route refuses
 *  it (`INVALID_CLOSED_WINDOW`), so a read of it asks the route nothing and lists no row. */
export function holdsNoMoment(window: ClosedWindow | undefined): boolean {
  return (
    window !== undefined && window.from !== null && window.to !== null && window.from >= window.to
  );
}

const DATE = /^\d{4}-\d{2}-\d{2}$/;
const UTC_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$/;

/** The first moment of a UTC date (`2026-11-03`), or undefined for a date that is not one. */
function readDay(text: string): number | undefined {
  if (!DATE.test(text)) return undefined;
  const at = Date.parse(`${text}T00:00:00Z`);
  return !Number.isNaN(at) && new Date(at).toISOString().slice(0, 10) === text ? at : undefined;
}

/** One end of a `<from>..<to>` window: null for an end left out, the moment of a UTC date or of
 *  an RFC 3339 time written in UTC with `Z`, or undefined for anything else. */
function readEnd(text: string): number | null | undefined {
  if (text === "") return null;
  if (DATE.test(text)) return readDay(text);
  if (!UTC_TIME.test(text)) return undefined;
  const at = Date.parse(text);
  return Number.isNaN(at) ? undefined : at;
}

/** The value of `f.closed` that names no window. */
export const ALL_CLOSED = "all";

/** The first moment the list route reads in `closed_since` or `closed_before`: an RFC 3339 time
 *  writes its year in four digits, so no time before 0000-01-01T00:00:00Z. No close comes before
 *  it. */
const FIRST_READABLE_MS = Date.parse("0000-01-01T00:00:00Z");

/**
 * The Closed window `f.closed` names, or undefined for none. `<N>d` is the last N times 24 hours
 * up to `nowMs`, rolling. `<from>..<to>` is a range of UTC dates (`2026-11-03`) or RFC 3339 times
 * written in UTC with `Z`, `from` included and `to` left out, as the list route takes
 * `closed_since` and `closed_before`; either end may be left out. A range whose `to` is at or
 * before its `from` holds no moment (`holdsNoMoment`): it lists no row, and no read asks the
 * route for it. `all`, and a value this cannot read, are no window: an older link still opens the
 * view.
 */
export function readWindow(
  value: string | null | undefined,
  nowMs: number
): ClosedWindow | undefined {
  if (value === null || value === undefined || value === ALL_CLOSED) return undefined;
  const days = /^(\d+)d$/.exec(value);
  if (days !== null) {
    const count = Number(days[1]);
    const from = nowMs - count * DAY_MS;
    // A count whose first moment comes before the first moment the list route reads holds every
    // close, as no window does: so it is none, and no read sends the route a time it refuses.
    return count > 0 && from >= FIRST_READABLE_MS ? { from, to: null, rolling: true } : undefined;
  }
  const split = value.indexOf("..");
  if (split === -1) return undefined;
  const from = readEnd(value.slice(0, split));
  const to = readEnd(value.slice(split + 2));
  if (from === undefined || to === undefined || (from === null && to === null)) return undefined;
  return { from, to, rolling: false };
}

/** Whether a row closed at `closedAt` is in `window`. With a window, a row that never closed, or
 *  whose close does not parse, is in none; with none, every row is. */
export function inWindow(
  closedAt: string | null | undefined,
  window: ClosedWindow | undefined
): boolean {
  if (window === undefined) return true;
  const at = closedAt === null || closedAt === undefined ? Number.NaN : Date.parse(closedAt);
  if (Number.isNaN(at)) return false;
  return (window.from === null || at >= window.from) && (window.to === null || at < window.to);
}

/** The `to` end that includes the UTC day `day`, which a date picker's To shows: the next day,
 *  which the window leaves out. "" stays "", an end left out. */
export function toEndOfDay(day: string): string {
  if (day === "") return "";
  const at = readDay(day);
  return at === undefined ? day : new Date(at + DAY_MS).toISOString().slice(0, 10);
}

/** The last UTC day a `to` end includes, which a date picker's To shows: the day before a date,
 *  or the day of the moment before a time. "" for an end left out or one this cannot read. */
export function lastDayOf(to: string): string {
  const at = readEnd(to);
  return at === null || at === undefined ? "" : new Date(at - 1).toISOString().slice(0, 10);
}

/** The `f.closed` value a date picker writes for the UTC days `from` to `to`, both included, or
 *  undefined where it names neither or its To comes before its From, a window that holds no
 *  moment: so 2026-11-03 to 2026-11-09 writes `2026-11-03..2026-11-10`, and one day is
 *  `<day>..<next day>`. */
export function rangeValue(from: string, to: string): string | undefined {
  if (from === "" && to === "") return undefined;
  const value = `${from}..${toEndOfDay(to)}`;
  // A range reads no clock, so any `nowMs` reads it the same.
  return holdsNoMoment(readWindow(value, 0)) ? undefined : value;
}

/** The UTC days a date picker shows for a `<from>..<to>` value, its To the last day the window
 *  includes (`lastDayOf`), or undefined for a value that is no range. */
export function rangeDays(
  value: string
): { readonly from: string; readonly to: string } | undefined {
  const split = value.indexOf("..");
  if (split === -1) return undefined;
  const from = readEnd(value.slice(0, split));
  const to = value.slice(split + 2);
  if (from === undefined || readEnd(to) === undefined) return undefined;
  return {
    from: from === null ? "" : new Date(from).toISOString().slice(0, 10),
    to: lastDayOf(to),
  };
}

/** What `GET /api/v1/issues` is asked for a Closed window: `closed_since` at its first moment and
 *  `closed_before` at its end, each where the window has it, and nothing for no window. */
export function closedRange(window: ClosedWindow | undefined) {
  const from = window?.from ?? null;
  const to = window?.to ?? null;
  return {
    ...(from === null ? {} : { closed_since: new Date(from).toISOString() }),
    ...(to === null ? {} : { closed_before: new Date(to).toISOString() }),
  };
}

/** `window` with a rolling window's first moment moved back to the start of its hour, so a read
 *  of it and its key hold for an hour. The browser leaves out the rows it reads before the
 *  window's own first moment. A window of fixed ends, and no window, stay as they are. */
export function hourStart(window: ClosedWindow | undefined): ClosedWindow | undefined {
  if (window === undefined || window.from === null || !window.rolling) return window;
  return { ...window, from: Math.floor(window.from / HOUR_MS) * HOUR_MS };
}
