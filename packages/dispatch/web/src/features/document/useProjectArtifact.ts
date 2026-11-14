import type { UseQueryResult } from "../../api/cache/keys";
import { useShownQuery } from "../../api/cache/shown-query";
import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import type { ProjectDocumentRoute } from "../refs/routes";

/**
 * The project document a route names, by `project/slug`. The `["artifact-ref", …]` key is the
 * one the live socket's event feed invalidates when a document changes, so every caller shares
 * one cache entry.
 */
export function useProjectArtifact(
  route: ProjectDocumentRoute | undefined
): UseQueryResult<Artifact> {
  return useShownQuery({
    enabled: route !== undefined,
    queryKey: ["artifact-ref", `${route?.project}/${route?.slug}`],
    queryFn: () => {
      if (route === undefined) {
        throw new Error("Project document query requires a document route.");
      }
      return api.getProjectArtifact(route.project, route.slug);
    },
  });
}
