import {
  type DefaultError,
  type DefaultedQueryObserverOptions,
  notifyManager,
  type Query,
  QueryClient,
  type QueryFunction,
  type QueryKey,
  type QueryObserverOptions,
  type RefetchOptions,
  type RefetchQueryFilters,
  type Updater,
} from "@tanstack/react-query";

import { ApiError, isEventHeadUnavailable, isRetryableQueryError } from "./client";

/** The wait before a failed read's retry when the server asked for none. */
export const QUERY_RETRY_DELAY_MS = 500;

/** The longest `Retry-After` a retry honours. With its jitter of up to as much again, a read
 *  refused by a proxy that answers a day (or more than `setTimeout` can hold) still retries within
 *  a minute rather than spinning with no error and no Retry for as long as it says. */
const RETRY_AFTER_CEILING_MS = 30_000;

/** The longest wait between two asks of a read that answers `EVENT_HEAD_UNAVAILABLE`: 500 ms,
 *  doubling, up to this. */
export const EVENT_HEAD_RETRY_CEILING_MS = 5_000;

/**
 * Whether a failed query is asked again (`retry` in `main.tsx`). An auth outcome (401) is
 * definitive; everything else retryable (a dropped connection, a 5xx) gets two quick retries
 * before the page shows an error. The sign-in check's `503 EVENT_HEAD_UNAVAILABLE` is asked again
 * for as long as it lasts: Dispatch answers it while an event write open since before its restart
 * keeps it from vouching for any event id, which can outlast two retries, and the page opens on
 * its own once it ends.
 */
export function queryRetry(failureCount: number, error: unknown): boolean {
  return isEventHeadUnavailable(error) || (failureCount < 2 && isRetryableQueryError(error));
}

/**
 * The wait before every query's retry (`retryDelay` in `main.tsx`). A server refusing excess load
 * (503 `OVERLOADED`, `DATABASE_BUSY` or `REQUEST_TIMEOUT`) says how long to stay away in
 * `Retry-After`: the retry waits that long, at most `RETRY_AFTER_CEILING_MS`, plus a random share
 * of it again, so the tabs it refused together do not all come back in the same instant, and the
 * whole wait stays under a minute. `EVENT_HEAD_UNAVAILABLE` waits 500 ms doubling to
 * `EVENT_HEAD_RETRY_CEILING_MS`. Any other failure waits 500 ms. `random` is a test seam;
 * TanStack calls this with the first two arguments.
 */
export function queryRetryDelay(
  failureCount: number,
  error: unknown,
  random: () => number = Math.random
): number {
  if (isEventHeadUnavailable(error)) {
    return Math.min(QUERY_RETRY_DELAY_MS * 2 ** failureCount, EVENT_HEAD_RETRY_CEILING_MS);
  }
  if (error instanceof ApiError && error.retryAfterMs !== undefined) {
    const retryAfterMs = Math.min(error.retryAfterMs, RETRY_AFTER_CEILING_MS);
    return retryAfterMs + Math.floor(random() * retryAfterMs);
  }
  return QUERY_RETRY_DELAY_MS;
}

/**
 * Dispatch's query client: TanStack's, except that no read ever has two requests in flight.
 * `refetchQueries` - and through it `invalidateQueries` and `resetQueries`, which TanStack routes
 * through this method - never cancels a running request. A query that is already fetching is
 * fetched once more when that fetch settles, so an invalidation a mutation makes mid-fetch still
 * ends on the answer composed after the write, and the first request is not left running on the
 * server with its answer thrown away. Calls for one query while it waits share that one follow-up
 * fetch. Every mutation's own `invalidateQueries` call keeps its shape. It also tells whoever
 * listens each time a query calls its function (`onAttempt`).
 */
export class DispatchQueryClient extends QueryClient {
  readonly #attempts = new Set<(queryHash: string) => void>();
  readonly #followUps = new Map<string, Promise<void>>();
  /** Every function `defaultQueryOptions` made, so that a wrapped function is never wrapped
   *  again. */
  readonly #told = new WeakSet<object>();

  /** Tells `listener` the hash of a query each time the query calls its function from now on:
   *  once for each attempt of a read - its first and each retry, which TanStack makes within one
   *  fetch - as the attempt starts, and once for each page an infinite query reads. */
  readonly onAttempt = (listener: (queryHash: string) => void): (() => void) => {
    this.#attempts.add(listener);
    return () => this.#attempts.delete(listener);
  };

  /** TanStack's defaults, with the query's function wrapped so that each call of it tells
   *  `onAttempt`'s listeners first; the wrapper hands the function its context and returns what
   *  the function returns, and a listener that throws fails no read. `skipToken`, an absent
   *  function and a wrapped one are left as they are. Each observer's render passes a function
   *  of its own, so a wrapper is made for each call rather than kept. */
  override defaultQueryOptions<
    TQueryFnData = unknown,
    TError = DefaultError,
    TData = TQueryFnData,
    TQueryData = TQueryFnData,
    TQueryKey extends QueryKey = QueryKey,
    TPageParam = never,
  >(
    options:
      | QueryObserverOptions<TQueryFnData, TError, TData, TQueryData, TQueryKey, TPageParam>
      | DefaultedQueryObserverOptions<TQueryFnData, TError, TData, TQueryData, TQueryKey>
  ): DefaultedQueryObserverOptions<TQueryFnData, TError, TData, TQueryData, TQueryKey> {
    const defaulted = super.defaultQueryOptions(options);
    const { queryFn, queryHash } = defaulted;
    if (typeof queryFn !== "function" || this.#told.has(queryFn)) return defaulted;
    const telling: QueryFunction<TQueryFnData, TQueryKey> = (context) => {
      for (const listener of this.#attempts) {
        try {
          listener(queryHash);
        } catch (error) {
          queueMicrotask(() => {
            throw error;
          });
        }
      }
      return queryFn(context);
    };
    this.#told.add(telling);
    // A fresh object is the parent's own copy, which no caller holds yet.
    if (defaulted === options) return { ...defaulted, queryFn: telling };
    defaulted.queryFn = telling;
    return defaulted;
  }

  override refetchQueries<TTaggedQueryKey extends QueryKey = QueryKey>(
    filters?: RefetchQueryFilters<TTaggedQueryKey>,
    options: RefetchOptions = {}
  ): Promise<void> {
    const promises = notifyManager.batch(() =>
      this.getQueryCache()
        .findAll(filters)
        .filter((query) => !query.isDisabled() && !query.isStatic())
        .map((query) => this.#refetch(query, options))
    );
    return Promise.all(promises).then(() => undefined);
  }

  #refetch(query: Query, options: RefetchOptions): Promise<void> {
    const running = query.state.fetchStatus === "idle" ? undefined : query.promise;
    if (running === undefined) {
      const fetched = query.fetch(undefined, { ...options, cancelRefetch: false });
      return (options.throwOnError ? fetched : fetched.catch(() => undefined)).then(
        () => undefined
      );
    }
    const hash = query.queryHash;
    let followUp = this.#followUps.get(hash);
    if (followUp === undefined) {
      followUp = running
        .then(
          () => undefined,
          () => undefined
        )
        .then(() => {
          this.#followUps.delete(hash);
          // Removed while it waited (a sign-out, garbage collection): nothing renders it now.
          if (this.getQueryCache().get(hash) !== query || query.isDisabled() || query.isStatic()) {
            return;
          }
          // A fetch that began after the one waited on was composed after this call: it answers.
          const newer = query.state.fetchStatus === "idle" ? undefined : query.promise;
          if (newer !== undefined) {
            return newer.then(
              () => undefined,
              () => undefined
            );
          }
          return this.#refetch(query, options);
        });
      this.#followUps.set(hash, followUp);
    }
    // TanStack resolves a paused fetch's refetch at once rather than holding its caller until the
    // network returns; the follow-up still runs when it does.
    return query.state.fetchStatus === "paused" ? Promise.resolve() : followUp;
  }
}

/**
 * Reads who is signed in (`whoAmIQuery`) again for news that it may have changed, cancelling a
 * check still out first: that check was sent before the news and may name the person signed in
 * before, and an invalidation would wait for its answer rather than start another (TanStack's
 * client joins a fetch while the query has no data, and this one fetches again only after it), so
 * the page would be drawn for that person. The cancel takes effect at once, so the invalidation
 * after it starts a check of its own; a sign-out's `resetQueries` needs no cancel, since a reset
 * drops the fetch it finds.
 *
 * It answers once the newest check has answered, which is what a caller that reads the answer
 * needs (`unauthorized` in `write-store.ts`). Another ask made while this one's check is out - a
 * second write's 401, news of a sign-in - cancels that check and starts its own, which ends this
 * ask's invalidation before any check has answered, so the wait goes on for the check out then. A
 * check the network paused, offline, has not answered either: TanStack calls it `paused` rather
 * than `fetching`, and only `idle` says no check is out, so the wait goes on until the network
 * returns and the check answers.
 */
export async function askSignInAgain(client: QueryClient): Promise<void> {
  void client.cancelQueries({ queryKey: ["whoami"] });
  await client.invalidateQueries({ queryKey: ["whoami"] });
  const whoami = client.getQueryCache().find({ exact: true, queryKey: ["whoami"] });
  for (;;) {
    const out =
      whoami === undefined || whoami.state.fetchStatus === "idle" ? undefined : whoami.promise;
    if (out === undefined) return;
    await out.catch(() => undefined);
  }
}

/**
 * Puts `updater`'s value into the query `queryKey` over every read of it still out. A read in
 * flight was asked before this write, so what it answers of what the write is of is the state
 * the write replaces, and its answer would put that state back when it lands: a mark's answer
 * that clears an unread count would be undone by the count a read asked before the mark brings
 * (`storeAgentState`, `unread.ts`). So the read is cancelled first, in the same synchronous step
 * as the write, as `askSignInAgain` and the board's moves (`board-moves.ts`) cancel theirs:
 * TanStack's cancel takes effect at once, and puts back the state from before that read, which
 * the write then replaces. Nothing runs between the two. A refetch `DispatchQueryClient` held
 * behind the cancelled read (`#refetch`) starts only once that read has settled, after this
 * write, so it asks for the state the write is of. An awaited cancel would put the write after
 * that refetch has started, and every reader of this step would still read the value the write
 * replaces. A read it cancels was asked for a reason - news of another change, a view mounting -
 * and its answer would have brought that news beside the old value, so once the write is in the
 * query is read again, if anything still shows it: the read asked then is asked after the write,
 * and answers both. Answers what `setQueryData` answers.
 */
export function replaceQueryData<T>(
  client: QueryClient,
  queryKey: QueryKey,
  updater: Updater<T | undefined, T | undefined>
): T | undefined {
  const fetchStatus = client.getQueryState(queryKey)?.fetchStatus ?? "idle";
  void client.cancelQueries({ exact: true, queryKey });
  const written = client.setQueryData<T>(queryKey, updater);
  if (fetchStatus !== "idle") {
    void client.invalidateQueries({ exact: true, queryKey, refetchType: "active" });
  }
  return written;
}
