/**
 * Threads' words (dispatch://INFRA-5704/spec#b-r-words). `page-words-parts.ts` joins them into
 * `page-words.ts`.
 *
 * - `add` holds Threads' new keys for the tables of `page-words.ts`. Key them by the table's
 *   name, and nest them as the table is: `add: { CONTROL_LABELS: { load_older: "Load older" } }`.
 * - A table of Threads' own is a named export of this file. Make it with `freeze` from
 *   `./page-structure`. List it in `unasked` if the page draws it on load, or in `answers` if
 *   the page draws it only after an act of the reader. `page-words.ts` exports it again.
 * - Put a new key for a table of this file in this file, from any pull request.
 * - Import nothing from `page-words.ts`, not even a type. Import only the type `PartWords` from
 *   `./page-words-parts`, and values from `./page-structure`. A value import from
 *   `page-words.ts` or `./page-words-parts` makes an import cycle.
 *
 * Threads draws some words that the page's tables hold already, and reads them there rather than
 * from a second copy here: Reply, Edit, Send and Retry (`CONTROL_LABELS`), and the control under
 * the rows, "Show the next 20" and its "left" (`DASHBOARD_WORDS`). Its name is `PRESET_NAMES`'
 * own, which takes keys only in `page-words.ts`.
 *
 * Every sentence Threads draws is in a table of `page-words.ts` through `add`: the lines that say
 * a place holds nothing in `EMPTY_STATE_LINES`, and its answers in `ANSWER_LINES`. So
 * `page-words.test.ts` reads each of them by the rule of its table.
 */
import { freeze } from "./page-structure";
import type { PartWords } from "./page-words-parts";

/** Threads' own words outside the text of an entry (R2, R7, R10): the choices over the list, the
 *  control under it, the kind of each record, who the Legion daemon is, and New thread's chooser
 *  and box. */
export const THREADS_WORDS = freeze({
  /** The four choices over the list, drawn in the order Mine, Waiting on me, All and Agents
   *  (R2). */
  choices: {
    agents: "Agents",
    all: "All",
    mine: "Mine",
    waiting: "Waiting on me",
  },
  /** Under the rows: the control that opens New thread in the main column (R2, R10). */
  new_thread: "New thread",
  /** Below 1024 px, above an opened record and above New thread: the control that returns to
   *  the list (R2). */
  back_to_list: "Back to the list",
  /** The line that names a record's kind on its row (R7): a question, a comment, a comment on
   *  text, and a message. A message whose root targets an agent reads `message_to` and then the
   *  agent. */
  kinds: {
    ask: "Question",
    comment: "Comment",
    message: "Message",
    message_to: "Message to",
    text: "Comment on text",
  },
  /** On a row and on an opened record whose comment thread is on text that was deleted in Graph
   *  (R7, R8). They say "text", since the page's rule refuses the model's word "node"
   *  (`page-words.test.ts`, "no word the page declares is the model's word for a node"). */
  gone_node: "On text deleted in Graph",
  /** The Legion daemon's name, which the page draws for its session `legion-daemon:<PROJECT>`,
   *  with the project's key after it (R7). */
  daemon: "Legion daemon",
  /** New thread's chooser of the agent, and the placeholder of the field that filters it
   *  (R10). */
  chooser: "Agent",
  chooser_filter: "Filter agents",
  /** New thread's box, which writes a message to the chosen agent (R10). */
  message_box: "Message",
} as const);

// The lists are typed as the join reads them (`PartWords`): two parts with tables of their own
// give `WORD_FILES.flatMap` two element types it cannot join, so each holds `unknown`.
const UNASKED: readonly unknown[] = [THREADS_WORDS];
const ANSWERS: readonly unknown[] = [];

export default {
  add: {
    ANSWER_LINES: {
      /** A choice's read that failed, beside its Retry (choice 7 of the plan): the comments', the
       *  messages' or the questions' page. */
      threads_unreadable: "Could not load the threads.",
      /** A build that answers a thread page with the route's bare list, as one from before the
       *  pages does while a release rolls out (`threadPageNotOffered`), which is not read
       *  again. */
      threads_older_build:
        "The server that answered cannot list threads yet, because a new release is in progress.",
      /** What the main column says for `/threads/<id>` whose id names no record of the issue
       *  (R12). It answers an address the reader followed, as part L's lines for an address the
       *  outline cannot find do (`ADDRESS_FALLBACK_WORDS`). */
      threads_not_found: "This issue holds no thread at this address.",
      /** What a box that writes a message says beside its Retry where its send failed and
       *  Dispatch gave no words of its own, as for a lost connection (R9). */
      threads_reply_not_sent: "Could not send the reply.",
    },
    EMPTY_STATE_LINES: {
      /** All, where the issue holds no thread that All lists (R3). */
      threads: "This issue has no threads yet.",
      /** Mine, where no thread of the issue holds a write of the reader's (R4). */
      threads_mine: "No thread of this issue holds anything you wrote.",
      /** Waiting on me, where no thread the reader wrote in holds a write they have not read
       *  (R6). */
      threads_waiting: "No thread waits on you.",
      /** Agents, where the issue holds no thread between agents (R5). */
      threads_agents: "No thread of this issue is between agents alone.",
      /** From 1024 px up, the main column with no record and no New thread open (R2). */
      threads_none_open: "Open a thread from the list, or start one with New thread.",
    },
    FILTER_SIGNAL_WORDS: {
      /** After the number of the rows of Waiting on me, on the Threads control (R1). */
      new_replies: "with new replies to you",
    },
  },
  unasked: UNASKED,
  answers: ANSWERS,
} as const satisfies PartWords;
