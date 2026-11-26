/**
 * The Inbox's **Dismiss**, and the **Move to Inbox** that puts a thread back: what one press
 * sends, what it takes off the page before the server answers, and what the page says when the
 * server refuses it.
 *
 * A thread stays in the Inbox, read or not, until it is dismissed, as an email stays until it is
 * archived: reading it, and **Mark all read**, leave it where it is. **Dismiss** takes it out, and
 * so does the reader's own answer or reply in it, which Dispatch dismisses in the write itself
 * (the own-answer rule, R1); in the Dismissed view the same write with `restore` puts it back.
 *
 * One press is one `POST /api/v1/me/inbox/dismiss` (`inboxDismissRequest`), naming either the row
 * ids it changes with what the page drew in each of them - which is every press made over a
 * list the reader has whole, a **Select all** of the unpaged Inbox read included - or, where the
 * list pages at the server and the ids are not all loaded, the narrowing it is under with
 * `dismiss_through`, that list read's own `as_of`, where the server draws the rows itself. The
 * press is applied here first, because the reader pressed it: the rows leave the view
 * they were in and the unread count every badge shares drops by the unread ones among them. A
 * refused press puts each row back where it stood and the page says so (`dismissRefusalText`),
 * which an old server with no such route is one way of causing. A row somebody wrote in since
 * the page drew it is no refusal: the server leaves that one row where it is, and the read the
 * answer triggers draws it again, unread.
 */

import { INBOX_DISMISS_ROW_PREFIXES, inboxEntryThread, inboxThreadRow } from "@legion/contracts";
import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { inboxDismissRequest } from "../../api/client";
import {
  meInboxCountQuery,
  meInboxDismissedQuery,
  meInboxKey,
  meInboxQuery,
  userAgentStateQuery,
} from "../../api/queries";
import type {
  InboxDismissRequest,
  InboxDismissResponse,
  InboxDismissSeen,
  MeInboxEntry,
  MeInboxResponse,
  MyReplyRow,
} from "../../api/types";
import { inboxThreadsWritten } from "../../api/write-events";
import { compareTimestamps } from "../../lib/timestamps";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import type { InboxFilters } from "./grouping";
import { type InboxListRow, rowId } from "./list-row";
import { cancelMeInbox, dropMeInboxThreads } from "./one-read-cache";
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

/** `count` threads, in the words the page says a press over them in. */
function threadsOf(count: number): string {
  return `${count} ${count === 1 ? "thread" : "threads"}`;
}

/** What the page says about a press the server refused: the rows are back, so it says what did
 *  not happen and the reason the server gave, as a refused bulk snooze does (`snoozeRefusalText`
 *  in `useInboxSelection.ts`). */
export function dismissRefusalText(count: number, restore: boolean, reason: string): string {
  const what = restore
    ? `move ${threadsOf(count)} back to the Inbox`
    : `dismiss ${threadsOf(count)}`;
  return `Could not ${what}: ${reason}.`;
}

/** What the page says about a press the reader stopped (`SendStoppedError`): the rows are back,
 *  as after a refusal, but Dispatch may have made it, and the next press is one of its own, under
 *  a key of its own. The error's own words are a composer's, of a message and its Retry. */
export function dismissStoppedText(count: number, restore: boolean): string {
  const them = count === 1 ? "it" : "them";
  return restore
    ? stoppedWriteNote(`the move of ${threadsOf(count)} back to the Inbox`, "made", {
        anew: `Moving ${them} back again sends a new write.`,
      })
    : stoppedWriteNote(`the dismissal of ${threadsOf(count)}`, "made", {
        anew: `Dismissing ${them} again sends a new write.`,
      });
}

/** The key the one read lists an entry under: its node, which the read lists once. */
function entryKey(entry: MeInboxEntry): string {
  return `${entry.node.kind}:${entry.node.id}`;
}

/** The pages of the view a press is made in, as they stand: for a restore the Dismissed view's,
 *  and for a dismiss the Inbox answer, as one page. */
function viewPages(queryClient: QueryClient, restore: boolean): readonly MeInboxResponse[] {
  if (restore) return queryClient.getQueryData(meInboxDismissedQuery().queryKey)?.pages ?? [];
  const answer = queryClient.getQueryData(meInboxQuery().queryKey);
  return answer === undefined ? [] : [answer];
}

/** What a press took out of one page of the view it was made in - the Inbox answer is one page,
 *  the Dismissed view one for each **Show more** - with the order that page stood in before it,
 *  so a refusal puts each entry back where the reader saw it. */
interface DroppedPage {
  /** The key of each entry the page held before the press, in its order. */
  readonly order: readonly string[];
  /** Each entry the press took out of the page, with its place in `order`, in that order. */
  readonly taken: readonly { readonly at: number; readonly entry: MeInboxEntry }[];
}

/** What a press that took the rows `dropped` out of a view took out of each of `pages`, that
 *  view's pages as they stood before it. */
function droppedPages(
  pages: readonly MeInboxResponse[],
  dropped: readonly MyReplyRow[]
): DroppedPage[] {
  const ids = new Set(dropped.map((thread) => rowId({ kind: "thread", thread })));
  return pages.map((page) => {
    const entries = page.entries ?? [];
    return {
      order: entries.map(entryKey),
      taken: entries.flatMap((entry, at) => {
        const thread = inboxEntryThread(entry);
        return thread !== undefined && ids.has(rowId({ kind: "thread", thread }))
          ? [{ at, entry }]
          : [];
      }),
    };
  });
}

/** `page` with the entries a refused press took out of it (`dropped`) put back, each before the
 *  first entry that stood after it at the press and stands still, else at the end: the place it
 *  held, whichever entries another press has taken out or put back since. An entry the page
 *  already holds - a read that landed since the press brought it back - is left where it is,
 *  since putting it in again would draw it twice. Answers the page and the entries put back. */
function withTaken(
  page: MeInboxResponse,
  dropped: DroppedPage | undefined
): { readonly page: MeInboxResponse; readonly back: readonly MeInboxEntry[] } {
  if (dropped === undefined || dropped.taken.length === 0) return { page, back: [] };
  const entries = [...(page.entries ?? [])];
  const rank = new Map(dropped.order.map((key, at) => [key, at]));
  const back: MeInboxEntry[] = [];
  for (const { at, entry } of dropped.taken) {
    const key = entryKey(entry);
    if (entries.some((standing) => entryKey(standing) === key)) continue;
    const next = entries.findIndex((standing) => (rank.get(entryKey(standing)) ?? -1) > at);
    entries.splice(next === -1 ? entries.length : next, 0, entry);
    back.push(entry);
  }
  if (back.length === 0) return { page, back };
  return { page: { ...page, entries, total: page.total + back.length }, back };
}

/** What a press rolls back to: the rows it took out of the view it was made in, and what it took
 *  out of each page of that view. Only one of the two views is written: a dismiss is made in the
 *  Inbox, a restore in the Dismissed view. */
interface DismissRollback {
  readonly dropped: readonly MyReplyRow[];
  readonly pages: readonly DroppedPage[];
}

/**
 * Puts back what a refused press took out of the view it was made in (`withTaken`), and, for a
 * dismiss, raises the unread count by the unread rows among them, as the press lowered it. Only
 * that press's own entries go back, and the count rises by them alone: every other entry stays as
 * it stands now, which another press still out, or a read since, may have moved.
 */
function putBack(queryClient: QueryClient, restore: boolean, rollback: DismissRollback): void {
  if (restore) {
    const read = queryClient.getQueryData(meInboxDismissedQuery().queryKey);
    if (read === undefined) return;
    const pages = read.pages.map((page, index) => withTaken(page, rollback.pages[index]).page);
    if (pages.every((page, index) => page === read.pages[index])) return;
    queryClient.setQueryData(meInboxDismissedQuery().queryKey, { ...read, pages });
    return;
  }
  const answer = queryClient.getQueryData(meInboxQuery().queryKey);
  if (answer === undefined) return;
  const { page, back } = withTaken(answer, rollback.pages[0]);
  if (back.length === 0) return;
  queryClient.setQueryData(meInboxQuery().queryKey, page);
  const unread = back.filter((entry) => inboxEntryThread(entry)?.unread === true).length;
  if (unread > 0) {
    queryClient.setQueryData<number>(meInboxCountQuery().queryKey, (count) =>
      count === undefined ? count : count + unread
    );
  }
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
 * The Inbox's one dismiss write, for one list: its own optimistic apply, its own rollback and its
 * own refusal. The Inbox owns it rather than each control, because the write takes the row - and
 * with it every control drawn on it - off the page, so a refusal owned by the row would be
 * unmounted before it arrived. The drawer mounts its own Inbox, so it gets its own.
 *
 * `onSettled` is told the rows one press named once Dispatch has decided it, and whether it
 * refused them: a row the rollback put back is an ordinary row again at once, and a row Dispatch
 * took is still leaving until the page no longer draws it.
 */
export function useInboxDismiss({
  onSettled,
}: {
  readonly onSettled?: (ids: readonly string[], refused: boolean) => void;
} = {}): InboxDismiss {
  const queryClient = useQueryClient();
  const [pending, setPending] = useState<ReadonlySet<string>>(() => new Set());
  const [refusal, setRefusal] = useState<string | undefined>(undefined);
  const heldKeys = [
    meInboxQuery().queryKey,
    meInboxDismissedQuery().queryKey,
    meInboxCountQuery().queryKey,
  ];

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

  const write = useKeyedWrite<InboxDismissResponse, InboxDismissInput, DismissRollback>({
    // A read of either view or the count asked while the press is out waits until Dispatch has
    // taken it (its answer or its thread-state event): Dispatch can answer it before, with the
    // rows it took out still in.
    holds: () => ({ endsOn: inboxThreadsWritten, keys: heldKeys }),
    inline: true,
    label: (input) => (input.restore === true ? "Threads moved to the Inbox" : "Threads dismissed"),
    onError: (error, input, rollback) => {
      // What the press did not do, which is what the reader is told. A press under the
      // narrowing names no row, so the count is every row the optimistic apply took out.
      const taken = "rows" in input ? input.rows.length : (rollback?.dropped.length ?? 0);
      setRefusal(
        error instanceof SendStoppedError
          ? dismissStoppedText(taken, input.restore === true)
          : dismissRefusalText(taken, input.restore === true, error.message)
      );
      if (rollback === undefined) return;
      putBack(queryClient, input.restore === true, rollback);
    },
    onMutate: async (input) => {
      setRefusal(undefined);
      // A read already out can land after this press's own apply and put every row it took back
      // on the page. Cancel the reads this press writes first, as every optimistic write here does.
      await cancelMeInbox(queryClient);
      const body = inboxDismissBody(input);
      const restore = input.restore === true;
      // The pages of the view the press is made in as they stand before it, which say where each
      // row it takes out stood there.
      const before = viewPages(queryClient, restore);
      // A dismiss takes its rows out of the Inbox answer, lowering the unread count by the unread
      // ones among them; a restore takes them out of the Dismissed pages, which counts for
      // nothing, since the dismiss that took a thread out marked it read. The other view is read
      // again when the answer lands, so a press never has to guess what the row looks like on the
      // far side of it.
      const dropped = dropMeInboxThreads(queryClient, restore, (thread) =>
        dismissesThread(thread, body)
      );
      return { dropped, pages: droppedPages(before, dropped) };
    },
    onSuccess: () => {
      // What the server changed is read again from the server: both views, the counts, and the
      // per-session agent state the Agents page's **New replies** badge counts, since a dismiss
      // marks its threads read as well.
      void queryClient.invalidateQueries({ queryKey: meInboxKey });
      void queryClient.invalidateQueries({ queryKey: userAgentStateQuery().queryKey });
    },
    onSettled: (_data, error, input, context) => {
      const refused = error !== null;
      if ("rows" in input) {
        const ids = input.rows.map(({ id }) => id);
        live(ids, false);
        onSettled?.(ids, refused);
        return;
      }
      // A press under the narrowing names no row, so the rows it settles are the ones its own
      // optimistic apply took out of the view.
      onSettled?.(
        (context?.dropped ?? []).map((thread) => rowId({ kind: "thread", thread })),
        refused
      );
    },
    request: (input) => inboxDismissRequest(inboxDismissBody(input)),
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
