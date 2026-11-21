import { queryOptions } from "@tanstack/react-query";

import { api } from "../../api/client";

/** The live document's text as the server reads it now. A read of it decides whether the editor
 * may connect: the server answers `409 DOC_SCHEMA` for a stored tree outside the Proof schema
 * (`ProofDocument`). A module of its own, so a surface that reads the cached text - a margin
 * compose's quote (`useQuoteInDocument`) - does not take in the whole document page. */
export const liveTextQueryOptions = (artifactId: string) =>
  queryOptions({
    queryKey: ["artifact", artifactId, "text"],
    queryFn: () => api.getArtifactText(artifactId),
  });
