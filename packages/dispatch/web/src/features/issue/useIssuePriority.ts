import { useQueryClient } from "@tanstack/react-query";

import { projectListsKey } from "../../api/queries";
import type { InboxRow, IssueDetails, IssuePriority, IssueSummary } from "../../api/types";
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
 * board cards, list rows, inbox rows) shares this: the new value lands optimistically in the
 * issue detail, every project issue list and every inbox query that shows the issue, rolls back
 * on failure, and the lists refetch once the server has answered (see `useIssueFieldWrite`).
 */
export function useIssuePriority(issueKey: string): IssuePriorityWrite {
  const queryClient = useQueryClient();
  const detailKey = ["issue", issueKey] as const;
  const showPriority = (priority: IssuePriority | null) => {
    queryClient.setQueryData<IssueDetails>(detailKey, (current) =>
      current === undefined ? undefined : { ...current, priority }
    );
    queryClient.setQueriesData<IssueSummary[]>({ queryKey: projectListsKey }, (current) =>
      current?.map((issue) => (issue.key === issueKey ? { ...issue, priority } : issue))
    );
    queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
      current?.map((row) => (row.issue_key === issueKey ? { ...row, priority } : row))
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
