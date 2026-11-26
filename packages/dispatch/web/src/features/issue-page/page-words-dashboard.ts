/**
 * Dashboard's words (dispatch://INFRA-5702/spec#b-r-words). `page-words-parts.ts` joins them into
 * `page-words.ts`.
 *
 * - `add` holds Dashboard's new keys for the tables of `page-words.ts`. Key them by the table's
 *   name, and nest them as the table is: `add: { CONTROL_LABELS: { load_older: "Load older" } }`.
 * - A table of Dashboard's own is a named export of this file. Make it with `freeze` from
 *   `./page-structure`. List it in `unasked` if the page draws it on load, or in `answers` if
 *   the page draws it only after an act of the reader. `page-words.ts` exports it again.
 * - Put a new key for a table of this file in this file, from any pull request.
 * - Import nothing from `page-words.ts`, not even a type. Import only the type `PartWords` from
 *   `./page-words-parts`, values from `./page-structure`, and the two lists below. A value import
 *   from `page-words.ts` or `./page-words-parts` makes an import cycle.
 *
 * This file takes in the two lists that Dashboard draws an ask and a decision with: the ask
 * body's (`ask-body/ask-body-words.ts`) and the decision card's
 * (`decision-card/decision-card-words.ts`), each the same on every surface (R22). Neither imports
 * a file of this directory. Each of their words is in `unasked` or in `answers`, so
 * `DECLARED_WORDS` holds every word of the three lists (R13).
 */
import {
  ASK_APPROVAL_WORDS,
  ASK_CARD_WORDS,
  ASK_DECLINE_WORDS,
  ASK_FORM_WORDS,
  ASK_HAND_ON_WORDS,
  ASK_LINE_WORDS,
  ASK_LINK_WORDS,
  ASK_RECORD_WORDS,
  ASK_SHARED_WORDS,
} from "../ask-body/ask-body-words";
import {
  DECISION_ACT_WORDS,
  DECISION_KIND_WORDS,
  DECISION_LINE_WORDS,
  DECISION_LINK_WORDS,
  DECISION_STATE_WORDS,
} from "../decision-card/decision-card-words";
import { freeze } from "./page-structure";
import type { PartWords } from "./page-words-parts";

/** Dashboard's own words: its name, the names of its four parts, the control under what waits,
 *  and the four tiles with the control under a tile's list. */
export const DASHBOARD_WORDS = freeze({
  /** The tab on today's page and the preset on the new page. */
  name: "Dashboard",
  /** The four parts, from the top: each is a region of that name. */
  parts: {
    agents: "Agents at work",
    phase: "Phase",
    progress: "Progress",
    waiting: "Waiting on you",
  },
  /** Under the five items drawn: the control that draws the next five, with the number left. */
  next_five: "Show the next five",
  /** After the number of items or entries that a control has not drawn yet: "4 left". */
  left: "left",
  /** The four tiles, each a number with a bar of that number against its whole (R6). */
  tiles: {
    children: "Children done",
    met: "Statements met",
    passed: "Checks passed",
    questions: "Questions waiting on others",
  },
  /** Under the 20 entries a tile's list draws: the control that draws the next 20. */
  next_twenty: "Show the next 20",
  /** A thread item's link to the whole thread at its own address. */
  thread_link: "Open the whole thread",
} as const);

/** The phase track's words (R3): the eight steps in the track's order, and the words a phase
 *  draws beside the track. These are the page's one list of them, which INFRA-4870's Legion
 *  block in Details reads too. */
export const PHASE_WORDS = freeze({
  steps: {
    admitted: "Admitted",
    planning: "Planning",
    implementing: "Implementing",
    testing: "Testing",
    reviewing: "Reviewing",
    accepting: "Accepting",
    retro: "Retro",
    merging: "Merging",
  },
  /** `awaiting_person`, which marks Admitted. */
  awaiting_person: "Waiting on a person",
  /** `integrating`, which marks Merging. */
  integrating: "Integrating",
  /** `held`, which marks no step. */
  held: "Held",
  /** A phase that none of the others names. */
  another: "In another phase",
} as const);

/** The words of a role's claim state (R7), each once: `features/dashboard/role-words.ts` says
 *  which claim states each word stands for. A `retired` claim has no word, because the agents at
 *  work do not list it. */
export const ROLE_STATE_WORDS = freeze({
  another: "in another state",
  failed: "failed",
  idle: "idle",
  in_turn: "in a turn",
  paused: "paused",
  starting: "starting",
} as const);

// The words of the two lists that answer the reader's own act, as `ANSWER_LINES` of
// `page-words.ts` holds the page's own: a write of theirs still out, versions they asked to compare
// still being read, a write or a read that failed, a write that stopped on its way, and a question
// that changed or closed under their answer. The rest of each table is what its part draws
// wherever it is drawn.
const { unsent_answer, ...cardWords } = ASK_CARD_WORDS;
const { diff_failed, diff_loading, ...approvalWords } = ASK_APPROVAL_WORDS;
const {
  answering,
  changed,
  clarification_failed,
  saving,
  sending,
  stopped: answerStopped,
  ...formWords
} = ASK_FORM_WORDS;
const { failed: handOnFailed, handing_on, ...handOnWords } = ASK_HAND_ON_WORDS;
const {
  again: declineAgain,
  declining,
  failed: declineFailed,
  stopped: declineStopped,
  ...declineWords
} = ASK_DECLINE_WORDS;
const { stopped: decisionStopped, ...actWords } = DECISION_ACT_WORDS;

/**
 * The ask body's words that it draws wherever it draws the part that holds them, which is on
 * load: every word of `ask-body/ask-body-words.ts` but `ASK_BODY_ANSWERS`. The list's urgency
 * words are read as a copy, since the table is `inbox/ask-urgency.ts`'s own. Its words for how a
 * question was closed (`ASK_SHARED_WORDS.resolution`) are not taken in again: they are
 * `refs/actor.ts`'s `ASK_RESOLUTION_WORDS`, which `NODE_STATE_WORDS.ask` holds already, and one
 * of them, "Retracted", is a check state's word too, which only that table may say.
 */
export const ASK_BODY_WORDS = freeze({
  approval: approvalWords,
  card: cardWords,
  decline: declineWords,
  form: formWords,
  hand_on: handOnWords,
  line: ASK_LINE_WORDS,
  link: ASK_LINK_WORDS,
  record: ASK_RECORD_WORDS,
  urgency: { ...ASK_SHARED_WORDS.urgency },
} as const);

/** The ask body's words that answer the reader's own act, and that it never draws on load. */
export const ASK_BODY_ANSWERS = freeze({
  approval: { diff_failed, diff_loading },
  card: { unsent_answer },
  decline: { again: declineAgain, declining, failed: declineFailed, stopped: declineStopped },
  form: { answering, changed, clarification_failed, saving, sending, stopped: answerStopped },
  hand_on: { failed: handOnFailed, handing_on },
} as const);

/** The decision card's words that it draws wherever it draws the part that holds them: every
 *  word of `decision-card/decision-card-words.ts` but `DECISION_CARD_ANSWERS`. */
export const DECISION_CARD_WORDS = freeze({
  act: actWords,
  kind: DECISION_KIND_WORDS,
  line: DECISION_LINE_WORDS,
  link: DECISION_LINK_WORDS,
  state: DECISION_STATE_WORDS,
} as const);

/** The decision card's words for a Keep or a Change that stopped on its way, which it says only
 *  after the reader's act. */
export const DECISION_CARD_ANSWERS = freeze({ stopped: decisionStopped } as const);

// The lists are typed as the join reads them (`PartWords`): two parts with tables of their own
// give `WORD_FILES.flatMap` two element types it cannot join, so each holds `unknown`.
const UNASKED: readonly unknown[] = [
  DASHBOARD_WORDS,
  PHASE_WORDS,
  ROLE_STATE_WORDS,
  ASK_BODY_WORDS,
  DECISION_CARD_WORDS,
];
const ANSWERS: readonly unknown[] = [ASK_BODY_ANSWERS, DECISION_CARD_ANSWERS];

export default {
  add: {
    EMPTY_STATE_LINES: {
      /** What waits on the reader, where nothing does (R4). */
      dashboard: "Nothing waits on you.",
    },
  },
  unasked: UNASKED,
  answers: ANSWERS,
} as const satisfies PartWords;
