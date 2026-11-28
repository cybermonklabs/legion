/**
 * Part N's words for the new issue page. `page-words-parts.ts` joins them into `page-words.ts`.
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
 * How the issue's pull request stands, in the page's own words, drawn on the header's pull
 * request link beside its number (part N's R22, `PullRequestLink.tsx`): one word per state
 * `IssueDetails.pull_request.state` carries (`open`, `merged`, `closed`).
 */
export const PULL_REQUEST_STATE_WORDS = freeze({
  closed: "Closed",
  merged: "Merged",
  open: "Open",
} as const);

export default {
  add: {},
  unasked: [PULL_REQUEST_STATE_WORDS],
  answers: [],
} as const satisfies PartWords;
