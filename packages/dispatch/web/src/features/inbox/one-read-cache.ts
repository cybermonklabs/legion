/**
 * The optimistic writes over the one read's cache, `meInboxKey`. The
 * reader's own asks, their Reply and Agent rows and their unread count are drawn from it, so every
 * write that moves one of them on the press - a hand-on, an assignee, a priority, a snooze, an
 * answer or a decline, Mark all read, a dismissal - moves it here as well as in the open asks'
 * reads it moves (`open-asks-cache.ts`), puts it back where the server refuses the write, and
 * reads the one read again once the server has decided it.
 *
 * Each edit leaves alone what it does not name: the count where it is given no count edit, and an
 * answer none of whose entries it changes. Nothing here is a hook, so the writes call it from
 * their own callbacks.
 */

import { inboxEntryThread, meInboxThreads } from "@legion/contracts";
import { hashKey, type QueryClient, type QueryKey } from "@tanstack/react-query";

import {
  meInboxCountQuery,
  meInboxDismissedQuery,
  meInboxKey,
  meInboxQuery,
} from "../../api/queries";
import { replaceQueryData } from "../../api/query-client";
import type { MeInboxEntry, MeInboxResponse, MyReplyRow, OpenAskRow } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";

/** Every cached read of the one read as it stood before a write moved it, for the rollback. */
export type MeInboxSnapshot = readonly (readonly [QueryKey, unknown])[];

/** How a write leaves one entry of an answer: the entry it leaves, or undefined where it takes the
 *  entry out. The entry itself where it changes nothing. */
type MeInboxEntryEdit = (entry: MeInboxEntry) => MeInboxEntry | undefined;

/** `answer` as `edit` leaves its entries, each entry taken out lowering its `total`; the answer
 *  itself where no entry changed. */
function editAnswer(answer: MeInboxResponse, edit: MeInboxEntryEdit): MeInboxResponse {
  const entries = answer.entries ?? [];
  let changed = false;
  const kept: MeInboxEntry[] = [];
  for (const entry of entries) {
    const next = edit(entry);
    if (next !== entry) changed = true;
    if (next !== undefined) kept.push(next);
  }
  if (!changed) return answer;
  return {
    ...answer,
    entries: kept,
    total: Math.max(0, answer.total - (entries.length - kept.length)),
  };
}

/** One cached read of the one read as a write leaves it: an answer's entries or the pages of the
 *  paged read (`meInboxDismissedQuery`) as `edit` leaves them, a count as `count` leaves it, and
 *  anything else - no data, a count with no count edit - as it is. */
function editRead(
  data: unknown,
  edit: MeInboxEntryEdit,
  count: ((count: number) => number) | undefined
): unknown {
  if (typeof data === "number") return count === undefined ? data : count(data);
  const read = data as { readonly entries?: unknown; readonly pages?: unknown } | undefined;
  if (Array.isArray(read?.pages)) {
    const paged = data as { readonly pages: readonly MeInboxResponse[] };
    const pages = paged.pages.map((page) => editAnswer(page, edit));
    return pages.every((page, index) => page === paged.pages[index]) ? data : { ...paged, pages };
  }
  return Array.isArray(read?.entries) ? editAnswer(data as MeInboxResponse, edit) : data;
}

/** What every cached read of the one read holds now, which a write keeps before it edits them. */
export function meInboxSnapshot(queryClient: QueryClient): MeInboxSnapshot {
  return queryClient.getQueriesData({ queryKey: meInboxKey });
}

/** Cancels every read of the one read already out, so none lands over the edit a write is about
 *  to make, as each write cancels the other reads it edits. */
export function cancelMeInbox(queryClient: QueryClient): Promise<void> {
  return queryClient.cancelQueries({ queryKey: meInboxKey });
}

/** Puts every cached read of the one read back as `snapshot` holds it. */
export function restoreMeInbox(
  queryClient: QueryClient,
  snapshot: MeInboxSnapshot | undefined
): void {
  for (const [key, data] of snapshot ?? []) queryClient.setQueryData(key, data);
}

/** Reads the one read again, which a write asks for once the server has decided it; a write
 *  that stays pending until its reads land awaits the answer. */
export function invalidateMeInbox(queryClient: QueryClient): Promise<void> {
  return queryClient.invalidateQueries({ queryKey: meInboxKey });
}

/** Edits every cached read of the one read: each answer's entries by `edit`, and the unread count
 *  by `count` where one is given. */
function patchMeInbox(
  queryClient: QueryClient,
  edit: MeInboxEntryEdit,
  count?: (count: number) => number
): void {
  queryClient.setQueriesData({ queryKey: meInboxKey }, (data: unknown) =>
    editRead(data, edit, count)
  );
}

/** The entry of the one read's Inbox answer that lists `ask`, as Dispatch lists an open ask: what
 *  a write that makes an ask wait on the reader adds to that answer before it reads again. */
export function meInboxAskEntry(ask: OpenAskRow): MeInboxEntry {
  return { ask, moment: ask.created_at, node: { id: ask.id, kind: "ask" }, relations: [] };
}

/**
 * Edits the ask rows of every cached read of the one read: `edit` leaves each ask, or takes its
 * entry out with undefined. Each ask of `joined` - a row the write makes wait on the reader, whom
 * the one read will list it for once it reads again - the reader's own Inbox answer gains an entry
 * for where it holds none, so the row joins their list on the press as it does in the open asks'
 * reads, and the list keeps it (`useInboxAsks`, which draws the reader's own asks from the one
 * read alone).
 */
export function patchMeInboxAsks(
  queryClient: QueryClient,
  edit: (ask: OpenAskRow) => OpenAskRow | undefined,
  joined: readonly OpenAskRow[] = []
): void {
  patchMeInbox(queryClient, (entry) => {
    if (entry.ask === undefined) return entry;
    const ask = edit(entry.ask);
    if (ask === undefined) return undefined;
    return ask === entry.ask ? entry : { ...entry, ask };
  });
  if (joined.length === 0) return;
  queryClient.setQueryData<MeInboxResponse>(meInboxQuery().queryKey, (answer) => {
    if (answer === undefined) return answer;
    const listed = new Set(
      (answer.entries ?? []).flatMap((entry) => (entry.ask ? [entry.ask.id] : []))
    );
    const added = joined.filter((ask) => !listed.has(ask.id)).map(meInboxAskEntry);
    if (added.length === 0) return answer;
    return {
      ...answer,
      entries: [...(answer.entries ?? []), ...added],
      total: answer.total + added.length,
    };
  });
}

/** The answers one cached read of the one read holds: an answer itself, and each page of the
 *  paged read (`meInboxDismissedQuery`) in turn; none for anything else - no data, a count. */
function answersOf(data: unknown): readonly MeInboxResponse[] {
  const read = data as { readonly entries?: unknown; readonly pages?: unknown } | undefined;
  if (Array.isArray(read?.pages)) {
    // Only the paged read holds `pages` under the one read's key, as `editRead` reads it.
    const paged = data as { readonly pages: readonly MeInboxResponse[] };
    return paged.pages;
  }
  return Array.isArray(read?.entries) ? [data as MeInboxResponse] : [];
}

/** `edit`, which leaves a Reply or Agent row as a write leaves it, as an edit of the entry that
 *  carries the row. An entry that carries none, or whose row `edit` leaves as it is, stays
 *  itself. */
function threadEntryEdit(edit: (thread: MyReplyRow) => MyReplyRow): MeInboxEntryEdit {
  return (entry) => {
    const thread = inboxEntryThread(entry);
    if (thread === undefined) return entry;
    const next = edit(thread);
    if (next === thread) return entry;
    return entry.agent === undefined
      ? { ...entry, thread: next }
      : { ...entry, agent: next as MeInboxEntry["agent"] };
  };
}

/** Raises the unread count, as the rollback of a refused write does, by the unread rows it put back
 *  in the reader's Inbox answer (`back`), and by no more than its write took off the count
 *  (`taken`): a write lowers the count no further than zero, and a count read before some of its
 *  rows reached the answer holds fewer than the answer does. */
function raiseUnreadCount(queryClient: QueryClient, taken: number, back: number): void {
  const by = Math.min(taken, back);
  if (by === 0) return;
  queryClient.setQueryData<number>(meInboxCountQuery().queryKey, (count) =>
    count === undefined ? count : count + by
  );
}

/** One cached read of the one read as `patchMeInboxThreads` left it: each row it wrote there - the
 *  very object the cache holds, which TanStack's structural sharing makes of the one the write
 *  gave it - with the row of that same read it replaced, and whether that read is the reader's
 *  own Inbox answer, which the unread count counts. */
interface MeInboxReadPatch {
  readonly inbox: boolean;
  readonly queryKey: QueryKey;
  readonly rows: ReadonlyMap<MyReplyRow, MyReplyRow>;
}

/** What `patchMeInboxThreads` wrote, which `unpatchMeInboxThreads` takes back: each cached read of
 *  the one read it changed, and how much it took off the unread count. */
export interface MeInboxThreadsPatch {
  readonly counted: number;
  readonly reads: readonly MeInboxReadPatch[];
}

/**
 * Edits the Reply and Agent rows of every cached read of the one read - `edit` leaves each row -
 * and lowers the unread count by `unread`, the unread rows of the reader's Inbox answer it draws
 * read, no further than zero. Each read is written over a read of it still out
 * (`replaceQueryData`): that read was asked before the edit, and its answer would put the rows
 * back as they stood. Answers what it wrote, for the rollback (`unpatchMeInboxThreads`). Mark all
 * read is the one caller.
 */
export function patchMeInboxThreads(
  queryClient: QueryClient,
  edit: (thread: MyReplyRow) => MyReplyRow,
  unread: number
): MeInboxThreadsPatch {
  const entryEdit = threadEntryEdit(edit);
  const inbox = hashKey(meInboxQuery().queryKey);
  const reads: MeInboxReadPatch[] = [];
  let counted = 0;
  for (const [queryKey, before] of meInboxSnapshot(queryClient)) {
    const written = replaceQueryData(queryClient, queryKey, (data: unknown) =>
      editRead(data, entryEdit, (count) => Math.max(0, count - unread))
    );
    if (typeof before === "number" && typeof written === "number") {
      counted = before - written;
      continue;
    }
    // The edit takes no entry out, so each row the read holds now stands where the row it
    // replaced stood.
    const was = answersOf(before).flatMap((answer) => meInboxThreads(answer));
    const now = answersOf(written).flatMap((answer) => meInboxThreads(answer));
    const rows = new Map<MyReplyRow, MyReplyRow>();
    for (const [at, row] of now.entries()) {
      const old = was[at];
      if (old !== undefined && row !== old) rows.set(row, old);
    }
    if (rows.size > 0) reads.push({ inbox: hashKey(queryKey) === inbox, queryKey, rows });
  }
  return { counted, reads };
}

/**
 * Takes back what `patch` wrote, as the rollback of a refused write does. In each read, each row
 * the write wrote that the cache still holds - that very object - goes back to the row of that
 * read it replaced, so every read keeps its own row, and a row that a read since, or another
 * write, put there stays as it stands. The unread count rises by the unread rows put back in the
 * reader's Inbox answer, and by no more than the write took off it (`raiseUnreadCount`). A read
 * with nothing to put back is not written, and no read of the one read still out is given up.
 */
export function unpatchMeInboxThreads(queryClient: QueryClient, patch: MeInboxThreadsPatch): void {
  let back = 0;
  for (const { inbox, queryKey, rows } of patch.reads) {
    const data = queryClient.getQueryData(queryKey);
    let unread = 0;
    const edit = threadEntryEdit((thread) => {
      const was = rows.get(thread);
      if (was === undefined) return thread;
      if (was.unread) unread += 1;
      return was;
    });
    const next = editRead(data, edit, undefined);
    if (next === data) continue;
    queryClient.setQueryData(queryKey, next);
    if (inbox) back += unread;
  }
  raiseUnreadCount(queryClient, patch.counted, back);
}

/** The Reply and Agent rows the reader's own Inbox answer of the one read holds: what a write
 *  reads to know which rows it moves. */
export function meInboxThreadRows(queryClient: QueryClient): readonly MyReplyRow[] {
  return meInboxThreads(queryClient.getQueryData<MeInboxResponse>(meInboxQuery().queryKey));
}

/** The key the one read lists an entry under: its node, which the read lists once. */
function entryKey(entry: MeInboxEntry): string {
  return `${entry.node.kind}:${entry.node.id}`;
}

/** What `dropMeInboxThreads` took out of one page of a view - the Inbox answer is one page, the
 *  Dismissed view one for each **Show more** - each entry with its place in the page as it stood,
 *  and the key of each entry the page held then, in its order. */
interface MeInboxDroppedPage {
  readonly order: readonly string[];
  readonly taken: readonly { readonly at: number; readonly entry: MeInboxEntry }[];
}

/** What `dropMeInboxThreads` took out of one view of the one read, which `undropMeInboxThreads`
 *  puts back: the view, the rows it took in the view's order, what it took out of each page of
 *  the view, and how much it took off the unread count. */
export interface MeInboxDrop {
  readonly counted: number;
  readonly dismissed: boolean;
  readonly dropped: readonly MyReplyRow[];
  readonly pages: readonly MeInboxDroppedPage[];
}

/**
 * Takes the Reply and Agent rows `drops` names out of one view of the one read - the reader's own
 * Inbox answer, or the pages of what they dismissed where `dismissed` is set - and answers what it
 * took, for the rollback (`undropMeInboxThreads`). The unread rows taken out of the Inbox answer
 * lower the unread count, no further than zero, as the dismissal that takes them marks them read;
 * a row taken out of the dismissed pages was read when it was dismissed, and counts for nothing.
 */
export function dropMeInboxThreads(
  queryClient: QueryClient,
  dismissed: boolean,
  drops: (thread: MyReplyRow) => boolean
): MeInboxDrop {
  const key = dismissed ? meInboxDismissedQuery().queryKey : meInboxQuery().queryKey;
  const before = answersOf(queryClient.getQueryData(key));
  const dropped: MyReplyRow[] = [];
  const edit: MeInboxEntryEdit = (entry) => {
    const thread = inboxEntryThread(entry);
    if (thread === undefined || !drops(thread)) return entry;
    dropped.push(thread);
    return undefined;
  };
  queryClient.setQueryData(key, (data: unknown) => editRead(data, edit, undefined) as never);
  const taken = new Set(dropped);
  const pages = before.map((page): MeInboxDroppedPage => {
    const entries = page.entries ?? [];
    return {
      order: entries.map(entryKey),
      taken: entries.flatMap((entry, at) => {
        const thread = inboxEntryThread(entry);
        return thread !== undefined && taken.has(thread) ? [{ at, entry }] : [];
      }),
    };
  });
  const unread = dropped.filter((thread) => thread.unread).length;
  const count = queryClient.getQueryData<number>(meInboxCountQuery().queryKey);
  if (dismissed || unread === 0 || count === undefined) {
    return { counted: 0, dismissed, dropped, pages };
  }
  const left = Math.max(0, count - unread);
  queryClient.setQueryData<number>(meInboxCountQuery().queryKey, left);
  return { counted: count - left, dismissed, dropped, pages };
}

/** Where the Dismissed view's own order puts `left` against `right`: negative before it, positive
 *  after. The server answers its pages newest dismissal first (the entry's `moment`), then by the
 *  greater node id, a uuid whose text orders as the uuid does (`meInboxDismissedCut` in
 *  `me_inbox.go`, as `myRepliesDismissedCut` in `replies.go`). */
function dismissedOrder(left: MeInboxEntry, right: MeInboxEntry): number {
  const byMoment = compareTimestamps(right.moment, left.moment);
  if (byMoment !== 0) return byMoment;
  if (left.node.id === right.node.id) return 0;
  return left.node.id > right.node.id ? -1 : 1;
}

/**
 * Puts back what `drop` took out, as the rollback of a refused write does: only those entries,
 * each where no read since has brought it back already - a read that holds it again wins, since
 * putting it in again would draw it twice - and every other entry as it stands. Each goes back on
 * the page it was taken from. In the Inbox answer it goes before the first entry that stood after
 * it there and stands still, else at the end: the place it held, whichever entries another write
 * has taken out or put back since, and the Inbox orders its rows again as it draws them. The
 * Dismissed view draws its pages in their own order, so there it goes where that order puts it
 * (`dismissedOrder`). The unread count rises by the unread rows put back in the Inbox answer, and
 * by no more than the drop took off it (`raiseUnreadCount`).
 */
export function undropMeInboxThreads(queryClient: QueryClient, drop: MeInboxDrop): void {
  if (drop.dismissed) {
    const key = meInboxDismissedQuery().queryKey;
    const read = queryClient.getQueryData(key);
    if (read === undefined) return;
    const held = new Set(read.pages.flatMap((page) => (page.entries ?? []).map(entryKey)));
    const pages = read.pages.map((page, index) => {
      const taken = drop.pages[index]?.taken ?? [];
      const back = taken.filter(({ entry }) => !held.has(entryKey(entry)));
      if (back.length === 0) return page;
      const entries = [...(page.entries ?? [])];
      for (const { entry } of back) {
        const next = entries.findIndex((standing) => dismissedOrder(standing, entry) > 0);
        entries.splice(next === -1 ? entries.length : next, 0, entry);
      }
      return { ...page, entries, total: page.total + back.length };
    });
    if (pages.every((page, index) => page === read.pages[index])) return;
    queryClient.setQueryData(key, { ...read, pages });
    return;
  }
  const key = meInboxQuery().queryKey;
  const answer = queryClient.getQueryData(key);
  const dropped = drop.pages[0];
  if (answer === undefined || dropped === undefined) return;
  const entries = [...(answer.entries ?? [])];
  const rank = new Map(dropped.order.map((entry, at) => [entry, at]));
  let unread = 0;
  let back = 0;
  for (const { at, entry } of dropped.taken) {
    const placed = entryKey(entry);
    if (entries.some((standing) => entryKey(standing) === placed)) continue;
    const next = entries.findIndex((standing) => (rank.get(entryKey(standing)) ?? -1) > at);
    entries.splice(next === -1 ? entries.length : next, 0, entry);
    back += 1;
    if (inboxEntryThread(entry)?.unread === true) unread += 1;
  }
  if (back === 0) return;
  queryClient.setQueryData(key, { ...answer, entries, total: answer.total + back });
  raiseUnreadCount(queryClient, drop.counted, unread);
}
