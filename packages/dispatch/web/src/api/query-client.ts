import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";
import {
  type DefaultError,
  type DefaultedQueryObserverOptions,
  hashKey,
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

import { SEND_DEADLINE_MS } from "../features/writes/keyed-write";
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
 * optimistic value stands in for until Dispatch has taken that write (`holdBackReads`).
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
 * and answers both. A write made before Dispatch has taken it may hold its query's reads first
 * (`holdBackReads`), and then that read waits for the hold to end too. Answers what
 * `setQueryData` answers.
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

/** What a write holds back (`holdBackReads`): the queries its value stands in, each by its exact
 *  key, and, where other people write those queries too, the live event that says Dispatch has
 *  taken the write. */
export interface ReadHold {
  readonly keys: readonly QueryKey[];
  /** Whether `event`, delivered to `viewer`'s feed, is one the write publishes: Dispatch publishes
   *  it once the write has committed, so a read asked after it answers the write too. */
  readonly endsOn?: (event: DispatchEvent, viewer: string | undefined) => boolean;
  /** How many such events the hold waits for: one, unless it stands for several writes that each
   *  publish their own (a bulk snooze, one for each ask). */
  readonly events?: number;
}

/**
 * What ends a hold's holding of reads (`holdBackReads`): its write calls it once its answer or
 * refusal is in, with how many of the writes the hold stands for (`ReadHold.events`) Dispatch
 * refused with no unanswered attempt before the refusal (`refusedByDispatch`), none where it
 * leaves that out. Dispatch commits nothing of such a write, so it publishes no event for it, and
 * the hold waits for none. A write refused after an attempt the page had no answer to may have
 * landed, and its hold waits for its event as an answered write's does.
 */
export type EndReadHold = (refused?: number) => void;

/** One hold: the hashes of the queries it holds back while `holding`, and the events of its write
 *  it still waits for. */
interface HeldReads {
  readonly hashes: ReadonlySet<string>;
  readonly endsOn: ReadHold["endsOn"];
  /** How many of its write's events have still to come (`ReadHold.events`): none for a hold that
   *  ends on no event. */
  eventsDue: number;
  /** Whether it holds back its queries' reads still: not once its write's answer or refusal is
   *  in, nor past its deadline, though it may wait for its write's events after either. */
  holding: boolean;
  /** Whether its write's answer or refusal is in. */
  answered: boolean;
  /** Ends it whole: it lets the reads it held go, and waits for no event any more. */
  readonly drop: () => void;
}

/** The holds a client's writes keep on its reads (`holdBackReads`), in the order they began, the
 *  reads waiting for one to end, and the queries whose held read was given up while it waited, by
 *  their keys' hashes. */
interface ReadHolds {
  readonly held: Set<HeldReads>;
  readonly waiting: Set<() => void>;
  readonly givenUp: Map<string, QueryKey>;
}

const readHolds = new WeakMap<QueryClient, ReadHolds>();

/** Whether one of `holds` still holds back the query `queryKey`, by its exact key. */
function holdsRead(holds: ReadHolds, queryKey: QueryKey): boolean {
  const hash = hashKey(queryKey);
  for (const hold of holds.held) {
    if (hold.holding && hold.hashes.has(hash)) return true;
  }
  return false;
}

/** Lets go the reads `hold` holds back, once: each read waiting that no other hold names asks,
 *  and each query whose held read was given up, and that no hold names now, is read again. */
function letReadsGo(client: QueryClient, holds: ReadHolds, hold: HeldReads): void {
  if (!hold.holding) return;
  hold.holding = false;
  for (const check of [...holds.waiting]) check();
  for (const [hash, queryKey] of holds.givenUp) {
    if (holdsRead(holds, queryKey)) continue;
    holds.givenUp.delete(hash);
    if ((client.getQueryState(queryKey)?.fetchStatus ?? "idle") !== "idle") continue;
    void client.invalidateQueries({ exact: true, queryKey, refetchType: "active" });
  }
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
 * Holds back every read of the queries `hold.keys` name, each by its exact key, that starts from
 * now until the hold ends: what a write keeps while its optimistic value stands in those queries
 * and Dispatch has not taken it. A read that started after the optimistic write and reached
 * Dispatch before the write did would answer the state the write replaces, and put it back over
 * the optimistic value while the write is still out; held, it asks once the hold ends, as
 * `projectIssuesQuery` waits for the board's moves. A read already out when the hold starts is not
 * held: the write cancels it (`replaceQueryData`).
 *
 * A held read keeps back more than the old state: whatever anybody else changed in those queries
 * meanwhile shows only once it asks. So a hold lets its reads go soon, at the first of: the
 * function this answers, which the write calls once its answer or refusal is in; the write's own
 * live event (`hold.endsOn`, heard through `hearLiveEvent`), or as many of them as `hold.events`
 * says, published once Dispatch has committed the write, so a read asked after it answers the
 * write and every change since; and `SEND_DEADLINE_MS` after the hold began, by when its write is
 * late at the latest (a write goes late that long after it is sent, and it is sent at the press or
 * after it), so no read waits longer than that for a write that may not reach Dispatch soon.
 *
 * An event says that one write is in, and no event names a tab, so each event ends one hold at
 * most (`hearLiveEvent`): a second write of the same kind may still be out, and its hold must not
 * end on the first one's event. So a hold whose reads went before its event came still waits for
 * that event, holding no read, and takes it ahead of every hold still holding, so the event ends
 * no later write's hold when it comes. That is so from its answer, and from its deadline while its
 * write is still out: Dispatch can take a write after its deadline, and its event then comes after
 * it too. The event can come seconds after the answer even with the live socket up all along:
 * Dispatch commits the write, then answers it, and its feed carries the event only once the event
 * stream relay's safe horizon has passed it, which trails the longest write transaction open on
 * Dispatch's whole Postgres instance (`events/horizon.go`), however little of it is Dispatch's.
 * Nothing bounds that lag, so a hold waits for its event until `SEND_DEADLINE_MS` after its answer:
 * it holds no read meanwhile, so where its event never comes it only takes a later write's, whose
 * hold then ends at its own answer, later and never sooner. A write Dispatch refused with no
 * unanswered attempt before the refusal publishes no event, and its hold waits for none
 * (`EndReadHold`). A write never answered - the page tries a snooze for as long as the reader
 * leaves it - waits until `IDEMPOTENCY_KEY_TTL_MS` after the hold began, a day, the longest
 * Dispatch keeps a write's key, so no hold outlives that.
 *
 * The write's answer, put in over the reads still out, cancels a held read too, though it has
 * asked nothing yet, and what it was asked for - news of another change, a view mounting - would
 * go unread: such a query is read again once no hold names it, if anything still shows it and no
 * read of it is out already (the one `replaceQueryData` asks). `DispatchQueryClient` holds a read
 * back in its query's function, after `onAttempt` is told, so a plain `QueryClient` lets every read
 * go. The function this answers lets the hold's reads go once; called again, it does nothing.
 */
export function holdBackReads(client: QueryClient, hold: ReadHold): EndReadHold {
  let holds = readHolds.get(client);
  if (holds === undefined) {
    holds = { givenUp: new Map(), held: new Set(), waiting: new Set() };
    readHolds.set(client, holds);
  }
  const ours = holds;
  const held: HeldReads = {
    answered: false,
    drop: () => {
      ours.held.delete(held);
      clearTimeout(ends);
      letReadsGo(client, ours, held);
    },
    endsOn: hold.endsOn,
    eventsDue: hold.endsOn === undefined ? 0 : (hold.events ?? 1),
    hashes: new Set(hold.keys.map((key) => hashKey(key))),
    holding: true,
  };
  ours.held.add(held);
  // What ends it short of its events: its deadline, then for a write still out the key's lifetime,
  // and once the answer is in, `SEND_DEADLINE_MS` after it.
  let ends = setTimeout(() => {
    if (held.eventsDue === 0) {
      held.drop();
      return;
    }
    letReadsGo(client, ours, held);
    ends = setTimeout(held.drop, IDEMPOTENCY_KEY_TTL_MS - SEND_DEADLINE_MS);
  }, SEND_DEADLINE_MS);
  return (refused = 0) => {
    if (held.answered) return;
    held.answered = true;
    held.eventsDue = Math.max(0, held.eventsDue - refused);
    if (held.eventsDue === 0 || !ours.held.has(held)) {
      held.drop();
      return;
    }
    letReadsGo(client, ours, held);
    clearTimeout(ends);
    ends = setTimeout(held.drop, SEND_DEADLINE_MS);
  };
}

/**
 * Gives `event` to one hold that waits for an event it matches (`ReadHold.endsOn`), and to no
 * other; the event ends that hold once it has had as many as it waits for. The live feed's every
 * event comes here (`useEventFeed`), before the refresh it asks for, which then reads past the hold
 * it ended. `viewer` is the login signed in.
 *
 * One event is one write's, and no event names a tab. The hold that takes it is the oldest whose
 * write's answer is in, since that write is in and its event is due, or else the oldest whose
 * write is still out, which is a hold past its deadline before any hold still holding: a hold
 * waits for its write's events after it lets its reads go (`holdBackReads`), so a write's event,
 * though it comes after the write's answer or deadline, finds its own write's hold and not a later
 * one's. Where the event names the entity written, a hold matches that entity's events alone, and
 * of two writes of one entity - two components saves of an issue, which go out in press order on
 * the issue's line - the older one's event comes first. Where it names none (a snooze's names no
 * ask, a Mark all read's no thread), a hold may take another write's event in place of its own,
 * and that write's hold then takes the first one's event when it comes.
 *
 * A hold that holds reads ends early, on another write's event, in three cases. Dispatch takes two
 * writes still out in the other order than their presses, which no event can tell apart. A write's
 * event comes after its hold stopped waiting for it: more than `SEND_DEADLINE_MS` after its answer
 * (the safe horizon of Dispatch's event stream held it back that long, or the live socket was down
 * that long), or a day after its press for a write never answered. And a Mark all read or a
 * dismiss that moves threads of several projects publishes one event for each, so its later ones
 * count toward the next such hold still out. In each, the read the hold let go can show the state
 * from before that write's press until the write's answer comes. Every other misreading ends a
 * hold later, never sooner: a write Dispatch took that publishes no such event (it moved nothing)
 * may take a later write's, and so may a write still out past its deadline - a snooze Dispatch has
 * not answered takes the next snooze event - and a write Dispatch refused after an attempt the
 * page had no answer to, which waits for its event though that attempt may not have landed; the
 * hold whose event it was then ends at its own answer, later and never sooner.
 */
export function hearLiveEvent(
  client: QueryClient,
  event: DispatchEvent,
  viewer: string | undefined
): void {
  const holds = readHolds.get(client);
  if (holds === undefined) return;
  let taker: HeldReads | undefined;
  for (const hold of holds.held) {
    if (hold.eventsDue === 0 || hold.endsOn?.(event, viewer) !== true) continue;
    if (hold.answered) {
      taker = hold;
      break;
    }
    taker ??= hold;
  }
  if (taker === undefined) return;
  taker.eventsDue -= 1;
  if (taker.eventsDue === 0) taker.drop();
}
