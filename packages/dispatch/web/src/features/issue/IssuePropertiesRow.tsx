import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { projectsQuery } from "../../api/queries";
import type { Artifact, IssueDetails } from "../../api/types";
import {
  bgTransparent,
  borderDefault,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  statusPill,
  statusPillDot,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { issueStatuses, openIssueStatuses, statusLabel } from "../project/board-model";
import { ApprovalSteps } from "./ApprovalSteps";
import { AssigneeControl } from "./AssigneeControl";
import { gatedStepEntries } from "./gated-steps";
import { IssueAddMenu } from "./IssueAddMenu";
import type { IssueField } from "./IssueFieldForms";
import { IssueLabels } from "./IssueLabels";
import { PriorityEditor } from "./PriorityControl";
import { ProgressChips } from "./ProgressChips";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";
import type { IssuePriorityWrite } from "./useIssuePriority";

/**
 * Row three of the identity block: how the issue stands and who it is for - the status, the
 * priority, the assignee and its labels - with the one add control for a property it has no
 * value for, and the waiting marker at the right.
 *
 * The row wraps rather than scrolling: every control is visible at 390 px as a second line, and
 * nothing here is reached by dragging a row sideways (R11, R17).
 */
export function IssuePropertiesRow({
  documentArtifact,
  isClosed,
  issue,
  onOpenField,
  priorityWrite,
  writes,
}: {
  documentArtifact: Artifact | undefined;
  isClosed: boolean;
  issue: IssueDetails;
  onOpenField: (field: IssueField) => void;
  priorityWrite: IssuePriorityWrite;
  writes: IssueHeaderWrites;
}): ReactNode {
  const saving = writes.updateIssue.isPending;
  const selectableStatuses = isClosed ? issueStatuses : openIssueStatuses;
  const openAsks = issue.open_asks.filter((ask) => ask.state === "open");
  // The detail carries waiting_on on every open ask; a response without it cannot say whose
  // turn it is, so the marker stays off rather than shown wrong.
  const knowsWhoseTurn = openAsks.every((ask) => ask.waiting_on !== undefined);
  const waitingForHuman = waitingOnHuman(openAsks);
  // The marker's words are the page's own; the count beside them is the issue's data, so it is
  // drawn as a value rather than as a word the page declares.
  const whoseTurn =
    openAsks.length === 0 || !knowsWhoseTurn
      ? null
      : waitingForHuman.length > 0
        ? { count: waitingForHuman.length, label: PROPERTY_LABELS.waiting_on_you }
        : {
            count: openAsks.length - waitingForHuman.length,
            label: PROPERTY_LABELS.waiting_on_agents,
          };
  const approvalRequestOpen = openAsks.some((ask) => ask.kind === "approval");
  // The one shared read of the projects, under the key the shell's sidebar and the search
  // palette already hold, so the daemon's `project.updated` publish refreshes these steps with
  // no reload and the row makes no second, per-issue read. While that read is pending or
  // failed there is no row, so the steps are empty and this row draws its one chip below
  // rather than claiming a gate it has not read. Only a root issue has steps at all
  // (`gatedStepEntries`), so a child issue does not ask for the list: a disabled query still
  // reads whatever the shared cache holds, and a child draws the single chip either way.
  const projects = useQuery({ ...projectsQuery(), enabled: issue.parent === null });
  const projectRow = projects.data?.find((candidate) => candidate.key === issue.project);
  const entries = gatedStepEntries(projectRow, issue);
  // Which document a step reviews is `gated-steps.ts`'s to say, so the chip asks the drawn
  // entries rather than the gate's words: a gate that names no step for the primary document
  // leaves that document's own state and its two controls to the chip.
  const stepHoldsPrimaryDocument = entries.some(
    (entry) => entry.document?.id === issue.primary_artifact_id
  );
  const showApprovalActions =
    documentArtifact?.approval !== undefined &&
    (documentArtifact.approval.state !== "draft" || approvalRequestOpen);

  return (
    <div
      className="flex w-full min-w-0 flex-wrap items-center gap-x-2 gap-y-1"
      data-testid="issue-state-actions"
    >
      {/* The status pill may shrink: at 390 px its widest option ("Needs review") would
          otherwise hold a line of its own and push the priority and the assignee onto a third,
          which is 44 px of the head the first open question needs (dispatch://INFRA-1196 R6). */}
      <label
        className={`flex min-h-11 min-w-0 items-center gap-2 rounded-full border px-2 text-sm font-semibold uppercase tracking-wide sm:px-3 md:min-h-8 xl:px-2 ${borderDefault} ${statusPill.bg} ${statusPill.text}`}
      >
        <span
          aria-hidden="true"
          className={`hidden h-2 w-2 rounded-full sm:block ${statusPillDot}`}
        />
        <select
          aria-label={PROPERTY_LABELS.status}
          className={`min-w-0 appearance-none font-inherit outline-none disabled:cursor-not-allowed disabled:opacity-50 ${bgTransparent} ${statusPill.text}`}
          disabled={isClosed || saving}
          onChange={(event) => writes.drafts.requestStatusSubmit(event.target.value)}
          value={writes.pendingStatus ?? issue.status}
        >
          {selectableStatuses.map((status) => (
            <option key={status} value={status}>
              {statusLabel(status)}
            </option>
          ))}
        </select>
      </label>
      <PriorityEditor
        disabled={isClosed || saving}
        issueKey={issue.key}
        priority={issue.priority}
        write={priorityWrite}
      />
      <AssigneeControl
        assignee={issue.assignee}
        disabled={isClosed || saving}
        issueKey={issue.key}
      />
      <IssueLabels
        disabled={isClosed}
        labels={issue.labels}
        onSave={(labels) => writes.labels.mutateAsync(labels)}
        project={issue.project}
        saving={writes.labels.isPending}
        variant="rail"
      />
      <IssueAddMenu isClosed={isClosed} issue={issue} onOpenField={onOpenField} />
      {/* How far the work has got, like the status: the children section draws the same counts
          as a bar, and the task count stays here. */}
      <ProgressChips bar progress={issue.progress} />
      {/* A root issue of a gated project reads its gate step by step, after the chip's
          place; the chip itself stays beside those steps whenever no step drawn reviews the
          primary document, since otherwise an open request on that document could not be
          answered on this page at all. Every other issue - and any issue whose project is
          still unread - keeps that chip for the primary document alone, unchanged. */}
      {documentArtifact === undefined || stepHoldsPrimaryDocument ? null : (
        <ApprovalChip
          artifact={documentArtifact}
          layout="contents"
          showActions={showApprovalActions}
          variant="header"
        />
      )}
      {entries.length === 0 ? null : (
        <ApprovalSteps entries={entries} primaryArtifactId={issue.primary_artifact_id} />
      )}
      {whoseTurn === null ? null : (
        <span
          className={`ml-auto inline-flex shrink-0 items-center rounded-full border px-3 py-1 text-xs font-semibold ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
          data-testid="issue-whose-turn"
          {...pageChrome}
        >
          {whoseTurn.label} (<span {...authorText}>{whoseTurn.count}</span>)
        </span>
      )}
    </div>
  );
}
