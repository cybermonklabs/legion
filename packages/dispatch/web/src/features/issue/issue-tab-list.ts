import type { TabDefinition } from "../../components/Tabs";
import type { IssueTab } from "../refs/routes";

/** The issue tablist's tabs, in the order a reader meets them, and nothing that renders them.
 *  The box the issue page keeps for the tablist stands in for it and so needs this list - the
 *  labels, and which tab carries a count, since a count is what the strip's height is
 *  (`issue-page-skeleton.tsx`) - but that box is in the shell every page loads: were this list
 *  in `IssueTabs.tsx`, the `Tabs` component and the `Pill` it draws counts with would load with
 *  the shell to supply nine strings. Everything here is data and types, so the shell takes the
 *  list alone and the tablist itself stays in the issue page's own chunk. */
export function issueTabs(artifactCount: number): readonly TabDefinition<IssueTab>[] {
  return [
    { id: "spec", label: "Spec" },
    { id: "verification", label: "Verification" },
    { id: "plan", label: "Plan" },
    { id: "conversation", label: "Conversation" },
    { id: "children", label: "Children" },
    artifactCount === 0
      ? { id: "artifacts", label: "Artifacts" }
      : { id: "artifacts", label: "Artifacts", count: artifactCount },
    { id: "activity", label: "Activity" },
    { id: "decisions", label: "Decisions" },
    { id: "collaborators", label: "Collaborators" },
  ];
}

/**
 * The chunk each tab's panel lives in, by the tab it draws. Two readers ask for these: the page
 * itself, which draws each panel in the commit its chunk arrives in (`IssuePage.tsx`), and the
 * shell, which asks for the panel an address names as the entry module evaluates (`app.tsx`'s
 * `requestTheOpenPagesChunk`). They were two lists of the same seven modules, so a tab added to
 * the page silently lost its early request (dispatch://INFRA-341).
 *
 * The Spec and Artifacts tabs are absent: both draw the document surface, whose chunk the page
 * preloads with its own (`DOCUMENT_ADDRESS_MODULES` in `web/vite.config.ts`).
 *
 * An `import()` here adds no static edge, so the shell still takes the list alone: these are
 * requests for chunks, not imports of them.
 */
export const issueTabChunks = {
  activity: () => import("../activity/ActivityTab"),
  children: () => import("./ChildrenTab"),
  collaborators: () => import("./CollaboratorsTab"),
  conversation: () => import("../conversation/ConversationTab"),
  decisions: () => import("./DecisionsPanel"),
  plan: () => import("../plan/PlanTab"),
  verification: () => import("../verification/VerificationTab"),
} satisfies Partial<Record<IssueTab, () => Promise<unknown>>>;
