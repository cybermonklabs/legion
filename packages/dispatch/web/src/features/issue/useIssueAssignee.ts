import { type IssueFieldWrite, useIssueFieldWrite } from "./useIssueFieldWrite";

/** The assignee's write: `error` is the server's reason after a refused pick (an unlisted
 *  login), and `submit` takes a login, or null to unassign. */
export type IssueAssigneeWrite = IssueFieldWrite<string | null>;

/**
 * The one way the SPA writes an issue's assignee. The issue header's picker and the inbox's
 * "Assign to me" share this: the pick's write shows the new login on the issue detail, every
 * list of issues and every inbox row that belongs to the issue (so a row the Inbox's assignee
 * filter judges by that login moves in or out of the list at once), keeps it there over every
 * answer until it lands, and puts back the assignee Dispatch last confirmed if it is refused (see
 * `useIssueFieldWrite`). An inbox row nobody handed on waits on the new assignee, or on an
 * unassign on its project's default assignee where the client holds the defaults read
 * (`showOnInboxRow` in `issue-field-looks.ts`). `drawn` is the assignee as the caller reads it
 * from the cache, and `shown` what to draw in its place.
 */
export function useIssueAssignee(issueKey: string, drawn: string | null): IssueAssigneeWrite {
  return useIssueFieldWrite(issueKey, "assignee", drawn);
}
