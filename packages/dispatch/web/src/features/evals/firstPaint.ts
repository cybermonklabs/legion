import { type QueryClient, QueryObserver } from "@tanstack/react-query";

import {
  type EvalKind,
  evalsViewQuery,
  itemsViewQuery,
  legionReadViewQuery,
} from "../../api/queries";
import { compareRange, liveWindow, readCompare, readWindow } from "../delivery/lib/window";
import { JOB_LISTS, type JobListId, jobListView } from "./jobLists";

/**
 * The kinds whose evals views the Evals tab's first render opens: every kind, since the reads that
 * hide the Legion or the delivery and CI sections (the project's Legion daemon and its delivery
 * settings) have not answered yet (`EvalsTab`, `sectionsOf`).
 */
const FIRST_PAINT_KINDS: readonly EvalKind[] = ["delivery", "legion", "ci", "experiment"];

/**
 * The live views the Evals tab of `project` opens for its first paint at an address whose query is
 * `search`, at `now`: one evals view of each of FIRST_PAINT_KINDS over the header's window, its
 * compare range and the repositories the address names, none narrowed, since the first paint is
 * never narrowed (plan D25 step 2, `useTabScopes`), and the header's legion-read view (plan D23),
 * which its first render opens; and the CI section's two job lists' items views, which the lists
 * open only once the CI evals view has answered (`CiHealth`). They are the views `useEvalsScope`,
 * `useEvalsViews` and the job lists build from the same address, so the tab takes over their
 * subscriptions rather than opening its own.
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
    jobLists: JOB_LISTS.map(
      (id) => [id, itemsViewQuery(jobListView(id, project, scope.window, scope.repos))] as const
    ),
    legionRead: legionReadViewQuery(project),
  };
}

/** A route's hold of the views it opened for the tab: the observers of the views the tab's first
 *  render opens (`views`), until the tab has mounted, and each job list's (`jobLists`), until that
 *  list observes its view, which it does only once the CI evals view has answered. */
interface Hold {
  readonly views: (() => void)[];
  readonly jobLists: Map<JobListId, () => void>;
}

/** Each client's hold of the views a route opened for the tab, until the tab takes them over. */
const holds = new WeakMap<QueryClient, Hold>();

/** Stops each of `stops`, once. */
function letGo(stops: (() => void)[]): void {
  for (const stop of stops.splice(0)) stop();
}

/**
 * Opens the views the Evals tab's first paint reads (`firstPaintViews`) as its route matches,
 * before the page's code has loaded and before the tab draws, so their reads on Dispatch run while
 * the page loads (dispatch://INFRA-2918, T33: the tab's first subscribe came 225 ms after the
 * navigation, and the job lists' after the CI evals view's frame was drawn). Each is held by an
 * observer of its own: the views of the tab's first render until the tab has mounted
 * (`releaseFirstPaint`), whose own observers then hold the views it opened, so a view the tab
 * does not open after all is unsubscribed with the hold; each job list's until that list has
 * mounted (`releaseJobList`), or until the tab draws no CI section (`releaseJobLists`); and every
 * one until the returned release, which the route calls as it leaves. Where one of the views of the
 * tab's first render is observed already, the tab is mounted, and nothing is held.
 */
export function holdFirstPaint(
  queryClient: QueryClient,
  project: string,
  search: URLSearchParams,
  now: number
): () => void {
  const { evals, jobLists, legionRead } = firstPaintViews(project, search, now);
  const cache = queryClient.getQueryCache();
  const observed = [...evals.map((view) => view.queryKey), legionRead.queryKey].some(
    (queryKey) => (cache.find({ exact: true, queryKey })?.getObserversCount() ?? 0) > 0
  );
  if (observed) return () => {};
  const previous = holds.get(queryClient);
  if (previous !== undefined) {
    letGo(previous.views);
    releaseJobLists(queryClient);
  }
  const hold: Hold = {
    views: [
      ...evals.map((view) => new QueryObserver(queryClient, view).subscribe(() => {})),
      new QueryObserver(queryClient, legionRead).subscribe(() => {}),
    ],
    jobLists: new Map(
      jobLists.map(([id, view]) => [id, new QueryObserver(queryClient, view).subscribe(() => {})])
    ),
  };
  holds.set(queryClient, hold);
  return () => {
    if (holds.get(queryClient) !== hold) return;
    letGo(hold.views);
    releaseJobLists(queryClient);
    holds.delete(queryClient);
  };
}

/** Lets go of the views of the tab's first render a route opened for the Evals tab
 *  (`holdFirstPaint`), once the tab's own observers hold those it opened. */
export function releaseFirstPaint(queryClient: QueryClient): void {
  const hold = holds.get(queryClient);
  if (hold !== undefined) letGo(hold.views);
}

/** Lets go of the items view of the job list `id` a route opened for the Evals tab, once the
 *  list's own observer holds it. */
export function releaseJobList(queryClient: QueryClient, id: JobListId): void {
  const jobLists = holds.get(queryClient)?.jobLists;
  jobLists?.get(id)?.();
  jobLists?.delete(id);
}

/** Lets go of both job lists' items views a route opened for the Evals tab: the tab draws no CI
 *  section, so no list takes them over. */
export function releaseJobLists(queryClient: QueryClient): void {
  for (const id of JOB_LISTS) releaseJobList(queryClient, id);
}
