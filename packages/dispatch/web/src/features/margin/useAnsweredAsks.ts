import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../../api/client";
import { useMarginDrawer } from "../issue-page/margin-slot";
import { EVENT_FRESH } from "../refs/reference-target";
import type { MarginOwner } from "./useMarginItems";

/**
 * Every anchored ask on the visible artifact. Document-owned asks arrive from the artifact
 * endpoint; issue-owned asks retain the single per-issue history query.
 */
export function useAnsweredAsks(owner: MarginOwner | undefined, artifactID: string | undefined) {
  // The drawer is the new issue page's alone, whose outline reads these asks under this key and
  // keeps them fresh by events (`useIssueTree.ts`): there a window focus 30 s after the read, the
  // app's staleness, would fetch them again for nothing. Every other page keeps the app's defaults.
  const drawer = useMarginDrawer();
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
    ...(drawer === undefined ? undefined : EVENT_FRESH),
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
