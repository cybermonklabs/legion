import {
  notifyManager,
  type Query,
  QueryClient,
  type QueryKey,
  type RefetchOptions,
  type RefetchQueryFilters,
} from "@tanstack/react-query";

import { ApiError, isEventHeadUnavailable, isRetryableQueryError } from "../client";

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
 * Whether a failed query is asked again (`retry` in `provider.tsx`). An auth outcome (401) is
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
 * The wait before every query's retry (`retryDelay` in `provider.tsx`). A server refusing excess load
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

/** Whether a fetch whose `meta` is `meta` reads one page beside the pages its query holds - an
 *  infinite query's `fetchNextPage` or `fetchPreviousPage`, whose `fetch` action and `fetchMeta`
 *  name `fetchMore` - rather than the whole query. TanStack builds such a page on the pages the
 *  cache holds and hands them back with it, so its answer says nothing new of those pages: the
 *  one rule both the follow-up below and the store's fetch counter (`Writes.#countFetches`) read
 *  a whole read by. */
export function readsOnePage(meta: { readonly fetchMore?: unknown } | null | undefined): boolean {
  return meta?.fetchMore !== undefined;
}

/** Waits for `promise` to settle either way. */
function settled(promise: Promise<unknown>): Promise<void> {
  return promise.then(
    () => undefined,
    () => undefined
  );
}

/**
 * Dispatch's query client: TanStack's, except that no read ever has two requests in flight.
 * `refetchQueries` - and through it `invalidateQueries` and `resetQueries`, which TanStack routes
 * through this method - never cancels a running request. A query that is already fetching is
 * fetched once more when that fetch settles, so an invalidation a mutation makes mid-fetch still
 * ends on the answer composed after the write, and the first request is not left running on the
 * server with its answer thrown away. Calls for one query while it waits share that one follow-up
 * fetch. A whole read that began after the one waited on answers the call in its place; a page
 * read (`readsOnePage`) does not - a Show more cancels the whole read out and builds on the pages
 * held - so the follow-up waits for it and then reads the whole query. Every mutation's own
 * `invalidateQueries` call keeps its shape.
 */
export class DispatchQueryClient extends QueryClient {
  readonly #followUps = new Map<string, Promise<void>>();

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
      followUp = this.#followUp(query, options, running);
      this.#followUps.set(hash, followUp);
    }
    // TanStack resolves a paused fetch's refetch at once rather than holding its caller until the
    // network returns; the follow-up still runs when it does.
    return query.state.fetchStatus === "paused" ? Promise.resolve() : followUp;
  }

  /** The fetch of `query` once `running` settles: none where a whole read began after it,
   *  which answers in its place, and otherwise one, after any page read out then settles. */
  async #followUp(query: Query, options: RefetchOptions, running: Promise<unknown>): Promise<void> {
    await settled(running);
    this.#followUps.delete(query.queryHash);
    for (;;) {
      // Removed while it waited (a sign-out, garbage collection): nothing renders it now.
      if (
        this.getQueryCache().get(query.queryHash) !== query ||
        query.isDisabled() ||
        query.isStatic()
      ) {
        return;
      }
      const newer = query.state.fetchStatus === "idle" ? undefined : query.promise;
      if (newer === undefined) return this.#refetch(query, options);
      // A whole read that began after the one waited on was composed after this call: it answers.
      if (!readsOnePage(query.state.fetchMeta)) return settled(newer);
      await settled(newer);
    }
  }
}
