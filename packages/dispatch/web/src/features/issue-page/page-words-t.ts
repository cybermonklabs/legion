/**
 * Part T's words for the new issue page. `page-words-parts.ts` joins them into `page-words.ts`.
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
    EMPTY_STATE_LINES: {
      /** Graph, the whole tree read, on an issue whose tree holds nothing under it. */
      graph: "This issue holds nothing yet.",
    },
    ANSWER_LINES: {
      /** What the outline says when the issue's tree could not be read before it first
       *  answered, so it holds no row to draw. */
      outline_unreadable: "Could not load the outline.",
    },
    CONTROL_LABELS: {
      /** The row after a node's first 50 children, and after each next page of them, which
       *  draws the next 50 (R15): the count before it is how many children are left and carries
       *  `AUTHOR_TEXT_ATTRIBUTE`, and the words agree with it: "1 more row", "7 more rows". */
      page_row: "more row",
      page_rows: "more rows",
      /** The control above the outline's first row under the issue while the outline read
       *  answers older comments than it holds, which reads the next page of them (R15). */
      older_comments: "Older comments",
      /** The control after the last row of an outline or a view while its read answers a next
       *  page, which reads that page (R15): the count before it is how many nodes the read holds
       *  after the pages read (its `remaining`) and carries `AUTHOR_TEXT_ATTRIBUTE`, and the
       *  words agree with it: "1 more row to load", "1,240 more rows to load". Where the read
       *  gives no count, the control says `tree_page` alone. */
      tree_page_row: "more row to load",
      tree_page_rows: "more rows to load",
      tree_page: "Load more rows",
      /** The reader's own comment carries it on its row: it sets the line at the foot of the
       *  outline to edit that comment, which `save` sends back (R11). */
      edit_comment: "Edit",
    },
  },
  unasked: [],
  answers: [],
} as const satisfies PartWords;
