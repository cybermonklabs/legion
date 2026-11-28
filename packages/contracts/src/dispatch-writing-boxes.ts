/**
 * Every box a person writes prose in, and every write each one makes.
 *
 * The dashboard mounts one text field in most of them (`MentionField.tsx`), and the two boxes
 * that are editors of their own - a document's body and an agent's live conversation - host the
 * same picker in their own way, so the picker is in every box rather than in the composer
 * alone. This table is what the readers of that fact share: each box reads its own row here at
 * the mount, for whether its write tells a picked party and whose rows its picker lists; a
 * browser row drives each covered box by the name under `reach`; and a box with no picker says
 * here why it has none.
 *
 * A row is one write of one box. A box that makes two - a reply box that answers a comment
 * thread or a message thread, an ask card that answers or asks back - has a row for each,
 * because what Dispatch does with a mention is the write's doing, not the box's.
 *
 * A unit test walks the dashboard's own source against this list and the reading list beside it
 * (`DISPATCH_READING_SURFACES`): a box drawn in a file no row names fails there, and so does a
 * row whose box the dashboard no longer draws.
 */

/** Where a person writes: one id per box, whatever surface draws it. The ask card's box is one
 *  box however many surfaces mount the card (the Inbox, a decision block on a document). */
export type DispatchWritingBox =
  | "conversation-composer"
  | "phone-thread-composer"
  | "issue-page-composer"
  | "threads-reply"
  | "threads-comment-edit"
  | "threads-message"
  | "threads-new-thread"
  | "inbox-thread-reply"
  | "margin-composer"
  | "margin-comment-edit"
  | "margin-thread-reply"
  | "document-body"
  | "agents-row-composer"
  | "agent-live-composer"
  | "suggestion-replacement"
  | "ask-option-label"
  | "ask-option-description"
  | "ask-answer"
  | "ask-decline-note"
  | "ask-thread-reply"
  | "decisions-change"
  | "create-issue-spec"
  | "approval-reason"
  | "broadcast"
  | "architecture-reason"
  | "priority-description"
  | "priority-title"
  | "issue-title"
  | "delivery-production-jobs"
  | "delivery-population-authors"
  | "delivery-excluded-repos"
  | "delivery-flag-note"
  | "delivery-reject-reason";

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
 * - `ask-decline`: a person's note on declining to answer an ask, which closes it with no
 *   answer; the note is stored on the ask's resolution and read by whoever asked.
 * - `message-reply`: a reply in an issue's message thread, delivered to the thread's target.
 * - `issue-message`: a root message of an issue, which starts a thread with the agent or the
 *   role it targets.
 * - `agent-message`: a message to one agent's session.
 * - `broadcast`: one message to each selected agent.
 * - `document-text`: the document's own text, which every reader of that document reads and
 *   which is sent to nobody.
 * - `issue-spec`: the spec document a new issue is created with.
 * - `issue-title`: an issue's title, which every reader of the issue reads and which is sent to
 *   nobody.
 * - `approval-reason`: why a reviewer asked for changes on a document.
 * - `architecture-reason`: why an issue touches no component.
 * - `priority-description`: a priority's description, which every reader of the priority reads
 *   and which is sent to nobody.
 * - `priority-title`: a priority's title.
 * - `issue-title`: an issue's title.
 * - `delivery-settings`: the delivery settings of this Dispatch.
 * - `production-failure`: a failure a person flags on a deploy, a production event of its
 *   project (`POST /api/v1/projects/{key}/production-events`), with its note.
 * - `failure-rejection`: why a person rejects a failure counted against a deploy, stored on the
 *   failure's production event (`PUT /api/v1/projects/{key}/production-events/{id}/state`).
 */
export type DispatchWriting =
  | "comment"
  | "comment-edit"
  | "suggestion"
  | "ask"
  | "ask-answer"
  | "ask-back"
  | "ask-decline"
  | "message-reply"
  | "issue-message"
  | "agent-message"
  | "broadcast"
  | "document-text"
  | "issue-spec"
  | "approval-reason"
  | "architecture-reason"
  | "priority-description"
  | "priority-title"
  | "issue-title"
  | "delivery-settings"
  | "production-failure"
  | "failure-rejection";

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
  /** The source files that draw the box, from the repository root. One of them holds the box's
   *  own mount - its field, its editor or its textarea - and one of them writes this row's
   *  `box` id, which is the whole tie between the list and the place it describes. A box drawn
   *  in two files, as the document body's editor and the picker bound to it are, names both. */
  readonly files: readonly string[];
  /** Whether the reference picker is in the box, so a browser row can drive it: the boxes that
   *  mount the shared text field, and the two that host the same picker in an editor of their
   *  own (the document body, the agent live conversation). A box with no picker says why in
   *  `reason`. */
  readonly covered: boolean;
  /** Whether Dispatch tells an agent or a role that this write names it. Only a comment does: a
   *  message reaches the session it is addressed to and tells nobody it names, and an ask, an
   *  answer, a spec, a reason and a setting tell nobody they name. A person is never told, and a
   *  person's reference is their name with no link until a person has a page (R5). */
  readonly delivers: boolean;
  /** Whether the picker here lists the rows that need an issue - the issue's people. A box that
   *  writes on no issue lists everyone who has signed in instead. For a box whose surface is
   *  sometimes a project document, this says what it does on an issue. */
  readonly issueRows: boolean;
  /** Why the box is not covered. Only a row whose `covered` is false carries one. */
  readonly reason?: string;
}

/** Why the delivery settings boxes have no picker. */
const SETTINGS_REASON =
  "One setting per line rather than prose: a job name, a login or an owner/repo, which a reference would not name.";

/** Why the drill-down's two boxes have no picker. */
const PRODUCTION_EVENT_REASON =
  "A production event is none of the four records the reference graph reads (an artifact, an ask, a comment, a message), so a reference written in a failure's note or in a rejection's reason names nothing and tells nobody. The words are stored on the event as the characters they are.";

export const DISPATCH_WRITING_BOXES: readonly DispatchWritingBoxRow[] = [
  // The issue's Conversation tab, above the list, and the same composer as a phone's thread
  // sheet draws it. Either can also answer a message thread, which goes to that thread's
  // target rather than to anyone the body names.
  {
    box: "conversation-composer",
    covered: true,
    delivers: true,
    files: ["packages/dispatch/web/src/features/conversation/ConversationTab.tsx"],
    issueRows: true,
    reach: "Comment",
    write: "comment",
  },
  {
    box: "conversation-composer",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/conversation/ConversationTab.tsx"],
    issueRows: true,
    reach: "Comment",
    write: "message-reply",
  },
  {
    box: "phone-thread-composer",
    covered: true,
    delivers: true,
    files: ["packages/dispatch/web/src/features/conversation/ConversationTab.tsx"],
    issueRows: true,
    reach: "Comment",
    write: "comment",
  },
  // The phone thread composer answers a comment thread alone. `phone.beginReply` is wired to
  // comment turns only and `phoneThread` resolves a comment card only, so a message's Reply at
  // phone width is answered by the docked conversation composer above, and no reader can put
  // this box in front of a message. The row stays, because `MentionComposer` would compute
  // this write for a message reply target, and says what it is: a pair nothing can drive.
  {
    box: "phone-thread-composer",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/conversation/ConversationTab.tsx"],
    issueRows: true,
    reach: "Comment",
    reason:
      "No reader reaches it: a message's Reply on a phone is answered by the docked conversation composer, and this box opens over a comment thread alone.",
    write: "message-reply",
  },
  // The new issue page's one box, at the foot of its outline: with no target it writes a comment
  // or a question on the issue; aimed at a row of the outline it writes a reply under it, which
  // under a message goes to that thread's target; and on the reader's own comment it writes a new
  // text of it. It answers no ask, and nothing sends the answer the page's writer holds yet
  // (`OutlineWriter.answer` in `outline-writer.tsx`). The line's name changes with what it
  // writes, so it is reached by the attribute its form carries for the purpose.
  {
    box: "issue-page-composer",
    covered: true,
    delivers: true,
    files: ["packages/dispatch/web/src/features/issue-page/IssueComposer.tsx"],
    issueRows: true,
    reach: "[data-issue-composer]",
    write: "comment",
  },
  {
    box: "issue-page-composer",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/issue-page/IssueComposer.tsx"],
    issueRows: true,
    reach: "[data-issue-composer]",
    write: "ask",
  },
  {
    box: "issue-page-composer",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/issue-page/IssueComposer.tsx"],
    issueRows: true,
    reach: "[data-issue-composer]",
    write: "message-reply",
  },
  // Only an Edit of the reader's own comment opens the box's edit line, and the page draws no
  // such control yet, so no reader reaches this write until one is under the box; the row is
  // covered then, with a driver step through that Edit.
  {
    box: "issue-page-composer",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/issue-page/IssueComposer.tsx"],
    issueRows: true,
    reach: "[data-issue-composer]",
    reason:
      "No reader reaches it yet: only an Edit opens the box's edit line - part M's in the facts, or part T's on the row of the reader's own comment - and the row is covered, with a driver step through that Edit, once the first of them is under the box.",
    write: "comment-edit",
  },
  // Threads on the new issue page (dispatch://INFRA-5704): the box at the foot of an opened
  // comment thread or settled ask and the box a reply control opens under a comment or under a
  // settled ask, which write a comment with the picker; the reader's own comment opened to edit;
  // and the boxes of a message thread, which are Threads' own field and take no picker (R11).
  {
    box: "threads-reply",
    covered: true,
    delivers: true,
    files: ["packages/dispatch/web/src/features/issue-page/threads/CommentBox.tsx"],
    issueRows: true,
    reach: "Reply",
    write: "comment",
  },
  {
    box: "threads-comment-edit",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/issue-page/threads/CommentBox.tsx"],
    issueRows: true,
    reach: "Edit comment",
    write: "comment-edit",
  },
  {
    box: "threads-message",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/issue-page/threads/MessageBox.tsx"],
    issueRows: true,
    reach: "Reply",
    reason:
      "A message names its agent by its target, so the box offers no @ and stores no mention (R11 of dispatch://INFRA-5704): it is Threads' own field, which mounts no picker.",
    write: "message-reply",
  },
  // New thread's box in Threads (R10 of dispatch://INFRA-5704): a root message of the issue to
  // the agent the chooser above the box names, in Threads' own field, which takes no picker.
  {
    box: "threads-new-thread",
    covered: false,
    delivers: false,
    files: [
      "packages/dispatch/web/src/features/issue-page/threads/MessageBox.tsx",
      "packages/dispatch/web/src/features/issue-page/threads/NewThread.tsx",
    ],
    issueRows: true,
    reach: "Message",
    reason:
      "A message names its agent by its target, which the chooser above the box picks, so the box offers no @ and stores no mention (R10 and R11 of dispatch://INFRA-5704): it is Threads' own field, which mounts no picker.",
    write: "issue-message",
  },
  // The Inbox's reading column: one reply box, under a comment thread or a message thread. A
  // thread item of Dashboard draws the same box under the same thread.
  {
    box: "inbox-thread-reply",
    covered: true,
    delivers: true,
    files: [
      "packages/dispatch/web/src/features/dashboard/ThreadItem.tsx",
      "packages/dispatch/web/src/features/inbox/ThreadBody.tsx",
    ],
    issueRows: true,
    reach: "Reply",
    write: "comment",
  },
  {
    box: "inbox-thread-reply",
    covered: true,
    delivers: false,
    files: [
      "packages/dispatch/web/src/features/dashboard/ThreadItem.tsx",
      "packages/dispatch/web/src/features/inbox/ThreadBody.tsx",
    ],
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
    files: ["packages/dispatch/web/src/features/margin/CommentsTab.tsx"],
    issueRows: true,
    reach: "Comment",
    write: "comment",
  },
  {
    box: "margin-composer",
    covered: true,
    delivers: true,
    files: ["packages/dispatch/web/src/features/margin/CommentsTab.tsx"],
    issueRows: true,
    reach: "Reason",
    write: "suggestion",
  },
  {
    box: "margin-composer",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/margin/CommentsTab.tsx"],
    issueRows: true,
    reach: "Question",
    write: "ask",
  },
  {
    box: "margin-comment-edit",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/margin/ThreadCard.tsx"],
    issueRows: true,
    reach: "Edit comment",
    write: "comment-edit",
  },
  {
    box: "margin-thread-reply",
    covered: true,
    delivers: true,
    files: ["packages/dispatch/web/src/features/margin/ThreadCard.tsx"],
    issueRows: true,
    reach: "Reply",
    write: "comment",
  },
  // The document itself: the body a reader edits, where the picker is hosted by the editor
  // rather than by the shared field (`@legion/proof-editor`'s `src/reference-picker.ts`). A
  // pick is written into the document's own text, so it reaches every reader of the document
  // and tells nobody.
  {
    box: "document-body",
    covered: true,
    delivers: false,
    files: [
      "packages/dispatch/web/src/features/doc/DocumentRefPicker.tsx",
      "packages/dispatch/web/src/features/doc/editor.ts",
    ],
    issueRows: true,
    reach: "Document editor",
    write: "document-text",
  },
  // An Agents row: a message to that session, or, once the reader picks an issue, a comment on
  // it that reaches the agent by naming it.
  {
    box: "agents-row-composer",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/agents/AgentMessageComposer.tsx"],
    issueRows: false,
    reach: "Comment",
    write: "agent-message",
  },
  {
    box: "agents-row-composer",
    covered: true,
    delivers: true,
    files: ["packages/dispatch/web/src/features/agents/AgentMessageComposer.tsx"],
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
    files: ["packages/dispatch/web/src/features/agents/AgentMessageComposer.tsx"],
    issueRows: true,
    reach: "Comment",
    write: "message-reply",
  },
  // The session's own conversation: the live composer, whose `/` lists the session's commands
  // and whose three reference doors list the same rows every other box lists. The message
  // reaches the session it is addressed to, so a picked agent or role is told nothing by it.
  {
    box: "agent-live-composer",
    covered: true,
    delivers: false,
    files: [
      "packages/dispatch/web/src/features/agent-view/AgentThread.tsx",
      "packages/dispatch/web/src/features/agent-view/ReferenceTriggers.tsx",
    ],
    issueRows: false,
    reach: "[data-agent-message]",
    write: "agent-message",
  },
  // The replacement text of a suggestion, beside the reason for it: both go in the one comment.
  // A party picked here is written as a reference like any other, but until someone accepts the
  // suggestion its card shows the replacement as the characters written, with no name drawn
  // over it (`margin-suggestion-diff`). Nobody is told of it: the delivery a comment makes is
  // read from its body alone, by the browser (`draftParties`) and by the server
  // (`commentPartiesOf` over `text.ExtractAt(input.Body)`), and neither reads
  // `suggestion.replace_with`.
  {
    box: "suggestion-replacement",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/conversation/MentionComposer.tsx"],
    issueRows: true,
    reach: "Replacement",
    write: "suggestion",
  },
  // An ask's options, which go out with the question and tell nobody by themselves.
  {
    box: "ask-option-label",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/conversation/MentionComposer.tsx"],
    issueRows: true,
    reach: "Option 1 label",
    write: "ask",
  },
  {
    box: "ask-option-description",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/conversation/MentionComposer.tsx"],
    issueRows: true,
    reach: "Option 1 description",
    write: "ask",
  },
  // The ask body's one box, which the Inbox and a document's decision block both draw (through
  // the ask card): an answer goes to whoever asked, and a question back is a comment on the ask.
  {
    box: "ask-answer",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/ask-body/AskBody.tsx"],
    issueRows: true,
    reach: "[data-ask-answer]",
    write: "ask-answer",
  },
  {
    box: "ask-answer",
    covered: true,
    delivers: true,
    files: ["packages/dispatch/web/src/features/ask-body/AskBody.tsx"],
    issueRows: true,
    reach: "[data-ask-answer]",
    write: "ask-back",
  },
  // The note on declining to answer, which the ask card's own Decline to answer opens and
  // which is the person's to leave empty. It is written onto the ask's resolution, where the
  // asking agent reads it beside the sentences Dispatch writes there; the decline route
  // resolves no party out of it, as only the comment route does, so a party picked here is a
  // reference for a reader and nobody is told.
  {
    box: "ask-decline-note",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/ask-body/DeclineControl.tsx"],
    issueRows: true,
    reach: "Note (optional)",
    write: "ask-decline",
  },
  {
    box: "ask-thread-reply",
    covered: true,
    delivers: true,
    files: ["packages/dispatch/web/src/features/inbox/AskThread.tsx"],
    issueRows: true,
    reach: "Reply",
    write: "comment",
  },
  // The issue's Decisions panel and the margin, which draw a change through the decision card,
  // and a decision's row on the new issue page: a change is a comment anchored to the decision
  // it quotes.
  {
    box: "decisions-change",
    covered: true,
    delivers: true,
    files: [
      "packages/dispatch/web/src/features/decision-card/DecisionCard.tsx",
      "packages/dispatch/web/src/features/issue-page/DecisionActions.tsx",
    ],
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
    files: ["packages/dispatch/web/src/features/issue/CreateIssueDialog.tsx"],
    issueRows: false,
    reach: "Spec (optional)",
    write: "issue-spec",
  },
  {
    box: "approval-reason",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/doc/ApprovalChip.tsx"],
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
    files: ["packages/dispatch/web/src/features/agents/AgentsPage.tsx"],
    issueRows: false,
    reach: "Broadcast message",
    write: "broadcast",
  },
  {
    box: "architecture-reason",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/architecture/AttachmentLists.tsx"],
    issueRows: true,
    reach: "Why <ISSUE> is not architectural",
    write: "architecture-reason",
  },
  // A priority's description, written where it is read: on the priority's own page and on the
  // open row of the priorities table, which both draw it in the documents' editor
  // (`PriorityDescription`), whose picker the editor hosts as a document's body does
  // (`DocumentRefPicker`, which `PriorityDescription` mounts over the editor `editor.ts` creates)
  // under this row. The priority write resolves no party out of it and indexes nothing from it,
  // so a party picked here is a reference for a reader and nobody is told. A priority belongs to
  // no issue, so the picker lists everyone who has signed in.
  {
    box: "priority-description",
    covered: true,
    delivers: false,
    files: [
      "packages/dispatch/web/src/features/priorities/PriorityDescription.tsx",
      "packages/dispatch/web/src/features/doc/editor.ts",
    ],
    issueRows: false,
    reach: "/priorities/<ID>, the Description editor",
    write: "priority-description",
  },
  // A priority's title, the heading of its own page, which opens into a `textarea` as an issue's
  // title does, so a long title wraps as the heading does (`InPlaceField`'s `closed="heading"`).
  {
    box: "priority-title",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/priorities/InPlaceField.tsx"],
    issueRows: false,
    reach: "/priorities/<ID>, the Title heading",
    reason: "A title is one line of text that holds no markup, so the box takes no picker.",
    write: "priority-title",
  },
  // The new issue page's title, which is its own editor (`IssueTitleRow.tsx`): a `textarea` so
  // it wraps as the heading it stands over does, where today's page edits the title in an
  // `<input>`. Neither takes a picker.
  {
    box: "issue-title",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/issue-page/IssueTitleRow.tsx"],
    issueRows: true,
    reach: "Issue title",
    reason: "A title is one line of text that holds no markup, so the box takes no picker.",
    write: "issue-title",
  },
  // A project's Settings tab, its delivery section (`DeliverySettingsSection.tsx`). These three
  // take no picker, and the spec's exclusion is why.
  {
    box: "delivery-production-jobs",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/project/DeliverySettingsSection.tsx"],
    issueRows: false,
    reach: "Production jobs",
    reason: SETTINGS_REASON,
    write: "delivery-settings",
  },
  {
    box: "delivery-population-authors",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/project/DeliverySettingsSection.tsx"],
    issueRows: false,
    reach: "Population authors",
    reason: SETTINGS_REASON,
    write: "delivery-settings",
  },
  {
    box: "delivery-excluded-repos",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/project/DeliverySettingsSection.tsx"],
    issueRows: false,
    reach: "Excluded repositories",
    reason: SETTINGS_REASON,
    write: "delivery-settings",
  },
  // A deploy's drill-down on a project's Evals tab (`DrillDown.tsx`): the note a person's Flag
  // writes with the failure it records, and the reason Reject writes on a counted failure.
  {
    box: "delivery-flag-note",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/delivery/DrillDown.tsx"],
    issueRows: false,
    reach: "/projects/<PROJECT>/evals, a deploy's drill-down, Flag, the Note box",
    reason: PRODUCTION_EVENT_REASON,
    write: "production-failure",
  },
  {
    box: "delivery-reject-reason",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/delivery/DrillDown.tsx"],
    issueRows: false,
    reach: "/projects/<PROJECT>/evals, a deploy's drill-down, a failure's Reject, the Reason box",
    reason: PRODUCTION_EVENT_REASON,
    write: "failure-rejection",
  },
];

/**
 * The row for one write of one box, which the box reads at the mount. A pair with no row is a
 * box that writes something this list does not hold, which is a mistake in one or the other, so
 * it throws.
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
