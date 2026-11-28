import type { ItemsViewParams } from "../../api/queries";
import type { DeliveryLiveWindow } from "../../api/types";

/** The eval whose job rows the CI section's two job lists read (`CiHealth`). */
const PR_CI = "pr-ci";

/** How many of the slowest jobs the list holds, as the Pipeline page's did
 *  (measures.SlowestJobs). */
export const SLOWEST_JOBS = 5;

/** As many jobs as one items page holds: every job that failed in the 24 hours. */
const FAILED_JOBS_LIMIT = 200;

/** The CI section's two job lists: pr-ci's slowest jobs, and the jobs that failed in the 24 hours
 *  before the window ends. */
export type JobListId = "slowest-jobs" | "failing-jobs";

export const JOB_LISTS: readonly JobListId[] = ["slowest-jobs", "failing-jobs"];

/** The items view one job list reads over a project's window and repositories: pr-ci's job rows,
 *  slowest first or most failures first, with no compare range. The list (`CiHealth`) and the
 *  Evals tab's route, which opens it as the route matches (`holdFirstPaint`), both build it here,
 *  so the list takes over the route's subscription. */
export function jobListView(
  id: JobListId,
  project: string,
  window: DeliveryLiveWindow,
  repos: readonly string[]
): ItemsViewParams {
  const kept =
    id === "slowest-jobs"
      ? ({ limit: SLOWEST_JOBS, sort: "median" } as const)
      : ({ limit: FAILED_JOBS_LIMIT, sort: "failures" } as const);
  return { compare: null, project, repos, slug: PR_CI, window, ...kept };
}
