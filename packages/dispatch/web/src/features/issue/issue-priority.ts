import type { IssuePriority } from "../../api/types";

/** What every issue row Dispatch answers carries of a priority - an issue's read, a row of a list
 *  of issues, an Inbox row, a reply row, an `issue.*` event's issue - read as one object, so that
 *  every surface that draws a priority hands `issuePriority` all three and none draws the issue's
 *  own value where it works at an inherited one. `resolved_priority` and `priority_from` are
 *  optional because a Dispatch of the release before the one that added them answers neither. */
interface PriorityFields {
  /** The issue's own priority, null where it sets none; a row that carries none reads as none. */
  readonly priority?: number | null;
  /** The priority the issue works at: its own, else that of the nearest issue above it that has
   *  one, else null. */
  readonly resolved_priority?: number | null;
  /** The key of the issue `resolved_priority` comes from; null where it is the issue's own or
   *  there is none. */
  readonly priority_from?: string | null;
}

/** An issue's priority as a surface draws it. Every surface draws `level`, and names `from`
 *  beside it where it is set; a pick writes `own`. */
export interface PriorityView {
  /** The issue's own priority, the one a pick sets and `PATCH {priority}` writes. */
  readonly own: IssuePriority | null;
  /** The priority the issue works at, which every surface shows, sorts, filters, groups and
   *  colours by. */
  readonly level: IssuePriority | null;
  /** The key of the issue `level` comes from where the issue inherits it, else null: the priority
   *  is inherited exactly while this is set. */
  readonly from: string | null;
}

/** `value` as a priority. The route table carries it as a plain number; Dispatch writes 0 to 3
 *  and nothing else, and a number outside that reads as no priority rather than as a lane and a
 *  label no surface has. */
function priorityLevel(value: number | null | undefined): IssuePriority | null {
  return value === 0 || value === 1 || value === 2 || value === 3 ? value : null;
}

/**
 * `issue`'s priority as a surface draws it: the one view every surface that shows, sorts,
 * filters, groups or colours by a priority reads, so none reads `resolved_priority` or
 * `priority_from` of an issue row itself; Delivery's rows come resolved from the server. An own
 * priority is the one the issue works at, from nowhere else, whatever else the row says:
 * Dispatch resolves an issue's own first, and a pick writes the own priority alone (`pickLook`),
 * so a row that holds a new own priority beside the inherited one Dispatch last answered shows
 * the new one. An issue with none of its own works at `resolved_priority`, from
 * `priority_from`; from a Dispatch that answers no `resolved_priority`, at none.
 *
 * It stands here rather than beside the write hook (`useIssuePriority.ts`) because every surface
 * that only draws a priority reads it - the Inbox's row rule among them - and that hook's module
 * reaches the query client, which reaches those surfaces back.
 */
export function issuePriority(issue: PriorityFields): PriorityView {
  const own = priorityLevel(issue.priority);
  if (own !== null) return { from: null, level: own, own };
  const level = priorityLevel(issue.resolved_priority);
  return { from: level === null ? null : (issue.priority_from ?? null), level, own };
}

/**
 * The words of an inherited priority where a surface names it in text rather than draws its
 * source beside it, as an accessible name or description: "P0, from ACME-1". `level` and `from`
 * are a `PriorityView`'s, and `fromWord` is the word that joins them, which the new issue page
 * passes in its own words (`CONTROL_LABELS.priority_from`).
 */
export function inheritedPriorityWords(
  level: IssuePriority,
  from: string,
  fromWord = "from"
): string {
  return `P${level}, ${fromWord} ${from}`;
}
