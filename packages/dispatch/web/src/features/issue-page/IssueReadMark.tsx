import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api, issueStateRequest } from "../../api/client";
import { issueAskThreadsQuery, userStateQuery } from "../../api/queries";
import type { UserState } from "../../api/types";
import { stateForIssue } from "../issue/pins";
import { useDwell } from "../shell/useDwell";
import { useSendMarkOnce } from "../writes/useSendMarkOnce";
import { outlineQuery } from "./tree-read/tree-read";
import { issueMessagesQuery } from "./useIssueTree";

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
 * answer that lands after the reader moved on is put where it belongs. It is drawn with no view
 * on and no document open alone (`slots/foot.tsx`): a view is a part of the conversation, and
 * its end is not the conversation's.
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
  // The reads the outline draws the conversation from, under the keys and by the requests it
  // makes them with (`useIssueTree.ts`), as `outline-reads.ts` holds them. Until each holds an
  // answer the outline is short of what it will draw - its empty line, or the documents' rows
  // alone - and the end under it stands on screen with none of the conversation above it, so the
  // dwell waits on them, as main's Conversation tab draws no end of the conversation before its
  // reads answer. One whose first read failed holds none, and the issue is not marked read over
  // rows never drawn. An answer held is enough: a refetch that fails keeps the rows the outline
  // drew. The outline makes these reads and keeps them fresh, so the mark's mount asks for none
  // of them again (`refetchOnMount: false`), and one the outline is already making is that same
  // request, under the same key, not a second one. The comments are the tree read's own nodes
  // (`decision-review.ts`' `treeComments`): its answer as the page holds it - Conversation's, or
  // Graph's in Graph - watched and never sent, since the outline sends it.
  const asks = useQuery({ ...issueAskThreadsQuery(issueKey), refetchOnMount: false });
  const conversation = useQuery({ ...outlineQuery(issueKey, "conversation"), enabled: false });
  const graph = useQuery({ ...outlineQuery(issueKey, "graph"), enabled: false });
  const messages = useQuery({ ...issueMessagesQuery(issueKey), refetchOnMount: false });
  const outlineHeld =
    asks.data !== undefined &&
    (conversation.data ?? graph.data) !== undefined &&
    messages.data !== undefined;
  const [readThrough, setReadThrough] = useState<{
    readonly issueKey: string;
    readonly seq: number;
  }>();
  const read = readThrough?.issueKey === issueKey ? readThrough : undefined;
  const register = useDwell({
    enabled: headSeq > 0 && outlineHeld,
    onDwell: () => setReadThrough({ issueKey, seq: headSeq }),
    resetKey: `${issueKey} ${headSeq}`,
    shouldDwell: () => headSeq > Math.max(lastReadSeq, read?.seq ?? 0),
  });
  useSendMarkOnce({
    mark: read === undefined ? undefined : { key: `${read.issueKey} ${read.seq}`, value: read },
    onSuccess: (next, mark) =>
      queryClient.setQueryData<UserState>(userStateQuery().queryKey, (current) => ({
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
