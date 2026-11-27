import type { QueryClient } from "@tanstack/react-query";

import { everyRowDrawn, onRowsDrawnChange } from "./drawn-in-slices";

/**
 * The page's end of the load (the plan's state table "The page's end of the load",
 * `load-end.test.ts`), which starts the work that makes the block editor ready (`block-room.ts`).
 *
 * The page reads the end from what it can see: every query of its `QueryClient` settled, every row
 * of the drawn pages in the document (`drawn-in-slices.ts` has drawn its last slice), the fonts
 * loaded, and then one second in which no `longtask` entry and no `resource` entry ends. The work
 * starts 500 ms after that second (the plan's Departure 3), so it starts after the end the load
 * rows read (`waitForLoadEnd`, `e2e/load-instruments.ts`); an entry or a query in those 500 ms
 * restarts the wait as it would the second. The mark `block-editor-start` is set as it starts. An
 * `Enter`, or a click on a row's text, before then starts it at once (`startNow`), and a later end
 * starts nothing more: one work for each page. Where the browser observes no `longtask` entry, the
 * second runs on the resource entries and the queries alone.
 */

/** The second with no long task and no resource ending, after which the load has ended. */
export const QUIET_MS = 1_000;

/** How long after that second the work starts (Departure 3). */
export const AFTER_QUIET_MS = 500;

/** The mark set as the work starts. */
export const BLOCK_EDITOR_START_MARK = "block-editor-start";

/** The entry types whose end restarts the quiet second. */
export type QuietEntry = "longtask" | "resource";

export interface LoadEndOptions {
  /** The page's client: the quiet second runs only while none of its queries is fetching. */
  readonly queryClient: QueryClient;
  /** Whether every row of the drawn pages is in the document and the fonts are loaded. */
  readonly ready: () => boolean;
  /** Calls the listener each time `ready` may have changed; answers how to stop. */
  readonly subscribeReady: (listener: () => void) => () => void;
  /** The time now, on the clock the entries' ends are read on. */
  readonly now: () => number;
  /** Reports the end of each entry of `type` the browser observes from now on; answers how to
   *  stop, or nothing where the browser observes no entry of that type. */
  readonly observe: (type: QuietEntry, report: (end: number) => void) => (() => void) | undefined;
  readonly mark: (name: string) => void;
  /** Starts the work. */
  readonly start: () => void;
}

export interface LoadEnd {
  /** Starts the work at once, for an act before the load has ended; nothing after the first. */
  startNow(): void;
  /** Stops watching; nothing starts after it. */
  destroy(): void;
}

export function watchLoadEnd({
  mark,
  now,
  observe,
  queryClient,
  ready,
  start,
  subscribeReady,
}: LoadEndOptions): LoadEnd {
  let done = false;
  // Where the quiet second counts from while the load can be quiet, and the timer that ends the
  // wait it starts; none while a query fetches or the rows or the fonts are not in.
  let quietFrom: number | undefined;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const stops: (() => void)[] = [];

  const arm = () => {
    clearTimeout(timer);
    if (quietFrom === undefined) return;
    timer = setTimeout(ended, Math.max(0, quietFrom + QUIET_MS + AFTER_QUIET_MS - now()));
  };
  // The load can be quiet once every query has settled and the rows and the fonts are in; from
  // the moment it can, the second counts, and it stops the moment it cannot.
  const check = () => {
    if (done) return;
    const quiet = queryClient.isFetching() === 0 && ready();
    if (!quiet) {
      quietFrom = undefined;
      clearTimeout(timer);
      return;
    }
    if (quietFrom !== undefined) return;
    quietFrom = now();
    arm();
  };
  const entryEnded = (end: number) => {
    if (done || quietFrom === undefined) return;
    quietFrom = Math.max(quietFrom, end);
    arm();
  };
  const ended = () => {
    check();
    if (quietFrom === undefined) return;
    if (now() < quietFrom + QUIET_MS + AFTER_QUIET_MS) {
      // A timer can run a fraction of a millisecond before `now` reads its due time (measured in
      // Chromium: the wait then stood still for good, with the load quiet), so it waits on.
      arm();
      return;
    }
    startNow();
  };
  const stop = () => {
    done = true;
    clearTimeout(timer);
    for (const stopOne of stops.splice(0)) stopOne();
  };
  const startNow = () => {
    if (done) return;
    stop();
    mark(BLOCK_EDITOR_START_MARK);
    start();
  };

  stops.push(queryClient.getQueryCache().subscribe(check));
  stops.push(subscribeReady(check));
  for (const type of ["longtask", "resource"] as const) {
    const stopObserving = observe(type, entryEnded);
    if (stopObserving !== undefined) stops.push(stopObserving);
  }
  check();
  return { destroy: stop, startNow };
}

/** Whether the rows of the drawn pages are all in the document and the fonts are loaded: the
 *  page's `ready` for `watchLoadEnd`. A document with no font set (the unit tests' DOM) never
 *  reads loaded, so only an act starts the work there; every browser the page serves holds one. */
export function rowsAndFontsReady(): boolean {
  return everyRowDrawn() && document.fonts?.status === "loaded";
}

/** Calls `listener` as the rows or the fonts change: the page's `subscribeReady`. */
export function onRowsAndFontsChange(listener: () => void): () => void {
  const stopRows = onRowsDrawnChange(listener);
  const fonts = document.fonts;
  fonts?.addEventListener("loadingdone", listener);
  fonts?.addEventListener("loading", listener);
  // A font set that loads nothing never sends `loadingdone`; its `ready` settles all the same.
  void fonts?.ready.then(listener);
  return () => {
    stopRows();
    fonts?.removeEventListener("loadingdone", listener);
    fonts?.removeEventListener("loading", listener);
  };
}

/** Reports the end of each entry of `type` the browser records from now on: the page's
 *  `observe`. A browser that observes no entry of the type (WebKit, for `longtask`) reports
 *  none. */
export function observeEntryEnds(
  type: QuietEntry,
  report: (end: number) => void
): (() => void) | undefined {
  if (
    typeof PerformanceObserver === "undefined" ||
    !PerformanceObserver.supportedEntryTypes?.includes(type)
  ) {
    return undefined;
  }
  const observer = new PerformanceObserver((list) => {
    for (const entry of list.getEntries()) report(entry.startTime + entry.duration);
  });
  observer.observe({ type });
  return () => observer.disconnect();
}
