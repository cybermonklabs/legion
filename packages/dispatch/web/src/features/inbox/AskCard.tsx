import { DISPATCH_EVENT_BODY_MAX } from "@legion/contracts";
import { useQueryClient } from "@tanstack/react-query";
import {
  type ReactNode,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import {
  type ArtifactOwner,
  api,
  apiErrorMessage,
  type DeclineAskInput,
  type WriteBounds,
} from "../../api/client";
import { inboxQuery } from "../../api/queries";
import type {
  AnswerAskInput,
  Ask,
  AskRead,
  AskUrgency,
  Comment,
  CreateCommentInput,
  InboxRow,
} from "../../api/types";
import { CopyButton } from "../../components/CopyButton";
import { ChevronIcon } from "../../components/DisclosureToggle";
import { QueryError } from "../../components/QueryError";
import { TruncatedText } from "../../components/TruncatedText";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { compareTimestamps } from "../../lib/timestamps";
import {
  askUrgencyAccent,
  backdrop40,
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  borderDefault,
  card,
  cardHoverBorder,
  dangerText,
  inlineWarningText,
  inputClasses,
  linkHoverText,
  linkText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceBg,
  textMutedHoverToSecondary,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { appendToDraft, DraftUploadStatus, useDraftUpload } from "../artifacts/ArtifactUpload";
import { AgentName, useAgentText } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
import { MarkdownBody } from "../refs/MarkdownBody";
import { ReferencedBy, ReferencedByToggle } from "../refs/ReferencedBy";
import { buildDispatchReference, itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { useDialog } from "../shell/useDialog";
import { composerFirst } from "../threads/thread-layout";
import { useThreadOrder } from "../threads/thread-order";
import { askRepliesOwner, useMarkThreadRead } from "../threads/useMarkThreadRead";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { AskAnchorHeader } from "./AskAnchorLink";
import { AskApprovalLink } from "./AskApprovalLink";
import { AskBlockLink } from "./AskBlockLink";
import {
  AskCompletionCard,
  AskEditHistory,
  type AskFrame,
  OrphanedAnchorNotice,
} from "./AskCompletionCard";
import { AskChoiceRow } from "./AskOptionRow";
import { AskRecipients } from "./AskRecipients";
import { AskThread } from "./AskThread";
import { AskThreadDisclosure } from "./AskThreadDisclosure";
import type { OwnedAsk } from "./ask-name";
import { askTurnHolder } from "./ask-turn";
import { askUrgency, URGENCY_LABELS } from "./ask-urgency";
import { EarlierApproval } from "./EarlierApproval";
import { markAnswerOutstanding } from "./outstanding-answers";
import { clearReplyDraft } from "./reply-drafts";
import { invalidateAskReads, useAskAnswerForm } from "./useAskAnswerForm";

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

/**
 * What **Decline to answer** opens: the person's note, which is optional, and the two controls.
 * It follows the document's **Request changes** dialog (`doc/ApprovalChip.tsx`) and adds the
 * count of what they may still write, in the UTF-16 units the server counts
 * (`DISPATCH_EVENT_BODY_MAX`). The field stops at that bound, which is where the count reaches
 * zero, so a long note meets the bound here rather than as a refusal from the server.
 */
function DeclineDialog({
  error,
  onClose,
  onSubmit,
  saving,
}: {
  /** What the server answered the last decline with, or null while none was refused. */
  error: Error | null;
  onClose: () => void;
  onSubmit: (note: string) => void;
  saving: boolean;
}): ReactNode {
  const [note, setNote] = useState("");
  const noteId = useId();
  const countId = `${noteId}-left`;
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const dialog = useDialog<HTMLDivElement>({ initialFocusRef: textareaRef, onClose, open: true });
  const left = DISPATCH_EVENT_BODY_MAX - note.length;

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop40}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-0 z-40 flex items-start justify-center p-4 pt-20">
        <div
          aria-label="Decline to answer"
          aria-modal="true"
          className={`pointer-events-auto w-full max-w-sm space-y-3 rounded-lg border p-4 shadow-xl ${card}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Decline to answer</h2>
          <p className={`text-sm ${textMutedOnSurface}`}>
            The question closes with no answer. Dispatch tells the agent that asked to decide
            without one.
          </p>
          <label className={`block text-sm font-medium ${textSecondaryOnSurface}`} htmlFor={noteId}>
            Note (optional)
            <textarea
              aria-describedby={countId}
              className={`mt-1 block w-full rounded-lg border px-2 py-1.5 text-sm font-normal outline-none ${inputClasses(false)}`}
              disabled={saving}
              id={noteId}
              maxLength={DISPATCH_EVENT_BODY_MAX}
              onChange={(event) => setNote(event.target.value)}
              onKeyDown={(event) =>
                submitOnModifiedEnter(event, {
                  onFormlessSubmit: () => {
                    if (!saving) onSubmit(note.trim());
                  },
                })
              }
              ref={textareaRef}
              value={note}
            />
          </label>
          <p className={`text-xs ${textMutedOnSurface}`} id={countId}>
            {left} {left === 1 ? "character" : "characters"} left
          </p>
          {error === null ? null : (
            <p className={`text-sm ${dangerText}`} role="alert">
              {/* The server's own words where it gave any - the note is too long, the ask is an
                  approval request, it is already closed - since each says what to do next. */}
              {apiErrorMessage(error, "Could not decline this question. Try again.")}
            </p>
          )}
          <div className="flex items-center justify-end gap-2">
            <button
              className={`rounded-lg border px-3 py-1.5 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              disabled={saving}
              onClick={onClose}
              type="button"
            >
              Cancel
            </button>
            <button
              className={`rounded-lg px-3 py-1.5 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
              disabled={saving}
              onClick={() => onSubmit(note.trim())}
              type="button"
            >
              {saving ? "Declining…" : "Decline to answer"}
            </button>
          </div>
        </div>
      </div>
    </>
  );
}

/**
 * The person's **Decline to answer**: they close an open question with no answer, and Dispatch
 * tells the agent that asked to decide without one. It is drawn on an open question alone -
 * an approval request is answered with Approve or Request changes, and the server refuses a
 * decline of one - and, since every surface that shows an ask shows this card, writing it here
 * reaches all of them: the Inbox row, the reading pane, the Conversation, a document's decision
 * block, the margin and the Answers page.
 *
 * The write takes the row out of the Inbox the moment it goes, as the answer write does, so the
 * ask is held while the server decides it (`outstanding-answers.ts`): the reading column keeps
 * the question where the reader pressed, and the refusal is shown by the card that wrote it.
 * The hold is that shared store rather than a report up to the Inbox, as `SnoozeControl` makes
 * one, for the reason the store exists: this card is drawn on surfaces that take no prop of the
 * Inbox's, a document's own decision block among them.
 */
function DeclineControl({
  ask,
  decline,
  onDeclined,
}: {
  ask: Ask;
  decline: (id: string, input: DeclineAskInput, bounds: WriteBounds) => Promise<Ask>;
  /** Called once the server has recorded the decline, as an answer from this card calls it. */
  onDeclined?: (id: string) => void;
}): ReactNode {
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const [open, setOpen] = useState(false);
  // The dialog shows a refused decline itself, over the note it keeps, so the write is inline:
  // the shell's indicator carries it only while it is out.
  const write = useKeyedWrite({
    inline: true,
    label: () => `Decline of “${ask.question}”`,
    write: (input: DeclineAskInput, bounds) => decline(ask.id, input, bounds),
    onMutate: async () => {
      // Registered before the row goes, never a commit after it: the reading column reads the
      // hold at the commit this removal makes.
      markAnswerOutstanding(ask.id, true);
      await queryClient.cancelQueries({ queryKey: inboxQuery().queryKey });
      const previous = queryClient.getQueryData<InboxRow[]>(inboxQuery().queryKey);
      queryClient.setQueryData<InboxRow[]>(inboxQuery().queryKey, (current) =>
        current?.filter((row) => row.id !== ask.id)
      );
      return previous;
    },
    onError: (_error, _input, previous) => {
      queryClient.setQueryData(inboxQuery().queryKey, previous);
      void queryClient.invalidateQueries({ queryKey: inboxQuery().queryKey });
    },
    onSettled: () => submitGuard.release(),
    onSuccess: (declined) => {
      setOpen(false);
      // The card reads its ask from the thread, so the record it swaps to is this decline.
      if (declined !== null) {
        queryClient.setQueryData<AskRead>(["ask-thread", ask.id], (current) =>
          current === undefined ? undefined : { ...current, ask: declined }
        );
      }
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
      invalidateAskReads(queryClient, ask);
      onDeclined?.(ask.id);
    },
  });
  // The same `pending || failed` liveness the snooze and the answer report, let go once the
  // server has decided the decline and when the card goes.
  const live = write.isPending || write.isError;
  useEffect(() => {
    if (!live) return;
    markAnswerOutstanding(ask.id, true);
    return () => markAnswerOutstanding(ask.id, false);
  }, [ask.id, live]);

  return (
    <div className="mt-3">
      <button
        className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`}
        disabled={write.isPending}
        onClick={() => setOpen(true)}
        type="button"
      >
        {write.isPending ? "Declining…" : "Decline to answer"}
      </button>
      {open ? (
        <DeclineDialog
          error={write.isError ? write.error : null}
          onClose={() => {
            write.reset();
            setOpen(false);
          }}
          onSubmit={(note) => submitGuard.guard(() => write.mutate(note === "" ? {} : { note }))}
          saving={write.isPending}
        />
      ) : null}
    </div>
  );
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
    isApproval,
    isSubmitting,
    mutation,
    otherSelected,
    questionChoice,
    reasonRequired,
    requiresReason,
    selectRealOption,
    selected,
    sendAnswer,
    sendClarification,
    setAnswerText,
    setQuestionChoice,
    startChanging,
    submit,
    submitHint,
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
  // A change seeds the current answer's note; a compact card shows it rather than folding text
  // the save would send behind its disclosure. Set while rendering the first changing render,
  // React's pattern for state derived from a prop change, so the field is never hidden a frame.
  const [seededFor, setSeededFor] = useState(false);
  if (changing !== seededFor) {
    setSeededFor(changing);
    if (changing && variant === "compact" && answerText.trim() !== "") setOwnWordsOpen(true);
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
  const source = textSourceOf("ask", displayedAsk.id);
  // Where a file pasted or dropped into the answer, or into a reply under the ask, goes: the
  // ask's issue, or the project of the document it was asked on.
  const documentProject = (ask.document ?? owner ?? displayedAsk.anchor_artifact)?.project;
  const uploadOwner: ArtifactOwner | undefined =
    displayedAsk.issue_key !== null
      ? { issue: displayedAsk.issue_key }
      : documentProject === undefined
        ? undefined
        : { project: documentProject };
  const upload = useDraftUpload((text) => {
    setAnswerText((current) => appendToDraft(current, text));
    setQuestionChoice(false);
  });
  const uploading = upload.pending > 0;
  const answerFieldRef = useRef<HTMLTextAreaElement>(null);
  // Picking Request changes moves the person straight to the field the server insists
  // on. Keyed on the field actually being mounted, not just on the option: the compact variant
  // unmounts the field when its disclosure closes, and reopening it must refocus.
  const reasonFieldShown = reasonRequired && (!isCompact || ownWordsOpen);
  useEffect(() => {
    if (reasonFieldShown) {
      answerFieldRef.current?.focus();
    }
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
      answerFieldRef.current?.focus();
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
  // The copy handles' values: the title the session stamped, as the clipboard has always had it.
  const sessionTitle = sessionAuthor?.origin?.session_title?.trim();
  const tmuxTarget = sessionAuthor?.origin?.tmux;
  const urgency = askUrgency(displayedAsk.urgency);
  const hasUrgencyNotch = !inBlock && (urgency === "blocking" || urgency === "high");
  // The thread's own "still open?" wording must track the post-answer ask, not the possibly
  // stale prop passed to this instance: `completed` renders before an invalidated `ask` prop
  // round-trips down from the parent.
  const currentAsk = completed ?? displayedAsk;
  // A resolved ask takes no more replies, so an unsent one has nowhere to go.
  const resolved = displayedAsk.state === "resolved" || currentAsk.state === "resolved";
  useEffect(() => {
    if (resolved) clearReplyDraft(displayedAsk.id);
  }, [displayedAsk.id, resolved]);
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

  const answerField = (
    <>
      <label className="block" htmlFor={answerFieldId}>
        <span
          className={
            reasonRequired ? `mb-1 block text-sm font-medium ${textSecondaryOnSurface}` : "sr-only"
          }
        >
          {reasonRequired ? "Reason (required)" : isApproval ? "Reason" : "Your answer"}
        </span>
        <textarea
          className={`block w-full rounded-lg px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
          data-ask-answer=""
          disabled={isSubmitting}
          id={answerFieldId}
          onChange={(event) => {
            setAnswerText(event.target.value);
            setQuestionChoice(false);
          }}
          onDrop={
            uploadOwner === undefined ? undefined : (event) => upload.drop(event, uploadOwner)
          }
          onKeyDown={(event) => submitOnModifiedEnter(event, { disabled: uploading })}
          onPaste={
            uploadOwner === undefined ? undefined : (event) => upload.paste(event, uploadOwner)
          }
          placeholder={answerPlaceholder}
          ref={answerFieldRef}
          rows={2}
          value={answerText}
        />
      </label>
      <DraftUploadStatus upload={upload} />
    </>
  );
  const submitButton = (
    <button
      aria-describedby={submitHint === undefined ? undefined : submitHintId}
      className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
      disabled={!canAnswer || isSubmitting || uploading}
      title={submitHint}
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
      <p className={`text-xs ${textMutedOnSurface}`} id={submitHintId}>
        {submitHint}
      </p>
    );
  const answerActions = questionChoice ? (
    <fieldset aria-label="Question-shaped answer" className="flex gap-2">
      <button
        className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
        disabled={isSubmitting}
        onClick={sendClarification}
        ref={(node) => node?.focus()}
        type="button"
      >
        {clarification.isPending ? "Sending…" : "Ask back instead"}
      </button>
      <button
        className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`}
        disabled={isSubmitting}
        onClick={() => sendAnswer(answerText)}
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
            className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`}
            disabled={isSubmitting}
            onClick={cancelChanging}
            type="button"
          >
            Cancel
          </button>
        ) : (
          <button
            className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`}
            disabled={trimmedAnswer === "" || isSubmitting || uploading}
            onClick={sendClarification}
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
          <div
            className={`mt-3 rounded-xl border p-3 ${borderDefault}`}
            data-testid="ask-unsent-answer"
          >
            <p className={`text-xs ${textMutedOnSurface}`}>
              This question closed before you sent your answer, so it was not sent. Your text is
              here to copy.
            </p>
            <p className={`mt-1 text-sm whitespace-pre-wrap ${textPrimaryOnSurface}`}>
              {unsentAnswer}
            </p>
          </div>
        )}
        {answerFailure === null ? null : <QueryError message={answerFailure.message} />}
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
          : `relative rounded-xl border-l-4 shadow-sm ${isCompact ? "px-3 pt-4 pb-3" : "px-4 pt-5 pb-4"} ${hasUrgencyNotch ? "mt-3" : ""} ${card} ${askUrgencyAccent[urgency]}`
      }
      // The one handle on a card as a whole, carrying the question the card draws: a page-level
      // key that must not fire while the reader is answering an ask (the issue page's priority
      // digits, beside this card's own options) matches the bare attribute, and the Inbox's
      // answer keys find the selected question's own card by its value (`answerHand` in
      // `Inbox.tsx`). Neither asks for a test id.
      data-ask-card={displayedAsk.id}
      data-testid={`ask-${displayedAsk.id}`}
    >
      {hasUrgencyNotch ? (
        <span
          aria-hidden="true"
          className={`absolute -top-2 left-3 rounded px-1.5 text-[10px] leading-4 font-semibold tracking-wide uppercase ${surfaceBg} ${URGENCY_NOTCH_TEXT[urgency]}`}
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
        <p className={`mt-2 text-sm ${linkText} ${linkHoverText}`}>
          <AskBlockLink ask={displayedAsk} />
        </p>
      )}
      <div>
        {isApproval ? (
          <p className={`text-[10px] font-semibold uppercase tracking-wide ${textMutedOnSurface}`}>
            Approval requested
          </p>
        ) : null}
        {isApproval && documentLink ? (
          <p className={`mt-1 text-sm ${linkText} ${linkHoverText}`}>
            <AskApprovalLink ask={displayedAsk} />
          </p>
        ) : null}
        {inBlock ? null : (
          <div className={`text-[15px] leading-relaxed font-medium ${textPrimaryOnSurface}`}>
            <MarkdownBody markdown={displayedAsk.question} source={source} />
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
          className={`@container mt-1.5 flex flex-wrap items-center gap-x-1.5 text-xs ${textMutedOnSurface}`}
        >
          {/* Turn, author and time are one group whose names truncate: a separator that wrapped
              left a line opening - or ending - on a bare "·", and the agents are the only parts
              that can give up width ("Waiting on you" never does). Below 24rem of line (a phone,
              a margin) the turn takes a line of its own with no separator after it, and the
              asker and time share the next: beside the turn and the live link's 44 px touch box
              the asker's name kept only two letters. A block ask the server indexed with no
              pending author has an empty label and still says when it was asked. */}
          <span className="flex min-w-0 items-center gap-x-1.5 whitespace-nowrap @max-sm:flex-wrap">
            {turnHolder === null ? null : (
              <span
                className={`${turnHolder === "you" ? "shrink-0" : "min-w-0 truncate"} font-semibold @max-sm:basis-full ${textSecondaryOnSurface}`}
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
                <TruncatedText className="min-w-0" title={askerTitle}>
                  {displayedAsk.author.id === "" ? (
                    "asked"
                  ) : (
                    <AgentName actor={displayedAsk.author} />
                  )}
                </TruncatedText>
              ) : (
                // The asker's button keeps its name and opens the session's copy handles; the
                // component's live link sits beside it, outside the button. `whitespace-pre`
                // keeps the space before an attribution suffix, which a flex item would drop.
                <AgentName
                  actor={sessionAuthor}
                  className="flex min-w-0 items-center whitespace-pre"
                  control={(name) => (
                    <button
                      aria-expanded={handlesOpen}
                      className={`flex min-w-0 items-center gap-1 ${textMutedHoverToSecondary}`}
                      onClick={() => setHandlesOpen((open) => !open)}
                      type="button"
                    >
                      <TruncatedText className="min-w-0" title={askerTitle}>
                        {name}
                      </TruncatedText>
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
        <AskEditHistory ask={displayedAsk} edits={edits} />
        {referencedByNode}
      </div>
      {askChanged ? (
        <p className={`mt-3 text-sm font-medium ${inlineWarningText}`}>
          This question changed while you were answering. Review the latest wording and confirm your
          answer again; your draft text is still here.
        </p>
      ) : null}
      {/* The options and the answer box sit where the order puts a thread's reply box: under
          the question in Newest first, at the foot of the card in Oldest first. */}
      {answerFirst ? null : threadNode}
      <form className="mt-3 space-y-3" onSubmit={submit} ref={trackForm}>
        {(displayedAsk.options ?? []).length === 0 ? null : (
          <fieldset className="min-w-0 space-y-2">
            <legend className="sr-only">Answer options</legend>
            {(displayedAsk.options ?? []).map((option) => (
              <AskChoiceRow
                checked={selected.includes(option.label)}
                disabled={isSubmitting}
                hotkey
                key={option.label}
                multiple={displayedAsk.multiple}
                name={`ask-${displayedAsk.id}`}
                onChange={() => {
                  selectRealOption(option.label);
                  if (isCompact)
                    setOwnWordsOpen(requiresReason(option.label) || trimmedAnswer !== "");
                }}
                option={option}
              />
            ))}
            {isApproval ? null : (
              <AskChoiceRow
                checked={otherSelected}
                disabled={isSubmitting}
                multiple={displayedAsk.multiple}
                name={`ask-${displayedAsk.id}`}
                onChange={() => {
                  toggleOther();
                  if (isCompact && !otherSelected) setOwnWordsOpen(true);
                }}
                option={{ label: "Other" }}
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
                className={`min-h-11 text-sm font-medium ${textSecondaryOnSurface}`}
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
            message="Could not send your clarification."
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
