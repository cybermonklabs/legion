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
 *   `./page-words-parts`, values from `./page-structure`, and the shared words the box at the
 *   foot of the outline draws, from the modules of words that write them for every composer
 *   (`conversation/composer-words.ts`, `conversation/delivery.ts`, `writes/write-words.ts`,
 *   `artifacts/upload-words.ts`). Each of those imports only other modules of words, types and
 *   constants of `@legion/contracts`, so this file still loads where no component can: a browser
 *   spec's Node loader reads `page-words.ts`, and cannot read a module that imports a JSON file
 *   as the components do. A value import from `page-words.ts` or `./page-words-parts` makes an
 *   import cycle.
 */
import {
  ADDRESS_WORDS,
  COMPOSER_WORDS,
  DELIVERY_COMMANDS,
  DISCARD_DRAFT_LABEL,
  OFFER_LABELS,
} from "../conversation/composer-words";
import { MODE_LABELS, TITLES_JOINER } from "../conversation/delivery";
import { STOP_TRYING_LABEL, STOPPING_LABEL, STOPPING_REASON } from "../writes/write-words";
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

/**
 * What the box at the foot of the outline says in answer to the reader's act (root R22): each
 * answer one sentence, in the page's own words where the composer every other surface draws
 * (`MentionComposer`) and the parts it draws say two or three, and in place of Dispatch's own
 * words of a refusal, which the box never draws. A word or a sentence the page keeps from that
 * composer is taken by import from the module that writes it for every composer, never copied:
 * why Send refuses a draft (`send_refusals`), the warning of a plain mode (`delivery.plain`),
 * the presses' commands and the joiner of the sessions a notice names (`commands`,
 * `titles_joiner`), the words an address is said in - its prepositions, and a reply's -
 * (`address`), and why Stop trying takes no second press (`stopping_reason`).
 * `IssueComposer.tsx` hands the rest to the box (`OutlineLook.answers`):
 *
 * - `deleted_reply`: Send's refusal of a reply to a post deleted since, by its kind, with where
 *   the draft goes instead;
 * - `late`: a send past its deadline;
 * - `delivery`: a send to sessions that advertise no delivery mode (`deaf`), by why - their
 *   lists hold nothing (`unlisted`), hold no mode this page knows (`unknown`), or some of each
 *   (`mixed`) - and to sessions that do not advertise the mode the draft asks for
 *   (`unsupported`), with the modes they take `instead`;
 * - `refused`: a send Dispatch refused, by its code, and `other` for any other code, a failure to
 *   reach Dispatch and a send it did not answer in time; each says the words are kept;
 * - `stopped`: a send that ended without Dispatch's verdict, by how it ended;
 * - `upload`: a file uploading into the draft that the reader stopped, that Dispatch refused by
 *   its code, or that ended any other way;
 * - `elsewhere`: Send's refusal of a refused draft written for another place than the box shows;
 * - `kept_reply`, `gone_reply`, `changed_edit`: a reply kept for another row, one whose post the
 *   outline no longer draws, and an edit that waits beside a newer comment.
 *
 * A sentence that names a session, an author, an address or how many hours Dispatch keeps a
 * write's key is cut around that name, as `BLOCKED_BY_REFUSALS` of `page-words.ts` cuts its
 * sentences around a key: the name is drawn between `before` and `after`. A sentence that
 * differs by the kind of post or by one or many is keyed by it.
 */
export const COMPOSER_ANSWER_WORDS = freeze({
  address: { prepositions: ADDRESS_WORDS.prepositions, reply: ADDRESS_WORDS.reply },
  changed_edit:
    "This comment changed since you wrote your edit, so the field shows it as it reads now and your edit is kept here:",
  commands: DELIVERY_COMMANDS,
  deleted_reply: {
    ask: "The question this reply answers was deleted and takes no reply, so press Write on the issue instead to send it on the issue.",
    comment:
      "The comment this reply answers was deleted and takes no reply, so press Write on the issue instead to send it on the issue.",
    message:
      "The message this reply answers was deleted and takes no reply, so press Write on the issue instead to send it on the issue.",
  },
  delivery: {
    deaf: {
      mixed:
        " advertise no delivery mode this Dispatch knows or cannot reach their conversations, so sending it records a failed attempt.",
      unknown: {
        many: " list no delivery mode this Dispatch knows, so sending it records a failed attempt.",
        one: " lists no delivery mode this Dispatch knows, so sending it records a failed attempt.",
      },
      unlisted: {
        many: " advertise no delivery mode while their plugins cannot reach their conversations, so sending it records a failed attempt.",
        one: " advertises no delivery mode while its plugin cannot reach its conversation, so sending it records a failed attempt.",
      },
    },
    plain: COMPOSER_WORDS.delivery.plain,
    unsupported: {
      after: COMPOSER_WORDS.delivery.unsupported.after,
      before: COMPOSER_WORDS.delivery.unsupported.before,
      instead: {
        ...COMPOSER_WORDS.delivery.unsupported.instead,
        before: ", so sending it records a failed attempt; prefix with ",
      },
    },
  },
  elsewhere: {
    address: {
      after: ", so go back there to send it, or discard it.",
      before: "This message was written ",
    },
    gone: {
      ask: {
        deleted:
          "This message was written as a reply to a question that was deleted and takes no reply, so discard it to write here.",
        gone: "This message was written as a reply to a question that is no longer listed, so discard it to write here.",
      },
      comment: {
        deleted:
          "This message was written as a reply to a comment that was deleted and takes no reply, so discard it to write here.",
        gone: "This message was written as a reply to a comment that is no longer listed, so discard it to write here.",
      },
      message: {
        deleted:
          "This message was written as a reply to a message that was deleted and takes no reply, so discard it to write here.",
        gone: "This message was written as a reply to a message that is no longer listed, so discard it to write here.",
      },
    },
    reply:
      "This message was written as a reply, so press Reply on that row again to send it, or discard it.",
  },
  gone_reply: {
    after: {
      deleted: {
        now: " that your reply answered was deleted and takes no reply, so your reply is kept here:",
        waits:
          " that your reply answered was deleted and takes no reply, so your reply is kept here to use in this field once it is empty:",
      },
      gone: {
        now: " that your reply answered is no longer listed, so your reply is kept here:",
        waits:
          " that your reply answered is no longer listed, so your reply is kept here to use in this field once it is empty:",
      },
    },
    before: { ask: "The question by ", comment: "The comment by ", message: "The message by " },
  },
  kept_reply: {
    ask: {
      after: " is kept: press Reply on that question to go on with it.",
      before: "Your reply to the question by ",
    },
    comment: {
      after: " is kept: press Reply on that comment to go on with it.",
      before: "Your reply to the comment by ",
    },
    message: {
      after: " is kept: press Reply on that message to go on with it.",
      before: "Your reply to the message by ",
    },
  },
  late: "Still sending, since Dispatch has not confirmed it yet: it keeps trying, and the message is delivered once.",
  refused: {
    ANCHOR_MISSING:
      "The words this comment is on are no longer in their document, so it cannot be placed there and your words are kept.",
    CAP_EXCEEDED:
      "Dispatch takes nothing this long, so your words are kept to shorten and send again.",
    DECISION_BLOCK_IN_BODY:
      "A decision cannot stand in a comment, so your words are kept to take it out and send again.",
    INVALID_INPUT:
      "Dispatch could not take this as it is written, so your words are kept to change and send again.",
    ISSUE_CLOSED: "This issue is closed and takes no question, so your words are kept.",
    KEY_REUSED:
      "Dispatch had already taken other words under this send's key, so this was not sent and your words are kept.",
    POST_DELETED: "What this reply answers was deleted and takes no reply, so your words are kept.",
    other: "Dispatch refused this message or did not answer, and your words are kept.",
  },
  send_refusals: COMPOSER_WORDS.refusals,
  stopped: {
    expired:
      "Dispatch has not confirmed this message and no longer holds its key, so a Retry sends it anew and may deliver it a second time.",
    interrupted: {
      after:
        " hours of the first try delivers it at most once, while a later one may deliver it twice.",
      before: "This page closed before Dispatch confirmed this message, and a Retry within ",
    },
    moved: {
      after:
        ", so it may have been delivered there, and sending it from here is a new message that may be delivered a second time.",
      before: "Dispatch has not confirmed this message, sent ",
    },
    unconfirmed: {
      after:
        " hours of the first try delivers it at most once, while a later one may deliver it twice.",
      before: "Dispatch has not confirmed this message, and a Retry within ",
    },
  },
  stopping_reason: STOPPING_REASON,
  titles_joiner: TITLES_JOINER,
  upload: {
    other:
      "The file was not uploaded, since Dispatch refused it or did not answer, and your words are kept.",
    refused: {
      PRECONDITION_FAILED:
        "The document changed while this file uploaded, so open it to see what it says now and upload again.",
      PRECONDITION_REQUIRED:
        "Dispatch needs to know which version this file replaces, so open the document and upload again.",
    },
    stopped: {
      expired:
        "Dispatch has not confirmed the file and no longer holds its key, so a Retry uploads it anew and may store it a second time.",
      unconfirmed: {
        after:
          " hours of the first try stores it at most once, while a later one may store it twice.",
        before: "Dispatch has not confirmed the file, which may be stored, and a Retry within ",
      },
    },
  },
} as const);

/**
 * The names the reference picker in the box at the foot of the outline draws (decision 527):
 * the list's accessible name, the heading over each kind's rows, and the control on a row that
 * holds parts, in the page's own kind words - a question, never an ask, and parts, never the
 * blocks a record is made of (root R1). They are names, as `CONTROL_LABELS` holds the box's
 * controls drawn after an act, so `unasked` lists them; the picker's lines are
 * `BOX_PICKER_LINES`. `IssueComposer.tsx` hands both to the box (`OutlineLook.picker`).
 */
export const BOX_PICKER_NAMES = freeze({
  headings: {
    agent: "Agents",
    ask: "Questions",
    "ask-part": "Question parts",
    comment: "Comments",
    "comment-block": "Comment parts",
    component: "Components",
    document: "Documents",
    "document-block": "Document parts",
    issue: "Issues",
    message: "Messages",
    "message-block": "Message parts",
    person: "People",
    role: "Roles",
  },
  label: "References",
  parts: { blocks: "Parts", documents: "Documents" },
} as const);

/**
 * What the reference picker in the box at the foot of the outline says once the reader has
 * opened it, each one sentence or a state's words: what two typed letters will add
 * (`instruction`), that no row matches (`empty`), that an opened row holds no parts
 * (`parts_none`), where the agent list behind the agent and role rows stands (`agents`), said of
 * the list and of no part of the system behind it, and what a role's row says of a role no
 * running session holds (`stopped_role`). The list's loading line and its failed read are the
 * page's own `ANSWER_LINES.references_loading` and `references_unreadable`, which
 * `IssueComposer.tsx` hands the picker beside these.
 */
export const BOX_PICKER_LINES = freeze({
  agents: {
    loading: "Loading the agents…",
    refilling: "The list of agents is refilling, so try again in a moment.",
    unreachable: "Could not load the agents.",
  },
  empty: "Nothing matches what you typed.",
  instruction: "Type two characters to search issues, documents, questions, comments and messages.",
  parts_none: { blocks: "This holds no parts.", documents: "This agent holds no documents." },
  stopped_role: "not running, so not told",
} as const);

export default {
  add: {
    ANSWER_LINES: {
      /** What Send says while the send is out, on the write line of the box at the foot of the
       *  outline, beside the busy words of `ANSWER_LINES` in `page-words.ts`. */
      sending: "Sending…",
      /** What Stop trying says once the reader has pressed it, while the attempt already sent is
       *  still out (`StopTrying`). */
      stopping: STOPPING_LABEL,
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
       *  row is said by `COMPOSER_ANSWER_WORDS.kept_reply`, which names the author it answers. */
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
      /** The marks drawn before the words of the box's two kinds, on their chips. */
      composer_comment_mark: "“",
      composer_question_mark: "?",
      /** The names a screen reader reads the box at the foot of the outline by (decision 527):
       *  its form, the field of the line that edits the reader's comment - the write line's
       *  field is named by its chip's word, `composer_comment` or `composer_question` - and the
       *  list, off screen, of the sessions and roles its comment is delivered to, each read by
       *  its name. */
      composer_form: "Draft",
      composer_edit: "Edit comment",
      composer_mentioned: "Mentioned",
      /** The field's description, which a screen reader reads beside its name for a reader who
       *  cannot see the chips: what the line's keys do, on the write line and on the line that
       *  edits a comment. */
      composer_keys:
        "Enter sends and Shift+Enter starts a new line; @ links an agent, a role or a person, # an issue, and [[ anything else",
      composer_keys_edit:
        "Enter saves and Shift+Enter starts a new line; @ links an agent, a role or a person, # an issue, and [[ anything else",
      /** The control beside a refusal that drops the draft it handed back, and the presses of a
       *  draft drawn beside the box's line: its own reply or statement's draft drops or takes it,
       *  and an edit that waits beside a newer comment does. The words of every composer. */
      discard_draft: DISCARD_DRAFT_LABEL,
      discard_my_draft: OFFER_LABELS.dropDraft,
      discard_my_edit: OFFER_LABELS.dropEdit,
      use_it_here: OFFER_LABELS.takeHere,
      use_my_edit: OFFER_LABELS.takeEdit,
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
      /** What the box says while it is empty and writes a comment on a statement. */
      statement_placeholder: "Your comment on the statement…  (Enter sends)",
      /** The same in the shorter words the box says at 760 px and below, which fit its line
       *  whole. */
      statement_placeholder_phone: "Comment on the statement…",
      /** The delivery modes by the names a notice of the box says them in, as every composer
       *  does. */
      mode_aside: MODE_LABELS.aside,
      mode_btw: MODE_LABELS.btw,
      mode_send: MODE_LABELS.steer,
      /** What a late send's control says, as every composer's does (`StopTrying`). */
      stop_trying: STOP_TRYING_LABEL,
      /** The mark the target line's control draws, which `write_on_issue` names, and the mark
       *  between the target's author and its kind. */
      target_end_mark: "×",
      target_separator: "·",
    },
  },
  unasked: [BOX_PICKER_NAMES],
  answers: [ADDRESS_FALLBACK_WORDS, COMPOSER_ANSWER_WORDS, BOX_PICKER_LINES],
} as const satisfies PartWords;
