import type { IssuePriority } from "../../api/types";
import { type IssueFieldWrite, useIssueFieldWrite } from "./useIssueFieldWrite";

/** The priority's write, without `error`: a refused one shows a fixed line (`PriorityControl`),
 *  not the server's reason. `submit` takes a priority, or null to unset it. */
export type IssuePriorityWrite = Omit<IssueFieldWrite<IssuePriority | null>, "error">;

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
