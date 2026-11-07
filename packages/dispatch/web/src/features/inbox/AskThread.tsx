import type { UseQueryResult } from "@tanstack/react-query";
import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { type ArtifactOwner, api, type WriteBounds } from "../../api/client";
import type { Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { useSending } from "../../hooks/useSending";
import {
  borderDefault,
  surfaceMutedBg,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurfaceMuted,
} from "../../theme/classes";
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
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { AskAnswerArea } from "./AskAnswerArea";
import { askCommentInput, postAskComment } from "./ask-comment";
import { emptyDraftRefusal } from "./draft-store";
import { clearReplyDraft, useReplyDraft } from "./reply-drafts";

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
  comment: Comment;
  current?: boolean;
  markRef?: DwellRef;
  /** The project document that owns a reply with no issue, so the reply can name its own
   *  `dispatch://` reference (`itemRoute`). */
  owner?: { project: string; slug: string };
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
        <MarkdownBody markdown={comment.body} source={textSourceOf("comment", comment.id)} />
      </div>
      <Unfurl body={comment.body} source={textSourceOf("comment", comment.id)} />
      <p
        className={`mt-1 flex flex-wrap items-center gap-x-1.5 text-xs ${textMutedOnSurfaceMuted}`}
      >
        <AgentName actor={comment.author} inline />
        <span aria-hidden="true">·</span>
        <Timestamp at={comment.created_at} />
        {/* A reply is a node like any other, so it carries the one copy control for its own
            reference (`dispatch://<KEY>/comment/<id>`), as a margin thread's comments do. */}
        {reference === undefined ? null : <CopyRefButton route={reference} />}
      </p>
    </li>
  );
}

/** AskCard owns the `["ask-thread", ask.id]` query, initializing it from an Inbox row when
 *  available; every card renders the shared thread without a duplicate read. */
export type AskThreadQuery = UseQueryResult<AskRead, Error>;

/**
 * The reply box under an answered ask, which is the same answer area as the open card's
 * (`AskAnswerArea.tsx`): the Conversation tab's field and `@` picker, with Reply as the one
 * button it sends by. It posts to the ask's issue, or to its document for a project-document
 * ask, then invalidates the shared thread so the reply shows first. A picked session rides with
 * the comment as a `session:` mention and gets it. Its draft is the ask's (`reply-drafts.ts`),
 * and its send is the ask's too: while any composer's reply to this ask is out, every composer
 * for it, including one mounted since, shows the draft disabled. A file pasted or dropped into
 * the field uploads to `uploadOwner` and its text joins the draft.
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
  const [draft, setDraft] = useReplyDraft(ask.id);
  // Keyed by the ask, so the send outlives the composer that started it and any composer for the
  // ask sees it in flight.
  const mutationKey = ["ask-reply", ask.id];
  const { sending } = useSending(mutationKey);
  // The form shows a refused reply itself, with Retry, under the draft it kept.
  const submit = useKeyedWrite({
    write: (input: CreateCommentInput, bounds) => postAskComment(ask, input, bounds, reply),
    inline: true,
    label: () => `Reply on “${ask.question}”`,
    mutationKey,
    onSuccess: (_comment, input) => {
      clearReplyDraft(ask.id, input.body);
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
    },
  });
  /** Why Reply refuses the draft as it stands: it posts what the box holds. */
  const refusal = emptyDraftRefusal(draft);

  // The field's own Ctrl/Cmd+Enter stands down while a picture is still uploading
  // (`AskAnswerArea`), and Reply says the upload is what holds it, so neither way in sends a
  // reply the picture would miss.
  const sendReply = () => {
    if (refusal !== null || sending) return;
    submit.mutate(askCommentInput(ask.id, draft));
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
      <AskAnswerArea
        actions={({ reasonId, uploading }) => (
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
        disabled={sending}
        draft={draft}
        label="Reply"
        onDraftChange={setDraft}
        refusals={{ reply: refusal }}
        uploadOwner={uploadOwner}
      />
      {submit.isError ? (
        <QueryError
          message="Could not post your reply."
          onRetry={() => {
            if (submit.variables !== undefined) {
              submit.mutate(submit.variables);
            }
          }}
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
  /** False where the caller renders the answered-ask composer itself: a collapsed thread
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
  owner?: { project: string; slug: string };
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
 * Answered asks keep a plain reply composer, at the end of that order; open asks reserve their
 * single composer for answering or asking back, and a resolved ask keeps its history without a
 * composer.
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
  const composer =
    showComposer && ask.state === "answered" ? (
      <AskReplyComposer ask={ask} createReply={reply} uploadOwner={uploadOwner} />
    ) : null;

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
