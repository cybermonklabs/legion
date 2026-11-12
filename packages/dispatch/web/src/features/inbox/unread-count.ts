/**
 * The unread count every badge shares, as the presses that drop it show it while they are out:
 * **Mark all read** (`mark-all.ts`) and **Dismiss** (`useInboxDismiss.ts`). One rule for both,
 * held per `QueryClient`, because two presses out at once each show their drop on the one count
 * and a refusal of either must take back its own drop alone.
 *
 * A count holds no rows, so an answer that lands while a press is out cannot say whether it
 * already counts the press. Each press therefore holds the count at a bound - at most what
 * Dispatch said before the press, less what the press takes - so an answer that already counts it
 * is shown as Dispatch gave it and one that does not is held at the bound: the count drops once
 * and never twice. A thread that arrives while a press is out is held under the bound until the
 * press's own read of the count, once its answer lands, sets it.
 *
 * The bound of each press is read when it is shown, from the presses live then, never fixed at
 * the press: a press refused while a later one is out takes its rows out of that later press's
 * bound the moment it leaves, so the count comes back by exactly the rows Dispatch did not take.
 */

import type { QueryClient } from "@tanstack/react-query";

import { myRepliesCountQuery } from "../../api/queries";

/** One press that drops the count, from its press until its write settles. */
export interface UnreadDrop {
  /** The count as Dispatch gave it at the press: the cache, with the rows of every press live
   *  then put back, since the cache holds what each of those presses shows. */
  readonly server: number;
  /** The unread threads this press takes out of the count. */
  readonly taken: number;
  /** The unread threads of the presses before this one that `server` does not count: every press
   *  live at this one's press, less each of them refused since. One that was made stays counted,
   *  since `server` was read before Dispatch made it. */
  earlier: number;
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
  const count = client.getQueryData<number>(myRepliesCountQuery().queryKey);
  if (count === undefined) return undefined;
  const live = liveDrops(client);
  const earlier = live.reduce((sum, drop) => sum + drop.taken, 0);
  const drop: UnreadDrop = { earlier, server: count + earlier, taken };
  live.push(drop);
  return drop;
}

/**
 * The change every press that drops the count shows on it, at its press and over every answer the
 * store shows it again over: the count held under the bound of every press live when it is shown,
 * which is Dispatch's count at that press less its own rows and the rows of the earlier presses
 * that count does not hold. The store shows each live press in the order they were pressed, and
 * each holds the count under the same bounds, so a count shown over again is the same count
 * however many presses are out.
 */
export function shownUnreadCount(client: QueryClient): (count: number) => number {
  return (count) =>
    (dropsOf.get(client) ?? []).reduce(
      (shown, drop) => Math.min(shown, Math.max(0, drop.server - drop.earlier - drop.taken)),
      count
    );
}

/**
 * The press of `drop` was refused, or let go with nothing made: it leaves, every press after it
 * stops counting its rows, and the count goes back to what it stood at under the presses before
 * it - Dispatch's count at the press less those of them not refused - over which the store shows
 * every press still live.
 */
export function refusedUnreadDrop(client: QueryClient, drop: UnreadDrop | undefined): void {
  if (drop === undefined) return;
  const live = liveDrops(client);
  const at = live.indexOf(drop);
  if (at === -1) return;
  live.splice(at, 1);
  for (const later of live.slice(at)) later.earlier -= drop.taken;
  client.setQueryData(myRepliesCountQuery().queryKey, drop.server - drop.earlier);
}

/** The write of `drop`'s press settled, either way: it no longer holds the count. A refused one
 *  has left already (`refusedUnreadDrop`). */
export function settledUnreadDrop(client: QueryClient, drop: UnreadDrop | undefined): void {
  const live = liveDrops(client);
  const at = drop === undefined ? -1 : live.indexOf(drop);
  if (at !== -1) live.splice(at, 1);
}
