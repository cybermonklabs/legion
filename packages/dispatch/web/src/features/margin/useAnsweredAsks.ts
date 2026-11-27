import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { ownerAsksQuery } from "../../api/queries";
import { useNewIssuePageFreshness } from "../refs/reference-target";
import type { MarginOwner } from "./useMarginItems";

/**
 * Every anchored ask on the visible artifact. Document-owned asks arrive from the artifact
 * endpoint; issue-owned asks retain the single per-issue history query.
 */
export function useAnsweredAsks(owner: MarginOwner | undefined, artifactID: string | undefined) {
  const freshness = useNewIssuePageFreshness();
  const query = useQuery({
    ...ownerAsksQuery(
      owner === undefined
        ? undefined
        : owner.kind === "document"
          ? { artifact: owner.artifactId }
          : { issue: owner.key }
    ),
    enabled: owner !== undefined,
    ...freshness,
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
