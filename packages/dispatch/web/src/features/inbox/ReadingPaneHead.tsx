import { type ReactNode, useCallback, useState } from "react";
import { Link } from "react-router-dom";

import type { Artifact, InboxRow, MyReplyRow } from "../../api/types";
import {
  linkHoverText,
  linkText,
  secondaryButtonCompact,
  textMutedOnCanvas,
} from "../../theme/classes";
import { ApprovalChip, paneActionBox } from "../doc/ApprovalChip";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { blockFragment } from "../refs/block-reference";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildIssuePath, buildProjectPath, itemRoute } from "../refs/routes";
import { useActionKeys } from "../shell/keymap-keys";
import { ControlKeyHint, keyshortcutsOf } from "../shell/ShortcutHelp";
import { InboxOwnerLink, ThreadOwnerLine } from "./AskOwnerLink";
import { AssignToMe } from "./AssignToMe";
import { askIssueKey, controlName } from "./ask-name";
import { DismissControl } from "./DismissControl";
import { EarlierApprovalDetails, EarlierApprovalToggle } from "./EarlierApproval";
import { inboxActionMark } from "./inbox-action-mark";
import { threadReferenceRoute } from "./list-row";
import { SnoozeControl } from "./SnoozeControl";
import { isUnassigned } from "./sections";
import { isSnoozed } from "./snooze";

/** The head's first row: the item's controls, in a row of their own above its title (R9). The
 *  controls are at its start and the place of a fixed width is at its end; the row lines them up
 *  at their tops, so neither a control that appears late nor a pill that wraps moves another. It
 *  wraps only where the column is narrower than the controls' least width and the place
 *  together, and the place then takes a line of its own: what decides that is the column's
 *  width, never the title. */
const CONTROL_ROW = "flex flex-wrap items-start gap-x-3 gap-y-1";

/** The controls at the row's start, in their fixed order: they share what the place leaves of
 *  the row (`flex-1` from no width), at least 12rem, and wrap among themselves, so a control
 *  that appears late (Skip to the answer, Next change) takes the end and moves none before it.
 *  Their first line is the place's height, 44 px, so its controls stand level with the place's
 *  actions. */
const CONTROL_GROUP = "flex min-h-11 min-w-48 flex-1 flex-wrap items-center gap-x-2 gap-y-1";

/** The place at the row's end, of one width from the head's first draw, which an approval
 *  request and a thread both draw (R9): the approval's state, its actions and the earlier
 *  approval's Show diff, or a thread's Dismiss, right-aligned, so the last of them - Request
 *  changes, or Dismiss - ends at the row's end. Its width holds Approve and Request changes with
 *  room beside them for the pill, which wraps inside that room (`ApprovalChip`'s `pane`).
 *  `data-inbox-head-place` marks it. */
const END_PLACE =
  "ml-auto flex w-[28rem] max-w-full shrink-0 flex-wrap items-start justify-end gap-x-2 gap-y-1";

/** The head's second row, a heading of its own: the title, whole, over as many lines as it needs. */
const TITLE_ROW = "text-sm";

/** The page "Open page" opens: the question's issue, or — for a project document, which has no
 *  issue page — the document itself at the section the column is scrolled to. The section is the
 *  heading the pane found; before the document has drawn, the question's own block, and for an
 *  anchored question the block its quote sits in. */
function openPagePath(ask: InboxRow, sectionBlockId: string | undefined): string | undefined {
  const issueKey = askIssueKey(ask);
  if (issueKey !== null) {
    return buildIssuePath({ key: issueKey, kind: "issue" });
  }
  if (ask.document === undefined) {
    return undefined;
  }
  const document = buildProjectPath({
    kind: "document",
    project: ask.document.project,
    slug: ask.document.slug,
  });
  const blockId = sectionBlockId ?? ask.block_id ?? ask.anchor?.block_id;
  return blockId === undefined || blockId === null
    ? document
    : `${document}${blockFragment(blockId)}`;
}

export interface ReadingPaneHeadProps {
  /** The document the column shows, once it is read; undefined for a question in no document and
   *  while the read is out. The approval actions are this document's. */
  artifact: Artifact | undefined;
  ask: InboxRow;
  /** The reader's own answer or review landed. */
  onAnswered: (askId: string) => void;
  /** Moves the column to the next block marked as changed; undefined when nothing is marked. */
  onNextChange: (() => void) | undefined;
  /** Moves the column to the decision block; undefined when the question is not one. */
  onSkipToAnswer: (() => void) | undefined;
  /** Registers the question while this head's Snooze write is in flight or failed, exactly as
   *  a row's own control registers its own: the Inbox keeps a live row listed, so the control
   *  holding `Snoozing…`, a refusal and its Retry is not unmounted by the fold the optimistic
   *  snooze causes - the column follows that row, so the fold would take this head with it. */
  onSnoozeLive: (askId: string, live: boolean) => void;
  /** The document's read failed, and the column draws the question's own card in its place,
   *  whose options answer an approval request: the head's place keeps its width and draws no
   *  Approve or Request changes of its own. */
  readFailed: boolean;
  /** The heading of the section the column is opened at, once the block has drawn. */
  sectionBlockId: string | undefined;
  viewer: string | undefined;
}

/**
 * The reading column's head: the controls that act on the selected question, in a row of their
 * own, and under them what the question is, whole (R9).
 *
 * An issue question carries "Open page", the issue's priority, "Assign to me" while nobody holds
 * the issue, Snooze and Copy ref, then the issue key and title. A question in a project document
 * carries "Open page" at the section, Snooze and Copy ref, then the project key and the document
 * name — a project document has no assignee and no priority, so neither control applies to it.
 * The controls keep that order, and the two the column offers once its document has drawn (Skip
 * to the answer, Next change) come after them, so no control moves when they appear. An approval
 * request draws the place at the row's end from its first draw, known from the row: Approve and
 * Request changes, held until the document's read returns, then the approval's state, the actions
 * it allows and the earlier approval's Show diff, whose sentence and diff go under the title. So
 * a control has one position for every title, from one character to many lines, and keeps it
 * when the read returns. Each control whose action has a key draws its first key inside it, the
 * person's own keys (`useActionKeys`), and names them all in `aria-keyshortcuts`: by default the
 * priority `p`, Snooze `b`, Copy ref `y`, Approve `a` and Request changes Shift+R, and any other
 * control once the person gives its action a key; a control whose action has none draws none.
 *
 * With the column on, `h`, `o` and `y` look inside the column rather than inside the selected
 * row (`hand()` returns the column in `useInboxKeymap.ts`), and the short row beside it draws no
 * Snooze and no copy-ref control at all. So this head draws the same three controls the card row
 * draws — `[data-inbox-snooze]`, `[data-inbox-owner]` (the title) and the copy-ref control — and
 * the keys find them here. The `<header>` of this head and of the thread's (`ThreadPaneHead`)
 * carries `data-inbox-pane-head`, so a key that looks for a control of the head alone finds it
 * there and never a control of the same kind inside the document the column draws under it: `a`
 * and Shift+R press the approval's Approve and Request changes (`data-approval-action`) there
 * while they take a press, and the command bar presses Open page, Skip to the answer, Next change
 * and the approval's state by their own marks (`inboxActionMark`).
 */
export function ReadingPaneHead({
  artifact,
  ask,
  onAnswered,
  onNextChange,
  onSkipToAnswer,
  onSnoozeLive,
  readFailed,
  sectionBlockId,
  viewer,
}: ReadingPaneHeadProps): ReactNode {
  // The Inbox row keeps "Assign to me" mounted while its write is in flight or failed, so
  // `Assigning…` and a refusal are shown by the instance that wrote rather than lost to the
  // optimistic update that takes the issue out of `isUnassigned`. The head does the same.
  const [assignLive, setAssignLive] = useState(false);
  const onAssignLive = useCallback((_askId: string, live: boolean) => setAssignLive(live), []);
  // The earlier approval's diff is open for the request it was opened on, and closed on the next.
  const [diffOpenFor, setDiffOpenFor] = useState<string | undefined>(undefined);
  const openKeys = useActionKeys("inbox.open-at-section");
  const priorityKeys = useActionKeys("inbox.priority");
  const assignKeys = useActionKeys("inbox.assign-to-me");
  const snoozeKeys = useActionKeys("inbox.snooze");
  const unsnoozeKeys = useActionKeys("inbox.unsnooze");
  const copyKeys = useActionKeys("inbox.copy-ref");
  const skipKeys = useActionKeys("inbox.skip-to-answer");
  const nextChangeKeys = useActionKeys("inbox.next-change");
  const diffKeys = useActionKeys("inbox.earlier-approval-diff");
  const reviewsKeys = useActionKeys("inbox.show-reviews");
  const approveKeys = useActionKeys("inbox.approve");
  const requestChangesKeys = useActionKeys("inbox.request-changes");
  const name = controlName(ask, undefined);
  const page = openPagePath(ask, sectionBlockId);
  const reference = itemRoute("ask", ask, ask.document);
  const approval = ask.kind === "approval" ? ask.approval : undefined;
  // The chip draws Approve only while the document is reviewable and not already approved; where
  // it draws none, the head says which approval this request follows instead.
  const approvalState = artifact?.approval?.state;
  const approveDrawn =
    approvalState !== undefined && approvalState !== "draft" && approvalState !== "approved";
  const earlier =
    approval === undefined || artifact === undefined || approveDrawn ? undefined : approval;
  const diffOpen = diffOpenFor === ask.id;

  return (
    <header className="flex flex-col gap-1" data-inbox-pane-head="">
      <div className={CONTROL_ROW}>
        <div className={CONTROL_GROUP}>
          {page === undefined ? null : (
            <Link
              aria-keyshortcuts={keyshortcutsOf(openKeys)}
              className={`text-sm ${linkText} ${linkHoverText}`}
              {...inboxActionMark("inbox.open-at-section")}
              to={page}
            >
              Open page
              <ControlKeyHint keys={openKeys} />
            </Link>
          )}
          {ask.issue_key === null ? null : (
            <PriorityControl
              issueKey={ask.issue_key}
              priority={issuePriority(ask.priority)}
              shortcutKeys={priorityKeys}
            />
          )}
          {ask.issue_key !== null &&
          viewer !== undefined &&
          (isUnassigned({ ask, kind: "ask" }) || assignLive) ? (
            <AssignToMe
              askId={ask.id}
              issueKey={ask.issue_key}
              onLive={onAssignLive}
              shortcutKeys={assignKeys}
              viewer={viewer}
            />
          ) : null}
          <SnoozeControl
            askId={ask.id}
            label={name}
            onLive={onSnoozeLive}
            shortcutKeys={snoozeKeys}
            snoozedUntil={isSnoozed(ask) ? ask.snoozed_until : null}
            unsnoozeKeys={unsnoozeKeys}
          />
          {reference === undefined ? null : (
            <CopyRefButton item route={reference} shortcutKeys={copyKeys} />
          )}
          {onSkipToAnswer === undefined ? null : (
            <button
              aria-keyshortcuts={keyshortcutsOf(skipKeys)}
              className={secondaryButtonCompact}
              {...inboxActionMark("inbox.skip-to-answer")}
              onClick={onSkipToAnswer}
              type="button"
            >
              Skip to the answer
              <ControlKeyHint keys={skipKeys} />
            </button>
          )}
          {onNextChange === undefined ? null : (
            <button
              aria-keyshortcuts={keyshortcutsOf(nextChangeKeys)}
              className={secondaryButtonCompact}
              {...inboxActionMark("inbox.next-change")}
              onClick={onNextChange}
              type="button"
            >
              Next change
              <ControlKeyHint keys={nextChangeKeys} />
            </button>
          )}
        </div>
        {approval === undefined ? null : (
          <div className={END_PLACE} data-inbox-head-place="">
            {earlier?.approved === undefined ? null : (
              <EarlierApprovalToggle
                diffOpen={diffOpen}
                onToggle={() => setDiffOpenFor(diffOpen ? undefined : ask.id)}
                shortcutKeys={diffKeys}
              />
            )}
            {readFailed ? null : (
              // One chip for each request, so a review out on one never lands on the next.
              <ApprovalChip
                artifact={artifact}
                key={ask.id}
                layout="contents"
                onReviewed={() => onAnswered(ask.id)}
                shortcutKeys={{
                  approve: approveKeys,
                  requestChanges: requestChangesKeys,
                  showReviews: reviewsKeys,
                }}
                stateMarks={inboxActionMark("inbox.show-reviews")}
                variant="pane"
              />
            )}
          </div>
        )}
      </div>
      <h2 className={TITLE_ROW}>
        <InboxOwnerLink ask={ask} rowOwner wrap />
      </h2>
      {earlier === undefined ? null : (
        <EarlierApprovalDetails approval={earlier} diffOpen={diffOpen} />
      )}
    </header>
  );
}

/**
 * The reading column's head for a Reply or an Agent row, in the question's two rows (R9): **Open
 * page** - the thread's own page, which the server names in the row's `ref`: the document with
 * the thread open, the issue's conversation, or `/agents/<id>/live` for an Agent row - and **Copy
 * ref**, with the **Dismiss** that takes the row out of the Inbox in the place at the row's end,
 * which in the Dismissed view reads **Move to Inbox** (`DismissControl`); then who the thread
 * belongs to, whole. Dismiss ends where an approval request's Request changes ends, at the row's
 * end. Each control draws its key: Open page `o`, Copy ref `y`, Dismiss `e`.
 *
 * **Open page** is what `o` follows here (`data-inbox-owner`), as the card's **Open** is below
 * this column's width: the key opens the thread, never the issue it happens to sit on, and it
 * opens an Agent row's live view, which has no owner link to fall back to. The owner under it
 * is a link a reader can still follow by hand.
 *
 * An Agent row has no **Copy ref**: Dispatch's reference grammar has no form for an issue-less
 * message (`dispatch://KEY/message/<id>` needs an issue key, and `dispatch://agent/` covers only
 * artifacts), which is why `y` does nothing on such a row either. Priority, **Assign to me** and
 * **Snooze** are a question's controls and show for no thread row at all.
 */
export function ThreadPaneHead({
  sectionBlockId,
  thread,
}: {
  /** The heading of the section the column is opened at, where the column shows a document:
   *  the page opens there rather than at the document's top. */
  sectionBlockId?: string | undefined;
  thread: MyReplyRow;
}): ReactNode {
  const openKeys = useActionKeys("inbox.open");
  const copyKeys = useActionKeys("inbox.copy-ref");
  const dismissKeys = useActionKeys("inbox.dismiss");
  const reference = threadReferenceRoute(thread);
  const page =
    sectionBlockId === undefined ? thread.ref : `${thread.ref}${blockFragment(sectionBlockId)}`;
  return (
    <header className="flex flex-col gap-1" data-inbox-pane-head="">
      <div className={CONTROL_ROW}>
        <div className={CONTROL_GROUP}>
          <Link
            aria-keyshortcuts={keyshortcutsOf(openKeys)}
            className={`text-sm ${linkText} ${linkHoverText}`}
            data-inbox-owner=""
            to={page}
          >
            Open page
            <ControlKeyHint keys={openKeys} />
          </Link>
          {reference === undefined ? null : (
            <CopyRefButton item route={reference} shortcutKeys={copyKeys} />
          )}
        </div>
        <div className={END_PLACE} data-inbox-head-place="">
          <span className={paneActionBox}>
            <DismissControl shortcutKeys={dismissKeys} shown thread={thread} />
          </span>
        </div>
      </div>
      <h2 className={TITLE_ROW}>
        <ThreadOwnerLine
          className={`break-words font-semibold ${textMutedOnCanvas}`}
          thread={thread}
          wrap
        />
      </h2>
    </header>
  );
}
