import { useQueries, useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, Navigate, useLocation } from "react-router-dom";

import { legionDaemonQuery, projectDeliverySettingsQuery, projectsQuery } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { linkText, textMutedOnCanvas } from "../../theme/classes";
import { buildProjectPath } from "../refs/routes";
import { useDocumentTitle } from "../shell/useDocumentTitle";

/**
 * `/delivery` and `/pipeline`, the pages the Evals tab replaced (dispatch://INFRA-2918, R1): the
 * default project's tab, the address replaced and its query kept, so an old link opens the same
 * window and filters there. The default project is the first, in the sidebar's order (the project
 * list as the sidebar reads it), whose tab has a source: a Legion daemon, or delivery on, which
 * Dispatch saves only with a deploy source; with none, the first project (the plan's decision 4).
 * Each project's daemon and delivery settings are read in parallel, and the choice waits only for
 * the projects before the first that has one. A read that failed is shown with Retry, never passed
 * over: passing it would send the reader to a project that is not the default.
 */
export function DefaultProjectRedirect(): ReactNode {
  useDocumentTitle("Evals · Dispatch");
  const location = useLocation();
  const projects = useQuery(projectsQuery());
  const daemons = useQueries({
    queries: (projects.data ?? []).map((project) => legionDaemonQuery(project.key)),
  });
  const deliveries = useQueries({
    queries: (projects.data ?? []).map((project) => projectDeliverySettingsQuery(project.key)),
  });
  if (projects.isPending) {
    return <p className={textMutedOnCanvas}>Loading projects…</p>;
  }
  if (projects.isError) {
    return (
      <QueryError
        message="Could not load the projects."
        onRetry={() => void projects.refetch()}
        retrying={projects.isFetching}
      />
    );
  }
  const first = projects.data[0];
  if (first === undefined) {
    return (
      <p className={textMutedOnCanvas}>
        No project has numbers yet. Create one in{" "}
        <Link className={linkText} to="/settings">
          Settings
        </Link>
        .
      </p>
    );
  }
  let chosen = first.key;
  for (const [index, project] of projects.data.entries()) {
    const daemon = daemons[index];
    const delivery = deliveries[index];
    // A source that either read answers is enough, whether or not the other has answered.
    const hasDaemon = daemon?.data !== undefined && daemon.data !== null;
    if (hasDaemon || delivery?.data?.delivery === true) {
      chosen = project.key;
      break;
    }
    if (daemon === undefined || delivery === undefined || daemon.isPending || delivery.isPending) {
      return <p className={textMutedOnCanvas}>Loading projects…</p>;
    }
    if (daemon.isError) {
      return (
        <QueryError
          message={`Could not read whether ${project.key} has a Legion daemon.`}
          onRetry={() => void daemon.refetch()}
          retrying={daemon.isFetching}
        />
      );
    }
    if (delivery.isError) {
      return (
        <QueryError
          message={`Could not read whether ${project.key} has delivery on.`}
          onRetry={() => void delivery.refetch()}
          retrying={delivery.isFetching}
        />
      );
    }
  }
  return (
    <Navigate
      replace
      to={{
        pathname: buildProjectPath({ kind: "evals", project: chosen }),
        search: location.search,
      }}
    />
  );
}
