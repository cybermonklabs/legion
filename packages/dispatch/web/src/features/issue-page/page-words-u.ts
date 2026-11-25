/**
 * Part U's words for the new issue page. `page-words-parts.ts` joins them into `page-words.ts`.
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
import type { PartWords } from "./page-words-parts";

export default {
  add: {
    DECISIONS_WORDS: {
      /** What a decision with a Change reads in place of its state's own word
       *  (`NODE_STATE_WORDS.decision.change_requested`), wherever the page draws it, the
       *  outline's row as much as this view: then the name of the person whose Change it waits
       *  on, the reader's own where they asked (R2, R3). */
      changed_by: "Changed by",
      /** After the count of a part's entries not yet drawn, on the control under the part that
       *  draws the next of them, which says it in brackets after what a press draws: "Show the
       *  next 20 (25 left)" (R2: a control at a part's foot "that says how many are left",
       *  decision 424). */
      page_left: "left",
      /** The control under a part whose entries are not all drawn, before the count of those a
       *  press draws, at most 20, the size root R40 pages a flat list by: "Show the next 20 (25
       *  left)". Each part draws its first 20 entries as the view opens, and a press draws the
       *  next ones; a page is no fold (R2, decision 424). */
      page_next: "Show the next",
    },
  },
  unasked: [],
  answers: [],
} as const satisfies PartWords;
