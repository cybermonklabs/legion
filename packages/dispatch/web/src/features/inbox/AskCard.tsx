import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";

import { type ArtifactOwner, api, type DeclineAskInput, type WriteBounds } from "../../api/client";
import { inboxQuery, whoAmIQuery } from "../../api/queries";
import type {
  AnswerAskInput,
  Ask,
  AskRead,
  Comment,
  CreateCommentInput,
  InboxRow,
} from "../../api/types";
import { AnchoredPanel } from "../../components/AnchoredPanel";
import { CopyButton } from "../../components/CopyButton";
import { ChevronIcon } from "../../components/DisclosureToggle";
import { QueryError } from "../../components/QueryError";
import { TruncatedText } from "../../components/TruncatedText";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { compareTimestamps } from "../../lib/timestamps";
import {
  askQuestionProse,
  askUrgencyAccent,
  borderDefault,
  card,
  cardHoverBorder,
  inlineWarningText,
  inputClasses,
  linkHoverText,
  linkText,
  menuRule,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
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
import { useKeymapScope } from "../shell/keymap";
import { composerFirst } from "../threads/thread-layout";
import { useThreadOrder } from "../threads/thread-order";
import { askRepliesOwner, useMarkThreadRead } from "../threads/useMarkThreadRead";
import { AskAnchorHeader } from "./AskAnchorLink";
import { AskApprovalLink } from "./AskApprovalLink";
import { AskBlockLink } from "./AskBlockLink";
import {
  AskCompletionCard,
  AskEditHistory,
  type AskFrame,
  OrphanedAnchorNotice,
} from "./AskCompletionCard";
import { AskHandOnPicker } from "./AskHandOnPicker";
import { AskChoiceRow } from "./AskOptionRow";
import { AskRecipients } from "./AskRecipients";
import { type AskMenuItem, AskSplitButton } from "./AskSplitButton";
import { AskThread } from "./AskThread";
import { AskThreadDisclosure } from "./AskThreadDisclosure";
import {
  type AskCardActions,
  askCardLeadKeys,
  registerAskCard,
  useAskCardKeys,
} from "./ask-card-keys";
import type { OwnedAsk } from "./ask-name";
import { type AskRewrite, rewriteAsk, rewriteOf } from "./ask-rewrite";
import { askTurnHolder } from "./ask-turn";
import { askUrgency, URGENCY_LABELS } from "./ask-urgency";
import { DeclineControl } from "./DeclineControl";
import { DefinePopover, type SelectedWords, selectedWordsIn } from "./DefinePopover";
import { EarlierApproval } from "./EarlierApproval";
import { AskHandOnLiveContext } from "./hand-on-live";
import { askWaitingOnPerson } from "./list-row";
import { invalidateOwnerReads } from "./owner-reads";
import { RewriteBadge } from "./RewriteBadge";
import { clearReplyDraft } from "./reply-drafts";
import { useAskAnswerForm } from "./useAskAnswerForm";
import { type AskHandOnWrite, useAskHandOn } from "./useAskHandOn";

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
  /** A decision block's header, where the card draws its provenance line (`frame="block"`):
   *  null while the block has not mounted it yet. A block host that passes none gets the line
   *  in the card. */
  provenanceSlot?: HTMLElement | null;
}

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
 * The hand-on write of an open ask, which the card's **Hand this question to…** makes: the
 * menu item, or `a`, opens the people picker (`AskHandOnPicker`), and the card names whom the
 * question waits on in the line under it. The person is the server's own answer
 * (`waiting_on_person`: whoever the ask was handed to, else the assignee of its issue, else the
 * default assignee of its project, which every project names).
 *
 * A refusal shows the server's reason with Retry under that line, as `AssignToMe` and
 * `SnoozeControl` do - and, as those two do, the card asks the list drawing it to hold the ask
 * while that write is live (pending or failed), because the optimistic `waiting_on_person` takes
 * the row out of the Inbox's own list the moment the write starts (`isMine`,
 * `inAssigneeFilter`): without the hold this card would be unmounted before the server
 * answered, and a refusal would put the row back with a fresh card that never failed, so the
 * reader would see it return and no reason. The hold comes from the list itself, through the
 * context it provides (`hand-on-live.ts`), rather than from a prop every host passes down: the
 * card the reader pressed can be one the Inbox never rendered itself - the decision block its
 * reading column draws through the document page - and a prop that path does not carry is a
 * hold that path does not get. What the list does with the request is the list's own: it holds
 * an ask it already listed a row for and ignores one it did not. A card drawn where no list can
 * drop it registers with nobody.
 */
function useHeldHandOn(ask: Ask): AskHandOnWrite {
  const write = useAskHandOn(ask);
  const onLive = useContext(AskHandOnLiveContext);
  const live = write.pending || write.failed;
  useEffect(() => {
    if (!live) return;
    onLive(ask.id, true);
    return () => onLive(ask.id, false);
  }, [ask.id, live, onLive]);
  return write;
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
  provenanceSlot,
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
  // The asker's chevron opens a popover: the session's handles and whom the answer reaches.
  const [askerOpen, setAskerOpen] = useState(false);
  const askerRef = useRef<HTMLButtonElement>(null);
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
  // The card's other actions, behind the split button's chevron and on the card's keys: Rewrite,
  // Define, Ask back, Hand this question to and Decline to answer (`e2e/ask-card-demo/README.md`).
  const queryClient = useQueryClient();
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const articleRef = useRef<HTMLElement | null>(null);
  const actionsRef = useRef<HTMLDivElement>(null);
  const [menuOpen, setMenuOpen] = useState(false);
  // Define reads the words selected when the menu opened: the menu's own focus may draw the
  // selection as inactive, and a press on an item must still find it.
  const [menuSelection, setMenuSelection] = useState<SelectedWords | null>(null);
  const [handOnOpen, setHandOnOpen] = useState(false);
  const [declineOpen, setDeclineOpen] = useState(false);
  const [askingBack, setAskingBack] = useState(false);
  const [showingOriginal, setShowingOriginal] = useState(false);
  const [definition, setDefinition] = useState<SelectedWords | null>(null);
  const [focusWithin, setFocusWithin] = useState(false);
  const handOn = useHeldHandOn(displayedAsk);
  const rewrite = rewriteOf<AskRewrite["original"]>(displayedAsk);
  const rewriting = useMutation({
    mutationFn: () => rewriteAsk(displayedAsk.id),
    onSuccess: (rewritten) => {
      // The card shows the rewrite at once: the thread it reads and the Inbox row it was drawn
      // from both take the ask the route answered, and every owner list reads it again.
      queryClient.setQueryData<AskRead>(["ask-thread", displayedAsk.id], (current) =>
        current === undefined ? undefined : { ...current, ask: rewritten }
      );
      queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxQuery().queryKey }, (rows) =>
        rows?.map((row) => (row.id === rewritten.id ? { ...row, ...rewritten } : row))
      );
      invalidateOwnerReads(queryClient, displayedAsk);
    },
  });
  const rewriteRunning = rewriting.isPending || rewrite?.state === "running";
  // A rewrite the route itself refused stands as a failed one, with the route's reason.
  const shownRewrite =
    rewriting.isError && !rewriting.isPending
      ? {
          at: new Date().toISOString(),
          by: "person" as const,
          original: { options: [], question: displayedAsk.question },
          reason: rewriting.error.message,
          state: "failed" as const,
        }
      : rewrite;
  const original =
    showingOriginal && shownRewrite?.state === "done" ? shownRewrite.original : undefined;
  // In a decision block the agent's wording stands in place of the document's text: the block's
  // frame is marked while it does, and its content hole hides (`styles.css`). The mark is on the
  // frame, outside the editor's content, so the editor reads nothing of it.
  const showsOriginalInBlock = inBlock && original !== undefined;
  useLayoutEffect(() => {
    const frameElement = articleRef.current?.closest('[data-proof-block-type="ask"]');
    if (!showsOriginalInBlock || frameElement == null) return undefined;
    frameElement.setAttribute("data-ask-original", "");
    return () => frameElement.removeAttribute("data-ask-original");
  }, [showsOriginalInBlock]);
  const startRewrite = () => {
    if (isApproval || rewriteRunning) return;
    setShowingOriginal(false);
    rewriting.mutate();
  };
  const startAskBack = () => {
    setQuestionChoice(false);
    setAskingBack(true);
    if (isCompact) setOwnWordsOpen(true);
  };
  const stopAskingBack = () => {
    if (!askingBack) return false;
    setAskingBack(false);
    return true;
  };
  useEffect(() => {
    if (askingBack) answerFieldRef.current?.focus();
  }, [askingBack]);
  useEffect(() => {
    if (clarification.isSuccess) setAskingBack(false);
  }, [clarification.isSuccess]);
  /** The words the reader selected in the card's question or options, outside any field. */
  const selectionInCard = () => {
    const active = document.activeElement;
    if (active instanceof HTMLTextAreaElement || active instanceof HTMLInputElement) {
      if (active.type !== "radio" && active.type !== "checkbox") return null;
    }
    return selectedWordsIn(articleRef.current);
  };
  const openDefine = (picked: SelectedWords | null) => {
    if (picked === null) return false;
    setDefinition(picked);
    return true;
  };
  const changeMenuOpen = (open: boolean) => {
    if (open) setMenuSelection(selectionInCard());
    setMenuOpen(open);
  };
  const declinable = displayedAsk.state === "open" && displayedAsk.kind === "question";
  // Registered again on every commit, so the keys always reach this render's state.
  useLayoutEffect(() => {
    const element = articleRef.current;
    if (element === null || displayedAsk.state !== "open") return undefined;
    const actions: AskCardActions = {
      askBack: startAskBack,
      decline: declinable ? () => setDeclineOpen(true) : undefined,
      define: () => openDefine(selectionInCard()),
      handOn: () => setHandOnOpen(true),
      hasSelection: () => selectionInCard() !== null,
      rewrite: isApproval ? undefined : startRewrite,
      send: () => formRef.current?.requestSubmit(),
      stopAskingBack,
      toggleOriginal:
        shownRewrite?.state === "done" ? () => setShowingOriginal((shown) => !shown) : undefined,
    };
    return registerAskCard(element, actions);
  });
  // Off the Inbox, the card in hand is the one holding the focus: its keys act while it does.
  useKeymapScope(focusWithin ? "ask" : null);
  const cardInHand = () => (focusWithin ? articleRef.current : null);
  useAskCardKeys(cardInHand, askCardLeadKeys(cardInHand));
  const waitingOnPerson = askWaitingOnPerson(displayedAsk) ?? "";
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
  // Whose turn it is, named only where it is not the reader's own question: an agent's, or a
  // person's the question was handed to.
  const turnLabel: ReactNode =
    turnHolder === null ? null : handOn.pending ? (
      "Handing on…"
    ) : turnHolder !== "you" ? (
      <>
        Waiting on <AgentName actor={turnHolder} inline />
      </>
    ) : waitingOnPerson === "" || waitingOnPerson === viewer ? null : (
      `Waiting on ${waitingOnPerson}`
    );
  // Who last handed the question on, and when, beside whose turn it is: the stored pair, which a
  // hand-on back to the issue's assignee writes too.
  const handedOnBy = displayedAsk.state === "open" ? (displayedAsk.asked_of_by ?? "") : "";
  const handedOnAt = displayedAsk.asked_of_at ?? "";
  const handedOn = handedOnBy !== "" && handedOnAt !== "" && !handOn.pending;
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
          className={`block w-full rounded-lg border px-3 py-2 text-base font-normal outline-none sm:text-sm ${inputClasses(true)}`}
          data-ask-answer=""
          data-asking-back={askingBack}
          disabled={isSubmitting}
          id={answerFieldId}
          onChange={(event) => {
            setAnswerText(event.target.value);
            setQuestionChoice(false);
          }}
          onDrop={
            uploadOwner === undefined ? undefined : (event) => upload.drop(event, uploadOwner)
          }
          onKeyDown={(event) => {
            if (event.key === "Escape" && askingBack) {
              event.preventDefault();
              stopAskingBack();
              return;
            }
            submitOnModifiedEnter(event, { disabled: uploading });
          }}
          onPaste={
            uploadOwner === undefined ? undefined : (event) => upload.paste(event, uploadOwner)
          }
          placeholder={askingBack ? "Ask a question back" : answerPlaceholder}
          ref={answerFieldRef}
          rows={2}
          value={answerText}
        />
      </label>
      <DraftUploadStatus upload={upload} />
    </>
  );
  // The menu in the shortcut help's order: what the reader does with the question, the way out
  // of it, and last Define, which acts on words they select.
  const menuGroups: AskMenuItem[][] =
    displayedAsk.state !== "open"
      ? []
      : [
          [
            ...(isApproval
              ? []
              : [
                  {
                    hotkey: "r",
                    id: "rewrite",
                    label: "Rewrite",
                    refused: rewriteRunning ? "A rewrite is running" : undefined,
                    run: startRewrite,
                  },
                ]),
            askingBack
              ? { hotkey: "Esc", id: "ask-back", label: "Answer instead", run: stopAskingBack }
              : { hotkey: "b", id: "ask-back", label: "Ask back", run: startAskBack },
            {
              hotkey: "a",
              id: "hand-on",
              label: "Hand this question to…",
              run: () => setHandOnOpen(true),
            },
          ],
          declinable
            ? [
                {
                  hotkey: "n",
                  id: "decline",
                  label: "Decline to answer",
                  run: () => setDeclineOpen(true),
                },
              ]
            : [],
          [
            {
              hotkey: "d",
              id: "define",
              label: "Define selected words",
              refused: menuSelection === null ? "Select words in the question first" : undefined,
              run: () => openDefine(menuSelection),
            },
          ],
        ];
  const submitButton = changing ? (
    <button
      aria-describedby={submitHint === undefined ? undefined : submitHintId}
      className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
      disabled={!canAnswer || isSubmitting || uploading}
      title={submitHint}
      type="submit"
    >
      {mutation.isPending ? "Saving…" : "Save answer"}
    </button>
  ) : (
    <div className="inline-flex" ref={actionsRef}>
      <AskSplitButton
        action={
          askingBack
            ? clarification.isPending
              ? "Sending…"
              : "Ask back"
            : mutation.isPending
              ? "Answering…"
              : "Answer"
        }
        actionDescribedBy={submitHint === undefined ? undefined : submitHintId}
        actionRefused={
          isSubmitting || uploading || (askingBack ? trimmedAnswer === "" : !canAnswer)
        }
        actionTitle={submitHint}
        groups={menuGroups}
        menuOpen={menuOpen}
        onMenuOpenChange={changeMenuOpen}
      />
    </div>
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
        ) : null}
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

  // The provenance line's parts, drawn in the card or in a decision block's header.
  const provenanceItems = (
    <>
      <span className="flex min-w-0 items-center gap-x-1.5 whitespace-nowrap">
        {/* A person's name and the hand-on stay whole; an agent's name is the part that gives up
            width, as the asker's does. Whose turn it is and who handed the question on read as
            one phrase: "Waiting on bruno, handed on by dana 20 minutes ago". */}
        {turnLabel === null ? null : (
          <span
            className={`${turnHolder === "you" ? "shrink-0" : "min-w-0 truncate"} font-semibold ${textSecondaryOnSurface}`}
            data-testid={`turn-${displayedAsk.id}`}
          >
            {turnLabel}
            {handedOn ? "," : null}
          </span>
        )}
        {handedOn ? (
          <span className="shrink-0" data-ask-handed-on="">
            handed on by {handedOnBy} <Timestamp at={handedOnAt} />
          </span>
        ) : null}
        {turnLabel === null && !handedOn ? null : (
          <span className="shrink-0" aria-hidden="true">
            ·
          </span>
        )}
        <span className="flex min-w-0 items-center gap-x-1.5">
          {displayedAsk.author.id === "" ? (
            "asked"
          ) : (
            // The asker's button keeps its name and opens the popover; a session's live link
            // sits beside it, outside the button. `whitespace-pre` keeps the space before an
            // attribution suffix, which a flex item would drop.
            <AgentName
              actor={displayedAsk.author}
              className="flex min-w-0 items-center whitespace-pre"
              control={(name) => (
                <button
                  aria-expanded={askerOpen}
                  aria-haspopup="dialog"
                  className={`flex min-w-0 items-center gap-1 ${textMutedHoverToSecondary}`}
                  data-ask-asker=""
                  onClick={() => setAskerOpen((open) => !open)}
                  ref={askerRef}
                  type="button"
                >
                  <TruncatedText className="min-w-0" title={askerTitle}>
                    {name}
                  </TruncatedText>
                  <ChevronIcon expanded={askerOpen} />
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
      {isApproval ? null : (
        <RewriteBadge
          onRetry={startRewrite}
          onToggle={() => setShowingOriginal((shown) => !shown)}
          rewrite={shownRewrite}
          running={rewriting.isPending}
          showingOriginal={showingOriginal}
        />
      )}
      {reference === undefined ? null : (
        <span className="ml-auto shrink-0">
          <CopyRefButton route={reference} />
        </span>
      )}
    </>
  );

  return (
    <article
      aria-label={`Urgency: ${URGENCY_LABELS[urgency]}`}
      className={
        inBlock
          ? // The agent's wording takes the place of the hidden text, right under the header.
            showsOriginalInBlock
            ? "relative"
            : "relative mt-3"
          : `relative rounded-xl border-l-4 shadow-sm ${isCompact ? "px-3 pt-3 pb-3" : "px-4 pt-4 pb-4"} ${card} ${askUrgencyAccent[urgency]}`
      }
      // The one handle on a card as a whole, carrying the question the card draws: a page-level
      // key that must not fire while the reader is answering an ask (the issue page's priority
      // digits, beside this card's own options) matches the bare attribute, and the Inbox's
      // answer keys find the selected question's own card by its value (`answerHand` in
      // `Inbox.tsx`). Neither asks for a test id.
      data-ask-card={displayedAsk.id}
      data-testid={`ask-${displayedAsk.id}`}
      onBlur={(event) => {
        if (
          !(
            event.relatedTarget instanceof Node && event.currentTarget.contains(event.relatedTarget)
          )
        )
          setFocusWithin(false);
      }}
      onFocus={() => setFocusWithin(true)}
      ref={articleRef}
    >
      {/* The urgency is the card's leading rule alone (and its label for a screen reader): the
          row that opens it already names the urgency in words. */}
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
        {/* One provenance line, above the question as a message's sender is: whose turn it is
            where it is not the reader's own, who asked, when, the rewrite's badge, and the ask's
            own reference at the line's end. Above the question it stays where it is when the
            badge swaps the wording in place, so the badge is pressed back where it was pressed;
            in a decision block it stands in the block's own header, above the document's text,
            for the same reason. The names are the parts that give up width: a separator that
            wrapped left a line opening - or ending - on a bare "·". A block ask the server
            indexed with no pending author has an empty label and still says when it was asked.
            The reader's own question is waiting on them wherever it is drawn, so "Waiting on
            you" is a line they do not need and the turn is named only where it is somebody
            else's. */}
        {inBlock && provenanceSlot != null ? (
          createPortal(
            <>
              <span aria-hidden="true">·</span>
              {provenanceItems}
            </>,
            provenanceSlot
          )
        ) : inBlock && provenanceSlot === null ? null : (
          <div className={`mb-1.5 flex items-center gap-x-1.5 text-xs ${textMutedOnSurface}`}>
            {provenanceItems}
          </div>
        )}
        {askerOpen ? (
          // What a reader wants perhaps once a week, one click away behind the asker's chevron:
          // the session's own handles - its ID, its title, its tmux target - and every session
          // a person's answer and replies reach, each of which the reader can take off the ask.
          <AnchoredPanel
            anchorRef={askerRef}
            className={`z-50 w-80 space-y-3 rounded-lg border p-3 shadow-lg outline-none ${card} ${borderDefault}`}
            flip
            label={askerTitle}
            onDismiss={() => setAskerOpen(false)}
            role="dialog"
          >
            {sessionAuthor === undefined ? null : (
              <div
                className={`flex flex-wrap items-center gap-x-3 gap-y-1 text-xs ${textMutedOnSurface}`}
              >
                <CopyButton value={sessionAuthor.id} what="session ID">
                  ID
                </CopyButton>
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
              </div>
            )}
            {sessionAuthor === undefined ? null : (
              <div aria-hidden="true" className={`h-px ${menuRule}`} />
            )}
            <AskRecipients
              askId={displayedAsk.id}
              claimant={threadQuery.data?.claimant ?? null}
              followers={threadQuery.data?.followers ?? []}
              owner={displayedAsk}
              route={threadQuery.data?.route ?? null}
              routeHolder={threadQuery.data?.route_holder ?? null}
              routeStatus={threadQuery.data?.route_status ?? null}
            />
          </AnchoredPanel>
        ) : null}
        {/* The agent's wording in a decision block stands in place of the document's text,
            which the block hides while it does (`data-ask-original`); the document still holds
            the rewrite. */}
        {inBlock && original !== undefined ? (
          <div
            className={`text-[15px] leading-relaxed font-medium ${textPrimaryOnSurface} ${askQuestionProse}`}
            data-ask-question=""
          >
            <MarkdownBody markdown={original.question} source={source} />
          </div>
        ) : null}
        {/* An approval request names the document and version it asks for, as a link: the
            question under it begins "Approve", so no label repeats that it is one. */}
        {isApproval && documentLink ? (
          <p className={`mb-1 text-sm ${linkText} ${linkHoverText}`}>
            <AskApprovalLink ask={displayedAsk} />
          </p>
        ) : null}
        {inBlock ? null : (
          <div
            className={`text-[15px] leading-relaxed font-medium ${textPrimaryOnSurface} ${askQuestionProse}`}
            data-ask-question=""
          >
            <MarkdownBody
              markdown={original?.question ?? displayedAsk.question}
              source={source}
              tree={original === undefined ? displayedAsk.question_tree : undefined}
            />
          </div>
        )}
        {/* A request to approve again names the approval it follows, under the question (the
            spec's own placement), with the diff from that version behind Show diff; a first
            request carries no earlier approval, and EarlierApproval renders nothing for it. */}
        {displayedAsk.approval === undefined ? null : (
          <EarlierApproval approval={displayedAsk.approval} />
        )}
        {handOn.failed ? (
          <div className="mt-1.5">
            <QueryError
              message={handOn.error ?? "Could not hand this question on."}
              onRetry={handOn.retry}
              retrying={handOn.pending}
            />
          </div>
        ) : null}
        {/* A rewrite is an edit of the ask, which its badge already marks. */}
        {shownRewrite?.state === "done" ? null : (
          <AskEditHistory ask={displayedAsk} edits={edits} />
        )}
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
      <form
        className="mt-3 space-y-3"
        onSubmit={
          askingBack
            ? (event) => {
                event.preventDefault();
                sendClarification();
              }
            : submit
        }
        ref={trackForm}
      >
        {(displayedAsk.options ?? []).length === 0 ? null : (
          <fieldset className="min-w-0 space-y-2">
            <legend className="sr-only">Answer options</legend>
            {(displayedAsk.options ?? []).map((option) => {
              // The original wording keeps each option's label, which a rewrite never changes,
              // with the description the agent wrote, or none.
              const before = original?.options.find((one) => one.label === option.label);
              const shown =
                original === undefined
                  ? option
                  : before?.description === undefined
                    ? { label: option.label }
                    : { description: before.description, label: option.label };
              return (
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
                  option={shown}
                />
              );
            })}
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
            {ownWordsOpen ? (
              <div className="space-y-3" id={`${answerFieldId}-disclosure`}>
                {answerField}
              </div>
            ) : null}
            <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
              {questionChoice ? answerActions : submitButton}
              {ownWordsOpen ? null : (
                <button
                  aria-controls={`${answerFieldId}-disclosure`}
                  aria-expanded={ownWordsOpen}
                  className={`min-h-11 text-sm font-medium ${textMutedHoverToSecondary}`}
                  // The Inbox's Enter opens this disclosure to reach the answer box behind it,
                  // and finds the control by this handle, as it finds the box by
                  // `data-ask-answer` and an option by `data-ask-option` (`Inbox.tsx`).
                  data-ask-note-disclosure=""
                  onClick={() => {
                    // Opening it is a move into the field behind it.
                    focusNoteOnOpen.current = true;
                    setOwnWordsOpen(true);
                  }}
                  type="button"
                >
                  Add a note
                </button>
              )}
            </div>
            {submitHintNode}
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
      {answerFirst ? threadNode : null}
      {declinable ? (
        <DeclineControl
          ask={displayedAsk}
          decline={decline}
          onDeclined={onAnswered}
          onOpenChange={setDeclineOpen}
          open={declineOpen}
        />
      ) : null}
      {handOnOpen ? (
        <AskHandOnPicker
          anchorRef={actionsRef}
          assignee={ask.issue?.assignee ?? null}
          onClose={() => setHandOnOpen(false)}
          onPick={handOn.submit}
          waitingOn={waitingOnPerson}
        />
      ) : null}
      {definition === null ? null : (
        <DefinePopover
          column={definition.column}
          onClose={() => setDefinition(null)}
          place={{ ask_id: displayedAsk.id }}
          rect={definition.rect}
          words={definition.words}
        />
      )}
    </article>
  );
}
