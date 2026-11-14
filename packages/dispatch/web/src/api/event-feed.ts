import { useEffect } from "react";

import { reaskHeldCredentialList } from "../features/credentials/pending";
import {
  markPendingAskThreadInvalidation,
  markPendingAskThreadsOnIssue,
} from "../features/inbox/ask-thread-freshness";
import { type Cache, useCache } from "./cache/cache";
import { decodeEvent, prependEventToLog } from "./cache/event-log";
import { liveSocketFor } from "./cache/live-socket";
import { type RefreshTarget, refreshQueries, trackQueryFetches } from "./cache/query-refresh";
import { askThreadsIssue, eventRefreshTargets } from "./event-refresh";
import { inboxQuery } from "./queries";
import type { AuthenticatedUser } from "./types";

const inboxQueryKey = inboxQuery().queryKey;

// Leading window for a burst: a cold-start replay or a flurry of events on one issue is handed to
// the refresh scheduler (`api/cache/query-refresh.ts`) as one batch, 100ms after the first event of the
// burst - later events join the pending batch but do not extend the window.
const BATCH_WINDOW_MS = 100;

// A whole-cache refresh reaches every query the app holds, the GitHub proxy queries included,
// and two callers ask for one: an event this client cannot parse, and a gap in the feed that its
// resume did not replay. Neither is rate-limited on its own - a mid-deploy stream of a new event
// type arrives at the frame rate, and a Dispatch restarting node by node can close the socket
// again and again - so the throttle belongs to "refresh everything" rather than to either caller.
// Every request schedules the one pending refresh: a request a window or more after the last one
// schedules it for now, and any inside the window joins that same pending refresh, so a change no
// target can be derived from always arrives within the window of the request that asked for it.
// The backlink panels' own `["references", "to", …]` queries are part of "everything", so such a
// gap refreshes them too.
// Exported for the one test that deliberately runs at the production window rather than an
// injected one, so it reads the real number instead of keeping a copy that drifts.
export const WHOLE_CACHE_REFRESH_MS = 5_000;

/**
 * An ask event that arrives while the Inbox is fetching, for a thread the cache does not hold
 * yet, is invisible to the flush: its `["ask-thread", id]` key matches nothing, and a card that
 * mounts from the landing rows seeds that thread from a body composed before the event. The
 * Inbox's own refetch once its load settles does not reach this - a card can mount from the
 * landing rows first - so the marker makes the card read the ask endpoint instead of trusting
 * the row it mounted from. An event that moves every thread on an issue (`askThreadsIssue`: a
 * claim, a release, a route change) marks the issue the same way, since its refresh reaches only
 * the threads already cached. Exported for the tests, which hold an Inbox fetch open around it.
 */
export function markAskThreadsSeededByAnInFlightInbox(
  cache: Cache,
  targets: readonly RefreshTarget[],
  issue: string | null
): void {
  if (!cache.fetching(inboxQueryKey)) {
    return;
  }
  for (const { queryKey } of targets) {
    if (queryKey[0] === "ask-thread" && typeof queryKey[1] === "string" && !cache.holds(queryKey)) {
      markPendingAskThreadInvalidation(cache, queryKey[1]);
    }
  }
  if (issue !== null) {
    markPendingAskThreadsOnIssue(cache, issue);
  }
}

/**
 * The batch without the repeats of a target that has no `when`: events on one issue each push
 * the same targets, and every copy past the first is one more full-cache `findAll` at the flush
 * that reaches nothing the first did not. A target with a `when` always stays, since its
 * predicate cannot be compared by key. First-seen order is kept.
 */
function dedupeRefreshTargets(targets: readonly RefreshTarget[]): RefreshTarget[] {
  const seen = new Set<string>();
  return targets.filter((target) => {
    if (target.when !== undefined) {
      return true;
    }
    const key = `${target.exact === true} ${JSON.stringify(target.queryKey)}`;
    if (seen.has(key)) {
      return false;
    }
    seen.add(key);
    return true;
  });
}

/** A decimal event id, comparable; null for anything else. */
function eventPosition(id: string): bigint | null {
  return /^\d+$/.test(id) ? BigInt(id) : null;
}

/**
 * Keeps the query cache current from the workspace's event feed, one subscription on the page's
 * live socket (`api/cache/live-socket.ts`). `wholeCacheRefreshMs` overrides the whole-cache refresh
 * throttle; the only callers that set it are tests that would otherwise have to wait out the real
 * 5 s window. Production calls this with no argument.
 */
export function useEventFeed({
  wholeCacheRefreshMs = WHOLE_CACHE_REFRESH_MS,
}: {
  wholeCacheRefreshMs?: number;
} = {}): void {
  const cache = useCache();

  useEffect(() => {
    // The greatest event id the page has seen, which the subscription resumes after; never
    // lowered. It starts at the sign-in check's `event_head`, which every answer carries:
    // AuthGate mounts the app, and so every read the page makes, only once that answer is in, so
    // those reads hold every event at or below it, and the first subscription replays exactly
    // what committed since, which each event's own refresh targets refresh. It is read once,
    // here: a later check names a head no read of the page was made after. The cursor is null
    // only where there is no answer to read it from - no page of the app, which mounts under
    // that answer - and the feed then adopts where Dispatch starts it (`events_start`).
    const signedIn = cache.dispatched<AuthenticatedUser>(["whoami"]);
    let cursor = signedIn === undefined ? null : eventPosition(signedIn.event_head);
    let flush: number | undefined;
    // `pendingAll` is the whole-cache entry of the pending batch: it means "something changed
    // that no target can be derived from", and it subsumes every target already queued.
    let pendingAll = false;
    // `performance.now()`, not `Date.now()`: a monotonic clock, so a backwards system-clock
    // step cannot park the throttle for the length of the step.
    let lastWholeCacheRefresh = Number.NEGATIVE_INFINITY;
    let wholeCacheTrailing: number | undefined;
    let pending: RefreshTarget[] = [];
    // Record when each query's fetches begin from now on, so the scheduler's one-a-second rule
    // holds for the first batch after the page's own loads too.
    trackQueryFetches(cache);

    const scheduleFlush = () => {
      if (flush !== undefined) {
        return;
      }
      flush = window.setTimeout(() => {
        flush = undefined;
        const targets = pendingAll ? undefined : dedupeRefreshTargets(pending);
        pendingAll = false;
        pending = [];
        refreshQueries(cache, targets);
      }, BATCH_WINDOW_MS);
    };

    /**
     * Every whole-cache refresh goes through here, whatever asked for one. A request outside the
     * window refreshes at once; one inside it schedules the single trailing refresh, which is
     * what the `setTimeout` of zero or less does for the leading edge too.
     */
    const refreshEverything = () => {
      if (wholeCacheTrailing !== undefined) {
        return;
      }
      // Clamped: any request more than one window after the last refresh - the first of a
      // page, which has none to measure from, most of all - leaves the delay negative.
      // Browsers clamp a negative timeout to zero; saying so here keeps the intent explicit and
      // the runtime from warning about it.
      const remaining = Math.max(
        0,
        wholeCacheRefreshMs - (performance.now() - lastWholeCacheRefresh)
      );
      wholeCacheTrailing = window.setTimeout(() => {
        wholeCacheTrailing = undefined;
        lastWholeCacheRefresh = performance.now();
        pendingAll = true;
        scheduleFlush();
      }, remaining);
    };

    const queueRefresh = (targets: readonly RefreshTarget[]) => {
      // A latched whole-cache refresh subsumes every target, so there is nothing to record.
      if (!pendingAll) {
        pending.push(...targets);
      }
      scheduleFlush();
    };

    const stop = liveSocketFor(cache).subscribeEvents({
      since: () => (cursor === null ? null : cursor.toString()),
      onStart: (since, afterGap, replayComplete) => {
        if (cursor === null) {
          cursor = eventPosition(since);
        }
        if (!afterGap) {
          return;
        }
        // Events may have committed while nothing carried the feed - or, when the first handshake
        // failed, since the page's own reads - so every query the app holds refreshes, once the
        // feed has started again (refreshing at each send would refetch everything again and
        // again while Dispatch cannot start it), unless Dispatch says this subscription replays
        // every event after the resume it was sent (`replay_complete`: a resume the log still
        // holds): those events refresh what they changed, as any other does. A resume above a
        // reset log's head says no such thing. TanStack matches every query when no filter is
        // given. Nothing is excluded from the refresh: the one query that looks expensive,
        // `["block-schema"]` with an infinite `staleTime`, resolves from a module-level
        // per-session cache (`features/doc/schema.ts`), so its refetch issues no request. The
        // refresh runs at the start itself, which follows a reopen `reconnectDelayMs`
        // (`api/cache/live-socket.ts`) already spreads after a dropped socket, so it waits no further:
        // tabs a restart dropped together and Dispatch does not replay in full refresh spread as
        // well.
        if (!replayComplete) {
          refreshEverything();
        }
        // The one query a refresh skips: a credential list held at `null` (no broker), which a
        // server restarted since with a broker configured would now answer with a list. A
        // restart replays nothing that says so.
        reaskHeldCredentialList(cache);
      },
      onEvent: (id, raw) => {
        // The cursor advances before the event is read on purpose: an event this page cannot
        // read is skipped by the next resume instead of being replayed to it again.
        const position = eventPosition(id);
        if (position !== null && (cursor === null || position > cursor)) {
          cursor = position;
        }
        // The socket hands the event over as the JSON Dispatch sent, read here as Dispatch's
        // event once its type is known (`decodeEvent`).
        const event = decodeEvent(raw);
        if (event === undefined) {
          // The server may add an event this client cannot parse. Refresh everything rather than
          // leaving a rendered field stale until the next gap.
          refreshEverything();
          return;
        }
        try {
          prependEventToLog(cache, event);
          const targets = eventRefreshTargets(
            event.body,
            cache.dispatched<AuthenticatedUser>(["whoami"])?.login
          );
          markAskThreadsSeededByAnInFlightInbox(cache, targets, askThreadsIssue(event.body));
          queueRefresh(targets);
        } catch (error) {
          // A known type whose payload this page cannot map: no target can be derived from it.
          console.error("event feed: an event this page cannot read", raw, error);
          refreshEverything();
        }
      },
    });

    return () => {
      stop();
      window.clearTimeout(flush);
      window.clearTimeout(wholeCacheTrailing);
      pending = [];
    };
  }, [cache, wholeCacheRefreshMs]);
}
