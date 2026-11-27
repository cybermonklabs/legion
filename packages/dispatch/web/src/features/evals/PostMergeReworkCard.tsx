import type { ReactNode } from "react";

import { type CodeCardSpec, CodeEvalCard } from "./CodeEvalCard";
import type { EvalsScope, EvalsViewState } from "./useEvalsViews";

/** The post-merge rework eval of dispatch://INFRA-3206, which its card draws by name. */
export const POST_MERGE_REWORK = "post-merge-rework-30d";

/** The post-merge rework card: each group's share of the lines its changes added that a later
 *  change of the same group for a different issue removed within 30 days (`rework:<g>`,
 *  `fold.FoldRework`), with the shares the other three kinds of removal took beside it. Named for
 *  what it counts, and never "Rework share", the title-match number beside it (spec R14). */
const REWORK: CodeCardSpec = {
  headline: "rework",
  kinds: [
    { base: "iteration", label: "iteration" },
    { base: "noissue", label: "no issue" },
    { base: "other", label: "other" },
  ],
  lines: "removed",
  slug: POST_MERGE_REWORK,
  title: "Post-merge rework at 30 days",
};

/** The post-merge rework card of Deploys and lead time (CodeEvalCard). */
export function PostMergeReworkCard({
  project,
  scope,
  view,
}: {
  project: string;
  scope: EvalsScope;
  view: EvalsViewState;
}): ReactNode {
  return <CodeEvalCard project={project} scope={scope} spec={REWORK} view={view} />;
}
