/**
 * The Inbox's **Dismiss**, and the **Move to Inbox** that puts a thread back: what one press
 * sends, what it takes off the page before the server answers, and what the page says when the
 * server refuses it.
 *
 * A thread stays in the Inbox, read or not, until it is dismissed, as an email stays until it is
 * archived: reading it, and **Mark all read**, leave it where it is. **Dismiss** is the one thing
 * that takes it out, and in the Dismissed view the same write with `restore` puts it back.
 *
 * One press is one `POST /api/v1/me/inbox/dismiss` (`inboxDismissRequest`), naming either the row
 * ids it changes with what the page drew in each of them - which is every press made over a
 * list the reader has whole, a **Select all** of the unpaged Inbox read included - or, where the
 * list pages at the server and the ids are not all loaded, the narrowing it is under with
 * `dismiss_through`, that list read's own `as_of`, where the server draws the rows itself. The
 * press is applied here first, because the reader pressed it: the rows leave the view they were
 * in and the unread count every badge shares drops by the unread ones among them, held at that
 * bound while the press is out (`unread-count.ts`, which **Mark all read** shares), so it drops
 * once. A refused press puts each row back where it stood and the page says so
 * (`dismissRefusalText`), which an old server with no such route is one way of causing. A row
 * somebody wrote in since the page drew it is no refusal: the server leaves that one row where it
 * is, and the read the answer triggers draws it again, unread.
 */

import { INBOX_DISMISS_ROW_PREFIXES, inboxThreadRow } from "@legion/contracts";
import { useState } from "react";
import { useCache } from "../../api/cache/cache";

import { inboxDismissRequest } from "../../api/client";
import {
  dismissedThreadsQuery,
  inboxThreadsQuery,
  myRepliesCountQuery,
  myRepliesKey,
  userAgentStateQuery,
} from "../../api/queries";
import type {
  InboxDismissRequest,
  InboxDismissResponse,
  InboxDismissSeen,
  MyRepliesResponse,
  MyReplyRow,
} from "../../api/types";
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

/** One row a press names, and what the page drew in it. The server writes a row's mark only
 *  while the row still holds what the page drew, so a press buries no write this page never
 *  drew; why no comparison of times can stand in for that check is written once, on the
 *  increment in `20261105093501_user_threads_record_dismissed.up.sql`. */
export interface InboxDismissRow {
  /** The id `POST /api/v1/me/inbox/dismiss` names the row by. */
  readonly id: string;
  /** What the page drew in the row, as the body carries it (`seen_replies`). */
  readonly seen: InboxDismissSeen;
}

/** What a press names `thread` as, read off the row the page is drawing: a press carries what
 *  the reader was shown, never anything looked up again when the request is built. A Reply row
 *  carries its own version; an Agent row has none of its own, so it carries the newest session
 *  reply it showed and the ids it drew as unread. */
export function dismissThreadRow(thread: MyReplyRow): InboxDismissRow {
  const row = inboxThreadRow(thread);
  return {
    id: rowId({ kind: "thread", thread }),
    seen:
      row.kind === "agent"
        ? { newest_reply: row.reply.id, unread_replies: row.unread_reply_ids }
        : { version: row.version },
  };
}

/** The row `POST /api/v1/me/inbox/dismiss` names `row` as, or undefined for a row it cannot name:
 *  an Ask row has no Dismiss at all, and naming `ask:<id>` is refused. A thread row's own `rowId`
 *  is already that id - a Reply row is `reply:<thread id>` and an Agent row
 *  `agent:<root message id>` - and this is the one place the list's two spellings are held to the
 *  contract's (`INBOX_DISMISS_ROW_PREFIXES`). */
export function dismissRow(row: InboxListRow): InboxDismissRow | undefined {
  if (row.kind !== "thread") return undefined;
  const named = dismissThreadRow(row.thread);
  const prefixes = Object.values(INBOX_DISMISS_ROW_PREFIXES);
  return prefixes.some((prefix) => named.id.startsWith(prefix)) ? named : undefined;
}

/** What one press changes: the rows it names, each with what the page drew in it - a
 *  row's own Dismiss, a picked selection, and a **Select all** of a list the reader has whole -
 *  or the whole group under the list's own narrowing, which only the paged Dismissed view needs:
 *  there the ids behind **Show more** are not loaded, so the request names the narrowing and the
 *  list's read mark instead, and the server draws the rows itself. `restore` puts the
 *  threads back in the Inbox rather than taking them out. */
export type InboxDismissInput =
  | { readonly restore?: boolean; readonly rows: readonly InboxDismissRow[] }
  | { readonly filters: InboxFilters; readonly restore?: boolean; readonly through: string };

/** The body one press sends. A dismissal that names its rows carries one entry per row with
 *  them; the two forms that carry none are the ones the route refuses it on - a restore, which
 *  draws the rows back rather than burying anything, and the narrowing, where the server draws
 *  the rows itself. */
export function inboxDismissBody(input: InboxDismissInput): InboxDismissRequest {
  if (!("rows" in input)) {
    const restore = input.restore === true ? { restore: true } : {};
    return { ...inboxWriteScope(input.filters), dismiss_through: input.through, ...restore };
  }
  const threads = input.rows.map(({ id }) => id);
  if (input.restore === true) return { restore: true, threads };
  return {
    seen_replies: Object.fromEntries(input.rows.map(({ id, seen }) => [id, seen])),
    threads,
  };
}

/**
 * Whether one press changes `thread`: the row ids it names, or - under the narrowing - the
 * server's own rule, which is every thread the list answers under it whose newest reply, or whose
 * dismissal, is at or before the mark (`inWriteScope`).
 */
export function dismissesThread(thread: MyReplyRow, body: InboxDismissRequest): boolean {
  const threads = body.threads;
  if (threads !== undefined && threads !== null) {
    return threads.includes(rowId({ kind: "thread", thread }));
  }
  const through = body.dismiss_through;
  if (through === undefined || through === null) return false;
  const at =
    body.restore === true
      ? (thread.dismissed_at ?? thread.reply.created_at)
      : thread.reply.created_at;
  if (compareTimestamps(at, through) > 0) return false;
  return inWriteScope(thread, body);
}

/** What the page says about a press the server refused: the rows are back, so it says what did
 *  not happen and the reason the server gave, as a refused bulk snooze does (`snoozeRefusalText`
 *  in `useInboxSelection.ts`). */
export function dismissRefusalText(count: number, restore: boolean, reason: string): string {
  const threads = `${count} ${count === 1 ? "thread" : "threads"}`;
  const what = restore ? `move ${threads} back to the Inbox` : `dismiss ${threads}`;
  return `Could not ${what}: ${reason}.`;
}

/** `answer` without the rows the press takes, and what it took. */
function withoutRows(
  answer: MyRepliesResponse,
  body: InboxDismissRequest
): { answer: MyRepliesResponse; dropped: readonly MyReplyRow[] } {
  const dropped: MyReplyRow[] = [];
  const rows = (answer.rows ?? []).filter((row) => {
    if (!dismissesThread(row, body)) return true;
    dropped.push(row);
    return false;
  });
  if (dropped.length === 0) return { answer, dropped };
  return {
    answer: { ...answer, rows, total: Math.max(0, answer.total - dropped.length) },
    dropped,
  };
}

/** What a press took, read at the press: the drop it made in the unread count
 *  (`unread-count.ts`), and the rows it took out of the view it was made in. Only one of the two
 *  views is written: a dismiss is made in the Inbox, a restore in the Dismissed view. A refused
 *  press stops being composed on them, as every change the store shows does
 *  (`WriteSpec.shows`). */
interface DismissPress {
  readonly drop: UnreadDrop | undefined;
  /** What left the Inbox's own read. */
  readonly inbox: readonly MyReplyRow[];
  /** What left each page of the Dismissed read, by page. */
  readonly dismissed: readonly (readonly MyReplyRow[])[];
}

/** Every row a press took out of the views it was applied to, both sides at once: a dismiss
 *  takes them out of the Inbox's own read and a restore out of the Dismissed read's pages, so a
 *  press that names no row knows what it moved only from these. One rule for the count a refusal
 *  names and for the ids `onSettled` hands back, so the two can never disagree. */
function droppedRows(press: DismissPress | undefined): readonly MyReplyRow[] {
  return [...(press?.inbox ?? []), ...(press?.dismissed ?? []).flat()];
}

/** The pages of the Dismissed read, as the infinite query holds them. */
interface DismissedPages {
  readonly pageParams: readonly unknown[];
  readonly pages: readonly MyRepliesResponse[];
}

export interface InboxDismiss {
  /** The row ids whose own press is still out, so each control says what it is doing. Empty for
   *  a press made under the narrowing, which names no row. */
  readonly pending: ReadonlySet<string>;
  /** What the reader is told about the last refused press, until the next one starts. */
  readonly refusal: string | undefined;
  /** Dismisses, or restores, the rows one press names. */
  readonly submit: (input: InboxDismissInput) => void;
}

/**
 * The Inbox's one dismiss write, for one list: its own optimistic apply and its own refusal. The
 * Inbox owns it rather than each control, because the write takes the row - and with it every
 * control drawn on it - off the page, so a refusal owned by the row would be unmounted before it
 * arrived. The drawer mounts its own Inbox, so it gets its own. A refused press stops being
 * composed: the views it wrote are what Dispatch last gave them, every other live write's change
 * over them (R10 of dispatch://INFRA-299), and `unread-count.ts` takes the
 * press's drop off the count.
 *
 * `onSettled` is told the rows one press named once Dispatch has decided it, either way: the
 * Inbox keeps those rows out of its hand-hold while the press is out, and a row the press no
 * longer takes out is an ordinary row again.
 */
export function useInboxDismiss({
  onSettled,
}: {
  readonly onSettled?: (ids: readonly string[]) => void;
} = {}): InboxDismiss {
  const cache = useCache();
  const [pending, setPending] = useState<ReadonlySet<string>>(() => new Set());
  const [refusal, setRefusal] = useState<string | undefined>(undefined);
  const inboxKey = inboxThreadsQuery().queryKey;
  const dismissedKey = dismissedThreadsQuery().queryKey;
  const countKey = myRepliesCountQuery().queryKey;

  const live = (rows: readonly string[], out: boolean) => {
    setPending((current) => {
      const next = new Set(current);
      for (const id of rows) {
        if (out) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  };

  const write = useKeyedWrite<InboxDismissResponse, InboxDismissInput, DismissPress>({
    inline: true,
    label: (input) => (input.restore === true ? "Threads moved to the Inbox" : "Threads dismissed"),
    onError: (error, input, press) => {
      // What the press did not do, which is what the reader is told. A press under the
      // narrowing names no row, and a restore's rows come out of the Dismissed read rather than
      // the Inbox's, so the count is every row the optimistic apply took out, either side.
      const taken = "rows" in input ? input.rows.length : droppedRows(press).length;
      setRefusal(dismissRefusalText(taken, input.restore === true, error.message));
      refusedUnreadDrop(cache, press?.drop);
    },
    onMutate: async (input) => {
      setRefusal(undefined);
      // A read already out was asked before this press.
      await Promise.all(
        [inboxKey, dismissedKey, countKey].map((queryKey) => cache.cancel({ queryKey }))
      );
      const body = inboxDismissBody(input);
      // The rows the press takes, read before it shows its change: what a refusal names and
      // what `onSettled` hands back. A dismiss takes its rows out of the Inbox's own read; a
      // restore takes them out of the Dismissed read's pages. The other view is read again when
      // the answer lands, so a press never has to guess what the row looks like on the far side
      // of it.
      const pages = cache.shown<DismissedPages>(dismissedKey);
      const answer = cache.shown<MyRepliesResponse>(inboxKey);
      const restoring = input.restore === true;
      const dismissed =
        restoring && pages !== undefined
          ? pages.pages.map((page) => withoutRows(page, body).dropped)
          : [];
      const inbox = !restoring && answer !== undefined ? withoutRows(answer, body).dropped : [];
      // A restored thread was marked read by the dismiss that took it out, so putting it back
      // counts for nothing; a dismiss takes its unread rows out of the count, counted once, here.
      const unread = inbox.filter((row) => row.unread).length;
      return {
        dismissed,
        drop: unread > 0 ? pressUnreadDrop(cache, unread, input) : undefined,
        inbox,
      };
    },
    onSuccess: () => {
      // What the server changed is read again from the server: both views, the counts, and the
      // per-session agent state the Agents page's **New replies** badge counts, since a dismiss
      // marks its threads read as well.
      void cache.invalidate({ queryKey: myRepliesKey });
      void cache.invalidate({ queryKey: userAgentStateQuery().queryKey });
    },
    onSettled: (_data, _error, input, context) => {
      settledUnreadDrop(cache, context?.drop);
      if ("rows" in input) {
        const ids = input.rows.map(({ id }) => id);
        live(ids, false);
        onSettled?.(ids);
        return;
      }
      // A press under the narrowing names no row, so the rows it settles are the ones its own
      // change took out of the view.
      onSettled?.(droppedRows(context).map((row) => rowId({ kind: "thread", thread: row })));
    },
    request: (input) => inboxDismissRequest(inboxDismissBody(input)),
    /** The change this press shows: the rows it names out of the view it was made in, and the
     *  count every badge shares held under every press live on it (`shownUnreadCount`) - once
     *  Dispatch made it, its own drop alone (`madeUnreadCount`). Each row is taken out of the
     *  cache as it stands, by its id, so an answer this is shown over again loses the same rows
     *  it names, and one that no longer holds them is left as it is. */
    shows: (input) => {
      const body = inboxDismissBody(input);
      return (shown) => {
        if (input.restore === true) {
          shown.on<DismissedPages>(dismissedKey, (pages) => ({
            ...pages,
            pages: pages.pages.map((page) => withoutRows(page, body).answer),
          }));
        } else {
          shown.on<MyRepliesResponse>(inboxKey, (current) => withoutRows(current, body).answer);
          shown.on<number>(countKey, shownUnreadCount(cache), madeUnreadCount(cache, input));
        }
      };
    },
    target: () => "inbox:dismiss",
  });

  return {
    pending,
    refusal,
    submit: (input) => {
      if ("rows" in input) {
        if (input.rows.length === 0) return;
        live(
          input.rows.map(({ id }) => id),
          true
        );
      }
      write.mutate(input);
    },
  };
}
