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
import type { PartWords } from "./page-words-parts";

export default {
  add: {
    EMPTY_STATE_LINES: {
      /** What a document's own address says where the issue holds no document or file at that
       *  address (part N's R2): never the slug the address wrote, and never the old tab's word. */
      document_missing: "This issue holds no document at this address.",
      /** What Documents' list says where the issue holds no document and no file (part N's R2,
       *  R4), as every other control of the view row says where it draws nothing. */
      documents: "This issue has no documents yet.",
    },
    ANSWER_LINES: {
      /** What the opened document's comparison of two versions says while it reads their text,
       *  which the reader asked for, when it could not, and of two files that hold the same bytes
       *  (part N's R2): today's comparison's words. */
      versions_loading: "Loading versions to compare…",
      versions_unreadable: "Could not load versions to compare.",
      versions_same: "The two versions are the same.",
      /** What Documents' list's Remove says while its removal is out, which today's control draws
       *  (`DocumentWayBack.tsx`) and the list marks as the page's own words (part N's R9). */
      saving_remove: "Removing…",
    },
    CONTROL_LABELS: {
      /** What the list of keys says of `t e`, which opens `/steps`, Details at the issue's steps
       *  where today's page has its Steps tab (part N's R6, `useIssueKeymap.ts`). */
      open_steps: "Open the issue's steps in Details",
      /** What the list of keys says of `t a`, which opens Documents' list at `/documents` with
       *  the focus on its first row, where today's page has its Artifacts tab (part N's R4,
       *  `useIssueKeymap.ts`). */
      open_documents: "Open the issue's documents",
      /** The opened document's way back to Documents' list, in its head (part N's R2): it opens
       *  the list at `/issues/<key>/documents` with the focus on the document's row. */
      back_to_documents: "Back to Documents",
      /** What the list of keys says of Escape on an opened document, which closes it: back to
       *  Documents' list, or to the row that holds the chip that opened it (part N's R1). */
      close_opened_document: "Close the document",
      /** What the list of keys says of Escape on Documents' list, which returns to the control
       *  the reader opened it from, or to Conversation (part N's R1, R2). */
      leave_documents: "Leave Documents",
      /** The opened document's comparison of any two versions under its editor or picture (part
       *  N's R2), today's comparison's words: its heading, the name its region takes before the
       *  document's own name ("Compare versions for notes.md"), the press that opens it and
       *  closes it, and its two pickers, "From" and "To", each named after the document as well
       *  ("Compare notes.md from"). */
      compare_versions: "Compare versions",
      compare_versions_for: "Compare versions for",
      show_comparison: "Show comparison",
      hide_comparison: "Hide comparison",
      compare: "Compare",
      compare_from: "From",
      compare_to: "To",
    },
    FILE_WORDS: {
      /** What the head of a document or file opened in place of the outline says of the version it
       *  shows, before that version's number, its author and its day: "version 3 · alice · 5 Nov"
       *  (part N's R2). */
      version: "version",
      /** The opened document's list of its versions (part N's R2): its heading, and the name its
       *  region takes before the document's own name, "Versions for notes.md". */
      versions: "Versions",
      versions_for: "Versions for",
    },
  },
  unasked: [],
  answers: [],
} as const satisfies PartWords;
