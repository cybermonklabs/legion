import type { ReactNode } from "react";

import { type CodeCardSpec, CodeEvalCard } from "./CodeEvalCard";
import type { EvalsScope, EvalsViewState } from "./useEvalsViews";

/** The code survival eval of dispatch://INFRA-3206, which its card draws by name. */
export const CODE_SURVIVAL = "code-survival-30d";

/** The code survival card: each group's share of the lines its changes added still on the
 *  default branch at each change's 30-day mark (`survival:<g>`, `fold.FoldSurvival`). */
const SURVIVAL: CodeCardSpec = {
  headline: "survival",
  kinds: [],
  lines: "alive",
  slug: CODE_SURVIVAL,
  title: "Code survival at 30 days",
};

/** The code survival card of Deploys and lead time (CodeEvalCard). */
export function CodeSurvivalCard({
  project,
  scope,
  view,
}: {
  project: string;
  scope: EvalsScope;
  view: EvalsViewState;
}): ReactNode {
  return <CodeEvalCard project={project} scope={scope} spec={SURVIVAL} view={view} />;
}
