import { dispatchWritingBox } from "@legion/contracts";
import type { UseQueryResult } from "@tanstack/react-query";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useContext, useState } from "react";

import { type ArtifactOwner, api, type WriteBounds, type WriteMethod } from "../../api/client";
import type { Ask, AskRead, Comment, CommentWithTrees, CreateCommentInput } from "../../api/types";
import { useHeldThroughAnswer } from "../../components/held-press";
import { QueryError } from "../../components/QueryError";
import { useTooltips } from "../../components/tooltips";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import {
  borderDefault,
  borderStrong,
  enabledCardHoverBorder,
  pageInkText,
  pageLineBorder,
  pageQuietText,
  surfaceMutedBg,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { appendToDraft, DraftUploadStatus, useDraftUpload } from "../artifacts/ArtifactUpload";
import { MentionField } from "../conversation/MentionField";
import { ConversationTurns, turnFocusRing, turnMarks } from "../conversation/turn-keys";
import { AgentName } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
import { PostBody } from "../refs/PostBody";
import { EditedMark } from "../refs/PostMarks";
import { postDeletion } from "../refs/post-marks";
import { itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import type { DwellRef } from "../shell/useDwell";
import { EarlierReplies } from "../threads/EarlierReplies";
import { threadLayout } from "../threads/thread-layout";
import type { ThreadOrder } from "../threads/thread-order";
import { stoppedNote, useKeyedWrite, usePendingKeyedPress } from "../writes/useKeyedWrite";
import { useOneCreateOut } from "../writes/useOneCreateOut";
import type { AskPress } from "./ask-writes";
import { inboxActionMark } from "./inbox-action-mark";
import {
  AskReplyWritesContext,
  type OutstandingAnswerWrite,
  type OutstandingAnswerWriteOptions,
  useInboxAnswerWrite,
} from "./outstanding-answers";
import { askArtifactId } from "./owner-reads";
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

/** One reply of an ask's thread, with the Copy reference of a reply on an issue. On an issue's
 *  Conversation tab it is a turn that tab's keys reach (`turn-keys.ts`). */
function AskReply({
  comment,
  current = false,
  markRef,
}: {
  comment: CommentWithTrees;
  current?: boolean;
  markRef?: DwellRef;
}): ReactNode {
  const inTab = useContext(ConversationTurns);
  const reference = itemRoute("comment", comment, undefined);
  return (
    <li
      aria-current={current ? "true" : undefined}
      className={`rounded-lg p-2 text-sm ${surfaceMutedBg} ${inTab ? turnFocusRing : ""}`}
      data-reply=""
      ref={markRef}
      {...turnMarks(inTab, reference)}
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
      <p className={`mt-1 flex flex-wrap items-center gap-x-1 text-xs ${textMutedOnSurfaceMuted}`}>
        <span>
          <AgentName actor={comment.author} inline /> · <Timestamp at={comment.created_at} />
          <EditedMark at={comment.edited_at} deleted={postDeletion(comment)} />
        </span>
        {reference === undefined ? null : <CopyRefButton route={reference} />}
      </p>
    </li>
  );
}

/** One press of a reply: the ask it is of (`AskPress`), the text it posts, and its question as
 *  the label names it (`questionWords`). */
interface ReplyPress extends AskPress {
  readonly questionWords: string;
  readonly text: string;
}

/** AskCard owns the `["ask-thread", ask.id]` query, initializing it from an Inbox row when
 *  available; every card renders the shared thread without a duplicate read. */
export type AskThreadQuery = UseQueryResult<AskRead, Error>;

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
 */
export function useUnsentReply(ask: Pick<Ask, "id" | "state">): string {
  const [draft] = useReplyDraft(ask.id);
  return takesReply(ask) || draft.trim() === "" ? "" : draft;
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

/** What the reply form says of a reply the reader stopped, or that Dispatch refused: a stopped one
 *  may have been posted, and Retry makes that very press again. The Inbox's row of the ask's
 *  thread says the same words of it (`ThreadShortRow`). */
export function replyRefusal(reply: {
  readonly error: unknown;
  readonly method: WriteMethod | undefined;
}): string {
  return stoppedNote(reply, "your reply", "posted") ?? "Could not post your reply.";
}

/**
 * The reply form under an ask that is no longer open (`takesReply`). It posts to the ask's issue,
 * or to its document for a project-document ask, then invalidates the shared thread so the reply
 * shows first. Its draft is the ask's (`reply-drafts.ts`), and its send is the ask's too: while
 * any composer's reply to this ask is out, every composer for it, including one mounted since,
 * holds the draft. A file pasted or dropped into the field uploads to `uploadOwner` and its text
 * joins the draft.
 *
 * Inside an Inbox the send is not the form's: the Inbox holds it, by ask, as it holds the card's
 * answer (`AskReplyWritesContext`), since the reading column's card goes whenever the reader
 * moves the selection. The send and its refusal outlive the form: the form drawn for the ask
 * again shows them, with Retry, and the row of the ask's thread says what the send is doing
 * (`ThreadShortRow`). Outside an Inbox, the write is named by its ask. Each composer for that ask
 * reads the same press, even if it mounts after the press (`usePendingKeyedPress`).
 *
 * Reply and the box are held until the send's deadline and through the commit after its answer
 * if it lands within that deadline (`useHeldThroughAnswer`). They keep the focus. While held,
 * Ctrl+Enter in the read-only box submits nothing, and a paste or a drop uploads nothing.
 * An Inbox's shared write has its own hold. Reply is also held while the draft is empty or an
 * upload is still out. A press of a held Reply still submits the form, and the form's submit sends
 * nothing while Reply is held, whatever holds it.
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
  const [body, setBody] = useReplyDraft(ask.id);
  // Keyed by the ask, so the form's own send outlives the composer that started it and any
  // composer for the ask sees it in flight.
  const mutationKey = ["ask-reply", ask.id];
  const pendingPress = usePendingKeyedPress(mutationKey);
  // A file's text joins the draft of the ask it was pasted or dropped under, which its press
  // carries (`useDraftUpload`): a host may draw this composer for another ask before it lands.
  const upload = useDraftUpload((text: string, askId: string) =>
    setReplyDraft(askId, (current) => appendToDraft(current, text))
  );
  // An upload holds Reply only within its deadline (`DraftUpload.holding`).
  const uploading = upload.holding > 0;
  const question = useQuestionLabel(ask);
  // The form shows a refused reply itself, with Retry, under the draft it kept. Each press
  // carries the ask it replies to, as an answer's does (`useAskAnswerForm`): where it posts, and
  // the draft and the thread its landing clears and reads again, are that ask's, whichever ask a
  // host draws this composer for by then. A reply leaves the ask's row in the Inbox's read.
  const options: OutstandingAnswerWriteOptions<Comment, ReplyPress> = {
    // The question goes in as its plain-text projection, the words the card draws: the reading
    // list's `write-label` (`DISPATCH_READING_SURFACES`) says why. Each press carries the line as
    // it reads at the press, while the composer is drawn for that ask, so a Retry made once a host
    // has drawn the composer for the next one names the question it answers.
    label: (press) => `Reply on “${press.questionWords}”`,
    method: "POST",
    removesRow: () => false,
    write: (press, bounds) => {
      if (press.ask.issue_key === null) {
        return api.createArtifactComment(
          askArtifactId(press.ask),
          { ask_id: press.ask.id, body: press.text },
          bounds
        );
      }
      return reply(press.ask.issue_key, { ask_id: press.ask.id, body: press.text }, bounds);
    },
    onSuccess: (_comment, press) => {
      clearReplyDraft(press.ask.id, press.text);
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", press.ask.id] });
    },
  };
  const own = useKeyedWrite({
    inline: true,
    label: options.label,
    method: options.method,
    mutationKey,
    write: options.write,
    readsLate: true,
    onSuccess: options.onSuccess,
  });
  const inbox = useInboxAnswerWrite(useContext(AskReplyWritesContext), ask.id, options);
  const submit: OutstandingAnswerWrite<ReplyPress> = inbox ?? own;
  const sending = inbox === undefined ? pendingPress.isPending : inbox.isPending;
  const late = inbox === undefined ? own.late || pendingPress.late : inbox.late;
  const held = useHeldThroughAnswer(sending && !late);
  // One reply out at a time, the Inbox's included: past the deadline a press while the reply is
  // still out sends nothing and says so.
  const oneReply = useOneCreateOut(inbox ?? pendingPress);
  // Reply is held while the send is out, while nothing is typed, and while a picture is still
  // uploading: a reply sent then would go without it.
  const replyHeld = held || body.trim() === "" || uploading;
  const refuseReply = () =>
    oneReply.refuse() ||
    (inbox === undefined && queryClient.isMutating({ exact: true, mutationKey }) > 0);

  const submitReply = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    // A held Reply is only `aria-disabled`, so its press still submits the form: that submit
    // sends nothing.
    if (replyHeld) return;
    if (refuseReply()) return;
    submit.mutate({ ask, questionWords: question.line(), text: body.trim() });
  };

  return (
    <form className="flex flex-col gap-2" onSubmit={submitReply}>
      {question.reads}
      {/* `replyBox`: the box a page's reply key hands the caret (the Inbox's `r`). */}
      <MentionField
        body={body}
        className="aria-disabled:opacity-50"
        held={held}
        issueKey={ask.issue_key ?? undefined}
        label="Reply"
        labelClassName={`block text-sm font-medium ${textSecondaryOnCanvas}`}
        onChange={(change) => setBody(change.body)}
        onDrop={
          uploadOwner === undefined
            ? undefined
            : (event) => {
                if (!held) upload.drop(event, uploadOwner, ask.id);
              }
        }
        onKeyDown={(event) => submitOnModifiedEnter(event, { disabled: uploading || held })}
        onPaste={
          uploadOwner === undefined
            ? undefined
            : (event) => {
                if (!held) upload.paste(event, uploadOwner, ask.id);
              }
        }
        replyBox
        writing={dispatchWritingBox("ask-thread-reply", "comment")}
      />
      <DraftUploadStatus upload={upload} />
      <button
        aria-disabled={replyHeld || undefined}
        className={`self-start rounded-lg border px-3 py-1.5 text-sm font-medium ${borderStrong} ${textSecondaryOnSurface} ${enabledCardHoverBorder} refused:cursor-not-allowed refused:opacity-50`}
        type="submit"
      >
        {sending ? "Replying…" : "Reply"}
      </button>
      {oneReply.refused ? (
        <p className={`text-sm ${textSecondaryOnCanvas}`} role="status">
          Reply sent nothing: your reply is still being sent. It shows in the thread once Dispatch
          posts it. If it fails, you can press Reply again.
        </p>
      ) : null}
      {submit.error === null ? null : (
        <QueryError
          message={replyRefusal(submit)}
          onRetry={() => {
            if (!held && !refuseReply()) submit.retry();
          }}
          retrying={held}
        />
      )}
    </form>
  );
}

export interface AskThreadProps {
  ask: Ask;
  /** The shared ask-thread query AskCard owns; this component never requests thread data itself. */
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
  /** The order the thread reads in: the reader's own choice, which the card holds
   *  (`threads/thread-order.ts`). */
  order: ThreadOrder;
  /** The ids of the replies the reader has not read (`unreadWrites` in
   *  `threads/thread-layout.ts`): an unread reply is never folded away. */
  unread?: ReadonlySet<string>;
}

/**
 * The reply thread under a question: every comment that replies directly to the ask
 * (Comment.ask_id) plus their own reply chains, in the reader's thread order, with the replies
 * outside the newest two folded behind **Show N earlier replies** (`threads/thread-layout.ts`,
 * drawn by `threads/EarlierReplies.tsx`, the fold control the Inbox's thread body shares).
 * An answered or resolved ask keeps a plain reply composer, next to the newest message
 * (`composerFirst`), and `takesReply` says which asks do; an open ask reserves its single
 * composer for answering or asking back, and draws a reply the reader had not sent before it
 * opened again read-only in the composer's place (`useUnsentReply`).
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
  order,
  unread,
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
          />
        ))}
      </ul>
    );
  // The fold and what it holds are one block, drawn furthest from the newest message: under the
  // question in Oldest first, at the foot of the thread in Newest first.
  const fold = (
    <EarlierReplies
      count={folded.length}
      marks={inboxActionMark("inbox.earlier-replies")}
      onToggle={() => setShowAllReplies(!showAll)}
      open={showAll}
      replies={(id) => (
        <ul className="space-y-2" id={id}>
          {folded.map((comment) => (
            <AskReply comment={comment} current={comment.id === currentReplyId} key={comment.id} />
          ))}
        </ul>
      )}
    />
  );
  const unsentReply = useUnsentReply(ask);
  const composer = !showComposer ? null : takesReply(ask) ? (
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
