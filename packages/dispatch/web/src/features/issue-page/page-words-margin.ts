/**
 * Part "margin"'s words for the new issue page (INFRA-5706, comments on selected text).
 * `page-words-parts.ts` joins them into `page-words.ts`.
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
 *
 * No file of `text-comments/` writes these words itself: each draws them from here (R11).
 */
import { freeze } from "./page-structure";
import type { PartWords } from "./page-words-parts";

/** The words the column of comments on text and its control draw of their own. */
export const MARGIN_WORDS = freeze({
  /** The control beside a selection's end that opens the composer on the selected words, and
   *  the composer's Send (R5). */
  comment: "Comment",
  /** The name of the column at the right of the main column, a region, and of a node's sheet
   *  of threads below 1024 px (R2, R3, R6). */
  region: "Comments on text",
} as const);

/** What the composer of a comment on text says after the reader's own send (R5). */
export const MARGIN_ANSWERS = freeze({
  /** The server took no anchor of the selected words: 400 `INVALID_ANCHOR`, or 404
   *  `TARGET_NOT_FOUND`. */
  words_gone: "The selected words are no longer in this text. Select them again.",
  /** A reply's 201 whose `reply_block` holds no quote though the send carried one (decision 465
   *  (4)): the reply is a row under what it answers, and no thread. */
  quote_not_kept: "The selected words were not kept, so this reply stands under what it answers.",
} as const);

// The lists are typed as the join reads them (`PartWords`): two parts with tables of their own
// give `WORD_FILES.flatMap` two element types it cannot join, so each holds `unknown`.
const UNASKED: readonly unknown[] = [MARGIN_WORDS];
const ANSWERS: readonly unknown[] = [MARGIN_ANSWERS];

export default {
  add: {},
  unasked: UNASKED,
  answers: ANSWERS,
} as const satisfies PartWords;
