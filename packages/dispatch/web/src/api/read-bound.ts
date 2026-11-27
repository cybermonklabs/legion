import {
  hashKey,
  onlineManager,
  type QueryClient,
  type QueryKey,
  useQueryClient,
} from "@tanstack/react-query";
import { useCallback, useSyncExternalStore } from "react";

import { holdHandOff } from "../components/focus-place";
import { NETWORK_ERROR_MESSAGE, REQUEST_DEADLINE_MS, requestTimeoutMessage } from "./client";

/**
 * Waits for `read`, the re-read that draws what a landed write changed, for `REQUEST_DEADLINE_MS`
 * at the most. The timer starts at this call, whatever state the query is in. TanStack pauses a
 * fetch the browser starts offline until the network returns, and no request deadline runs while
 * a fetch is paused, so a paused read alone does not settle for as long as the browser is
 * offline. A read of a query whose fetch the network had already paused is the one that does
 * settle at once, since TanStack answers its refetch there and then: a caller that waits on such
 * a read reads again through `query-client.ts`'s `waitingForPausedReads`, which has it answered
 * when the fetch is, and this bound ends the wait either way. Answers `undefined` once the read
 * settles, either way. At the bound it answers why the read has not settled, in the words of the
 * error a read that failed for that reason ends with: a `NetworkError`'s while the browser is
 * offline (`onlineManager`), and a `RequestTimeoutError`'s otherwise.
 */
export function readWithinBound(read: Promise<unknown>): Promise<string | undefined> {
  const { promise, resolve } = Promise.withResolvers<string | undefined>();
  const bound = setTimeout(
    () =>
      resolve(
        onlineManager.isOnline()
          ? requestTimeoutMessage(REQUEST_DEADLINE_MS)
          : NETWORK_ERROR_MESSAGE
      ),
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
 * The latest hold through one read, as every caller of that read sees it (`useUnsettledRead`):
 * whose press made it, whether it holds still, and why its read has not settled once the bound
 * ended it.
 */
export interface BoundHold {
  /** The row whose press made the hold, in its caller's own name for it: a list's key for the row
   *  (`useListReadBound`), a decision's form (`DecisionActions`), or, where one control speaks for
   *  each press in turn, what it said that press did (the issues table's notice,
   *  `useIssueTableActions`). `undefined` for a control's own hold (`useReadBound`). */
  readonly row: string | undefined;
  /** Whether the hold is on: from its start until its read settles or `readWithinBound`'s bound
   *  ends, whichever is first. */
  readonly holding: boolean;
  /** Why its read has not settled, once the bound ended the hold, until that read settles;
   *  `undefined` while the hold is on and once the read has settled. */
  readonly unsettled: string | undefined;
}

/** A hold as the store keeps it: what its callers read of it, and the read it waits on. */
interface KeptBoundHold extends BoundHold {
  readonly read: Promise<unknown>;
}

/**
 * The holds through reads that live with one query client (`boundHoldsOf`): the latest hold of
 * each read, by the hash of the read's query key. Every caller of a read draws this one hold, so
 * two callers of one read share one bound and one reason, and a caller drawn again while the read
 * is out - a card's view closed and opened, a tab or a view switched away and back - finds the
 * hold and its reason where the caller drawn before it left them. A hold that has ended - it holds
 * no more and its read has settled - is kept only while a caller of its read is drawn, which still
 * reads whose press made it (`DecisionActions`): with none drawn the store drops it, since the
 * next caller drawn reads it as no hold.
 */
class BoundHolds {
  readonly #holds = new Map<string, KeptBoundHold>();
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
  #keep(hash: string, hold: KeptBoundHold): void {
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
 * through (`BoundHold`), and the hold that caller takes (`waitWithinBound`). Every caller of one
 * read, and every caller drawn again while it is out, reads that one hold (`BoundHolds`).
 * `waitWithinBound` takes a read as it begins, with the row whose press made it, forgets at once
 * why the earlier read of that key had not settled, and answers once the read settles or
 * `readWithinBound`'s bound ends, whichever is first. Where the bound ends first and no later
 * read of the key has begun, the hold keeps why the read has not settled, which goes once that
 * read settles or a later read begins, whichever is first. An earlier read whose bound ends, or
 * which settles, once a later read has begun changes nothing. The page shows the hold's
 * `unsettled` beside the sentence it shows for a read that failed.
 */
export function useUnsettledRead(
  readKey: QueryKey
): readonly [
  latest: BoundHold | undefined,
  waitWithinBound: (row: string | undefined, read: Promise<unknown>) => Promise<void>,
] {
  const holds = boundHoldsOf(useQueryClient());
  const hash = hashKey(readKey);
  const subscribe = useCallback(
    (listener: () => void) => holds.subscribe(hash, listener),
    [hash, holds]
  );
  const latest = useSyncExternalStore(subscribe, () => holds.latest(hash));
  const waitWithinBound = useCallback(
    (row: string | undefined, read: Promise<unknown>) => holds.wait(hash, row, read),
    [hash, holds]
  );
  return [latest, waitWithinBound];
}

/** `waitWithinBound` of `row` and `read` (`useUnsettledRead`), with the armed hand-off of the
 *  reader's press that made the write kept live while it waits (`holdHandOff`). */
async function holdWithPress(
  waitWithinBound: (row: string | undefined, read: Promise<unknown>) => Promise<void>,
  row: string | undefined,
  read: Promise<unknown>
): Promise<void> {
  const answered = holdHandOff();
  await waitWithinBound(row, read);
  answered();
}

/**
 * A control's hold through the re-read that draws what its write changed, `readKey`'s read,
 * bounded by `readWithinBound`. `holdThrough` answers once its read settles or the bound runs
 * out, whichever is first, so a write that returns it from `onSuccess` stays pending, and its
 * control held, for that long and no longer. While it waits, the hold keeps the armed hand-off of
 * the reader's press that made the write live (`holdHandOff`), so a row or a form that read takes
 * away is still the press's doing (`causedByReader`). `unsettled` is why the read of the latest
 * hold has not settled, once that hold ran out: it goes once that read settles, and at once as a
 * later hold of the same read begins, and an earlier hold's read never puts its reason back.
 * `holding` is whether that hold is on: a control drawn again while it is, whose write is out of
 * its own hook's reach, holds by it until the hold ends. Every caller of one read takes and reads
 * one hold (`useUnsettledRead`), and this hook's are the holds no row made: a row's hold of the
 * same read (`useListReadBound`) forgets this one's reason as it begins, and is said only on its
 * row. A list whose rows each read the whole list again says that reason once, for the whole
 * list, with this hook (`RevocableList`, `AgentsPage`'s conversation), and on the row whose press
 * made the read with `useListReadBound`.
 */
export function useReadBound(
  readKey: QueryKey
): readonly [
  unsettled: string | undefined,
  holdThrough: (read: Promise<unknown>) => Promise<void>,
  holding: boolean,
] {
  const [latest, waitWithinBound] = useUnsettledRead(readKey);
  const holdThrough = useCallback(
    (read: Promise<unknown>) => holdWithPress(waitWithinBound, undefined, read),
    [waitWithinBound]
  );
  const own = latest?.row === undefined ? latest : undefined;
  return [own?.unsettled, holdThrough, own?.holding === true];
}

/** A list's hold through its re-read, as each of its rows takes it (`useListReadBound`): it holds
 *  as `useReadBound`'s `holdThrough` does, with `row` the list's key for the row whose press made
 *  `read`. */
export type ListReadHold = (row: string, read: Promise<unknown>) => Promise<void>;

/** A list's one hold through its re-read (`useListReadBound`): why that read has not settled, for
 *  the row whose press made the latest one (`unsettledOf`), the hold each row takes, and whether
 *  that hold is on, for that row (`holdingOf`). */
export type ListReadBound = readonly [
  unsettledOf: (row: string) => string | undefined,
  holdThrough: ListReadHold,
  holdingOf: (row: string) => boolean,
];

/**
 * One hold through a list's re-read, `readKey`'s read, for every row of the list
 * (`useReadBound`), for a list whose rows each press a write that reads the whole list again and
 * that says why that read has not settled on the row whose press made it: the expired requests of
 * the Inbox's Credential requests section (`CredentialRequestsSection`), the documents of an
 * issue, each of whose Remove takes the hold of the issue's one document list, on its row of the
 * Artifacts tab and in a document page's header alike (`RemoveDocument`), the Inbox's thread
 * cards, each through the Inbox's read (`ThreadRowCard`), the comments of an issue's
 * Conversation, whose every edit reads the comments again (`ConversationTab`), the approval chips
 * of the new issue page's header, whose every review reads the issue again
 * (`IssuePropertiesRow`), every other approval chip, through its own document's read
 * (`ApprovalChip`), and the priority line of each issue, through the priorities read
 * (`usePriorityLineWrite`). `holdThrough` takes the row whose press made the read with the
 * read (`ListReadHold`). Each press begins a new read of the same list, so it forgets why an
 * earlier read had not settled, whichever row's press made that one. `unsettledOf` answers that
 * reason for the row whose press made the latest read, and `holdingOf` whether its hold is on,
 * for that row: nothing for every other row.
 */
export function useListReadBound(readKey: QueryKey): ListReadBound {
  const [latest, waitWithinBound] = useUnsettledRead(readKey);
  const holdThrough = useCallback<ListReadHold>(
    (row, read) => holdWithPress(waitWithinBound, row, read),
    [waitWithinBound]
  );
  return [
    (row) => (latest?.row === row ? latest.unsettled : undefined),
    holdThrough,
    (row) => latest?.row === row && latest.holding,
  ];
}
