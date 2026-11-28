// The number formats the Evals tab's charts share, ported from the prototype: `formatMinutes`
// from web/src/lib/dora.ts:34-40, `formatDuration` and `formatRate` from
// web/src/lib/pipeline.ts:60-83, and `pct` from web/src/components/DoraPanel.tsx:13-15. Pure.

import { formatMinutes as formatLeadTime, wholeMinutes } from "./prList";

/** What a measure with nothing to measure reads as: never a zero. */
export const NO_VALUE = "n/a";

/** Minutes as a lead time is shown: the PR list's buckets ("45m", "3.5h", "2.1d"),
 *  with `extra` decimals past theirs where a toned figure takes them (`lib/target.ts`'s
 *  `judgeFigure`), and `NO_VALUE` when there is nothing to measure. */
export function formatMinutes(v: number | null, extra = 0): string {
  return v === null ? NO_VALUE : formatLeadTime(v, extra);
}

/** Minutes as a CI timing is shown: "4.6m" under ten minutes, "93m" under three hours,
 *  then hours ("3.2h"), with `extra` decimals past those where a toned figure takes them
 *  (`lib/target.ts`'s `judgeFigure`); `NO_VALUE` when there is nothing to measure. */
export function formatDuration(minutes: number | null, extra = 0): string {
  if (minutes === null) return NO_VALUE;
  if (minutes < 10) return `${minutes.toFixed(1 + extra)}m`;
  if (minutes < 180) return `${wholeMinutes(minutes, extra)}m`;
  return `${(minutes / 60).toFixed(1 + extra)}h`;
}

/** A success rate as a whole percentage; `NO_VALUE` when nothing ended in success or failure. */
export function formatRate(rate: number | null): string {
  return rate === null ? NO_VALUE : `${Math.round(rate * 100)}%`;
}

/** A share as a percentage to `digits` decimals: "4.96%". */
export function pct(v: number, digits = 1): string {
  return `${(v * 100).toFixed(digits)}%`;
}
