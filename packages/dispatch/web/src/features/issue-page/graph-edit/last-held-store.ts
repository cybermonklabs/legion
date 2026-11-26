import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useCallback, useId, useLayoutEffect, useSyncExternalStore } from "react";

/**
 * The versions that last held the gone references a page read (R9 of INFRA-5705's spec), shared
 * with every surface that draws one: a row's words (`RowText.tsx`) and the address fallback
 * (`address-target.ts`' `useLastHeldAddress`). The page's one batched names read answers each
 * gone reference with its `last_held` (`useIssueTree.ts`, `last-held-chips.ts`' `targetLastHeld`),
 * and publishes the map here beside the names it publishes (`refs/reference-names.tsx`), which
 * hold no version.
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

/** What one client's store holds: every value still published, by its publisher's key
 *  (`useId`), in the order the publishers last published; the one value readers are handed; and
 *  each reader to tell when that value changes. */
interface LastHeldState {
  readonly listeners: Set<() => void>;
  merged: LastHeld;
  readonly publications: Map<string, LastHeld>;
}

/** No gone reference: what a page that published none has read. */
const NO_LAST_HELD: LastHeld = new Map();

const states = new WeakMap<QueryClient, LastHeldState>();

function stateOf(client: QueryClient): LastHeldState {
  let state = states.get(client);
  if (state === undefined) {
    state = { listeners: new Set(), merged: NO_LAST_HELD, publications: new Map() };
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
  const subscribe = useCallback(
    (listener: () => void) => {
      state.listeners.add(listener);
      return () => {
        state.listeners.delete(listener);
      };
    },
    [state]
  );
  const read = useCallback(() => state.merged, [state]);
  return useSyncExternalStore(subscribe, read, read);
}
