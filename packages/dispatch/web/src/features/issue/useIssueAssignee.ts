import { useQueryClient } from "@tanstack/react-query";

import { issueQuery, projectDefaultAssigneesQuery, projectListsKey } from "../../api/queries";
import type { InboxRow, IssueDetails, IssueSummary } from "../../api/types";
import { issueProject } from "../inbox/list-row";
import type { Shown } from "../writes/write-store";
import { type IssueFieldWrite, inboxKey, useIssueFieldWrite } from "./useIssueFieldWrite";

/**
 * The one way the SPA writes an issue's assignee. The issue header's picker and the inbox's
 * "Assign to me" share this: the new login lands in the issue detail, every project issue list
 * and every inbox row that belongs to the issue (so a row the Inbox's assignee filter judges by
 * that login moves in or out of the list at once), the store composes it there over each answer
 * that lands while the write is live (R7) and stops composing it if it fails, and the lists
 * refetch once the server has answered (see `useIssueFieldWrite`).
 *
 * An inbox row is judged by the person it waits on, which the server answers as
 * `coalesce(a.asked_of, i.assignee, p.default_assignee)`: so a row whose ask nobody has handed on
 * follows the new assignee here, and one somebody handed to a person keeps that person whoever
 * holds its issue. An unassign moves a row nobody handed on to its project's default assignee,
 * taken from the defaults read the Settings page holds (`projectDefaultAssigneesQuery`) where the
 * client has it; where it does not, the row keeps the person it waited on until the refetch names
 * the default. It never names nobody: every project names a default, so no row the server
 * answers does, and one written so would read as an ask no person waits on.
 */
export function useIssueAssignee(issueKey: string): IssueFieldWrite<string | null> {
  const queryClient = useQueryClient();
  const detailKey = issueQuery(issueKey).queryKey;
  const showAssignee = (assignee: string | null, shown: Shown) => {
    shown.on<IssueDetails>(detailKey, (current) => ({ ...current, assignee }));
    shown.under<IssueSummary[]>(projectListsKey, (current) =>
      current.map((issue) => (issue.key === issueKey ? { ...issue, assignee } : issue))
    );
    // Every row the update moves is of this issue, so of its project: the person a row nobody
    // handed on waits on now is the new assignee, else that project's default where the client
    // holds the defaults read, looked up once each time the change is shown.
    const project = issueProject(issueKey);
    const person =
      assignee ??
      queryClient
        .getQueryData(projectDefaultAssigneesQuery().queryKey)
        ?.find((entry) => entry.project === project)?.assignee;
    shown.under<InboxRow[]>(inboxKey, (current) =>
      current.map((row) => {
        if (row.issue_key !== issueKey || row.issue === undefined) return row;
        const moved = { ...row, issue: { ...row.issue, assignee } };
        if (row.asked_of !== null && row.asked_of !== undefined) return moved;
        return person === undefined ? moved : { ...moved, waiting_on_person: person };
      })
    );
  };
  return useIssueFieldWrite<string | null>(issueKey, {
    label: (assignee) => `${issueKey} assigned to ${assignee ?? "nobody"}`,
    name: "assignee",
    patch: (assignee) => ({ assignee }),
    show: showAssignee,
  });
}
