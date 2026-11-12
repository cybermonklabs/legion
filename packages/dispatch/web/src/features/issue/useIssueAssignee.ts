import { issueQuery, projectListsKey } from "../../api/queries";
import type { InboxRow, IssueDetails, IssueSummary } from "../../api/types";
import type { Shown } from "../writes/write-store";
import { type IssueFieldWrite, inboxKey, useIssueFieldWrite } from "./useIssueFieldWrite";

/**
 * The one way the SPA writes an issue's assignee. The issue header's picker and the inbox's
 * "Assign to me" share this: the new login lands in the issue detail, every project issue list
 * and every inbox row that belongs to the issue (so a row the Inbox's assignee filter judges by
 * that login moves in or out of the list at once), the store shows it there again over each
 * answer that lands while the write is live (R7), it rolls back on failure, and the lists
 * refetch once the server has answered (see `useIssueFieldWrite`).
 *
 * An inbox row is judged by the person it waits on, which the server answers as
 * `coalesce(asked_of, assignee)`: so a row whose ask nobody has handed on follows the new
 * assignee here, and one somebody handed to a person keeps that person whoever holds its issue.
 */
export function useIssueAssignee(issueKey: string): IssueFieldWrite<string | null> {
  const detailKey = issueQuery(issueKey).queryKey;
  const showAssignee = (assignee: string | null, shown: Shown) => {
    shown.on<IssueDetails>(detailKey, (current) => ({ ...current, assignee }));
    shown.under<IssueSummary[]>(projectListsKey, (current) =>
      current.map((issue) => (issue.key === issueKey ? { ...issue, assignee } : issue))
    );
    shown.under<InboxRow[]>(inboxKey, (current) =>
      current.map((row) =>
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
