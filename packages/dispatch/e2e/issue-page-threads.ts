/**
 * The helpers that only the rows of `e2e/issue-page-threads.e2e.ts` read (dispatch://INFRA-5704):
 * the contract between those rows and the Threads view, the seed, the reads that say what each row
 * expects, and the readers of what the view draws.
 *
 * The rows are written before the view exists. This file and its rows are copied alone into a
 * build of the stack base for the before-and-after run (c-before-after), so both import only
 * modules that exist there: nothing under `features/issue-page/threads/` and not the view's words
 * file. Every word and selector the rows read is a literal constant under "The contract" below.
 *
 * Each row derives what it expects from the server's records, read through the API with the reads
 * the view sends, and never from a count written here. The oracles below hold the spec's rules
 * (R3 to R8) over those reads: which records a thread gives, the order and the pages of each
 * choice, and the count of new writes of each row.
 */

import { randomUUID } from "node:crypto";
import type {
  AskThreadPage,
  CommentThreadPage,
  DispatchEvent,
  IssueCollaborators,
  MessageThreadPage,
} from "@legion/contracts";
import { expect, type Locator, type Page } from "@playwright/test";

import { routeUrl } from "../web/src/api/paths";
import type {
  Actor,
  Agent,
  CommentWithTrees,
  MessageWithTrees,
  MyReplyRow,
} from "../web/src/api/types";
import { agentRoleWord } from "../web/src/features/issue-page/agent-role";
import { isViewer } from "../web/src/features/refs/actor";
import { mentionOptions, stoppedRoleOptions } from "../web/src/features/refs/party-options";
import { partyOfAddress } from "../web/src/features/refs/routes";
import { compareTimestamps } from "../web/src/lib/timestamps";
import { type FakeSession, legionAgentEntry, setLegionAgents, setLiveSessions } from "./agents";
import {
  answerAsk,
  baseUrl,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  deleteComment,
  getIssue,
  getIssueEvents,
  getMyState,
  listAgents,
  listMyReplies,
  putIssueState,
  replyToCommentBlock,
  replyToMessageBlock,
  userHeaders,
} from "./api";
import { issuePage, openMenu, presetChooser, presetRow } from "./issue-page";

// =============================================================================================
// The contract between these rows and the Threads view
// =============================================================================================
//
// The rows read the Threads view only through the words and the selectors below. The view's code
// and its words file (`features/issue-page/page-words-threads.ts`) draw each of them exactly as it
// is written here. Each constant says which element carries it. Where a role and an accessible
// name can name a thing, the rows read it so. Where a role cannot, they read a `data-` attribute.
//
// The page's own convention holds inside the view: the view's words carry `data-page-chrome`, and
// every value that is no word of the page stands inside an element that carries
// `data-author-text` (`page-structure.ts`). Such a value is an entry's text, a name, a role token,
// a key, a count, or a session's directory. Every time stands inside a `time` element.

/** The view's name. From 1280 px up it names the view row's toggle `button` (`aria-pressed`),
 *  whose `[data-preset-name]` reads it. Below 1280 px it starts the name of the one control's
 *  `menuitemradio`. The control stands right after Decisions. The view itself is a `region` (a
 *  `section` with `aria-label`) of the same name inside `[data-issue-page]`, which holds the
 *  list column and the main column. */
export const THREADS = "Threads";

/** The view's token: the address `/issues/<KEY>/threads`, and the value of the view row's count
 *  `[data-preset-count="threads"]`, which holds the number alone. */
export const THREADS_TOKEN = "threads";

/** The words after the control's count (R1). The count's element stands in the signal that
 *  `IssuePresets.tsx` draws, so from 1280 px up the toggle's accessible description is
 *  `<N> with new replies to you`, and below 1280 px the menu item's accessible name is
 *  `Threads <N> with new replies to you`. Neither the count nor these words are drawn at 0. */
export const NEW_REPLIES = "with new replies to you";

/** The four choices (R2), each a `button` carrying `aria-pressed` and named exactly its word,
 *  inside the one element `[data-thread-choices]` above the rows, in the order of
 *  `CHOICE_ORDER`. One is pressed at a time, and All is pressed when the view opens. */
export const CHOICES = {
  agents: "Agents",
  all: "All",
  mine: "Mine",
  waiting: "Waiting on me",
} as const;

/** The order the choices are drawn in, left to right. */
export const CHOICE_ORDER = ["mine", "waiting", "all", "agents"] as const;

export type Choice = (typeof CHOICE_ORDER)[number];

/** The `button` under the rows, named exactly this, which opens New thread in the main column. */
export const NEW_THREAD = "New thread";

/** The control under a choice's rows while rows are left (R2): a `button` whose accessible name
 *  is `Show the next 20 · <N> left`. N is the count R3 names for All and Mine, the route's
 *  `remaining` for Agents, and the route's rows not drawn for Waiting on me. A route's next page
 *  is read when the reader presses the control, before the next 20 rows are drawn, and never
 *  earlier. No control is drawn once no row is left. */
export const SHOW_NEXT = "Show the next 20";

/** The word after the number the control above says is left. */
export const LEFT = "left";

/** The control's accessible name, with the number it reads. */
export const SHOW_NEXT_NAME = /^Show the next 20 · (\d+) left$/;

/** The Legion daemon's name, wherever a row or an entry names its session
 *  (`legion-daemon:<PROJECT>`): these words, a middle dot, and the project's key (`daemonName`).
 *  The page never draws the session's id there. */
export const DAEMON_WORDS = "Legion daemon";

/** The Legion daemon's whole name on `project`'s issues. */
export function daemonName(project: string): string {
  return `${DAEMON_WORDS} · ${project}`;
}

/** The words of a record's kind (R7), which `[data-thread-kind]` holds whole. The kind is read
 *  from the record's root entry: the thread's root, or for a record that a reply with a quote
 *  begins, that reply. A message whose root entry targets an agent reads `Message to <agent>`:
 *  the role's token for `role:<token>`, and for `session:<id>` that session's name as the row's
 *  who-is-in-it names it. */
export const KIND_WORDS = {
  ask: "Question",
  comment: "Comment",
  message: "Message",
  messageTo: "Message to",
  text: "Comment on text",
} as const;

/** The words a row and an opened record draw in `[data-thread-gone]` for a record on a node that
 *  was deleted in Graph (R7, R8). Owed until dispatch://INFRA-1253's change adds these words to
 *  `page-words-threads.ts` with the component that draws them. */
export const GONE_NODE = "On text deleted in Graph";

/** From 1024 px up, with no record and no New thread open, the main column's one line (R2): a
 *  `p` inside `[data-thread-main]` holding exactly these words. */
export const MAIN_LINE = "Open a thread from the list, or start one with New thread.";

/** Below 1024 px, the `button` above an opened record and above New thread, named exactly this,
 *  which returns to the list (R2). */
export const BACK_TO_LIST = "Back to the list";

/** The line the main column draws for `/threads/<id>` whose id names no record of the issue
 *  (R12): a `p` inside `[data-thread-main]` holding exactly these words. */
export const NOT_FOUND_LINE = "This issue holds no thread at this address.";

/** New thread's chooser (R10): a `listbox` named exactly this. Each party is an `option` whose
 *  accessible name starts with the party's title and holds its detail, and which carries
 *  `data-thread-party` with the party's target (`role:<token>` or `session:<id>`). The chosen
 *  option carries `aria-selected="true"`. */
export const CHOOSER = "Agent";

/** The chooser's filter: a `textbox` named exactly this, with the same words as its placeholder.
 *  The chooser lists only the parties whose title or detail holds what it holds. */
export const CHOOSER_FILTER = "Filter agents";

/** New thread's box: a `textbox` named exactly this, which holds the caret once New thread opens.
 *  It offers no `@` (R11). */
export const NEW_THREAD_BOX = "Message";

/** The box of an opened record (R9), at its foot or under an entry: a `textbox` named exactly
 *  this. A comment record's and a settled ask's box is a `MentionComposer` box with the picker,
 *  and a message record's box offers no `@`. */
export const REPLY_BOX = "Reply";

/** Each box's `button` that sends, named exactly this. In New thread it is disabled until an
 *  agent is chosen and the box holds words. */
export const SEND = "Send";

/** Each entry's control that opens a box under it: a `button` named exactly this. */
export const REPLY = "Reply";

/** The control on the reader's own comment that opens its words to edit: a `button` named
 *  exactly this. No message carries it. */
export const EDIT = "Edit";

/** The selectors the rows read where no role names the element. */
export const SELECTORS = {
  /** The one element that holds the four choices. */
  choices: "[data-thread-choices]",
  /** The list column: the choices, the rows in their order, the control under them and New
   *  thread. From 1024 px up it stands at the left of the main column. Below 1024 px it fills the
   *  column, and an opened record or New thread stands in its place. */
  list: "[data-thread-list]",
  /** One row, whose value is its record's id: the ask's id, the id of a comment thread's first
   *  comment or of a message thread's root, or the id of the reply with a quote that begins the
   *  record. It holds exactly one `a`, whose `href` is `/issues/<KEY>/threads/<record id>`. */
  row: "[data-thread-row]",
  /** The row's kind line, holding the words of `KIND_WORDS` whole. */
  kind: "[data-thread-kind]",
  /** The row's first line: the root entry's text up to its first line break, cut in script at a
   *  word with `…` (U+2026) where it is longer than the row. No `text-overflow` and no
   *  `line-clamp` cut it, and it does not scroll. */
  firstLine: "[data-thread-first-line]",
  /** One author of the record, each once, in the order of their first entry: a person by login,
   *  an agent by its role (`agentRoleWord` of its session's title), the Legion daemon by
   *  `daemonName`. */
  person: "[data-thread-person]",
  /** The row's last activity: the `time` inside `[data-thread-activity]` whose `datetime` is
   *  the record's last activity (`Timestamp`). */
  activity: "[data-thread-activity] time[datetime]",
  /** The row's count of new writes (R7): its text is the number, or the number and `+`, such as
   *  `3+`. A row with no counted write draws no such element. */
  mark: "[data-thread-mark]",
  /** The words of `GONE_NODE`, on a row and on an opened record. */
  gone: "[data-thread-gone]",
  /** The main column: an opened record, New thread, or one line. */
  main: "[data-thread-main]",
  /** The opened record, whose value is the record's id. */
  record: "[data-thread-record]",
  /** Above a record on text's root, the words it quotes: the anchor's quote, or the quote of the
   *  reply that begins the record. */
  quote: "[data-thread-quote]",
  /** One entry of the opened record, whose value is the entry's id and whose `data-depth` is its
   *  depth: 0 for the record's root, and one more than the entry it answers for a reply. Entries
   *  are siblings in document order, each reply after the entry it answers and that entry's
   *  earlier replies, and each reply stands to the right of the entry it answers. Each holds its
   *  author's name, a `time` whose `datetime` is its creation, its whole words, and `REPLY`. An
   *  ask's root is the ask body (`[data-ask-body]`), and its replies are entries. */
  entry: "[data-thread-entry]",
  /** One box of the opened record or of New thread: `foot` for the box at a record's foot, the
   *  id of the entry it answers for a box under an entry, and `new` for New thread's box. */
  box: "[data-thread-box]",
  /** New thread, in the main column, with its chooser and its box. */
  newThread: "[data-new-thread]",
  /** A record on text's link to its node where a view draws that node. Owed until
   *  dispatch://INFRA-1253's node anchor is on the base. */
  nodeLink: "[data-thread-node-link]",
} as const;

/** `/issues/<key>/threads`, and with `id` the record's own address. */
export function threadsPath(key: string, id?: string): string {
  return id === undefined ? `/issues/${key}/threads` : `/issues/${key}/threads/${id}`;
}

/** The view row's Threads toggle, from 1280 px up. */
export function threadsToggle(page: Page): Locator {
  return presetRow(page).getByRole("button", { exact: true, name: THREADS });
}

/** The one control's menu item for Threads, below 1280 px, while the menu is open. */
export function threadsMenuItem(page: Page): Locator {
  return openMenu(page).getByRole("menuitemradio", { name: new RegExp(`^${THREADS}( |$)`) });
}

/** The count the Threads control draws, in either shape of the row. */
export function threadsCount(page: Page): Locator {
  return presetRow(page).locator(`[data-preset-count="${THREADS_TOKEN}"]`);
}

/** The Threads view. */
export function threadsView(page: Page): Locator {
  return issuePage(page).getByRole("region", { exact: true, name: THREADS });
}

/** The list column. */
export function threadsList(page: Page): Locator {
  return threadsView(page).locator(SELECTORS.list);
}

/** The main column. */
export function threadsMain(page: Page): Locator {
  return threadsView(page).locator(SELECTORS.main);
}

/** One choice's button. */
export function choiceButton(page: Page, choice: Choice): Locator {
  return threadsView(page)
    .locator(SELECTORS.choices)
    .getByRole("button", { exact: true, name: CHOICES[choice] });
}

/** Every row the list draws, in order. */
export function threadRows(page: Page): Locator {
  return threadsList(page).locator(SELECTORS.row);
}

/** The row of the record `id`. */
export function threadRow(page: Page, id: string): Locator {
  return threadsList(page).locator(`${SELECTORS.row}[data-thread-row="${id}"]`);
}

/** The control under the rows. */
export function showNextControl(page: Page): Locator {
  return threadsList(page).getByRole("button", { name: SHOW_NEXT_NAME });
}

/** The opened record. */
export function openedRecord(page: Page): Locator {
  return threadsView(page).locator(SELECTORS.record);
}

/** The entry `id` of the opened record. */
export function recordEntry(page: Page, id: string): Locator {
  return openedRecord(page).locator(`${SELECTORS.entry}[data-thread-entry="${id}"]`);
}

/** The box `which` of the opened record: `foot`, or the id of the entry it answers. */
export function recordBox(page: Page, which: string): Locator {
  return openedRecord(page).locator(`${SELECTORS.box}[data-thread-box="${which}"]`);
}

/** New thread, opened. */
export function newThreadPanel(page: Page): Locator {
  return threadsView(page).locator(SELECTORS.newThread);
}

/** The control that returns to the list below 1024 px. */
export function backToList(page: Page): Locator {
  return threadsView(page).getByRole("button", { exact: true, name: BACK_TO_LIST });
}

// =============================================================================================
// The seed
// =============================================================================================

/** The person every row signs in as: `phase` (`e2e/issue-page.ts`) signs alice in. */
export const READER = "alice";

/** Another person, who begins two message threads and replies to the reader's post. */
export const OTHER = "bob";

/** The agent that plans the issue: it replies, asks, writes a note and writes on the spec. */
export const PLANNER: Actor = {
  id: "threads-planner",
  kind: "session",
  origin: { session_title: "Planner (threads)" },
};

/** The agent that reviews: it begins the thread between agents and replies. */
export const REVIEWER: Actor = {
  id: "threads-reviewer",
  kind: "session",
  origin: { session_title: "Reviewer (threads)" },
};

/** The agent that answers one block of the reader's message with a quote. */
export const TESTER: Actor = {
  id: "threads-tester",
  kind: "session",
  origin: { session_title: "Tester (threads)" },
};

/** The bearer options that write as `actor`. */
function as(actor: Actor) {
  return { actor, as: "agent" as const };
}

/** The Legion project token the fake registry's entries are written under (`roleToken`). */
const LEGION_TOKEN = "threadslegion";

/** The sentences of the seeded issue's spec that its comments on text quote. */
const QUOTES = {
  agentText: "counts what is new",
  filler: [
    "reads three routes",
    "names who is in the thread",
    "opens the whole thread",
    "keeps the pages it drew",
  ],
  text: "merges them by last activity",
} as const;

const SPEC_TEXT = [
  ":::requirement{#r1}",
  "The list reads three routes and merges them by last activity.",
  ":::",
  "",
  ":::requirement{#r2}",
  "Each row names who is in the thread and counts what is new.",
  ":::",
  "",
  ":::requirement{#r3}",
  "A press opens the whole thread, and the list keeps the pages it drew.",
  ":::",
  "",
].join("\n");

/** The first line of the second message thread's root: longer than a row at every width, so the
 *  row cuts it at a word with an ellipsis (R7). */
export const LONG_FIRST_LINE =
  "This first line runs well past the width of any row the list can draw, because a row shows " +
  "the first line of its root and cuts it at a word with an ellipsis where the line is longer " +
  "than the row, so a reader sees where the words stop and that more of them follow in the thread.";

/** What one filler of the seed is: a comment the reader writes, a comment the planner writes on
 *  the spec's text, a question the planner asks and the reader answers, or a message the reader
 *  sends to the planner's role. */
type Filler = "ask" | "comment" | "message" | "text";

/** The fillers written before the main threads, between them and the reader's last read of the
 *  issue, and after: the three routes interleave in time, so no route's first page holds All's
 *  first 20 threads alone, All draws three pages, and the planner's comments on text stand on
 *  All's second page (row 12). */
const FILLERS = {
  after: ["message", "comment", "ask", "comment", "text", "message", "comment", "ask", "comment"],
  before: [
    "comment",
    "ask",
    "message",
    "comment",
    "ask",
    "message",
    "comment",
    "ask",
    "message",
    "comment",
    "text",
    "message",
    "comment",
    "ask",
    "comment",
    "text",
    "message",
    "ask",
    "comment",
    "message",
  ],
  between: ["comment", "ask", "message", "text", "comment", "ask", "message", "comment", "ask"],
} as const satisfies Record<string, readonly Filler[]>;

/** What `seedThreads` wrote, by the ids the rows read. */
export interface ThreadsSeed {
  readonly key: string;
  readonly project: string;
  /** The second issue, where more than 200 events stand after the reader's last read. */
  readonly second: { readonly key: string; readonly note: string } | null;
  readonly messages: {
    /** The first message thread: the reader's message to the planner's role. */
    readonly first: string;
    /** The planner's reply to it, and the reader's reply to that reply. */
    readonly firstAgentReply: string;
    readonly firstPersonReply: string;
    /** The planner's two replies after the reader last read the thread. */
    readonly firstLate: readonly [string, string];
    /** The second: the reader began it and wrote last, after the first's last reply. */
    readonly second: string;
    /** The third: another person began it, and the agent replied after the reader's last read. */
    readonly third: string;
    /** The fourth: another person began it, and the reader wrote last. */
    readonly fourth: string;
    /** The agent's note: a root with no target that no person answered. */
    readonly note: string;
    /** The thread between agents, and the planner's reply in it. */
    readonly agents: string;
    readonly agentsReply: string;
    /** The thread the Legion daemon began. */
    readonly daemon: string;
    /** The reader's message whose block the tester answers with a quote, and that reply. */
    readonly quoting: string;
    readonly quotingReply: string;
  };
  readonly comments: {
    /** The reader's remark, with the planner's and the daemon's replies. */
    readonly remark: string;
    /** The reader's comment on the spec's text, the planner's reply, and the reviewer's reply
     *  with a quote of that reply, which joins the record on text. */
    readonly text: string;
    readonly textReply: string;
    readonly textQuote: string;
    /** The reviewer's comment on the spec's text that no person wrote in. */
    readonly agentText: string;
    /** The two comments agents wrote to each other with no anchor. */
    readonly agentsTalk: string;
    /** The planner's status note with no anchor. */
    readonly status: string;
    /** The reader's post, the other person's reply, the planner's reply with a quote of the
     *  post's second block, and the reviewer's comment that answers that reply. */
    readonly post: string;
    readonly postReply: string;
    readonly postQuote: string;
    readonly postAnswer: string;
    /** A reply the reader wrote and deleted. */
    readonly deleted: string;
    /** The oldest comment thread but the post: it stands past the comment route's first page. */
    readonly oldestFiller: string;
  };
  readonly asks: {
    /** The open question that waits on the reader, and the reader's reply in its thread. */
    readonly open: string;
    readonly openReply: string;
    /** The settled question with replies. */
    readonly settled: string;
  };
  /** Every name and key a row may draw: the people's logins, each agent's title and role word,
   *  the project's key and the issues' keys. */
  readonly names: readonly string[];
}

/** One filler, as `FILLERS` names it, the `index`th of the seed. Answers its thread's id. */
async function writeFiller(key: string, kind: Filler, index: number): Promise<string> {
  const n = index + 1;
  switch (kind) {
    case "comment":
      return (await createComment(key, { body: `Filler comment ${n} on the list.` })).id;
    case "text":
      return (
        await createComment(
          key,
          {
            anchor: { artifact: "spec", quote: QUOTES.filler[index % QUOTES.filler.length] ?? "" },
            body: `Filler note ${n} on the spec's words.`,
          },
          as(PLANNER)
        )
      ).id;
    case "ask": {
      const ask = await createAsk(key, { question: `Filler question ${n}?` }, as(PLANNER));
      await answerAsk(ask.id, {
        expected_edited_at: null,
        selected: [],
        text: `Filler answer ${n}.`,
      });
      return ask.id;
    }
    case "message":
      return (
        await createMessage(key, {
          body: `Filler message ${n} to the planner.`,
          delivery: "aside",
          target: "role:planner",
        })
      ).id;
  }
}

/** Writes `kinds` in order, numbered from `from`. Answers each thread's id. */
async function writeFillers(
  key: string,
  kinds: readonly Filler[],
  from: number
): Promise<string[]> {
  const ids: string[] = [];
  for (const [at, kind] of kinds.entries()) ids.push(await writeFiller(key, kind, from + at));
  return ids;
}

/** How many events the second issue holds after the reader's last read of it: more than the
 *  200 newest the events read answers (R7's "+"). */
const PAST_TWO_HUNDRED = 205;

/** The second issue: an agent's note the reader last read before more than 200 events, then a
 *  thread of agents alone with no anchor that writes those events, then a reply in the note's
 *  thread, which the reader did not write in. */
async function seedSecondIssue(
  project: string
): Promise<{ readonly key: string; readonly note: string }> {
  const issue = await createIssue({ project, title: "Threads past the newest 200 events" });
  const note = await createMessage(
    issue.key,
    { body: "The second issue's note to its people." },
    as(PLANNER)
  );
  await putIssueState(issue.key, { last_read_seq: (await getIssue(issue.key)).last_seq });
  const root = await createComment(
    issue.key,
    { body: "The event log of this issue." },
    as(REVIEWER)
  );
  for (let at = 1; at < PAST_TWO_HUNDRED; at += 10) {
    await Promise.all(
      Array.from({ length: Math.min(10, PAST_TWO_HUNDRED - at) }, (_unused, step) =>
        createComment(
          issue.key,
          { body: `Event ${at + step} of the log.`, reply_to: root.id },
          as(REVIEWER)
        )
      )
    );
  }
  await createMessage(
    issue.key,
    { body: "A reply after the newest 200 events began.", in_reply_to: note.id },
    as(REVIEWER)
  );
  return { key: issue.key, note: note.id };
}

/**
 * The Verification document's "The seed", with its quoting message reply (R8), written through the
 * routes a person and an agent write through, in an order that sets each thread's last activity
 * and the reader's last read of the issue where the rows need them. The project's key is fresh,
 * as `mainHolds` in `e2e/dashboard-seed.ts` makes one, so no row depends on another's records.
 * With `second`, the second issue too (rows 4 and 10).
 */
export async function seedThreads(
  options: { readonly second?: boolean } = {}
): Promise<ThreadsSeed> {
  const project = `P${randomUUID().replaceAll("-", "").slice(0, 8).toUpperCase()}`;
  const daemon: Actor = { id: `legion-daemon:${project}`, kind: "session" };
  await createProject({ key: project, name: "Threads" });
  const issue = await createIssue({ project, spec: SPEC_TEXT, title: "Threads of one issue" });
  const key = issue.key;

  // The post comes first, so its thread is the oldest comment thread and stands past the comment
  // route's first page (row 15 looks a reply that begins a record up). The planner's reply quotes
  // the post's second block, and the reviewer answers that reply.
  const post = await createComment(key, {
    body: "The list reads three routes.\n\nEach route answers twenty threads at a time.",
  });
  const postReply = await createComment(
    key,
    { body: "Twenty is the size the server holds to.", reply_to: post.id },
    { login: OTHER }
  );
  const postQuote = await replyToCommentBlock(
    key,
    post.id,
    "b2",
    { body: "Why twenty and not fifty?", occurrence: 0, quote: "twenty threads" },
    as(PLANNER)
  );
  const postAnswer = await createComment(
    key,
    { body: "Fifty would not fit the first screen.", reply_to: postQuote.id },
    as(REVIEWER)
  );

  // The first filler is a comment of the reader's, and a reply the reader writes and deletes
  // follows it at once: the server counts a deleted entry's time in its thread's last activity,
  // so the thread stays the oldest comment thread but the post, past the route's first page.
  const oldestFiller = await writeFiller(key, FILLERS.before[0], 0);
  const deleted = await createComment(key, {
    body: "A reply the reader takes back.",
    reply_to: oldestFiller,
  });
  await deleteComment(deleted.id);
  let written = 1;
  written += (await writeFillers(key, FILLERS.before.slice(1), written)).length;

  // The threads no person wrote in, and the agents' posts with no anchor.
  const note = await createMessage(
    key,
    { body: "The planner reads the spec before it writes the plan." },
    as(PLANNER)
  );
  const agentsTalk = await createComment(
    key,
    { body: "The plan needs one more task." },
    as(REVIEWER)
  );
  await createComment(key, { body: "Added it.", reply_to: agentsTalk.id }, as(PLANNER));
  const status = await createComment(key, { body: "Status: the plan is written." }, as(PLANNER));
  const agents = await createMessage(
    key,
    { body: "Which reads does the list send?", delivery: "aside", target: `session:${PLANNER.id}` },
    as(REVIEWER)
  );
  const agentsReply = await createMessage(
    key,
    { body: "Three pages and the events.", in_reply_to: agents.id },
    as(PLANNER)
  );
  const agentText = await createComment(
    key,
    {
      anchor: { artifact: "spec", quote: QUOTES.agentText },
      body: "This count needs the events read.",
    },
    as(REVIEWER)
  );

  // The settled question with replies.
  const settled = await createAsk(
    key,
    { question: "Does the list keep its pages on a refresh?" },
    as(PLANNER)
  );
  await answerAsk(settled.id, {
    expected_edited_at: null,
    selected: [],
    text: "Yes, every page stays.",
  });
  await createComment(key, { ask_id: settled.id, body: "Noted." }, as(PLANNER));
  await createComment(key, { ask_id: settled.id, body: "Thank you." });

  // The fourth message thread: the other person began it, and the reader wrote last.
  const fourth = await createMessage(
    key,
    { body: "Does Agents list the agents' notes?", delivery: "aside", target: "role:planner" },
    { login: OTHER }
  );
  const fourthReply = await createMessage(
    key,
    { body: "No, only the threads between agents.", in_reply_to: fourth.id },
    as(PLANNER)
  );
  await createMessage(key, {
    body: "Good, that keeps it short.",
    in_reply_to: fourthReply.id,
  });

  // The reader's comment on the spec's text, the planner's reply, and the reviewer's reply that
  // quotes that reply, which joins the record on text (R8).
  const text = await createComment(key, {
    anchor: { artifact: "spec", quote: QUOTES.text },
    body: "Is a tie broken by the id?",
  });
  const textReply = await createComment(
    key,
    { body: "Yes, the larger id comes first.", reply_to: text.id },
    as(PLANNER)
  );
  const textQuote = await replyToCommentBlock(
    key,
    textReply.id,
    "b1",
    { body: "Which id, the root's?", occurrence: 0, quote: "larger id" },
    as(REVIEWER)
  );

  // The first message thread, up to the reader's reply.
  const first = await createMessage(key, {
    body: "Can the list show who is in each thread?",
    delivery: "aside",
    target: "role:planner",
  });
  const firstAgentReply = await createMessage(
    key,
    { body: "Yes, each author once.", in_reply_to: first.id },
    as(PLANNER)
  );
  const firstPersonReply = await createMessage(key, {
    body: "In the order they first wrote?",
    in_reply_to: firstAgentReply.id,
  });

  // The open question that waits on the reader, whose thread the reader wrote in.
  const open = await createAsk(
    key,
    { question: "Should Waiting on me leave out the daemon's threads?" },
    as(PLANNER)
  );
  const openReply = await createComment(key, {
    ask_id: open.id,
    body: "Which threads does it begin?",
  });

  // The third message thread, begun by the other person.
  const third = await createMessage(
    key,
    { body: "How many rows does a page draw?", delivery: "aside", target: "role:reviewer" },
    { login: OTHER }
  );

  const between = await writeFillers(key, FILLERS.between, written);
  written += between.length;

  // The reader's last read of the issue.
  await putIssueState(key, { last_read_seq: (await getIssue(key)).last_seq });

  await createMessage(key, { body: "Twenty rows.", in_reply_to: third.id }, as(REVIEWER));
  await createComment(key, { ask_id: open.id, body: "The ones its session begins." }, as(PLANNER));
  const firstLate = [
    (
      await createMessage(
        key,
        { body: "Yes, in that order.", in_reply_to: firstPersonReply.id },
        as(PLANNER)
      )
    ).id,
    (
      await createMessage(
        key,
        { body: "And each name once.", in_reply_to: firstPersonReply.id },
        as(PLANNER)
      )
    ).id,
  ] as const;

  // The Legion daemon's thread, which the reader replied in and an agent replied after.
  const daemonRoot = await createMessage(key, { body: "Planning started." }, as(daemon));
  await createMessage(key, { body: "Thank you.", in_reply_to: daemonRoot.id });
  await createMessage(
    key,
    { body: "The plan is on its way.", in_reply_to: daemonRoot.id },
    as(PLANNER)
  );

  // The reader's remark, with the planner's reply and the daemon's.
  const remark = await createComment(key, {
    body: "The list should name the daemon by its project.",
  });
  await createComment(
    key,
    { body: "It names it by its words and the key.", reply_to: remark.id },
    as(PLANNER)
  );
  await createComment(key, { body: "Noted.", reply_to: remark.id }, as(daemon));

  // The second message thread: the reader wrote last, after the first thread's last reply.
  const second = await createMessage(key, {
    body: `${LONG_FIRST_LINE}\n\nThe rest of the message.`,
    delivery: "aside",
    target: "role:reviewer",
  });
  const secondReply = await createMessage(
    key,
    { body: "Agreed.", in_reply_to: second.id },
    as(REVIEWER)
  );
  await createMessage(key, { body: "Then I will write it so.", in_reply_to: secondReply.id });

  // The reader's message whose second block the tester answers with a quote (R8).
  const quoting = await createMessage(key, {
    body: "The row draws its first line.\n\nThe line is cut at a word with an ellipsis.",
    delivery: "aside",
    target: "role:tester",
  });
  const quotingReply = await replyToMessageBlock(
    key,
    quoting.id,
    "b2",
    { body: "Cut where, at the row's edge?", occurrence: 0, quote: "cut at a word" },
    as(TESTER)
  );

  await writeFillers(key, FILLERS.after, written);

  const secondIssue = options.second === true ? await seedSecondIssue(project) : null;
  const titles = [PLANNER, REVIEWER, TESTER].map((agent) => agent.origin?.session_title ?? "");
  return {
    asks: { open: open.id, openReply: openReply.id, settled: settled.id },
    comments: {
      agentText: agentText.id,
      agentsTalk: agentsTalk.id,
      deleted: deleted.id,
      oldestFiller,
      post: post.id,
      postAnswer: postAnswer.id,
      postQuote: postQuote.id,
      postReply: postReply.id,
      remark: remark.id,
      status: status.id,
      text: text.id,
      textQuote: textQuote.id,
      textReply: textReply.id,
    },
    key,
    messages: {
      agents: agents.id,
      agentsReply: agentsReply.id,
      daemon: daemonRoot.id,
      first: first.id,
      firstAgentReply: firstAgentReply.id,
      firstLate,
      firstPersonReply: firstPersonReply.id,
      fourth: fourth.id,
      note: note.id,
      quoting: quoting.id,
      quotingReply: quotingReply.id,
      second: second.id,
      third: third.id,
    },
    names: [
      READER,
      OTHER,
      ...titles,
      ...titles.map(agentRoleWord),
      "planner",
      "reviewer",
      "tester",
      project,
      key,
      ...(secondIssue === null ? [] : [secondIssue.key]),
      DAEMON_WORDS,
    ],
    project,
    second: secondIssue,
  };
}

/** The planner as a live session holding its role, which takes every delivery mode, as the
 *  fake Envoy registry lists it (`setLiveSessions`). */
export const LIVE_PLANNER: FakeSession = {
  capabilities: ["steer", "aside", "btw"],
  dir: "/workspaces/planner",
  roles: ["planner"],
  session_id: PLANNER.id,
  title: PLANNER.origin?.session_title ?? PLANNER.id,
};

/** The parties of New thread's chooser and of the `@` picker: the planner live, holding its
 *  role, and the reviewer's Legion role on the issue, which no live session holds (R10). The
 *  registry is the fake Envoy's, which the harness gives every row. */
export async function seedParties(seed: ThreadsSeed): Promise<void> {
  await setLiveSessions([{ ...LIVE_PLANNER, last_seen: Date.now() - 5_000 }]);
  await setLegionAgents([
    legionAgentEntry({
      issue: seed.key,
      legionProject: LEGION_TOKEN,
      role: "reviewer",
      sessionId: REVIEWER.id,
      startedAt: new Date(Date.now() - 60_000).toISOString(),
    }),
  ]);
}

// =============================================================================================
// What the server holds, read as the view reads it
// =============================================================================================

/** One GET under the reader's session, its answer read as `T`. */
export async function readAs<T>(path: string, login: string = READER): Promise<T> {
  const response = await fetch(new URL(path, baseUrl), { headers: await userHeaders(login) });
  if (!response.ok) {
    throw new Error(`GET ${path} failed: ${response.status} ${await response.text()}`);
  }
  return (await response.json()) as T;
}

/** The three routes All and Mine read (R3, R4); Agents reads `messages` alone (R5). */
export type Route = "asks" | "comments" | "messages";

/** One entry of a record: a comment, a message, an ask, or an ask's answer or resolution. */
export interface Entry {
  readonly id: string;
  readonly author: Actor;
  readonly at: string;
  /** The entry this one answers, or null for the thread's root. */
  readonly answers: string | null;
  readonly body: string;
  /** The words of the block it quotes, for a reply with a quote (`reply_block.quote`). */
  readonly quote: string | null;
  readonly target: string | null;
  /** Whether the server's triggers count its writer as one who wrote in the thread: every entry
   *  but an ask's resolution (R7). */
  readonly joins: boolean;
}

/** One record of R8: a thread, or the part of it that a reply with a quote begins. */
export interface ThreadRecord {
  /** The record's id: its address `/threads/<id>` and its row's `data-thread-row`. */
  readonly id: string;
  readonly threadId: string;
  readonly route: Route;
  readonly kind: "ask" | "comment" | "message";
  readonly onText: boolean;
  /** The words quoted above its root, for a record on text. */
  readonly quote: string | null;
  readonly root: Entry;
  /** Every entry of the record, oldest first, its root among them. */
  readonly entries: readonly Entry[];
  /** The ids an event names for an entry of the record (R7). */
  readonly ids: ReadonlySet<string>;
  /** The newest time of the record's own entries; for a whole thread, its `last_activity_at`. */
  readonly lastActivity: string;
}

/** One thread of a route's page, with its records. */
export interface ServerThread {
  readonly id: string;
  readonly route: Route;
  readonly lastActivity: string;
  /** Its records, newest first, as All draws them together at the thread's place (R3). */
  readonly records: readonly ThreadRecord[];
  /** Every entry of the thread, oldest first. */
  readonly entries: readonly Entry[];
  /** Whether it is an open ask that waits on the reader (R7). */
  readonly waitsOnReader: boolean;
}

/** One page of a route, as the view reads it. */
export interface RoutePage {
  readonly threads: readonly ServerThread[];
  readonly before: string | null;
  readonly remaining: number;
}

/** The text of a reply's quote, or null where it quotes nothing. */
function quoteOf(block: { readonly quote?: string } | null): string | null {
  const quote = block?.quote ?? "";
  return quote === "" ? null : quote;
}

/** The records of one thread by R8's rule (decision 469): a reply with a quote begins a record of
 *  its own unless the entry it answers is in a record on text already, which it then joins, and
 *  every entry that answers an entry of a record is in that record. */
function recordsOf(
  thread: Omit<ServerThread, "records" | "entries" | "waitsOnReader">,
  kind: ThreadRecord["kind"],
  root: Entry,
  replies: readonly Entry[],
  rootQuote: string | null
): { records: ThreadRecord[]; entries: Entry[] } {
  const entries = [root, ...[...replies].sort((one, two) => compareTimestamps(one.at, two.at))];
  const built = new Map<string, { entries: Entry[]; onText: boolean; quote: string | null }>();
  const recordOf = new Map<string, string>();
  built.set(root.id, { entries: [root], onText: rootQuote !== null, quote: rootQuote });
  recordOf.set(root.id, root.id);
  for (const entry of entries.slice(1)) {
    const parent = recordOf.get(entry.answers ?? root.id) ?? root.id;
    const parentRecord = built.get(parent);
    if (entry.quote !== null && parentRecord?.onText !== true) {
      built.set(entry.id, { entries: [entry], onText: true, quote: entry.quote });
      recordOf.set(entry.id, entry.id);
    } else {
      parentRecord?.entries.push(entry);
      recordOf.set(entry.id, parent);
    }
  }
  const whole = built.size === 1;
  const records = [...built].map(([id, record]): ThreadRecord => {
    const newest = record.entries.reduce(
      (latest, entry) => (compareTimestamps(entry.at, latest) > 0 ? entry.at : latest),
      record.entries[0]?.at ?? thread.lastActivity
    );
    return {
      entries: record.entries,
      id,
      ids: new Set(record.entries.map((entry) => entry.id)),
      kind,
      lastActivity: whole ? thread.lastActivity : newest,
      onText: record.onText,
      quote: record.quote,
      root: record.entries[0] ?? root,
      route: thread.route,
      threadId: thread.id,
    };
  });
  records.sort(
    (one, two) =>
      compareTimestamps(two.lastActivity, one.lastActivity) || (two.id > one.id ? 1 : -1)
  );
  return { entries, records };
}

function commentEntry(comment: CommentWithTrees, root: string | null): Entry {
  return {
    answers: root === null ? null : (comment.answers ?? root),
    at: comment.created_at,
    author: comment.author,
    body: comment.body,
    id: comment.id,
    joins: true,
    quote: quoteOf(comment.reply_block),
    target: null,
  };
}

function messageEntry(message: MessageWithTrees): Entry {
  return {
    answers: message.in_reply_to,
    at: message.created_at,
    author: message.author,
    body: message.body,
    id: message.id,
    joins: true,
    quote: quoteOf(message.reply_block),
    target: message.target,
  };
}

/** The query a page read of the comments or messages route takes. */
interface PageQuery {
  readonly author?: string;
  readonly before?: string;
  readonly person?: "in" | "none";
}

/** One page of `route` of `key`, as All, Mine and Agents read it: `order=activity`, `limit=20`,
 *  and for comments and messages `person` (R3 to R5); asks carry `thread=1`. */
export async function readRoutePage(
  key: string,
  route: Route,
  query: PageQuery,
  reader: string = READER
): Promise<RoutePage> {
  const shared = { author: query.author, before: query.before, limit: 20, order: "activity" };
  if (route === "comments") {
    const page = await readAs<CommentThreadPage>(
      routeUrl("/api/v1/issues/{key}/comments", { key }, { ...shared, person: query.person }),
      reader
    );
    return {
      before: page.before,
      remaining: page.remaining,
      threads: (page.threads ?? []).map((row) => {
        const thread = { id: row.comment.id, lastActivity: row.last_activity_at, route };
        const built = recordsOf(
          thread,
          "comment",
          commentEntry(row.comment, null),
          (row.replies ?? []).map((reply) => commentEntry(reply, row.comment.id)),
          row.comment.anchor?.quote ?? null
        );
        return { ...thread, ...built, waitsOnReader: false };
      }),
    };
  }
  if (route === "messages") {
    const page = await readAs<MessageThreadPage>(
      routeUrl("/api/v1/issues/{key}/messages", { key }, { ...shared, person: query.person }),
      reader
    );
    return {
      before: page.before,
      remaining: page.remaining,
      threads: (page.threads ?? []).map((row) => {
        const thread = { id: row.message.id, lastActivity: row.last_activity_at, route };
        const built = recordsOf(
          thread,
          "message",
          messageEntry(row.message),
          (row.replies ?? []).map(messageEntry),
          null
        );
        return { ...thread, ...built, waitsOnReader: false };
      }),
    };
  }
  const page = await readAs<AskThreadPage>(
    routeUrl("/api/v1/issues/{key}/asks", { key }, { ...shared, thread: "1" }),
    reader
  );
  return {
    before: page.before,
    remaining: page.remaining,
    threads: (page.threads ?? []).map((row) => {
      const thread = { id: row.id, lastActivity: row.last_activity_at, route };
      const root: Entry = {
        answers: null,
        at: row.created_at,
        author: row.author,
        body: row.question,
        id: row.id,
        joins: true,
        quote: null,
        target: null,
      };
      const replies: Entry[] = (row.thread?.replies ?? []).map((reply) =>
        commentEntry(reply, row.id)
      );
      if (row.answer !== null) {
        replies.push({
          answers: row.id,
          at: row.answer.at,
          author: { id: row.answer.user, kind: "user" },
          body: row.answer.text ?? "",
          id: `${row.id}#answer`,
          joins: true,
          quote: null,
          target: null,
        });
      }
      if (row.resolution !== undefined) {
        replies.push({
          answers: row.id,
          at: row.resolution.at,
          author: row.resolution.actor,
          body: row.resolution.reason,
          id: `${row.id}#resolution`,
          joins: false,
          quote: null,
          target: null,
        });
      }
      const built = recordsOf(thread, "ask", root, replies, row.anchor?.quote ?? null);
      return {
        ...thread,
        ...built,
        waitsOnReader: row.state === "open" && row.waiting_on_person === reader,
      };
    }),
  };
}

/** One page a choice draws: its rows, and the number its control says is left. */
export interface ChoicePage {
  readonly rows: readonly ThreadRecord[];
  readonly left: number;
}

/** A route as a choice reads it: its first page, then the page from each `before`. */
type RouteReader = (before: string | undefined) => Promise<RoutePage>;

/** Orders records as All does (R3): the newest thread first, the larger id first on the same
 *  time, and a thread's records together at its place, the newest first. */
function allOrder(
  one: { readonly record: ThreadRecord; readonly thread: ServerThread },
  two: { readonly record: ThreadRecord; readonly thread: ServerThread }
): number {
  return (
    compareTimestamps(two.thread.lastActivity, one.thread.lastActivity) ||
    (two.thread.id === one.thread.id
      ? compareTimestamps(two.record.lastActivity, one.record.lastActivity) ||
        (two.record.id > one.record.id ? 1 : -1)
      : two.thread.id > one.thread.id
        ? 1
        : -1)
  );
}

/** Every page a choice that merges `routes` draws, by R3's rule: each route's first page read
 *  when the choice opens, the records `keep` holds merged in All's order, 20 rows a page, and
 *  before each next page, the next page of each route whose rows read and not drawn are fewer
 *  than 20 and whose `before` is not null. The number left after a page is the rows read and not
 *  drawn and each route's `remaining`. */
async function mergedPages(
  routes: readonly RouteReader[],
  keep: (record: ThreadRecord, thread: ServerThread) => boolean
): Promise<ChoicePage[]> {
  type Row = { readonly record: ThreadRecord; readonly thread: ServerThread };
  const state = await Promise.all(
    routes.map(async (read) => ({ page: await read(undefined), read, rows: [] as Row[] }))
  );
  const take = (route: (typeof state)[number]) => {
    for (const thread of route.page.threads) {
      for (const record of thread.records) {
        if (keep(record, thread)) route.rows.push({ record, thread });
      }
    }
  };
  for (const route of state) take(route);
  const drawn = new Set<string>();
  const pages: ChoicePage[] = [];
  for (;;) {
    const undrawn = state
      .flatMap((route) => route.rows)
      .filter((row) => !drawn.has(row.record.id))
      .sort(allOrder);
    const rows = undrawn.slice(0, 20).map((row) => row.record);
    for (const record of rows) drawn.add(record.id);
    const left =
      undrawn.length - rows.length + state.reduce((sum, route) => sum + route.page.remaining, 0);
    pages.push({ left, rows });
    if (left === 0 || rows.length === 0) return pages;
    for (const route of state) {
      const waiting = route.rows.filter((row) => !drawn.has(row.record.id)).length;
      if (waiting < 20 && route.page.before !== null) {
        route.page = await route.read(route.page.before);
        take(route);
      }
    }
  }
}

/** All's three routes, as the view reads them for `reader`, with `author` for Mine. */
function threeRoutes(key: string, author?: string): RouteReader[] {
  return [
    (before) => readRoutePage(key, "comments", { author, before, person: "in" }),
    (before) => readRoutePage(key, "messages", { author, before, person: "in" }),
    (before) => readRoutePage(key, "asks", { author, before }),
  ];
}

/** Every page of All (R3). */
export function allPages(key: string): Promise<ChoicePage[]> {
  return mergedPages(threeRoutes(key), () => true);
}

/** Every page of Mine (R4): the three pages with `author=` the reader, and of their records only
 *  those that hold an entry the reader wrote, left out before the rows are taken. */
export function minePages(key: string, reader: string = READER): Promise<ChoicePage[]> {
  return mergedPages(threeRoutes(key, reader), (record) =>
    record.entries.some((entry) => isViewer(entry.author, reader))
  );
}

/** Every page of Agents (R5): `messages?person=none` alone. */
export function agentsPages(key: string): Promise<ChoicePage[]> {
  return mergedPages(
    [(before) => readRoutePage(key, "messages", { before, person: "none" })],
    () => true
  );
}

/** Every thread of All, by its id, read from every page of the three routes. */
export async function allThreads(key: string): Promise<ReadonlyMap<string, ServerThread>> {
  const threads = new Map<string, ServerThread>();
  for (const route of threeRoutes(key)) {
    for (let page = await route(undefined); ; page = await route(page.before ?? undefined)) {
      for (const thread of page.threads) threads.set(thread.id, thread);
      if (page.before === null) break;
    }
  }
  return threads;
}

/** The first page of each of All's routes: the pages the view holds once All has opened. */
export async function allFirstPages(key: string): Promise<ReadonlySet<string>> {
  const pages = await Promise.all(threeRoutes(key).map((route) => route(undefined)));
  return new Set(pages.flatMap((page) => page.threads.map((thread) => thread.id)));
}

/** Whether `author` is the Legion daemon's session of `project` (R6, R13). */
export function isDaemon(author: Actor | undefined, project: string): boolean {
  return author?.kind === "session" && author.id === `legion-daemon:${project}`;
}

/** The rows of Waiting on me (R6): `me/replies?issue=<KEY>&state=unread` with no `limit`, less
 *  each row the Legion daemon began, in the route's order, and each page's records, 20 route
 *  rows a page, each row's records together, the newest first. */
export async function waitingPages(
  key: string,
  project: string
): Promise<{ readonly rows: readonly MyReplyRow[]; readonly pages: readonly ChoicePage[] }> {
  const threads = await allThreads(key);
  const rows = ((await listMyReplies({ issue: key, state: "unread" })).rows ?? []).filter(
    (row) => !isDaemon(row.root_author, project)
  );
  const pages: ChoicePage[] = [];
  for (let at = 0; at === 0 || at < rows.length; at += 20) {
    const page = rows.slice(at, at + 20);
    pages.push({
      left: rows.length - at - page.length,
      rows: page.flatMap((row) => threads.get(row.thread_id)?.records ?? []),
    });
  }
  return { pages, rows };
}

/** What the count of R7 is read from: the reader's unread threads, the reader's last read of the
 *  issue, and the issue's newest 200 events. */
export interface MarkReads {
  readonly reader: string;
  readonly replies: readonly MyReplyRow[];
  readonly lastReadSeq: number;
  readonly events: readonly DispatchEvent[];
}

/** R7's three reads, as the view sends them. */
export async function markReads(key: string, reader: string = READER): Promise<MarkReads> {
  const [replies, state, events] = await Promise.all([
    listMyReplies({ issue: key, state: "unread" }),
    getMyState(reader),
    getIssueEvents(key, { limit: 200, order: "desc" }),
  ]);
  return {
    events,
    lastReadSeq: state[key]?.last_read_seq ?? 0,
    reader,
    replies: replies.rows ?? [],
  };
}

/** The event types that are writes of an entry (R7): a comment, a delivered comment
 *  reply, a message, an agent's reply message, an ask, its answer and its resolution. An edit, a
 *  delivery and an acceptance name the same ids and are no writes. */
export const WRITE_EVENTS: Readonly<Record<string, true>> = {
  "ask.answered": true,
  "ask.opened": true,
  "ask.resolved": true,
  "comment.answered": true,
  "comment.created": true,
  "message.answered": true,
  "message.created": true,
};

/** The id of the entry an event names: an ask's, a comment's or a message's `payload.id`. */
function eventEntry(event: DispatchEvent): string | undefined {
  const payload: unknown = event.payload;
  if (typeof payload !== "object" || payload === null || !("id" in payload)) return undefined;
  return typeof payload.id === "string" ? payload.id : undefined;
}

/** The mark of R7 a row of `record` draws, or null for none: for a record whose thread the reader
 *  wrote in, its writes past the `read_through` of the thread's `me/replies` row, 0 where the read
 *  lists none; for any other record and for the open ask that waits on the reader, its writes
 *  whose events stand past `last_read_seq` among the newest 200, with "+" where that read holds
 *  200 events all past it. A write of the reader is never counted. */
export function markOf(
  record: ThreadRecord,
  thread: ServerThread,
  reads: MarkReads
): string | null {
  const wroteIn = thread.entries.some(
    (entry) => entry.joins && isViewer(entry.author, reads.reader)
  );
  if (wroteIn && !thread.waitsOnReader) {
    const row = reads.replies.find((one) => one.thread_id === record.threadId);
    if (row?.read_through === undefined) return null;
    const through = row.read_through;
    const count = record.entries.filter(
      (entry) => !isViewer(entry.author, reads.reader) && compareTimestamps(entry.at, through) > 0
    ).length;
    return count === 0 ? null : String(count);
  }
  const count = reads.events.filter((event) => {
    const entry = eventEntry(event);
    return (
      event.seq > reads.lastReadSeq &&
      WRITE_EVENTS[event.type] === true &&
      !isViewer(event.actor, reads.reader) &&
      entry !== undefined &&
      record.ids.has(entry)
    );
  }).length;
  const oldest = reads.events.reduce(
    (low, event) => Math.min(low, event.seq),
    Number.POSITIVE_INFINITY
  );
  const plus = reads.events.length === 200 && oldest > reads.lastReadSeq && oldest > 1;
  return count === 0 ? null : `${count}${plus ? "+" : ""}`;
}

/** The name the page gives an author (R7, root R9): a person by login, the Legion daemon by
 *  `daemonName`, and an agent by its role, as its session's title gives it. */
export function authorName(actor: Actor, project: string): string {
  if (isDaemon(actor, project)) return daemonName(project);
  if (actor.kind === "session") return agentRoleWord(actor.origin?.session_title ?? actor.id);
  return actor.id;
}

/** The name of the agent a message's target names, as the row names it. */
function targetName(target: string, thread: ServerThread, project: string): string {
  const party = partyOfAddress(target);
  if (party?.kind === "role") return party.role;
  const id = party?.kind === "agent-session" ? party.session : target;
  const author = thread.entries.find(
    (entry) => entry.author.kind === "session" && entry.author.id === id
  )?.author;
  return authorName(author ?? { id, kind: "session" }, project);
}

/** What one row draws (R7), as the server's records say it. */
export interface ExpectedRow {
  readonly id: string;
  readonly href: string;
  readonly kind: string;
  readonly firstLine: string;
  readonly people: readonly string[];
  readonly activity: string;
  readonly mark: string | null;
}

/** The row R7 draws for `record` of `thread` on issue `key` of `project`. */
export function expectedRow(
  record: ThreadRecord,
  thread: ServerThread,
  reads: MarkReads,
  key: string,
  project: string
): ExpectedRow {
  const target = record.root.target;
  const kind =
    record.kind === "ask"
      ? KIND_WORDS.ask
      : record.kind === "comment"
        ? record.onText
          ? KIND_WORDS.text
          : KIND_WORDS.comment
        : target === null
          ? KIND_WORDS.message
          : `${KIND_WORDS.messageTo} ${targetName(target, thread, project)}`;
  const people: string[] = [];
  for (const entry of record.entries) {
    const name = authorName(entry.author, project);
    if (!people.includes(name)) people.push(name);
  }
  return {
    activity: record.lastActivity,
    firstLine: (record.root.body.split("\n")[0] ?? "").trim(),
    href: threadsPath(key, record.id),
    id: record.id,
    kind,
    mark: markOf(record, thread, reads),
    people,
  };
}

/** The parties New thread's chooser lists (R10): the parties the `@` picker offers on `main`,
 *  read from the same two reads the picker sends. Answers each party's target and the delivery
 *  its capabilities give. */
export async function chooserParties(key: string): Promise<
  readonly {
    readonly target: string;
    readonly title: string;
    readonly capabilities: readonly string[];
  }[]
> {
  const agents: Agent[] = await listAgents();
  const collaborators = await readAs<IssueCollaborators>(
    routeUrl("/api/v1/issues/{key}/collaborators", { key })
  );
  const text = { sessionText: (id: string, title: string | undefined) => title ?? id };
  return [
    ...mentionOptions(agents, text),
    ...stoppedRoleOptions(agents, collaborators.agents ?? [], text),
  ].map((option) => ({
    capabilities: option.capabilities,
    target: option.target,
    title: option.title,
  }));
}

// =============================================================================================
// What the view draws
// =============================================================================================

/** One row as the list draws it. */
export interface DrawnRow {
  readonly id: string;
  readonly hrefs: readonly string[];
  readonly kind: string;
  readonly firstLine: string;
  readonly people: readonly string[];
  readonly activity: string | null;
  readonly mark: string | null;
}

/** Every row the list draws, in order. */
export function drawnRows(page: Page): Promise<DrawnRow[]> {
  return threadsList(page).evaluate((list, selectors) => {
    const text = (element: Element | null) =>
      (element?.textContent ?? "").replace(/\s+/g, " ").trim();
    return [...list.querySelectorAll(selectors.row)].map((row) => ({
      activity: row.querySelector(selectors.activity)?.getAttribute("datetime") ?? null,
      firstLine: text(row.querySelector(selectors.firstLine)),
      hrefs: [...row.querySelectorAll("a")].map((link) => link.getAttribute("href") ?? ""),
      id: row.getAttribute("data-thread-row") ?? "",
      kind: text(row.querySelector(selectors.kind)),
      mark:
        row.querySelector(selectors.mark) === null ? null : text(row.querySelector(selectors.mark)),
      people: [...row.querySelectorAll(selectors.person)].map(text),
    }));
  }, SELECTORS);
}

/** Fails unless the view row draws the Threads control, then presses it in the shape the row has
 *  at this width: its toggle from 1280 px up, and its item of the one control's menu below. */
export async function pressThreads(page: Page): Promise<void> {
  await presetRow(page).waitFor();
  if ((await presetChooser(page).count()) > 0) {
    await presetChooser(page).click();
    await expect(threadsMenuItem(page), "the view row's menu holds no Threads").toHaveCount(1);
    await threadsMenuItem(page).click();
    return;
  }
  await expect(threadsToggle(page), "the view row draws no control named Threads").toHaveCount(1);
  await threadsToggle(page).click();
}

/** Fails unless the page draws the Threads view with its list. */
export async function expectThreadsDrawn(page: Page, where: string): Promise<void> {
  await expect(threadsView(page), `${where} draws no Threads view`).toBeVisible();
}

/** Opens `/threads`, or `/threads/<id>`, and fails unless the page draws Threads. */
export async function loadThreads(page: Page, key: string, id?: string): Promise<void> {
  await page.goto(threadsPath(key, id));
  await expect(issuePage(page), `${threadsPath(key, id)} drew no [data-issue-page]`).toHaveCount(1);
  await expectThreadsDrawn(page, threadsPath(key, id));
}

/** Presses `choice` and fails unless it is the one choice pressed. */
export async function pickChoice(page: Page, choice: Choice): Promise<void> {
  await expect(
    choiceButton(page, choice),
    `Threads draws no choice ${CHOICES[choice]}`
  ).toHaveCount(1);
  await choiceButton(page, choice).click();
  await expect(choiceButton(page, choice)).toHaveAttribute("aria-pressed", "true");
}

/** Fails unless the list draws rows, and answers how many. */
export async function expectRowsDrawn(page: Page, where: string): Promise<number> {
  await expect(threadRows(page).first(), `${where} draws no row`).toBeVisible();
  return threadRows(page).count();
}

/** The number the control under the rows says is left, or 0 where it draws none. */
export async function leftNumber(page: Page): Promise<number> {
  const control = showNextControl(page);
  if ((await control.count()) === 0) return 0;
  const name = ((await control.textContent()) ?? "").replace(/\s+/g, " ").trim();
  return Number(SHOW_NEXT_NAME.exec(name)?.[1] ?? Number.NaN);
}

/** Draws every page of the choice that is on: the first page as drawn, then one press of the
 *  control for each next 20. Answers the ids each page added and the number left after it. */
export async function drawEveryPage(
  page: Page,
  where: string
): Promise<{ readonly pages: string[][]; readonly left: number[] }> {
  const pages: string[][] = [];
  const left: number[] = [];
  let seen = 0;
  await expectRowsDrawn(page, where);
  for (;;) {
    const rows = await drawnRows(page);
    pages.push(rows.slice(seen).map((row) => row.id));
    seen = rows.length;
    left.push(await leftNumber(page));
    if ((await showNextControl(page).count()) === 0) return { left, pages };
    await showNextControl(page).click();
    await expect
      .poll(() => threadRows(page).count(), {
        message: `${where}: the control under the rows drew no next row`,
      })
      .toBeGreaterThan(seen);
  }
}

/** Presses the row of `id` and fails unless the record opens at its own address. */
export async function openRow(page: Page, key: string, id: string): Promise<void> {
  const row = threadRow(page, id);
  await expect(row, `the list draws no row of ${id}`).toHaveCount(1);
  await row.getByRole("link").click();
  await expect(page).toHaveURL(new RegExp(`${threadsPath(key, id)}$`));
  await expect(openedRecord(page), `the row of ${id} opened no record`).toHaveAttribute(
    "data-thread-record",
    id
  );
}

/** One entry of the opened record as drawn. */
export interface DrawnEntry {
  readonly id: string;
  readonly depth: number;
  readonly left: number;
  readonly text: string;
  readonly time: string | null;
  readonly edit: boolean;
}

/** Every entry of the opened record, in document order. */
export function drawnEntries(page: Page): Promise<DrawnEntry[]> {
  return openedRecord(page).evaluate(
    (record, { edit, entry }) =>
      [...record.querySelectorAll(entry)].map((element) => ({
        depth: Number(element.getAttribute("data-depth")),
        edit: [...element.querySelectorAll("button")].some(
          (button) => (button.textContent ?? "").trim() === edit
        ),
        id: element.getAttribute("data-thread-entry") ?? "",
        left: element.getBoundingClientRect().left,
        text: (element.textContent ?? "").replace(/\s+/g, " ").trim(),
        time: element.querySelector("time")?.getAttribute("datetime") ?? null,
      })),
    { edit: EDIT, entry: SELECTORS.entry }
  );
}

/** Whether `drawn` is `whole` cut at a word with an ellipsis, or `whole` itself. */
export function cutAtWord(drawn: string, whole: string): boolean {
  if (drawn === whole) return true;
  if (!drawn.endsWith("…")) return false;
  const stem = drawn.slice(0, -1).trimEnd();
  return stem.length > 0 && whole.startsWith(stem) && /\s/.test(whole.charAt(stem.length));
}
