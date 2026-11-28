import {
  hashKey,
  onlineManager,
  type QueryClient,
  type QueryKey,
  useQueryClient,
} from "@tanstack/react-query";
import { useCallback, useSyncExternalStore } from "react";

import { holdHandOff, type WritePress } from "../components/focus-place";
import { NETWORK_ERROR_MESSAGE, REQUEST_DEADLINE_MS, requestTimeoutMessage } from "./client";

/**
 * Why a read has not settled once a bound of `deadlineMs` ended the wait for it, in the words of
 * the error a read that failed for that reason ends with: a `NetworkError`'s while the browser is
 * offline (`onlineManager`), and a `RequestTimeoutError`'s otherwise. A wait with no read to wait
 * on says it too, for the update it waits for in its place (`useHoldUntilDrawn`).
 */
export function unsettledReason(deadlineMs: number): string {
  return onlineManager.isOnline() ? requestTimeoutMessage(deadlineMs) : NETWORK_ERROR_MESSAGE;
}

/**
 * Waits for `read`, the re-read that draws what a landed write changed, for `REQUEST_DEADLINE_MS`
 * at the most. The timer starts at this call, whatever state the query is in. TanStack pauses a
 * fetch the browser starts offline until the network returns, and no request deadline runs while
 * a fetch is paused, so a paused read alone does not settle for as long as the browser is
 * offline. A read of a query whose fetch the network had already paused is the one that does
 * settle at once, since TanStack answers its refetch there and then: a caller that waits on such
 * a read reads again through `query-client.ts`'s `waitingForPausedReads`, which has it answered
 * when the fetch is, and past a cancel of the fetch by another press once a later fetch answers,
 * and this bound ends the wait either way. Answers `undefined` once the read settles, either way.
 * At the bound it answers why the read has not settled (`unsettledReason`).
 */
export function readWithinBound(read: Promise<unknown>): Promise<string | undefined> {
  const { promise, resolve } = Promise.withResolvers<string | undefined>();
  const bound = setTimeout(
    () => resolve(unsettledReason(REQUEST_DEADLINE_MS)),
    REQUEST_DEADLINE_MS
  );
  const settled = () => {
    clearTimeout(bound);
    resolve(undefined);
  };
  read.then(settled, settled);
  return promise;
}

/**
 * The latest hold through one read, as every caller of that read sees it (`useUnsettledRead`),
 * with the read it waits on: whose press made it, whether it holds still, and why its read has
 * not settled once the bound ended it.
 */
interface BoundHold {
  /** The row whose press made the hold, in its list's own key for the row (`useListReadBound`);
   *  `undefined` for a control's own hold (`useReadBound`). */
  readonly row: string | undefined;
  /** Whether the hold is on: from its start until its read settles or `readWithinBound`'s bound
   *  ends, whichever is first. */
  readonly holding: boolean;
  /** Why its read has not settled, once the bound ended the hold, until that read settles;
   *  `undefined` while the hold is on and once the read has settled. */
  readonly unsettled: string | undefined;
  /** The read the hold waits on, which tells a later hold of the same read from this one. */
  readonly read: Promise<unknown>;
}

/**
 * The holds through reads that live with one query client (`boundHoldsOf`): the latest hold of
 * each read, by the hash of the read's query key. Every caller of a read draws this one hold, so
 * two callers of one read share one bound and one reason, and a caller drawn again while the read
 * is out finds the hold and its reason where the caller drawn before it left them. A hold that
 * has ended - it holds no more and its read has settled - is kept only while a caller of its read
 * is drawn: with none drawn the store drops it, since the next caller drawn reads it as no hold.
 */
class BoundHolds {
  readonly #holds = new Map<string, BoundHold>();
  readonly #listeners = new Map<string, Set<() => void>>();

  /** Tells `listener` each time the latest hold of the read whose query key hashes to `hash`
   *  changes. As the last listener of that read goes, a hold of it that has ended goes too. */
  subscribe(hash: string, listener: () => void): () => void {
    let listeners = this.#listeners.get(hash);
    if (listeners === undefined) {
      listeners = new Set();
      this.#listeners.set(hash, listeners);
    }
    listeners.add(listener);
    return () => {
      listeners.delete(listener);
      if (listeners.size > 0) return;
      this.#listeners.delete(hash);
      const hold = this.#holds.get(hash);
      if (hold !== undefined && !hold.holding && hold.unsettled === undefined) {
        this.#holds.delete(hash);
      }
    };
  }

  /** The latest hold through the read whose query key hashes to `hash`: the same object until it
   *  changes, and `undefined` where no hold of that read was taken, or where the latest ended
   *  with no caller of the read drawn. */
  latest(hash: string): BoundHold | undefined {
    return this.#holds.get(hash);
  }

  /**
   * Holds through `read`, a read of the query whose key hashes to `hash`, for `row`: it forgets
   * the earlier hold of that read, and why its read had not settled, as it begins, and answers
   * once `read` settles or `readWithinBound`'s bound ends, whichever is first. Where the bound
   * ends first, the hold keeps why `read` has not settled until it does. An earlier hold whose
   * bound ends, or whose read settles, once a later hold of the same read has begun changes
   * nothing: the store holds the later hold then, or none where that one has ended undrawn.
   */
  async wait(hash: string, row: string | undefined, read: Promise<unknown>): Promise<void> {
    this.#keep(hash, { holding: true, read, row, unsettled: undefined });
    const unsettled = await readWithinBound(read);
    if (this.#holds.get(hash)?.read !== read) return;
    this.#keep(hash, { holding: false, read, row, unsettled });
    if (unsettled === undefined) return;
    const settled = () => {
      if (this.#holds.get(hash)?.read === read) {
        this.#keep(hash, { holding: false, read, row, unsettled: undefined });
      }
    };
    read.then(settled, settled);
  }

  /** Keeps `hold` as the latest of the read whose query key hashes to `hash`, or drops that
   *  read's entry where `hold` has ended and no caller of the read is drawn, and tells that
   *  read's callers. */
  #keep(hash: string, hold: BoundHold): void {
    const listeners = this.#listeners.get(hash);
    if (listeners === undefined && !hold.holding && hold.unsettled === undefined) {
      this.#holds.delete(hash);
    } else {
      this.#holds.set(hash, hold);
    }
    for (const listener of listeners ?? []) listener();
  }
}

const stores = new WeakMap<QueryClient, BoundHolds>();

/** The holds through reads that live with `client`: one store per app, as there is one client,
 *  as the writes are (`writesOf`). */
function boundHoldsOf(client: QueryClient): BoundHolds {
  let store = stores.get(client);
  if (store === undefined) {
    store = new BoundHolds();
    stores.set(client, store);
  }
  return store;
}

/**
 * The latest hold through the read `readKey` names, the query key of the read its caller holds
 * through (`BoundHold`), and the hold that caller takes (`holdThrough`). Every caller of one read,
 * and every caller drawn again while it is out, reads that one hold (`BoundHolds`). `holdThrough`
 * takes a read as it begins, with the row whose press made it, forgets at once why the earlier
 * read of that key had not settled, and answers once the read settles or `readWithinBound`'s
 * bound ends, whichever is first. While it waits, the hold keeps the armed hand-off of the
 * reader's press that made the write live (`holdHandOff`), so a row or a form that read takes
 * away is still the press's doing (`causedByReader`). That press is `press`, the one the write
 * began under (`pressOfWrite`), which the caller reads as the write begins - a keyed write in its
 * `onMutate` - since this hold begins only as the write's answer comes, by when the reader may
 * have pressed again: a later press is no doing of this write's, and one that has ended keeps
 * nothing live.
 */
function useUnsettledRead(
  readKey: QueryKey
): readonly [
  latest: BoundHold | undefined,
  holdThrough: (
    row: string | undefined,
    read: Promise<unknown>,
    press: WritePress
  ) => Promise<void>,
] {
  const holds = boundHoldsOf(useQueryClient());
  const hash = hashKey(readKey);
  const subscribe = useCallback(
    (listener: () => void) => holds.subscribe(hash, listener),
    [hash, holds]
  );
  const latest = useSyncExternalStore(subscribe, () => holds.latest(hash));
  const holdThrough = useCallback(
    async (row: string | undefined, read: Promise<unknown>, press: WritePress) => {
      const answered = holdHandOff(press);
      await holds.wait(hash, row, read);
      answered();
    },
    [hash, holds]
  );
  return [latest, holdThrough];
}

/**
 * A control's hold through the re-read that draws what its write changed, `readKey`'s read,
 * bounded by `readWithinBound`. `holdThrough` answers once its read settles or the bound runs
 * out, whichever is first, so a write that returns it from `onSuccess` stays pending, and its
 * control held, for that long and no longer, with the press's hand-off kept live while it waits
 * (`useUnsettledRead`). `unsettled` is why the read of the latest hold has not settled, once that
 * hold ran out, and nothing of an earlier hold's. The page shows it beside the sentence it shows
 * for a read that failed. It goes once that read settles, and at once as a later hold of the same
 * read begins, and an earlier hold whose bound ends, or whose read settles, once a later hold has
 * begun changes nothing. Every caller of one read takes and reads that read's one hold, so a
 * control's hold and reason are those of every control drawn over the same read. This hook's are
 * the holds no row made: a row's hold of the same read (`useListReadBound`) forgets this one's
 * reason as it begins, and is said only on its row.
 */
export function useReadBound(
  readKey: QueryKey
): readonly [
  unsettled: string | undefined,
  holdThrough: (read: Promise<unknown>, press: WritePress) => Promise<void>,
] {
  const [latest, holdRowThrough] = useUnsettledRead(readKey);
  const holdThrough = useCallback(
    (read: Promise<unknown>, press: WritePress) => holdRowThrough(undefined, read, press),
    [holdRowThrough]
  );
  return [latest?.row === undefined ? latest?.unsettled : undefined, holdThrough];
}

/** A list's hold through its re-read, as each of its rows takes it (`useListReadBound`): it holds
 *  as `useReadBound`'s `holdThrough` does, with `row` the list's key for the row whose press made
 *  `read`. */
export type ListReadHold = (
  row: string,
  read: Promise<unknown>,
  press: WritePress
) => Promise<void>;

/**
 * One hold through a list's re-read, `readKey`'s read, for every row of the list
 * (`useReadBound`), for a list whose rows each press a write that reads the whole list again and
 * that says why that read has not settled on the row whose press made it: the expired requests of
 * the Inbox's Credential requests section (`CredentialRequestsSection`), and the documents of an
 * issue, each of whose Remove takes the hold of the issue's one document list, on its row of the
 * Artifacts tab and in a document page's header alike (`RemoveDocument`). `holdThrough` takes the
 * row whose press made the read with the read (`ListReadHold`). Each press begins a new read of
 * the same list, so it forgets why an earlier read had not settled, whichever row's press made
 * that one. `unsettledOf` answers that reason for the row whose press made the latest read, and
 * nothing for every other row.
 */
export function useListReadBound(
  readKey: QueryKey
): readonly [unsettledOf: (row: string) => string | undefined, holdThrough: ListReadHold] {
  const [latest, holdThrough] = useUnsettledRead(readKey);
  return [(row) => (latest?.row === row ? latest.unsettled : undefined), holdThrough];
}
