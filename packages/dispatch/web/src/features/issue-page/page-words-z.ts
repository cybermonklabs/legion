/**
 * Part Z's words for the new issue page. `page-words-parts.ts` joins them into `page-words.ts`.
 *
 * - `add` holds the part's new keys for the tables of `page-words.ts`. Key them by the table's
 *   name, and nest them as the table is: `add: { CONTROL_LABELS: { load_older: "Load older" } }`.
 * - A table of the part's own is a named export of this file. Make it with `freeze` from
 *   `./page-structure`. List it in `unasked` if the page draws it on load, or in `answers` if
 *   the page draws it only after an act of the reader. `page-words.ts` exports it again.
 * - Put a new key for a table of this file in this file, from any pull request.
 * - Import nothing from `page-words.ts`, not even a type. Import only the type `PartWords` from
 *   `./page-words-parts`, and values from `./page-structure`. A value import from
 *   `page-words.ts` or `./page-words-parts` makes an import cycle.
 */
import { freeze } from "./page-structure";
import type { PartWords } from "./page-words-parts";

/**
 * The headings of a command bar run's own page (`run-page/RunPage.tsx`): the asker's whole
 * question, and the nodes the run changed. The page's heading is the run's kind word,
 * `NODE_REFERENCES_WORDS.kinds.interaction`, and what each frame of its answer says is in
 * `STATE_LINES`, `EMPTY_STATE_LINES` and `ANSWER_LINES`.
 */
export const RUN_PAGE_WORDS = freeze({
  question: "Question",
  what_changed: "What it changed",
} as const);

export default {
  add: {
    NODE_REFERENCES_WORDS: {
      edges: {
        /** The edge from a node a command bar run changed to that run, which
         *  `run-references/run-references.ts` draws on the run's row in Details' Referenced by. */
        changed_by_run: "changed this issue",
      },
      kinds: {
        /** A run of the command bar, which only its asker reads. It is lowercase, as this table
         *  holds its words, and the row draws it with `sentenceCase`. */
        interaction: "command bar run",
      },
    },
    EMPTY_STATE_LINES: {
      /** What a command bar run's page says for its `none` frame, where the model produced no
       *  answer text at all (`run-page/RunPage.tsx`). */
      run_none: "The model gave no answer.",
    },
    STATE_LINES: {
      /** What a command bar run's page says for its `changed` frame, where pieces of the answer
       *  are shown as plain text or removed (`run-page/RunPage.tsx`). The frame's `dropped` count
       *  is drawn between `before` and `after`, as `BLOCKED_BY_REFUSALS` cuts its sentences. */
      run_changed: { after: ".", before: "Pieces of the answer shown as plain text or removed: " },
      /** What a command bar run's page says for its `cut` frame, where the answer was cut short
       *  (`run-page/RunPage.tsx`). The frame's `words` count is drawn between `before` and
       *  `after`, as `BLOCKED_BY_REFUSALS` cuts its sentences. */
      run_cut: { after: " words.", before: "The answer was cut short after " },
      /** What a command bar run's page says for the error code `ANSWER_DID_NOT_ARRIVE`
       *  (`run-page/RunPage.tsx`). */
      run_did_not_arrive: "The answer did not arrive in time.",
      /** What a command bar run's page says for the error code `ANSWER_FAILED`, and for any code
       *  the page does not know (`run-page/RunPage.tsx`). */
      run_failed: "The question could not be answered.",
      /** What a command bar run's page says for the error code `NO_MODEL_CONFIGURED`
       *  (`run-page/RunPage.tsx`). */
      run_no_model: "No model is set up to answer questions.",
    },
    ANSWER_LINES: {
      /** What a command bar run's page says when its one read failed with anything but 400 or
       *  404, with Retry beside it (`run-page/RunPage.tsx`). */
      run_unreadable: "Could not load this run.",
    },
  },
  unasked: [RUN_PAGE_WORDS],
  answers: [],
} as const satisfies PartWords;
