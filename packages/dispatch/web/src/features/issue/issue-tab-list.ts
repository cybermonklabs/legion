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
