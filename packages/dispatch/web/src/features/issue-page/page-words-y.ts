/**
 * Part Y's words for the new issue page. `page-words-parts.ts` joins them into `page-words.ts`.
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
 * What the box's criterion line says of a write the reader stopped, which may have reached
 * Dispatch, by whether its Retry still goes under the write's key: while Dispatch holds the key,
 * a sentence cut around the hours it keeps one, and once it holds it no longer, one sentence
 * whole. They are the page's own, as the box's sentences for a send and a file a reader stopped
 * are (`COMPOSER_ANSWER_WORDS`), so the box draws no word of another surface's.
 */
export const CRITERION_STOPPED_WORDS = freeze({
  expired:
    "Dispatch has not confirmed the criterion and no longer holds its key, so a Retry writes it anew and may write it a second time.",
  unconfirmed: {
    after: " hours of the first try writes it at most once, while a later one may write it twice.",
    before: "Dispatch has not confirmed the criterion, which may be written, and a Retry within ",
  },
});

export default {
  add: {
    VERIFICATION_WORDS: {
      /** A check's own control, which opens its facts where they are shut and shuts them where
       *  they are open, and keeps them so once the focus leaves the check. */
      facts: "Facts",
    },
    ANSWER_LINES: {
      /** What the box's criterion line says when Dispatch refused its write. */
      criterion_not_written: "Could not write the criterion.",
      /** What it says when the row the criterion is written under is gone, a 404. */
      criterion_gone: "This is no longer here.",
    },
    CONTROL_LABELS: {
      /** What the box's criterion line says of the row it writes under, before that row's
       *  author and the page's word for its kind. */
      criterion_target: "Criterion under:",
      /** What the box's criterion line says while it is empty. */
      criterion_placeholder: "What should a person be able to do?  (Enter sends)",
      /** What the box's criterion line says while it is empty, at 760 px and below. */
      criterion_placeholder_phone: "What should a person be able to do?",
      /** The act of a focused question, comment or message that aims the box's criterion
       *  line. */
      write_criterion: "Write a criterion",
    },
  },
  unasked: [],
  answers: [CRITERION_STOPPED_WORDS],
} as const satisfies PartWords;
