import type { ReactNode } from "react";

import type { LegionIssueResponse } from "../../api/types";
import { pageInkText, pageMutedText, pageQuietText } from "../../theme/classes";
import { DASHBOARD_WORDS } from "../issue-page/page-words";
import { legionRuns } from "./phase-track";
import { agentsAtWork } from "./role-words";

/**
 * Dashboard's fourth part (dispatch://INFRA-5702/spec#b-r-agents): for an issue Legion runs, each
 * role of its Legion state that holds a claim, in the order the state gives, with its claim state
 * in the role words. A `retired` claim is not listed, and an issue Legion does not run draws none
 * of it.
 */
export function Agents({ legion }: { legion: LegionIssueResponse | undefined }): ReactNode {
  if (!legionRuns(legion)) return null;
  const lines = agentsAtWork(legion.state.roles);
  return (
    <section aria-label={DASHBOARD_WORDS.parts.agents} className="space-y-2">
      <h2 className={`text-xs font-semibold tracking-wide uppercase ${pageQuietText}`}>
        {DASHBOARD_WORDS.parts.agents}
      </h2>
      <ul className="flex flex-wrap gap-x-5 gap-y-1 text-sm">
        {lines.map((line, index) => (
          // biome-ignore lint/suspicious/noArrayIndexKey: nothing holds a state to one line per role
          <li key={`${line.role}-${index}`}>
            <span className={`font-medium ${pageInkText}`}>{line.role}</span>{" "}
            <span className={pageMutedText}>{line.words}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}
