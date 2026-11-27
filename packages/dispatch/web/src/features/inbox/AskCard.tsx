import { dispatchWritingBox } from "@legion/contracts";
import {
  type ReactNode,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { type ArtifactOwner, api, type DeclineAskInput, type WriteBounds } from "../../api/client";
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
import { QueryError } from "../../components/QueryError";
import { TruncatedText } from "../../components/TruncatedText";
import { useTooltips } from "../../components/tooltips";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { compareTimestamps } from "../../lib/timestamps";
import {
  askUrgencyAccent,
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  borderDefault,
  card,
  cardHoverBorder,
  focusBorder,
  growingField,
  inlineWarningText,
  linkHoverText,
  linkText,
  pageFailedText,
  pageHairBorder,
  pageInkFill,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  pageStateText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  surfaceBg,
  textMutedHoverToSecondary,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { appendToDraft, DraftUploadStatus, useDraftUpload } from "../artifacts/ArtifactUpload";
import { MentionField, type MentionFieldHandle } from "../conversation/MentionField";
import { AgentName, useAgentText } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
import { MarkdownBody } from "../refs/MarkdownBody";
import { ReferencedBy, ReferencedByToggle } from "../refs/ReferencedBy";
import { buildDispatchReference, itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { composerFirst } from "../threads/thread-layout";
import { useThreadOrder } from "../threads/thread-order";
import { askRepliesOwner, useMarkThreadRead } from "../threads/useMarkThreadRead";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { stoppedNote } from "../writes/useKeyedWrite";
import { AskAnchorHeader } from "./AskAnchorLink";
import { AskApprovalLink } from "./AskApprovalLink";
import { AskBlockLink } from "./AskBlockLink";
import {
  AskCompletionCard,
  AskEditHistory,
  type AskFrame,
  OrphanedAnchorNotice,
} from "./AskCompletionCard";
import { AskHandOn } from "./AskHandOn";
import { AskChoiceRow } from "./AskOptionRow";
import { AskRecipients } from "./AskRecipients";
import { AskThread, UnsentBox } from "./AskThread";
import { AskThreadDisclosure } from "./AskThreadDisclosure";
import type { OwnedAsk } from "./ask-name";
import { askTurnHolder } from "./ask-turn";
import { askUrgency, URGENCY_LABELS } from "./ask-urgency";
import { DeclineControl } from "./DeclineControl";
import { EarlierApproval } from "./EarlierApproval";
import { handOffFocus, useAnswerFormFocus } from "./useAnswerFormFocus";
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
  /** `compact` (the margin and the review sheet) renders the same option rows as `full`, but
   *  folds the free-text field and its Answer / Ask back actions behind an **Add a note or answer
   *  in your own words** disclosure, showing a lone Answer button once an option is picked; the
   *  disclosure opens itself when the pick needs words (Other, or an option requiring a reason).
   *  The article also takes tighter padding (`px-3 pt-4 pb-3` against `px-4 pt-5 pb-4`). Nothing
   *  else depends on the variant. */
  variant?: "compact" | "full";
  /** `card` (the default) draws the card: frame, urgency accent and notch, the question, and the
   *  link to a block ask's document. `block` is the card hosted inside its own decision block
   *  (`features/doc/AskBlockCard.tsx`): the block shell already draws the frame and accent, the
   *  editor shows the question, and the block is the link's target, so the card renders only
   *  what the block lacks — who asked and when, the exchange, and the composer or the record. */
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

/** What the unsent answer's box says, on today's pages and inside `NoTooltips` alike. */
const UNSENT_ANSWER_WHY =
  "This question closed before you sent your answer, so it was not sent. Your text is here to copy.";

/**
 * Each colour the card's own elements draw inside `NoTooltips`, the new issue page's, by the
 * role `main`'s colour has: the card is the page's paper inside its hairline rule; the question
 * is the ink, what a reader acts on the muted grey, what qualifies it the quiet grey, a link
 * the ink, and a question that changed under the reader the page's warning, its waiting
 * orange; the answer box is the page's field, which keeps `main`'s focus rule, the application's
 * one focus indicator; Answer and Ask back instead are filled with the ink, as the page's one
 * filled control is, and the other actions are its header control's rule and muted words. What
 * the card's children draw - the recipients, the thread, the options, the earlier approval, the
 * unsent answer's box (`UnsentBox`) - is theirs.
 *
 * They stand in this table, not beside `main`'s classes, because `theme/text-on-background.test.ts`
 * pairs each text class with the nearest enclosing ground of the same file across both branches
 * of a ternary: a page colour written beside `main`'s would pair `main`'s words with the page's
 * paper and the page's words with `main`'s card. Each pair here is one `theme/classes.ts`
 * registers for the page.
 */
const ON_PAGE = {
  action: `${pageLineBorder} ${pageMutedText}`,
  field: `${pageHairBorder} ${pagePaperBg} ${pageInkText} placeholder:text-page-quiet ${focusBorder}`,
  filled: `${pageInkFill} refused:cursor-not-allowed refused:bg-page-hair refused:text-page-muted`,
  frame: `${pageHairBorder} ${pagePaperBg}`,
  ink: pageInkText,
  link: pageInkText,
  muted: pageMutedText,
  notch: pagePaperBg,
  quiet: pageQuietText,
  quietAct: `${pageQuietText} hover:text-page-muted`,
  warning: pageStateText.waiting,
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
  const {
    answerFailure,
    answerFieldId,
    answerPlaceholder,
    answers,
    answerText,
    askChanged,
    canAnswer,
    canChangeAnswer,
    cancelChanging,
    changing,
    clarification,
    completed,
    displayedAsk,
    edits,
    formRefusal,
    isApproval,
    isSubmitting,
    mutation,
    otherSelected,
    questionChoice,
    questionReads,
    reasonRequired,
    requiresReason,
    selectRealOption,
    selected,
    sendAnswer,
    sendClarification,
    setAnswerText,
    setAnswerTextOf,
    setQuestionChoice,
    startChanging,
    submit,
    submitHint,
    toggleOther,
    submitGuard,
    takeFormRefusalFocus,
    threadQuery,
    trimmedAnswer,
    unsentAnswer,
  } = useAskAnswerForm({
    answer,
    ask,
    createReply: reply,
    getAskThread: getThread,
    initialThread,
    initialThreadUpdatedAt,
    initiallyChanging: mode === "change",
    onAnswered,
  });
  const [ownWordsOpen, setOwnWordsOpen] = useState(false);
  const [handlesOpen, setHandlesOpen] = useState(false);
  const [referencesOpen, setReferencesOpen] = useState(false);
  // A change seeds the current answer's note; a compact card shows it rather than folding text
  // the save would send behind its disclosure. Set while rendering the first changing render,
  // React's pattern for state derived from a prop change, so the field is never hidden a frame.
  const [seededFor, setSeededFor] = useState(false);
  if (changing !== seededFor) {
    setSeededFor(changing);
    if (changing && variant === "compact" && answerText.trim() !== "") setOwnWordsOpen(true);
  }
  // A refusal of the form fills the reason of Request changes with what Dispatch wrote for the
  // author (`useAskAnswerForm`); a compact card shows that reason too rather than folding the
  // text the next press sends behind its disclosure. The same pattern as the change above, from
  // no refusal, so a card drawn again with a kept refusal opens it too.
  const [shownFormRefusal, setShownFormRefusal] = useState<typeof formRefusal>(null);
  if (formRefusal !== shownFormRefusal) {
    setShownFormRefusal(formRefusal);
    if (formRefusal !== null && variant === "compact") setOwnWordsOpen(true);
  }
  // One page can host the same ask twice (a decision block and the margin sheet), so the
  // panel each control names is this instance's.
  const referencesPanelId = useId();
  const isCompact = variant === "compact";
  const inBlock = frame === "block";
  // `document` is the Inbox row's: the ask read carries no owner, so the fresher `displayedAsk`
  // cannot name it and this card's own row is where it is read.
  const reference = itemRoute("ask", displayedAsk, ask.document ?? owner);
  // Where a `#id` in the ask's own text resolves: the document the server scopes this ask to -
  // the one its block is on, the one it is anchored to, else its issue's spec (`refs.ScopeOf`).
  const source = textSourceOf("ask", displayedAsk.id, displayedAsk.issue_key);
  // Where a file pasted or dropped into the answer, or into a reply under the ask, goes: the
  // ask's issue, or the project of the document it was asked on.
  const documentProject = (ask.document ?? owner ?? displayedAsk.anchor_artifact)?.project;
  const uploadOwner: ArtifactOwner | undefined =
    displayedAsk.issue_key !== null
      ? { issue: displayedAsk.issue_key }
      : documentProject === undefined
        ? undefined
        : { project: documentProject };
  // A file's text joins the answer of the ask it was pasted or dropped under, which its press
  // carries (`useDraftUpload`, `setAnswerTextOf`); only text added to the field on screen puts its
  // question choice away.
  const upload = useDraftUpload((text: string, askId: string) => {
    setAnswerTextOf(askId, (current) => appendToDraft(current, text));
    if (askId === ask.id) setQuestionChoice(false);
  });
  const uploading = upload.pending > 0;
  const answerFieldRef = useRef<MentionFieldHandle | null>(null);
  // Picking Request changes moves the person straight to the field the server insists on. Keyed
  // on the field actually being mounted, not just on the option: the compact variant mounts the
  // field when its disclosure opens (reopening it is the disclosure's own move, below). Only the
  // person's own pick moves the focus, so the intent is recorded as the option is picked: a
  // refusal of the form also picks Request changes, in each card of the ask, and leaves the focus
  // where the person pressed (`useAnswerFormFocus`).
  const reasonFieldShown = reasonRequired && (!isCompact || ownWordsOpen);
  const focusReasonOnShow = useRef(false);
  useEffect(() => {
    if (!reasonFieldShown) return;
    if (focusReasonOnShow.current) answerFieldRef.current?.textarea()?.focus();
    focusReasonOnShow.current = false;
  }, [reasonFieldShown]);
  // Opening the disclosure is a move into the field it reveals, whoever opened it: the reader's
  // own click, and the Inbox's Enter, which presses this same control (`Inbox.tsx`). The field
  // is drawn by the render the press schedules, so the focus can only be this effect's - the
  // opener runs before React has committed the box and would focus nothing. Only an open by the
  // disclosure itself focuses, which is why the intent is recorded as the control is pressed: an
  // option that opens the disclosure for its own words, and a change that seeds the note, leave
  // the reader where they are.
  const focusNoteOnOpen = useRef(false);
  useEffect(() => {
    if (!focusNoteOnOpen.current) return;
    focusNoteOnOpen.current = false;
    if (ownWordsOpen) {
      answerFieldRef.current?.textarea()?.focus();
    }
  }, [ownWordsOpen]);
  const [formRef, trackForm] = useAnswerFormFocus({
    formRefusal,
    pending: mutation.isPending,
    takeFormRefusalFocus,
  });
  const tooltips = useTooltips();
  // Inside `NoTooltips` the card refuses its form from the press of an answer or a clarification
  // until that write is done with: while it is out, and, once it lands, until a read of the ask's
  // thread newer than the landing is drawn (`awaitingRead`). Until then the form on the page is
  // stale: after an answer that landed with no body to show, its options are still picked, and
  // Answer would send a second answer. A landing that starts no read of the thread ends the
  // refusal as it lands (a first answer that comes back with its body, whose record replaces the
  // form at once), and so does a read that fails, so the card never stays refused. Each control it
  // refuses - the answer box, Answer (Save answer), Ask back, Ask back instead, Answer with it
  // anyway, the Cancel of a change, and the options with Other - is `aria-disabled` in place of
  // `disabled`: the box is `readOnly`, and the options are refused by their group. `disabled`
  // drops the focus to the body in a browser; this way the control the reader pressed keeps it.
  // A press of any of them does nothing then, and neither does Ctrl+Enter, a paste or a drop in
  // the box, or the form's submit. Each is drawn from the form as it stood when the refusal began
  // (`held`): the landing empties the box and closes the choice a question-shaped answer offers,
  // and the control that would turn `disabled` or leave the page with that - Ask back, its text
  // gone, or Ask back instead - stays on the page, refused. So the hand-off above moves no focus
  // while the card refuses, and nothing holding the focus leaves. The one move of the focus comes
  // as the refusal ends: the form's hand-off, or Ask back instead's, as it leaves the page, or the
  // one above as Ask back turns `disabled`; a focus the reader moved out of the form meanwhile
  // stays where they put it. Any other reason a control cannot be pressed - nothing typed before
  // the press, an upload still out - still disables it, on both sides. Outside `NoTooltips` each
  // is `disabled` while the write is out and no longer, as on `main`.
  const holdsFocus = !tooltips;
  // The newest write of the form that has landed, by when it was sent: 0 while none has.
  const landedWrite = Math.max(
    mutation.isSuccess ? mutation.submittedAt : 0,
    clarification.isSuccess ? clarification.submittedAt : 0
  );
  // The landing is recorded while rendering, React's pattern for state derived from a change (as
  // `seededFor` is): an effect would let one render between the landing and the record reach the
  // screen, and a press then would be taken. `readAfter` is when the thread read on the page at
  // the landing was made; the card waits for a newer one while the thread is being read. Outside
  // `NoTooltips` nothing waits, so nothing is recorded and the card renders as on `main`.
  const [landing, setLanding] = useState<{ readAfter: number | null; write: number }>({
    readAfter: null,
    write: landedWrite,
  });
  const landedNow = landedWrite > landing.write;
  let readAfter = landedNow ? threadQuery.dataUpdatedAt : landing.readAfter;
  const awaitingRead =
    holdsFocus &&
    readAfter !== null &&
    threadQuery.isFetching &&
    threadQuery.dataUpdatedAt <= readAfter;
  if (!awaitingRead) readAfter = null;
  if (holdsFocus && (landedNow || readAfter !== landing.readAfter)) {
    setLanding({ readAfter, write: Math.max(landedWrite, landing.write) });
  }
  // The one flag every refusal of the form reads: outside `NoTooltips` it is `isSubmitting`.
  const refusing = isSubmitting || awaitingRead;
  const refusedWhileOut = (holdsFocus && refusing) || undefined;
  const disabledWhileOut = !holdsFocus && isSubmitting;
  // What a refused control is drawn from, recorded while rendering as the landing is, in the
  // first render that refuses: the press, before the landing empties the box.
  const formNow = {
    cannotAnswer: !canAnswer,
    choice: questionChoice,
    nothingToAskBack: trimmedAnswer === "",
  };
  const [heldForm, setHeldForm] = useState<typeof formNow | null>(null);
  const held = refusedWhileOut ? (heldForm ?? formNow) : null;
  if (held !== heldForm) setHeldForm(held);
  const cannotAnswer = held?.cannotAnswer ?? !canAnswer;
  const nothingToAskBack = held?.nothingToAskBack ?? trimmedAnswer === "";
  const choiceShown = held?.choice ?? questionChoice;
  // Ask back instead takes the focus as it is drawn. Outside `NoTooltips` its callback is a new
  // one at each render, which focuses it again at each render, as on `main`. Inside it is this
  // one callback, stable across renders as `useFormRef`'s is, which focuses it once: a focus
  // taken at each render would move the reader's focus while a write is out - from Answer with
  // it anyway, say, whose answer is out. The choice leaves the page, and the button with it, as
  // the clarification lands outside `NoTooltips`, and inside once the ask is read again after it;
  // a focus the button still holds then goes where the form's own hand-off puts it, not to the
  // body.
  const focusAskBackInstead = useCallback(
    (button: HTMLButtonElement | null) => {
      if (button === null) return undefined;
      button.focus();
      return () => {
        const form = formRef.current;
        if (form !== null && document.activeElement === button) handOffFocus(form);
      };
    },
    [formRef]
  );
  const submitHintId = `${answerFieldId}-hint`;
  const agentText = useAgentText();
  // The asker as its button's tooltip: a block ask the server indexed with no pending author
  // has an empty label and reads as "asked". A session's tooltip also carries its full id, since
  // the innermost title is the one a browser shows, and an untitled session's name is the short
  // form `session:<8>…` that sessions started in the same minute share.
  const sessionAuthor = displayedAsk.author.kind === "session" ? displayedAsk.author : undefined;
  const askerTitle =
    displayedAsk.author.id === ""
      ? "asked"
      : sessionAuthor === undefined
        ? agentText.label(displayedAsk.author)
        : `${agentText.label(sessionAuthor)} · ${sessionAuthor.id}`;
  // The asker's name, cut with its tooltip where one is drawn; inside `NoTooltips`, where no
  // tooltip gives back what an ellipsis cut, it is a value that breaks anywhere instead.
  const askerName = (name: ReactNode) =>
    tooltips ? (
      <TruncatedText className="min-w-0" title={askerTitle}>
        {name}
      </TruncatedText>
    ) : (
      <span className="min-w-0 [overflow-wrap:anywhere]">{name}</span>
    );
  // The copy handles' values: the title the session stamped, as the clipboard has always had it.
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
  const turnHolder = askTurnHolder(currentAsk, (threadQuery.data?.replies ?? []).at(-1));
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

  // Inside `NoTooltips` the actions are the page's (`ON_PAGE`): filled with the ink, or its
  // header control's rule and muted words.
  const filledLook = tooltips
    ? `${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`
    : ON_PAGE.filled;
  const secondaryLook = tooltips
    ? `${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`
    : ON_PAGE.action;
  const answerField = (
    <>
      {/* Inside `NoTooltips` the box is the page's (`ON_PAGE.field`), with its label and the line
          under it in the page's words, and while the card refuses it is `readOnly` rather than
          `disabled`, and takes no Ctrl+Enter, paste or drop (`refusing`). Outside it is
          `disabled` while the write is out, as on `main`. It grows with its text up to half the
          window (`growingField`), so a reason that Dispatch wrote after a form refusal shows
          whole and Answer stays on the screen. */}
      <MentionField
        askAnswer={true}
        body={answerText}
        className={`font-normal ${growingField}`}
        disabled={disabledWhileOut}
        fieldClassName={tooltips ? undefined : ON_PAGE.field}
        fieldRef={answerFieldRef}
        hintClassName={tooltips ? undefined : `text-xs ${ON_PAGE.quiet}`}
        issueKey={displayedAsk.issue_key ?? undefined}
        label={reasonRequired ? "Reason (required)" : isApproval ? "Reason" : "Your answer"}
        labelClassName={tooltips ? undefined : `block text-sm font-medium ${ON_PAGE.muted}`}
        labelHidden={!reasonRequired}
        onChange={(change) => {
          setAnswerText(change.body);
          setQuestionChoice(false);
        }}
        onDrop={
          uploadOwner === undefined
            ? undefined
            : (event) => {
                if (!refusing) upload.drop(event, uploadOwner, ask.id);
              }
        }
        onKeyDown={(event) => submitOnModifiedEnter(event, { disabled: uploading || refusing })}
        onPaste={
          uploadOwner === undefined
            ? undefined
            : (event) => {
                if (!refusing) upload.paste(event, uploadOwner, ask.id);
              }
        }
        placeholder={answerPlaceholder}
        project={documentProject}
        readOnly={holdsFocus && refusing}
        rows={2}
        writing={dispatchWritingBox("ask-answer", "ask-answer")}
      />
      <DraftUploadStatus upload={upload} />
    </>
  );
  const submitButton = (
    <button
      // While nothing can be sent it is `disabled` alone, on both sides of `NoTooltips`; while the
      // card refuses, that is read from the form as the refusal found it (`held`).
      aria-disabled={cannotAnswer || uploading ? undefined : refusedWhileOut}
      aria-describedby={submitHint === undefined ? undefined : submitHintId}
      className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${filledLook}`}
      disabled={cannotAnswer || disabledWhileOut || uploading}
      title={tooltips ? submitHint : undefined}
      type="submit"
    >
      {mutation.isPending
        ? changing
          ? "Saving…"
          : "Answering…"
        : changing
          ? "Save answer"
          : "Answer"}
    </button>
  );
  const submitHintNode =
    submitHint === undefined ? null : (
      <p className={`text-xs ${tooltips ? textMutedOnSurface : ON_PAGE.quiet}`} id={submitHintId}>
        {submitHint}
      </p>
    );
  const answerActions = choiceShown ? (
    <fieldset aria-label="Question-shaped answer" className="flex gap-2">
      <button
        aria-disabled={refusedWhileOut}
        className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${filledLook}`}
        disabled={disabledWhileOut}
        onClick={() => {
          if (!refusing) sendClarification();
        }}
        ref={tooltips ? (node) => node?.focus() : focusAskBackInstead}
        type="button"
      >
        {clarification.isPending ? "Sending…" : "Ask back instead"}
      </button>
      <button
        aria-disabled={refusedWhileOut}
        className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryLook}`}
        disabled={disabledWhileOut}
        onClick={() => {
          if (!refusing) sendAnswer(answerText);
        }}
        type="button"
      >
        Answer with it anyway
      </button>
    </fieldset>
  ) : (
    <div className="space-y-2">
      <div className="flex gap-2">
        {submitButton}
        {changing ? (
          <button
            aria-disabled={refusedWhileOut}
            className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryLook}`}
            disabled={disabledWhileOut}
            onClick={() => {
              if (!refusing) cancelChanging();
            }}
            type="button"
          >
            Cancel
          </button>
        ) : (
          <button
            // While it has no text to send it is `disabled` alone, on both sides of `NoTooltips`;
            // while the card refuses, that is read from the form as the refusal found it (`held`),
            // so the text the landing clears leaves it refused and not `disabled`.
            aria-disabled={nothingToAskBack || uploading ? undefined : refusedWhileOut}
            className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryLook}`}
            disabled={nothingToAskBack || disabledWhileOut || uploading}
            onClick={() => {
              if (!refusing) sendClarification();
            }}
            type="button"
          >
            {clarification.isPending ? "Sending…" : "Ask back"}
          </button>
        )}
      </div>
      {submitHintNode}
    </div>
  );

  if (completed !== null) {
    return (
      <>
        {questionReads}
        <AskCompletionCard
          answers={answers}
          artifactSlug={artifactSlug}
          ask={completed}
          documentLink={documentLink}
          edits={edits}
          frame={frame}
          markRef={answerIsNewest ? markNewest : undefined}
          onChangeAnswer={canChangeAnswer ? startChanging : undefined}
        />
        {unsentAnswer === "" ? null : (
          <UnsentBox
            className="mt-3"
            testId="ask-unsent-answer"
            text={unsentAnswer}
            why={UNSENT_ANSWER_WHY}
          />
        )}
        {answerFailure === null ? null : (
          <QueryError
            message={
              // This branch offers no Retry: the question has closed, so an answer the reader
              // stopped is said with what the card shows in place of its form.
              mutation.error instanceof SendStoppedError
                ? stoppedWriteNote("your answer", "saved", {
                    anew: "The question has closed, and the record above shows how.",
                  })
                : answerFailure.message
            }
          />
        )}
        {referencedByNode}
        {threadNode}
      </>
    );
  }

  return (
    <article
      aria-label={`Urgency: ${URGENCY_LABELS[urgency]}`}
      className={
        inBlock
          ? "relative mt-3"
          : `relative rounded-xl border-l-4 shadow-sm ${isCompact ? "px-3 pt-4 pb-3" : "px-4 pt-5 pb-4"} ${hasUrgencyNotch ? "mt-3" : ""} ${tooltips ? `${card} ${askUrgencyAccent[urgency]}` : `${ON_PAGE.frame} ${PAGE_URGENCY_ACCENT[urgency]}`}`
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
          {URGENCY_LABELS[urgency].toUpperCase()}
        </span>
      ) : null}
      <AskAnchorHeader ask={displayedAsk} inBlock={inBlock} quoteSpacing="roomy" tone="surface" />
      <OrphanedAnchorNotice artifactSlug={artifactSlug} ask={displayedAsk} />
      {inBlock ||
      displayedAsk.block_id === undefined ||
      displayedAsk.block_id === null ||
      displayedAsk.block_artifact === undefined ? null : (
        <p className={`mt-2 text-sm ${tooltips ? `${linkText} ${linkHoverText}` : ON_PAGE.link}`}>
          <AskBlockLink ask={displayedAsk} />
        </p>
      )}
      <div>
        {isApproval ? (
          <p
            className={`text-[10px] font-semibold uppercase tracking-wide ${tooltips ? textMutedOnSurface : ON_PAGE.quiet}`}
          >
            Approval requested
          </p>
        ) : null}
        {isApproval && documentLink ? (
          <p className={`mt-1 text-sm ${tooltips ? `${linkText} ${linkHoverText}` : ON_PAGE.link}`}>
            <AskApprovalLink ask={displayedAsk} />
          </p>
        ) : null}
        {inBlock ? null : (
          <div
            className={`text-[15px] leading-relaxed font-medium ${tooltips ? textPrimaryOnSurface : ON_PAGE.ink}`}
          >
            <MarkdownBody
              markdown={displayedAsk.question}
              source={source}
              surface="ask-card-question"
              tree={displayedAsk.question_tree}
            />
          </div>
        )}
        {/* A request to approve again names the approval it follows, under the question (the
            spec's own placement), with the diff from that version behind Show diff; a first
            request carries no earlier approval, and EarlierApproval renders nothing for it. */}
        {displayedAsk.approval === undefined ? null : (
          <EarlierApproval approval={displayedAsk.approval} />
        )}
        {/* One provenance line under the question, in the spec's decided conversation grammar:
            whose turn it is (the only part a reader acts on), who asked, when, and the ask's
            own reference. The session's own handles - its ID, its title, its tmux target - are
            chrome a reader wants perhaps once a week, so they fold behind the author chip and
            open onto their own row. That keeps the line inside the card at 390 px and in a
            280 px margin, where a single row could not hold five copy controls without pushing
            the reference out of reach. */}
        <div
          className={`@container mt-1.5 flex flex-wrap items-center gap-x-1.5 text-xs ${tooltips ? textMutedOnSurface : ON_PAGE.quiet}`}
        >
          {/* Turn, author and time are one group whose names truncate: a separator that wrapped
              left a line opening - or ending - on a bare "·", and the agents are the only parts
              that can give up width ("Waiting on you" never does). Below 24rem of line (a phone,
              a margin) the turn takes a line of its own with no separator after it, and the
              asker and time share the next: beside the turn and the live link's 44 px touch box
              the asker's name kept only two letters. A block ask the server indexed with no
              pending author has an empty label and still says when it was asked. Inside
              `NoTooltips` no title gives back what an ellipsis cut, and a name and an instant
              are values, which wrap there: each name breaks anywhere inside its own part of the
              group, which still holds its one line, so no separator opens a line there either. */}
          <span
            className={`flex min-w-0 items-center gap-x-1.5 ${tooltips ? "whitespace-nowrap " : ""}@max-sm:flex-wrap`}
          >
            {turnHolder === null ? null : (
              <span
                className={`${turnHolder === "you" ? "shrink-0" : tooltips ? "min-w-0 truncate" : "min-w-0 [overflow-wrap:anywhere]"} font-semibold @max-sm:basis-full ${tooltips ? textSecondaryOnSurface : ON_PAGE.muted}`}
                data-testid={`turn-${displayedAsk.id}`}
              >
                Waiting on {turnHolder === "you" ? "you" : <AgentName actor={turnHolder} inline />}
              </span>
            )}
            {turnHolder === null ? null : (
              <span className="shrink-0 @max-sm:hidden" aria-hidden="true">
                ·
              </span>
            )}
            <span className="flex min-w-0 items-center gap-x-1.5">
              {sessionAuthor === undefined ? (
                askerName(
                  displayedAsk.author.id === "" ? (
                    "asked"
                  ) : (
                    <AgentName actor={displayedAsk.author} />
                  )
                )
              ) : (
                // The asker's button keeps its name and opens the session's copy handles; the
                // component's live link sits beside it, outside the button. `whitespace-pre`
                // keeps the space before an attribution suffix, which a flex item would drop;
                // inside `NoTooltips` `whitespace-pre-wrap` keeps it too, and lets the name wrap.
                <AgentName
                  actor={sessionAuthor}
                  className={`flex min-w-0 items-center ${tooltips ? "whitespace-pre" : "whitespace-pre-wrap"}`}
                  control={(name) => (
                    <button
                      aria-expanded={handlesOpen}
                      className={`flex min-w-0 items-center gap-1 ${tooltips ? textMutedHoverToSecondary : ON_PAGE.quietAct}`}
                      onClick={() => setHandlesOpen((open) => !open)}
                      type="button"
                    >
                      {askerName(name)}
                      <ChevronIcon expanded={handlesOpen} />
                    </button>
                  )}
                  kind="control"
                />
              )}
              <span className="shrink-0" aria-hidden="true">
                ·
              </span>
              <Timestamp at={displayedAsk.created_at} />
            </span>
          </span>
          {reference === undefined ? null : <CopyRefButton route={reference} />}
          {!handlesOpen ? null : (
            <span className="flex basis-full flex-wrap items-center gap-x-1.5">
              {sessionAuthor === undefined ? null : (
                <CopyButton value={sessionAuthor.id} what="session ID">
                  ID
                </CopyButton>
              )}
              {sessionTitle === undefined || sessionTitle === "" ? null : (
                <CopyButton value={sessionTitle} what="session title">
                  title
                </CopyButton>
              )}
              {tmuxTarget === undefined ? null : (
                <CopyButton value={tmuxTarget} what="tmux target">
                  {tmuxTarget}
                </CopyButton>
              )}
            </span>
          )}
        </div>
        {/* Whose question it is now, and the one control that hands it to somebody else. An
            ask that is not open waits on nobody, so it has none: a card whose ask is answered,
            resolved or withdrawn draws its record instead, and the one case that reaches here
            is a reader changing the answer of a closed ask. The ask is the fresher read and
            the issue is the card's own prop: only an Inbox row carries one, and the ask reads
            do not. */}
        {displayedAsk.state === "open" ? (
          <AskHandOn ask={displayedAsk} assignee={ask.issue?.assignee ?? null} />
        ) : null}
        <AskEditHistory ask={displayedAsk} edits={edits} />
        {referencedByNode}
      </div>
      {askChanged ? (
        <p className={`mt-3 text-sm font-medium ${tooltips ? inlineWarningText : ON_PAGE.warning}`}>
          This question changed while you were answering. Review the latest wording and confirm your
          answer again; your draft text is still here.
        </p>
      ) : null}
      {/* The options and the answer box sit where the order puts a thread's reply box: under
          the question in Newest first, at the foot of the card in Oldest first. */}
      {answerFirst ? null : threadNode}
      <form
        className="mt-3 space-y-3"
        // While the card refuses a submit sends nothing: inside `NoTooltips` a press of Answer,
        // or Enter in an option, still reaches it then (`refusing`).
        onSubmit={(event) => {
          if (refusing) event.preventDefault();
          else submit(event);
        }}
        ref={trackForm}
      >
        {(displayedAsk.options ?? []).length === 0 ? null : (
          // Inside `NoTooltips`, while the card refuses, the options are refused by their group's
          // `aria-disabled`, which ARIA passes to each option in it: an option row puts no
          // attribute of its own on its input (`AskChoiceRow`).
          <fieldset aria-disabled={refusedWhileOut} className="min-w-0 space-y-2">
            <legend className="sr-only">Answer options</legend>
            {(displayedAsk.options ?? []).map((option) => (
              <AskChoiceRow
                checked={selected.includes(option.label)}
                disabled={disabledWhileOut}
                hotkey
                key={option.label}
                multiple={displayedAsk.multiple}
                name={`ask-${displayedAsk.id}`}
                onChange={() => {
                  if (refusing) return;
                  focusReasonOnShow.current = requiresReason(option.label);
                  selectRealOption(option.label);
                  if (isCompact)
                    setOwnWordsOpen(requiresReason(option.label) || trimmedAnswer !== "");
                }}
                option={option}
                source={source}
              />
            ))}
            {isApproval ? null : (
              <AskChoiceRow
                checked={otherSelected}
                disabled={disabledWhileOut}
                multiple={displayedAsk.multiple}
                name={`ask-${displayedAsk.id}`}
                onChange={() => {
                  if (refusing) return;
                  toggleOther();
                  if (isCompact && !otherSelected) setOwnWordsOpen(true);
                }}
                option={{ label: "Other" }}
                source={source}
              />
            )}
          </fieldset>
        )}
        {isCompact ? (
          <>
            {selected.length === 0 || ownWordsOpen ? null : (
              <div className="space-y-2">
                {submitButton}
                {submitHintNode}
              </div>
            )}
            <div>
              <button
                aria-controls={`${answerFieldId}-disclosure`}
                aria-expanded={ownWordsOpen}
                className={`min-h-11 text-sm font-medium ${tooltips ? textSecondaryOnSurface : ON_PAGE.muted}`}
                // The Inbox's Enter opens this disclosure to reach the answer box behind it,
                // and finds the control by this handle, as it finds the box by
                // `data-ask-answer` and an option by `data-ask-option` (`Inbox.tsx`).
                data-ask-note-disclosure=""
                onClick={() => {
                  // Opening it is a move into the field behind it; closing it moves nowhere.
                  focusNoteOnOpen.current = !ownWordsOpen;
                  setOwnWordsOpen(!ownWordsOpen);
                }}
                type="button"
              >
                Add a note or answer in your own words
              </button>
              {ownWordsOpen ? (
                <div className="space-y-3 pt-3" id={`${answerFieldId}-disclosure`}>
                  {answerField}
                  {answerActions}
                </div>
              ) : null}
            </div>
          </>
        ) : (
          <>
            {answerField}
            {answerActions}
          </>
        )}
        {answerFailure === null ? null : (
          <QueryError
            message={answerFailure.message}
            onRetry={answerFailure.retryable ? () => submitGuard.retryLast(mutation) : undefined}
            retrying={mutation.isPending}
          />
        )}
        {clarification.isError ? (
          <QueryError
            message={
              // A question back the reader stopped may have been sent, and Retry makes that very
              // press again (`retryLast`).
              stoppedNote(clarification, "your clarification", "sent") ??
              "Could not send your clarification."
            }
            onRetry={() => submitGuard.retryLast(clarification)}
            retrying={clarification.isPending}
          />
        ) : null}
      </form>
      {/* The person's way out of a question they will not answer, under the form that answers
          it. An approval request draws none: it is answered with Approve or Request changes. */}
      {displayedAsk.state === "open" && displayedAsk.kind === "question" ? (
        <DeclineControl ask={displayedAsk} decline={decline} onDeclined={onAnswered} />
      ) : null}
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
