/**
 * Part L's words for the new issue page. `page-words-parts.ts` joins them into `page-words.ts`.
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
 * What the page says in the outline's place where the address the reader followed names nothing
 * the outline draws a row for (`slots/body.tsx`): an ask, a comment or a message the issue holds
 * none of, or whose read failed, by the kind of item the address names; a block of a document no
 * row is drawn for; the child issues of an issue with none; and the link back to the issue's own
 * address under each. Each is drawn in answer to an address followed, which is the reader's own
 * act, as `KEY_REPORTS.agents_thread` is (`page-structure.ts`), so this is one of the page's
 * answers (`answers` below, which `ANSWER_SOURCES` of `page-words.ts` holds). They are the words
 * of today's page for the same states (`issue/IssuePage.tsx`'s `ItemLandingFailure`), which
 * `slots/body.tsx` copies until that page is removed. A line that names the issue is cut where
 * the issue's key goes, as `BLOCKED_BY_REFUSALS` of `page-words.ts` cuts its sentences: the key
 * is drawn between `before` and `after`.
 */
export const ADDRESS_FALLBACK_WORDS = freeze({
  /** The link under each line that takes the reader to the issue's own address. */
  back: { after: "", before: "Back to " },
  /** A block of a document the outline draws no row for - a paragraph, a heading, a table - is
   *  no node of the tree as much as a block id no document holds, so the words fit both. */
  block: {
    heading: "No row for this block",
    line: { after: "'s outline.", before: "That link names no row of " },
  },
  children: {
    heading: "No child issues",
    line: { after: ", which has none.", before: "That link names the child issues of " },
  },
  /** An item the issue holds none of (`missing`), and one whose read failed (`unreadable`): the
   *  heading over each, the line under a missing one, and the alert beside a failed read's
   *  Retry (`FailedReadLine`). */
  items: {
    ask: {
      missing: {
        heading: "Ask not found",
        line: {
          after: " — it may have been deleted.",
          before: "That link doesn't name an ask on ",
        },
      },
      unreadable: { heading: "Couldn't load this ask", message: "Couldn't load this ask." },
    },
    comment: {
      missing: {
        heading: "Comment not found",
        line: {
          after: " — it may have been deleted.",
          before: "That link doesn't name a comment on ",
        },
      },
      unreadable: { heading: "Couldn't load this comment", message: "Couldn't load this comment." },
    },
    message: {
      missing: {
        heading: "Message not found",
        line: {
          after: " — it may have been deleted.",
          before: "That link doesn't name a message on ",
        },
      },
      unreadable: { heading: "Couldn't load this message", message: "Couldn't load this message." },
    },
  },
} as const);

/**
 * The value of `aria-current` on the row of the conversation's current point, the node the
 * reader's next turn answers (`current-point.ts`). A screen reader says it in its own words for
 * the current location, so the page adds no hidden text and every row keeps its accessible name.
 * It is no word the page draws, so neither `unasked` nor `answers` below lists it.
 */
export const CURRENT_POINT = "location" as const;

export default {
  add: {
    ANSWER_LINES: {
      /** What Send says while the send is out, on the write line of the box at the foot of the
       *  outline, beside the busy words of `ANSWER_LINES` in `page-words.ts`. */
      sending: "Sending…",
      /** What the write line says while a file pasted or dropped into its draft uploads: on its
       *  Send, and on the status line under the field. */
      uploading: "Uploading file…",
      /** What the write line says when Dispatch stored a file pasted or dropped into its draft
       *  and lost its answer, so the file's reference could not be written into the draft. */
      uploaded_unanswered:
        "The file was uploaded, but Dispatch lost its answer, so its reference is not in the draft.",
      /** What the box says when Dispatch refused a comment on a statement whose words its
       *  document holds twice (`TARGET_AMBIGUOUS`): the box places a statement's comment by the
       *  statement's whole words alone, so the same words are refused again whatever is sent, and
       *  the draft stays in the box. */
      statement_twice:
        "The statement's words stand twice in its document, so the comment cannot be placed on it.",
      /** What the box says, once a row's Reply moved it on, of what the reader wrote on the
       *  issue, or on a statement, and the box keeps there, with the control that goes back to
       *  it: the box's own that writes on the issue, or the statement's Reply. A reply kept for a
       *  row is said in the composer's own words, which name the author of what it answers. */
      kept_on_issue:
        "What you wrote on the issue is kept: press Write on the issue instead to go on with it.",
      kept_on_statement:
        "What you wrote on a statement is kept: press Reply on that statement to go on with it.",
      /** What the box says of what the reader wrote on a statement the outline no longer draws -
       *  a new version took it out or changed its words, so its Reply aims the box at other
       *  words - with the draft drawn after it, Use it here and Discard my draft; and, while the
       *  box holds words of its own, that Use it here waits for it to be empty. */
      kept_statement_gone:
        "The statement you wrote on has changed or is gone, so what you wrote is kept here:",
      kept_statement_gone_waits:
        "The statement you wrote on has changed or is gone, so what you wrote is kept here to use in this field once it is empty:",
    },
    CONTROL_LABELS: {
      /** Opens the editor of the issue's title: the button laid over the title's heading in the
       *  header's second row (`IssueTitleRow.tsx`), which draws no words of its own and has the
       *  title as its description. */
      edit_title: "Edit title",
      /** What the box at the foot of the outline says while it is empty and writes a comment or
       *  a question on the issue itself: where the words go, as the reply's and the statement's
       *  placeholders say where theirs go. */
      issue_placeholder: "Write on the issue…  (Enter sends)",
      /** Each placeholder of that box in the shorter words it says at 760 px and below, where
       *  its line is about 240 px wide and a placeholder is one line, cut by the line's edge:
       *  each fits that width whole. */
      issue_placeholder_phone: "Write on the issue…",
      reply_placeholder_phone: "Your reply, under the row…",
      edit_placeholder_phone: "Your comment, rewritten…",
      /** What the box says while it is empty and writes a comment on a statement: the comment
       *  goes beside the statement, in the margin that draws a comment on a statement's text, and
       *  not under its row as a reply does. */
      statement_placeholder: "Your comment, beside the statement it is on…  (Enter sends)",
      /** The same in the shorter words the box says at 760 px and below, which fit its line
       *  whole. */
      statement_placeholder_phone: "Comment beside the statement…",
    },
  },
  unasked: [],
  answers: [ADDRESS_FALLBACK_WORDS],
} as const satisfies PartWords;
