// Every project's Legion daemon, read one project at a time. No route lists them: R26 of
// dispatch://INFRA-979's spec allows one cross-project read, and that one is not this. So the
// project list is read and then each project's `GET /api/v1/projects/{key}/legion/daemon`, each
// its own cache entry (`legionDaemonQuery`), so a write to one project's daemon changes that entry
// alone. A project with no daemon answers `null` and is simply not among the daemons.
import { useQueries, useQuery } from "@tanstack/react-query";

import { legionDaemonQuery, projectsQuery } from "../../../api/queries";
import type { LegionSetting, Project } from "../../../api/types";

/** The daemons of every project, and the state of the reads behind them. `daemons` and
 *  `projects` are set once every read has answered, `daemons` by project key, as a list of
 *  settings was ordered. */
export interface LegionDaemons {
  readonly daemons: readonly LegionSetting[] | undefined;
  readonly projects: readonly Project[] | undefined;
  /** The project list or any one project's daemon is still being read for the first time. */
  readonly isPending: boolean;
  /** The project list or at least one project's daemon could not be read. */
  readonly isError: boolean;
  readonly isFetching: boolean;
  /** Reads again what failed: the project list, or each daemon whose read failed. */
  readonly retry: () => void;
}

export function useLegionDaemons(): LegionDaemons {
  const projects = useQuery(projectsQuery());
  const keys = projects.data?.map((project) => project.key) ?? [];
  const reads = useQueries({
    queries: keys.map((key) => legionDaemonQuery(key)),
    combine: (results) => ({
      daemons: results.every((result) => result.isSuccess)
        ? results
            .flatMap((result) => (result.data == null ? [] : [result.data]))
            .sort((a, b) => (a.project < b.project ? -1 : a.project > b.project ? 1 : 0))
        : undefined,
      isError: results.some((result) => result.isError),
      isFetching: results.some((result) => result.isFetching),
      isPending: results.some((result) => result.isPending),
      retryFailed: () => {
        for (const result of results) if (result.isError) void result.refetch();
      },
    }),
  });
  const ready = projects.isSuccess && reads.daemons !== undefined;
  return {
    daemons: ready ? reads.daemons : undefined,
    isError: projects.isError || reads.isError,
    isFetching: projects.isFetching || reads.isFetching,
    isPending: projects.isPending || reads.isPending,
    projects: ready ? projects.data : undefined,
    retry: () => {
      if (projects.isError) {
        void projects.refetch();
        return;
      }
      reads.retryFailed();
    },
  };
}
