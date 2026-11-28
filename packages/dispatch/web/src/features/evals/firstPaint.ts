import { type QueryClient, QueryObserver } from "@tanstack/react-query";

import { type EvalKind, evalsViewQuery, legionReadViewQuery } from "../../api/queries";
import { compareRange, liveWindow, readCompare, readWindow } from "../delivery/lib/window";

/**
 * The kinds whose evals views the Evals tab's first render opens: every kind, since the reads that
 * hide the Legion or the delivery and CI sections (the project's Legion daemon and its delivery
 * settings) have not answered yet (`EvalsTab`, `sectionsOf`).
 */
const FIRST_PAINT_KINDS: readonly EvalKind[] = ["delivery", "legion", "ci", "experiment"];

/**
 * The live views the Evals tab of `project` opens on its first render at an address whose query is
 * `search`, at `now`: one evals view of each of FIRST_PAINT_KINDS over the header's window, its
 * compare range and the repositories the address names, none narrowed, since the first paint is
 * never narrowed (plan D25 step 2, `useTabScopes`), and the header's legion-read view (plan D23).
 * They are the views `useEvalsScope` and `useEvalsViews` build from the same address, so the tab
 * takes over their subscriptions rather than opening its own.
 */
function firstPaintViews(project: string, search: URLSearchParams, now: number) {
  const choice = readWindow(search);
  const scope = {
    project,
    window: liveWindow(choice),
    compare: compareRange(readCompare(search), choice, now),
    repos: [...new Set(search.getAll("repo").filter((name) => name !== ""))],
    narrowing: "",
  };
  return {
    evals: FIRST_PAINT_KINDS.map((kind) => evalsViewQuery({ ...scope, kind, offset: 0 })),
    legionRead: legionReadViewQuery(project),
  };
}

/** Each client's hold of the views a route opened for the tab, until the tab takes them over. */
const holds = new WeakMap<QueryClient, () => void>();

/**
 * Opens the views the Evals tab's first render opens (`firstPaintViews`) as its route matches,
 * before the page's code has loaded and before the tab draws, so their reads on Dispatch run
 * while the page loads (dispatch://INFRA-2918, T33: the tab's first subscribe came 225 ms after
 * the navigation). Each is held by an observer of its own until the tab has mounted
 * (`releaseFirstPaint`), whose own observers then hold the views it opened, so a view the tab does
 * not open after all is unsubscribed with the hold; or until the returned release, which the
 * route calls as it leaves. Where one of the views is observed already, the tab is mounted, and
 * nothing is held.
 */
export function holdFirstPaint(
  queryClient: QueryClient,
  project: string,
  search: URLSearchParams,
  now: number
): () => void {
  const { evals, legionRead } = firstPaintViews(project, search, now);
  const cache = queryClient.getQueryCache();
  const observed = [...evals.map((view) => view.queryKey), legionRead.queryKey].some(
    (queryKey) => (cache.find({ exact: true, queryKey })?.getObserversCount() ?? 0) > 0
  );
  if (observed) return () => {};
  holds.get(queryClient)?.();
  const stops = [
    ...evals.map((view) => new QueryObserver(queryClient, view).subscribe(() => {})),
    new QueryObserver(queryClient, legionRead).subscribe(() => {}),
  ];
  let held = true;
  const release = () => {
    if (!held) return;
    held = false;
    if (holds.get(queryClient) === release) holds.delete(queryClient);
    for (const stop of stops) stop();
  };
  holds.set(queryClient, release);
  return release;
}

/** Lets go of the views a route opened for the Evals tab (`holdFirstPaint`), once the tab's own
 *  observers hold those it opened. */
export function releaseFirstPaint(queryClient: QueryClient): void {
  holds.get(queryClient)?.();
}
