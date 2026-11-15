import type { IssuePriority } from "../../api/types";
import { useIssueFieldWrite } from "./useIssueFieldWrite";

export interface IssuePriorityWrite {
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  pending: boolean;
  /** Repeats the refused save; a no-op while nothing is refused. */
  retry: () => void;
  /** Saves `priority`: a pick of the page's write store, joined to the issue's line as it is
   *  made, which takes out a pick of the priority still waiting there unsent; the newest is the
   *  one saved. */
  submit: (priority: IssuePriority | null) => void;
}

/**
 * The one way the SPA writes an issue's priority. Every surface that renders it (issue header,
 * board cards, list rows, the tables of the Priorities view, inbox rows) shares this, as do the
 * issue page's keys `0`-`3` and its palette: the pick's write shows the value on the issue detail,
 * every list of issues and every inbox query that shows the issue, keeps it there over every
 * answer until it lands, and puts back the value Dispatch last confirmed if it is refused (see
 * `useIssueFieldWrite` and `issue-field-writes.ts`).
 */
export function useIssuePriority(issueKey: string): IssuePriorityWrite {
  const write = useIssueFieldWrite(issueKey, "priority");
  return { failed: write.failed, pending: write.pending, retry: write.retry, submit: write.submit };
}
