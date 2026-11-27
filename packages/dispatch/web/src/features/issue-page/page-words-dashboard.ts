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
 *   `./page-words-parts`, and values from `./page-structure`. A value import from
 *   `page-words.ts` or `./page-words-parts` makes an import cycle.
 *
 * INFRA-4870's Legion block (dispatch://INFRA-4870/spec#b-r-block) writes this file with the
 * phase words and the claim-state words alone, and INFRA-5702 adds Dashboard's own words to it.
 */
import { freeze } from "./page-structure";
import type { PartWords } from "./page-words-parts";

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
  /** `awaiting_children`, which marks Admitted. */
  awaiting_children: "Waiting on children",
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

export default {
  add: {},
  unasked: [PHASE_WORDS, ROLE_STATE_WORDS],
  answers: [],
} as const satisfies PartWords;
