import { useQueryClient } from "@tanstack/react-query";

import { issueQuery, projectListsKey } from "../../api/queries";
import type { InboxRow, IssueDetails, IssueSummary } from "../../api/types";
import { type IssueFieldWrite, inboxKey, useIssueFieldWrite } from "./useIssueFieldWrite";

/**
 * The one way the SPA writes an issue's assignee. The issue header's picker and the inbox's
 * "Assign to me" share this: the new login lands optimistically in the issue detail, every
 * project issue list and every inbox row that belongs to the issue (so a row the Inbox's
 * assignee filter judges by that login moves in or out of the list at once), rolls back on
 * failure, and the lists refetch once the server has answered (see `useIssueFieldWrite`).
 *
 * An inbox row is judged by the person it waits on, which the server answers as
 * `coalesce(asked_of, assignee)`: so a row whose ask nobody has handed on follows the new
 * assignee here, and one somebody handed to a person keeps that person whoever holds its issue.
 */
export function useIssueAssignee(issueKey: string): IssueFieldWrite<string | null> {
  const queryClient = useQueryClient();
  const detailKey = issueQuery(issueKey).queryKey;
  const showAssignee = (assignee: string | null) => {
    queryClient.setQueryData<IssueDetails>(detailKey, (current) =>
      current === undefined ? undefined : { ...current, assignee }
    );
    queryClient.setQueriesData<IssueSummary[]>({ queryKey: projectListsKey }, (current) =>
      current?.map((issue) => (issue.key === issueKey ? { ...issue, assignee } : issue))
    );
    queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
      current?.map((row) =>
        row.issue_key === issueKey && row.issue !== undefined
          ? {
              ...row,
              issue: { ...row.issue, assignee },
              ...(row.asked_of === null || row.asked_of === undefined
                ? { waiting_on_person: assignee ?? undefined }
                : {}),
            }
          : row
      )
    );
  };
  return useIssueFieldWrite<string | null>(issueKey, {
    label: (assignee) => `${issueKey} assigned to ${assignee ?? "nobody"}`,
    name: "assignee",
    patch: (assignee) => ({ assignee }),
    show: showAssignee,
  });
}
