import { useQueryClient } from "@tanstack/react-query";

import { projectDefaultAssigneesQuery } from "../../api/queries";
import type { InboxRow, IssueDetails, IssueSummary } from "../../api/types";
import { askProject } from "../inbox/list-row";
import { inboxKey, projectListsKey, useIssueFieldWrite } from "./useIssueFieldWrite";

export interface IssueAssigneeWrite {
  /** The server's reason after a refused save (an unlisted login), until the next attempt. */
  error: string | undefined;
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  pending: boolean;
  /** Repeats the failed save; a no-op while one is in flight. */
  retry: () => void;
  /** Saves `assignee` (a login, or null to unassign). A pick made while a save is in flight
   *  shows at once and is saved when that save settles; the newest such pick is the one saved. */
  submit: (assignee: string | null) => void;
}

/**
 * The one way the SPA writes an issue's assignee. The issue header's picker and the inbox's
 * "Assign to me" share this: the new login lands optimistically in the issue detail, every
 * project issue list and every inbox row that belongs to the issue (so a row the Inbox's
 * assignee filter judges by that login moves in or out of the list at once), rolls back on
 * failure, and the lists refetch once the server has answered (see `useIssueFieldWrite`).
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
export function useIssueAssignee(issueKey: string): IssueAssigneeWrite {
  const queryClient = useQueryClient();
  const detailKey = ["issue", issueKey] as const;
  const showAssignee = (assignee: string | null) => {
    queryClient.setQueryData<IssueDetails>(detailKey, (current) =>
      current === undefined ? undefined : { ...current, assignee }
    );
    queryClient.setQueriesData<IssueSummary[]>({ queryKey: projectListsKey }, (current) =>
      current?.map((issue) => (issue.key === issueKey ? { ...issue, assignee } : issue))
    );
    const defaults =
      assignee === null
        ? queryClient.getQueryData(projectDefaultAssigneesQuery().queryKey)
        : undefined;
    // A row the update moves: one of this issue that says who holds it.
    const moves = (row: InboxRow): row is InboxRow & Required<Pick<InboxRow, "issue">> =>
      row.issue_key === issueKey && row.issue !== undefined;
    queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) => {
      // Every row the update matches is of the one issue, so of the one project: its default
      // is looked up once, off the first of them.
      const first = current?.find(moves);
      if (current === undefined || first === undefined) return current;
      const project = askProject(first);
      const person = assignee ?? defaults?.find((entry) => entry.project === project)?.assignee;
      return current.map((row) => {
        if (!moves(row)) return row;
        const moved = { ...row, issue: { ...row.issue, assignee } };
        if (row.asked_of !== null && row.asked_of !== undefined) return moved;
        return person === undefined ? moved : { ...moved, waiting_on_person: person };
      });
    });
  };
  return useIssueFieldWrite<string | null>(issueKey, {
    label: (assignee) => `${issueKey} assigned to ${assignee ?? "nobody"}`,
    patch: (assignee) => ({ assignee }),
    show: showAssignee,
  });
}
