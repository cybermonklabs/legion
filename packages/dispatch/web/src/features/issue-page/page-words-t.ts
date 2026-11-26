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
    NODE_TYPE_NAMES: {
      /** A `prose` node: a document's plain paragraph, as the model stores it, which Graph draws
       *  as a row of its own. The page draws the word a reader uses, never the model's token. */
      prose: "paragraph",
    },
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
      /** The control at the foot of the Contents list while the outline read answers older
       *  comments than it holds, which reads the next page of them, the same page the control
       *  above the outline reads (R15). */
      contents_older: "Load older comments",
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
      /** The heading of the children's section above the outline, its one control, which opens
       *  and closes the list of the issue's children; the header's bar and both counts stand
       *  beside it (R14). */
      children_section: "Child issues",
      /** The control after the children the section lists, while more remain, which lists the
       *  next 20 (R14, R15): the count before it is how many children are left and carries
       *  `AUTHOR_TEXT_ATTRIBUTE`, and the words agree with it: "1 more child", "3 more children". */
      children_page_child: "more child",
      children_page_children: "more children",
    },
    PLAN_WORDS: {
      /** The units a task's size is drawn in, after its number: "45 min", "1 h 30 min"
       *  (`plan-filter/plan-order.ts`'s `sizeWords`). */
      hours: "h",
      minutes: "min",
    },
    NODE_FACT_LABELS: {
      /** The issue under this one that a task of the Plan view belongs to, drawn before its
       *  key, which links to it. */
      issue: "issue",
    },
    GLYPH_TOKENS: {
      types: {
        /** A `prose` node's shape (`NodeGlyph.tsx`), by the model's token, as every type's is. */
        prose: "prose",
      },
    },
  },
  unasked: [],
  answers: [],
} as const satisfies PartWords;
