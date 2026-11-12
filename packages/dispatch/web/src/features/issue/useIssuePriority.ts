import { issueQuery, projectListsKey } from "../../api/queries";
import type { InboxRow, IssueDetails, IssuePriority, IssueSummary } from "../../api/types";
import type { Shown } from "../writes/write-store";
import { inboxKey, useIssueFieldWrite } from "./useIssueFieldWrite";

export interface IssuePriorityWrite {
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  pending: boolean;
  /** Repeats the refused save; undefined where a newer pick of this issue's priority stands in
   *  the store's line, since a Retry would store the older pick last. */
  retry: (() => void) | undefined;
  /** Saves `priority`: one write of the page's store, on the issue's line, as it is pressed. */
  submit: (priority: IssuePriority | null) => void;
}

/**
 * The one way the SPA writes an issue's priority. Every surface that renders it (issue header,
 * board cards, list rows, inbox rows) shares this: the new value lands in the issue detail,
 * every project issue list and every inbox query that shows the issue, the store shows it there
 * again over each answer that lands while the write is live (R7), it rolls back on failure, and
 * the lists refetch once the server has answered (see `useIssueFieldWrite`).
 */
export function useIssuePriority(issueKey: string): IssuePriorityWrite {
  const detailKey = issueQuery(issueKey).queryKey;
  const showPriority = (priority: IssuePriority | null, shown: Shown) => {
    shown.on<IssueDetails>(detailKey, (current) => ({ ...current, priority }));
    shown.under<IssueSummary[]>(projectListsKey, (current) =>
      current.map((issue) => (issue.key === issueKey ? { ...issue, priority } : issue))
    );
    shown.under<InboxRow[]>(inboxKey, (current) =>
      current.map((row) => (row.issue_key === issueKey ? { ...row, priority } : row))
    );
  };
  const write = useIssueFieldWrite<IssuePriority | null>(issueKey, {
    label: (priority) => `${issueKey} set to ${priority === null ? "no priority" : `P${priority}`}`,
    name: "priority",
    patch: (priority) => ({ priority }),
    show: showPriority,
  });
  return { failed: write.failed, pending: write.pending, retry: write.retry, submit: write.submit };
}
