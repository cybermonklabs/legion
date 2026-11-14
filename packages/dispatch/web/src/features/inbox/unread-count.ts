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
 * The bound of each press is read when it is composed, from the presses live then. A press made
 * leaves and takes its rows out of the count before every earlier press still out: Dispatch has
 * taken them, whatever becomes of those; and the store goes on composing it over the count the
 * cache holds, Dispatch's own, until the count answers a read begun after the landing, as its
 * own drop alone (`madeUnreadCount`): the count before every press that was still out ahead of
 * it, less its own rows, never the bound of a press still out beside it. A count read sent before
 * the landing may answer after it with the count as it stood, or with the press already counted;
 * held under that bound, either reads the same and the count drops once. A refused press leaves
 * at once and takes its rows out of the bound of every later press still out, and the store
 * composes the count again (`Writes.recompose`): the count Dispatch last gave under the bound of
 * every press still out. The store reads the count again as the press leaves, whose answer
 * counts every press Dispatch made and is composed under every press still out; while that read
 * is out, or once it fails, the count composed from Dispatch's last is what the badge shows.
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
  /** Once Dispatch made the press and it settled, the count its making leaves (`madeBound`),
   *  which the store goes on composing while it holds the landed press; nothing before. */
  made: number | undefined;
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
 * its press and before it shows its change: the count before it is the count the person sees
 * (`Writes.shown`), every press still out composed over Dispatch's. Undefined while the count has
 * not been read, where there is nothing to drop; the count's first answer is then Dispatch's own.
 */
export function pressUnreadDrop(
  client: QueryClient,
  taken: number,
  press: object
): UnreadDrop | undefined {
  const before = writesOf(client).shown<number>(myRepliesCountQuery().queryKey);
  if (before === undefined) return undefined;
  const drop: UnreadDrop = { before, made: undefined, taken };
  liveDrops(client).push(drop);
  dropOfPress.set(press, drop);
  return drop;
}

/**
 * The change every press that drops the count shows on it, composed over every answer the count
 * takes while the press is live: the count held under the bound of every press live when it is
 * composed, the count before that press less its own rows. The store composes each live press in
 * the order they were pressed, and each holds the count under the same bounds, so a count
 * composed again is the same count however many presses are out.
 */
export function shownUnreadCount(client: QueryClient): (count: number) => number {
  return (count) =>
    (dropsOf.get(client) ?? []).reduce(
      (shown, drop) => Math.min(shown, Math.max(0, drop.before - drop.taken)),
      count
    );
}

/** The count the press of `drop`, still live, leaves once Dispatch made it: at most the count
 *  before every press still out ahead of it, less its own rows; nothing for a press not live. */
function madeBound(client: QueryClient, drop: UnreadDrop): number | undefined {
  const live = dropsOf.get(client) ?? [];
  const at = live.indexOf(drop);
  if (at === -1) return undefined;
  const before = live.slice(0, at).reduce((sum, earlier) => sum + earlier.taken, drop.before);
  return Math.max(0, before - drop.taken);
}

/**
 * What the press made with `press` leaves of the count Dispatch last gave once Dispatch made it
 * (`Shown.on`'s `made`, which the store composes over the count until it answers a read begun
 * after the landing): its own drop alone, held under `madeBound` - as it stands while the press
 * is still settling, and as it stood when it settled from then on - and never the bound of a
 * press still out beside it, which the store goes on composing over the count while that press
 * is out. Held under a bound, a count that already counts the press is left as it is, so the
 * count drops once. A press that registered no drop leaves the count as it is.
 */
export function madeUnreadCount(client: QueryClient, press: object): (count: number) => number {
  return (count) => {
    const drop = dropOfPress.get(press);
    const bound = drop === undefined ? undefined : (drop.made ?? madeBound(client, drop));
    return bound === undefined ? count : Math.min(count, bound);
  };
}

/**
 * The press of `drop` was refused, or let go with nothing made: it leaves, every press after it
 * stops counting its rows, and the count is composed again under the bound of every press still
 * live (`Writes.recompose` of the count's key) - the store stopped composing this press as it
 * left, and its own bound would otherwise hold the count down until the next answer. It leaves
 * here rather than when its write settles for the same reason.
 */
export function refusedUnreadDrop(client: QueryClient, drop: UnreadDrop | undefined): void {
  if (drop === undefined) return;
  const live = liveDrops(client);
  const at = live.indexOf(drop);
  if (at === -1) return;
  live.splice(at, 1);
  for (const later of live.slice(at)) later.before += drop.taken;
  writesOf(client).recompose(myRepliesCountQuery().queryKey);
}

/** The write of `drop`'s press settled, either way: it no longer holds the count. A refused one
 *  has left already (`refusedUnreadDrop`), so one still live here was made: Dispatch took its
 *  rows, its own drop is kept as its making left it for as long as the store composes it
 *  (`UnreadDrop.made`), and every earlier press still out stops counting them in the count
 *  before it, which its refusal would add back. */
export function settledUnreadDrop(client: QueryClient, drop: UnreadDrop | undefined): void {
  if (drop === undefined) return;
  const live = liveDrops(client);
  const at = live.indexOf(drop);
  if (at === -1) return;
  drop.made = madeBound(client, drop);
  live.splice(at, 1);
  for (const earlier of live.slice(0, at)) earlier.before -= drop.taken;
}
