import {
  type DefaultError,
  type DefaultedQueryObserverOptions,
  hashKey,
  notifyManager,
  partialMatchKey,
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
import type { DispatchEvent } from "./types";

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
 * listens each time a query calls its function (`onAttempt`), and holds back a read that a write's
 * optimistic value stands in for until that write's answer is in (`holdBackReads`).
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
   *  `onAttempt`'s listeners first, and then waits while a write holds the query's reads
   *  (`holdBackReads`); the wrapper hands the function its context and returns what the function
   *  returns, and a listener that throws fails no read. `skipToken`, an absent function and a
   *  wrapped one are left as they are. Each observer's render passes a function of its own, so a
   *  wrapper is made for each call rather than kept. */
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
      const holds = readHolds.get(this);
      if (holds === undefined || !holdsRead(holds, context.queryKey)) return queryFn(context);
      return holdEnded(holds, context).then(() => queryFn(context));
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
 * Puts `updater`'s value into the query `queryKey` over every read of it still out. A read in
 * flight was asked before this write, so it answers the state the write replaces, and its answer
 * would put that state back when it lands: a mark's answer that clears an unread count would be
 * undone by the count a read asked before the mark brings (`storeAgentState`, `unread.ts`). So the
 * read is cancelled first, in the same synchronous step as the write, as `askSignInAgain` and the
 * board's moves (`board-moves.ts`) cancel theirs: TanStack's cancel takes effect at once, and puts
 * back the state from before that read, which the write then replaces. Nothing runs between the
 * two. A refetch `DispatchQueryClient` held behind the cancelled read (`#refetch`) starts only once
 * that read has settled, after this write, so it asks for the state the write is of. An awaited
 * cancel would put the write after that refetch has started, and every reader of this step would
 * still read the value the write replaces. A read it cancels was asked for a reason - news of
 * another change, a view mounting - and its answer would have brought that news beside the old
 * value, so once the write is in the query is read again, if anything still shows it: the read
 * asked then is asked after the write, and answers both. A write made before Dispatch has taken it
 * may hold its query's reads first (`holdBackReads`), and then that read waits for the hold to
 * end too. Answers what `setQueryData` answers.
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

/** What a write holds back (`holdBackReads`): the queries its value stands in, each key, as a
 *  filter's `queryKey` does, naming every query it is a prefix of, and the live event that says
 *  Dispatch has taken the write. */
export interface ReadHold {
  readonly keys: readonly QueryKey[];
  /** Whether `event`, delivered to `viewer`'s feed, is the write's own: Dispatch publishes it once
   *  the write has committed, so a read asked after it answers the write too. */
  readonly endsOn?: (event: DispatchEvent, viewer: string | undefined) => boolean;
}

/** One hold in force: the keys it holds, its event, and how it ends. */
interface HeldReads {
  readonly keys: readonly QueryKey[];
  readonly endsOn: ReadHold["endsOn"];
  readonly end: () => void;
}

/** The holds a client's writes keep on its reads (`holdBackReads`), the reads waiting for one to
 *  end, and the queries whose held read was given up while it waited, by their keys' hashes. */
interface ReadHolds {
  readonly held: Set<HeldReads>;
  readonly waiting: Set<() => void>;
  readonly givenUp: Map<string, QueryKey>;
}

const readHolds = new WeakMap<QueryClient, ReadHolds>();

/** Whether one of `holds` names the query `queryKey`: as a filter's `queryKey` does, a key names
 *  every query it is a prefix of. */
function holdsRead(holds: ReadHolds, queryKey: QueryKey): boolean {
  for (const hold of holds.held) {
    if (hold.keys.some((key) => partialMatchKey(queryKey, key))) return true;
  }
  return false;
}

/** Resolves once no hold names the read's query; rejects if the read is cancelled first, and
 *  notes its query to be read again once no hold names it (`holdBackReads`). */
function holdEnded(
  holds: ReadHolds,
  context: { readonly queryKey: QueryKey; readonly signal: AbortSignal }
): Promise<void> {
  // Read only here: once a fetch reads its signal, query-core cancels it when the last observer
  // unmounts, which a read that is not held has no reason to invite (`projectIssuesQuery`).
  const { signal } = context;
  const ended = Promise.withResolvers<void>();
  const stop = () => {
    holds.waiting.delete(check);
    signal.removeEventListener("abort", abort);
  };
  const abort = () => {
    stop();
    holds.givenUp.set(hashKey(context.queryKey), context.queryKey);
    ended.reject(signal.reason);
  };
  const check = () => {
    if (holdsRead(holds, context.queryKey)) return;
    stop();
    ended.resolve();
  };
  holds.waiting.add(check);
  signal.addEventListener("abort", abort);
  return ended.promise;
}

/**
 * Holds back every read of the queries `hold.keys` name - each key, as a filter's `queryKey`
 * does, every query it is a prefix of - that starts from now until the function this answers is
 * called, or until `hearLiveEvent` hears the write's own event (`hold.endsOn`): what a write keeps
 * while its optimistic value stands in those queries and its answer is not in. A read that started
 * after the optimistic write and reached Dispatch before the write did would answer the state the
 * write replaces, and put it back over the optimistic value while the write is still out; held, it
 * asks once the hold ends, as `projectIssuesQuery` waits for the board's moves. A read already out
 * when the hold starts is not held: the write cancels it (`replaceQueryData`). The write's answer,
 * put in over the reads still out, cancels a held read too, though it has asked nothing yet, and
 * what it was asked for - news of another change, a view mounting - would go unread: such a query
 * is read again once no hold names it, if anything still shows it and no read of it is out already
 * (the one `replaceQueryData` asks). `DispatchQueryClient` holds a read back in its query's
 * function, after `onAttempt` is told, so a plain `QueryClient` lets every read go. The function
 * this answers ends the hold once; called again, it does nothing.
 */
export function holdBackReads(client: QueryClient, hold: ReadHold): () => void {
  let holds = readHolds.get(client);
  if (holds === undefined) {
    holds = { givenUp: new Map(), held: new Set(), waiting: new Set() };
    readHolds.set(client, holds);
  }
  const ours = holds;
  const end = () => {
    if (!ours.held.delete(held)) return;
    for (const check of [...ours.waiting]) check();
    for (const [hash, queryKey] of ours.givenUp) {
      if (holdsRead(ours, queryKey)) continue;
      ours.givenUp.delete(hash);
      if ((client.getQueryState(queryKey)?.fetchStatus ?? "idle") !== "idle") continue;
      void client.invalidateQueries({ exact: true, queryKey, refetchType: "active" });
    }
  };
  const held: HeldReads = {
    end,
    endsOn: hold.endsOn,
    keys: [...hold.keys],
  };
  ours.held.add(held);
  return end;
}

/** Ends each hold whose write `event` says Dispatch has taken (`ReadHold.endsOn`): the live
 *  feed's every event comes here (`useEventFeed`), before the refresh it asks for, which then
 *  reads past the holds it ended. `viewer` is the login signed in. */
export function hearLiveEvent(
  client: QueryClient,
  event: DispatchEvent,
  viewer: string | undefined
): void {
  const holds = readHolds.get(client);
  if (holds === undefined) return;
  for (const hold of [...holds.held]) {
    if (hold.endsOn?.(event, viewer) === true) hold.end();
  }
}
