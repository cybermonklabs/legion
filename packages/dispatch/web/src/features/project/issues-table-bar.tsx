import type { ReactNode } from "react";
import {
  focusVisibleRing,
  issueStatusScale,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  tableFilterActive,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { type IssueStatus, issueStatuses, statusLabel } from "./board-model";

/** One segment of the bar: a status with listed rows, and how many. */
export interface StatusSegment {
  readonly status: IssueStatus;
  readonly count: number;
}

/** How many of `statuses`, the statuses of the rows the table lists, each status holds. */
export function countStatuses(statuses: Iterable<string>): ReadonlyMap<string, number> {
  const counts = new Map<string, number>();
  for (const status of statuses) counts.set(status, (counts.get(status) ?? 0) + 1);
  return counts;
}

/** The bar's segments: each status with a listed row, in the status order (Triage first, Done
 *  last). A status with no listed row has no segment. */
export function statusSegments(counts: ReadonlyMap<string, number>): StatusSegment[] {
  return issueStatuses.flatMap((status) => {
    const count = counts.get(status) ?? 0;
    return count === 0 ? [] : [{ status, count }];
  });
}

/**
 * The bar under the view tabs on a view whose bar is by status (Open): the rows the table lists,
 * counted by status, from the left edge, each segment as wide as its share of those rows. Under
 * it the legend, one entry for each segment: a swatch, the status and its count. The legend
 * wraps onto as many lines as it needs and never scrolls. A press on an entry narrows the view
 * to that status; a press on the entry of the one status the view is narrowed to restores the
 * view's own statuses.
 */
export function IssueStatusBar({
  counts,
  narrowTo,
  picked,
}: {
  /** How many listed rows each status holds (`countStatuses`). */
  counts: ReadonlyMap<string, number>;
  /** Narrows the view to `status`, or restores the view's own statuses for `undefined`. */
  narrowTo: (status: IssueStatus | undefined) => void;
  /** The statuses the view's Status filter picks now. */
  picked: readonly string[];
}): ReactNode {
  const segments = statusSegments(counts);
  const listed = segments.reduce((sum, segment) => sum + segment.count, 0);
  const isPicked = (status: IssueStatus) => picked.length === 1 && picked[0] === status;
  return (
    <section aria-label="Issues by status" className="flex flex-col gap-1">
      <span aria-hidden className={`flex h-3 overflow-hidden rounded-sm ${surfaceMutedBg}`}>
        {segments.map(({ count, status }) => (
          <span
            className={`h-full shrink-0 ${issueStatusScale[status]}`}
            data-bar-segment={status}
            key={status}
            style={{ width: `${(count / listed) * 100}%` }}
          />
        ))}
      </span>
      <div className="flex flex-wrap gap-x-0.5">
        {segments.map(({ count, status }) => (
          <button
            aria-pressed={isPicked(status)}
            className={`inline-flex min-h-11 shrink-0 items-center gap-1 rounded-md px-1 text-xs whitespace-nowrap md:min-h-7 ${focusVisibleRing} ${
              isPicked(status)
                ? tableFilterActive
                : `${textSecondaryOnCanvas} ${surfaceMutedHoverBg}`
            }`}
            data-bar-legend={status}
            key={status}
            onClick={() => narrowTo(isPicked(status) ? undefined : status)}
            type="button"
          >
            <span aria-hidden className={`size-2.5 rounded-sm ${issueStatusScale[status]}`} />
            {statusLabel(status)} <span className="tabular-nums">{count}</span>
          </button>
        ))}
      </div>
    </section>
  );
}
