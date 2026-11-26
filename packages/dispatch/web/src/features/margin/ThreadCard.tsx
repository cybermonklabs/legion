import { hashKey, type MutationKey } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useState } from "react";
import { Link } from "react-router-dom";

import type { Suggestion } from "../../api/types";
import { Pill } from "../../components/Pill";
import { QueryError } from "../../components/QueryError";
import { useCopyFeedback } from "../../hooks/useCopyFeedback";
import {
  borderDefault,
  calloutDangerBorder,
  calloutSuccessBorder,
  card,
  dangerHoverText,
  dangerText,
  inlineWarningText,
  linkHoverText,
  linkText,
  quoteAccentBorder,
  quoteBodyText,
  selectedCardBg,
  selectedCardBorder,
  successHoverText,
  successText,
  suggestionAddedBg,
  suggestionAddedText,
  suggestionRemovedBg,
  suggestionRemovedText,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useHeldSend, useHeldSends, useHeldSendsUnder } from "../conversation/held-sends";
import { KeptReplyToDeleted } from "../conversation/KeptDraftOffers";
import { KeptReply } from "../conversation/KeptReply";
import { MentionComposer } from "../conversation/MentionComposer";
import { passageItem, ReplyQuote } from "../conversation/ReplyQuote";
import type { SaveCommentEdit } from "../conversation/send-request";
import { useUnsentDrafts } from "../conversation/unsent-drafts";
import { composerNameOf } from "../conversation/useKeptDrafts";
import { AgentName } from "../refs/AgentName";
import { EMPTY_AGENTS } from "../refs/agent-registry";
import { CopyRefButton } from "../refs/CopyRefButton";
import { MarkdownBody } from "../refs/MarkdownBody";
import { PostBody, PostPreview } from "../refs/PostBody";
import { EditedMark } from "../refs/PostMarks";
import { type PostDeletion, postDeletion } from "../refs/post-marks";
import { ReferenceHostIs } from "../refs/reference-host";
import { buildIssuePath, buildProjectPath, itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { threadLayout } from "../threads/thread-layout";
import { useThreadOrder } from "../threads/thread-order";
import { useMarkThreadRead } from "../threads/useMarkThreadRead";
import { useMargin } from "./margin-context";
import type { CommentActionFailure } from "./useCommentActionQueue";
import {
  composerOwner,
  type MarginItemAction,
  type MarginOwner,
  type Thread,
  type ThreadComment,
} from "./useMarginItems";

/** The name every comment edit's save sits beneath, followed by the comment's id. */
const COMMENT_EDIT_SEND_KEY: MutationKey = ["comment-edit"];

/** 44 px tall through tablet widths (the compact sheet's `min-height: 44px` rule in `styles.css`
 *  holds until `xl`), 32 px in the desktop margin. */
const suggestionActionButton =
  "min-h-11 rounded-lg border px-3 py-2 text-xs font-semibold disabled:cursor-not-allowed xl:min-h-8 xl:py-1";

/** The replacement a suggestion proposes: the anchored text struck through, the new text
 *  beneath. `clamp` keeps a collapsed card's preview to two lines of each.
 *
 *  A diff is one of the four shapes a host can have (`ReferenceHostIs`): what it swaps is the
 *  exact characters of each side, so a `dispatch://` address in either reads as those
 *  characters and no title is drawn over it. Accepting the suggestion writes those characters
 *  into the document, and a reader has to see what they would be. */
function SuggestionDiff({
  clamp,
  quote,
  suggestion,
}: {
  clamp: boolean;
  quote: string;
  suggestion: Suggestion;
}): ReactNode {
  // The clamp sits on the padded `<del>`/`<ins>` itself: `overflow: hidden` clips at the padding
  // edge, not the content edge, so a clamped element with vertical padding can show the top of
  // the line it cut - a fragment of a third line under the ellipsis. Moving the clamp onto an
  // inner span (leaving the padding on the outer element) avoids that.
  const clampClass = clamp ? "line-clamp-2" : "";
  return (
    // No `source` on either side: why a suggestion's two sides are no record is the reading
    // list's `margin-suggestion-diff` (`DISPATCH_READING_SURFACES`).
    <ReferenceHostIs host="diff">
      <del
        className={`block rounded px-2 py-1 ${clampClass} ${suggestionRemovedBg} ${suggestionRemovedText}`}
      >
        <MarkdownBody markdown={quote} surface="margin-suggestion-diff" variant="inline" />
      </del>
      <ins
        className={`block rounded px-2 py-1 ${clampClass} ${suggestionAddedBg} ${suggestionAddedText}`}
      >
        <MarkdownBody
          markdown={suggestion.replace_with}
          surface="margin-suggestion-diff"
          variant="inline"
        />
      </ins>
    </ReferenceHostIs>
  );
}

export interface ThreadCardProps {
  actionFailure: CommentActionFailure | undefined;
  artifactSlug: string | undefined;
  className?: string;
  /** The reply a deep link named, which only the Conversation passes: that reply's row carries
   *  `aria-current`, as the card itself does when the link names its root. */
  currentReplyId?: string;
  composerClassName?: string;
  /** Another composer answers this thread - the Conversation's docked composer, or a phone
   *  thread's own - so the card's reply composer stays mounted, hidden, keeping an unsent draft
   *  for when that reply ends. */
  hideReplyComposer?: boolean;
  expanded: boolean;
  /** The card stands open for as long as it is drawn, as the margin of comments on text draws each
   *  thread (INFRA-5706, R4): the host passes `expanded` with a toggle that does nothing, and the
   *  card draws no control that would collapse it. Its reply composer is the thread's standing
   *  box (`MentionComposer`'s `standing`): it offers no Cancel reply, and it takes no focus for
   *  itself, neither as it mounts nor as a read draws the card again, so a card drawn beside the
   *  page moves neither the reader's focus nor the page's scroll. */
  fixedOpen?: boolean;
  hovered: boolean;
  isClosed: boolean;
  onAction(id: string, action: MarginItemAction): void;
  onEdit: SaveCommentEdit;
  onRetryAction(): void;
  onSelect?(): void;
  onToggle(): void;
  owner: MarginOwner;
  /** This thread's action is saving, or waiting its turn behind another thread's save. The
   *  buttons stay enabled meanwhile - a disabled button drops the focus it holds and turns a
   *  click into a silent no-op; the margin queues the click instead. */
  pendingAction: boolean;
  /** Renders the root and reply delivery attempts where the owner has event-sourced deliveries. */
  renderDeliveries?(comment: ThreadComment): ReactNode;
  /** The host's own control that opens this thread again, which the card names when it keeps a
   *  reply the reader left - the Conversation's Expand thread. A card with none draws a Reply of
   *  its own beside the line. */
  reopenControl?: string;
  /** `false` draws no Resolve and no Reopen: a thread whose root is a message, which no route of a
   *  message resolves, or a reply with a quote, whose resolution nobody could undo, since the
   *  server reopens a thread's root alone (INFRA-5706, R4). */
  rootActions?: boolean;
  /** Names the reply composer's send and the thread the held-send store holds it for, so the
   *  reply composer that mounts whenever the card expands shows a reply still out, or its refusal
   *  and draft, whatever unmounted the last one; and a host that holds its own Replies while one
   *  of its sends is out (the Conversation tab) holds them for this one too. */
  replyMutationKey?: MutationKey;
  /** Conversation owns the turn-level copy control outside this card. */
  showReference?: boolean;
  /** Document margins pulse an orphaned block; timeline cards link to the document instead. */
  pulseOrphanBlock?: boolean;
  /** The thread named by the current document URL; it can be selected without expanding on phone. */
  selected?: boolean;
  thread: Thread;
  viewerLogin: string;
  /** The comment whose inline editor is open; owned by the host - the margin, the Conversation by
   *  thread - so a card drawn again keeps an edit in progress: one that moves between the anchored
   *  and discussion lists, or a phone thread view opened again. The editor keeps what the reader
   *  wrote while the card is closed or gone (`useKeptDrafts`'s `awayName`). */
  editingCommentId: string | undefined;
  /** The comment whose edit save is in flight; owned with the shared editor state. */
  savingCommentEditId: string | undefined;
  onEditingChange(id: string | undefined): void;
}

/** The collapsed card's preview of its root, and beside it the ` · edited` the expanded card's
 *  byline carries: a collapsed card draws no byline, so the mark sits after the text it marks,
 *  outside the clamp that would cut it. */
function CollapsedRootPreview({
  className = "",
  comment,
  deleted,
  lines,
}: {
  className?: string;
  comment: ThreadComment;
  deleted: PostDeletion | null;
  lines: 1 | 2;
}): ReactNode {
  return (
    <span className={`flex min-w-0 items-baseline gap-1 ${className}`}>
      <PostPreview
        className="min-w-0 flex-1"
        deleted={deleted}
        lines={lines}
        markdown={comment.body}
        source={textSourceOf("comment", comment.id, comment.issue_key)}
        surface="margin-thread-preview"
        tree={comment.body_tree}
      />
      <EditedMark
        at={comment.edited_at}
        className={`shrink-0 text-xs ${textMutedOnSurfaceMuted}`}
        deleted={deleted}
      />
    </span>
  );
}

function CommentBody({
  artifactSlug,
  comment,
  owner,
  showOrphan,
  showReference,
}: {
  artifactSlug: string | undefined;
  comment: ThreadComment;
  owner: MarginOwner;
  showOrphan: boolean;
  showReference: boolean;
}): ReactNode {
  const anchor = comment.anchor;
  const deleted = postDeletion(comment);
  const suggestion = comment.suggestion;
  const reference = itemRoute("comment", comment, owner.kind === "document" ? owner : undefined);
  let orphanNotice: ReactNode = null;
  if (showOrphan && anchor?.orphaned) {
    const originalPath =
      comment.issue_key === null
        ? owner.kind === "document"
          ? buildProjectPath({
              kind: "document",
              project: owner.project,
              slug: owner.slug,
              version: anchor.version,
            })
          : undefined
        : artifactSlug === undefined
          ? undefined
          : buildIssuePath({
              key: comment.issue_key,
              kind: "artifact",
              slug: artifactSlug,
              version: anchor.version,
            });
    orphanNotice =
      originalPath === undefined ? null : (
        <p className={`mb-2 text-xs font-medium ${inlineWarningText}`}>
          Text changed.{" "}
          <Link className="underline" to={`${originalPath}&comment=${comment.id}`}>
            View original text
          </Link>
        </p>
      );
  }
  // A thread rooted in a reply that quotes words of the post it answers keeps no document version
  // to open: once those words left that post's text (`reply_block.removed`), the notice says the
  // text changed, with no link (INFRA-5706, R4). No root of today's margin is such a reply.
  if (showOrphan && orphanNotice === null && anchor?.orphaned && comment.reply_block?.removed) {
    orphanNotice = <p className={`mb-2 text-xs font-medium ${inlineWarningText}`}>Text changed.</p>;
  }
  return (
    <>
      {anchor === null ? null : (
        <blockquote className={`mb-2 border-l-2 pl-2 ${quoteAccentBorder} ${quoteBodyText}`}>
          {/* The document's own words, as they are: the reading list's `anchor-quote`. */}
          {anchor.quote}
        </blockquote>
      )}
      {orphanNotice}
      {suggestion === null ? (
        <PostBody
          deleted={deleted}
          markdown={comment.body}
          source={textSourceOf("comment", comment.id, comment.issue_key)}
          surface="margin-comment-body"
          tree={comment.body_tree}
        />
      ) : null}
      {suggestion !== null && anchor !== null ? (
        <div className="space-y-1 font-mono text-xs">
          <SuggestionDiff clamp={false} quote={anchor.quote} suggestion={suggestion} />
          {comment.body === "Suggested replacement." ? null : (
            <div className={`font-sans ${textSecondaryOnSurface}`}>
              <PostBody
                deleted={deleted}
                markdown={comment.body}
                source={textSourceOf("comment", comment.id, comment.issue_key)}
                surface="margin-comment-body"
                tree={comment.body_tree}
              />
            </div>
          )}
        </div>
      ) : null}
      <p className={`mt-2 flex flex-wrap items-center gap-x-1 text-xs ${textMutedOnSurfaceMuted}`}>
        <span>
          <AgentName actor={comment.author} inline /> · <Timestamp at={comment.created_at} />
          <EditedMark at={comment.edited_at} deleted={deleted} />
        </span>
        {showReference && reference !== undefined ? <CopyRefButton route={reference} /> : null}
      </p>
    </>
  );
}

/**
 * The edit the reader kept for a comment its author has since deleted. Dispatch saves no edit of
 * a deleted post (`409 POST_DELETED`), so the card opens no editor over one (`editing`): the
 * comment reads as deleted where its body was, never as an empty field or the words it had. The
 * edit waits as the reader's own - a save Dispatch refused or the reader stopped, or what the
 * editor held as the delete came, which it kept for the comment as it went (`useKeptDrafts`'
 * `awayName`) - and the card says it is kept in this browser, drawing none of its words: an edit
 * is mostly the words of the comment it was written over, and the delete took those off every
 * surface (INFRA-1523, R3). Copy my edit puts the whole edit on the clipboard, and a copy the
 * browser refuses says so and keeps the edit; Discard my edit drops it where it is kept. Nothing
 * is saved meanwhile, and a save still out draws nothing until Dispatch answers it.
 */
function DeletedCommentEdit({ commentId }: { commentId: string }): ReactNode {
  const mutationKey = [...COMMENT_EDIT_SEND_KEY, commentId];
  const store = useHeldSends();
  const held = useHeldSend(mutationKey);
  const unsent = useUnsentDrafts();
  const { copy, status } = useCopyFeedback();
  const name = composerNameOf(mutationKey);
  // An edit's address is the comment its send name names, so its draft is kept for no reply and
  // no words of a document.
  const kept = unsent
    .useAway(name)
    .find((away) => away.replyTo === null && away.anchor === undefined);
  const refused = held?.status === "refused" ? held : undefined;
  const draft = refused?.draft.draft ?? (held === undefined ? kept?.draft : undefined);
  if (draft === undefined) return null;
  return (
    <div className="mt-2 space-y-2">
      <div className={`space-y-1 text-sm ${textMutedOnSurfaceMuted}`} role="status">
        <p>
          This comment was deleted, and Dispatch saves no edit of a deleted comment. Your unsaved
          edit is kept in this browser.
        </p>
        {status === "copied" ? <p>Your edit is copied.</p> : null}
        {typeof status === "object" ? (
          <p className={dangerText}>
            Your edit was not copied: this browser refused the clipboard. It is still kept here.
          </p>
        ) : null}
      </div>
      <div className="flex flex-wrap gap-3">
        <button
          className={`text-sm font-medium ${linkText} ${linkHoverText}`}
          onClick={() => copy(draft.body)}
          type="button"
        >
          Copy my edit
        </button>
        <button
          className={`text-sm font-medium underline ${dangerText}`}
          onClick={() => {
            if (refused !== undefined) store.discard(mutationKey);
            else unsent.forgetAway(name, { anchor: undefined, replyTo: null });
          }}
          type="button"
        >
          Discard my edit
        </button>
      </div>
    </div>
  );
}

export function ThreadCard({
  actionFailure,
  artifactSlug,
  className,
  currentReplyId,
  composerClassName,
  hideReplyComposer = false,
  expanded,
  fixedOpen = false,
  hovered,
  isClosed,
  onAction,
  onEdit,
  onRetryAction,
  onSelect,
  onToggle,
  owner,
  selected = false,
  showReference = true,
  pendingAction,
  pulseOrphanBlock = true,
  renderDeliveries,
  reopenControl,
  rootActions = true,
  replyMutationKey,
  thread,
  viewerLogin,
  editingCommentId: editingId,
  savingCommentEditId,
  onEditingChange: setEditingId,
}: ThreadCardProps): ReactNode {
  const { focusBlock } = useMargin();
  const active = expanded || selected;
  const root = thread.root.comment;
  const rootDeleted = postDeletion(root);
  const rootSuggestion = root.suggestion;
  const terminalSuggestion = rootSuggestion !== null && rootSuggestion.accepted !== null;
  // The server refuses accept and reject on an orphaned anchor (409 ANCHOR_ORPHANED), so the
  // card offers neither; the thread is closed with Resolve like a comment.
  const actionableSuggestion =
    !isClosed &&
    rootSuggestion !== null &&
    rootSuggestion.accepted === null &&
    !thread.resolved &&
    root.anchor?.orphaned !== true;
  const resolutionLabel =
    rootSuggestion?.accepted === true
      ? "Accepted"
      : rootSuggestion?.accepted === false
        ? "Rejected"
        : "Resolved";
  // The shared thread read mark: an open thread is read once its newest reply has been on screen.
  const newestReply = thread.replies.at(-1);
  const markRead = useMarkThreadRead({
    enabled: expanded,
    newestShownAt: newestReply?.created_at,
    owner: owner.kind === "issue" ? { issue: owner.key } : { artifact: owner.artifactId },
    threadId: root.id,
  }).ref;
  // A margin thread folds nothing - it is the thread's own page - so the order alone places its
  // replies and the reply box (`features/threads/thread-layout.ts`).
  const [threadOrder] = useThreadOrder();
  const { composerFirst, shown } = threadLayout({
    collapse: false,
    order: threadOrder,
    replies: thread.replies,
  });
  // The thread's posts, root first: the post a reply quotes is looked up among them.
  const threadItems = [root, ...thread.replies];
  // An edit whose save is out, or was refused, is the held-send store's: its editor stays open on
  // its comment until the save lands or the reader drops it, whatever closed the card meanwhile.
  // A comment its author deleted takes no edit (`409 POST_DELETED`), so no editor opens over it,
  // whatever held one open: the comment reads as deleted, and an edit kept for it waits under it
  // (`DeletedCommentEdit`).
  const heldEdits = useHeldSendsUnder(COMMENT_EDIT_SEND_KEY);
  const editing = (comment: ThreadComment) =>
    postDeletion(comment) === null &&
    (editingId === comment.id || heldEdits.some((held) => held.request.edit?.id === comment.id));
  const renderEditor = (comment: ThreadComment) => (
    <MentionComposer
      box="margin-comment-edit"
      edit={{ body: comment.body, id: comment.id }}
      kind="comment"
      mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}
      onClose={() => setEditingId(undefined)}
      onSent={() => setEditingId(undefined)}
      owner={composerOwner(owner)}
      saveEdit={onEdit}
    />
  );
  /* A deleted post takes no new text: the server refuses an edit of one (409), so its author is
     offered none and the editor never opens on an empty body. */
  const editButton = (comment: ThreadComment) =>
    savingCommentEditId === undefined &&
    !isClosed &&
    postDeletion(comment) === null &&
    comment.author.kind === "user" &&
    comment.author.id === viewerLogin ? (
      <button
        className={`font-medium ${linkText} ${linkHoverText}`}
        onClick={() => setEditingId(comment.id)}
        type="button"
      >
        Edit
      </button>
    ) : null;
  // A message thread's reply writes a message in that thread (`in_reply_to`), as a thread on text
  // rooted in a message takes one (INFRA-5706, R4).
  const messageReply =
    thread.kind === "message"
      ? {
          replyTo: { actor: root.author, excerpt: "", id: root.id, parentKind: "message" as const },
        }
      : {};
  // A fixed-open card's reply composer stands open from the load (INFRA-5706, R4), so it is handed
  // no agents, and reads no list of them for its mention picker, until the reader uses it: the
  // focus in a composer of the card, or a reply of theirs that the held-send store or the kept
  // drafts hold under the thread's reply send name. So a page of such cards sends no read that the
  // page without them does not (R9). Every other card's reply composer reads the list as it mounts.
  const [replyActive, setReplyActive] = useState(false);
  const replyHeld = useHeldSend(replyMutationKey) !== undefined;
  const replyKept = useUnsentDrafts().useKept(
    replyMutationKey === undefined ? "" : hashKey(replyMutationKey)
  );
  const hasReplyDraft = replyHeld || replyKept;
  /* Mounted while the card is expanded, under the thread's reply send name, so a reply still
     out or refused - which the held-send store keeps for the thread - is there each time the
     card expands. A closed issue takes a reply as it takes any comment, so the composer shows
     and sends there too (`closed` only hides the kinds a closed issue refuses), while Resolve,
     Reopen, Edit, Accept and Reject stay hidden; on a decided suggestion's thread, which offers
     no reply, it shows only a reply of its own still out or its refusal, and still sends
     (`finishing`). A deleted root takes no reply either - the server refuses one (409) - so the
     composer stays away from it. Its quote of the root marks the root edited as the root's own
     byline does (`replyParent`). `contents`, so the composer's own box is the one laid out. It
     sits next to the newest message: under the root in Newest first, at the foot of the thread
     in Oldest first. Its Cancel reply, and Escape in its field, close the thread and keep the
     draft for it, typed or refused or stopped, which the closed card says (`keptReply`). It
     answers the card's own comment alone, which leaves the page with the card, so no line of it
     can outlive the comment it names, and it is handed no `listed`. */
  const replyComposer =
    expanded && rootDeleted === null ? (
      <div className="contents" hidden={hideReplyComposer}>
        <MentionComposer
          agents={!fixedOpen || replyActive || hasReplyDraft ? undefined : EMPTY_AGENTS}
          box="margin-thread-reply"
          closed={isClosed}
          finishing={terminalSuggestion}
          frame={composerClassName}
          inline
          kind="comment"
          mutationKey={replyMutationKey}
          onCancelReply={onToggle}
          onClose={onToggle}
          onSent={() => {}}
          owner={composerOwner(owner)}
          replyParent={root}
          replyTo={{ actor: root.author, excerpt: "", id: root.id, parentKind: "comment" }}
          standing={fixedOpen}
          {...messageReply}
        />
      </div>
    ) : null;
  // A reply the card keeps while it is closed - typed and never sent, or a send Dispatch refused
  // or the reader stopped - is out of sight with its composer, under the thread's reply send name
  // (`MentionComposer`'s `inline`). The card says so, whose comment it answers and how to go on
  // with it, until the reader opens the thread again: nothing is sent until they press. Under a
  // root its author deleted the card draws no reply composer, so no Reply goes on with the reply:
  // its own words are drawn instead, open or closed, saying the comment was deleted and giving
  // no word of it, with Discard my draft (`KeptReplyToDeleted`).
  const heldReply = useHeldSend(replyMutationKey);
  const unsentReply = useUnsentDrafts().useKept(
    replyMutationKey === undefined ? "" : hashKey(replyMutationKey)
  );
  const keptReply =
    rootDeleted === null && !expanded && (heldReply?.status === "refused" || unsentReply) ? (
      <div className="mt-2 flex flex-wrap items-center gap-3">
        <p className={`text-sm ${textMutedOnSurfaceMuted}`} role="status">
          <KeptReply actor={root.author} press={reopenControl ?? "Reply"} />
        </p>
        {reopenControl === undefined ? (
          <button
            className={`font-medium ${linkText} ${linkHoverText}`}
            onClick={onToggle}
            type="button"
          >
            Reply
          </button>
        ) : null}
      </div>
    ) : null;
  const keptReplyToDeleted =
    rootDeleted === null || replyMutationKey === undefined ? null : (
      <KeptReplyToDeleted actor={root.author} mutationKey={replyMutationKey} parentKind="comment" />
    );

  return (
    <>
      {/* biome-ignore lint/a11y/useAriaPropsSupportedByRole: the card exposes its expand/collapse state to the margin bridge */}
      <article
        aria-current={active ? "true" : undefined}
        aria-busy={pendingAction || undefined}
        aria-expanded={expanded}
        className={`rounded-xl border p-3 text-sm shadow-sm ${
          active || hovered ? `${selectedCardBorder} ${selectedCardBg}` : card
        } ${className ?? ""}`}
        data-hovered={hovered ? "true" : undefined}
        data-anchor-block={thread.anchor?.block_id ?? undefined}
        onClickCapture={() => {
          if (
            pulseOrphanBlock &&
            thread.anchor?.orphaned &&
            typeof thread.anchor.block_id === "string"
          ) {
            focusBlock(thread.anchor.block_id);
          }
        }}
        // The focus entering a composer of a fixed-open card - its reply, or an editor the reader
        // opened - is the reader using it, from which its reply composer reads the agents' list.
        onFocusCapture={(event) => {
          if (
            fixedOpen &&
            event.target instanceof Element &&
            event.target.closest("form") !== null
          ) {
            setReplyActive(true);
          }
        }}
        onKeyDown={(event) => {
          if (
            event.target !== event.currentTarget ||
            (event.key !== "Enter" && event.key !== " ")
          ) {
            return;
          }
          event.preventDefault();
          onSelect?.();
        }}
        onClick={(event) => {
          if (
            event.target instanceof Element &&
            event.target.closest("a, button, input, label, select, textarea") !== null
          ) {
            return;
          }
          if (
            pulseOrphanBlock &&
            thread.anchor?.orphaned &&
            typeof thread.anchor.block_id === "string"
          ) {
            focusBlock(thread.anchor.block_id);
          }
          event.stopPropagation();
          onSelect?.();
        }}
        data-margin-item={thread.key}
        data-testid={`margin-comment-${thread.key}`}
      >
        {rootSuggestion === null ? null : (
          <header className="mb-2 flex flex-wrap items-center justify-between gap-2">
            <Pill>Suggestion</Pill>
            {actionableSuggestion ? (
              <span className="flex items-center gap-2">
                <button
                  aria-label="Accept suggestion"
                  className={`${suggestionActionButton} ${calloutSuccessBorder} ${successText} ${successHoverText}`}
                  onClick={() => onAction(root.id, "accept")}
                  type="button"
                >
                  Accept
                </button>
                <button
                  aria-label="Reject suggestion"
                  className={`${suggestionActionButton} ${calloutDangerBorder} ${dangerText} ${dangerHoverText}`}
                  onClick={() => onAction(root.id, "reject")}
                  type="button"
                >
                  Reject
                </button>
              </span>
            ) : null}
          </header>
        )}
        {expanded ? (
          <>
            {editing(root) ? (
              renderEditor(root)
            ) : (
              <>
                <CommentBody
                  artifactSlug={artifactSlug}
                  comment={root}
                  owner={owner}
                  showOrphan
                  showReference={showReference}
                />
                {rootDeleted === null ? null : <DeletedCommentEdit commentId={root.id} />}
                {renderDeliveries?.(root)}
              </>
            )}
            {isClosed ? null : (
              <div className="mt-2 flex flex-wrap items-center gap-3 text-sm">
                {actionableSuggestion ? null : thread.resolved && !terminalSuggestion ? (
                  <button
                    className={`font-medium ${linkText} ${linkHoverText}`}
                    onClick={() => onAction(root.id, "reopen")}
                    hidden={rootActions ? undefined : true}
                    type="button"
                  >
                    Reopen
                  </button>
                ) : !thread.resolved ? (
                  <button
                    className={`font-medium ${linkText} ${linkHoverText}`}
                    onClick={() => onAction(root.id, "resolve")}
                    hidden={rootActions ? undefined : true}
                    type="button"
                  >
                    Resolve
                  </button>
                ) : null}
                {editButton(root)}
                {pendingAction ? (
                  <span className={`text-xs ${textMutedOnSurfaceMuted}`} role="status">
                    Saving…
                  </span>
                ) : null}
              </div>
            )}
            {thread.resolved && root.resolved_by !== null && root.resolved_at !== null ? (
              <p className={`mt-2 text-xs ${textMutedOnSurfaceMuted}`}>
                {resolutionLabel} by <AgentName actor={root.resolved_by} inline /> ·{" "}
                <Timestamp at={root.resolved_at} />
              </p>
            ) : null}
            {composerFirst ? replyComposer : null}
            {shown.length === 0 ? null : (
              <ol aria-label="Replies" className={`mt-3 space-y-2 border-t pt-3 ${borderDefault}`}>
                {shown.map((reply) => {
                  // The quoted post, read once in the thread's own items: its author names the
                  // quote, its deletion and its edit mark it, and the record its text is names
                  // the quote's source.
                  const quoted =
                    reply.reply_block == null
                      ? undefined
                      : passageItem(reply.reply_block, threadItems);
                  return (
                    <li
                      aria-current={reply.id === currentReplyId ? "true" : undefined}
                      className={`rounded-lg p-2 ${card}`}
                      data-reply=""
                      key={reply.id}
                      ref={reply === newestReply ? markRead : undefined}
                      style={{ marginLeft: "0px" }}
                    >
                      {editing(reply) ? (
                        renderEditor(reply)
                      ) : (
                        <>
                          {reply.reply_block == null ? null : (
                            <ReplyQuote
                              author={quoted?.author}
                              block={reply.reply_block}
                              className="mb-2"
                              deleted={postDeletion(quoted)}
                              edited={quoted?.edited_at}
                              excerpt=""
                              source={textSourceOf("comment", quoted?.id, quoted?.issue_key)}
                            />
                          )}
                          <CommentBody
                            artifactSlug={artifactSlug}
                            comment={reply}
                            owner={owner}
                            showOrphan={false}
                            showReference={showReference}
                          />
                          {postDeletion(reply) === null ? null : (
                            <DeletedCommentEdit commentId={reply.id} />
                          )}
                          {renderDeliveries?.(reply)}
                        </>
                      )}
                      {editing(reply) ? null : (
                        <div className="mt-2 flex gap-3 text-sm">{editButton(reply)}</div>
                      )}
                    </li>
                  );
                })}
              </ol>
            )}
          </>
        ) : (
          // The collapsed card is a record a reader reads - it is the Conversation tab's whole
          // default view of a turn - so its references are links, as they were before the host
          // decided it (`links="live"` at both previews below). The button around it opens the
          // thread; it is not the link-or-button shape R6 names, which is a reference drawn
          // inside a control that navigates somewhere else.
          <button
            aria-expanded={false}
            className={`block w-full text-left ${textPrimaryOnSurface}`}
            onClick={() => {
              if (
                pulseOrphanBlock &&
                thread.anchor?.orphaned &&
                typeof thread.anchor.block_id === "string"
              ) {
                focusBlock(thread.anchor.block_id);
              }
              onToggle();
            }}
            type="button"
          >
            {/* The compact sheet's stylesheet lays every button out inline-flex, so one block
                wrapper keeps the preview's rows stacked. */}
            <span className="block w-full">
              {rootSuggestion !== null && root.anchor !== null ? (
                <>
                  {root.anchor.orphaned ? (
                    <p className={`mb-2 text-xs font-medium ${inlineWarningText}`}>Text changed.</p>
                  ) : null}
                  <span className="block space-y-1 font-mono text-xs">
                    <SuggestionDiff clamp quote={root.anchor.quote} suggestion={rootSuggestion} />
                  </span>
                  {root.body === "Suggested replacement." ? null : (
                    <CollapsedRootPreview
                      className={`mt-2 text-xs ${textSecondaryOnSurface}`}
                      comment={root}
                      deleted={rootDeleted}
                      lines={1}
                    />
                  )}
                </>
              ) : (
                <>
                  {root.anchor === null ? null : (
                    <blockquote
                      className={`mb-2 border-l-2 pl-2 ${quoteAccentBorder} ${quoteBodyText}`}
                    >
                      {/* The document's own words: the reading list's `anchor-quote`. */}
                      {root.anchor.quote}
                    </blockquote>
                  )}
                  <CollapsedRootPreview comment={root} deleted={rootDeleted} lines={2} />
                </>
              )}
              {thread.replies.length === 0 ? null : (
                <p className={`mt-2 text-xs ${textMutedOnSurfaceMuted}`}>
                  {thread.replies.length} {thread.replies.length === 1 ? "reply" : "replies"} · last
                  reply{" "}
                  {thread.lastReplyAt === undefined ? null : <Timestamp at={thread.lastReplyAt} />}
                </p>
              )}
            </span>
          </button>
        )}
        {keptReply}
        {keptReplyToDeleted}
        {composerFirst ? null : replyComposer}
        {expanded ? null : renderDeliveries?.(root)}
        {actionFailure === undefined ? null : (
          <div className="mt-2">
            <QueryError
              message={actionFailure.message}
              onRetry={actionFailure.retryable ? onRetryAction : undefined}
              retrying={pendingAction}
            />
          </div>
        )}
      </article>
    </>
  );
}
