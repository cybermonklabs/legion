import { type ReactNode, useId, useState } from "react";

import { api, type DeclineAskInput, type WriteBounds } from "../../api/client";
import type {
  AnswerAskInput,
  Ask,
  AskRead,
  AskUrgency,
  Comment,
  CreateCommentInput,
} from "../../api/types";
import { CopyButton } from "../../components/CopyButton";
import { ChevronIcon } from "../../components/DisclosureToggle";
import { useTooltips } from "../../components/tooltips";
import { compareTimestamps } from "../../lib/timestamps";
import {
  askUrgencyAccent,
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  card,
  pageFailedText,
  pageHairBorder,
  pagePaperBg,
  pageQuietText,
  pageStateText,
  surfaceBg,
  textMutedHoverToSecondary,
  textMutedOnSurface,
} from "../../theme/classes";
import { AskBody, askUploadOwner } from "../ask-body/AskBody";
import { AskEditHistory, type AskFrame } from "../ask-body/AskCompletionCard";
import { ASK_CARD_WORDS, ASK_SHARED_WORDS } from "../ask-body/ask-body-words";
import { markdownText } from "../ask-body/ask-text";
import { CopyRefButton } from "../refs/CopyRefButton";
import { ReferencedBy, ReferencedByToggle } from "../refs/ReferencedBy";
import { buildDispatchReference, itemRoute } from "../refs/routes";
import { composerFirst } from "../threads/thread-layout";
import { useThreadOrder } from "../threads/thread-order";
import { askRepliesOwner, useMarkThreadRead } from "../threads/useMarkThreadRead";
import { AskRecipients } from "./AskRecipients";
import { AskThread } from "./AskThread";
import { AskThreadDisclosure } from "./AskThreadDisclosure";
import type { OwnedAsk } from "./ask-name";
import { askUrgency } from "./ask-urgency";
import { useAskAnswerForm } from "./useAskAnswerForm";

const answerAsk = (id: string, input: AnswerAskInput, bounds: WriteBounds): Promise<Ask> =>
  api.answerAsk(id, input, bounds);
const declineAsk = (id: string, input: DeclineAskInput, bounds: WriteBounds): Promise<Ask> =>
  api.declineAsk(id, input, bounds);
const getAskThread = (id: string): Promise<AskRead> => api.getAsk(id);
const createReply = (
  issueKey: string,
  input: CreateCommentInput,
  bounds: WriteBounds
): Promise<Comment> => api.createComment(issueKey, input, bounds);

export interface AskCardProps {
  artifactSlug?: string;
  ask: OwnedAsk;
  /** The project document that owns an ask whose read lacks `document` (only Inbox rows carry
   *  it), so the card can still name the ask's `dispatch://` reference. */
  owner?: { project: string; slug: string };
  thread?: "inline" | "collapsed";
  /** `compact` (the margin, a decision block and the Conversation) takes tighter padding
   *  (`px-3 pt-4 pb-3` against `px-4 pt-5 pb-4`). Nothing else depends on the variant: the
   *  answer box stands in place in both. */
  variant?: "compact" | "full";
  /** `card` (the default) draws the card: frame, urgency accent and notch, the question, and the
   *  link to a block ask's document. `block` is the card hosted inside its own decision block
   *  (`features/doc/AskBlockCard.tsx`): the block shell already draws the frame and accent, the
   *  editor shows the question, the block names its kind, and the block is the link's target,
   *  so the card renders only what the block lacks — who asked and when, the exchange, and the
   *  composer or the record. */
  frame?: AskFrame;
  answerAsk?: (id: string, input: AnswerAskInput, bounds: WriteBounds) => Promise<Ask>;
  declineAsk?: (id: string, input: DeclineAskInput, bounds: WriteBounds) => Promise<Ask>;
  /** Reply-thread fetch/write seams for tests; default to the real API. */
  getAskThread?: (id: string) => Promise<AskRead>;
  /** Data the Inbox response already hydrated for the first card render. */
  initialThread?: AskRead;
  /** Timestamp of the Inbox snapshot that supplied initialThread. */
  initialThreadUpdatedAt?: number;
  createReply?: (
    issueKey: string,
    input: CreateCommentInput,
    bounds: WriteBounds
  ) => Promise<Comment>;
  /** Whether an approval ask's label also links its document and version. Default `true`: the
   *  Inbox, the drawer and the Conversation name it. The margin passes `false` — the reader is
   *  already on that document, so the link would point at the page under it. */
  documentLink?: boolean;
  /** Called once the server has recorded the reader's answer from this card. */
  onAnswered?: (id: string) => void;
  /** `change` opens the seeded form when the viewer may replace the current answer. */
  mode?: "answer" | "change";
  /** Marks the ask's thread read through the shared thread read mark (`useMarkThreadRead`) once
   *  its newest write (the newest reply, or an answer given after every reply) has been on
   *  screen. Set only on the ask's own pages - its Conversation turn, its margin card, its
   *  decision block - so the Inbox, its drawer and the Answers page start no unread-set reads. */
  markRead?: boolean;
  /** The ids of the ask's replies the reader has not read, from a host that holds the fact
   *  itself: the Inbox, whose row carries the thread's read mark. Without it the card reads the
   *  set from the mark it makes under `markRead`, and a card that does neither folds by count
   *  alone. An unread reply is never folded away (`threads/thread-layout.ts`). */
  unread?: ReadonlySet<string>;
  /** The reply a deep link named: the Conversation marks that reply and opens the card's
   *  collapsed thread over it. No other surface lands on a reply. */
  currentReplyId?: string;
}

/** The urgency notch's text colour, matching the urgency badge of the same level. */
const URGENCY_NOTCH_TEXT: Record<AskUrgency, string> = {
  blocking: badgeBlocking.text,
  high: badgeHigh.text,
  low: badgeLow.text,
  med: badgeMed.text,
};

/**
 * Each colour the card's own elements draw inside `NoTooltips`, the new issue page's, by the
 * role `main`'s colour has: the card is the page's paper inside its hairline rule, the line of
 * its copy handles is the quiet grey, and the asker's control, which opens the copy handles, is
 * the quiet grey a reader acts on. What the card's children draw - the ask body, the recipients,
 * the thread - is theirs.
 *
 * They stand in this table, not beside `main`'s classes, because `theme/text-on-background.test.ts`
 * pairs each text class with the nearest enclosing ground of the same file across both branches
 * of a ternary: a page colour written beside `main`'s would pair `main`'s words with the page's
 * paper and the page's words with `main`'s card. Each pair here is one `theme/classes.ts`
 * registers for the page.
 */
const ON_PAGE = {
  frame: `${pageHairBorder} ${pagePaperBg}`,
  line: pageQuietText,
  notch: pagePaperBg,
  quietAct: `${pageQuietText} hover:text-page-muted`,
} as const;

/** The card's urgency accent inside `NoTooltips`, in the new issue page's colours: blocking
 *  the failed red, high the waiting orange, med the questions' blue, low the hairline grey. */
const PAGE_URGENCY_ACCENT: Record<AskUrgency, string> = {
  blocking: "border-l-page-failed",
  high: "border-l-page-waiting",
  low: "border-l-page-hair",
  med: "border-l-page-question",
};

/** The urgency notch's words inside `NoTooltips`, matching `PAGE_URGENCY_ACCENT` on the page's
 *  paper. */
const PAGE_URGENCY_NOTCH_TEXT: Record<AskUrgency, string> = {
  blocking: pageFailedText,
  high: pageStateText.waiting,
  low: pageQuietText,
  med: "text-page-question",
};

/**
 * One ask on every surface: the ask body (`ask-body/AskBody.tsx`), which draws the ask's
 * question, options and answer parts and its settled record, inside the card's own frame. The
 * card holds what its surfaces alone draw - the urgency notch, the recipients, the copy handles,
 * the edit history, the references and the ask's thread - and draws each above the body or
 * below it, never between its parts: the thread above it where the reader's thread order puts
 * the answer box at the foot (Oldest first), else below it. It also holds the form that holds
 * the ask's state (`useAskAnswerForm`), which reads the ask's thread.
 */
export function AskCard({
  artifactSlug,
  ask,
  owner,
  thread = "inline",
  variant = "full",
  frame = "card",
  answerAsk: answer = answerAsk,
  createReply: reply = createReply,
  declineAsk: decline = declineAsk,
  getAskThread: getThread = getAskThread,
  initialThread,
  initialThreadUpdatedAt,
  documentLink = true,
  onAnswered,
  mode = "answer",
  markRead = false,
  unread,
  currentReplyId,
}: AskCardProps): ReactNode {
  const form = useAskAnswerForm({
    answer,
    ask,
    createReply: reply,
    getAskThread: getThread,
    initialThread,
    initialThreadUpdatedAt,
    initiallyChanging: mode === "change",
    onAnswered,
  });
  const { completed, displayedAsk, edits, questionReads, threadQuery } = form;
  const [handlesOpen, setHandlesOpen] = useState(false);
  const [referencesOpen, setReferencesOpen] = useState(false);
  // One page can host the same ask twice (a decision block and the margin sheet), so the
  // panel each control names is this instance's.
  const referencesPanelId = useId();
  const inBlock = frame === "block";
  // `document` is the Inbox row's: the ask read carries no owner, so the fresher `displayedAsk`
  // cannot name it and this card's own row is where it is read.
  const reference = itemRoute("ask", displayedAsk, ask.document ?? owner);
  const uploadOwner = askUploadOwner(ask, displayedAsk, owner);
  const tooltips = useTooltips();
  // The copy handles' values: the title the session stamped, as the clipboard has always had it.
  const sessionAuthor = displayedAsk.author.kind === "session" ? displayedAsk.author : undefined;
  const sessionTitle = sessionAuthor?.origin?.session_title?.trim();
  const tmuxTarget = sessionAuthor?.origin?.tmux;
  const urgency = askUrgency(displayedAsk.urgency);
  const hasUrgencyNotch = !inBlock && (urgency === "blocking" || urgency === "high");
  // The thread's own "still open?" wording must track the post-answer ask, not the possibly
  // stale prop passed to this instance: `completed` renders before an invalidated `ask` prop
  // round-trips down from the parent.
  const currentAsk = completed ?? displayedAsk;
  // The count comes from `displayedAsk`, never from `completed`: answering an ask does not move
  // a backlink count, and the answer response is the one ask shape that carries no count.
  const referencedByCount = displayedAsk.referenced_by_count ?? 0;
  // The thread's newest write the card shows, which the shared thread read mark watches and
  // reads through: the newest reply, unless the record's answer came after it (an answer is a
  // write like a reply).
  const newestReply = (threadQuery.data?.replies ?? []).at(-1);
  const recordedAnswer = completed?.state === "answered" ? completed.answer : null;
  const answerIsNewest =
    recordedAnswer !== null &&
    (newestReply === undefined || compareTimestamps(recordedAnswer.at, newestReply.created_at) > 0);
  // The mark the card makes, and with it which of the replies on screen are still unread: an
  // unread reply is never folded away. A card that marks nothing read reads nothing, so it takes
  // what its host passes, and folds by count alone where the host passes none.
  const threadRead = useMarkThreadRead({
    enabled: markRead,
    newestShownAt: answerIsNewest ? recordedAnswer?.at : newestReply?.created_at,
    owner: askRepliesOwner(displayedAsk),
    threadId: displayedAsk.id,
    writes: threadQuery.data?.replies ?? undefined,
  });
  const markNewest = threadRead.ref;
  const unreadReplies = unread ?? threadRead.unread;
  // The one order every thread reads in, the reader's own (`threads/thread-order.ts`): it places
  // the replies, this card's options and answer box, and the thread's reply composer. Where the
  // box goes is the layout's own rule, which this card reads rather than works out again.
  const [threadOrder] = useThreadOrder();
  const answerFirst = composerFirst(threadOrder);
  const referencedByNode =
    reference === undefined || referencedByCount === 0 ? null : (
      <div className="mt-3">
        <ReferencedByToggle
          controls={referencesPanelId}
          count={referencedByCount}
          expanded={referencesOpen}
          onToggle={() => setReferencesOpen((open) => !open)}
        />
        {referencesOpen ? (
          <ReferencedBy
            className="mt-2"
            reference={buildDispatchReference(reference)}
            toggle={{ id: referencesPanelId }}
          />
        ) : null}
      </div>
    );
  const threadNode =
    thread === "collapsed" ? (
      <AskThreadDisclosure
        ask={currentAsk}
        createReply={reply}
        currentReplyId={currentReplyId}
        embedded={completed === null}
        markNewest={answerIsNewest ? undefined : markNewest}
        order={threadOrder}
        thread={threadQuery}
        unread={unreadReplies}
        uploadOwner={uploadOwner}
      />
    ) : (
      <AskThread
        ask={currentAsk}
        createReply={reply}
        currentReplyId={currentReplyId}
        embedded={completed === null}
        markNewest={answerIsNewest ? undefined : markNewest}
        order={threadOrder}
        thread={threadQuery}
        unread={unreadReplies}
        uploadOwner={uploadOwner}
      />
    );
  // The ask's own reference, and the session's handles - its ID, its title, its tmux target -
  // folded behind the asker's control, which opens them onto their own row: one line under the
  // body.
  const handlesNode =
    reference === undefined && !handlesOpen ? null : (
      <div
        className={`mt-1.5 flex flex-wrap items-center gap-x-1.5 text-xs ${tooltips ? textMutedOnSurface : ON_PAGE.line}`}
      >
        {reference === undefined ? null : <CopyRefButton route={reference} />}
        {!handlesOpen ? null : (
          <span className="flex basis-full flex-wrap items-center gap-x-1.5">
            {sessionAuthor === undefined ? null : (
              <CopyButton value={sessionAuthor.id} what={ASK_CARD_WORDS.copy_id_what}>
                {ASK_CARD_WORDS.copy_id}
              </CopyButton>
            )}
            {sessionTitle === undefined || sessionTitle === "" ? null : (
              <CopyButton value={sessionTitle} what={ASK_CARD_WORDS.copy_title_what}>
                {ASK_CARD_WORDS.copy_title}
              </CopyButton>
            )}
            {tmuxTarget === undefined ? null : (
              <CopyButton value={tmuxTarget} what={ASK_CARD_WORDS.copy_tmux_what}>
                {tmuxTarget}
              </CopyButton>
            )}
          </span>
        )}
      </div>
    );
  const body = (
    <AskBody
      agentNames="live"
      artifactSlug={artifactSlug}
      ask={ask}
      askerControl={(name) => (
        <button
          aria-expanded={handlesOpen}
          className={`flex min-w-0 items-center gap-1 ${tooltips ? textMutedHoverToSecondary : ON_PAGE.quietAct}`}
          onClick={() => setHandlesOpen((open) => !open)}
          type="button"
        >
          {name}
          <ChevronIcon expanded={handlesOpen} />
        </button>
      )}
      decline={decline}
      documentLink={documentLink}
      form={form}
      frame={frame}
      markRecord={answerIsNewest ? markNewest : undefined}
      onAnswered={onAnswered}
      owner={owner}
      renderText={markdownText}
      urgencyWord={false}
    />
  );

  // A settled ask's record, then its references and its thread under it.
  if (completed !== null) {
    return (
      <>
        {questionReads}
        {body}
        {referencedByNode}
        {threadNode}
      </>
    );
  }

  return (
    <article
      aria-label={`${ASK_CARD_WORDS.urgency} ${ASK_SHARED_WORDS.urgency[urgency]}`}
      className={
        inBlock
          ? "relative mt-3"
          : `relative rounded-xl border-l-4 shadow-sm ${variant === "compact" ? "px-3 pt-4 pb-3" : "px-4 pt-5 pb-4"} ${hasUrgencyNotch ? "mt-3" : ""} ${tooltips ? `${card} ${askUrgencyAccent[urgency]}` : `${ON_PAGE.frame} ${PAGE_URGENCY_ACCENT[urgency]}`}`
      }
      // The one handle on a card as a whole, carrying the question the card draws: a page-level
      // key that must not fire while the reader is answering an ask (the issue page's priority
      // digits, beside this card's own options) matches the bare attribute, and the Inbox's
      // answer keys find the selected question's own card by its value (`answerHand` in
      // `Inbox.tsx`). Neither asks for a test id.
      data-ask-card={displayedAsk.id}
      data-testid={`ask-${displayedAsk.id}`}
    >
      {questionReads}
      {hasUrgencyNotch ? (
        <span
          aria-hidden="true"
          className={`absolute -top-2 left-3 rounded px-1.5 text-[10px] leading-4 font-semibold tracking-wide uppercase ${tooltips ? `${surfaceBg} ${URGENCY_NOTCH_TEXT[urgency]}` : `${ON_PAGE.notch} ${PAGE_URGENCY_NOTCH_TEXT[urgency]}`}`}
        >
          {ASK_SHARED_WORDS.urgency[urgency].toUpperCase()}
        </span>
      ) : null}
      {answerFirst ? null : threadNode}
      {body}
      {handlesNode}
      <AskEditHistory ask={displayedAsk} edits={edits} renderText={markdownText} />
      {referencedByNode}
      {answerFirst ? threadNode : null}
      <AskRecipients
        askId={displayedAsk.id}
        claimant={threadQuery.data?.claimant ?? null}
        followers={threadQuery.data?.followers ?? []}
        owner={displayedAsk}
        route={threadQuery.data?.route ?? null}
        routeHolder={threadQuery.data?.route_holder ?? null}
        routeStatus={threadQuery.data?.route_status ?? null}
      />
    </article>
  );
}
