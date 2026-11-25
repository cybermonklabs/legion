import type { QueryFunctionContext } from "@tanstack/react-query";
import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../../api/client";
import type { AskWithTrees } from "../../api/types";
import { issueAskThreadsQuery } from "../issue-page/useIssueTree";
import { useNewIssuePageFreshness } from "../refs/reference-target";
import type { MarginOwner } from "./useMarginItems";

/**
 * Every anchored ask on the visible artifact. Document-owned asks arrive from the artifact
 * endpoint; issue-owned asks retain the single per-issue history query.
 */
export function useAnsweredAsks(owner: MarginOwner | undefined, artifactID: string | undefined) {
  const freshness = useNewIssuePageFreshness();
  // On the new issue page, watch the same ask threads the outline reads.
  const threads =
    freshness !== undefined && owner?.kind === "issue"
      ? issueAskThreadsQuery(owner.key)
      : undefined;
  const readThreads = threads?.queryFn;
  const query = useQuery({
    enabled: owner !== undefined,
    queryFn: () => {
      if (owner === undefined) {
        throw new Error("Answered asks require a margin owner.");
      }
      return owner.kind === "document"
        ? api.listArtifactAsks(owner.artifactId, "all")
        : api.listIssueAsks(owner.key, "all");
    },
    queryKey:
      owner?.kind === "document"
        ? ["artifact", owner.artifactId, "asks"]
        : ["asks", owner?.kind === "issue" ? owner.key : undefined],
    ...freshness,
    ...(threads !== undefined && typeof readThreads === "function"
      ? {
          queryKey: threads.queryKey,
          queryFn: (context: QueryFunctionContext): AskWithTrees[] | Promise<AskWithTrees[]> =>
            readThreads({ ...context, queryKey: threads.queryKey }),
        }
      : {}),
  });
  const asks = useMemo(
    () =>
      owner?.kind === "document"
        ? (query.data ?? [])
        : (query.data ?? []).filter((ask) => ask.anchor?.artifact_id === artifactID),
    [artifactID, owner?.kind, query.data]
  );

  return {
    /** Every ask of the owner, unfiltered: what the Pinned tab resolves a pinned ask from. */
    all: query.data ?? [],
    asks,
    error: query.isError ? query : undefined,
    pending: query.isPending,
  };
}
