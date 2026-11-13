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
 * taken them, whatever becomes of those; and it leaves in the count Dispatch last gave, which the
 * store keeps apart from the screen, its own drop alone (`madeUnreadCount`), never the bound of a
 * press still out beside it, which the store goes on drawing over that count. A refused press
 * leaves at once and takes its rows out of the bound of every later press still out, and the
 * store draws the count again (`Writes.redraw`): the count Dispatch last gave (`Writes.#copies`)
 * under the bound of every press still out. The store reads the count again as the press leaves,
 * whose answer counts every press Dispatch made and is drawn under every press still out; while
 * that read is out, or once it fails, the count drawn from Dispatch's last is what the badge
 * shows.
 */

import type { QueryClient } from "@tanstack/react-query";

import { myRepliesCountQuery } from "../../api/queries";
import { writesOf } from "../writes/write-store";

/** One press that drops the count, from its press until its write settles or is refused. */
export interface UnreadDrop {
  /** The count before this press: the cache at the press, with the rows of every earlier press
   *  refused since added back and those of every later press made since taken out. */
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

/** The drop each press registered, by the variables the press was made with: the one value
 *  TanStack hands both a mutation's `onMutate`, which registers the drop, and its `mutationFn`,
 *  where the store is handed the press's change, so that change finds its own drop. */
const dropOfPress = new WeakMap<object, UnreadDrop>();

/**
 * Registers the press made with `press` that takes `taken` unread threads out of the count, at
 * its press and before it shows its change. Undefined while the count has not been read, where
 * there is nothing to drop; the count's first answer is then Dispatch's own.
 */
export function pressUnreadDrop(
  client: QueryClient,
  taken: number,
  press: object
): UnreadDrop | undefined {
  const before = client.getQueryData<number>(myRepliesCountQuery().queryKey);
  if (before === undefined) return undefined;
  const drop: UnreadDrop = { before, taken };
  liveDrops(client).push(drop);
  dropOfPress.set(press, drop);
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
 * What the press made with `press` leaves of the count Dispatch last gave once Dispatch made it
 * (`Shown.on`'s `made`, which the store writes into its copy of the count): its own drop alone -
 * at most the count before every press still out ahead of it, less the rows this press took -
 * and never the bound of a press still out beside it, which the store goes on drawing over the
 * copy while that press is out. A press that registered no drop leaves the count as it is.
 */
export function madeUnreadCount(client: QueryClient, press: object): (count: number) => number {
  return (count) => {
    const drop = dropOfPress.get(press);
    const live = dropsOf.get(client) ?? [];
    const at = drop === undefined ? -1 : live.indexOf(drop);
    if (drop === undefined || at === -1) return count;
    const before = live.slice(0, at).reduce((sum, earlier) => sum + earlier.taken, drop.before);
    return Math.min(count, Math.max(0, before - drop.taken));
  };
}

/**
 * The press of `drop` was refused, or let go with nothing made: it leaves, every press after it
 * stops counting its rows, and the count is drawn again under the bound of every press still
 * live (`Writes.redraw`) - the store stopped drawing this press as it left, and its own bound
 * would otherwise hold the count down until the next answer. It leaves here rather than when
 * its write settles for the same reason.
 */
export function refusedUnreadDrop(client: QueryClient, drop: UnreadDrop | undefined): void {
  if (drop === undefined) return;
  const live = liveDrops(client);
  const at = live.indexOf(drop);
  if (at === -1) return;
  live.splice(at, 1);
  for (const later of live.slice(at)) later.before += drop.taken;
  writesOf(client).redraw(myRepliesCountQuery().queryKey);
}

/** The write of `drop`'s press settled, either way: it no longer holds the count. A refused one
 *  has left already (`refusedUnreadDrop`), so one still live here was made: Dispatch took its
 *  rows, and every earlier press still out stops counting them in the count before it, which
 *  its refusal would add back. */
export function settledUnreadDrop(client: QueryClient, drop: UnreadDrop | undefined): void {
  if (drop === undefined) return;
  const live = liveDrops(client);
  const at = live.indexOf(drop);
  if (at === -1) return;
  live.splice(at, 1);
  for (const earlier of live.slice(0, at)) earlier.before -= drop.taken;
}
