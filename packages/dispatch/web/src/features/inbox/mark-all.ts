/**
 * The Inbox's **Mark all read**: what one press sends, what it changes on the page before the
 * server answers, and what comes back when the server refuses it.
 *
 * One press is one `POST /api/v1/me/inbox/read` (`markInboxReadRequest`), carrying the read mark
 * the list was read at (`as_of`) and the narrowing the list is under, so the server marks exactly
 * the threads the reader is looking at: no row that arrived after that read is swallowed, and no
 * row the Filter hid is marked. The press is applied here first - the rows are drawn as read and
 * the unread count every badge shares drops - because the reader pressed it, and is taken back
 * whole when the request fails, which an old server with no such route is one way of doing.
 *
 * It takes no row out of the list. A thread stays in the Inbox, read or not, until it is
 * dismissed (`useInboxDismiss.ts`), as an email stays until it is archived.
 */

import { useState } from "react";
import { useCache } from "../../api/cache/cache";

import { markInboxReadRequest } from "../../api/client";
import {
  inboxThreadsQuery,
  myRepliesCountQuery,
  myRepliesKey,
  userAgentStateQuery,
} from "../../api/queries";
import type { InboxReadRequest, MyRepliesResponse, MyReplyRow } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import type { InboxFilters } from "./InboxBar";
import { type InboxListRow, rowId } from "./list-row";
import {
  madeUnreadCount,
  pressUnreadDrop,
  refusedUnreadDrop,
  settledUnreadDrop,
  shownUnreadCount,
  type UnreadDrop,
} from "./unread-count";
import { inboxWriteScope, inWriteScope } from "./write-scope";

/** What the page says when the server refused the press and nothing was marked. */
export const MARK_ALL_REFUSED_TEXT = "Nothing was marked read.";

/**
 * The body one press sends: the read mark the list was read at, and the list's own narrowing
 * (`inboxWriteScope`).
 */
export function markAllReadBody(filters: InboxFilters, readThrough: string): InboxReadRequest {
  return { ...inboxWriteScope(filters), read_through: readThrough };
}

/**
 * Whether one press marks `thread`: the server's own rule (`inboxReadScope`), read here so the
 * page draws as read exactly the rows the request marks. A thread written in since the read mark
 * stays unread - the reader has not seen that write - and the list's narrowing decides the rest
 * (`inWriteScope`).
 */
export function marksThread(thread: MyReplyRow, body: InboxReadRequest): boolean {
  if (!thread.unread) return false;
  const through = body.read_through;
  if (through === undefined || through === null) return false;
  if (compareTimestamps(thread.reply.created_at, through) > 0) return false;
  return inWriteScope(thread, body);
}

/** A thread row as a read mark leaves it: read, with nothing unread in it. A Reply row's mark
 *  moves to `readThrough`; an Agent row has none - its replies are read by id - so only its
 *  unread ids are emptied. The press reads every row it marks through this. */
export function readRow(thread: MyReplyRow, readThrough: string): MyReplyRow {
  if (thread.kind === "agent") {
    return { ...thread, unread: false, unread_count: 0, unread_reply_ids: [] };
  }
  return { ...thread, read_through: readThrough, unread: false, unread_count: 0 };
}

/** The list as the press leaves it: every row it marked drawn as read, and not one of them taken
 *  out of the list. A read thread is still in the Inbox - only a dismiss takes it out - so the
 *  rows stay where the reader can see what their press did. */
export function applyMarkAll(
  answer: MyRepliesResponse,
  marked: ReadonlySet<string>,
  readThrough: string
): MyRepliesResponse {
  if (marked.size === 0) return answer;
  return {
    ...answer,
    rows: (answer.rows ?? []).map((row) =>
      marked.has(rowId({ kind: "thread", thread: row })) ? readRow(row, readThrough) : row
    ),
  };
}

/** What a press holds of the unread count: the rows it takes out of it (`unread-count.ts`). */
interface MarkAllDrop {
  readonly drop: UnreadDrop | undefined;
}

/** The threads of `threads` one press marks, by row id. */
function markedRows(threads: MyRepliesResponse, body: InboxReadRequest): Set<string> {
  return new Set(
    (threads.rows ?? [])
      .filter((thread) => marksThread(thread, body))
      .map((thread) => rowId({ kind: "thread", thread }))
  );
}

export interface MarkAllRead {
  /** Marks every unread thread the list is showing read. */
  readonly markAll: () => void;
  /** Whether the last press was refused, which is the one thing this says to the reader. */
  readonly refused: boolean;
  /** Whether the list holds a row this would mark: the button shows only then. */
  readonly offered: boolean;
}

/**
 * The Inbox's **Mark all read**, for one list: its own filters, its own read mark, its own
 * optimistic apply. The drawer mounts its own Inbox, so it gets its own. A refused press stops
 * being composed: the list is what Dispatch last gave it with every other live write's change
 * over it (R10 of dispatch://INFRA-299), and `unread-count.ts` takes the press's
 * drop off the count.
 *
 * The press writes two reads at once - the Inbox's own list and the unread count every badge
 * shares - so the rows are drawn as read and the Type filter's counts, the sidebar's Inbox count
 * and the compact header's Inbox link are all down before the server answers
 * (`unread-count.ts`). The Agents page's **New replies** badge is the server's own count of
 * unread agent replies, so it drops when the answer lands and the viewer's agent state is read
 * again.
 *
 * The Dismissed view has no press of its own: it holds what the reader has already dealt with, so
 * the Inbox passes no read mark there and nothing is offered (`asOf`).
 */
export function useMarkAllRead({
  asOf,
  filters,
  rows,
}: {
  /** The `as_of` of the list read the page is showing, which is the read mark the press sends;
   *  undefined while the list has not been read, where the press is not offered. */
  readonly asOf: string | undefined;
  readonly filters: InboxFilters;
  /** The rows the list is showing, narrowed as the reader narrowed it. The button is offered
   *  while one of them is a thread this would mark, so a list with no thread row in it - an
   *  agent filter, `?section=needs-you`, a selection naming somebody else - offers none. */
  readonly rows: readonly InboxListRow[];
}): MarkAllRead {
  const cache = useCache();
  const [refused, setRefused] = useState(false);
  const threadsKey = inboxThreadsQuery().queryKey;
  const countKey = myRepliesCountQuery().queryKey;
  const body = asOf === undefined ? undefined : markAllReadBody(filters, asOf);
  // The button is offered only while the list holds an unread Reply or Agent row, which is what
  // the spec asks for: a list showing none has nothing for one press to mark.
  const offered =
    body !== undefined &&
    rows.some((row) => row.kind === "thread" && marksThread(row.thread, body));

  const write = useKeyedWrite<unknown, InboxReadRequest, MarkAllDrop>({
    inline: true,
    label: () => "Inbox marked read",
    onError: (_error, _input, press) => {
      setRefused(true);
      refusedUnreadDrop(cache, press?.drop);
    },
    onMutate: (input) => {
      setRefused(false);
      // The rows the press takes out of the unread count, counted once, here.
      const threads = cache.shown<MyRepliesResponse>(threadsKey);
      return {
        drop:
          threads === undefined
            ? undefined
            : pressUnreadDrop(cache, markedRows(threads, input).size, input),
      };
    },
    onSettled: (_data, _error, _input, press) => settledUnreadDrop(cache, press?.drop),
    onSuccess: () => {
      // What the server marked is read again from the server: the list, its counts, and the
      // per-session agent state the Agents page's **New replies** badge counts.
      void cache.invalidate({ queryKey: myRepliesKey });
      void cache.invalidate({ queryKey: userAgentStateQuery().queryKey });
    },
    request: (input) => markInboxReadRequest(input),
    /** The mark this press shows: every row of the list it marks drawn as read, and the count
     *  every badge shares held under every press live on it (`shownUnreadCount`) - once
     *  Dispatch made it, its own drop alone (`madeUnreadCount`). The rows are read off the cache
     *  as it stands, so an answer this is shown over again marks the rows that answer carries,
     *  and one that already holds them read is left as it is. */
    shows: (input) => (shown) => {
      const through = input.read_through;
      if (through === undefined || through === null) return;
      shown.on<MyRepliesResponse>(threadsKey, (threads) =>
        applyMarkAll(threads, markedRows(threads, input), through)
      );
      shown.on<number>(countKey, shownUnreadCount(cache), madeUnreadCount(cache, input));
    },
    target: () => "inbox:read",
  });

  return {
    markAll: () => {
      if (body !== undefined) write.mutate(body);
    },
    offered,
    refused,
  };
}
