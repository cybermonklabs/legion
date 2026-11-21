import { queryOptions } from "@tanstack/react-query";

import { api } from "../../api/client";

// Two of the reads the outline draws the conversation from - the issue's comments and its
// messages - under the keys and by the requests `useIssueTree.ts` makes them with; the third, the
// issue's asks, is `useIssueTree.ts`'s own `issueAsksQuery`. The box (`IssueComposer.tsx`) and
// the read mark (`IssueReadMark.tsx`) observe them, and take each one from here rather than
// writing it out again: a key holds whatever the first request mounted under it asked for, so a
// copy whose request had drifted from the outline's would put another answer under the same key,
// and nothing would fail to say so. The outline (`outline-reads.ts`) still writes these two reads
// out itself. Once its own module exports them, those exports take the place of these.

/** The issue's comments (`GET /api/v1/issues/{key}/comments`). */
export const outlineCommentsQuery = (key: string) =>
  queryOptions({
    queryKey: ["comments", key],
    queryFn: () => api.listComments(key),
  });

/** The issue's messages, each with its replies (`GET /api/v1/issues/{key}/messages`). */
export const outlineMessagesQuery = (key: string) =>
  queryOptions({
    queryKey: ["issue-messages", key],
    queryFn: () => api.listIssueMessages(key),
  });
