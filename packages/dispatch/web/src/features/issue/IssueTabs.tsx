import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import { Tabs } from "../../components/Tabs";
import { buildIssuePath, type IssueTab } from "../refs/routes";
import { issueTabs } from "./issue-tab-list";

/** The IssuePage tablist keeps its active view in the canonical issue route. `artifactCount` is
 *  how many rows the Artifacts tab lists - every artifact the issue has, its primary document
 *  included - so the badge and the list a reader checks it against say the same number. */
export function IssueTabs({
  activeTab,
  artifactCount,
  issueKey,
  onBeforeTabChange,
  toolbar,
}: {
  activeTab: IssueTab;
  artifactCount: number;
  issueKey: string;
  onBeforeTabChange: (current: IssueTab) => void;
  toolbar?: ReactNode;
}): ReactNode {
  const navigate = useNavigate();

  return (
    <div data-testid="issue-tabs">
      <Tabs
        activeTab={activeTab}
        ariaLabel="Issue detail"
        compact
        idPrefix="issue"
        onBeforeTabChange={onBeforeTabChange}
        onSelect={(tab) => navigate(buildIssuePath({ key: issueKey, kind: tab }))}
        tabs={issueTabs(artifactCount)}
        trailing={toolbar}
      />
    </div>
  );
}
