import { createContext, type ReactNode, useContext } from "react";
import { Link } from "react-router-dom";

import type { Artifact, AskBlockArtifact, InboxRow, MyReplyRow } from "../../api/types";
import { issueProject } from "../../lib/issue-key";
import {
  linkHoverText,
  linkText,
  secondaryButtonCompact,
  textMutedOnCanvas,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { blockFragment } from "../refs/block-reference";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildIssuePath, buildReferencePath, documentRoute, itemRoute } from "../refs/routes";
import { InboxOwnerLink, ThreadOwnerLine } from "./AskOwnerLink";
import { AssignToMe, useAskAssign } from "./AssignToMe";
import { askIssueKey } from "./ask-name";
import { DismissControl } from "./DismissControl";
import { EarlierApproval } from "./EarlierApproval";
import { threadReferenceRoute } from "./list-row";
import { SnoozeControl } from "./SnoozeControl";
import { isUnassigned } from "./sections";
import { isSnoozed } from "./snooze";
import { useControlName } from "./useControlName";

/** What names an issue document in an address: its slug, and whether it is the issue's primary
 *  document, which opens as its spec. Each document field of an ask row carries both. */
type IssueDocument = Pick<AskBlockArtifact, "primary" | "slug">;

/** The issue document a Reply thread's decision block sits in: the issue, the document, and the
 *  block the question was written as. */
export interface QuestionDocument {
  blockId: string;
  document: IssueDocument;
  issueKey: string;
}

/** The page of an issue's document: its spec for the primary document, else the artifact.
 *  `documentRoute` reads `kind` only for a primary document, which is always a `doc` (the
 *  database's `artifacts_primary_is_doc`), and reads no project for an issue document, so
 *  neither filled value changes the address. */
function issueDocumentPath(issueKey: string, document: IssueDocument): string {
  return buildReferencePath(
    documentRoute({
      issue_key: issueKey,
      kind: "doc",
      primary: document.primary,
      project: issueProject(issueKey),
      session_id: null,
      slug: document.slug,
    })
  );
}

/** `path` opened at the block `blockId`, or at its top where there is no block. */
function atBlock(path: string, blockId: string | null | undefined): string {
  return blockId === undefined || blockId === null ? path : `${path}${blockFragment(blockId)}`;
}

/** The issue document a question is about, picked by its kind in the order the column picks what
 *  it shows (`subjectOf` in `ReadingPane.tsx`): an approval request's document, a decision
 *  block's, the one a quote was taken from. Undefined for a question in no document, and for a
 *  row that does not name the document of its kind: another field's document is not this
 *  question's. */
function issueDocumentOf(ask: InboxRow): IssueDocument | undefined {
  if (ask.kind === "approval" && ask.approval !== undefined) {
    return ask.approval_artifact;
  }
  if (ask.block_id !== undefined && ask.block_id !== null) {
    return ask.block_artifact;
  }
  if (ask.anchor !== null) {
    return ask.anchor_artifact;
  }
  return undefined;
}

/** The page "Open page" opens: the document the question is about (`issueDocumentOf`) at the
 *  section the column is scrolled to, or the issue page where the row names no document. A
 *  question in a project document, which has no issue page, opens that document at the section.
 *  The section is the heading the pane found; before the document has drawn, the question's own
 *  block, and for an anchored question the block its quote sits in. An approval request has
 *  neither, so it opens its document at the top. */
function openPagePath(ask: InboxRow, sectionBlockId: string | undefined): string | undefined {
  const issueKey = askIssueKey(ask);
  const blockId = sectionBlockId ?? ask.block_id ?? ask.anchor?.block_id;
  if (issueKey !== null) {
    const document = issueDocumentOf(ask);
    return document === undefined
      ? buildIssuePath({ key: issueKey, kind: "issue" })
      : atBlock(issueDocumentPath(issueKey, document), blockId);
  }
  if (ask.document === undefined) {
    return undefined;
  }
  const route = documentRoute({
    issue_key: null,
    kind: "doc",
    primary: false,
    project: ask.document.project,
    session_id: null,
    slug: ask.document.slug,
  });
  return atBlock(buildReferencePath(route), blockId);
}

/** What the Inbox around the reading column hears of a review its head's approval chip sends, by
 *  the approval request it is of: the reader's press (`ApprovalChip`'s `onReviewSent`), and its
 *  refusal or the reader's stop (`onReviewFailed`). Its landing reaches the Inbox as an answer's
 *  does, through the head's `onAnswered`. The head goes when the reader moves the selection and
 *  the review's write goes on, so the Inbox holds what it hears (`Inbox.tsx`'s `reviewedHere`). */
export interface HeadReviews {
  readonly failed: (askId: string) => void;
  readonly sent: (askId: string) => void;
}

/** The `HeadReviews` of the Inbox that draws the reading column; undefined where no Inbox draws
 *  the head, and nothing there listens. */
export const HeadReviewsContext = createContext<HeadReviews | undefined>(undefined);

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
  /** The heading of the section the column is opened at, once the block has drawn. */
  sectionBlockId: string | undefined;
  viewer: string | undefined;
}

/**
 * The reading column's head: what the selected question is and the controls that act on it.
 *
 * An issue question carries the issue key and title, "Open page", the issue's priority, "Assign
 * to me" while nobody holds the issue, Snooze and Copy ref. A question in a project document
 * carries the project key and the document name, "Open page", Snooze and Copy ref — a project
 * document has no assignee and no priority, so neither control applies to it. "Open page" opens
 * the page `openPagePath` names.
 *
 * With the column on, `h`, `o` and `y` look inside the column rather than inside the selected
 * row (`hand()` returns the column in `Inbox.tsx`), and the selected short row beside it draws no
 * Snooze and no copy-ref control at all. So this head draws the same three controls the card row
 * draws — `[data-inbox-snooze]`, `[data-inbox-owner]` and the copy-ref control — and the keys
 * find them here. Another short row draws its own Snooze only while a snooze of its ask is out or
 * refused (`AskShortRow`).
 */
export function ReadingPaneHead({
  artifact,
  ask,
  onAnswered,
  onNextChange,
  onSkipToAnswer,
  sectionBlockId,
  viewer,
}: ReadingPaneHeadProps): ReactNode {
  // The Inbox holds the Assign to me write, by ask, so it outlives this head, which goes when the
  // reader moves the selection (`AskAssignWritesProvider`). The optimistic assignee takes the
  // issue out of `isUnassigned` at the press, so the head draws the control while that write is
  // out or refused as well: `Assigning…`, and a refusal with its Retry.
  const assign = useAskAssign(ask.id);
  const { name, reads } = useControlName(ask, undefined);
  const page = openPagePath(ask, sectionBlockId);
  const reference = itemRoute("ask", ask, ask.document);
  const approval = ask.kind === "approval" ? ask.approval : undefined;
  // The chip draws Approve only while the document is reviewable and not already approved; where
  // it draws none, the head says which approval this request follows instead.
  const approvalState = artifact?.approval?.state;
  const approveDrawn =
    approvalState !== undefined && approvalState !== "draft" && approvalState !== "approved";
  // The Inbox hears the chip's review from its press, since its row is named leaving from there
  // (`HeadReviewsContext`).
  const reviews = useContext(HeadReviewsContext);

  return (
    <header className="flex flex-wrap items-center gap-x-2 gap-y-1">
      <InboxOwnerLink ask={ask} rowOwner />
      {page === undefined ? null : (
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to={page}>
          Open page
        </Link>
      )}
      {ask.issue_key === null ? null : (
        <PriorityControl issueKey={ask.issue_key} priority={issuePriority(ask.priority)} />
      )}
      {ask.issue_key !== null &&
      viewer !== undefined &&
      (isUnassigned({ ask, kind: "ask" }) || assign.pending || assign.failed) ? (
        <AssignToMe askId={ask.id} issueKey={ask.issue_key} viewer={viewer} />
      ) : null}
      {/* The Inbox holds the write this Snooze makes, by ask, so it outlives this head, which
          goes when the reader moves the selection (`useAskSnoozeWrites`). */}
      {reads}
      <SnoozeControl
        askId={ask.id}
        label={name}
        snoozedUntil={isSnoozed(ask) ? ask.snoozed_until : null}
      />
      {reference === undefined ? null : <CopyRefButton route={reference} />}
      {onSkipToAnswer === undefined ? null : (
        <button className={secondaryButtonCompact} onClick={onSkipToAnswer} type="button">
          Skip to the answer
        </button>
      )}
      {onNextChange === undefined ? null : (
        <button className={secondaryButtonCompact} onClick={onNextChange} type="button">
          Next change
        </button>
      )}
      {artifact === undefined || approval === undefined ? null : (
        <>
          <ApprovalChip
            artifact={artifact}
            onReviewFailed={() => reviews?.failed(ask.id)}
            onReviewSent={() => reviews?.sent(ask.id)}
            onReviewed={() => onAnswered(ask.id)}
            variant="header"
          />
          {approveDrawn ? null : <EarlierApproval approval={approval} />}
        </>
      )}
    </header>
  );
}

/**
 * The reading column's head for a Reply or an Agent row: who the thread belongs to, **Open
 * page** - the thread's own page, which the server names in the row's `ref`: the document with
 * the thread open, the issue's conversation, or `/agents/<id>/live` for an Agent row - **Copy
 * ref**, and the **Dismiss** that takes the row out of the Inbox, which in the Dismissed view
 * reads **Move to Inbox** (`DismissControl`).
 *
 * The Reply thread of a decision block in an issue document is the one exception, once the
 * column shows that document: its `ref` is the ask's page, which sends the reader to the
 * document at the block and drops the section, so **Open page** is that document at the
 * section the column is opened at (`questionDocument`), as a question's own head opens it.
 *
 * **Open page** is what `o` follows here (`data-inbox-owner`), whichever page it is, as the
 * card's **Open** is below this column's width: the key opens the thread, never the issue it
 * happens to sit on, and it opens an Agent row's live view, which has no owner link to fall back
 * to. The owner beside it is a link a reader can still follow by hand.
 *
 * An Agent row has no **Copy ref**: Dispatch's reference grammar has no form for an issue-less
 * message (`dispatch://KEY/message/<id>` needs an issue key, and `dispatch://agent/` covers only
 * artifacts), which is why `y` does nothing on such a row either. Priority, **Assign to me** and
 * **Snooze** are a question's controls and show for no thread row at all.
 */
export function ThreadPaneHead({
  questionDocument,
  sectionBlockId,
  thread,
}: {
  /** The issue document of a decision block's Reply thread, given only while the column shows
   *  that document: **Open page** opens it rather than the row's `ref`. */
  questionDocument?: QuestionDocument | undefined;
  /** The heading of the section the column is opened at, where the column shows a document:
   *  the page opens there rather than at the document's top. */
  sectionBlockId?: string | undefined;
  thread: MyReplyRow;
}): ReactNode {
  const reference = threadReferenceRoute(thread);
  const page =
    questionDocument === undefined
      ? atBlock(thread.ref, sectionBlockId)
      : atBlock(
          issueDocumentPath(questionDocument.issueKey, questionDocument.document),
          sectionBlockId ?? questionDocument.blockId
        );
  return (
    <header className="flex flex-wrap items-center gap-x-2 gap-y-1">
      <ThreadOwnerLine
        className={`min-w-0 truncate text-sm font-semibold ${textMutedOnCanvas}`}
        thread={thread}
      />
      <Link className={`text-sm ${linkText} ${linkHoverText}`} data-inbox-owner="" to={page}>
        Open page
      </Link>
      {reference === undefined ? null : <CopyRefButton route={reference} />}
      <DismissControl shown thread={thread} />
    </header>
  );
}
