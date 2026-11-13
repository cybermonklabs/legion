import type { IssueTab } from "../refs/routes";
import { chunk } from "../shell/chunk-component";

/**
 * The issue page's tab panels other than the Spec, each a chunk of its own, fetched when the
 * reader first opens that tab.
 *
 * WHY. `/issues/<key>` opens on the Spec, and a panel that is not open is not mounted
 * (`IssuePage.tsx`, `activatedTabs`). But a static import puts the panel's code in the issue
 * page's chunk all the same, and Vite preloads every chunk that chunk imports. Before this
 * split, the Conversation alone pulled the ask card, the comment composer, the thread card and
 * the message cards into what a cold Spec landing fetched before its first block drew: 60
 * chunks, 2,065,150 bytes, in the reading of dispatch://INFRA-1109's R13 at its head 90ee14dd.
 * None of that code draws the Spec.
 *
 * Each chunk is named once, here, so that the panel in `IssuePage.tsx` and the entry's start of
 * the same chunk (`app.tsx`, `preloadRouteChunk`) share one download. The panel then draws in
 * the issue page's own commit when the entry's download is already in
 * (`features/shell/chunk-component.tsx` says why that matters).
 */
export const conversationTabChunk = chunk(() =>
  import("../conversation/ConversationTab").then((module) => module.ConversationTab)
);
export const activityTabChunk = chunk(() =>
  import("../activity/ActivityTab").then((module) => module.ActivityTab)
);
export const childrenTabChunk = chunk(() =>
  import("./ChildrenTab").then((module) => module.ChildrenTab)
);
export const decisionsTabChunk = chunk(() =>
  import("./DecisionsPanel").then((module) => module.DecisionsPanel)
);
export const collaboratorsTabChunk = chunk(() =>
  import("./CollaboratorsTab").then((module) => module.CollaboratorsTab)
);
export const verificationTabChunk = chunk(() =>
  import("../verification/VerificationTab").then((module) => module.VerificationTab)
);
export const planTabChunk = chunk(() => import("../plan/PlanTab").then((module) => module.PlanTab));
export const artifactsTabChunk = chunk(() =>
  import("../artifacts/ArtifactRoutePanel").then((module) => module.ArtifactRoutePanel)
);

/** The chunk of the panel `tab` draws, or `undefined` for the Spec, whose panel is in the issue
 *  page's own chunk: it is the tab a bare `/issues/<key>` opens on. */
export function issueTabChunk(tab: IssueTab): (() => Promise<unknown>) | undefined {
  switch (tab) {
    case "spec":
      return undefined;
    case "conversation":
      return conversationTabChunk.load;
    case "activity":
      return activityTabChunk.load;
    case "children":
      return childrenTabChunk.load;
    case "decisions":
      return decisionsTabChunk.load;
    case "collaborators":
      return collaboratorsTabChunk.load;
    case "verification":
      return verificationTabChunk.load;
    case "plan":
      return planTabChunk.load;
    case "artifacts":
      return artifactsTabChunk.load;
  }
}
