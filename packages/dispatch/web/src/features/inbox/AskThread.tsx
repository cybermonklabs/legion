import { dispatchWritingBox } from "@legion/contracts";
import type { UseQueryResult } from "@tanstack/react-query";
import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { type ArtifactOwner, api, type WriteBounds } from "../../api/client";
import type { Ask, AskRead, Comment, CommentWithTrees, CreateCommentInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { useTooltips } from "../../components/tooltips";
import { useSending } from "../../hooks/useSending";
import {
  borderDefault,
  pageInkText,
  pageLineBorder,
  pageQuietText,
  surfaceMutedBg,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { appendToDraft, useDraftUpload } from "../artifacts/ArtifactUpload";
import { MentionField } from "../conversation/MentionField";
import { AgentName } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
import { PostBody } from "../refs/PostBody";
import { EditedMark } from "../refs/PostMarks";
import { postDeletion } from "../refs/post-marks";
import { type DocumentOwner, itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import type { DwellRef } from "../shell/useDwell";
import { EarlierReplies } from "../threads/EarlierReplies";
import { threadLayout } from "../threads/thread-layout";
import type { ThreadOrder } from "../threads/thread-order";
import { stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { AskAnswerArea } from "./AskAnswerArea";
import { postAskComment } from "./ask-comment";
import { emptyDraftRefusal } from "./draft-store";
import type { AskPress } from "./outstanding-answers";
import { useQuestionLabel } from "./question-label";
import { clearReplyDraft, setReplyDraft, useReplyDraft } from "./reply-drafts";
import { UnsentText } from "./UnsentText";

export type CreateReply = (
  issueKey: string,
  input: CreateCommentInput,
  bounds: WriteBounds
) => Promise<Comment>;

const createReply: CreateReply = (issueKey, input, bounds) =>
  api.createComment(issueKey, input, bounds);

function AskReply({
  comment,
  current = false,
  markRef,
  owner,
}: {
  comment: CommentWithTrees;
  current?: boolean;
  markRef?: DwellRef;
  /** The project document that owns a reply with no issue, so the reply can name its own
   *  `dispatch://` reference (`itemRoute`). */
  owner?: DocumentOwner;
}): ReactNode {
  const reference = itemRoute("comment", comment, owner);
  return (
    <li
      aria-current={current ? "true" : undefined}
      className={`rounded-lg p-2 text-sm ${surfaceMutedBg}`}
      data-reply=""
      ref={markRef}
    >
      <div className={textPrimaryOnSurfaceMuted}>
        <PostBody
          deleted={postDeletion(comment)}
          markdown={comment.body}
          source={textSourceOf("comment", comment.id, comment.issue_key)}
          surface="ask-thread-reply"
          tree={comment.body_tree}
        />
      </div>
      <p
        className={`mt-1 flex flex-wrap items-center gap-x-1.5 text-xs ${textMutedOnSurfaceMuted}`}
      >
        {/* The spaces around the dot draw nothing between the row's items; they keep the
            byline's text reading `alice · <time> · edited`, as every other byline's does. */}
        <AgentName actor={comment.author} inline /> <span aria-hidden="true">·</span>{" "}
        <Timestamp at={comment.created_at} />
        <EditedMark at={comment.edited_at} deleted={postDeletion(comment)} />
        {/* A reply is a node like any other, so it carries the one copy control for its own
            reference (`dispatch://<KEY>/comment/<id>`), as a margin thread's comments do. */}
        {reference === undefined ? null : <CopyRefButton route={reference} />}
      </p>
    </li>
  );
}

/** One press of a reply: the ask it is of (`AskPress`), the text it posts, and its question as
 *  the label names it (`questionWords`). The text is frozen with the press, a mention the reader
 *  picked in it included. */
interface ReplyPress extends AskPress {
  readonly questionWords: string;
  readonly text: string;
}

/** The draft a file pasted or dropped into a reply joins: the ask's (`reply-drafts.ts`), with
 *  whether the ask took a reply as a settled ask at the paste, which the text it adds records
 *  (`writtenSettled`). */
interface ReplyUploadDraft {
  readonly askId: string;
  readonly settled: boolean;
}

/** AskCard owns the `["ask-thread", ask.id]` query, initializing it from an Inbox row when
 *  available; every card renders the shared thread without a duplicate read. */
export type AskThreadQuery = UseQueryResult<AskRead, Error>;

/** Where a file pasted or dropped into an ask's answer, or into a reply under it, goes: the ask's
 *  issue, else the project of the document it was asked on (`documentOwner`, the host's, where
 *  the ask read carries none), else nowhere, and the box takes a paste as text. */
export function askUploadOwner(
  ask: Pick<Ask, "anchor_artifact" | "issue_key">,
  documentOwner: DocumentOwner | undefined
): ArtifactOwner | undefined {
  if (ask.issue_key !== null) return { issue: ask.issue_key };
  const project = (documentOwner ?? ask.anchor_artifact)?.project;
  return project === undefined ? undefined : { project };
}

/**
 * Whether an ask's thread takes a reply through the reply composer: every ask that is no longer
 * open, whether it was answered or resolved (resolved, retracted, declined or withdrawn alike),
 * since the conversation under a question goes on after the question is settled. An open ask has
 * no reply composer: its card's one composer answers it or asks back.
 */
export function takesReply(ask: Pick<Ask, "state">): boolean {
  return ask.state !== "open";
}

/**
 * The text of a reply the reader was writing under a settled ask that has since opened again (a
 * withdrawn ask whose issue was reopened, a retracted question a document's settlement restored),
 * or `""` when there is none. An open ask draws no reply composer (`takesReply`), so without this
 * the draft would sit in `reply-drafts.ts` with nothing showing it: the thread draws it read-only
 * where the composer stood (`UnsentReply`), as the card draws an answer whose question closed
 * under it (`unsentAnswer` in `useAskAnswerForm.ts`). The draft stays the ask's, so the composer
 * picks it up again if the ask is settled once more.
 *
 * Only a draft the reader last wrote while the ask was settled is one (`writtenSettled`). A draft
 * written while the ask was open was written in a host that replies while the ask is open
 * (`repliesWhileOpen`): it is a reply in progress, which that host's box holds and sends, not a
 * send that failed, so this is `""` for it, and any composer picks it up once the ask settles.
 */
export function useUnsentReply(ask: Pick<Ask, "id" | "state">): string {
  const settled = takesReply(ask);
  const [draft] = useReplyDraft(ask.id, settled);
  return settled || !draft.writtenSettled || draft.text.trim() === "" ? "" : draft.text;
}

/** What the unsent reply's box says, on today's pages and inside `NoTooltips` alike. */
const UNSENT_REPLY_WHY =
  "This question opened again before you sent your reply, so it was not sent. Your text is here to copy.";

/**
 * Each colour the unsent box draws inside `NoTooltips`, the new issue page's: its rule the
 * page's, what happened to the reader's text in the quiet grey, and that text in the ink. They
 * stand in a table, as `AskCard`'s do, because `theme/text-on-background.test.ts` reads a class
 * only where it names an export of `theme/classes.ts`, and the issue page's shared controls test
 * (`shared-controls.tooltips.test.tsx`) pairs each entry here with the page's grounds.
 */
const ON_PAGE = {
  ink: pageInkText,
  quiet: pageQuietText,
  rule: pageLineBorder,
} as const;

/** Text the reader wrote that no form will send any more, read-only and copyable where its box
 *  stood: the thread's unsent reply (`UnsentReply`) and the card's unsent answer (`AskCard`).
 *  `main`'s `UnsentText` outside `NoTooltips`, the same box in the page's colours inside. */
export function UnsentBox({
  className,
  testId,
  text,
  why,
}: {
  /** The caller's own spacing around the box. */
  className?: string;
  testId: string;
  text: string;
  why: string;
}): ReactNode {
  const tooltips = useTooltips();
  if (tooltips) {
    return <UnsentText className={className} testId={testId} text={text} why={why} />;
  }
  // Inside `NoTooltips` the box is drawn in the page's colours, which `UnsentText` does not take,
  // and the reader's own words are a value, which breaks anywhere.
  return (
    <div
      className={`rounded-xl border p-3 ${ON_PAGE.rule} ${className ?? ""}`}
      data-testid={testId}
    >
      <p className={`text-xs ${ON_PAGE.quiet}`}>{why}</p>
      <p
        className={`mt-1 max-w-full text-sm whitespace-pre-wrap [overflow-wrap:anywhere] ${ON_PAGE.ink}`}
      >
        {text}
      </p>
    </div>
  );
}

/** `useUnsentReply`'s text, read-only and copyable, in the reply composer's place. */
export function UnsentReply({ className, text }: { className?: string; text: string }): ReactNode {
  return (
    <UnsentBox className={className} testId="ask-unsent-reply" text={text} why={UNSENT_REPLY_WHY} />
  );
}

/**
 * The reply box under an ask that is no longer open (`takesReply`), and under an open one whose
 * host replies while it is open (`AskThread`'s `repliesWhileOpen`), which is the same answer
 * area as the open card's (`AskAnswerArea.tsx`): the Conversation tab's field and `@` picker,
 * with Reply as the one button it sends by. It posts to the ask's issue, or to its document for
 * a project-document ask, then invalidates the shared thread so the reply shows first. A picked
 * session's reference is in the comment body, which the server reads for delivery. The draft
 * belongs to the ask (`reply-drafts.ts`). Each edit records whether the ask was settled when
 * the reader wrote it. A thread with no composer shows an unsent reply only if the reader
 * typed it under a settled ask (`useUnsentReply`). Its send is the ask's too: while any composer's reply
 * to this ask is out, every composer for it, including one mounted since, shows the draft
 * disabled. A file pasted or dropped into the field uploads to `uploadOwner` and its text joins
 * the draft.
 */
export function AskReplyComposer({
  ask,
  createReply: reply = createReply,
  uploadOwner,
}: {
  ask: Ask;
  createReply?: CreateReply;
  /** Where a file pasted or dropped into the reply goes: the ask's issue, or its document's
   *  project. Without one the field takes a paste as text. */
  uploadOwner?: ArtifactOwner;
}): ReactNode {
  const queryClient = useQueryClient();
  const tooltips = useTooltips();
  const settled = takesReply(ask);
  const [draft, setDraft] = useReplyDraft(ask.id, settled);
  // The uploads behind the reply, held by the composer rather than by the box it writes in, as
  // the ask card holds its answer's (`AskCard.tsx`): the send is this composer's, so what holds
  // the send is too. A file's text joins the draft of the ask it was pasted or dropped under,
  // which its press carries (`useDraftUpload`): a host may draw this composer for another ask
  // before it lands.
  const upload = useDraftUpload(
    (text: string, { askId, settled: pastedSettled }: ReplyUploadDraft) =>
      setReplyDraft(askId, (current) => ({
        text: appendToDraft(current.text, text),
        writtenSettled: pastedSettled,
      }))
  );
  const uploading = upload.pending > 0;
  const question = useQuestionLabel(ask);
  // Keyed by the ask, so the send outlives the composer that started it and any composer for the
  // ask sees it in flight.
  const mutationKey = ["ask-reply", ask.id];
  const { sending } = useSending(mutationKey);
  // The form shows a refused reply itself, with Retry, under the draft it kept. Each press
  // carries the ask it replies to, as an answer's does (`useAskAnswerForm`): where it posts, and
  // the draft and the thread its landing clears and reads again, are that ask's, whichever ask a
  // host draws this composer for by then.
  const submit = useKeyedWrite({
    method: "POST",
    write: (press: ReplyPress, bounds) => postAskComment(press.ask, press.text, bounds, reply),
    inline: true,
    // The question goes in as its plain-text projection, the words the card draws: the reading
    // list's `write-label` (`DISPATCH_READING_SURFACES`) says why. Each press carries the line as
    // it reads at the press, while the composer is drawn for that ask, so a Retry made once a host
    // has drawn the composer for the next one names the question it answers.
    label: (press: ReplyPress) => `Reply on “${press.questionWords}”`,
    mutationKey,
    onSuccess: (_comment, press) => {
      clearReplyDraft(press.ask.id, press.text);
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", press.ask.id] });
    },
  });
  /** Why Reply refuses the draft as it stands: it posts what the box holds. */
  const refusal = emptyDraftRefusal(draft.text);

  // The field's own Ctrl/Cmd+Enter stands down while a picture is still uploading
  // (`AskAnswerArea`), and Reply says the upload is what holds it, so neither way in sends a
  // reply the picture would miss.
  const sendReply = () => {
    if (refusal !== null || sending || uploading) return;
    submit.mutate({ ask, questionWords: question.line(), text: draft.text.trim() });
  };

  return (
    <form
      className="flex flex-col gap-2"
      // Ctrl/Cmd+Enter submits the form without passing through the button that would have
      // swallowed a refused press, so the form owes the same refusal (`RefusableButton`).
      onSubmit={(event) => {
        event.preventDefault();
        sendReply();
      }}
    >
      {question.reads}
      <AskAnswerArea
        actions={({ reasonId }) => (
          <RefusableButton
            busy={sending ? "Replying…" : uploading ? "Reply" : undefined}
            className="self-start"
            look="secondary-compact"
            refusal={refusal}
            refusalShownBy={reasonId.reply}
            type="submit"
          >
            Reply
          </RefusableButton>
        )}
        draft={draft.text}
        field={(props) => (
          <MentionField
            {...props}
            disabled={sending}
            issueKey={ask.issue_key ?? undefined}
            label="Reply"
            labelClassName={
              tooltips ? `block text-sm font-medium ${textSecondaryOnCanvas}` : undefined
            }
            writing={dispatchWritingBox("ask-thread-reply", "comment")}
          />
        )}
        onDraftChange={setDraft}
        refusals={{ reply: refusal }}
        upload={upload}
        uploadDraft={{ askId: ask.id, settled }}
        uploadOwner={uploadOwner}
      />
      {submit.isError ? (
        <QueryError
          message={stoppedNote(submit, "your reply", "posted") ?? "Could not post your reply."}
          onRetry={submit.retry}
          retrying={submit.isPending}
        />
      ) : null}
    </form>
  );
}

export interface AskThreadProps {
  ask: Ask;
  /** The read of the ask's thread its host holds (`useAskThreadRead`); this component never
   *  requests thread data itself. */
  thread: AskThreadQuery;
  createReply?: CreateReply;
  /** False where the caller renders the reply composer itself: a collapsed thread
   *  (Conversation, decision blocks) puts it after its own Write a reply control. */
  showComposer?: boolean;
  /** A thread inside an open ask card inherits the card's compact flow. */
  embedded?: boolean;
  /** Where a file pasted or dropped into the reply goes (`AskReplyComposer`). */
  uploadOwner?: ArtifactOwner;
  /** The shared thread read mark for the ask (`AskCard`, `useMarkThreadRead`), placed on the
   *  newest reply's own element when that reply is the newest write the card shows. */
  markNewest?: DwellRef;
  /** The reply a deep link named, which only the Conversation passes: that reply carries
   *  `aria-current`, and the fold over the older replies opens when it is one of them. */
  currentReplyId?: string;
  /** The project document that owns an ask with no issue, so each reply can name its own
   *  `dispatch://` reference (`AskReply`, `itemRoute`). */
  owner?: DocumentOwner;
  /** The order the thread reads in: the reader's own choice, which the card holds
   *  (`threads/thread-order.ts`). */
  order: ThreadOrder;
  /** The ids of the replies the reader has not read (`unreadWrites` in
   *  `threads/thread-layout.ts`): an unread reply is never folded away. */
  unread?: ReadonlySet<string>;
  /** Whether the reply composer stands under an open ask too, where the host answers the ask with
   *  controls of its own and draws no answer area to ask back in: a reply is then the one way
   *  into the thread (the Inbox reading pane's approval request, `ApprovalExchange`). A reply
   *  typed here while the ask is open is one the reader is still writing, which no other thread
   *  for the ask reads as unsent (`useUnsentReply`). */
  repliesWhileOpen?: boolean;
}

/**
 * The reply thread under a question: every comment that replies directly to the ask
 * (Comment.ask_id) plus their own reply chains, in the reader's thread order, with the replies
 * outside the newest two folded behind **Show N earlier replies** (`threads/thread-layout.ts`,
 * drawn by `threads/EarlierReplies.tsx`, the fold control the Inbox's thread body shares).
 * An answered or resolved ask keeps a plain reply composer, next to the newest message
 * (`composerFirst`), and `takesReply` says which asks do; an open ask reserves its single
 * composer for answering or asking back, and draws a reply the reader typed while it was settled
 * and had not sent before it opened again read-only in the composer's place (`useUnsentReply`) -
 * unless its host answers it elsewhere and draws no such composer (`repliesWhileOpen`), which
 * keeps the reply composer here. A reply typed in that host while the ask is open is drawn by no
 * other thread for the ask until the ask settles, when its composer picks it up.
 */
export function AskThread({
  ask,
  thread,
  createReply: reply = createReply,
  showComposer = true,
  embedded = false,
  uploadOwner,
  markNewest,
  currentReplyId,
  owner,
  order,
  unread,
  repliesWhileOpen = false,
}: AskThreadProps): ReactNode {
  const replies = thread.data?.replies ?? [];
  const [showAllReplies, setShowAllReplies] = useState<boolean>();
  const { composerFirst, folded, shown } = threadLayout({
    collapse: true,
    order,
    replies,
    unread,
  });
  const newestReply = replies.at(-1);
  // A link to a reply the fold holds seeds the control open, so the reply it names is on screen
  // rather than behind a control the reader must find. Seeding, not overriding: once the reader
  // uses the control, their choice wins.
  const showAll = showAllReplies ?? folded.some((comment) => comment.id === currentReplyId);

  const replyList =
    shown.length === 0 ? null : (
      <ul className="space-y-2">
        {shown.map((comment) => (
          <AskReply
            comment={comment}
            current={comment.id === currentReplyId}
            key={comment.id}
            markRef={comment === newestReply ? markNewest : undefined}
            owner={owner}
          />
        ))}
      </ul>
    );
  // The fold and what it holds are one block, drawn furthest from the newest message: under the
  // question in Oldest first, at the foot of the thread in Newest first.
  const fold = (
    <EarlierReplies
      count={folded.length}
      onToggle={() => setShowAllReplies(!showAll)}
      open={showAll}
      replies={(id) => (
        <ul className="space-y-2" id={id}>
          {folded.map((comment) => (
            <AskReply
              comment={comment}
              current={comment.id === currentReplyId}
              key={comment.id}
              owner={owner}
            />
          ))}
        </ul>
      )}
    />
  );
  const unsentReply = useUnsentReply(ask);
  const composer = !showComposer ? null : repliesWhileOpen || takesReply(ask) ? (
    <AskReplyComposer ask={ask} createReply={reply} uploadOwner={uploadOwner} />
  ) : unsentReply === "" ? null : (
    <UnsentReply text={unsentReply} />
  );

  return (
    <section
      aria-label="Replies"
      className={embedded ? "space-y-3" : `mt-4 space-y-3 border-t pt-4 ${borderDefault}`}
      data-testid={`thread-${ask.id}`}
    >
      {/* Three slots, so a change of order moves the ends and leaves the replies where they
          are: the box the reader writes in sits next to the newest message, the fold over the
          earlier replies furthest from it. */}
      {composerFirst ? composer : fold}
      {replyList}
      {composerFirst ? fold : composer}
    </section>
  );
}
