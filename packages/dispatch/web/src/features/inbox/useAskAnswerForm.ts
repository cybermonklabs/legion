import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type FormEvent,
  type SetStateAction,
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";

import {
  ApiError,
  api,
  apiErrorMessage,
  isSpecFormRefusal,
  type WriteBounds,
  type WriteMethod,
} from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
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
import {
  approvalRefusalKey,
  type FormRefusalPress,
  pressOn,
  setFormRefusal,
  takeFormRefusalFocus,
  takeFormRefusalReason,
  useFormRefusal,
  useRefusalSurface,
} from "../doc/form-refusals";
import { isViewer } from "../refs/actor";
import { stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { answerAskInput } from "./answer-ask";
import { setAnswerDraft, useAnswerDraft } from "./answer-drafts";
import {
  clearPendingAskThreadInvalidation,
  hasPendingAskThreadInvalidation,
} from "./ask-thread-freshness";
import { type AskPress, useOutstandingAnswerWrite } from "./outstanding-answers";
import { askArtifactId, invalidateOwnerReads } from "./owner-reads";
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
}

/** A failed answer, as the card shows it. */
interface AskAnswerFailure {
  message: string;
  /** False for a refusal the same answer never gets past. */
  retryable: boolean;
}

/** The last line of a form refusal on the card, once it has picked Request changes and filled
 *  the reason with what Dispatch wrote for the author. */
const CARD_FORM_NEXT_STEP = "To send these findings to the author, choose Answer.";

/**
 * The answer field after a refusal of the form (plan decision 14): Dispatch's reason for the
 * author, then one empty line and the note that the person typed, as it was. A field that holds no
 * note takes the reason alone, and one that already starts with the reason keeps its text.
 */
function withRefusalReason(reason: string, note: string): string {
  if (note.startsWith(reason)) return note;
  return note.trim() === "" ? reason : `${reason}\n\n${note}`;
}

/** One press of the answer: the ask it is of (`AskPress`) and what it sends, with the press a
 *  form refusal of it answers (`form-refusals.ts`): this card's surface and the version. */
interface AnswerPress extends AskPress {
  readonly input: AnswerAskInput;
  readonly refusal: FormRefusalPress;
}

/** Whether a press changes an answer the server has already recorded, which its input names by
 *  that answer's time (`answerAskInput`). */
function changesAnswer({ input }: AnswerPress): boolean {
  return input.expected_answer_at !== undefined;
}

/** One press of a question back: the ask it is of (`AskPress`) and the text it posts. */
interface ClarificationPress extends AskPress {
  readonly text: string;
}

/**
 * The refusals the same answer is refused with again however often it is sent, and what the card
 * says for each: the ask was answered or closed elsewhere, its question changed (the card reloads
 * it, so the next answer is to the new wording), writing even its server state would leave the
 * document's live state past what the server can decode, or an approval of a document whose text
 * breaks the form of The Spec. For that one the card shows Dispatch's own words, which name each
 * text, but its own last line: Dispatch's names Request changes, which the card has already
 * picked, so the card's names the press that is left (`CARD_FORM_NEXT_STEP`). An answer the
 * reader stopped may have been saved, and the card's Retry makes that very press again
 * (`retryLast`). Any other failure may pass on a retry.
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
      case "SPEC_FORM":
        return {
          message: [...error.message.split("\n").slice(0, -1), CARD_FORM_NEXT_STEP].join("\n"),
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
  // A refusal of Approve for the form of The Spec belongs to the request, not to this card
  // (`doc/form-refusals.ts`): its document's header and each card of the request read it under one
  // key, and a host that draws the card again while the answer is out reads it back. It holds the
  // press it answers, with this card and the version, which `sendAnswer` sends with the answer
  // (`AnswerPress`), so the focus goes back to this card and to no other place of the request.
  const formRefusalKey =
    ask.approval === undefined ? `ask:${ask.id}` : approvalRefusalKey(ask.approval.artifact_id);
  const refusalSurface = useRefusalSurface();
  const takeRefusalFocus = useCallback(
    (root: Element | null) => takeFormRefusalFocus(formRefusalKey, refusalSurface, root),
    [formRefusalKey, refusalSurface]
  );
  const initialChangeConsumed = useRef(false);
  const submitGuard = useSubmitGuard();
  // Shared by this card, its edit-version history, its collapsed disclosure, and its inline
  // thread. A thread invalidated while its Inbox snapshot was in flight bypasses that snapshot,
  // and so does every thread on an issue whose claim or route moved while it was in flight.
  const refreshInitialThread =
    initialThread !== undefined && hasPendingAskThreadInvalidation(queryClient, ask);
  const threadQuery = useQuery<AskRead, Error>({
    initialData: refreshInitialThread ? undefined : initialThread,
    initialDataUpdatedAt: refreshInitialThread ? undefined : initialThreadUpdatedAt,
    queryKey: ["ask-thread", ask.id],
    queryFn: () => getAskThread(ask.id),
    staleTime: Number.POSITIVE_INFINITY,
  });
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
      if (isSpecFormRefusal(error)) {
        // Dispatch refused the approval and wrote the reason a request for changes sends to the
        // author: the card picks Request changes with that reason (below, from the kept
        // refusal), and one more press sends it. The reason goes in the request's draft before
        // the refusal shows, so Answer is ready for the focus that the press owes. Both go to
        // the press's own ask (`setAnswerTextOf`, and its request's key).
        const reason = error.changesReason ?? "";
        setAnswerTextOf(press.ask.id, (note) => withRefusalReason(reason, note));
        const key =
          press.ask.approval === undefined
            ? `ask:${press.ask.id}`
            : approvalRefusalKey(press.ask.approval.artifact_id);
        setFormRefusal(key, { error, press: press.refusal });
      }
    },
    onSettled: () => {
      submitGuard.release();
    },
    onSuccess: (updatedAsk, press) => {
      const key =
        press.ask.approval === undefined
          ? `ask:${press.ask.id}`
          : approvalRefusalKey(press.ask.approval.artifact_id);
      setFormRefusal(key, undefined);
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
          queryClient.setQueryData<AskRead>(["ask-thread", press.ask.id], (current) =>
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
  const formRefusal = useFormRefusal(formRefusalKey, displayedAsk.approval?.version);
  // The kept refusal picks Request changes, in this card and in a card drawn after it: React's
  // pattern for state derived from a change, so the option is never unpicked for a frame.
  const [pickedFor, setPickedFor] = useState<ApiError | undefined>(undefined);
  if (formRefusal !== pickedFor) {
    setPickedFor(formRefusal);
    if (formRefusal !== undefined) setSelected(["Request changes"]);
  }
  // The kept refusal puts its reason before the note in the request's draft, once for each
  // refusal, also when the press was in the document's header or in another card.
  useEffect(() => {
    if (formRefusal === undefined || !takeFormRefusalReason(formRefusalKey)) return;
    const reason = formRefusal.changesReason ?? "";
    setAnswerText((note) => withRefusalReason(reason, note));
  }, [formRefusal, formRefusalKey, setAnswerText]);
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
      ? `Add a reason to send ${reasonRequiredFor}`
      : undefined;
  const answerPlaceholder = reasonRequired
    ? "Why?"
    : isApproval && selected.includes("Approve")
      ? "Note (optional)"
      : selected.length > 0
        ? "Add a note (optional)"
        : "Answer in your own words, or ask a question back";
  const sendAnswer = (text: string) => {
    submitGuard.guard(() => {
      // Each press is answered on its own: the refusal of the last one goes with it.
      setFormRefusal(formRefusalKey, undefined);
      const answerSelection = isApproval || hasOptions ? selected : [];
      mutation.mutate({
        ask,
        input: answerAskInput(displayedAsk, answerSelection, text, replacing.current),
        refusal: pressOn(refusalSurface, displayedAsk.approval?.version),
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
    submitGuard.guard(() => clarification.mutate({ ask, text }));
  };
  const completed = changing ? null : recordedAsk;
  // What the reader typed and no form will ever send: the question closed under them - withdrawn,
  // resolved, or answered by somebody else - while their answer was still a draft. The card keeps
  // it where they can read and copy it, since the box it was in has gone. Their own answer is not
  // this: it left as a draft the moment the server recorded it.
  const unsentAnswer = completed !== null && justAnswered === null ? answerText : "";

  return {
    answerFailure:
      mutation.error !== null
        ? answerFailure(mutation)
        : formRefusal === undefined
          ? null
          : answerFailure({ error: formRefusal, method: "POST" }),
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
    /** Dispatch's refusal of this card's Approve for the form of The Spec, kept for the ask. */
    formRefusal: formRefusal ?? null,
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
    setAnswerTextOf,
    setQuestionChoice,
    startChanging,
    submitGuard,
    submit,
    submitHint,
    /** Whether the press that the kept refusal answers gives its focus to this card, whose form
     *  is the root given: true once, and only for this card or a card drawn in its place. */
    takeFormRefusalFocus: takeRefusalFocus,
    toggleOther,
    threadQuery,
    trimmedAnswer,
    unsentAnswer,
  };
}
