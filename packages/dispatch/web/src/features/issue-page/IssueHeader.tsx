import { type ReactNode, useRef } from "react";

import type { Artifact, IssueDetails, UserIssueState } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  pageFailedText,
  pageInkText,
  pageMutedText,
  pagePaperBg,
} from "../../theme/classes";
import type { IssuePriorityWrite } from "../issue/useIssuePriority";
import type { IssueAction } from "./IssueActionsMenu";
import type { IssueField } from "./IssueFieldForms";
import { IssueIdentityRow } from "./IssueIdentityRow";
import { IssuePropertiesRow } from "./IssuePropertiesRow";
import { IssueTitleRow } from "./IssueTitleRow";
import { useIdentityBlock } from "./identity-block";
import { pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS, STATE_LINES } from "./page-words";
import { type IssueHeaderWrites, statusPickOnly } from "./useIssueHeaderWrites";
import { useIssuePalette } from "./useIssuePalette";

/**
 * The header: what tells a reader to act, and nothing else, drawn as the prototype's
 * `#hdr-sticky` and `#hdr-props` draw it.
 *
 * 1. The breadcrumb, the key, the control that copies it and the one overflow menu, with
 *    Details at the right (`IssueIdentityRow.tsx`).
 * 2. The title, which is its own editor (`IssueTitleRow.tsx`).
 * 3. One properties line: the status, the priority, the assignee, the children's progress, the
 *    count of what waits on the reader, the approval control, the one marker where the route
 *    reaches nobody, and the labels that fit (`IssuePropertiesRow.tsx`).
 *
 * Every other property, and every control of one, is in Details (`IssueMargin.tsx`), which the
 * control at the right of row one opens. The header and Details each call `useIssueHeaderWrites`
 * and keep their own drafts; every write either makes goes in the issue's one line of the keyed
 * store (`issue:<key>`), so the two keep their order, and both read one pending status
 * (`pickStatus`).
 *
 * Rows one and two are the identity block, which stays at the top of the screen from 761 px up
 * while the properties line scrolls away under it, and which scrolls away with the rest of the
 * header at 760 px and below, where the filter row stays instead (the prototype's
 * `#hdr-sticky`): the `<header>` itself draws no box (`display: contents`), so the block's
 * `position: sticky` holds against the page's whole column rather than against the header. The
 * block publishes the height of whatever stays (`identity-block.ts`), so the tree's focused row
 * can clear it rather than guess at it.
 *
 * `data-issue-header` is load-bearing for the keyboard: the `issue` scope's bindings find the
 * status, the priority, the title and the pin through it, and scoping to it is what keeps a
 * child issue's row - which carries the same accessible names - out of their reach.
 */
export function IssueHeader({
  documentArtifact,
  isClosed,
  issue,
  onOpenField,
  onOpenLabels,
  priorityWrite,
  state,
  writes,
}: {
  documentArtifact: Artifact | undefined;
  isClosed: boolean;
  issue: IssueDetails;
  /** Opens Details at the row whose editor `field` is, with that editor open. */
  onOpenField: (field: IssueField) => void;
  /** Opens Details at its Labels row. */
  onOpenLabels: () => void;
  /** The page's one priority write, shared with the keyboard's `0`–`3` and the palette's Set
   *  priority rows, so a refusal of any of them is reported once and by this header. */
  priorityWrite: IssuePriorityWrite;
  state: UserIssueState;
  writes: IssueHeaderWrites;
}): ReactNode {
  const { drafts, pendingStatus, updateIssue } = writes;
  const block = useRef<HTMLDivElement | null>(null);
  const { condensed } = useIdentityBlock(block);
  useIssuePalette({
    drafts,
    isClosed,
    priorityWrite,
    saving: updateIssue.isPending,
  });

  const actions: IssueAction[] = [
    {
      disabled: writes.pin.isPending,
      id: "pin",
      pressed: state.pinned,
      run: () => writes.pinGuard.guard(() => writes.pin.mutate(!state.pinned)),
      text: state.pinned ? CONTROL_LABELS.unpin_issue : CONTROL_LABELS.pin_issue,
    },
    isClosed
      ? {
          disabled: updateIssue.isPending,
          id: "reopen",
          run: () => drafts.requestStatusSubmit("backlog"),
          text:
            pendingStatus === "backlog"
              ? ANSWER_LINES.saving_reopen
              : CONTROL_LABELS.reopen_into_backlog,
        }
      : {
          disabled: updateIssue.isPending,
          id: "close",
          run: () => drafts.requestStatusSubmit("done"),
          text: pendingStatus === "done" ? ANSWER_LINES.saving_close : CONTROL_LABELS.close_issue,
        },
    {
      disabled: isClosed,
      id: "parent",
      run: () => onOpenField("parent"),
      text: issue.parent === null ? CONTROL_LABELS.set_parent : CONTROL_LABELS.edit_parent,
    },
  ];

  return (
    <header
      className="contents"
      data-condensed={condensed ? "true" : "false"}
      data-issue-header=""
      data-testid="issue-header"
    >
      {/* The band stays from 761 px up and scrolls away at 760 px and below: `min-[761px]`
          and no `max-[760px]`, which Tailwind builds as `width < 760px` and so leaves 760 px
          itself sticky. */}
      <div
        className={`z-20 flex min-w-0 flex-col px-3.5 pt-2 pb-0.5 min-[761px]:sticky min-[761px]:top-0 min-[761px]:px-7 min-[761px]:pt-[7px] min-[761px]:pb-px ${pagePaperBg} ${pageInkText} ${
          condensed ? "shadow-[0_1px_0_var(--color-page-line),0_6px_14px_rgba(28,25,23,0.06)]" : ""
        }`}
        data-identity-block=""
        ref={block}
      >
        {isClosed ? (
          <p
            className={`mb-1 rounded-lg border px-3 py-1 text-sm ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
            {...pageChrome}
          >
            {STATE_LINES.closed}
          </p>
        ) : null}
        <IssueIdentityRow
          actions={actions}
          issueKey={issue.key}
          parent={issue.parent}
          project={issue.project}
        />
        <IssueTitleRow drafts={drafts} isClosed={isClosed} />
        {drafts.titleError === null ? null : (
          <p className={`text-sm ${pageFailedText}`} id="issue-title-help">
            {drafts.titleError}
          </p>
        )}
      </div>
      <IssuePropertiesRow
        documentArtifact={documentArtifact}
        isClosed={isClosed}
        issue={issue}
        onOpenLabels={onOpenLabels}
        priorityWrite={priorityWrite}
        writes={writes}
      />
      {/* What the page says in answer to a write of the reader's, under the line it came from:
          drawn only while there is one, so the header is its three lines otherwise. */}
      <div className="flex min-w-0 flex-col gap-1 px-3.5 empty:hidden min-[761px]:px-7">
        {writes.statusSaving ? (
          <span className={`block text-xs ${pageMutedText}`} role="status" {...pageChrome}>
            {pendingStatus === "done"
              ? ANSWER_LINES.saving_close
              : pendingStatus === "backlog" && isClosed
                ? ANSWER_LINES.saving_reopen
                : ANSWER_LINES.saving_field}
          </span>
        ) : null}
        {writes.pin.isError ? (
          <QueryError
            message="Could not save pin status."
            onRetry={() => writes.pinGuard.retryLast(writes.pin)}
            retrying={writes.pin.isPending}
          />
        ) : null}
        {/* A refused parent is said beside its editor, in Details (`IssueMargin.tsx`). */}
        {updateIssue.isError &&
        !statusPickOnly(updateIssue.variables) &&
        !writes.parentSaveFailed ? (
          writes.titleRefusal !== null ? (
            <p className={`text-sm ${pageFailedText}`} id="issue-title-error" role="alert">
              {writes.titleRefusal}
            </p>
          ) : (
            <QueryError
              message="Could not update this issue."
              onRetry={drafts.retry}
              retrying={updateIssue.isPending}
            />
          )
        ) : null}
      </div>
    </header>
  );
}
