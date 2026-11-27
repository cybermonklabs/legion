import { renderChunk } from "../features/shell/chunk-loads";
import type { markCorrectedPost } from "./corrected-post";
import type { askThreadsIssue, eventRefreshTargets } from "./event-refresh";

/** What the feed reads an event with, once the chunk holding the map has arrived. */
export interface EventRefreshMap {
  readonly askThreadsIssue: typeof askThreadsIssue;
  readonly eventRefreshTargets: typeof eventRefreshTargets;
  readonly markCorrectedPost: typeof markCorrectedPost;
}

/**
 * The map from an event to the queries it changes (`event-refresh.ts`), and the correction an
 * edited or deleted post writes into the reads that hold the post (`corrected-post.ts`), fetched
 * when the feed starts rather than with the entry chunk every page parses before it draws
 * anything. The two are one chunk: this imports the correction's module, which imports the map
 * and hands on the two functions of it the feed reads, so the feed waits on one download.
 *
 * The chunk is about 34,500 bytes (`bun run build:web`, 2026-11-24) and no first paint reads one
 * byte of it: a page draws from its landing read, and the feed is opened in an effect, on the
 * commit that drew the page. So the feed starts this download at that moment - or, on a landing
 * whose page draws a live document, once that document's first block is in the page, which is
 * long after the shell's first paint (`features/shell/content-drawn.ts`) - and every event it
 * receives is read once the download is in: the feed keeps the event in the log itself, at once,
 * and the correction an event marks and the refreshes it asks for are chained onto the one
 * promise of this download the feed holds, in the order the events arrived (`api/event-feed.ts`).
 *
 * What a reader loses by its being late: nothing of the page they landed on, and, for the
 * round trip this download takes, the refresh of a change another person makes in that moment -
 * which arrives as soon as the chunk does, because the event is held, not dropped.
 *
 * Under `renderChunk`: a reader in front of a page that stops refreshing is waiting on this
 * download as much as on a route's, so a failure spends the tab's recovery reload rather than
 * leaving the page stale for as long as it is open
 * (`features/shell/DeploymentResilience.tsx`). Where no reload can be spent, the feed refreshes
 * everything for the events that waited on the failed download and asks for it again with the
 * next event (`api/event-feed.ts`).
 */
export function loadEventRefresh(): Promise<EventRefreshMap> {
  return renderChunk(() => import("./corrected-post"));
}
