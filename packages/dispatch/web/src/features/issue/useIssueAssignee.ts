import { type IssueFieldWrite, useIssueFieldWrite } from "./useIssueFieldWrite";

/** The assignee's write: `error` is the server's reason after a refused pick (an unlisted
 *  login), or what a pick the reader stopped may have done (`stopped`), and `submit` takes a
 *  login, or null to unassign. */
export type IssueAssigneeWrite = IssueFieldWrite<string | null>;

/**
 * The one way the SPA writes an issue's assignee. The issue header's picker and the inbox's
 * "Assign to me" share this: the pick's write shows the new login on the issue detail, every
 * list of issues and every inbox row that belongs to the issue (so a row the Inbox's assignee
 * filter judges by that login moves in or out of the list at once), keeps it there over every
 * answer until it lands, and puts back the assignee Dispatch last confirmed if it is refused (see
 * `useIssueFieldWrite`). An inbox row nobody handed on waits on the new assignee, or on an
 * unassign on its project's default assignee where the client holds the defaults read
 * (`showOnInboxRow` in `issue-field-looks.ts`). `host` is the assignee as the caller reads it
 * from the issue's caches; draw `shown`.
 *
 * The one read, which the reader's own asks are drawn from, takes the same move, and a row the
 * write makes wait on the reader joins its Inbox answer where it held no entry (`shownOn` in
 * `issue-field-looks.ts`): without it, taking an unassigned ask would take the row out of the
 * open asks' list it was drawn from and put it in no other.
 */
export function useIssueAssignee(issueKey: string, host: string | null): IssueAssigneeWrite {
  return useIssueFieldWrite(issueKey, "assignee", host);
}
