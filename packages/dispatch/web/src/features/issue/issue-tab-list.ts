import type { TabDefinition } from "../../components/Tabs";
import type { IssueTab } from "../refs/routes";

/** The issue tablist's tabs, in the order a reader meets them, and nothing that renders them.
 *  The loading skeleton in `app.tsx` stands in for the tablist and so needs these labels, but it
 *  is in the shell every page loads: were this list in `IssueTabs.tsx`, the `Tabs` component and
 *  the `Pill` it draws counts with would load with the shell to supply nine strings. Everything
 *  here is data and types, so the shell takes the list alone and the tablist itself stays in the
 *  issue page's own chunk. */
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

/** The tablist's labels in order, for the loading skeleton that stands in for this tablist
 *  (`app.tsx`). It stood on a hand-written copy of this list until that copy had drifted by
 *  three tabs, so it reads the list itself; the count a label can carry is a number the skeleton
 *  has no issue to take, and it stands in for the plain label. */
export const ISSUE_TAB_LABELS: readonly string[] = issueTabs(0).map((tab) => tab.label);
