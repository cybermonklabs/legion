/**
 * Part W's words for the new issue page. `page-words-parts.ts` joins them into `page-words.ts`.
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
 * The words the rows of a lens, a finding, a risk and a constraint draw of their own: the lines
 * under a lens's, a risk's and a statement's words, the chain control of a lens, and the
 * act that opens the menu of lens kinds with that menu's heading. `taken_at` is lowercase
 * because a measurement's line draws it between the author's own words. What a read or an
 * invocation the reader started says while it is out and when it failed is `LENS_ANSWERS`.
 */
export const LENS_WORDS = freeze({
  /** Where a lens's verdict goes, as its words, when it has none. */
  no_verdict: "No verdict yet",
  /** The line of chips for the lenses this lens's `triggered_by` names, and the line for the
   *  lenses whose `triggered_by` names this one. */
  triggered_by: "Triggered by",
  triggered: "Triggered",
  /** A risk's line that opens its `early_warning` words, the line of chips for the checks that
   *  prevent it or the one line saying none does, and the line that opens its `prevention`
   *  words. */
  early_warning: "Early warning:",
  prevented_by: "Prevented by",
  prevented_by_nothing: "Prevented by nothing",
  prevention: "Prevention:",
  /** The control of a lens that reads its chain, and the end of a chain the read cut
   *  short, before the chip of the lens it stopped at. */
  show_chain: "Show the whole chain",
  more_lenses: "More lenses past this one",
  /** The menu of lens kinds' heading, and the act of a row that opens it. */
  menu_title: "Examine through a lens",
  act: "Lens",
  /** A statement's line of chips for the constraints it names, and what a measurement's line
   *  draws before where it was taken. */
  constrained_by: "Constrained by",
  taken_at: "taken at",
} as const);

/**
 * What the lens rows say in answer to the reader's own act, and never on load: the menu of lens
 * kinds while its catalogue is read and when that read failed, an invocation while it is out
 * and when Dispatch refused it, and a chain while it is read and when that read failed. Each is
 * at most one sentence.
 */
export const LENS_ANSWERS = freeze({
  loading_kinds: "Loading lens kinds…",
  kinds_failed: "Could not load the lens kinds.",
  examining: "Examining…",
  /** A refusal with 404, of an invocation and of a chain's read alike, or of an invocation with
   *  409 `NODE_GONE`. */
  gone: "This is no longer here.",
  failed: "Could not examine this through a lens.",
  loading_chain: "Loading the chain…",
  chain_failed: "Could not load the chain.",
} as const);

export default {
  add: {
    NODE_TYPE_NAMES: {
      /** A constraint of the issue or of the lens catalogue: a standard the work keeps, or a
       *  measurement taken of it. */
      constraint: "constraint",
      /** One block a lens wrote under itself: a `block` node whose parent is a lens, which the
       *  model gives no kind of its own. */
      finding: "finding",
      /** One analysis of a kind of the lens catalogue, invoked on a node of the issue. Its row's
       *  tag draws its kind's words; this names its shape. */
      lens: "lens",
      /** What a pre-mortem says could go wrong, a row under that lens. */
      risk: "risk",
    },
    NODE_REFERENCES_WORDS: {
      edges: {
        constrained_by: "constrained statement",
        prevented_by: "prevented risk",
        triggered_by: "triggered lens",
      },
      kinds: {
        constraint: "constraint",
        lens: "lens",
        /** A lens's finding, whose address is the lens's with the block's anchor after it. */
        lens_block: "finding",
        lens_kind: "lens kind",
        risk: "risk",
      },
    },
    GLYPH_TOKENS: {
      types: {
        constraint: "constraint",
        finding: "finding",
        lens: "lens",
        risk: "risk",
      },
    },
  },
  unasked: [LENS_WORDS],
  answers: [LENS_ANSWERS],
} as const satisfies PartWords;
