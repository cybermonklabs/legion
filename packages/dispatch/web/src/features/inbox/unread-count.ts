/**
 * The unread count every badge shares, as the presses that drop it show it while they are out:
 * **Mark all read** (`mark-all.ts`) and **Dismiss** (`useInboxDismiss.ts`). One rule for both,
 * held per `QueryClient`, because two presses out at once each show their drop on the one count
 * and a refusal of either must take back its own drop alone.
 *
 * A count holds no rows, so an answer that lands while a press is out cannot say whether it
 * already counts the press. Each press therefore holds the count at a bound - at most the count
 * before the press, less what the press takes - so an answer that already counts it is shown as
 * Dispatch gave it and one that does not is held at the bound: the count drops once and never
 * twice. A thread that arrives while a press is out is held under the bound until the press's own
 * read of the count, once its answer lands, sets it.
 *
 * The bound of each press is read when it is shown, from the presses live then. A press made
 * leaves and takes its rows out of the count before every earlier press still out: Dispatch has
 * taken them, whatever becomes of those. A refused press leaves at once, takes its rows out of
 * the bound of every later press still out, and puts the count back to what it stood at before
 * it, less the rows of every later press already made - the value Dispatch last confirmed for
 * the rows the presses touched. A thread may have arrived or been read elsewhere meanwhile, so a
 * refusal also reads the count again from Dispatch, whose answer counts every press it made and
 * is shown under every press still out; while that read is out, or once it fails, the count put
 * back is what the badge shows.
 */

import type { QueryClient } from "@tanstack/react-query";

import { myRepliesCountQuery } from "../../api/queries";

/** One press that drops the count, from its press until its write settles or is refused. */
export interface UnreadDrop {
  /** The count before this press: the cache at the press, with the rows of every earlier press
   *  refused since put back and those of every later press made since taken out. */
  before: number;
  /** The unread threads this press takes out of the count. */
  readonly taken: number;
}

/** The live drops of each client, in press order. */
const dropsOf = new WeakMap<QueryClient, UnreadDrop[]>();

function liveDrops(client: QueryClient): UnreadDrop[] {
  let live = dropsOf.get(client);
  if (live === undefined) {
    live = [];
    dropsOf.set(client, live);
  }
  return live;
}

/**
 * Registers a press that takes `taken` unread threads out of the count, at its press and before
 * it shows its change. Undefined while the count has not been read, where there is nothing to
 * drop; the count's first answer is then Dispatch's own.
 */
export function pressUnreadDrop(client: QueryClient, taken: number): UnreadDrop | undefined {
  const before = client.getQueryData<number>(myRepliesCountQuery().queryKey);
  if (before === undefined) return undefined;
  const drop: UnreadDrop = { before, taken };
  liveDrops(client).push(drop);
  return drop;
}

/**
 * The change every press that drops the count shows on it, at its press and over every answer the
 * store shows it again over: the count held under the bound of every press live when it is shown,
 * the count before that press less its own rows. The store shows each live press in the order
 * they were pressed, and each holds the count under the same bounds, so a count shown over again
 * is the same count however many presses are out.
 */
export function shownUnreadCount(client: QueryClient): (count: number) => number {
  return (count) =>
    (dropsOf.get(client) ?? []).reduce(
      (shown, drop) => Math.min(shown, Math.max(0, drop.before - drop.taken)),
      count
    );
}

/**
 * The press of `drop` was refused, or let go with nothing made: it leaves, every press after it
 * stops counting its rows, the count goes back to what it stood at before it less the rows of
 * every later press already made (`settledUnreadDrop`), over which the store shows every press
 * still live, and the count is read again from Dispatch. It leaves here rather than when its
 * write settles, since its own bound would otherwise hold that count down.
 */
export function refusedUnreadDrop(client: QueryClient, drop: UnreadDrop | undefined): void {
  if (drop === undefined) return;
  const live = liveDrops(client);
  const at = live.indexOf(drop);
  if (at === -1) return;
  live.splice(at, 1);
  for (const later of live.slice(at)) later.before += drop.taken;
  const countKey = myRepliesCountQuery().queryKey;
  client.setQueryData(countKey, drop.before);
  void client.invalidateQueries({ exact: true, queryKey: countKey });
}

/** The write of `drop`'s press settled, either way: it no longer holds the count. A refused one
 *  has left already (`refusedUnreadDrop`), so one still live here was made: Dispatch took its
 *  rows, and every earlier press still out stops counting them in the count before it, which its
 *  refusal would put back. */
export function settledUnreadDrop(client: QueryClient, drop: UnreadDrop | undefined): void {
  if (drop === undefined) return;
  const live = liveDrops(client);
  const at = live.indexOf(drop);
  if (at === -1) return;
  live.splice(at, 1);
  for (const earlier of live.slice(0, at)) earlier.before -= drop.taken;
}
