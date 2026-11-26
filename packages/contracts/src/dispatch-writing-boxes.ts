/**
 * Every box a person writes prose in, and every write each one makes.
 *
 * The dashboard mounts one text field in most of them (`MentionField.tsx`), and the two boxes
 * that are editors of their own - a document's body and an agent's live conversation - host the
 * same picker in their own way, so the picker is in every box rather than in the composer
 * alone. This table is what the three readers of that fact share: each box reads its own hint
 * here at the mount, so the words under a box and the words in this list cannot disagree; a
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
 * - `ask-decline`: a person's note on declining to answer an ask, which closes it with no
 *   answer; the note is stored on the ask's resolution and read by whoever asked.
 * - `message-reply`: a reply in an issue's message thread, delivered to the thread's target.
 * - `agent-message`: a message to one agent's session.
 * - `broadcast`: one message to each selected agent.
 * - `document-text`: the document's own text, which every reader of that document reads and
 *   which is sent to nobody.
 * - `criterion`: a criterion, written into the issue's own text under the comment, question or
 *   message it answers, which every reader of the issue reads and which is sent to nobody.
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
  | "agent-message"
  | "broadcast"
  | "document-text"
  | "criterion"
  | "issue-spec"
  | "approval-reason"
  | "architecture-reason"
  | "priority-description"
  | "priority-title"
  | "issue-title"
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
   *  person's reference is their name with no link until a person has a page (R5), which is why
   *  a delivering box says so. */
  readonly delivers: boolean;
  /** Whether the picker here lists the rows that need an issue - the issue's people. A box that
   *  writes on no issue lists everyone who has signed in instead. For a box whose surface is
   *  sometimes a project document, this says what it does on an issue. */
  readonly issueRows: boolean;
  /** The line under the box, which the box itself reads from this row at the mount: the doors,
   *  and what a pick does on this write - whether a picked agent or role is told and, where
   *  nobody picked is told, what does tell (R4). Empty for a box that is not covered: with no
   *  picker in it, it has no such line to show. It is never written into a row: it follows from
   *  the row's other fields (`writingHint`). */
  readonly hint: string;
  /** Why the box is not covered. Only a row whose `covered` is false carries one. */
  readonly reason?: string;
}

/** How each door is opened, which every hint starts with. */
const DOORS = "Type @ for an agent, a role or a person, # for an issue, [[ for anything else.";

/** What a box says when a mention on its write is delivered. The comment route tells an agent
 *  or a role the body names and never a person (`commentPartiesOf` in the Go API's
 *  comment_create.go), and it tells a role only while a session holds it: a role nobody holds is
 *  refused at once ("no holder for role …", `resolveDeliveryTarget`) and kept for no later
 *  holder, though the picker offers the issue's stopped roles and the reader links them. A
 *  person's reference is drawn as their name with no link until a person has a page (R5). */
export const DISPATCH_WRITING_HINT_DELIVERED = `${DOORS} A picked agent or role is told while a session holds it. A role that is not running gets a link and is not told. A picked person shows as a name without a link and is not told.`;

/** What a box whose write tells nobody it names says of a pick, before it says what does tell:
 *  every such write but a suggestion's, whose replacement shows no link (below). It says nothing
 *  of a person's link, since a person's reference has none. */
const PICKED_QUIET =
  "A picked agent or role gets a link, and a picked person shows as a name without one. Nobody you pick is told.";

/**
 * What a box says when its write tells nobody it names, by the write: that a pick links and
 * tells nobody, and then what does tell, which is the write's own doing. Only the comment route
 * reads the parties of a body and delivers to them (`createCommentFor` in the Go API's
 * comment_create.go), so each sentence says where the write itself goes, or, for a write that
 * goes nowhere, which comment would tell a picked agent or role, or that nothing would. A
 * suggestion's replacement box does not say that a pick links. Until someone accepts the
 * suggestion, its card shows the replacement as the characters written, with no link and no
 * name drawn over a reference (the reading list's `margin-suggestion-diff`), so the box says
 * that instead.
 *
 * - a message reply goes to the target of its thread, and to whoever wrote the message it
 *   answers (`createStoredMessage`, and the outbox's `addAuthorRoutes`);
 * - an edit of a comment resolves no party and opens no delivery (`editComment`): only a new
 *   comment tells a party the edit adds;
 * - an ask waits in the Inbox of its issue's assignee, or of its project's default assignee
 *   (ask_asked_of.go);
 * - a message to one agent goes to that session, and a broadcast to each selected session
 *   (`createAgentMessage`, broadcasts.go);
 * - a decline's note rides the ask's resolution to whoever asked (`declineAsk`, and the outbox's
 *   `addFollowerRoutes`);
 * - a reason for asking changes answers the open approval request, which reaches whoever asked
 *   (`createArtifactReview`); with none open, it rides the review's own event alone, on the
 *   document's issue (`docs.WriteReview`);
 * - a criterion is written into the issue's own text (an edit of the issue's tree artifact),
 *   which opens no delivery: a comment on the issue tells a picked agent or role, since a reply
 *   under a message is a message reply, which tells nobody it names;
 * - a priority's write is published to nobody (`priorityEvent`), and a priority takes no
 *   comment.
 */
export const DISPATCH_WRITING_HINT_QUIET = {
  "agent-message": `${DOORS} ${PICKED_QUIET} The message goes to this agent.`,
  "approval-reason": `${DOORS} ${PICKED_QUIET} An open approval request sends the reason to whoever asked.`,
  "architecture-reason": `${DOORS} ${PICKED_QUIET} A comment on the issue tells a picked agent or role.`,
  ask: `${DOORS} ${PICKED_QUIET} The ask goes to the assignee's Inbox.`,
  "ask-decline": `${DOORS} ${PICKED_QUIET} The note goes to whoever asked.`,
  broadcast: `${DOORS} ${PICKED_QUIET} The message goes to each agent you selected.`,
  "comment-edit": `${DOORS} ${PICKED_QUIET} A new comment, not an edit, tells a picked agent or role.`,
  criterion: `${DOORS} ${PICKED_QUIET} The criterion goes under the comment, question or message it answers. A comment on the issue tells a picked agent or role.`,
  "document-text": `${DOORS} ${PICKED_QUIET} A comment on the document tells a picked agent or role.`,
  "issue-spec": `${DOORS} ${PICKED_QUIET} A comment on the new issue tells a picked agent or role.`,
  "message-reply": `${DOORS} ${PICKED_QUIET} Your reply goes to whoever the thread is with.`,
  "priority-description": `${DOORS} ${PICKED_QUIET} Nothing on a priority tells anyone.`,
  suggestion: `${DOORS} Until someone accepts the suggestion, the replacement shows the characters you write. Nobody you pick is told. A pick in the Reason box tells an agent or a role.`,
} as const satisfies Partial<Record<DispatchWriting, string>>;

/** What the ask card's one box says, since the reader chooses there which write it makes: an
 *  answer reaches whoever asked and tells nobody it names, while a question back is a comment,
 *  which tells what a delivering box's line says it tells (`DISPATCH_WRITING_HINT_DELIVERED`): a
 *  picked agent or role while a session holds it, never a role that is not running, which still
 *  gets a link, and never a person. Both of that box's rows carry it, since the box shows one
 *  line for the two. */
export const DISPATCH_WRITING_HINT_ASK = `${DOORS} An answer goes to whoever asked and tells nobody you pick. Ask back to tell a picked agent or role while a session holds it. A role that is not running gets a link and is not told. A picked person shows as a name without a link and is not told.`;

/** Why the delivery settings boxes have no picker. */
const SETTINGS_REASON =
  "One setting per line rather than prose: a job name, a login or an owner/repo, which a reference would not name.";

/** The words of the writes that tell nobody they name, by write, for `writingHint` to look up. */
const QUIET_WORDS: Partial<Record<DispatchWriting, string>> = DISPATCH_WRITING_HINT_QUIET;

/**
 * The line under a box, from what its write does: none where there is no picker; the ask card's
 * one line for its two writes, since the reader chooses there which one they make; the line of a
 * write that tells a picked agent or role; and otherwise the words of the write that tells nobody
 * it names, which say what does. A covered write that tells nobody and has no words of its own
 * is a mistake in this list, so it throws rather than leaving the box with no words.
 */
function writingHint(row: Omit<DispatchWritingBoxRow, "hint">): string {
  if (!row.covered) return "";
  if (row.box === "ask-answer") return DISPATCH_WRITING_HINT_ASK;
  if (row.delivers) return DISPATCH_WRITING_HINT_DELIVERED;
  const words = QUIET_WORDS[row.write];
  if (words === undefined) {
    throw new Error(`${row.box} writing ${row.write} tells nobody it names and has no words`);
  }
  return words;
}

const WRITING_BOXES: readonly Omit<DispatchWritingBoxRow, "hint">[] = [
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
  // Its criterion line writes a criterion into the issue's own text, which tells nobody, and says
  // so in words of its own rather than the document editor's. No row offers Write a criterion
  // until the page reads the issue's tree, so no control reaches the line yet.
  {
    box: "issue-page-composer",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/issue-page/IssueComposer.tsx"],
    issueRows: true,
    reach: "[data-issue-composer]",
    reason:
      "No control reaches it yet: Write a criterion is offered only once the page reads the issue's tree.",
    write: "criterion",
  },
  // The Inbox's reading column: one reply box, under a comment thread or a message thread.
  {
    box: "inbox-thread-reply",
    covered: true,
    delivers: true,
    files: ["packages/dispatch/web/src/features/inbox/ThreadBody.tsx"],
    issueRows: true,
    reach: "Reply",
    write: "comment",
  },
  {
    box: "inbox-thread-reply",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/inbox/ThreadBody.tsx"],
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
  // `suggestion.replace_with`. So this box says both rather than promising a link or a delivery
  // no reader of the write would ever see.
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
  // The ask card's one box, which the Inbox and a document's decision block both draw: an
  // answer goes to whoever asked, and a question back is a comment on the ask.
  {
    box: "ask-answer",
    covered: true,
    delivers: false,
    files: ["packages/dispatch/web/src/features/inbox/AskCard.tsx"],
    issueRows: true,
    reach: "[data-ask-answer]",
    write: "ask-answer",
  },
  {
    box: "ask-answer",
    covered: true,
    delivers: true,
    files: ["packages/dispatch/web/src/features/inbox/AskCard.tsx"],
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
    files: ["packages/dispatch/web/src/features/inbox/DeclineControl.tsx"],
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
  // The issue's Decisions panel, and a decision's row on the new issue page: a change is a
  // comment anchored to the decision it quotes.
  {
    box: "decisions-change",
    covered: true,
    delivers: true,
    files: [
      "packages/dispatch/web/src/features/issue/DecisionsPanel.tsx",
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
  // Settings' delivery form. These three take no picker, and the spec's exclusion is why.
  {
    box: "delivery-production-jobs",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/delivery/DeliverySettingsForm.tsx"],
    issueRows: false,
    reach: "Production jobs",
    reason: SETTINGS_REASON,
    write: "delivery-settings",
  },
  {
    box: "delivery-population-authors",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/delivery/DeliverySettingsForm.tsx"],
    issueRows: false,
    reach: "Population authors",
    reason: SETTINGS_REASON,
    write: "delivery-settings",
  },
  {
    box: "delivery-excluded-repos",
    covered: false,
    delivers: false,
    files: ["packages/dispatch/web/src/features/delivery/DeliverySettingsForm.tsx"],
    issueRows: false,
    reach: "Excluded repositories",
    reason: SETTINGS_REASON,
    write: "delivery-settings",
  },
];

export const DISPATCH_WRITING_BOXES: readonly DispatchWritingBoxRow[] = WRITING_BOXES.map(
  (row) => ({ ...row, hint: writingHint(row) })
);

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
