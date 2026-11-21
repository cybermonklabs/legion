import { answerSelected } from "@legion/contracts/dispatch-constants";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type SetStateAction, useEffect, useId, useRef, useState } from "react";

import { ApiError, apiErrorMessage, type WriteBounds, type WriteMethod } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import { replaceQueryData } from "../../api/query-client";
import type {
  AnswerAskInput,
  Ask,
  AskAnswer,
  AskRead,
  AskWithTrees,
  Comment,
  CreateCommentInput,
} from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { isViewer } from "../refs/actor";
import { stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { answerAskInput } from "./answer-ask";
import { setAnswerDraft as setAnswerDraftOfAsk, useAnswerDraft } from "./answer-drafts";
import { askCommentInput, postAskComment } from "./ask-comment";
import { type Draft, draftTargets, EMPTY_DRAFT, emptyDraftRefusal } from "./draft-store";
import { type AskPress, useOutstandingAnswerWrite } from "./outstanding-answers";
import { invalidateOwnerReads } from "./owner-reads";
import { isQuestionShapedAnswer } from "./question-shaped-answer";
import { useAskThreadRead } from "./useAskThreadRead";

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
}

/** A failed answer, as the card shows it. */
interface AskAnswerFailure {
  message: string;
  /** False for a refusal the same answer never gets past. */
  retryable: boolean;
}

/** Why Answer and **Answer with it anyway** refuse a draft that names a session: an answer is a
 *  decision on the ask, which carries no mentions (`AnswerAskInput`), so the words would reach
 *  the session as prose and the session itself not at all. Ask back posts a comment, which does
 *  carry them, which is why the reason says where to send it instead. */
export const ANSWER_MENTION_REFUSAL =
  "An answer cannot mention a session. Remove the mention, or send it with Ask back.";

/** Why Answer refuses a draft that is neither an option picked nor words written: the two ways
 *  an ask is answered, and the box holds neither. */
export const ANSWER_REFUSAL = "Pick an option or write an answer.";

/** One press of the answer: the ask it is of (`AskPress`) and what it sends. */
interface AnswerPress extends AskPress {
  readonly input: AnswerAskInput;
}

/** Whether a press changes an answer the server has already recorded, which its input names by
 *  that answer's time (`answerAskInput`). */
function changesAnswer({ input }: AnswerPress): boolean {
  return input.expected_answer_at !== undefined;
}

/** One press of a question back: the ask it is of (`AskPress`) and the comment it posts - the
 *  words and the sessions picked in them, frozen with the press (`askCommentInput`). */
interface ClarificationPress extends AskPress {
  readonly input: CreateCommentInput;
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
}: UseAskAnswerFormOptions) {
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
  // The reader's unsent answer - what they typed and the sessions they picked in it - which
  // outlives this form: the ask's own Inbox row can leave under it (withdrawn, resolved, or
  // answered by somebody else), and the reading column then keeps the question with this text
  // under it rather than taking both off the screen.
  const [draftAnswer, setDraftAnswer] = useAnswerDraft(ask.id);
  // The recorded answer the reader is changing. It is this form's own and never the
  // shared draft: an answer the server has recorded is not unsent text, so a change they cancel
  // or walk away from leaves nothing behind for another card to show them as one
  // (`unsentAnswer`) or for the reading column to hold the question by (`pane-selection.ts`,
  // `paneAsk`).
  // Cancelling is what puts the draft that stood before the change back in the box.
  const [changeAnswer, setChangeAnswer] = useState<Draft>(EMPTY_DRAFT);
  const answerDraft = changing ? changeAnswer : draftAnswer;
  const setAnswerDraft = changing ? setChangeAnswer : setDraftAnswer;
  const answerText = answerDraft.text;
  const replacing = useRef<AskAnswer | undefined>(undefined);
  const initialChangeConsumed = useRef(false);
  const submitGuard = useSubmitGuard();
  // The ask's thread, as every view of it on the page reads it (`useAskThreadRead`).
  const threadQuery = useAskThreadRead({
    ask,
    getAskThread,
    initialThread,
    initialThreadUpdatedAt,
  });
  const edits = threadQuery.data?.edits ?? [];
  const answers = threadQuery.data?.answers ?? [];
  // What a press does to the card goes to the press's own ask. Its callbacks run with the newest
  // render's values (`useKeyedWrite`), and a host may draw this form for one ask after another,
  // so the state they set - the answer the card shows, the form it closes, the choice it clears -
  // is set only while the form is drawn for the press's ask (`drawnFor`), and never on the ask
  // the card shows by then; the reads they refresh are the press's ask's either way. A draft a
  // press addresses to its ask is this form's own while the form is drawn for that ask, and that
  // ask's stored draft once a host has drawn the form for another (`setAnswerDraftOfAsk`), so the
  // draft a landing clears, or an upload adds to, is the press's ask's and never the one the card
  // shows.
  const drawnFor = (press: AskPress) => press.ask.id === ask.id;
  const setAnswerDraftOf = (askId: string, next: SetStateAction<Draft>) => {
    if (askId === ask.id) setAnswerDraft(next);
    else setAnswerDraftOfAsk(askId, next);
  };
  // The card says why an answer was refused, in the ask's own words, beside the draft it holds;
  // the shell's indicator shows the answer only while it is out. The row's removal from the
  // Inbox, its restore on a refusal and the hold that keeps the question where the reader
  // pressed it are the decline's too, and are written once beside the hold itself. Each press
  // carries the ask it answers: `retry()` runs the newest render's `write` and callbacks with the
  // press's own values (`useKeyedWrite`), and a host may draw this card for one ask after
  // another, so a Retry sends the stopped answer to its own ask under its key, and its landing
  // reads that ask again.
  const mutation = useOutstandingAnswerWrite({
    label: (press: AnswerPress) => `Answer to “${press.ask.question}”`,
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
      setAnswerDraftOf(press.ask.id, EMPTY_DRAFT);
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
    label: (press: ClarificationPress) => `Question back on “${press.ask.question}”`,
    method: "POST",
    // The clarification is a comment, so the sessions the reader picked ride with it and each
    // one gets the comment; the targets are frozen with the attempt, as the text is
    // (`askCommentInput`).
    write: (press: ClarificationPress, bounds) =>
      postAskComment(press.ask, press.input, bounds, createReply),
    onSettled: () => {
      submitGuard.release();
    },
    onSuccess: (_comment, press) => {
      setAnswerDraftOf(press.ask.id, EMPTY_DRAFT);
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
    const realSelected = answerSelected(current).filter((label) =>
      (recordedAsk.options ?? []).some((option) => option.label === label)
    );
    replacing.current = current;
    mutation.reset();
    setSelected(realSelected);
    setOtherSelected(realSelected.length === 0 && current.text !== null && current.text !== "");
    // `changing` is still false this render, so the recorded text goes straight to the form's
    // own state rather than through `setAnswerDraft` into the shared draft. A recorded answer
    // carries no mentions, since no answer ever could.
    setChangeAnswer({ mentions: [], text: current.text ?? "" });
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
      ? `Add a reason to send ${reasonRequiredFor}`
      : undefined;
  // The sessions this draft would reach, which an answer cannot carry: Answer and "Answer with
  // it anyway" refuse while one stands, by button and by Ctrl/Cmd+Enter alike, rather than
  // sending the mention as prose to nobody.
  const answerMentioned = draftTargets(answerDraft).length > 0;
  /** Why Answer refuses the draft as it stands, or null while it would take it. An approval
   *  whose choice demands a reason keeps its own words; the rest is a draft nothing can send. */
  const answerRefusal =
    submitHint ?? (answerMentioned ? ANSWER_MENTION_REFUSAL : canAnswer ? null : ANSWER_REFUSAL);
  /** Why Ask back refuses it: it posts what the box holds, and the box is empty. */
  const clarificationRefusal = emptyDraftRefusal(answerDraft);
  const answerPlaceholder = reasonRequired
    ? "Why?"
    : isApproval && selected.includes("Approve")
      ? "Note (optional)"
      : selected.length > 0
        ? "Add a note (optional)"
        : "Answer in your own words, or ask a question back";
  const sendAnswer = (text: string) => {
    if (answerRefusal !== null) return;
    submitGuard.guard(() => {
      const answerSelection = isApproval || hasOptions ? selected : [];
      mutation.mutate({
        ask,
        input: answerAskInput(displayedAsk, answerSelection, text, replacing.current),
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
          setAnswerDraft(EMPTY_DRAFT);
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
    // The buttons swallow their own refused clicks, but Ctrl/Cmd+Enter in the field submits the
    // form without passing through one, so the form owes the same refusal.
    if (answerRefusal !== null) return;
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
    if (clarificationRefusal !== null) return;
    submitGuard.guard(() =>
      clarification.mutate({ ask, input: askCommentInput(ask.id, answerDraft) })
    );
  };
  const completed = changing ? null : recordedAsk;
  // What the reader typed and no form will ever send: the question closed under them - withdrawn,
  // resolved, or answered by somebody else - while their answer was still a draft. The card keeps
  // it where they can read and copy it, since the box it was in has gone. Their own answer is not
  // this: it left as a draft the moment the server recorded it.
  const unsentAnswer = completed !== null && justAnswered === null ? answerText : "";

  return {
    answerDraft,
    answerFailure: mutation.error === null ? null : answerFailure(mutation),
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
    reasonRequired,
    selectRealOption,
    selected,
    sendAnswer,
    sendClarification,
    setAnswerDraft,
    setAnswerDraftOf,
    setQuestionChoice,
    startChanging,
    submitGuard,
    submit,
    toggleOther,
    threadQuery,
    trimmedAnswer,
    unsentAnswer,
  };
}
