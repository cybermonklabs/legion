import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import { buildProjectPath } from "../refs/routes";
import { LegionIssueTimeline } from "./LegionIssueTimeline";
import { LegionSections } from "./LegionSections";

export function EvalsTab({
  issue,
  project,
}: {
  readonly project: string;
  readonly issue?: string;
}): ReactNode {
  const navigate = useNavigate();

  return (
    <div className="flex flex-col gap-3" data-evals-tab>
      {issue === undefined ? null : (
        <LegionIssueTimeline
          issueKey={issue}
          onClose={() => navigate(buildProjectPath({ kind: "evals", project }))}
          onOpenIssue={(key) => navigate(buildProjectPath({ kind: "evals", project, issue: key }))}
        />
      )}
      <LegionSections project={project} />
    </div>
  );
}
