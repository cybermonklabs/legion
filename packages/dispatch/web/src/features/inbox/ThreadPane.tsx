/**
 * The Inbox's reading column for a Reply or an Agent row: one body per shape the thread comes
 * in, each marked `data-inbox-pane-body`.
 *
 * A thread that needs no document is drawn whole under its subject line (`ThreadBodyPane`); a
 * thread whose document the column can open is drawn in that document, at the block it belongs
 * to (`ThreadDocumentBody`) - a comment thread anchored to text of a document, an issue's or a
 * project's, below its block, a decision block's ask in the block itself. A document that cannot
 * be opened, and an anchor that cannot be placed, fall back to the thread alone with the text it
 * is anchored to.
 */

import { inboxThreadRow } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { askThreadQuery } from "../../api/queries";
import type { MyReplyRow, MyReplyThreadRow } from "../../api/types";
import { inlineWarningText, textPrimaryOnCanvas } from "../../theme/classes";
import { CHANGED_TEXT_SENTENCE } from "../doc/changed-text";
import type { ProofDocumentProps } from "../doc/ProofDocument";
import { PostPreview } from "../refs/PostBody";
import { EditedMark } from "../refs/PostMarks";
import { postDeletion } from "../refs/post-marks";
import { textSourceKind, textSourceOf } from "../refs/text-references";
import { isDecisionAsk } from "./ask-decision";
import { DocumentPane, PaneFrame } from "./PaneDocument";
import { type QuestionDocument, ThreadPaneHead } from "./ReadingPaneHead";
import { ThreadBody } from "./ThreadBody";

/** The subject line a thread body opens with: what the thread is about, in one line, drawn by
 *  the one renderer so it reads as the thread's root does - or the deleted line for a root its
 *  author deleted, whose subject the delete cleared - with the root's ` · edited` once its
 *  author edited it, on the line beside the clamped subject. */
function ThreadSubject({ thread }: { thread: MyReplyRow }): ReactNode {
  const deleted = postDeletion(thread);
  return (
    <h2
      className={`flex min-w-0 items-baseline gap-1 text-sm font-semibold ${textPrimaryOnCanvas}`}
      data-inbox-thread-subject=""
    >
      <PostPreview
        className="min-w-0"
        deleted={deleted}
        lines={1}
        markdown={thread.subject}
        source={textSourceOf(textSourceKind(thread.kind), thread.thread_id, thread.issue?.key)}
        surface="my-reply-subject"
        tree={thread.subject_tree}
      />
      <EditedMark at={thread.edited_at} className="shrink-0" deleted={deleted} />
    </h2>
  );
}

/** Which of the column's four thread bodies this is, as the pane marks it. */
type ThreadPaneBody = "agent" | "ask" | "document" | "issue";

/** What a thread body is drawn from, whether it is a pane of its own or the document body's
 *  fall-back. */
interface ThreadBodyProps {
  body: ThreadPaneBody;
  /** Whether this is the document body falling back to the thread alone. */
  fallback?: boolean;
  quote: boolean;
  /** Whether the text the thread is anchored to is gone from its document, which the fall-back
   *  says above the quote in the document page's words. */
  textChanged?: boolean;
  thread: MyReplyRow;
}

/** What a thread body is: the thread's subject line and the thread itself, marked as the
 *  column's own body. The document body draws this in its place where it falls back. */
function ThreadBodyContent({
  body,
  fallback = false,
  quote,
  textChanged = false,
  thread,
}: ThreadBodyProps): ReactNode {
  return (
    <div data-inbox-pane-body={body} data-inbox-pane-fallback={fallback ? "" : undefined}>
      <ThreadSubject thread={thread} />
      {textChanged ? (
        <p className={`mt-2 text-xs font-medium ${inlineWarningText}`}>{CHANGED_TEXT_SENTENCE}</p>
      ) : null}
      <ThreadBody openAtFirstUnread quote={quote} thread={thread} />
    </div>
  );
}

/** A thread that needs no document: a comment thread anchored to no text, an issue message
 *  conversation, an ask that is not a decision block, and a thread whose document the column
 *  could not open, which shows the quote the thread is anchored to in place of it. */
function ThreadBodyPane(pane: ThreadBodyProps): ReactNode {
  return (
    <PaneFrame head={<ThreadPaneHead thread={pane.thread} />} label="Selected thread">
      <ThreadBodyContent {...pane} />
    </PaneFrame>
  );
}

/**
 * A thread drawn in its own document: the live document, opened at the section that holds the
 * thread's block, with the quote highlighted.
 *
 * The two shapes differ in what the block carries. A comment thread anchored to text of a
 * document, an issue's or a project's, is drawn inside the document column directly below its
 * block, through the node view's own slot (`anchoredThread`); a decision block's ask needs none,
 * because the block draws the question, its answer and its thread itself (`AskBlockCard`).
 *
 * It falls back to the thread alone, with its quote, when the document cannot be opened - and,
 * for a thread the column must place itself, when the document holds no such block: an orphaned
 * anchor, or one naming a block that has gone, leaves the thread nowhere to be drawn.
 */
function ThreadDocumentBody({
  anchoredThread,
  artifactId,
  blockId,
  body,
  issueKey,
  loadingName,
  questionDocument,
  thread,
}: {
  anchoredThread: ProofDocumentProps["anchoredThread"];
  artifactId: string;
  blockId: string;
  body: Extract<ThreadPaneBody, "ask" | "document">;
  issueKey: string | null;
  /** The document the skeleton says it is opening. */
  loadingName: string;
  /** The issue document of a decision block's ask, which the head's **Open page** opens once
   *  the column shows it; undefined for every other thread. */
  questionDocument: QuestionDocument | undefined;
  thread: MyReplyThreadRow;
}): ReactNode {
  return (
    <DocumentPane
      anchoredThread={anchoredThread}
      artifactId={artifactId}
      blockId={blockId}
      fallback={<ThreadBodyContent body={body} fallback quote thread={thread} />}
      // The open-page link lands at the section only where the row's own page is the document's:
      // an issue comment's page is the comment's, whose `#b-` would name a block of its body.
      head={(document) => (
        <ThreadPaneHead
          questionDocument={document.showsDocument ? questionDocument : undefined}
          sectionBlockId={
            thread.kind === "comment" && thread.document === undefined
              ? undefined
              : document.sectionBlockId
          }
          thread={thread}
        />
      )}
      issueKey={issueKey}
      item={thread.thread_id}
      label="Selected thread"
      loadingName={loadingName}
      onAskAnswered={noAnswer}
      paneBody={body}
    />
  );
}

/** The thread of an ask. A decision block's ask opens its document at its section, where the
 *  block hosts its own answer form and exchange, as an open question's does; every other ask
 *  shows its card, with its answer and its thread. */
function AskThreadPane({ thread }: { thread: MyReplyThreadRow }): ReactNode {
  // The card's own key, so the card below reads no second copy of the same thread.
  const read = useQuery(askThreadQuery(thread.thread_id));
  const ask = read.data?.ask;
  if (ask !== undefined && isDecisionAsk(ask)) {
    return (
      <ThreadDocumentBody
        anchoredThread={undefined}
        artifactId={ask.block_artifact.id}
        blockId={ask.block_id}
        body="ask"
        issueKey={ask.issue_key}
        loadingName={thread.document?.name ?? "the document"}
        questionDocument={
          ask.issue_key === null
            ? undefined
            : { blockId: ask.block_id, document: ask.block_artifact, issueKey: ask.issue_key }
        }
        thread={thread}
      />
    );
  }
  return <ThreadBodyPane body="ask" quote={false} thread={thread} />;
}

/** The reading column for a Reply or an Agent row: one body per shape the thread comes in. */
export function ThreadPane({ thread }: { thread: MyReplyRow }): ReactNode {
  const row = inboxThreadRow(thread);
  if (row.kind === "agent") {
    return <ThreadBodyPane body="agent" quote={false} thread={row} />;
  }
  if (row.kind === "ask") {
    return <AskThreadPane thread={row} />;
  }
  const anchor = row.anchor;
  const blockId = anchor?.block_id;
  // A comment anchored to text of a document opens in that document, whether the document is an
  // issue's (the row carries `issue`) or a project's (the row carries `document`).
  const inDocument = row.kind === "comment" && anchor !== undefined;
  if (inDocument && !anchor.orphaned && blockId !== null && blockId !== undefined) {
    return (
      <ThreadDocumentBody
        anchoredThread={{
          blockId,
          // The column this thread is drawn in is the document's, which the pane places at the
          // anchored block. So this thread does not open at its first unread message: bounded,
          // the later of two writers would win, and the opening of the row would be decided by
          // when its replies are drawn. A thread the pane shows on its own, and the fall-back
          // it draws where the document holds no such block, open at theirs as they do today
          // (`ThreadBodyContent`).
          children: <ThreadBody thread={thread} />,
          markId: anchor.mark_id,
        }}
        artifactId={anchor.artifact_id}
        blockId={blockId}
        body="document"
        // An issue's document is read with its issue, as the issue page reads it, so a closed
        // issue's document opens closed here too.
        issueKey={row.issue?.key ?? null}
        // A project document's row names its document; the anchor names none, so an issue's
        // document is named by its issue.
        loadingName={
          row.document?.name ??
          (row.issue === undefined ? "the document" : `${row.issue.key}'s document`)
        }
        questionDocument={undefined}
        thread={row}
      />
    );
  }
  // A thread in a document whose anchor cannot be placed - orphaned, or naming no block - is the
  // document body falling back to the thread alone, with its quote and the document page's
  // sentence that the text changed. It reads no document. A project document's thread anchored
  // to no text is drawn as that fall-back too, with no quote and no sentence, since no text of
  // it changed.
  const ofDocument = inDocument || row.document !== undefined;
  return (
    <ThreadBodyPane
      body={ofDocument ? "document" : "issue"}
      fallback={ofDocument}
      quote={inDocument}
      textChanged={inDocument}
      thread={row}
    />
  );
}

/** A thread's document answers nothing of the column's: an ask answered in one of its blocks is
 *  the block's own business, and the list reads its rows again on the event. */
function noAnswer(): void {}
