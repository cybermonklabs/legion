import { type UseQueryResult, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type Dispatch,
  type FormEvent,
  type ReactNode,
  type SetStateAction,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import {
  ApiError,
  api,
  apiErrorMessage,
  type WriteBounds,
  type WriteMethod,
} from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import { replaceQueryData } from "../../api/query-client";
import type {
  AnswerAskInput,
  Ask,
  AskAnswer,
  AskAnswerWithTrees,
  AskEditWithTrees,
  AskRead,
  AskWithTrees,
  Comment,
  CreateCommentInput,
} from "../../api/types";
import { useTooltips } from "../../components/tooltips";
import { type SubmitGuard, useSubmitGuard } from "../../hooks/useSubmitGuard";
import { appendToDraft, type DraftUpload, useDraftUpload } from "../artifacts/ArtifactUpload";
import { ASK_FORM_WORDS } from "../ask-body/ask-body-words";
import { isViewer } from "../refs/actor";
import { type KeyedWriteResult, stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { answerAskInput } from "./answer-ask";
import { setAnswerDraft, useAnswerDraft } from "./answer-drafts";
import {
  clearPendingAskThreadInvalidation,
  hasPendingAskThreadInvalidation,
} from "./ask-thread-freshness";
import { type AskPress, useOutstandingAnswerWrite } from "./outstanding-answers";
import { askArtifactId, invalidateOwnerReads } from "./owner-reads";
import { useQuestionLabel } from "./question-label";
import { isQuestionShapedAnswer } from "./question-shaped-answer";

interface UseAskAnswerFormOptions {
  ask: AskWithTrees;
  answer: (id: string, input: AnswerAskInput, bounds: WriteBounds) => Promise<Ask>;
  createReply: (
    issueKey: string,
    input: CreateCommentInput,
    bounds: WriteBounds
  ) => Promise<Comment>;
  getAskThread: (id: string) => Promise<AskRead>;
  /** Data the Inbox response already hydrated for this card's first render. */
  initialThread?: AskRead;
  /** Timestamp of the Inbox snapshot that supplied initialThread. */
  initialThreadUpdatedAt?: number;
  /** Called once the server has recorded the reader's answer from this card. */
  onAnswered?: (id: string) => void;
  /** Opens the form for an answer the viewer may change, instead of its completion record. */
  initiallyChanging?: boolean;
  /** Whether the form reads the ask's thread (`GET /api/v1/asks/{id}`), which the card's thread,
   *  its recipients and its edit history draw, and which tells the form the ask changed or
   *  closed under the reader. Default `true`, as every card reads it; a caller that draws no
   *  thread and sends no such read turns it off, and the form then draws from the ask it is
   *  handed and the answers it records. */
  readThread?: boolean;
}

/** A failed answer, as the card shows it. */
interface AskAnswerFailure {
  message: string;
  /** False for a refusal the same answer never gets past. */
  retryable: boolean;
}

/** One press of the answer: the ask it is of (`AskPress`), what it sends, and its question as the
 *  label names it (`questionWords`). */
interface AnswerPress extends AskPress {
  readonly input: AnswerAskInput;
  readonly questionWords: string;
}

/** Whether a press changes an answer the server has already recorded, which its input names by
 *  that answer's time (`answerAskInput`). */
function changesAnswer({ input }: AnswerPress): boolean {
  return input.expected_answer_at !== undefined;
}

/** One press of a question back: the ask it is of (`AskPress`), the text it posts, and its
 *  question as the label names it (`questionWords`). */
interface ClarificationPress extends AskPress {
  readonly questionWords: string;
  readonly text: string;
}

/** An ask's form, as `useAskAnswerForm` answers it: the state the ask body draws, and the read of
 *  the ask's thread that its card draws the thread, the recipients and the edit history from.
 *  Every state of the form lives as long as the form does, so a body drawn again for it - its
 *  form after its record, or its record after its form - draws what the one before it drew. */
export interface AskAnswerForm {
  readonly answerFailure: AskAnswerFailure | null;
  readonly answerFieldId: string;
  readonly answerPlaceholder: string;
  /** Every real answer, oldest first, from the thread read. */
  readonly answers: AskAnswerWithTrees[];
  readonly answerText: string;
  readonly askChanged: boolean;
  /** Whether Answer is refused for want of something to send, read from the form as a refusal
   *  found it while the form refuses (`refusedWhileOut`). */
  readonly cannotAnswer: boolean;
  readonly canChangeAnswer: boolean;
  readonly cancelChanging: () => void;
  readonly changing: boolean;
  /** Whether the choice a question-shaped answer offers is drawn, read as `cannotAnswer` is. */
  readonly choiceShown: boolean;
  readonly clarification: KeyedWriteResult<Comment, ClarificationPress, unknown>;
  /** The settled ask whose record stands in place of the form, or null while the form is drawn. */
  readonly completed: AskWithTrees | null;
  /** Outside `NoTooltips`, whether a write of the form is out, which disables its controls. */
  readonly disabledWhileOut: boolean;
  /** The ask the form is drawn for: the thread read's once the question changed or the ask
   *  closed under the reader, else the one its caller handed. */
  readonly displayedAsk: AskWithTrees;
  readonly edits: AskEditWithTrees[];
  readonly isApproval: boolean;
  readonly mutation: KeyedWriteResult<Ask, AnswerPress, unknown>;
  /** Ask back has nothing to send, read as `cannotAnswer` is. */
  readonly nothingToAskBack: boolean;
  readonly otherSelected: boolean;
  /** Reads the names of the references the question's projection holds, for the labels of this
   *  form's writes; the card mounts it wherever it draws. */
  readonly questionReads: ReactNode;
  /** Whether the answer box stood for a reason the option picked needs at the body's last
   *  commit, so the box takes the focus as it comes to stand for one, and not again when a body
   *  is drawn again for the form. */
  readonly reasonFieldShownBefore: { current: boolean };
  readonly reasonRequired: boolean;
  /** Inside `NoTooltips`, the refusal of every control of the form, as `true` for its
   *  `aria-disabled`; undefined while the form refuses nothing or outside `NoTooltips`. */
  readonly refusedWhileOut: true | undefined;
  /** Whether the form refuses a press: a write of it is out, or, inside `NoTooltips`, the read of
   *  the thread after its landing is not yet drawn. */
  readonly refusing: boolean;
  readonly selectRealOption: (label: string) => void;
  readonly selected: string[];
  readonly sendAnswer: (text: string) => void;
  readonly sendClarification: () => void;
  readonly setAnswerText: Dispatch<SetStateAction<string>>;
  readonly setQuestionChoice: Dispatch<SetStateAction<boolean>>;
  readonly startChanging: () => void;
  readonly submit: (event: FormEvent<HTMLFormElement>) => void;
  readonly submitGuard: SubmitGuard;
  readonly submitHint: string | undefined;
  readonly toggleOther: () => void;
  readonly threadQuery: UseQueryResult<AskRead, Error>;
  readonly unsentAnswer: string;
  /** The files pasted or dropped into the answer box, each uploaded and its text added to the
   *  answer of the ask it was pasted under. */
  readonly upload: DraftUpload<string>;
}

/**
 * The refusals the same answer is refused with again however often it is sent, and what the card
 * says for each: the ask was answered or closed elsewhere, its question changed (the card reloads
 * it, so the next answer is to the new wording), or writing even its server state would leave the
 * document's live state past what the server can decode. An answer the reader stopped may have
 * been saved, and the card's Retry makes that very press again (`retryLast`). Any other failure
 * may pass on a retry.
 */
function answerFailure(answer: {
  readonly error: Error;
  readonly method: WriteMethod | undefined;
}): AskAnswerFailure {
  const stopped = stoppedNote(answer, "your answer", "saved");
  if (stopped !== undefined) {
    return { message: stopped, retryable: true };
  }
  const { error } = answer;
  if (error instanceof ApiError) {
    switch (error.code) {
      case "ASK_ANSWER_CHANGED":
        return {
          message: "The answer changed since you opened it; here is the current one.",
          retryable: false,
        };
      case "NOT_ANSWERER":
        return {
          message: "Only the person who answered can change this answer.",
          retryable: false,
        };
      case "ASK_APPROVAL_REVIEW":
        return {
          message: "An approval is a review; record a new review on the document.",
          retryable: false,
        };
      case "ASK_CLOSED":
        return {
          message: "This ask was already answered, so your answer was not saved.",
          retryable: false,
        };
      case "ASK_RESOLVED":
        return { message: "This ask was closed, so your answer was not saved.", retryable: false };
      case "ASK_EDITED":
        return {
          message: "Your answer was not saved, because the question changed.",
          retryable: false,
        };
      case "CAP_EXCEEDED":
        return {
          message: apiErrorMessage(
            error,
            "Your answer was not saved, because its document would be too large to load."
          ),
          retryable: false,
        };
    }
  }
  return { message: "Could not save your answer.", retryable: true };
}

export function useAskAnswerForm({
  answer,
  ask,
  createReply,
  getAskThread,
  initialThread,
  initialThreadUpdatedAt,
  initiallyChanging = false,
  onAnswered,
  readThread = true,
}: UseAskAnswerFormOptions): AskAnswerForm {
  const queryClient = useQueryClient();
  const viewer = useQuery(whoAmIQuery()).data?.login;
  // Each AskCard instance owns its answer field label so cards with the same
  // ask id never collide when a responsive transition briefly renders both.
  const answerFieldId = `${useId()}-answer`;
  const [selected, setSelected] = useState<string[]>([]);
  const [otherSelected, setOtherSelected] = useState(false);
  const [questionChoice, setQuestionChoice] = useState(false);
  const [justAnswered, setJustAnswered] = useState<Ask | null>(null);
  const [askChanged, setAskChanged] = useState(false);
  const [changing, setChanging] = useState(false);
  // The reader's unsent answer, which outlives this form: the ask's own Inbox row can leave
  // under it (withdrawn, resolved, or answered by somebody else), and the reading column then
  // keeps the question with this text under it rather than taking both off the screen.
  const [draftAnswer, setDraftAnswer] = useAnswerDraft(ask.id);
  // The text of a recorded answer the reader is changing. It is this form's own and never the
  // shared draft: an answer the server has recorded is not unsent text, so a change they cancel
  // or walk away from leaves nothing behind for another card to show them as one
  // (`unsentAnswer`) or for the reading column to hold the question by (`pane-selection.ts`,
  // `paneAsk`).
  // Cancelling is what puts the draft that stood before the change back in the box.
  const [changeAnswer, setChangeAnswer] = useState("");
  const answerText = changing ? changeAnswer : draftAnswer;
  const setAnswerText = changing ? setChangeAnswer : setDraftAnswer;
  const replacing = useRef<AskAnswer | undefined>(undefined);
  const initialChangeConsumed = useRef(false);
  const submitGuard = useSubmitGuard();
  // Shared by this card, its edit-version history, its collapsed disclosure, and its inline
  // thread. A thread invalidated while its Inbox snapshot was in flight bypasses that snapshot,
  // and so does every thread on an issue whose claim or route moved while it was in flight.
  const refreshInitialThread =
    initialThread !== undefined && hasPendingAskThreadInvalidation(queryClient, ask);
  // A view that reads this thread with no seed (the reading column's thread, `ThreadPane`) may
  // already have a read of it out, asked at a time this card cannot know: perhaps before the Inbox
  // read the seed comes from, so that it answers an older thread - the ask still open, say, after
  // the seed showed it answered. TanStack puts the seed into that read's query, which holds no
  // data yet, and leaves the read running, and its answer then replaces the seed with no compare
  // of their ages. So the card notes on its first render, before `useQuery` puts the seed in,
  // whether it seeds over such a read, and if it does, puts the seed over that read as every write
  // goes over the reads still out (`replaceQueryData`).
  const threadBeforeSeed = queryClient.getQueryState(["ask-thread", ask.id]);
  const seedsOverRead = useRef(
    initialThread !== undefined &&
      !refreshInitialThread &&
      threadBeforeSeed !== undefined &&
      threadBeforeSeed.data === undefined &&
      threadBeforeSeed.fetchStatus !== "idle"
  );
  const threadQuery = useQuery<AskRead, Error>({
    enabled: readThread,
    initialData: refreshInitialThread ? undefined : initialThread,
    initialDataUpdatedAt: refreshInitialThread ? undefined : initialThreadUpdatedAt,
    queryKey: ["ask-thread", ask.id],
    queryFn: () => getAskThread(ask.id),
    staleTime: Number.POSITIVE_INFINITY,
  });
  // In a layout effect, so that no answer from the network lands between the commit that drew the
  // seed and this write. Cancelling the read puts its query back as it stood before that read,
  // with no data, so the seed is written after the cancel; and with a read cancelled,
  // `replaceQueryData` asks the thread again, so the next answer is of a read asked after the seed.
  useLayoutEffect(() => {
    if (!seedsOverRead.current) return;
    seedsOverRead.current = false;
    replaceQueryData<AskRead>(
      queryClient,
      ["ask-thread", ask.id],
      (current) => current ?? initialThread
    );
  }, [ask.id, initialThread, queryClient]);
  useEffect(() => {
    if (refreshInitialThread && threadQuery.data !== undefined) {
      clearPendingAskThreadInvalidation(queryClient, ask.id);
    }
  }, [ask.id, queryClient, refreshInitialThread, threadQuery.data]);
  const edits = threadQuery.data?.edits ?? [];
  const answers = threadQuery.data?.answers ?? [];
  // What a press does to the card goes to the press's own ask. Its callbacks run with the newest
  // render's values (`useKeyedWrite`), and a host may draw this form for one ask after another,
  // so the state they set - the answer the card shows, the form it closes, the choice it clears -
  // is set only while the form is drawn for the press's ask (`drawnFor`), and never on the ask
  // the card shows by then; the reads they refresh are the press's ask's either way. Text a press
  // addresses to its ask is this form's own while the form is drawn for that ask, and that ask's
  // draft once a host has drawn the form for another (`setAnswerDraft`), so the text a landing
  // clears, or an upload adds, is the press's ask's and never the one the card shows.
  const drawnFor = (press: AskPress) => press.ask.id === ask.id;
  const setAnswerTextOf = (askId: string, next: SetStateAction<string>) => {
    if (askId === ask.id) setAnswerText(next);
    else setAnswerDraft(askId, next);
  };
  // The question as the labels of this form's two writes read it: its plain-text projection,
  // the words the card draws (`useQuestionLabel`). A label is its press's ask's, so each press
  // carries the line as it reads at the press, while the form is drawn for that ask: a Retry
  // made once a host has drawn the form for the next one names the question it answers.
  const question = useQuestionLabel(ask);
  // The card says why an answer was refused, in the ask's own words, beside the draft it holds;
  // the shell's indicator shows the answer only while it is out. The row's removal from the
  // Inbox, its restore on a refusal and the hold that keeps the question where the reader
  // pressed it are the decline's too, and are written once beside the hold itself. Each press
  // carries the ask it answers: `retry()` runs the newest render's `write` and callbacks with the
  // press's own values (`useKeyedWrite`), and a host may draw this card for one ask after
  // another, so a Retry sends the stopped answer to its own ask under its key, and its landing
  // reads that ask again.
  const mutation = useOutstandingAnswerWrite({
    // A write's label is one line the Writes indicator reads as text, so it holds the question's
    // plain-text projection: the reading list's `write-label` (`DISPATCH_READING_SURFACES`).
    label: (press: AnswerPress) => `Answer to “${press.questionWords}”`,
    method: "POST",
    write: (press: AnswerPress, bounds) => answer(press.ask.id, press.input, bounds),
    // A change to an answer the server has already recorded leaves the Inbox alone: the ask has
    // no open row there to take out.
    removesRow: (press) => !changesAnswer(press),
    onError: (error, press) => {
      if (error instanceof ApiError && error.code === "ASK_EDITED") {
        if (drawnFor(press)) {
          setAskChanged(true);
          setSelected([]);
          setOtherSelected(false);
        }
        void queryClient.invalidateQueries({ queryKey: ["ask-thread", press.ask.id] });
      }
      if (
        error instanceof ApiError &&
        (error.code === "ASK_ANSWER_CHANGED" ||
          error.code === "NOT_ANSWERER" ||
          error.code === "ASK_APPROVAL_REVIEW")
      ) {
        // Each refusal meets the same change again, so the form it was sent from closes and the
        // card shows the thread's current answer, not one this card recorded earlier.
        void queryClient.invalidateQueries({ queryKey: ["ask-thread", press.ask.id] });
        if (drawnFor(press)) {
          replacing.current = undefined;
          setJustAnswered(null);
          setChanging(false);
        }
      }
    },
    onSettled: () => {
      submitGuard.release();
    },
    onSuccess: (updatedAsk, press) => {
      const changed = changesAnswer(press);
      if (drawnFor(press)) {
        replacing.current = undefined;
        setChanging(false);
        // Answered either way; with no answer to give, the card reads the ask afresh instead of
        // showing the one Dispatch would have handed back.
        setJustAnswered(updatedAsk);
      }
      // The text is the recorded answer now, not an unsent draft: a later card for this ask
      // starts empty, and nothing offers to copy what the server already took.
      setAnswerTextOf(press.ask.id, "");
      if (changed || updatedAsk === null) {
        if (updatedAsk?.answer != null) {
          const changedAnswer = updatedAsk.answer;
          // Over any read of the thread still out, which was asked before the change and would
          // put back the answer it replaced (`replaceQueryData`).
          replaceQueryData<AskRead>(queryClient, ["ask-thread", press.ask.id], (current) =>
            current === undefined
              ? undefined
              : {
                  ...current,
                  answers: [...(current.answers ?? []), changedAnswer],
                  ask: updatedAsk,
                }
          );
        }
        void queryClient.invalidateQueries({ queryKey: ["ask-thread", press.ask.id] });
        void queryClient.invalidateQueries({ queryKey: ["me", "answers"] });
      }
      if (!changed) onAnswered?.(press.ask.id);
      invalidateOwnerReads(queryClient, press.ask);
    },
  });
  // A question back carries its ask as an answer does, for the same reason.
  const clarification = useKeyedWrite({
    inline: true,
    // The same line of the Writes indicator: `write-label` again (see above).
    label: (press: ClarificationPress) => `Question back on “${press.questionWords}”`,
    method: "POST",
    write: (press: ClarificationPress, bounds) => {
      if (press.ask.issue_key === null) {
        return api.createArtifactComment(
          askArtifactId(press.ask),
          { ask_id: press.ask.id, body: press.text },
          bounds
        );
      }
      return createReply(press.ask.issue_key, { ask_id: press.ask.id, body: press.text }, bounds);
    },
    onSettled: () => {
      submitGuard.release();
    },
    onSuccess: (_comment, press) => {
      setAnswerTextOf(press.ask.id, "");
      if (drawnFor(press)) setQuestionChoice(false);
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", press.ask.id] });
      // A clarification hands the turn to the asker: every surface that renders this ask from
      // its owner's list (a decision block reads `["asks", key]`) must see the new `waiting_on`,
      // not just the Inbox.
      invalidateOwnerReads(queryClient, press.ask);
    },
  });

  // The thread read is the truth once the question changed under the draft, and once the ask
  // was answered or resolved elsewhere while this card still holds its open row (an Inbox row
  // kept in place while the reader is on it): the card then shows the recorded outcome.
  const threadAsk = threadQuery.data?.ask;
  const displayedAsk =
    threadAsk !== undefined && (askChanged || threadAsk.state !== "open") ? threadAsk : ask;
  const recordedAsk = justAnswered ?? (displayedAsk.state === "open" ? null : displayedAsk);
  const canChangeAnswer =
    recordedAsk !== null &&
    recordedAsk.kind !== "approval" &&
    recordedAsk.state === "answered" &&
    recordedAsk.answer !== null &&
    isViewer({ id: recordedAsk.answer.user, kind: "user" }, viewer);
  const startChanging = () => {
    if (!canChangeAnswer || recordedAsk?.answer === null || recordedAsk === null) return;
    const current = recordedAsk.answer;
    const realSelected = (current.selected ?? []).filter((label) =>
      (recordedAsk.options ?? []).some((option) => option.label === label)
    );
    replacing.current = current;
    mutation.reset();
    setSelected(realSelected);
    setOtherSelected(realSelected.length === 0 && current.text !== null && current.text !== "");
    // `changing` is still false this render, so the recorded text goes straight to the form's
    // own state rather than through `setAnswerText` into the shared draft.
    setChangeAnswer(current.text ?? "");
    setQuestionChoice(false);
    setChanging(true);
  };
  // Cancelling leaves the recorded answer where it was recorded: `answerText` falls back to the
  // draft that stood before the change, which is empty on a card the reader only read.
  const cancelChanging = () => {
    replacing.current = undefined;
    mutation.reset();
    setChanging(false);
  };
  // No dependency list on purpose: it runs after every render until the viewer and the recorded
  // answer have both resolved, then once (the ref is the guard), so `mode="change"` opens the form
  // as soon as the card knows the viewer may change the answer.
  useEffect(() => {
    if (!initiallyChanging || initialChangeConsumed.current) return;
    if (viewer === undefined || recordedAsk === null) return;
    initialChangeConsumed.current = true;
    if (canChangeAnswer) startChanging();
  });
  const hasOptions = (displayedAsk.options ?? []).length > 0;
  const isApproval = displayedAsk.kind === "approval";
  const isSubmitting = mutation.isPending || clarification.isPending;
  const trimmedAnswer = answerText.trim();
  // The server rejects Request changes without text; the form says so instead of leaving the
  // submit button silently dead.
  const requiresReason = (label: string): boolean => isApproval && label === "Request changes";
  const reasonRequiredFor = selected.find(requiresReason);
  const reasonRequired = reasonRequiredFor !== undefined;
  // The four submit rules: an approval needs a choice (and a reason when the choice demands
  // one); free text needs text; "Other" needs text; options need a choice or a question typed.
  function canSubmitAnswer(): boolean {
    if (isApproval) {
      return selected.length > 0 && (!reasonRequired || trimmedAnswer !== "");
    }
    if (!hasOptions) {
      return trimmedAnswer !== "";
    }
    if (otherSelected) {
      return trimmedAnswer !== "";
    }
    return selected.length > 0 || isQuestionShapedAnswer(trimmedAnswer);
  }
  const canAnswer = canSubmitAnswer();
  const submitHint =
    reasonRequired && trimmedAnswer === ""
      ? `${ASK_FORM_WORDS.reason_hint} ${reasonRequiredFor}`
      : undefined;
  const answerPlaceholder = reasonRequired
    ? ASK_FORM_WORDS.placeholder.why
    : isApproval && selected.includes("Approve")
      ? ASK_FORM_WORDS.placeholder.note
      : selected.length > 0
        ? ASK_FORM_WORDS.placeholder.add_note
        : ASK_FORM_WORDS.placeholder.own_words;
  const sendAnswer = (text: string) => {
    submitGuard.guard(() => {
      const answerSelection = isApproval || hasOptions ? selected : [];
      mutation.mutate({
        ask,
        input: answerAskInput(displayedAsk, answerSelection, text, replacing.current),
        questionWords: question.line(),
      });
    });
  };

  const selectRealOption = (label: string) => {
    setQuestionChoice(false);
    if (displayedAsk.multiple) {
      setSelected((current) =>
        current.includes(label)
          ? current.filter((currentLabel) => currentLabel !== label)
          : [...current, label]
      );
      return;
    }
    setSelected([label]);
    setOtherSelected(false);
  };

  const toggleOther = () => {
    setQuestionChoice(false);
    if (displayedAsk.multiple) {
      setOtherSelected((current) => {
        const next = !current;
        if (!next) {
          setAnswerText("");
        }
        return next;
      });
      return;
    }
    setSelected([]);
    setOtherSelected(true);
  };

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!canAnswer) return;
    if (
      !isApproval &&
      !otherSelected &&
      selected.length === 0 &&
      isQuestionShapedAnswer(trimmedAnswer)
    ) {
      setQuestionChoice(true);
      return;
    }
    sendAnswer(answerText);
  };
  const sendClarification = () => {
    const text = answerText.trim();
    if (text === "") return;
    submitGuard.guard(() => clarification.mutate({ ask, questionWords: question.line(), text }));
  };
  const completed = changing ? null : recordedAsk;
  // What the reader typed and no form will ever send: the question closed under them - withdrawn,
  // resolved, or answered by somebody else - while their answer was still a draft. The card keeps
  // it where they can read and copy it, since the box it was in has gone. Their own answer is not
  // this: it left as a draft the moment the server recorded it.
  const unsentAnswer = completed !== null && justAnswered === null ? answerText : "";
  const reasonFieldShownBefore = useRef(false);
  // A file's text joins the answer of the ask it was pasted or dropped under, which its press
  // carries (`useDraftUpload`, `setAnswerTextOf`); only text added to the field on screen puts its
  // question choice away.
  const upload = useDraftUpload((text: string, askId: string) => {
    setAnswerTextOf(askId, (current) => appendToDraft(current, text));
    if (askId === ask.id) setQuestionChoice(false);
  });
  const tooltips = useTooltips();
  // Inside `NoTooltips` the form refuses itself from the press of an answer or a clarification
  // until that write is done with: while it is out, and, once it lands, until a read of the ask's
  // thread newer than the landing is drawn (`awaitingRead`). Until then the form on the page is
  // stale: after an answer that landed with no body to show, its options are still picked, and
  // Answer would send a second answer. A landing that starts no read of the thread ends the
  // refusal as it lands (a first answer that comes back with its body, whose record replaces the
  // form at once), and so does a read that fails, so the form never stays refused. Each control it
  // refuses - the answer box, Answer (Save answer), Ask back, Ask back instead, Answer with it
  // anyway, the Cancel of a change, and the options with Other - is `aria-disabled` in place of
  // `disabled`: the box is `readOnly`, and the options are refused by their group. `disabled`
  // drops the focus to the body in a browser; this way the control the reader pressed keeps it.
  // A press of any of them does nothing then, and neither does Ctrl+Enter, a paste or a drop in
  // the box, or the form's submit. Each is drawn from the form as it stood when the refusal began
  // (`held`): the landing empties the box and closes the choice a question-shaped answer offers,
  // and the control that would turn `disabled` or leave the page with that - Ask back, its text
  // gone, or Ask back instead - stays on the page, refused. So the ask body's hand-off of the
  // focus moves none while the form refuses, and nothing holding the focus leaves. The one move of
  // the focus comes as the refusal ends: the form's hand-off, or Ask back instead's, as it leaves
  // the page, or the body's as Ask back turns `disabled`; a focus the reader moved out of the form
  // meanwhile stays where they put it. Any other reason a control cannot be pressed - nothing
  // typed before the press, an upload still out - still disables it, on both sides. Outside
  // `NoTooltips` each is `disabled` while the write is out and no longer, as on `main`.
  const holdsFocus = !tooltips;
  // The newest write of the form that has landed, by when it was sent: 0 while none has.
  const landedWrite = Math.max(
    mutation.isSuccess ? mutation.submittedAt : 0,
    clarification.isSuccess ? clarification.submittedAt : 0
  );
  // The landing is recorded while rendering, React's pattern for state derived from a change: an
  // effect would let one render between the landing and the record reach the screen, and a press
  // then would be taken. `readAfter` is when the thread read on the page at the landing was made;
  // the form waits for a newer one while the thread is being read. Outside `NoTooltips` nothing
  // waits, so nothing is recorded and the form renders as on `main`.
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
  // What the refusing controls are drawn from: the press as the refusal found it while it
  // refuses, else the form now.
  const shown = held ?? formNow;

  return {
    answerFailure: mutation.error === null ? null : answerFailure(mutation),
    answerFieldId,
    answerPlaceholder,
    answers,
    answerText,
    askChanged,
    cannotAnswer: shown.cannotAnswer,
    canChangeAnswer,
    cancelChanging,
    changing,
    choiceShown: shown.choice,
    clarification,
    completed,
    disabledWhileOut,
    displayedAsk,
    edits,
    isApproval,
    mutation,
    nothingToAskBack: shown.nothingToAskBack,
    otherSelected,
    questionReads: question.reads,
    reasonFieldShownBefore,
    reasonRequired,
    refusedWhileOut,
    refusing,
    selectRealOption,
    selected,
    sendAnswer,
    sendClarification,
    setAnswerText,
    setQuestionChoice,
    startChanging,
    submitGuard,
    submit,
    submitHint,
    toggleOther,
    threadQuery,
    unsentAnswer,
    upload,
  };
}
