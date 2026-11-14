import {
  type DefaultError,
  QueryObserver as LibraryQueryObserver,
  type QueryClient,
  type QueryKey,
  type UseQueryResult,
  useQuery as useLibraryQuery,
} from "@tanstack/react-query";

import { libraryOptions, type ReadOptions } from "./read-options";

/**
 * What a test's rig needs of TanStack, through the cache layer: a real client to mount a page on
 * and to play Dispatch's part with - seeding an answer, reading what a cache holds raw, observing
 * a read's fetches - and the managers that stand for the browser's focus and connection. Only the
 * layer imports TanStack (`biome.json`), and lint refuses this module to every shipped module, so
 * a feature still holds a `Cache` and never a client. `cacheOf` hands a rig's client to code that
 * is handed the page's `Cache` (a write store, a kind's callback, the live socket).
 */
export type {
  InfiniteData,
  MutationKey,
  Query,
  QueryKey,
  QueryObserverOptions,
} from "@tanstack/react-query";
export {
  environmentManager,
  focusManager,
  InfiniteQueryObserver,
  notifyManager,
  onlineManager,
  QueryClient,
  QueryClientProvider,
} from "@tanstack/react-query";
export { cacheOf } from "./cache";

/** TanStack's `QueryObserver` over a read's options as a feature builds them (`ReadOptions`):
 *  a rig's stand-in for a surface that renders the read, which sees Dispatch's data raw. */
export class QueryObserver<
  TQueryFnData = unknown,
  TError = DefaultError,
  TData = TQueryFnData,
  TQueryKey extends QueryKey = QueryKey,
> extends LibraryQueryObserver<TQueryFnData, TError, TData, TQueryFnData, TQueryKey> {
  constructor(client: QueryClient, options: ReadOptions<TQueryFnData, TError, TData, TQueryKey>) {
    super(client, libraryOptions(options) as never);
  }
}

/** TanStack's `useQuery` over a read's options as a feature builds them: a rig's raw read of
 *  what a cache holds, with no live change over it. */
export function useQuery<
  TQueryFnData = unknown,
  TError = DefaultError,
  TData = TQueryFnData,
  TQueryKey extends QueryKey = QueryKey,
>(options: ReadOptions<TQueryFnData, TError, TData, TQueryKey>): UseQueryResult<TData, TError> {
  return useLibraryQuery(libraryOptions(options) as never);
}
