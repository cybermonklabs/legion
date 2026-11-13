import type { IssueTab } from "../refs/routes";

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
 * Each import is named once, here, so that the panel's `lazy` in `IssuePage.tsx` and the entry's
 * start of the same chunk (`app.tsx`, `preloadRouteChunk`) cannot ask for two different chunks.
 */
export const loadConversationTab = () =>
  import("../conversation/ConversationTab").then((module) => ({
    default: module.ConversationTab,
  }));
export const loadActivityTab = () =>
  import("../activity/ActivityTab").then((module) => ({ default: module.ActivityTab }));
export const loadChildrenTab = () =>
  import("./ChildrenTab").then((module) => ({ default: module.ChildrenTab }));
export const loadDecisionsTab = () =>
  import("./DecisionsPanel").then((module) => ({ default: module.DecisionsPanel }));
export const loadCollaboratorsTab = () =>
  import("./CollaboratorsTab").then((module) => ({ default: module.CollaboratorsTab }));
export const loadVerificationTab = () =>
  import("../verification/VerificationTab").then((module) => ({
    default: module.VerificationTab,
  }));
export const loadPlanTab = () =>
  import("../plan/PlanTab").then((module) => ({ default: module.PlanTab }));
export const loadArtifactsTab = () =>
  import("../artifacts/ArtifactRoutePanel").then((module) => ({
    default: module.ArtifactRoutePanel,
  }));

/** The chunk of the panel `tab` draws, or `undefined` for the Spec, whose panel is in the issue
 *  page's own chunk: it is the tab a bare `/issues/<key>` opens on. */
export function issueTabChunk(tab: IssueTab): (() => Promise<unknown>) | undefined {
  switch (tab) {
    case "spec":
      return undefined;
    case "conversation":
      return loadConversationTab;
    case "activity":
      return loadActivityTab;
    case "children":
      return loadChildrenTab;
    case "decisions":
      return loadDecisionsTab;
    case "collaborators":
      return loadCollaboratorsTab;
    case "verification":
      return loadVerificationTab;
    case "plan":
      return loadPlanTab;
    case "artifacts":
      return loadArtifactsTab;
  }
}
