import { dispatchWritingBox } from "@legion/contracts";
import {
  type ReactNode,
  type SetStateAction,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

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
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { useTooltips } from "../../components/tooltips";
import { compareTimestamps } from "../../lib/timestamps";
import {
  askUrgencyAccent,
  card,
  inlineWarningText,
  linkHoverText,
  linkText,
  pageHairBorder,
  pageInkFill,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  pageStateText,
  primaryButton,
  secondaryButton,
  textMutedHoverToSecondary,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { appendToDraft, DraftUploadStatus, useDraftUpload } from "../artifacts/ArtifactUpload";
import { MentionField, type MentionFieldHandle } from "../conversation/MentionField";
import { MarkdownBody } from "../refs/MarkdownBody";
import { ReferencedBy, ReferencedByToggle } from "../refs/ReferencedBy";
import { buildDispatchReference, type DocumentOwner, itemRoute } from "../refs/routes";
import { textSourceOf } from "../refs/text-references";
import { composerFirst } from "../threads/thread-layout";
import { useThreadOrder } from "../threads/thread-order";
import { askRepliesOwner, useMarkThreadRead } from "../threads/useMarkThreadRead";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { stoppedNote } from "../writes/useKeyedWrite";
import { type AskAnswerActions, AskAnswerArea } from "./AskAnswerArea";
import { AskApprovalLink } from "./AskApprovalLink";
import { AskBlockLink } from "./AskBlockLink";
import {
  AskCompletionCard,
  AskEditHistory,
  type AskFrame,
  OrphanedAnchorNotice,
} from "./AskCompletionCard";
import { AskHandOn } from "./AskHandOn";
import { AskHeader, hasUrgencyNotch } from "./AskHeader";
import { AskChoiceRow } from "./AskOptionRow";
import { AskRecipients } from "./AskRecipients";
import { AskThread, askUploadOwner, UnsentBox } from "./AskThread";
import { AskThreadDisclosure } from "./AskThreadDisclosure";
import type { OwnedAsk } from "./ask-name";
import { askUrgency, URGENCY_LABELS } from "./ask-urgency";
import { DeclineControl } from "./DeclineControl";
import { EarlierApproval } from "./EarlierApproval";
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

/** The names this card's buttons know their refusals by, which the answer area is typed over:
 *  a misspelling is then a type error rather than a button whose on-screen reason goes missing
 *  (`AskAnswerArea.tsx`). */
type AnswerRefusalName = "answer" | "anyway" | "askBack";

export interface AskCardProps {
  artifactSlug?: string;
  ask: OwnedAsk;
  /** The project document that owns an ask whose read lacks `document` (only Inbox rows carry
   *  it), so the card can still name the ask's `dispatch://` reference. */
  owner?: DocumentOwner;
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

/** What the unsent answer's box says, on today's pages and inside `NoTooltips` alike. */
const UNSENT_ANSWER_WHY =
  "This question closed before you sent your answer, so it was not sent. Your text is here to copy.";

/**
 * Each colour the card's own elements draw inside `NoTooltips`, the new issue page's, by the
 * role `main`'s colour has: the card is the page's paper inside its hairline rule; the question
 * is the ink, what a reader acts on the muted grey, what qualifies it the quiet grey, a link
 * the ink, and a question that changed under the reader the page's warning, its waiting
 * orange. Ask back instead is filled with the ink. Cancel uses the header control's rule and
 * muted words. The answer field and the other shared components define their own colours.
 *
 * They stand in this table, not beside `main`'s classes, because `theme/text-on-background.test.ts`
 * pairs each text class with the nearest enclosing ground of the same file across both branches
 * of a ternary: a page colour written beside `main`'s would pair `main`'s words with the page's
 * paper and the page's words with `main`'s card. Each pair here is one `theme/classes.ts`
 * registers for the page.
 */
const ON_PAGE = {
  action: `${pageLineBorder} ${pageMutedText}`,
  filled: `${pageInkFill} refused:cursor-not-allowed refused:bg-page-hair refused:text-page-muted`,
  frame: `${pageHairBorder} ${pagePaperBg}`,
  ink: pageInkText,
  link: pageInkText,
  muted: pageMutedText,
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

/** Moves focus from inside the form to the nearest focusable ancestor (in the Inbox, the row, so
 *  j/k/Escape still start from the same place) instead of letting it fall to the document body. */
function handOffFocus(form: HTMLElement): void {
  if (!form.contains(document.activeElement)) return;
  form.parentElement?.closest<HTMLElement>("[tabindex]")?.focus({ preventScroll: true });
}

/** The form's callback ref. When the form leaves while a control inside it has focus - the ask
 *  was answered or resolved elsewhere and the card swaps to its record - focus is handed off
 *  without any event; React detaches a ref before removing its node, so the form is still in the
 *  document when this cleanup runs. The form is also kept in `formRef` for the disabled-control
 *  hand-off below: in the compact variant the answer field is unmounted while its disclosure is
 *  closed, so the field's `.form` is not a reliable way to reach it. The callback must be stable
 *  across renders: React re-runs a changed callback ref's cleanup on every commit, and that
 *  cleanup moves focus. */
function useFormRef(): [
  { current: HTMLFormElement | null },
  (form: HTMLFormElement | null) => (() => void) | undefined,
] {
  const formRef = useRef<HTMLFormElement | null>(null);
  const track = useCallback((form: HTMLFormElement | null) => {
    formRef.current = form;
    if (form === null) return undefined;
    return () => {
      formRef.current = null;
      handOffFocus(form);
    };
  }, []);
  return [formRef, track];
}

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
    answerRefusal,
    answers,
    answerText,
    askChanged,
    canChangeAnswer,
    cancelChanging,
    changing,
    clarification,
    clarificationRefusal,
    completed,
    displayedAsk,
    edits,
    isApproval,
    isSubmitting,
    mutation,
    otherSelected,
    questionChoice,
    questionReads,
    reasonRequired,
    selectRealOption,
    selected,
    sendAnswer,
    sendClarification,
    setAnswerText,
    setAnswerTextOf,
    setQuestionChoice,
    startChanging,
    submit,
    toggleOther,
    submitGuard,
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
  // One page can host the same ask twice (a decision block and the margin sheet), so the
  // panel each control names, and the line the lone Answer over a closed disclosure is
  // described by, are this instance's.
  const referencesPanelId = useId();
  const loneAnswerReasonId = useId();
  const isCompact = variant === "compact";
  const inBlock = frame === "block";
  // Whether the box for the person's own words is shown is a value of the answer's state, read
  // at each render; no handler sets it (dispatch://INFRA-969). The full card always shows it. A
  // compact card shows it while the answer needs it - Other is ticked, the chosen option needs a
  // reason (Request changes, on an approval), or the answer text holds words Answer would send -
  // and while the person has opened it. So "Other ticked, no box" is a state nothing can reach:
  // on an ask that takes several answers it left the ask unanswerable, since Answer refuses Other
  // without its words. The answer text is the ask's shared draft (`answer-drafts.ts`), or the
  // recorded answer a change is editing, so a second card of the ask and a change both show the
  // box their words are in.
  const ownWordsNeeded = otherSelected || reasonRequired || trimmedAnswer !== "";
  const ownWordsShown = !isCompact || ownWordsNeeded || ownWordsOpen;
  // The disclosure is drawn only where a press of it both opens and folds the box, which is
  // where the answer needs nothing in it: no control hides a box the answer needs, and where the
  // control is drawn the person's own press is all that decides the box (`aria-expanded`).
  const noteFolds = isCompact && !ownWordsNeeded;
  // A box held open by its words alone - Other unticked under them by a real option, an approval
  // moved off Request changes, a change's recorded note - is the box the person is writing in.
  // When those words go, by a deletion of theirs or by the Ask back that sends them, the box stays
  // open as if they had opened it, rather than folding away from under their caret or the button
  // they pressed. Recorded while rendering, React's pattern for state derived from a change, so
  // no frame draws the box gone. Nothing but its words can hold the box open while the form is
  // drawn, Other is unticked and the choice demands no reason (`onlyWordsHold`).
  const onlyWordsHold = completed === null && !otherSelected && !reasonRequired;
  const heldByWords = isCompact && onlyWordsHold && trimmedAnswer !== "";
  const [wordsHeldBox, setWordsHeldBox] = useState(false);
  if (heldByWords !== wordsHeldBox) {
    setWordsHeldBox(heldByWords);
    if (wordsHeldBox && onlyWordsHold) {
      setOwnWordsOpen(true);
    }
  }
  /** The project document this ask belongs to: the one it was asked on, else the one the host
   *  is showing it under. It names the ask's own reference, the replies' references, and where
   *  a file dropped into the box goes. `document` is the Inbox row's: the ask read carries no
   *  owner, so the fresher `displayedAsk` cannot name it and this card's own row is where it
   *  is read. */
  const askOwner = ask.document ?? owner;
  const reference = itemRoute("ask", displayedAsk, askOwner);
  // Where a `#id` in the ask's own text resolves: the document the server scopes this ask to -
  // the one its block is on, the one it is anchored to, else its issue's spec (`refs.ScopeOf`).
  const source = textSourceOf("ask", displayedAsk.id, displayedAsk.issue_key);
  // Where a file pasted or dropped into the answer, or into a reply under the ask, goes.
  const uploadOwner = askUploadOwner(displayedAsk, askOwner);
  // The project whose components the box's picker offers: the document the ask was asked on.
  // An issue ask's box needs none, the issue's key holding it.
  const documentProject = (askOwner ?? displayedAsk.anchor_artifact)?.project;
  // Everything the reader puts in the box goes through here, the keystroke and the text an
  // upload lands alike: a draft the field changed is no longer the question-shaped answer the
  // card offered to ask back instead.
  const changeAnswerText = (next: SetStateAction<string>) => {
    setAnswerText(next);
    setQuestionChoice(false);
  };
  // The uploads behind this card's answer, held here rather than in the box they are started
  // in (`AskAnswerArea`): the compact variant unmounts that box when nothing holds it open -
  // the person folds it, or a real option unticks Other over an empty box - and an upload that
  // went with it left the lone Answer drawn over the folded note sending an answer without the
  // picture (dispatch://INFRA-341, R2). Every send this card draws reads the one `uploading`
  // below, wherever the button stands. A file's text joins the answer of the ask it was pasted
  // or dropped under, which its press carries (`useDraftUpload`, `setAnswerTextOf`); only text
  // added to the field on screen puts its question choice away.
  const upload = useDraftUpload((text: string, askId: string) => {
    const append = (current: string) => appendToDraft(current, text);
    if (askId === ask.id) changeAnswerText(append);
    else setAnswerTextOf(askId, append);
  });
  const uploading = upload.pending > 0;
  const answerFieldRef = useRef<MentionFieldHandle | null>(null);
  // Picking Request changes moves the person straight to the field the server insists on, which
  // the choice itself shows on either variant (`ownWordsShown`).
  useEffect(() => {
    if (reasonRequired) {
      answerFieldRef.current?.textarea()?.focus();
    }
  }, [reasonRequired]);
  // Opening the disclosure is a move into the field it reveals, whoever opened it: the reader's
  // own click, and the Inbox's Enter, which presses this same control (`Inbox.tsx`). The field
  // is drawn by the render the press schedules, so the focus can only be this effect's - the
  // opener runs before React has committed the box and would focus nothing. Only an open by the
  // disclosure itself focuses, which is why the intent is recorded as the control is pressed: a
  // box that Other, an option or the words in it show leaves the reader where they are.
  const focusNoteOnOpen = useRef(false);
  useEffect(() => {
    if (!focusNoteOnOpen.current) return;
    focusNoteOnOpen.current = false;
    if (ownWordsOpen) {
      answerFieldRef.current?.textarea()?.focus();
    }
  }, [ownWordsOpen]);
  // A control that disables itself under the reader's focus - Ask back while sending and once
  // its text is sent - would have the browser drop focus to the document body (in the Inbox, out
  // of the row, which then lets the row go) with nothing the card could act on afterwards; the
  // same hand-off as a vanishing form, in the commit that disables it, before the browser looks.
  const [formRef, trackForm] = useFormRef();
  useLayoutEffect(() => {
    const form = formRef.current;
    const active = document.activeElement;
    if (form !== null && active instanceof HTMLElement && active.matches(":disabled")) {
      handOffFocus(form);
    }
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
  // and the control that would leave the page or say a new refusal with that - Ask back, its text
  // gone, or Ask back instead - stays on the page, refused as it was pressed. So the hand-off
  // above moves no focus while the card refuses, and nothing holding the focus leaves. The one
  // move of the focus comes as the refusal ends: the form's hand-off, or Ask back instead's, as
  // it leaves the page; a focus the reader moved out of the form meanwhile stays where they put
  // it. An upload still out still disables a send, on both sides, and a refusal of the draft
  // (`answerRefusal`, `clarificationRefusal`) refuses it on both sides with its reason. Outside
  // `NoTooltips` each is `disabled` while the write is out and no longer.
  const holdsFocus = !tooltips;
  // The newest write of the form that has landed, by when it was sent: 0 while none has.
  const landedWrite = Math.max(
    mutation.isSuccess ? mutation.submittedAt : 0,
    clarification.isSuccess ? clarification.submittedAt : 0
  );
  // The landing is recorded while rendering, React's pattern for state derived from a change (as
  // `wordsHeldBox` is): an effect would let one render between the landing and the record reach
  // the screen, and a press then would be taken. `readAfter` is when the thread read on the page
  // at the landing was made; the card waits for a newer one while the thread is being read.
  // Outside `NoTooltips` nothing waits, so nothing is recorded and the card renders as it does
  // there.
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
  const refusedWhileOut = holdsFocus && refusing;
  const disabledWhileOut = !holdsFocus && isSubmitting;
  // What a refused control is drawn from, recorded while rendering as the landing is, in the
  // first render that refuses: the press, before the landing empties the box.
  const formNow = { answerRefusal, choice: questionChoice, clarificationRefusal };
  const [heldForm, setHeldForm] = useState<typeof formNow | null>(null);
  const held = refusedWhileOut ? (heldForm ?? formNow) : null;
  if (held !== heldForm) setHeldForm(held);
  const answerRefusalShown = held === null ? answerRefusal : held.answerRefusal;
  const clarificationRefusalShown =
    held === null ? clarificationRefusal : held.clarificationRefusal;
  const choiceShown = held === null ? questionChoice : held.choice;
  // A send this card draws keeps the focus while the card refuses, unless an upload holds it:
  // that one is `disabled` on both sides (`busy`).
  const sendRefusedWhileOut = refusedWhileOut && !uploading;
  // Ask back instead takes focus when it appears. Keep the ref stable so a re-render cannot
  // steal focus from Answer with it anyway or from another card editing the shared draft.
  // Inside `NoTooltips`, if the button still holds focus when the choice leaves, hand off
  // focus rather than dropping it to the body.
  const focusAskBackInstead = useCallback(
    (button: HTMLButtonElement | null) => {
      if (button === null) return undefined;
      button.focus();
      if (!holdsFocus) return undefined;
      return () => {
        const form = formRef.current;
        if (form !== null && document.activeElement === button) handOffFocus(form);
      };
    },
    [formRef, holdsFocus]
  );
  // A block ask the server indexed with no pending author has an empty label and reads as
  // "asked", with no session handles to open.
  const sessionAuthor = displayedAsk.author.kind === "session" ? displayedAsk.author : undefined;
  // The copy handles' values: the title the session stamped, as the clipboard has always had it.
  const sessionTitle = sessionAuthor?.origin?.session_title?.trim();
  const tmuxTarget = sessionAuthor?.origin?.tmux;
  const urgency = askUrgency(displayedAsk.urgency);
  const notched = hasUrgencyNotch(displayedAsk, inBlock);
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
  // A reply's own `dispatch://` reference needs the project document behind a document ask
  // (`askOwner`), which only an Inbox row carries on the ask itself.
  const threadNode =
    thread === "collapsed" ? (
      <AskThreadDisclosure
        ask={currentAsk}
        createReply={reply}
        currentReplyId={currentReplyId}
        embedded={completed === null}
        markNewest={answerIsNewest ? undefined : markNewest}
        order={threadOrder}
        owner={askOwner}
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
        owner={askOwner}
        thread={threadQuery}
        unread={unreadReplies}
        uploadOwner={uploadOwner}
      />
    );

  const answerLabel = changing ? "Save answer" : "Answer";
  /** What Answer says while it is busy: this card's own send, or an upload the draft is waiting
   *  on, which keeps the label while the picture lands. */
  const answerBusy = mutation.isPending ? (changing ? "Saving…" : "Answering…") : undefined;
  /** Answer, in each place this card draws it: inside the answer area, and as the lone button a
   *  compact card shows over a closed note disclosure. Only its own send, or an upload the draft
   *  is waiting on, disables it; every other reason it cannot be pressed it states and refuses.
   *  The upload it waits on is the card's (`uploading`), not the box's, so both places are held
   *  by the same picture and neither can be drawn without the gate. Inside `NoTooltips` its own
   *  send, and the read after it, refuse it holding the focus (`sendRefusedWhileOut`). */
  const submitButton = (shownBy: string | undefined) => (
    <RefusableButton
      busy={answerBusy ?? (uploading ? answerLabel : undefined)}
      className="min-h-11"
      refusal={answerRefusalShown}
      refusalShownBy={shownBy}
      refusedWhileOut={sendRefusedWhileOut}
      type="submit"
    >
      {answerLabel}
    </RefusableButton>
  );
  const answerActions = ({ reasonId }: AskAnswerActions<AnswerRefusalName>): ReactNode =>
    choiceShown ? (
      // The pair a question-shaped answer offers is two more sends of this card, so a picture
      // the draft is still taking holds them both, in the shape their siblings keep: the label
      // stands and the press is taken away until the upload lands (`submitButton`, Ask back).
      <fieldset aria-label="Question-shaped answer" className="flex gap-2">
        <button
          aria-disabled={sendRefusedWhileOut || undefined}
          className={`min-h-11 ${tooltips ? primaryButton : `rounded-lg px-3 py-2 text-sm font-semibold ${ON_PAGE.filled}`}`}
          disabled={disabledWhileOut || uploading}
          onClick={() => {
            if (!refusing) sendClarification();
          }}
          ref={focusAskBackInstead}
          type="button"
        >
          {clarification.isPending ? "Sending…" : "Ask back instead"}
        </button>
        <RefusableButton
          busy={mutation.isPending ? "Answering…" : uploading ? "Answer with it anyway" : undefined}
          look="secondary"
          onPress={() => sendAnswer(answerText)}
          refusal={answerRefusalShown}
          refusalShownBy={reasonId.anyway}
          refusedWhileOut={sendRefusedWhileOut}
        >
          Answer with it anyway
        </RefusableButton>
      </fieldset>
    ) : (
      <div className="flex gap-2">
        {submitButton(reasonId.answer)}
        {changing ? (
          <button
            aria-disabled={refusedWhileOut || undefined}
            className={
              tooltips
                ? secondaryButton
                : `min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${ON_PAGE.action}`
            }
            disabled={disabledWhileOut}
            onClick={() => {
              if (!refusing) cancelChanging();
            }}
            type="button"
          >
            Cancel
          </button>
        ) : (
          <RefusableButton
            busy={clarification.isPending ? "Sending…" : uploading ? "Ask back" : undefined}
            look="secondary"
            onPress={sendClarification}
            refusal={clarificationRefusalShown}
            refusalShownBy={reasonId.askBack}
            refusedWhileOut={sendRefusedWhileOut}
          >
            Ask back
          </RefusableButton>
        )}
      </div>
    );
  /** The card's half of the one answer area (`AskAnswerArea.tsx`): the draft and the field's paste,
   *  drop and keys are the area's, and the box and these submits are the card's, because Answer,
   *  Save answer and Ask back all post through `useAskAnswerForm` rather than as a comment of the
   *  box's own, and the box is the ask card's one row of the writing list. The upload is the
   *  card's too, since a send of the card's stands where this area does not. */
  const answerArea = (
    <AskAnswerArea
      actions={answerActions}
      draft={answerText}
      field={(props) => (
        <MentionField
          {...props}
          askAnswer={true}
          className="font-normal"
          disabled={disabledWhileOut}
          fieldRef={answerFieldRef}
          issueKey={displayedAsk.issue_key ?? undefined}
          label={reasonRequired ? "Reason (required)" : isApproval ? "Reason" : "Your answer"}
          labelHidden={!reasonRequired}
          placeholder={answerPlaceholder}
          project={documentProject}
          writing={dispatchWritingBox("ask-answer", "ask-answer")}
        />
      )}
      onDraftChange={changeAnswerText}
      // Inside `NoTooltips`, while the card refuses, the box is read-only in place of disabled
      // and takes no paste, drop or Ctrl+Enter.
      readOnly={refusedWhileOut}
      // Every name the card's buttons know, whichever pair is drawn: a reason that is null
      // shows no line, so the two the other pair uses cost the card nothing.
      refusals={
        choiceShown
          ? { answer: null, anyway: answerRefusalShown, askBack: null }
          : {
              answer: answerRefusalShown,
              anyway: null,
              askBack: changing ? null : clarificationRefusalShown,
            }
      }
      upload={upload}
      uploadDraft={ask.id}
      uploadOwner={uploadOwner}
    />
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
          // `askOwner`, not the host's `owner`: in the Inbox and the reading pane the document
          // is the row's own and the host passes none, and `completed` is the ask read, which
          // carries no document either - so the header had nothing to build the ask's reference
          // from and drew no copy-link control at all (dispatch://INFRA-341, R1). The replies
          // under the same card already take this one.
          owner={askOwner}
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
          : `relative rounded-xl border-l-4 shadow-sm ${isCompact ? "px-3 pt-4 pb-3" : "px-4 pt-5 pb-4"} ${notched ? "mt-3" : ""} ${tooltips ? `${card} ${askUrgencyAccent[urgency]}` : `${ON_PAGE.frame} ${PAGE_URGENCY_ACCENT[urgency]}`}`
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
      {/* One provenance line under the question, in the spec's decided conversation grammar:
          whose turn it is (the only part a reader acts on), who asked, when, and the ask's
          own reference, drawn by the one header every surface shows an ask through. The
          session's own handles - its ID, its title, its tmux target - are chrome a reader
          wants perhaps once a week, so they fold behind the author chip and open onto their
          own row. That keeps the line inside the card at 390 px and in a 280 px margin, where
          a single row could not hold five copy controls without pushing the reference out of
          reach. */}
      <AskHeader
        ask={displayedAsk}
        control={(name) => (
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
        inBlock={inBlock}
        lastReply={(threadQuery.data?.replies ?? []).at(-1)}
        owner={askOwner}
        place="card"
        trailing={
          !handlesOpen ? null : (
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
          )
        }
      >
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
            <p
              className={`mt-1 text-sm ${tooltips ? `${linkText} ${linkHoverText}` : ON_PAGE.link}`}
            >
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
        </div>
      </AskHeader>
      {/* Whose question it is now, and the one control that hands it to somebody else, below the
          provenance line rather than inside the header: a header's `children` are drawn between
          the anchor and that line, where this control would sit between the reader and the
          question's own answer. An ask that is not open waits on nobody, so it has none: a card
          whose ask is answered, resolved or withdrawn draws its record instead, and the one case
          that reaches here is a reader changing the answer of a closed ask. The ask is the
          fresher read and the issue is the card's own prop: only an Inbox row carries one, and
          the ask reads do not. */}
      {displayedAsk.state === "open" ? (
        <AskHandOn ask={displayedAsk} assignee={ask.issue?.assignee ?? null} />
      ) : null}
      <AskEditHistory ask={displayedAsk} edits={edits} />
      {referencedByNode}
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
          <fieldset aria-disabled={refusedWhileOut || undefined} className="min-w-0 space-y-2">
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
                  if (!refusing) selectRealOption(option.label);
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
                  if (!refusing) toggleOther();
                }}
                option={{ label: "Other" }}
                source={source}
              />
            )}
          </fieldset>
        )}
        {isCompact ? (
          <>
            {selected.length === 0 || ownWordsShown ? null : (
              // Over a folded box there is no area: the lone Answer still says why it refuses,
              // and what holds it, on lines of this card's own. It follows the box's own value,
              // so the card never draws two Answers. The upload outlives the box it was started
              // in, so the picture still being taken says so here rather than disappearing with
              // the note it was pasted into.
              <div className="space-y-2">
                <DraftUploadStatus upload={upload} />
                {answerRefusalShown === null ? null : (
                  <p
                    className={`text-xs ${tooltips ? textMutedOnSurface : ON_PAGE.quiet}`}
                    id={loneAnswerReasonId}
                  >
                    {answerRefusalShown}
                  </p>
                )}
                {submitButton(loneAnswerReasonId)}
              </div>
            )}
            {/* The control comes and goes above the box, never around it: the box keeps its place
                in the tree, so the field the person is typing in stays mounted, and keeps the
                focus, as the words they type take the control away. */}
            <div>
              {noteFolds ? (
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
              ) : null}
              {ownWordsShown ? (
                <div
                  className={noteFolds ? "space-y-3 pt-3" : "space-y-3"}
                  id={`${answerFieldId}-disclosure`}
                >
                  {answerArea}
                </div>
              ) : null}
            </div>
          </>
        ) : (
          answerArea
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
