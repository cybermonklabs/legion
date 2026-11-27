import { queryOptions } from "@tanstack/react-query";

import { api } from "../../api/client";

/** The history's own filtered log of an issue, under its own key so it never shares Activity's
 *  unfiltered cache. */
export function historyLogKey(issueKey: string): readonly ["events", string, "history"] {
  return ["events", issueKey, "history"];
}

/** The first event of the whole log past the reader's read mark, shared by the Conversation tab
 *  and Details' history: its time tells whether a thread whose created event the filtered log
 *  leaves out is new. */
export function issueUnreadBoundaryQuery(issueKey: string, lastReadSeq: number) {
  return queryOptions({
    queryKey: ["events", issueKey, "unread-boundary", lastReadSeq] as const,
    queryFn: () => api.getIssueEvents(issueKey, { after: lastReadSeq, limit: 1 }),
  });
}
