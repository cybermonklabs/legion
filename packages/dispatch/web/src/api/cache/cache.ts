import type {
  DefaultError,
  InferDataFromTag,
  MutationFilters as LibraryMutationFilters,
  Mutation,
  MutationKey,
  MutationStatus,
  Query,
  QueryClient,
  QueryFilters,
  QueryFunctionContext,
  QueryKey,
} from "@tanstack/react-query";
import { useIsMutating, useMutationState, useQueryClient } from "@tanstack/react-query";
import { useRef, useSyncExternalStore } from "react";

import { type Answered, answeredBody } from "./answered";
import { bindClient } from "./client-handle";
import { isLiveQuery } from "./query-refresh";
import { libraryOptions, type ReadContext, type ReadOptions } from "./read-options";
import { type Write, Writes } from "./write-store";

/**
 * The page's caches, as every module outside the cache layer (`api/cache/`) holds them: the one
 * door past which only the layer imports TanStack (`biome.json`'s `noRestrictedImports`), so no
 * feature can name a `QueryClient`, a `QueryCache` or a `Query`, and none reads a cache's data
 * but through the two named reads. The cache holds only Dispatch's answers; what the person sees
 * is that data with every live write's change composed over it as it is read (`Writes.overlay`,
 * R7 and R10 of dispatch://INFRA-299):
 *
 * - the shown read: `shown`, and the hooks of `shown-query.ts` and its `watchShownQuery`;
 * - the Dispatch read: `dispatched` and `fetchDispatched`, and Dispatch's answer the layer hands an
 *   option's callback (`ReadOptions`' `enabled` and `refetchInterval`).
 *
 * Everything else here moves a cache without reading its data - invalidate, cancel, prefetch,
 * remove, reset, whether a read is held or fetching - or writes Dispatch's answer into one
 * (`setDispatched`, the one way a feature writes a cache, which takes nothing but an `Answered`
 * value), or counts the mutations a write holds open. One `Cache` lives beside each TanStack
 * client (`cacheOf`); a component reaches the page's through `useCache`, and code outside React
 * is handed it.
 */
export interface Cache {
  /** Every write the page makes (`write-store.ts`). */
  readonly writes: Writes;

  /** The cache at `key` as the person sees it: Dispatch's data with every live change over it
   *  (`Writes.overlay`), for a read made outside React that must see what the person sees - the
   *  board's neighbours and drop indices, the pin's toggle, the rows a mark-all or a dismiss
   *  takes, the count an unread press drops from. A read that wants Dispatch's answer alone is
   *  `dispatched`; every read of a cache's data is one of the two. */
  shown<T>(key: QueryKey): T | undefined;

  /** The cache at `key` as Dispatch answered it, with no live change over it, for a reader that
   *  wants exactly that: who Dispatch says is signed in, the event feed's cursor, the id a
   *  reference warms a document's text by, and a read a change makes inside its own composition,
   *  which must carry no other write's change: each change is composed over the changes pressed
   *  before it, never over a value one of them made. Typed by the key as TanStack's
   *  `getQueryData` is. */
  dispatched<
    TQueryFnData = unknown,
    TTaggedQueryKey extends QueryKey = QueryKey,
    TInferredQueryFnData = InferDataFromTag<TQueryFnData, TTaggedQueryKey>,
  >(key: TTaggedQueryKey): TInferredQueryFnData | undefined;

  /** `dispatched`, fetched first where the cache holds no answer or one stale by `options`'
   *  `staleTime`: TanStack's `fetchQuery`, which rejects with the fetch's error. */
  fetchDispatched<
    TQueryFnData,
    TError = DefaultError,
    TData = TQueryFnData,
    TQueryKey extends QueryKey = QueryKey,
  >(options: ReadOptions<TQueryFnData, TError, TData, TQueryKey>): Promise<TData>;

  /** Writes into the cache at `key` what Dispatch answered: `answer`'s body itself, the cache's
   *  whole data, typed by the key as TanStack's `setQueryData` is - a live snapshot, a mark's
   *  own state. Every write a module outside this layer makes into a cache is this one, and the
   *  live changes are composed over it as over any answer. `answer` is `Answered`, which only
   *  Dispatch's answer to a write and a decoded live frame are (`answered.ts`), so nothing the
   *  screen built can be written: any other value is refused by type, and the write throws. */
  setDispatched<TTaggedQueryKey extends QueryKey>(
    key: TTaggedQueryKey,
    answer: Answered<InferDataFromTag<unknown, TTaggedQueryKey>>
  ): void;
  /** Writes into the cache at `key` what `merge` makes of Dispatch's `answer` and the data the
   *  cache holds (`dispatched`: Dispatch's own, with no live change over it, never the shown
   *  value): a mutation's answer folded into the list or the record it belongs to, a live delta,
   *  an event the log prepends. `undefined` writes nothing. `T` is the key's tagged data, or what
   *  `merge` names. */
  setDispatched<
    TTaggedQueryKey extends QueryKey,
    A,
    T = InferDataFromTag<unknown, TTaggedQueryKey>,
  >(
    key: TTaggedQueryKey,
    answer: Answered<A>,
    merge: (dispatched: T | undefined, answer: A) => T | undefined
  ): void;

  /** Whether the cache holds a read under `key` at all, answered or not. */
  holds(key: QueryKey): boolean;

  /** Whether a fetch of the read under `key` is out now. */
  fetching(key: QueryKey): boolean;

  /** Marks every read `filters` names stale, and fetches again each one something renders (or
   *  as `refetchType` says). A read already fetching is fetched once more when that fetch
   *  settles (`DispatchQueryClient`). */
  invalidate(filters?: InvalidateFilters): Promise<void>;

  /** Stops every fetch out for a read `filters` names, so its answer, asked before a write, cannot
   *  land over that write's own. */
  cancel(filters?: CacheFilters): Promise<void>;

  /** Fetches `options`' read into its cache where it holds no fresh answer, for a surface about
   *  to show it. */
  prefetch<
    TQueryFnData,
    TError = DefaultError,
    TData = TQueryFnData,
    TQueryKey extends QueryKey = QueryKey,
  >(options: ReadOptions<TQueryFnData, TError, TData, TQueryKey>): Promise<void>;

  /** Drops every read `filters` names from the cache. */
  remove(filters?: CacheFilters): void;

  /** Puts every read `filters` names back as it was before its first answer, and fetches again
   *  each one something renders. */
  reset(filters?: CacheFilters): Promise<void>;

  /** How many mutations `filters` names are out now. */
  pendingMutations(filters?: MutationFilters): number;

  /** Where each mutation `filters` names stands, in the order they were made. */
  mutations<TVariables = unknown>(filters: MutationFilters): readonly MutationState<TVariables>[];

  /** Drops every mutation `filters` names that is no longer out. */
  forgetSettledMutations(filters: MutationFilters): void;

  /** Counts `answer` as a mutation named `mutationKey` until it settles, whether or not the
   *  browser says it is online, so a hold on the name (`useSending`) counts a send from the task
   *  that starts it on, whether or not the surface that pressed it is still mounted. */
  holdMutation(mutationKey: MutationKey, answer: () => Promise<unknown>): void;
}

/** Which reads a call names: every read under `queryKey` (by prefix unless `exact`), narrowed to
 *  those something renders or nothing does (`type`), to the stale ones (`stale`), to those the
 *  live socket owns or the rest (`live`), and to those whose key `predicate` accepts. */
export interface CacheFilters {
  readonly queryKey?: QueryKey;
  readonly exact?: boolean;
  readonly type?: "active" | "inactive" | "all";
  readonly stale?: boolean;
  readonly live?: boolean;
  readonly predicate?: (queryKey: QueryKey) => boolean;
}

/** `CacheFilters`, and which of the reads named are fetched again (`refetchType`, by default the
 *  ones something renders). */
export interface InvalidateFilters extends CacheFilters {
  readonly refetchType?: "active" | "inactive" | "all" | "none";
}

/** Which mutations a call names: every one under `mutationKey` (by prefix unless `exact`),
 *  narrowed to those in `status` and to those whose key `predicate` accepts. */
export interface MutationFilters {
  readonly mutationKey?: MutationKey;
  readonly exact?: boolean;
  readonly status?: MutationStatus;
  readonly predicate?: (mutationKey: MutationKey | undefined) => boolean;
}

/** Where one mutation stands: its status, the variables it was made with and its refusal. */
export interface MutationState<TVariables = unknown> {
  readonly status: MutationStatus;
  readonly variables: TVariables | undefined;
  readonly error: Error | null;
}

/** `filters` in TanStack's terms. */
function queryFilters({ live, predicate, ...filters }: CacheFilters = {}): QueryFilters {
  if (live === undefined && predicate === undefined) return filters;
  return {
    ...filters,
    predicate: (query: Query) =>
      (live === undefined || isLiveQuery(query) === live) &&
      (predicate === undefined || predicate(query.queryKey)),
  };
}

/** `filters` in TanStack's terms. */
function mutationFilters({ predicate, ...filters }: MutationFilters = {}): LibraryMutationFilters {
  if (predicate === undefined) return filters;
  return {
    ...filters,
    predicate: (mutation: Mutation) => predicate(mutation.options.mutationKey),
  };
}

/** Where `mutation` stands. */
function mutationStateOf<TVariables>(mutation: Mutation): MutationState<TVariables> {
  const { error, status, variables } = mutation.state;
  return {
    error: error ?? null,
    status,
    variables: variables as TVariables | undefined,
  };
}

/** The `Cache` over one TanStack client. */
class ClientCache implements Cache {
  readonly #client: QueryClient;
  #writes: Writes | undefined;

  constructor(client: QueryClient) {
    this.#client = client;
  }

  get writes(): Writes {
    this.#writes ??= new Writes(this);
    return this.#writes;
  }

  shown<T>(key: QueryKey): T | undefined {
    return this.writes.overlay(key)(this.#client.getQueryData(key)) as T | undefined;
  }

  dispatched<
    TQueryFnData = unknown,
    TTaggedQueryKey extends QueryKey = QueryKey,
    TInferredQueryFnData = InferDataFromTag<TQueryFnData, TTaggedQueryKey>,
  >(key: TTaggedQueryKey): TInferredQueryFnData | undefined {
    return this.#client.getQueryData<TQueryFnData, TTaggedQueryKey, TInferredQueryFnData>(key);
  }

  fetchDispatched<
    TQueryFnData,
    TError = DefaultError,
    TData = TQueryFnData,
    TQueryKey extends QueryKey = QueryKey,
  >(options: ReadOptions<TQueryFnData, TError, TData, TQueryKey>): Promise<TData> {
    return this.#client.fetchQuery(libraryOptions(options) as never);
  }

  setDispatched(
    key: QueryKey,
    answer: Answered<unknown>,
    merge?: (dispatched: unknown, answer: unknown) => unknown
  ): void {
    const body = answeredBody(answer);
    this.#client.setQueryData(
      key,
      merge === undefined ? body : (dispatched: unknown) => merge(dispatched, body)
    );
  }

  holds(key: QueryKey): boolean {
    return this.#client.getQueryState(key) !== undefined;
  }

  fetching(key: QueryKey): boolean {
    return this.#client.getQueryState(key)?.fetchStatus === "fetching";
  }

  invalidate({ refetchType, ...filters }: InvalidateFilters = {}): Promise<void> {
    return this.#client.invalidateQueries({
      ...queryFilters(filters),
      refetchType,
    });
  }

  cancel(filters?: CacheFilters): Promise<void> {
    return this.#client.cancelQueries(queryFilters(filters));
  }

  prefetch<
    TQueryFnData,
    TError = DefaultError,
    TData = TQueryFnData,
    TQueryKey extends QueryKey = QueryKey,
  >(options: ReadOptions<TQueryFnData, TError, TData, TQueryKey>): Promise<void> {
    return this.#client.prefetchQuery(libraryOptions(options) as never);
  }

  remove(filters?: CacheFilters): void {
    this.#client.removeQueries(queryFilters(filters));
  }

  reset(filters?: CacheFilters): Promise<void> {
    return this.#client.resetQueries(queryFilters(filters));
  }

  pendingMutations(filters?: MutationFilters): number {
    return this.#client.isMutating(mutationFilters(filters));
  }

  mutations<TVariables = unknown>(filters: MutationFilters): readonly MutationState<TVariables>[] {
    return this.#client
      .getMutationCache()
      .findAll(mutationFilters(filters))
      .map((mutation) => mutationStateOf<TVariables>(mutation));
  }

  forgetSettledMutations(filters: MutationFilters): void {
    const mutations = this.#client.getMutationCache();
    for (const mutation of mutations.findAll(mutationFilters(filters))) {
      if (mutation.state.status !== "pending") mutations.remove(mutation);
    }
  }

  holdMutation(mutationKey: MutationKey, answer: () => Promise<unknown>): void {
    // `networkMode: "always"`: a mutation TanStack paused offline would hold nothing.
    this.#client
      .getMutationCache()
      .build(this.#client, {
        mutationFn: answer,
        mutationKey,
        networkMode: "always",
      })
      .execute(undefined)
      .catch(() => {});
  }
}

const caches = new WeakMap<QueryClient, Cache>();

/** The one `Cache` over `client`, made on first use: for the layer's own modules, the app root
 *  (`provider.tsx`) and the tests' rigs (`test-client.ts`). Lint refuses it to every other
 *  module (`biome.json`), which holds a `Cache` and never a client. */
export function cacheOf(client: QueryClient): Cache {
  let cache = caches.get(client);
  if (cache === undefined) {
    cache = new ClientCache(client);
    bindClient(cache, client);
    caches.set(client, cache);
  }
  return cache;
}

/** The page's `Cache`, the one over the client the app root provides (`CacheProvider`). */
export function useCache(): Cache {
  return cacheOf(useQueryClient());
}

/** The cache a read's fetch is filling, for a fetch that writes beside its answer (the Inbox's
 *  fetch marks, `ask-thread-freshness.ts`) or subscribes for it (a live view, `live-socket.ts`).
 *  TanStack hands every fetch its client; the context a fetch is typed to see (`ReadContext`)
 *  leaves it out, so a fetch is handed the `Cache` over it here and never the client. */
export function cacheFetching(context: ReadContext): Cache {
  return cacheOf((context as unknown as QueryFunctionContext).client);
}

/** How many mutations `filters` names are out, rendering again as that changes. */
export function usePendingMutations(filters?: MutationFilters): number {
  return useIsMutating(mutationFilters(filters));
}

/** Where each mutation `filters` names stands, in the order they were made, rendering again as
 *  any of them moves. */
export function useMutations<TVariables = unknown>(
  filters: MutationFilters
): readonly MutationState<TVariables>[] {
  return useMutationState({
    filters: mutationFilters(filters),
    select: (mutation) => mutationStateOf<TVariables>(mutation),
  });
}

/** The page's writes (`Cache.writes`). */
export function useWrites(): Writes {
  return useCache().writes;
}

/**
 * What `select` picks out of every write the store holds, re-rendering only when that changes:
 * the pick is made again only when a write changes or `key` - what the pick is of, by name - does,
 * and handed back as the same value while it holds the same items (an array) or is the same value.
 */
export function useWriteSelection<R>(key: string, select: (rows: readonly Write[]) => R): R {
  const store = useWrites();
  const last = useRef<{
    readonly key: string;
    readonly rows: readonly Write[];
    readonly picked: R;
  }>(undefined);
  const pick = useRef(select);
  pick.current = select;
  return useSyncExternalStore(store.subscribe, () => {
    const rows = store.rows();
    const before = last.current;
    if (before?.rows === rows && before.key === key) return before.picked;
    const picked = pick.current(rows);
    const same =
      before !== undefined &&
      (Object.is(before.picked, picked) ||
        (Array.isArray(before.picked) &&
          Array.isArray(picked) &&
          before.picked.length === picked.length &&
          before.picked.every((item, index) => Object.is(item, picked[index]))));
    const kept = same ? before.picked : picked;
    last.current = { key, picked: kept, rows };
    return kept;
  });
}
