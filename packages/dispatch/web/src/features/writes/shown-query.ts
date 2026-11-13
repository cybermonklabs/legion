import {
  type QueryClient,
  type QueryKey,
  QueryObserver,
  type QueryObserverOptions,
  useInfiniteQuery,
  useQueries,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { useMemo, useSyncExternalStore } from "react";

import { writesOf } from "./write-store";

/**
 * Every hook read of the page's caches, and the one observer outside React, through the one
 * module that may import TanStack's query hooks and observers (`no-bare-cache-reads.test.ts`).
 * The cache holds only Dispatch's answers; what a component reads is that data with every live
 * write's change over it, composed at the moment it reads, in the store's order
 * (`Writes.overlay`, R7 and R10 of dispatch://INFRA-299). Each read takes its options as
 * TanStack's hook would and adds the composition as its `select`, ahead of the read's own
 * `select` where it has one. The composition is one function for as long as the live changes
 * hold (`Writes.version`, read here so a change that starts or stops being composed renders the
 * read again), and TanStack runs `select` again only when the data or that function changes, so
 * an answer, a press, a refusal and a landing each compose once.
 */

/** The `select` a read of `queryKey` takes: the store's composition of the live changes over
 *  Dispatch's data, then the read's own `select`, if it names one. */
function useOverlay(
  queryKey: QueryKey,
  select: ((data: unknown) => unknown) | undefined,
  client: QueryClient | undefined
): (data: unknown) => unknown {
  const writes = writesOf(useQueryClient(client));
  useSyncExternalStore(writes.subscribe, writes.version);
  const overlay = writes.overlay(queryKey);
  return useMemo(
    () => (select === undefined ? overlay : (data: unknown) => select(overlay(data))),
    [overlay, select]
  );
}

/** The options every read below is handed, as far as the composition reads them. */
interface ReadOptions {
  readonly queryKey: QueryKey;
  readonly select?: (data: never) => unknown;
}

function useComposedQuery(options: ReadOptions, client?: QueryClient) {
  const select = useOverlay(
    options.queryKey,
    options.select as ((data: unknown) => unknown) | undefined,
    client
  );
  return useQuery({ ...options, select } as Parameters<typeof useQuery>[0], client);
}

function useComposedInfiniteQuery(options: ReadOptions, client?: QueryClient) {
  const select = useOverlay(
    options.queryKey,
    options.select as ((data: unknown) => unknown) | undefined,
    client
  );
  return useInfiniteQuery({ ...options, select } as Parameters<typeof useInfiniteQuery>[0], client);
}

function useComposedQueries(
  { queries, ...options }: { readonly queries: readonly ReadOptions[] },
  client?: QueryClient
) {
  const writes = writesOf(useQueryClient(client));
  useSyncExternalStore(writes.subscribe, writes.version);
  return useQueries(
    {
      ...options,
      queries: queries.map((query) => {
        const overlay = writes.overlay(query.queryKey);
        const select = query.select as ((data: unknown) => unknown) | undefined;
        return {
          ...query,
          select: select === undefined ? overlay : (data: unknown) => select(overlay(data)),
        };
      }),
    } as Parameters<typeof useQueries>[0],
    client
  );
}

/** `useQuery`, with every live change over Dispatch's data. */
export const useShownQuery = useComposedQuery as typeof useQuery;

/** `useInfiniteQuery`, with every live change over Dispatch's pages: a page a fetch-more reads is
 *  built from Dispatch's own pages, so a change that takes a row out never moves the cursor. */
export const useShownInfiniteQuery = useComposedInfiniteQuery as typeof useInfiniteQuery;

/** `useQueries`, with every live change over each query's data. */
export const useShownQueries = useComposedQueries as unknown as typeof useQueries;

/**
 * A read of the cache at `options.queryKey` for a reader outside React (a document editor's
 * picture node view), as `useShownQuery` reads it inside: one `QueryObserver`, which fetches as a
 * hook read would and shares its query with every reader on the page, handing `onData` the data
 * with every live change over it - now, whenever the query's answer changes, and whenever a
 * change starts or stops being composed (`Writes.version`). Undefined while the query holds no
 * data. The returned function stops.
 */
export function watchShownQuery<TData, TKey extends QueryKey>(
  client: QueryClient,
  options: QueryObserverOptions<TData, Error, TData, TData, TKey>,
  onData: (data: TData | undefined) => void
): () => void {
  const writes = writesOf(client);
  const composed = () => ({
    ...options,
    select: writes.overlay(options.queryKey) as (data: TData) => TData,
  });
  const observer = new QueryObserver(client, composed());
  const stopObserving = observer.subscribe((result) => onData(result.data));
  let version = writes.version();
  const stopComposing = writes.subscribe(() => {
    if (writes.version() === version) return;
    version = writes.version();
    observer.setOptions(composed());
  });
  onData(observer.getCurrentResult().data);
  return () => {
    stopComposing();
    stopObserving();
  };
}
