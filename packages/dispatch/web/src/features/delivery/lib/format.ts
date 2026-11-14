// The number formats the measures panel and the Pipeline page share, ported from the prototype:
// `formatMinutes` from web/src/lib/dora.ts:34-40, `formatDuration`, `formatRate` and `formatDue`
// from web/src/lib/pipeline.ts:60-83, and `pct` from web/src/components/DoraPanel.tsx:13-15. Pure.

import { formatMinutes as formatLeadTime } from "./prList";

/** What a measure the measures panel and the Pipeline page have nothing to measure of reads as:
 *  never a zero. */
export const NO_VALUE = "n/a";

/** Minutes as the measures panel shows durations: the PR list's buckets ("45m", "3.5h", "2.1d"),
 *  and `NO_VALUE` when there is nothing to measure. */
export function formatMinutes(v: number | null): string {
  return v === null ? NO_VALUE : formatLeadTime(v);
}

/** Minutes as the Pipeline page shows them: "4.6m" under ten minutes, "93m" under three hours,
 *  then hours ("3.2h"); `NO_VALUE` when there is nothing to measure. */
export function formatDuration(minutes: number | null): string {
  if (minutes === null) return NO_VALUE;
  if (minutes < 10) return `${minutes.toFixed(1)}m`;
  if (minutes < 180) return `${Math.round(minutes)}m`;
  return `${(minutes / 60).toFixed(1)}h`;
}

/** A success rate as a whole percentage; `NO_VALUE` when nothing ended in success or failure. */
export function formatRate(rate: number | null): string {
  return rate === null ? NO_VALUE : `${Math.round(rate * 100)}%`;
}

/** A share as a percentage to `digits` decimals: "4.96%". */
export function pct(v: number, digits = 1): string {
  return `${(v * 100).toFixed(digits)}%`;
}

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

/** A `YYYY-MM-DD` day as the Pipeline page names it, "Oct 1"; anything else is an error. */
export function formatDue(day: string): string {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day);
  const month = match === null ? undefined : MONTHS[Number(match[2]) - 1];
  if (match === null || month === undefined) throw new Error(`day ${day} is not YYYY-MM-DD`);
  return `${month} ${Number(match[3])}`;
}
