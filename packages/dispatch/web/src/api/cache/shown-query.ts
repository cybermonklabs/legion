import {
  type DefaultError,
  type DefinedUseQueryResult,
  hashKey,
  type InfiniteData,
  type QueryKey,
  QueryObserver,
  replaceEqualDeep,
  type UseInfiniteQueryResult,
  type UseQueryResult,
  useInfiniteQuery,
  useQueries,
  useQuery,
} from "@tanstack/react-query";
import { useRef, useSyncExternalStore } from "react";

import { type Cache, useCache } from "./cache";
import { clientOf } from "./client-handle";
import { libraryOptions, type PagesOptions, type ReadOptions } from "./read-options";
import type { Writes } from "./write-store";

/**
 * Every hook read of the page's caches, and the one observer outside React, in the cache layer:
 * the one place TanStack's query hooks and observers are called, as only the layer imports
 * TanStack (`biome.json`) and a feature's `Cache` offers neither (`cache.ts`).
 * The cache holds only Dispatch's answers; what a component reads is that data with every live
 * write's change over it, composed at the moment it reads, in the store's order
 * (`Writes.overlay`, R7 and R10 of dispatch://INFRA-299).
 *
 * The composition happens here, outside TanStack: each read hands TanStack its options without
 * any `select` - neither one of its own nor the read's - so TanStack's result is Dispatch's data
 * as the cache holds it, and a query's own rule for its answers (`structuralSharing`, such as
 * `issueQuery`'s, which keeps out an issue older than the one held) runs only where Dispatch's
 * answers enter the cache. It never sees a composed value, and never a value of the query a
 * reader read before its key changed. For an infinite query, `hasNextPage`, `hasPreviousPage`
 * and the page params TanStack reports are read off Dispatch's own pages for the same reason.
 * The read then composes the store's overlay for its key over that data, runs the read's own
 * `select` over what that leaves, and shares the result with the value it showed last for the
 * same key alone (`shownOf`).
 *
 * The overlay is read through `useSyncExternalStore`, and the store hands one function for as
 * long as the changes naming the key are the same ones, so a press composes and renders in the
 * task it ran in, and a read whose key no change names never renders again for a change made
 * elsewhere.
 */

type Overlay = (data: unknown) => unknown;
type Select = (data: unknown) => unknown;

/** What one read last showed, with what it was made from. */
interface Shown {
  readonly hash: string;
  readonly data: unknown;
  readonly overlay: Overlay;
  readonly select: Select | undefined;
  readonly value: unknown;
}

/**
 * What a read of the cache hashed `hash` shows: `data`, Dispatch's data, with `overlay`, the
 * store's composition for that cache, over it, then the read's own `select` over that. The value
 * keeps every part of `last` that is deeply equal to it (`replaceEqualDeep`), only where `last`
 * was shown for the same cache: a read moved to another key never shows a part of the value it
 * showed for the one before. `last` itself comes back while nothing it was made from changed.
 */
function shownOf(
  last: Shown | undefined,
  hash: string,
  data: unknown,
  overlay: Overlay,
  select: Select | undefined
): Shown {
  if (
    last !== undefined &&
    last.hash === hash &&
    Object.is(last.data, data) &&
    last.overlay === overlay &&
    last.select === select
  ) {
    return last;
  }
  const composed = overlay(data);
  const selected = composed === undefined || select === undefined ? composed : select(composed);
  const value =
    last !== undefined && last.hash === hash ? replaceEqualDeep(last.value, selected) : selected;
  return { data, hash, overlay, select, value };
}

/** The members of TanStack's result that fetch again and answer with a result of their own,
 *  whose `data` is Dispatch's as the cache holds it. */
const REFETCHES: ReadonlySet<PropertyKey> = new Set([
  "fetchNextPage",
  "fetchPreviousPage",
  "refetch",
]);

type Refetch = (...args: never[]) => Promise<{ readonly data: unknown }>;

/** What a read last rendered of the cache at `key`, for a refetch it handed out to answer by. */
interface Read {
  readonly key: QueryKey;
  readonly shown: Shown;
  readonly writes: Writes;
}

/** Each of TanStack's refetches a read has rendered, by TanStack's function, which is one for as
 *  long as its observer is: the read it last rendered for, and the refetch the read hands out in
 *  its place (`shownRefetch`). */
const refetches = new WeakMap<Refetch, { read: Read; readonly shown: Refetch }>();

/** The refetch a read rendering `read` hands out in place of TanStack's `refetch`: one function
 *  for as long as TanStack's is, so a reader naming it in an effect's dependencies runs nothing
 *  again, that answers with TanStack's result as the read shows it once it answers - Dispatch's
 *  data then, with the store's overlay for the key then over it and the read's `select` over
 *  that, sharing what it can with the value the read last rendered (`shownOf`). */
function shownRefetch(refetch: Refetch, read: Read): Refetch {
  const held = refetches.get(refetch);
  if (held !== undefined) {
    held.read = read;
    return held.shown;
  }
  const entry = {
    read,
    shown: ((...args) =>
      refetch(...args).then((answer) => {
        const { key, shown, writes } = entry.read;
        const now = shownOf(shown, shown.hash, answer.data, writes.overlay(key), shown.select);
        return withData(answer, now.value);
      })) as Refetch,
  };
  refetches.set(refetch, entry);
  return entry.shown;
}

/** TanStack's `result` with `data` in place of its own, and each of its refetches answering as
 *  the read shows it (`shownRefetch`): every other member is read through to `result` as it is
 *  read, so TanStack, which tracks what a reader reads, still renders it again only for that. */
function withData<R extends object>(result: R, data: unknown): R {
  return new Proxy(result, {
    get: (target, member) => {
      if (member === "data") return data;
      const value: unknown = Reflect.get(target, member);
      if (!REFETCHES.has(member) || typeof value !== "function") return value;
      return refetches.get(value as Refetch)?.shown ?? value;
    },
  });
}

/** `result` as a read rendering `read` shows it: `withData`, its refetches answering by `read`
 *  until the read renders again. */
function shownResult<R extends object>(result: R, read: Read): R {
  for (const member of REFETCHES) {
    const refetch: unknown = Reflect.get(result, member);
    if (typeof refetch === "function") shownRefetch(refetch as Refetch, read);
  }
  return withData(result, read.shown.value);
}

/** The store's overlay for the cache at `key`, read through `useSyncExternalStore`: the store
 *  hands one function while the changes naming the key are the same ones, so a change composed
 *  only on other caches renders nothing here. */
function useOverlay(writes: Writes, key: QueryKey): Overlay {
  return useSyncExternalStore(writes.subscribe, () => writes.overlay(key));
}

const NO_OVERLAYS: readonly Overlay[] = Object.freeze([]);

/** `useOverlay` for each of `keys`: one array while every overlay is the one it was. */
function useOverlays(writes: Writes, keys: readonly QueryKey[]): readonly Overlay[] {
  const held = useRef(NO_OVERLAYS);
  return useSyncExternalStore(writes.subscribe, () => {
    const overlays = keys.map((key) => writes.overlay(key));
    const last = held.current;
    if (overlays.length === last.length && overlays.every((one, at) => one === last[at])) {
      return last;
    }
    held.current = overlays;
    return overlays;
  });
}

/** What one read of the cache at `key` shows of `data` (`shownOf`), held from render to render
 *  so the read shares its value with the one it showed last. */
function useShownData(
  key: QueryKey,
  data: unknown,
  overlay: Overlay,
  select: Select | undefined
): Shown {
  const last = useRef<Shown | undefined>(undefined);
  last.current = shownOf(last.current, hashKey(key), data, overlay, select);
  return last.current;
}

/** The options every read below is handed, as far as the composition reads them. */
interface Composed {
  readonly queryKey: QueryKey;
  readonly select?: (data: never) => unknown;
}

function useComposedQuery({ select, ...options }: Composed) {
  const { writes } = useCache();
  const overlay = useOverlay(writes, options.queryKey);
  const result = useQuery(libraryOptions(options) as Parameters<typeof useQuery>[0]);
  const shown = useShownData(options.queryKey, result.data, overlay, select as Select | undefined);
  return shownResult(result, { key: options.queryKey, shown, writes });
}

function useComposedInfiniteQuery({ select, ...options }: Composed) {
  const { writes } = useCache();
  const overlay = useOverlay(writes, options.queryKey);
  const result = useInfiniteQuery(
    libraryOptions(options) as Parameters<typeof useInfiniteQuery>[0]
  );
  const shown = useShownData(options.queryKey, result.data, overlay, select as Select | undefined);
  return shownResult(result, { key: options.queryKey, shown, writes });
}

/** What a `useShownQueries` read last showed: each query's value by its key's hash, and what its
 *  `combine` made of them. */
interface ShownQueries {
  readonly byHash: ReadonlyMap<string, Shown>;
  readonly combined: unknown;
}

const NOTHING_SHOWN: ShownQueries = { byHash: new Map(), combined: undefined };

function useComposedQueries({
  combine,
  queries,
  ...options
}: {
  readonly combine?: (results: never) => unknown;
  readonly queries: readonly Composed[];
}) {
  const { writes } = useCache();
  const keys = queries.map((query) => query.queryKey);
  const overlays = useOverlays(writes, keys);
  const results = useQueries({
    ...options,
    queries: queries.map(({ select: _select, ...query }) => libraryOptions(query)),
  } as Parameters<typeof useQueries>[0]) as readonly { readonly data: unknown }[];
  const last = useRef<ShownQueries>(NOTHING_SHOWN);
  const byHash = new Map<string, Shown>();
  const shown = results.map((result, at) => {
    const query = queries[at] as Composed;
    const hash = hashKey(query.queryKey);
    const one = shownOf(
      last.current.byHash.get(hash),
      hash,
      result.data,
      overlays[at] as Overlay,
      query.select as Select | undefined
    );
    byHash.set(hash, one);
    return shownResult(result, { key: query.queryKey, shown: one, writes });
  });
  const combined =
    combine === undefined
      ? shown
      : replaceEqualDeep(last.current.combined, (combine as (results: unknown) => unknown)(shown));
  last.current = { byHash, combined };
  return combined;
}

/** A read of one cache, with every live change over Dispatch's data: TanStack's `useQuery` over
 *  options in the layer's terms (`ReadOptions`). One whose `initialData` is defined is never
 *  without data. */
export function useShownQuery<
  TQueryFnData = unknown,
  TError = DefaultError,
  TData = TQueryFnData,
  TQueryKey extends QueryKey = QueryKey,
>(
  options: ReadOptions<TQueryFnData, TError, TData, TQueryKey> & {
    readonly initialData: TQueryFnData | (() => TQueryFnData);
  }
): DefinedUseQueryResult<NoInfer<TData>, TError>;
export function useShownQuery<
  TQueryFnData = unknown,
  TError = DefaultError,
  TData = TQueryFnData,
  TQueryKey extends QueryKey = QueryKey,
>(
  options: ReadOptions<TQueryFnData, TError, TData, TQueryKey>
): UseQueryResult<NoInfer<TData>, TError>;
export function useShownQuery(options: Composed): unknown {
  return useComposedQuery(options);
}

/** A paged read of one cache, with every live change over Dispatch's pages: a page a fetch-more
 *  reads is built from Dispatch's own pages, and `hasNextPage` and the page params are read off
 *  them, so a change that takes a row out never moves the cursor. */
export function useShownInfiniteQuery<
  TQueryFnData,
  TError = DefaultError,
  TData = InfiniteData<TQueryFnData>,
  TQueryKey extends QueryKey = QueryKey,
  TPageParam = unknown,
>(
  options: PagesOptions<TQueryFnData, TError, TData, TQueryKey, TPageParam>
): UseInfiniteQueryResult<TData, TError> {
  return useComposedInfiniteQuery(options as Composed) as UseInfiniteQueryResult<TData, TError>;
}

/** Reads of several caches of one kind, with every live change over each one's data; a
 *  `combine` is handed the results as shown. */
export function useShownQueries<
  TQueryFnData = unknown,
  TError = DefaultError,
  TData = TQueryFnData,
  TQueryKey extends QueryKey = QueryKey,
  TCombined = UseQueryResult<TData, TError>[],
>(options: {
  readonly queries: readonly ReadOptions<TQueryFnData, TError, TData, TQueryKey>[];
  readonly combine?: (results: UseQueryResult<TData, TError>[]) => TCombined;
}): TCombined {
  return useComposedQueries(
    options as {
      readonly queries: readonly Composed[];
      readonly combine?: () => unknown;
    }
  ) as TCombined;
}

/**
 * A read of the cache at `options.queryKey` for a reader outside React (a document editor's
 * picture node view), as `useShownQuery` reads it inside: one `QueryObserver`, handed the options
 * without their `select`, which fetches as a hook read would and shares its query with every
 * reader on the page. `onData` is handed what the reader shows - Dispatch's data with every live
 * change over it, then `options.select` over that (`shownOf`) - now, and whenever that changes:
 * as the query's answer changes, and as a change naming the key starts or stops being composed.
 * Undefined while the query holds no data. The returned function stops.
 */
export function watchShownQuery<TData, TKey extends QueryKey>(
  cache: Cache,
  { select, ...options }: ReadOptions<TData, Error, TData, TKey>,
  onData: (data: TData | undefined) => void
): () => void {
  const { writes } = cache;
  const hash = hashKey(options.queryKey);
  const observer = new QueryObserver(
    clientOf(cache),
    libraryOptions(options) as ConstructorParameters<typeof QueryObserver>[1]
  );
  let last: Shown | undefined;
  const tell = () => {
    const shown = shownOf(
      last,
      hash,
      observer.getCurrentResult().data,
      writes.overlay(options.queryKey),
      select as Select | undefined
    );
    const changed = last === undefined || !Object.is(last.value, shown.value);
    last = shown;
    if (changed) onData(shown.value as TData | undefined);
  };
  const stopObserving = observer.subscribe(tell);
  const stopComposing = writes.subscribe(tell);
  tell();
  return () => {
    stopComposing();
    stopObserving();
  };
}
