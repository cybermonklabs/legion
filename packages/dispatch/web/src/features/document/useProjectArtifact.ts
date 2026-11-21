import { type UseQueryResult, useQuery } from "@tanstack/react-query";

import { projectArtifactQuery } from "../../api/queries";
import type { Artifact } from "../../api/types";
import type { ProjectDocumentRoute } from "../refs/routes";

/**
 * The project document a route names, by `project/slug` (`projectArtifactQuery`). Every caller
 * shares that one cache entry, which the landing seeds and the live socket's event feed refreshes
 * when a document changes.
 */
export function useProjectArtifact(
  route: ProjectDocumentRoute | undefined
): UseQueryResult<Artifact> {
  return useQuery({ ...projectArtifactQuery(route), enabled: route !== undefined });
}
