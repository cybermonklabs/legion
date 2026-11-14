import { inboxThreadRow } from "@legion/contracts";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { useCache } from "../../api/cache/cache";
import { storeAgentState } from "../../api/cache/viewer-state";
import { agentStateRequest, threadReadRequest } from "../../api/client";
import { myRepliesKey } from "../../api/queries";
import type { MyReplyRow } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  card,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { ThreadOwnerLine } from "./AskOwnerLink";
import { DismissControl } from "./DismissControl";
import { rowPriority, rowType } from "./list-row";
import { RowKindIcon, RowPriorityTag } from "./RowMarks";
import { ThreadBody } from "./ThreadBody";
import { ThreadTitle } from "./ThreadTitle";

/**
 * A Reply or an Agent row as a card: the layout below the reading pane's width, and the one the
 * drawer shows at every width.
 *
 * It draws the head - the kind icon, the title and the time of the last write, then the priority
 * tag and the whole issue key and title - then the thread itself, exactly as the reading pane
 * draws it and collapsed the same way (`ThreadBody`), and the three controls a thread row offers:
 * **Open**, **Mark read**, and the **Dismiss** that takes the row out of the Inbox - **Move to
 * Inbox** in the Dismissed view - which the list it is drawn in provides (`DismissControl`). A
 * thread anchored in a document shows the text it is anchored to rather than the document: a card
 * is no place to open one.
 */
export function ThreadRowCard({ thread }: { thread: MyReplyRow }): ReactNode {
  const cache = useCache();
  const read = inboxThreadRow(thread);
  const session = read.kind === "agent" ? read.session.id : undefined;
  const markRead = useKeyedWrite({
    label: () => "Thread marked read",
    // An Agent row is read by id, as every read of one is: its session's own read mark covers
    // replies in the live conversation and in its other threads, which this press has not
    // shown. Every other row carries a thread mark, which this moves to its newest write.
    request: () =>
      read.kind === "agent"
        ? agentStateRequest(read.session.id, { read_replies: read.unread_reply_ids })
        : threadReadRequest(thread.thread_id, thread.reply.created_at),
    // The row's own read line, the one the dwell marks it on (`threads/useMarkThreadRead.ts`,
    // `features/agents/unread.ts`), so the press and the dwell of one row order against each
    // other rather than racing.
    target: () =>
      read.kind === "agent" ? `read:agent:${read.session.id}` : `read:thread:${thread.thread_id}`,
    onSuccess: (state) => {
      if (state !== null && state !== undefined && session !== undefined) {
        storeAgentState(cache, session, state);
      }
      return cache.invalidate({ queryKey: myRepliesKey });
    },
  });
  const row = { kind: "thread", thread } as const;
  return (
    <div className={`rounded-xl border p-3 ${card} ${borderDefault}`} data-inbox-thread-card="">
      <div className="flex min-w-0 items-baseline gap-2">
        <RowKindIcon type={rowType(row)} />
        <ThreadTitle className="min-w-0 grow truncate text-sm" thread={thread} />
        <span className={`shrink-0 text-xs ${textMutedOnCanvas}`}>
          <Timestamp at={thread.reply.created_at} />
        </span>
      </div>
      <div className="mt-1 flex min-w-0 flex-wrap items-baseline gap-2">
        <RowPriorityTag priority={rowPriority(row)} />
        <ThreadOwnerLine className={`truncate text-sm ${textMutedOnCanvas}`} thread={thread} />
      </div>
      <ThreadBody quote thread={thread} />
      <div className="mt-2 flex flex-wrap items-center gap-4 text-sm">
        {/* The server's `ref` names the thread's root, never the reply: an anchored root
            redirects to its document, and a comment's thread opens as it lands. */}
        <Link
          className={`font-medium ${linkText} ${linkHoverText}`}
          data-inbox-owner=""
          to={thread.ref}
        >
          Open
        </Link>
        <button
          className={`min-h-11 font-medium ${linkText} ${linkHoverText}`}
          disabled={markRead.isPending}
          onClick={() => markRead.mutate()}
          type="button"
        >
          {markRead.isPending ? "Marking read…" : "Mark read"}
        </button>
        <DismissControl shown thread={thread} />
      </div>
      {markRead.isError ? (
        <div className="mt-2">
          <QueryError message={markRead.error.message} onRetry={() => markRead.mutate()} />
        </div>
      ) : null}
    </div>
  );
}
