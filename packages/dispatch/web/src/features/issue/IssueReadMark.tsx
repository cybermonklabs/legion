import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api, issueStateRequest } from "../../api/client";
import { userStateQuery } from "../../api/queries";
import type { UserState } from "../../api/types";
import { useDwell } from "../shell/useDwell";
import { useSendMarkOnce } from "../writes/useSendMarkOnce";
import { stateForIssue } from "./pins";

/**
 * The issue's read mark, and the only writer of it (`user_issue_state.last_read_seq`, which the
 * Inbox, the board and the issues table read): the end of the conversation on screen for a second
 * reads the issue through its head. It is the rule the panel's old list of the conversation kept
 * - the newest conversation three-quarters on screen for a second - moved to where the newest of
 * the conversation now is, the end of the outline, which the outline draws its turns last at
 * (`tree-drafts.ts`'s `ROOT_ORDER`). A single watermark cannot say "I read the third thread
 * only", so nothing above the end writes it, and under-marking never hides an unread one.
 *
 * The mark is a quiet write of the one write store (`useSendMarkOnce`): made as the person
 * reading, tried under its key until Dispatch decides it, kept across a reload and never shown;
 * one Dispatch refuses is sent again once the stored mark changes. It names its issue, so an
 * answer that lands after the reader moved on is put where it belongs. It is drawn with no
 * filter on alone: a filter is a part of the conversation, and its end is not the conversation's.
 */
export function IssueReadMark({ issueKey }: { issueKey: string }): ReactNode {
  const queryClient = useQueryClient();
  const state = useQuery(userStateQuery());
  const lastReadSeq = stateForIssue(state.data, issueKey).last_read_seq;
  // The issue's head: what a dwell on the end marks read.
  const head = useQuery({
    queryFn: () => api.getIssueEvents(issueKey, { limit: 1, order: "desc" }),
    queryKey: ["events", issueKey, "head"],
  });
  const headSeq = head.data?.[0]?.seq ?? 0;
  const [readThrough, setReadThrough] = useState<{
    readonly issueKey: string;
    readonly seq: number;
  }>();
  const read = readThrough?.issueKey === issueKey ? readThrough : undefined;
  const register = useDwell({
    enabled: headSeq > 0,
    onDwell: () => setReadThrough({ issueKey, seq: headSeq }),
    resetKey: `${issueKey} ${headSeq}`,
    shouldDwell: () => headSeq > Math.max(lastReadSeq, read?.seq ?? 0),
  });
  useSendMarkOnce({
    mark: read === undefined ? undefined : { key: `${read.issueKey} ${read.seq}`, value: read },
    onSuccess: (next, mark) =>
      queryClient.setQueryData<UserState>(["user-state"], (current) => ({
        ...current,
        [mark.issueKey]: next,
      })),
    request: (mark: { issueKey: string; seq: number }) =>
      issueStateRequest(mark.issueKey, { last_read_seq: mark.seq }),
    shouldSend: read !== undefined && read.seq > lastReadSeq,
    target: `read:issue:${issueKey}`,
    wake: lastReadSeq,
  });
  return <div aria-hidden="true" className="h-px" data-outline-end="" ref={register} />;
}
