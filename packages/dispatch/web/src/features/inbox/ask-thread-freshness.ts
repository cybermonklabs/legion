/** The two reads an Inbox card can mount its thread from: the read of the open asks
 *  (`openAsksQuery`), which the page draws every person's asks but the reader's own from, and the
 *  one read (`meInboxQuery`), which it draws the reader's own asks from. */
export type InboxSource = "open-asks" | "one-read";

/** An event that arrived while a read of the Inbox was in flight: the generation newest then,
 *  and the reads whose response in flight then can still seed a card with the body composed
 *  before it. Each read settles its own part; the marker clears once all have. */
interface Marker {
  readonly during: number;
  readonly waiting: Set<InboxSource>;
}

interface FreshnessState {
  /** Generation → the read it is a request of, for each request in flight. */
  activeInboxFetches: Map<number, InboxSource>;
  latestInboxFetch: number;
  /** Ask id → the marker of an event on that ask. */
  pending: Map<string, Marker>;
  /** Issue key → the marker of an event that moves every thread on it. */
  pendingIssues: Map<string, Marker>;
}

const stateByQueryClient = new WeakMap<object, FreshnessState>();

function stateFor(queryClient: object): FreshnessState {
  let state = stateByQueryClient.get(queryClient);
  if (state === undefined) {
    state = {
      activeInboxFetches: new Map(),
      latestInboxFetch: 0,
      pending: new Map(),
      pendingIssues: new Map(),
    };
    stateByQueryClient.set(queryClient, state);
  }
  return state;
}

function discardEmptyState(queryClient: object, state: FreshnessState): void {
  if (
    state.activeInboxFetches.size === 0 &&
    state.pending.size === 0 &&
    state.pendingIssues.size === 0
  ) {
    stateByQueryClient.delete(queryClient);
  }
}

/** Starts a generation of `source`'s requests. A marker records the generation newest when its
 *  event arrived, so only a later request of each read in flight then can supersede it. */
export function beginInboxFetch(queryClient: object, source: InboxSource): number {
  const state = stateFor(queryClient);
  state.latestInboxFetch += 1;
  state.activeInboxFetches.set(state.latestInboxFetch, source);
  return state.latestInboxFetch;
}

/** Whether a read of the Inbox is in flight, so an event now may postdate the body it lands. */
export function isInboxFetching(queryClient: object): boolean {
  return (stateByQueryClient.get(queryClient)?.activeInboxFetches.size ?? 0) > 0;
}

/** Settles the read `source`'s part of `key`'s marker, and clears the marker once no read it
 *  waited on is left. */
function settle(
  markers: Map<string, Marker>,
  key: string,
  marker: Marker,
  source: InboxSource
): void {
  marker.waiting.delete(source);
  if (marker.waiting.size === 0) markers.delete(key);
}

/** Settles, for the read whose request `fetch` was, the markers its response resolves: a newer
 *  one, or one that omits the ask, so no card of that read mounts from it. An issue's marker is
 *  settled only by a newer response that arrived: its body was composed after the event, so every
 *  card it seeds on that issue holds the moved claimant and route. Each read settles only its own
 *  part, since a card the other read seeds still mounts from that read's body. */
export function finishInboxFetch(
  queryClient: object,
  fetch: number,
  asks: readonly { readonly id: string }[] | undefined
): void {
  const state = stateFor(queryClient);
  const source = state.activeInboxFetches.get(fetch);
  state.activeInboxFetches.delete(fetch);
  if (source !== undefined) {
    const listed = asks === undefined ? undefined : new Set(asks.map((ask) => ask.id));
    for (const [askID, marker] of state.pending) {
      if (marker.during <= fetch && (marker.during < fetch || !listed?.has(askID))) {
        settle(state.pending, askID, marker, source);
      }
    }
    if (asks !== undefined) {
      for (const [issueKey, marker] of state.pendingIssues) {
        if (marker.during < fetch) settle(state.pendingIssues, issueKey, marker, source);
      }
    }
  }
  discardEmptyState(queryClient, state);
}

/** The marker of an event arriving now: it waits on every read in flight. */
function markerNow(state: FreshnessState): Marker {
  return {
    during: state.latestInboxFetch,
    waiting: new Set(state.activeInboxFetches.values()),
  };
}

/** Records an ask event that arrived while a read of the Inbox was fetching but before this
 * thread existed in the cache. Its card must read the targeted endpoint instead of trusting that
 * in-flight list response. */
export function markPendingAskThreadInvalidation(queryClient: object, askID: string): void {
  const state = stateFor(queryClient);
  state.pending.set(askID, markerNow(state));
}

/** Records an event that moves every ask thread on an issue (a claim, a release, a route change)
 * and arrived while a read of the Inbox was fetching. The in-flight body names the old claimant
 * and route for each of the issue's asks, so a card on that issue that mounts from it reads the
 * targeted endpoint instead, until a newer response of each read in flight then lands. */
export function markPendingAskThreadsOnIssue(queryClient: object, issueKey: string): void {
  const state = stateFor(queryClient);
  state.pendingIssues.set(issueKey, markerNow(state));
}

/** The marker is read during render but cleared only after the targeted read has supplied data,
 * so a concurrent React render cannot consume it before the query starts. An ask on a marked issue
 * is pending too. */
export function hasPendingAskThreadInvalidation(
  queryClient: object,
  ask: { readonly id: string; readonly issue_key: string | null }
): boolean {
  const state = stateByQueryClient.get(queryClient);
  if (state === undefined) return false;
  return (
    state.pending.has(ask.id) || (ask.issue_key !== null && state.pendingIssues.has(ask.issue_key))
  );
}

/** Clears the ask's own marker once its targeted read has supplied data. An issue's marker stays:
 * other cards on the issue may still mount from the stale Inbox body, and only a newer Inbox
 * response clears it (`finishInboxFetch`). */
export function clearPendingAskThreadInvalidation(queryClient: object, askID: string): void {
  const state = stateByQueryClient.get(queryClient);
  if (state === undefined) return;
  state.pending.delete(askID);
  discardEmptyState(queryClient, state);
}
