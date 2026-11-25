/**
 * Part X's words for the new issue page. `page-words-parts.ts` joins them into `page-words.ts`.
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
 * The marks a row carries beside its state where the model holds them: a proposal nobody has
 * settled yet, what a later version of the issue overtook, and what only its author may read. Each
 * is drawn as its shape and this word, in the row's margin and in the compact form after a chip's
 * name, and the word names that shape.
 */
export const ROW_MARK_WORDS = freeze({
  only_you: "Only you",
  overtaken: "Overtaken",
  proposed: "Proposed",
} as const);

/** One mark of a row, by its own token, which is its `data-node-mark`. */
export type RowMark = keyof typeof ROW_MARK_WORDS;

/**
 * What Settle says while its write is out, and the one sentence it draws after itself when
 * Dispatch refuses the write, in place of the server's message: the act reads the refusal's status
 * and code, never its text. One of the tables `ANSWER_SOURCES` gathers.
 */
export const SETTLE_ANSWERS = freeze({
  /** Every refusal the three below do not name: a refusal of any other status or code. */
  failed: "Could not settle this.",
  /** 404: nothing answers to the row's address now. */
  gone: "This is no longer here.",
  /** 403 `SETTLE_NOT_PERMITTED`. */
  not_permitted: "You may not settle this.",
  /** 409 `NODE_NOT_PROPOSED`: the row is no proposal now. */
  not_proposed: "This is no longer proposed.",
  /** While the write is out: Dispatch has not answered, so it does not say the row is settled. */
  settling: "Settling…",
} as const);

export default {
  add: {
    CONTROL_LABELS: {
      /** Makes a proposed row the issue's own, among the focused row's acts: its mark goes, and
       *  the row draws its gaps and is counted. */
      settle: "Settle",
    },
    GLYPH_TOKENS: {
      /** One shape per mark a row carries beside its state (`ROW_MARK_WORDS`), each named by its
       *  mark's word, so a proposed, an overtaken and a reserved row are told apart by a word and
       *  a shape. */
      marks: {
        only_you: "mark-only-you",
        overtaken: "mark-overtaken",
        proposed: "mark-proposed",
      },
    },
  },
  // Typed as `PartWords` types them, so the join of every part's tables reads one type.
  unasked: [ROW_MARK_WORDS] as readonly unknown[],
  answers: [SETTLE_ANSWERS] as readonly unknown[],
} as const satisfies PartWords;
