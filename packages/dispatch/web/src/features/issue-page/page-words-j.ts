/**
 * Part J's words for the new issue page. `page-words-parts.ts` joins them into `page-words.ts`.
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
    STATE_LINES: {
      /** What the Spec view says over its columns, on load as much as later, of a group of its
       *  reads that stands failed, beside one Retry for the group
       *  (`spec-filter/SpecFilterBody.tsx`): the spec's blocks and their coverage, and the results
       *  and the Verification document's blocks. The second is not the outline's
       *  `results_unreadable`, which names the results read alone. */
      statements_unreadable: "Could not load the statements.",
      spec_results_unreadable: "Could not load the results.",
    },
  },
  unasked: [],
  answers: [],
} as const satisfies PartWords;
