/**
 * Every box a person writes prose in, and every write each one makes.
 *
 * The dashboard mounts one text field in all of them (`MentionField.tsx`), so the reference
 * picker is in every box rather than in the composer alone. This table is what the three
 * readers of that fact share: each box reads its own hint here at the mount, so the words under
 * a box and the words in this list cannot disagree; a browser row drives each covered box by the
 * name under `reach`; and a box with no picker says here why it has none.
 *
 * A row is one write of one box. A box that makes two - a reply box that answers a comment
 * thread or a message thread, an ask card that answers or asks back - has a row for each,
 * because what Dispatch does with a mention is the write's doing, not the box's.
 */

/** Where a person writes: one id per box, whatever surface draws it. The ask card's box is one
 *  box however many surfaces mount the card (the Inbox, a decision block on a document). */
export type DispatchWritingBox =
  | "conversation-composer"
  | "phone-thread-composer"
  | "inbox-thread-reply"
  | "margin-composer"
  | "margin-comment-edit"
  | "margin-thread-reply"
  | "agents-row-composer"
  | "suggestion-replacement"
  | "ask-option-label"
  | "ask-option-description"
  | "ask-answer"
  | "ask-thread-reply"
  | "decisions-change"
  | "create-issue-spec"
  | "approval-reason"
  | "broadcast"
  | "architecture-reason"
  | "delivery-production-jobs"
  | "delivery-population-authors"
  | "delivery-excluded-repos";

/**
 * What a box's write makes:
 *
 * - `comment`: a comment on an issue or on a document, a reply to one included. The comment
 *   route is the one route that resolves a mention and delivers it.
 * - `comment-edit`: a new body for a comment already posted; its save carries no mention.
 * - `suggestion`: a comment that offers replacement text for the document it is anchored to.
 * - `ask`: a new ask, with its options.
 * - `ask-answer`: an answer to an ask, which reaches whoever asked it.
 * - `ask-back`: a question back at the asker, which is a comment on the ask.
 * - `message-reply`: a reply in an issue's message thread, delivered to the thread's target.
 * - `agent-message`: a message to one agent's session.
 * - `broadcast`: one message to each selected agent.
 * - `issue-spec`: the spec document a new issue is created with.
 * - `approval-reason`: why a reviewer asked for changes on a document.
 * - `architecture-reason`: why an issue touches no component.
 * - `delivery-settings`: the delivery settings of this Dispatch.
 */
export type DispatchWriting =
  | "comment"
  | "comment-edit"
  | "suggestion"
  | "ask"
  | "ask-answer"
  | "ask-back"
  | "message-reply"
  | "agent-message"
  | "broadcast"
  | "issue-spec"
  | "approval-reason"
  | "architecture-reason"
  | "delivery-settings";

/** One row of DISPATCH_WRITING_BOXES: one write of one box. */
export interface DispatchWritingBoxRow {
  readonly box: DispatchWritingBox;
  /** The write this row is about. Every write the box can make has a row of its own. */
  readonly write: DispatchWriting;
  /** How a test reaches the box: the box's accessible name, which a browser row types into with
   *  `getByRole("textbox", { name })`. A box whose name changes with what it is answering gives
   *  a CSS selector in brackets instead, the attribute the dashboard puts there for the purpose,
   *  and a name that holds an issue key writes that key as `<ISSUE>`. */
  readonly reach: string;
  /** Whether the box mounts the shared text field, so the picker is in it and a browser row can
   *  drive it. A box that does not mount it says why in `reason`. */
  readonly covered: boolean;
  /** Whether Dispatch tells an agent or a role that this write names it. Only a comment does: a
   *  message reaches the session it is addressed to and tells nobody it names, and an ask, an
   *  answer, a spec, a reason and a setting reach nobody at all. A person is never told; a
   *  person's reference is a link for a reader, which is why a delivering box says so. */
  readonly delivers: boolean;
  /** Whether the picker here lists the rows that need an issue - the issue's people, and the
   *  Legion roles the issue records that nobody is running. A box that writes on no issue lists
   *  everyone who has signed in instead. For a box whose surface is sometimes a project
   *  document, this says what it does on an issue. */
  readonly issueRows: boolean;
  /** The line under the box, which the box itself reads from this row at the mount. Empty for a
   *  box that is not covered: with no picker in it, it has no such line to show. */
  readonly hint: string;
  /** Why the box is not covered. Only a row whose `covered` is false carries one. */
  readonly reason?: string;
}

/** How each door is opened, which every hint starts with. */
const DOORS = "Type @ for an agent, a role or a person, # for an issue, [[ for anything else.";

/** What a box says when a mention on its write is delivered. */
export const DISPATCH_WRITING_HINT_DELIVERED = `${DOORS} A picked agent or role is told; a person is not.`;

/** What a box says when its write tells nobody it names. */
export const DISPATCH_WRITING_HINT_QUIET = `${DOORS} Nobody you pick is told.`;

/** What the ask card's one box says, since the reader chooses there which write it makes: an
 *  answer reaches whoever asked and tells nobody it names, while a question back is a comment.
 *  Both of that box's rows carry it, since the box shows one line for the two. */
export const DISPATCH_WRITING_HINT_ASK = `${DOORS} Ask back and a picked agent or role is told; an answer tells nobody.`;

/** Why the delivery settings boxes have no picker. */
const SETTINGS_REASON =
  "One setting per line rather than prose: a job name, a login or an owner/repo, which a reference would not name.";

export const DISPATCH_WRITING_BOXES: readonly DispatchWritingBoxRow[] = [
  // The issue's Conversation tab, above the list, and the same composer as a phone's thread
  // sheet draws it. Either can also answer a message thread, which goes to that thread's
  // target rather than to anyone the body names.
  {
    box: "conversation-composer",
    covered: true,
    delivers: true,
    hint: DISPATCH_WRITING_HINT_DELIVERED,
    issueRows: true,
    reach: "Comment",
    write: "comment",
  },
  {
    box: "conversation-composer",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: true,
    reach: "Comment",
    write: "message-reply",
  },
  {
    box: "phone-thread-composer",
    covered: true,
    delivers: true,
    hint: DISPATCH_WRITING_HINT_DELIVERED,
    issueRows: true,
    reach: "Comment",
    write: "comment",
  },
  {
    box: "phone-thread-composer",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: true,
    reach: "Comment",
    write: "message-reply",
  },
  // The Inbox's reading column: one reply box, under a comment thread or a message thread.
  {
    box: "inbox-thread-reply",
    covered: true,
    delivers: true,
    hint: DISPATCH_WRITING_HINT_DELIVERED,
    issueRows: true,
    reach: "Reply",
    write: "comment",
  },
  {
    box: "inbox-thread-reply",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: true,
    reach: "Reply",
    write: "message-reply",
  },
  // A document's margin: the composer the reader opens from a selection, which writes a
  // comment, a suggestion or an ask, and the two boxes of a thread card.
  {
    box: "margin-composer",
    covered: true,
    delivers: true,
    hint: DISPATCH_WRITING_HINT_DELIVERED,
    issueRows: true,
    reach: "Comment",
    write: "comment",
  },
  {
    box: "margin-composer",
    covered: true,
    delivers: true,
    hint: DISPATCH_WRITING_HINT_DELIVERED,
    issueRows: true,
    reach: "Reason",
    write: "suggestion",
  },
  {
    box: "margin-composer",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: true,
    reach: "Question",
    write: "ask",
  },
  {
    box: "margin-comment-edit",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: true,
    reach: "Edit comment",
    write: "comment-edit",
  },
  {
    box: "margin-thread-reply",
    covered: true,
    delivers: true,
    hint: DISPATCH_WRITING_HINT_DELIVERED,
    issueRows: true,
    reach: "Reply",
    write: "comment",
  },
  // An Agents row: a message to that session, or, once the reader picks an issue, a comment on
  // it that reaches the agent by naming it.
  {
    box: "agents-row-composer",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: false,
    reach: "Comment",
    write: "agent-message",
  },
  {
    box: "agents-row-composer",
    covered: true,
    delivers: true,
    hint: DISPATCH_WRITING_HINT_DELIVERED,
    issueRows: true,
    reach: "Comment",
    write: "comment",
  },
  // A Reply under a message the row shows, where that message belongs to an issue: it goes to
  // the thread's target, as the Conversation's does.
  {
    box: "agents-row-composer",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: true,
    reach: "Comment",
    write: "message-reply",
  },
  // The replacement text of a suggestion, beside the reason for it: both go in the one comment.
  {
    box: "suggestion-replacement",
    covered: true,
    delivers: true,
    hint: DISPATCH_WRITING_HINT_DELIVERED,
    issueRows: true,
    reach: "Replacement",
    write: "suggestion",
  },
  // An ask's options, which go out with the question and tell nobody by themselves.
  {
    box: "ask-option-label",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: true,
    reach: "Option 1 label",
    write: "ask",
  },
  {
    box: "ask-option-description",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: true,
    reach: "Option 1 description",
    write: "ask",
  },
  // The ask card's one box, which the Inbox and a document's decision block both draw: an
  // answer goes to whoever asked, and a question back is a comment on the ask.
  {
    box: "ask-answer",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_ASK,
    issueRows: true,
    reach: "[data-ask-answer]",
    write: "ask-answer",
  },
  {
    box: "ask-answer",
    covered: true,
    delivers: true,
    hint: DISPATCH_WRITING_HINT_ASK,
    issueRows: true,
    reach: "[data-ask-answer]",
    write: "ask-back",
  },
  {
    box: "ask-thread-reply",
    covered: true,
    delivers: true,
    hint: DISPATCH_WRITING_HINT_DELIVERED,
    issueRows: true,
    reach: "Reply",
    write: "comment",
  },
  // The issue's Decisions panel: a change is a comment anchored to the decision it quotes.
  {
    box: "decisions-change",
    covered: true,
    delivers: true,
    hint: DISPATCH_WRITING_HINT_DELIVERED,
    issueRows: true,
    reach: "Your change",
    write: "comment",
  },
  // The Create issue dialog: the issue has no key yet, so the picker offers everyone who has
  // signed in rather than an issue's people.
  {
    box: "create-issue-spec",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: false,
    reach: "Spec (optional)",
    write: "issue-spec",
  },
  {
    box: "approval-reason",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: true,
    reach: "Reason",
    write: "approval-reason",
  },
  // The Agents page's broadcast box: one message to each selected agent, which tells nobody the
  // body names, and which belongs to no issue.
  {
    box: "broadcast",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: false,
    reach: "Broadcast message",
    write: "broadcast",
  },
  {
    box: "architecture-reason",
    covered: true,
    delivers: false,
    hint: DISPATCH_WRITING_HINT_QUIET,
    issueRows: true,
    reach: "Why <ISSUE> is not architectural",
    write: "architecture-reason",
  },
  // Settings' delivery form. These three take no picker, and the spec's exclusion is why.
  {
    box: "delivery-production-jobs",
    covered: false,
    delivers: false,
    hint: "",
    issueRows: false,
    reach: "Production jobs",
    reason: SETTINGS_REASON,
    write: "delivery-settings",
  },
  {
    box: "delivery-population-authors",
    covered: false,
    delivers: false,
    hint: "",
    issueRows: false,
    reach: "Population authors",
    reason: SETTINGS_REASON,
    write: "delivery-settings",
  },
  {
    box: "delivery-excluded-repos",
    covered: false,
    delivers: false,
    hint: "",
    issueRows: false,
    reach: "Excluded repositories",
    reason: SETTINGS_REASON,
    write: "delivery-settings",
  },
];

/**
 * The row for one write of one box, which is what the box reads its hint from at the mount. A
 * pair with no row is a box that writes something this list does not hold, which is a mistake in
 * one or the other, so it throws rather than leaving the box with no words.
 */
export function dispatchWritingBox(
  box: DispatchWritingBox,
  write: DispatchWriting
): DispatchWritingBoxRow {
  const row = DISPATCH_WRITING_BOXES.find((one) => one.box === box && one.write === write);
  if (row === undefined) {
    throw new Error(`no writing row for ${box} writing ${write}`);
  }
  return row;
}
