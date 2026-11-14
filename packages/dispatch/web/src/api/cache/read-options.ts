import type {
  DataTag,
  DefaultError,
  InfiniteData,
  QueryKey,
  QueryMeta,
  UseInfiniteQueryOptions,
  UseQueryOptions,
} from "@tanstack/react-query";

/** A `placeholderData` that shows the read's last answer while a changed key loads its first. */
export { keepPreviousData } from "@tanstack/react-query";

/**
 * A read's options in the cache layer's own terms: TanStack's, but for every option whose callback
 * TanStack hands a `Query` or the `QueryClient` (a fetch's context, `enabled`, `refetchInterval`,
 * `placeholderData`'s previous query, ...). A feature's callback is handed what it needs and no
 * door past the named reads: a fetch its key, its signal and its meta, and `enabled` and
 * `refetchInterval` Dispatch's answer the cache holds, the named Dispatch read
 * (`Cache.dispatched`). `libraryOptions` turns them into TanStack's where the layer hands them
 * on; a builder here (`queryOptions`, `infiniteQueryOptions`) only types them, as TanStack's do.
 */

/** The options of TanStack's whose callbacks are handed a `Query` or the client. */
type HandsOver =
  | "behavior"
  | "enabled"
  | "persister"
  | "placeholderData"
  | "queryFn"
  | "refetchInterval"
  | "refetchOnMount"
  | "refetchOnReconnect"
  | "refetchOnWindowFocus"
  | "staleTime"
  | "throwOnError";

/** What a read's fetch is handed. */
export interface ReadContext<TQueryKey extends QueryKey = QueryKey> {
  readonly queryKey: TQueryKey;
  readonly signal: AbortSignal;
  readonly meta: QueryMeta | undefined;
}

/** What a fetch of one page of a paged read is handed. */
export interface PageContext<TQueryKey extends QueryKey, TPageParam>
  extends ReadContext<TQueryKey> {
  readonly pageParam: TPageParam;
  readonly direction: "forward" | "backward";
}

/** An option decided once, or on Dispatch's answer the cache holds (`undefined` while none). */
type OnDispatched<TQueryData, R> = R | ((dispatched: NoInfer<TQueryData> | undefined) => R);

/** Where a read's last fetch left it: answered, refused, or not yet either. */
export type ReadStatus = "pending" | "error" | "success";

/** An option decided once, or on where the read's last fetch left it (`ReadStatus`). */
type OnStatus<R> = R | ((read: { readonly status: ReadStatus }) => R);

/** The options this layer gives its own terms (`HandsOver`). A read's data is inferred from its
 *  `queryFn` alone, as TanStack's is. */
interface OwnTerms<TQueryFnData, TQueryData, TContext> {
  readonly queryFn?: (context: TContext) => TQueryFnData | Promise<TQueryFnData>;
  readonly enabled?: OnDispatched<TQueryData, boolean>;
  readonly staleTime?: number | "static";
  readonly refetchInterval?: OnDispatched<TQueryData, number | false | undefined>;
  readonly refetchOnMount?: boolean | "always";
  readonly refetchOnReconnect?: boolean | "always";
  readonly refetchOnWindowFocus?: OnStatus<boolean | "always">;
  readonly placeholderData?:
    | NoInfer<TQueryData>
    | ((previous: NoInfer<TQueryData> | undefined) => NoInfer<TQueryData> | undefined);
  readonly throwOnError?: boolean;
}

/** A read of one cache. */
export type ReadOptions<
  TQueryFnData = unknown,
  TError = DefaultError,
  TData = TQueryFnData,
  TQueryKey extends QueryKey = QueryKey,
> = Omit<UseQueryOptions<TQueryFnData, TError, TData, TQueryKey>, HandsOver> &
  OwnTerms<TQueryFnData, TQueryFnData, ReadContext<TQueryKey>>;

/** A paged read of one cache. */
export type PagesOptions<
  TQueryFnData = unknown,
  TError = DefaultError,
  TData = InfiniteData<TQueryFnData>,
  TQueryKey extends QueryKey = QueryKey,
  TPageParam = unknown,
> = Omit<UseInfiniteQueryOptions<TQueryFnData, TError, TData, TQueryKey, TPageParam>, HandsOver> &
  OwnTerms<
    TQueryFnData,
    InfiniteData<TQueryFnData, TPageParam>,
    PageContext<TQueryKey, TPageParam>
  >;

/** `options`, its key tagged with the data it reads, so a named read of the key is typed by it
 *  (`Cache.dispatched`), as TanStack's `queryOptions` tags it. */
export function queryOptions<
  TQueryFnData = unknown,
  TError = DefaultError,
  TData = TQueryFnData,
  TQueryKey extends QueryKey = QueryKey,
>(
  options: ReadOptions<TQueryFnData, TError, TData, TQueryKey>
): ReadOptions<TQueryFnData, TError, TData, TQueryKey> & {
  queryKey: DataTag<TQueryKey, TQueryFnData, TError>;
} {
  return options as ReadOptions<TQueryFnData, TError, TData, TQueryKey> & {
    queryKey: DataTag<TQueryKey, TQueryFnData, TError>;
  };
}

/** `queryOptions` of a paged read. */
export function infiniteQueryOptions<
  TQueryFnData,
  TError = DefaultError,
  TData = InfiniteData<TQueryFnData>,
  TQueryKey extends QueryKey = QueryKey,
  TPageParam = unknown,
>(
  options: PagesOptions<TQueryFnData, TError, TData, TQueryKey, TPageParam>
): PagesOptions<TQueryFnData, TError, TData, TQueryKey, TPageParam> & {
  queryKey: DataTag<TQueryKey, InfiniteData<TQueryFnData>, TError>;
} {
  return options as PagesOptions<TQueryFnData, TError, TData, TQueryKey, TPageParam> & {
    queryKey: DataTag<TQueryKey, InfiniteData<TQueryFnData>, TError>;
  };
}

/** The shape every option below is read as: `ReadOptions` or `PagesOptions` of any data. */
interface AnyOwnTerms {
  readonly enabled?: OnDispatched<never, boolean>;
  readonly refetchInterval?: OnDispatched<never, number | false | undefined>;
  readonly refetchOnWindowFocus?: OnStatus<boolean | "always">;
}

/** What TanStack's option callbacks are handed: a query, of which the layer hands on its data
 *  or its status. */
interface HandedQuery {
  readonly state: { readonly data: unknown; readonly status: ReadStatus };
}

/**
 * `options` in TanStack's terms, for the layer to hand on (a hook, an observer, a fetch): each of
 * `enabled` and `refetchInterval` given as a callback is handed the query's data where TanStack
 * hands it the query, and `refetchOnWindowFocus` its status. Every other option is TanStack's
 * own, typed narrower here.
 */
export function libraryOptions<T extends object>(options: T): T {
  const { enabled, refetchInterval, refetchOnWindowFocus } = options as AnyOwnTerms;
  if (
    typeof enabled !== "function" &&
    typeof refetchInterval !== "function" &&
    typeof refetchOnWindowFocus !== "function"
  ) {
    return options;
  }
  return {
    ...options,
    ...(typeof enabled === "function"
      ? { enabled: (query: HandedQuery) => enabled(query.state.data as never) }
      : {}),
    ...(typeof refetchInterval === "function"
      ? {
          refetchInterval: (query: HandedQuery) => refetchInterval(query.state.data as never),
        }
      : {}),
    ...(typeof refetchOnWindowFocus === "function"
      ? {
          refetchOnWindowFocus: (query: HandedQuery) =>
            refetchOnWindowFocus({ status: query.state.status }),
        }
      : {}),
  };
}
