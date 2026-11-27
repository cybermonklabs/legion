import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useCallback, useId, useLayoutEffect, useSyncExternalStore } from "react";

/**
 * The versions that last held the gone references a page read (R9 of INFRA-5705's spec), shared
 * with every surface that draws one: a row's words (`RowText.tsx`) and the address fallback
 * (`address-target.ts`' `useLastHeldAddress`). The page's one batched names read answers each
 * gone reference with its `last_held` (`useIssueTree.ts`, `last-held-chips.ts`' `targetLastHeld`),
 * and publishes the map here beside the names it publishes (`refs/reference-names.tsx`), which
 * hold no version. Beside the map it publishes where the Spec view's own address opens instead,
 * by the issue's key (`usePublishLastHeldOpening`), which the view's slot opens
 * (`slots/view-spec.tsx`), so the slot reads it with no tree of its own beside the one its reads
 * build (`outline-reads.ts`' `useOutlineReads`).
 *
 * One store per query client, as `node-writes.ts` keeps its chains: the app has one client, so
 * every surface under it - the outline, a view, the margin - reads one store, and each test's
 * own client keeps its own, with no provider to mount. More than one surface can publish at
 * once; readers are handed every value still published, merged (`mergedOf`), so a surface that
 * unmounts takes only its own value with it.
 */

/** Each gone reference, with the pinned `dispatch://` address of the version that last held it
 *  where the read names one, and undefined where it names none. */
type LastHeld = ReadonlyMap<string, string | undefined>;

/** Where the Spec view's own address opens instead (`usePublishLastHeldOpening`): the issue the
 *  address is of, by its key, and the address on the page it opens. */
interface Opening {
  readonly address: string;
  readonly issue: string;
}

/** What one client's store holds: every value still published, by its publisher's key
 *  (`useId`), in the order the publishers last published; the one value readers are handed; each
 *  opening still published, by its publisher's key, in the same order; and each reader to tell
 *  when what it reads changes. */
interface LastHeldState {
  readonly listeners: Set<() => void>;
  merged: LastHeld;
  readonly openings: Map<string, Opening>;
  readonly publications: Map<string, LastHeld>;
}

/** No gone reference: what a page that published none has read. */
const NO_LAST_HELD: LastHeld = new Map();

const states = new WeakMap<QueryClient, LastHeldState>();

function stateOf(client: QueryClient): LastHeldState {
  let state = states.get(client);
  if (state === undefined) {
    state = {
      listeners: new Set(),
      merged: NO_LAST_HELD,
      openings: new Map(),
      publications: new Map(),
    };
    states.set(client, state);
  }
  return state;
}

/** Whether `one` and `other` hold the same references with the same versions. */
function sameEntries(one: LastHeld, other: LastHeld): boolean {
  if (one.size !== other.size) return false;
  for (const [reference, version] of one) {
    if (!other.has(reference) || other.get(reference) !== version) return false;
  }
  return true;
}

/** Every value still published as the one value readers see: none where no publisher names a
 *  reference; one publisher's own map where it alone names any, so no copy is made; and the
 *  union of them all where two do, the one that published last standing where two name one
 *  reference, since a publisher publishes again each time its read answers anew. `previous`
 *  where it holds the same, so a reader is handed a new map only where what it holds changed. */
function mergedOf(publications: ReadonlyMap<string, LastHeld>, previous: LastHeld): LastHeld {
  const filled = [...publications.values()].filter((map) => map.size > 0);
  const next =
    filled.length === 0
      ? NO_LAST_HELD
      : filled.length === 1
        ? filled[0]
        : new Map(filled.flatMap((map) => [...map]));
  return sameEntries(next, previous) ? previous : next;
}

/** Sets `publisher`'s value in `client`'s store, or takes it out (`undefined`), and tells each
 *  reader where what readers see changed. A value set again moves to the end of the order. */
function publish(client: QueryClient, publisher: string, lastHeld: LastHeld | undefined): void {
  const state = stateOf(client);
  state.publications.delete(publisher);
  if (lastHeld !== undefined) state.publications.set(publisher, lastHeld);
  const merged = mergedOf(state.publications, state.merged);
  if (merged === state.merged) return;
  state.merged = merged;
  for (const listener of [...state.listeners]) listener();
}

/** Sets `publisher`'s opening in `client`'s store, or takes it out (`undefined`), and tells each
 *  reader where it changed. A new opening moves its publisher to the end of the order. */
function publishOpening(
  client: QueryClient,
  publisher: string,
  opening: Opening | undefined
): void {
  const state = stateOf(client);
  const before = state.openings.get(publisher);
  if (before?.address === opening?.address && before?.issue === opening?.issue) return;
  state.openings.delete(publisher);
  if (opening !== undefined) state.openings.set(publisher, opening);
  for (const listener of [...state.listeners]) listener();
}

/** Where the Spec view's address of `issue` opens instead, as the publisher that published last
 *  names it, or undefined where no publisher names one. */
function openingOf(state: LastHeldState, issue: string): string | undefined {
  let address: string | undefined;
  for (const opening of state.openings.values()) {
    if (opening.issue === issue) address = opening.address;
  }
  return address;
}

/** Tells `listener` each time what `state` holds changes, until the returned function is called:
 *  what a reader of the store subscribes by (`useSyncExternalStore`). */
function useSubscription(state: LastHeldState): (listener: () => void) => () => void {
  return useCallback(
    (listener: () => void) => {
      state.listeners.add(listener);
      return () => {
        state.listeners.delete(listener);
      };
    },
    [state]
  );
}

/**
 * Publishes `lastHeld` under the page's query client for as long as the calling surface is
 * mounted, and withdraws it when the surface leaves, leaving every other publisher's value as it
 * was. `undefined` publishes nothing. It publishes in the layout phase of the commit that draws
 * what the page read, as the names are published (`refs/reference-names.tsx`'s
 * `usePublishReferenceNames`), so a row drawn in that commit draws the version before the page
 * is painted. It should be stable across renders (`useMemo`); one that holds what the last held
 * draws no reader again.
 */
export function usePublishLastHeldReferences(lastHeld: LastHeld | undefined): void {
  const client = useQueryClient();
  // The key this publisher's value is held under, the same for as long as it is mounted, so its
  // cleanup withdraws its own value and no other.
  const publisher = useId();
  useLayoutEffect(() => {
    publish(client, publisher, lastHeld);
  }, [client, lastHeld, publisher]);
  useLayoutEffect(() => () => publish(client, publisher, undefined), [client, publisher]);
}

/** Every reference the page read gone, by reference, with the pinned `dispatch://` address of
 *  the version that last held it where the read names one (`ReferenceTarget.last_held`), and
 *  undefined where it names none; empty where no surface under the page's query client publishes
 *  any (`usePublishLastHeldReferences`). None of them has a name: a row's words draw one with a
 *  version as the page's gone word linked to that version (`RowText.tsx`, `row-references.ts`),
 *  as the row's chip draws one (`NodeRow.tsx`'s `Chip`). */
export function useKnownLastHeldReferences(): LastHeld {
  const state = stateOf(useQueryClient());
  const subscribe = useSubscription(state);
  const read = useCallback(() => state.merged, [state]);
  return useSyncExternalStore(subscribe, read, read);
}

/**
 * Publishes where the Spec view's own address of the issue `issue` (its key) opens instead, under
 * the page's query client, for as long as the calling surface is mounted, and withdraws it when
 * the surface leaves or `address` is undefined, leaving every other publisher's as it was: on that
 * address naming, by its `#b-` fragment, a block the read the view draws from lacks, once the
 * names read under a set that holds the block's reference has answered, the version that last
 * held the block, or the same address without its fragment where none is named
 * (`last-held-ask.ts`' `lastHeldOpening`, R9 of Graph editing's spec). The page's names read
 * publishes it (`useIssueTree.ts`), in the layout phase, as it publishes the map above
 * (`usePublishLastHeldReferences`).
 */
export function usePublishLastHeldOpening(issue: string, address: string | undefined): void {
  const client = useQueryClient();
  // The key this publisher's opening is held under, the same for as long as it is mounted, so its
  // cleanup withdraws its own opening and no other.
  const publisher = useId();
  useLayoutEffect(() => {
    publishOpening(client, publisher, address === undefined ? undefined : { address, issue });
  }, [address, client, issue, publisher]);
  useLayoutEffect(() => () => publishOpening(client, publisher, undefined), [client, publisher]);
}

/** Where the Spec view's own address of the issue `issue` (its key) opens instead
 *  (`usePublishLastHeldOpening`); undefined for every other address, while the names read is
 *  out or answers the block live, and where no surface under the page's query client publishes
 *  one. The Spec view's slot opens it (`slots/view-spec.tsx`). */
export function useLastHeldOpening(issue: string): string | undefined {
  const state = stateOf(useQueryClient());
  const subscribe = useSubscription(state);
  const read = useCallback(() => openingOf(state, issue), [issue, state]);
  return useSyncExternalStore(subscribe, read, read);
}
