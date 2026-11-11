import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Navigate } from "react-router-dom";

import {
  deliverySettingsQuery,
  legionSettingsQuery,
  projectsQuery,
  repoProjectsQuery,
} from "../../api/queries";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { buildProjectPath } from "../refs/routes";

export function HealthRedirect(): ReactNode {
  const projects = useQuery(projectsQuery());
  const mappings = useQuery(repoProjectsQuery());
  const delivery = useQuery(deliverySettingsQuery());
  const legion = useQuery(legionSettingsQuery());
  if (projects.isPending || mappings.isPending || delivery.isPending || legion.isPending) {
    return <LoadingSkeleton label="Opening the project" />;
  }
  const deployRepo = delivery.data?.deploy_repo;
  const project =
    mappings.data?.find((mapping) => mapping.repo.toLowerCase() === deployRepo)?.project ??
    legion.data?.[0]?.project ??
    projects.data?.[0]?.key;
  const hasEvals = legion.data?.some((setting) => setting.project === project) ?? false;
  return (
    <Navigate
      replace
      to={
        project === undefined
          ? "/"
          : buildProjectPath({ kind: hasEvals ? "evals" : "issues", project })
      }
    />
  );
}
