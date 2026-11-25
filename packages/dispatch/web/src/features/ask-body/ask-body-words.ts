/**
 * The ask body's words (dispatch://INFRA-5702/spec#b-r-two-lists): every word the ask body draws,
 * and the card around it (`inbox/AskCard.tsx`), the same on every surface. Each part reads its
 * words from here, so a test, a browser spec and a part read one list, and the issue page's list
 * can take this one in.
 *
 * Each table is frozen, and so is each pair in it. A pair is the words on either side of a value
 * the part draws between them (`before`, `after`). A part adds the spaces and the marks between
 * its words and its values (` · `, `.`) where it draws them.
 *
 * Two tables are other files', and this list reads them where they are (`ASK_SHARED_WORDS`): how
 * a closed question was closed, `refs/actor.ts`'s `ASK_RESOLUTION_WORDS`, which the issue page's
 * list reads too, and each urgency's word, `inbox/ask-urgency.ts`'s `URGENCY_LABELS`.
 *
 * This file, as every file under `features/ask-body/`, imports no file under
 * `features/issue-page/` (R22).
 */
import { URGENCY_LABELS } from "../inbox/ask-urgency";
import { ASK_RESOLUTION_WORDS } from "../refs/actor";

/** The card's own words, around the body: its name for a screen reader, the box that keeps an
 *  answer the reader could not send, and the session's copy handles. */
export const ASK_CARD_WORDS = Object.freeze({
  copy_id: "ID",
  copy_id_what: "session ID",
  copy_title: "title",
  copy_title_what: "session title",
  copy_tmux_what: "tmux target",
  unsent_answer:
    "This question closed before you sent your answer, so it was not sent. Your text is here to copy.",
  /** Before the urgency's word, in the card's name: "Urgency: High". */
  urgency: "Urgency:",
} as const);

/** The body's first line and the line under it: the kind of every ask, the urgency word a
 *  caller that sets `urgencyWord` has drawn beside it, who asked, and whose turn it is. */
export const ASK_LINE_WORDS = Object.freeze({
  /** The asker of a block ask the server indexed with no author. */
  asked: "asked",
  /** The kind of every ask, at the right end of the first line: the new issue page's one word
   *  for an ask (`NODE_TYPE_NAMES.ask` in `issue-page/page-words.ts`). */
  kind: "question",
  /** The urgency words the first line draws beside the kind (R17), only for an ask so urgent it
   *  carries one: the card's notch's words for those urgencies (`URGENCY_LABELS`). An ask of
   *  urgency `low` or `med` draws none. */
  urgency: Object.freeze({
    blocking: URGENCY_LABELS.blocking,
    high: URGENCY_LABELS.high,
  } as const),
  waiting_on: "Waiting on",
  /** Whom the ask waits on, where that is the reader. */
  you: "you",
} as const);

/** An approval request's words: its mark, its document's link, and the approval it follows. */
export const ASK_APPROVAL_WORDS = Object.freeze({
  /** "Approved at version N on <date> by <who>", with no full stop at its end. */
  approved: Object.freeze({ at_version: "Approved at version", by: "by", on: "on" } as const),
  diff_failed: "Could not load versions to compare.",
  diff_loading: "Loading versions to compare…",
  hide_diff: "Hide diff",
  requested: "Approval requested",
  show_diff: "Show diff",
  /** Between the document's name and its number: "Spec, version 3". */
  version: "version",
} as const);

/** The links to where an ask stands: its block, and the text an anchored ask quotes. */
export const ASK_LINK_WORDS = Object.freeze({
  open_in_document: "Open in document",
  text_changed: "Text changed",
  view_original: "View original text",
} as const);

/** The form an open ask is answered in: its options, its box, and its actions. */
export const ASK_FORM_WORDS = Object.freeze({
  answer: "Answer",
  answer_anyway: "Answer with it anyway",
  answering: "Answering…",
  ask_back: "Ask back",
  ask_back_instead: "Ask back instead",
  cancel: "Cancel",
  changed:
    "This question changed while you were answering. Review the latest wording and confirm your answer again; your draft text is still here.",
  clarification_failed: "Could not send your clarification.",
  options: "Answer options",
  other: "Other",
  placeholder: Object.freeze({
    add_note: "Add a note (optional)",
    note: "Note (optional)",
    own_words: "Answer in your own words, or ask a question back",
    why: "Why?",
  } as const),
  /** Under Answer while the option picked needs a reason: "Add a reason to send Request
   *  changes". */
  reason_hint: "Add a reason to send",
  question_shaped: "Your answer reads as a question",
  reason: "Reason",
  reason_required: "Reason (required)",
  save_answer: "Save answer",
  saving: "Saving…",
  sending: "Sending…",
  /** What a stopped write says of the answer and the question back (`stoppedNote`), and of an
   *  answer stopped as the question closed. */
  stopped: Object.freeze({
    answer: "your answer",
    answer_done: "saved",
    closed: "The question has closed, and the record above shows how.",
    clarification: "your clarification",
    clarification_done: "sent",
  } as const),
  your_answer: "Your answer",
} as const);

/** Who an open ask waits on, and the control that hands it on. */
export const ASK_HAND_ON_WORDS = Object.freeze({
  asked_of: "Asked of",
  asked_of_nobody: "Asked of nobody",
  /** After the assignee's login among the people: "ada (the issue's assignee)". */
  assignee: "(the issue's assignee)",
  failed: "Could not hand this question on.",
  hand_to: "Hand this question to",
  handed_on_by: "handed on by",
  handing_on: "Handing on…",
  nobody: "Nobody",
} as const);

/** Decline to answer, and the dialog it opens. */
export const ASK_DECLINE_WORDS = Object.freeze({
  again: "Pressing Decline to answer again sends a new decline.",
  cancel: "Cancel",
  /** "1 character left", "12 characters left". */
  character: "character",
  characters: "characters",
  decline: "Decline to answer",
  declining: "Declining…",
  explained: "The question closes with no answer, and the agent that asked decides without one",
  failed: "Could not decline this question. Try again.",
  left: "left",
  note: "Note (optional)",
  /** What a stopped decline says of itself (`stoppedWriteNote`). */
  stopped: Object.freeze({ done: "made", what: "your decline" } as const),
} as const);

/** A settled ask's record: its answer and the earlier ones, its options, how it closed, and the
 *  question's earlier wordings. */
export const ASK_RECORD_WORDS = Object.freeze({
  answered_by: "Answered by",
  asked: "Asked",
  change_answer: "Change answer",
  changed: "Changed",
  earlier_answer: "Show 1 earlier answer",
  /** "Show 3 earlier answers". */
  earlier_answers: Object.freeze({ after: "earlier answers", before: "Show" } as const),
  edited: "Edited",
  options: "Options",
  other: "Other",
  previous_version: "Show 1 previous version",
  /** "Show 3 previous versions". */
  previous_versions: Object.freeze({ after: "previous versions", before: "Show" } as const),
  reworded_by: "Reworded by",
  /** The tick of a chosen option, for a screen reader. */
  selected: "Selected",
} as const);

/** The tables of other files this list reads where they are: how a closed question was closed,
 *  and each urgency's word. */
export const ASK_SHARED_WORDS = Object.freeze({
  resolution: ASK_RESOLUTION_WORDS,
  urgency: URGENCY_LABELS,
} as const);
