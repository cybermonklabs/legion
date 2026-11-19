import type { ReactNode } from "react";

import { issuePriority } from "../../issue/issue-priority";
import { stateForIssue } from "../../issue/pins";
import { useIssueDetail } from "../../issue/useIssueDetail";
import { useIssuePriority } from "../../issue/useIssuePriority";
import { useOpenDetailsAt } from "../details-rows";
import { IssueHeader } from "../IssueHeader";
import { NoTooltips } from "../NoTooltips";
import type { SlotProps } from "../slot-props";
import { useHeaderKeys } from "../useHeaderKeys";
import { useIssueHeaderWrites } from "../useIssueHeaderWrites";

/**
 * The header (`IssueHeader.tsx`): the key, the title and the one properties line, inside
 * `NoTooltips`, so each shared control in it draws the page's words and no tooltip.
 *
 * The primary document and the reader's state of the issue are the reads today's page makes
 * (`useIssueDetail`, `stateForIssue`). The header makes its own set of the issue's writes;
 * Details makes another, and each write goes in the issue's one line of the keyed store
 * (`issue:<key>`), so the two keep their order. The picker, the digits and the palette's Set
 * priority rows share one priority write, so a refusal is said once, here.
 */
export function HeaderSlot({ issue, route }: SlotProps): ReactNode {
  const { primaryArtifact, state } = useIssueDetail(route);
  const issueState = stateForIssue(state.data, issue.key);
  const writes = useIssueHeaderWrites(issue, issueState);
  const priorityWrite = useIssuePriority(issue.key, issuePriority(issue.priority));
  useHeaderKeys(priorityWrite);
  const openDetailsAt = useOpenDetailsAt();
  return (
    <NoTooltips>
      <IssueHeader
        documentArtifact={primaryArtifact}
        isClosed={issue.closed_at !== null}
        issue={issue}
        onOpenField={(field) => openDetailsAt(field, true)}
        onOpenLabels={() => openDetailsAt("labels")}
        priorityWrite={priorityWrite}
        state={issueState}
        writes={writes}
      />
    </NoTooltips>
  );
}
