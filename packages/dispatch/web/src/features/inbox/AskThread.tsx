import type { UseQueryResult } from "@tanstack/react-query";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useContext, useId, useState } from "react";

import { type ArtifactOwner, api, type WriteBounds } from "../../api/client";
import type { Ask, AskRead, Comment, CommentWithTrees, CreateCommentInput } from "../../api/types";
import { heldField } from "../../components/held-press";
import { QueryError } from "../../components/QueryError";
import { useTooltips } from "../../components/tooltips";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useSending } from "../../hooks/useSending";
import {
  borderDefault,
  borderStrong,
  enabledCardHoverBorder,
  inputClasses,
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
import { ConversationTurns, turnFocusRing, turnMarks } from "../conversation/turn-keys";
import { AgentName } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
import { MarkdownBody } from "../refs/MarkdownBody";
import { itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { Unfurl } from "../refs/Unfurl";
import type { DwellRef } from "../shell/useDwell";
import { EarlierReplies } from "../threads/EarlierReplies";
import { threadLayout } from "../threads/thread-layout";
import type { ThreadOrder } from "../threads/thread-order";
import { stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { inboxActionMark } from "./inbox-action-mark";
import type { AskPress } from "./outstanding-answers";
import { askArtifactId } from "./owner-reads";
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
        <MarkdownBody
          markdown={comment.body}
          source={textSourceOf("comment", comment.id)}
          tree={comment.body_tree}
        />
      </div>
      <Unfurl body={comment.body} source={textSourceOf("comment", comment.id)} />
      <p className={`mt-1 flex flex-wrap items-center gap-x-1 text-xs ${textMutedOnSurfaceMuted}`}>
        <span>
          <AgentName actor={comment.author} inline /> · <Timestamp at={comment.created_at} />
        </span>
        {reference === undefined ? null : <CopyRefButton route={reference} />}
      </p>
    </li>
  );
}

/** One press of a reply: the ask it is of (`AskPress`) and the text it posts. */
interface ReplyPress extends AskPress {
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

/**
 * The reply form under an ask that is no longer open (`takesReply`). It posts to the ask's issue,
 * or to its document for a project-document ask, then invalidates the shared thread so the reply
 * shows first. Its draft is the ask's (`reply-drafts.ts`), and its send is the ask's too: while
 * any composer's reply to this ask is out, every composer for it, including one mounted since,
 * shows the draft held: the field and Reply keep the focus and their Tab stops and send nothing
 * more (`heldPress`). A file pasted or dropped into the field uploads to `uploadOwner` and its
 * text joins the draft.
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
  // Each composer owns its field id so transient duplicate mounts during a responsive
  // transition cannot share an ask-id-derived id.
  const fieldId = `${useId()}-reply`;
  const [body, setBody] = useReplyDraft(ask.id);
  // Keyed by the ask, so the send outlives the composer that started it and any composer for the
  // ask sees it in flight.
  const mutationKey = ["ask-reply", ask.id];
  const { sending } = useSending(mutationKey);
  // A file's text joins the draft of the ask it was pasted or dropped under, which its press
  // carries (`useDraftUpload`): a host may draw this composer for another ask before it lands.
  const upload = useDraftUpload((text: string, askId: string) =>
    setReplyDraft(askId, (current) => appendToDraft(current, text))
  );
  const uploading = upload.pending > 0;
  const held = sending;
  // The form shows a refused reply itself, with Retry, under the draft it kept. Each press
  // carries the ask it replies to, as an answer's does (`useAskAnswerForm`): where it posts, and
  // the draft and the thread its landing clears and reads again, are that ask's, whichever ask a
  // host draws this composer for by then.
  const submit = useKeyedWrite({
    method: "POST",
    write: (press: ReplyPress, bounds) => {
      if (press.ask.issue_key === null) {
        return api.createArtifactComment(
          askArtifactId(press.ask),
          { ask_id: press.ask.id, body: press.text },
          bounds
        );
      }
      return reply(press.ask.issue_key, { ask_id: press.ask.id, body: press.text }, bounds);
    },
    inline: true,
    label: (press: ReplyPress) => `Reply on “${press.ask.question}”`,
    mutationKey,
    onSuccess: (_comment, press) => {
      clearReplyDraft(press.ask.id, press.text);
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", press.ask.id] });
    },
  });

  const submitReply = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const text = body.trim();
    // A reply sent while a picture is still uploading would go without it.
    if (text === "" || sending || uploading) {
      return;
    }
    submit.mutate({ ask, text });
  };

  return (
    <form className="flex flex-col gap-2" onSubmit={submitReply}>
      <label className={`block text-sm font-medium ${textSecondaryOnCanvas}`} htmlFor={fieldId}>
        Reply
        {/* `data-reply-box`: the box a page's reply key hands the caret (the Inbox's `r`). */}
        <textarea
          {...heldField(held)}
          className={`mt-1 block w-full rounded-lg px-3 py-2 font-normal outline-none aria-disabled:opacity-50 ${inputClasses(true)}`}
          data-reply-box=""
          id={fieldId}
          onChange={(event) => setBody(event.target.value)}
          onDrop={
            uploadOwner === undefined || held
              ? undefined
              : (event) => upload.drop(event, uploadOwner, ask.id)
          }
          onKeyDown={(event) => submitOnModifiedEnter(event, { disabled: uploading })}
          onPaste={
            uploadOwner === undefined || held
              ? undefined
              : (event) => upload.paste(event, uploadOwner, ask.id)
          }
          value={body}
        />
      </label>
      <DraftUploadStatus upload={upload} />
      <button
        aria-disabled={held || undefined}
        className={`self-start rounded-lg border px-3 py-1.5 text-sm font-medium ${borderStrong} ${textSecondaryOnSurface} ${enabledCardHoverBorder} refused:cursor-not-allowed refused:opacity-50`}
        disabled={body.trim() === "" || uploading}
        type="submit"
      >
        {sending ? "Replying…" : "Reply"}
      </button>
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
