import { type ReactNode, useRef, useState } from "react";

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
import { IssueStatusNotes } from "../issue/parent-status/IssueStatusNotes";
import type { IssuePriorityWrite } from "../issue/useIssuePriority";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { stoppedNote } from "../writes/useKeyedWrite";
import { chipOfLostAction } from "./approval-chip-box";
import { type FocusFallback, useFocusKeeper } from "./focus-keeper";
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
 * Where the header's keeper puts the focus once a write takes away the control that held it -
 * a close that disables the status, an answered request that removes Approve, a refusal line
 * that goes once its write lands - in this order (decision 216). When the lost control is one of
 * an approval chip's two actions, or a control of the Request changes dialog the chip opened,
 * that chip comes first (`chipOfLostAction`): both actions go once the request is answered, and
 * the dialog shuts once its change request is read, so the reader stays on the document they
 * just reviewed rather than being sent to the top of the header. "Issue actions" comes next: the
 * header draws it in every state and no write disables it, and it opens the actions a reader
 * goes on with. The title's heading comes last, as the field the issue's refusal line stands for,
 * which is the place the decision names; it stands in the same block as "Issue actions", so it
 * is reached only should that control ever be missing.
 */
const HEADER_FOCUS_FALLBACKS: readonly FocusFallback[] = [
  chipOfLostAction,
  ({ root }) =>
    root.querySelector<HTMLElement>(`button[aria-label="${CONTROL_LABELS.issue_actions}"]`),
  ({ root }) => root.querySelector<HTMLElement>("h1"),
];

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
  const { condensed, scrolled } = useIdentityBlock(block);
  const header = useRef<HTMLElement | null>(null);
  useFocusKeeper(header, HEADER_FOCUS_FALLBACKS);
  useIssuePalette({
    isClosed,
    priorityWrite,
    requestStatus: writes.requestStatus,
    saving: updateIssue.isPending,
  });

  const actions: IssueAction[] = [
    {
      disabled: writes.pin.isPending,
      id: "pin",
      pressed: state.pinned,
      run: () =>
        writes.pinGuard.guard(() =>
          writes.pin.mutate({ issueKey: issue.key, pinned: !state.pinned })
        ),
      text: state.pinned ? CONTROL_LABELS.unpin_issue : CONTROL_LABELS.pin_issue,
    },
    isClosed
      ? {
          disabled: updateIssue.isPending,
          id: "reopen",
          run: () => writes.requestStatus("backlog"),
          text:
            pendingStatus === "backlog"
              ? ANSWER_LINES.saving_reopen
              : CONTROL_LABELS.reopen_into_backlog,
        }
      : {
          disabled: updateIssue.isPending,
          id: "close",
          run: () => writes.requestStatus("done"),
          text: pendingStatus === "done" ? ANSWER_LINES.saving_close : CONTROL_LABELS.close_issue,
        },
    {
      disabled: isClosed,
      id: "parent",
      run: () => onOpenField("parent"),
      text: issue.parent === null ? CONTROL_LABELS.set_parent : CONTROL_LABELS.edit_parent,
    },
  ];

  // Whether this header says the issue's refused write: a refused parent is said beside its
  // editor, in Details (`IssueMargin.tsx`), and a status pick alone rolls back in its own control.
  const issueRefused =
    updateIssue.isError &&
    !statusPickOnly(updateIssue.variables?.input) &&
    !writes.parentSaveFailed;

  return (
    <header
      className="contents"
      data-condensed={condensed ? "true" : "false"}
      data-issue-header=""
      data-testid="issue-header"
      ref={header}
    >
      {/* The band stays from 761 px up and scrolls away at 760 px and below: `min-[761px]`
          and no `max-[760px]`, which Tailwind builds as `width < 760px` and so leaves 760 px
          itself sticky. Where it stays it draws its lower line, 1 px in the page's line colour,
          from the first pixel the page scrolls, and its soft shadow under it past 40 px; at the
          page's top, and where it scrolls away, neither (`useIdentityBlock`). */}
      <div
        className={`z-20 flex min-w-0 flex-col px-3.5 pt-2 pb-0.5 min-[761px]:sticky min-[761px]:top-0 min-[761px]:px-7 min-[761px]:pt-[7px] min-[761px]:pb-px ${pagePaperBg} ${pageInkText} ${
          condensed
            ? "shadow-[0_1px_0_var(--color-page-line),0_6px_14px_rgba(28,25,23,0.06)]"
            : scrolled
              ? "shadow-[0_1px_0_var(--color-page-line)]"
              : ""
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
        {/* A write the reader stopped is said as stopped, as today's header says it
            (`stoppedNote`, `stoppedWriteNote`): it may have been saved. The pin's Retry makes the
            same press again; the issue's line's Retry is a press of its own, of what the field
            holds now (`drafts.retry`). Any other refusal keeps its plain line. */}
        <RefusalLine
          failed={writes.pin.isError}
          message={
            stoppedNote(writes.pin, "the pin status", "saved") ?? "Could not save pin status."
          }
          onRetry={() => writes.pinGuard.retryLast(writes.pin)}
          pending={writes.pin.isPending}
        />
        {writes.titleRefusal !== null ? (
          <p className={`text-sm ${pageFailedText}`} id="issue-title-error" role="alert">
            {writes.titleRefusal}
          </p>
        ) : null}
        <RefusalLine
          failed={issueRefused && writes.titleRefusal === null}
          message={
            updateIssue.error instanceof SendStoppedError
              ? stoppedWriteNote("this change to the issue", "saved", {
                  anew: "Retry sends what the field holds now as a new write.",
                })
              : "Could not update this issue."
          }
          onRetry={drafts.retry}
          pending={updateIssue.isPending}
        />
        <IssueStatusNotes
          issueKey={issue.key}
          noteClassName={`text-sm ${pageFailedText}`}
          statusMove={writes.statusMove}
        />
      </div>
    </header>
  );
}

/**
 * A refused write's line with its Retry (decision 209). It is drawn while the write `failed`, and
 * it stays drawn while the write runs again (`pending`), until that write lands or is refused
 * once more. A `QueryError` drawn only while the write failed goes the moment Retry starts it,
 * since a pending write is no longer a failed one, and its `retrying` disables the Retry while
 * the write runs: either way the focus of a keyboard reader who pressed Retry falls to the page's
 * body. Here the line and its Retry stay, and the Retry stays enabled, which the write's own
 * guard makes safe: it ignores a second press while one is out. While the write runs again the
 * line says "Saving…" (`ANSWER_LINES.saving_field`) in place of its message, and says the
 * message again once the write is refused once more: the alert's words change, so a second
 * refusal is shown and announced as the first was, where the same words kept from one refusal to
 * the next would change nothing a reader sees or a screen reader hears. When the write lands the
 * line goes, and the header's keeper then moves the focus. The header draws it at all times, so
 * it remembers across renders that it was drawn when the write started again.
 */
function RefusalLine({
  failed,
  message,
  onRetry,
  pending,
}: {
  failed: boolean;
  message: string;
  onRetry: () => void;
  pending: boolean;
}): ReactNode {
  // Whether the line was drawn at the last render: a write that starts while it is drawn - from
  // Retry or from the field itself - keeps it until that write ends.
  const [drawn, setDrawn] = useState(failed);
  const draw = failed || (drawn && pending);
  if (draw !== drawn) setDrawn(draw);
  return draw ? (
    <QueryError message={failed ? message : ANSWER_LINES.saving_field} onRetry={onRetry} />
  ) : null;
}
