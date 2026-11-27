import type { QueryClient } from "@tanstack/react-query";

import { issueTreeKey } from "../../../api/queries";
import { type RefreshTarget, refreshQueries } from "../../../api/query-refresh";

/**
 * How the issue page's tree read is told of the block index catching up (`artifact.indexed`):
 * once for each burst of such events of one issue (`indexedTreeTarget`). A module of its own
 * beside `tree-read.ts`, which keeps the tree read fresh otherwise (`subscribeTreeFreshness`),
 * since `api/event-refresh.ts` imports this and the app's entry imports that, and the entry loads
 * no file of the new issue page's model (`__tests__/entry-closure.test.ts`), which `tree-read.ts`
 * imports. It imports the tree's key and the refresh scheduler alone.
 *
 * While an issue's block index catches up it sends one such event for each pass, and each read
 * the whole outline again, with every page it had loaded: on the copy of INFRA-1196, 64 passes in
 * 231 s, a median of 2.6 s apart, and one load read the outline 43 times, 2 MB decoded each. No
 * freshness rule of the page bounds how soon a pass of the index reaches the outline: R6's re-read
 * follows the reader's own writes, whose events are of other types and whose hold reads the tree
 * as it ends (`tree-read.ts`' `subscribeTreeFreshness`), and decision 180 asks only that events,
 * never age, refresh it. A pass moves no document's words, name or version, only the blocks the
 * server reads from the index, and the coverage read, which draws the "still being indexed" line,
 * is told of it at once. So what bounds the wait is what the reader can see of it: until the read,
 * the outline may draw a document's blocks from before a pass, while the coverage read no longer
 * says the document is being indexed.
 */

/**
 * How long after the last `artifact.indexed` event of an issue its tree and its views are read
 * again, once, for every event of the burst (`indexedTreeTarget`), unless the burst reached
 * `INDEX_REFRESH_SPACING_MS` first, and no sooner than that long after the issue's previous index
 * read. So a pass of the index that comes alone, after ten seconds with no pass and no index read,
 * reaches the outline within it: two and a half seconds, root R43's bound on drawing a view's
 * first screen from a cold load, so the outline draws such a pass no later than a reload begun at
 * the event would, its read aside.
 */
export const INDEX_REFRESH_QUIET_MS = 2_500;

/**
 * The longest an `artifact.indexed` event waits for its read (`indexedTreeTarget`), however many
 * events follow it, and the shortest time between two index reads of one issue. So a catch-up
 * reads the outline at most once in each ten seconds, not once a second as the scheduler would
 * (`MIN_REFETCH_INTERVAL_MS`), and the outline is never more than ten seconds behind the index
 * while it catches up. Only a pass inside a catch-up waits longer than `INDEX_REFRESH_QUIET_MS`:
 * one that another pass follows within that time, or one that comes less than seven and a half
 * seconds after the issue's previous index read. Replayed through these rules, the copy's 64
 * passes, a median of 2.6 s apart, read the outline 21 times, and the longest wait of any pass is
 * ten seconds; a fixed window of 2.5 s from each burst's first pass reads it 41 times, and the
 * scheduler's one second 56.
 */
export const INDEX_REFRESH_SPACING_MS = 10_000;

/** The open burst of `artifact.indexed` events of one issue: when its first event came, and the
 *  timer that reads the issue's tree at its end. */
interface IndexBurst {
  readonly first: number;
  timer: number | undefined;
}

/** One client's index reads of each issue, by the issue's key: the burst open, from its first
 *  event until its end asks for the issue's tree, and when the last such read was asked for. */
interface IndexReads {
  readonly bursts: Map<string, IndexBurst>;
  readonly last: Map<string, number>;
}

const indexReads = new WeakMap<QueryClient, IndexReads>();

/**
 * The target an `artifact.indexed` event of issue `key` names its tree by (`event-refresh.ts`):
 * the tree's key, by prefix, as every event of the issue names it, with a check that reads nothing
 * now and puts the issue's read `INDEX_REFRESH_QUIET_MS` after this event, or
 * `INDEX_REFRESH_SPACING_MS` after the burst's first, whichever is sooner, but no sooner than
 * `INDEX_REFRESH_SPACING_MS` after the issue's previous index read. At the burst's end the tree
 * and each view under its key are asked for once (`refreshQueries`), under the scheduler's rules
 * and the hold of the issue's writes, for every event the burst held, and the next event starts
 * another burst. Any other event of the issue refreshes them as it did. A query whose read is out
 * as the event comes is judged as that read lands (`refreshQueries`), and counts as an event of
 * then. No wait is longer than `INDEX_REFRESH_SPACING_MS`, a wall clock stepped backwards
 * included. A check handed no client, apart from a refresh, reads the tree as another event's
 * would.
 */
export function indexedTreeTarget(key: string): RefreshTarget {
  return {
    queryKey: issueTreeKey(key),
    when: (_data, _query, client) => {
      if (client === undefined) return true;
      const reads = indexReads.get(client) ?? { bursts: new Map(), last: new Map() };
      indexReads.set(client, reads);
      const now = Date.now();
      const burst = reads.bursts.get(key) ?? { first: now, timer: undefined };
      reads.bursts.set(key, burst);
      window.clearTimeout(burst.timer);
      const ends = Math.min(INDEX_REFRESH_QUIET_MS, burst.first + INDEX_REFRESH_SPACING_MS - now);
      const spaced = (reads.last.get(key) ?? Number.NEGATIVE_INFINITY) + INDEX_REFRESH_SPACING_MS;
      burst.timer = window.setTimeout(
        () => {
          reads.bursts.delete(key);
          reads.last.set(key, Date.now());
          refreshQueries(client, [{ queryKey: issueTreeKey(key) }]);
        },
        Math.min(INDEX_REFRESH_SPACING_MS, Math.max(ends, spaced - now))
      );
      return false;
    },
  };
}
