/**
 * Every surface the dashboard shows a stored record on, and how each one draws it.
 *
 * One renderer draws a record wherever a reader meets one: `MarkdownBody` for a body and
 * `MarkdownPreview` for a line or two of it, both over the one engine. This table is what the
 * readers of that fact share: a browser row drives each surface by the route and handle under
 * `reach`, and a surface that is drawn some other way says here why, so the exception lives in
 * one list rather than in a comment beside each call site.
 *
 * A row is one surface. Two mounts that draw the same thing in two layouts - a margin card
 * collapsed and open - are one surface and one row; two things drawn in one component - a
 * session's turn and its reasoning - are two.
 *
 * A row is excluded when either of its two answers is no:
 *
 * - `rendered` is false: the surface draws its text some way other than the one renderer.
 * - `record` is false: what it draws is no record of this Dispatch, so the renderer is given no
 *   `source` and a `#id` in the text names nothing.
 *
 * Either way the row carries the `reason`, and the call site carries a pointer to the row
 * rather than a second copy of the words that can drift from it.
 *
 * Every mount of the renderer, of a reference card and of the plain-text projection names its
 * row in its own `surface`, typed by `DispatchReadingSurface` so a misspelt id does not compile.
 * A unit test (`surface-lists.test.ts`) holds this list to those mounts, and fails on a stored
 * text the dashboard draws as its characters with no excluded row named beside it.
 */

/** Where a reader reads a stored record: one id per surface, whatever route draws it. */
export type DispatchReadingSurface =
  | "activity-event-body"
  | "event-line"
  | "earlier-chosen-option"
  | "agents-row-reply"
  | "agents-row-message"
  | "agent-turn-text"
  | "agent-turn-reasoning"
  | "broadcast-recipient-reply"
  | "broadcast-sent-body"
  | "broadcast-send-row"
  | "broadcasts-list-body"
  | "answers-subject"
  | "answers-answer"
  | "answers-chosen-options"
  | "answers-reply"
  | "component-prose"
  | "not-architectural-reason"
  | "conversation-reply"
  | "conversation-message"
  | "conversation-targeted-message"
  | "reply-quote"
  | "approval-review-reason"
  | "agent-held-send"
  | "session-switch-answer"
  | "conversation-handoff-body"
  | "conversation-handoff-data"
  | "ask-card-question"
  | "ask-edit-history"
  | "ask-answer-history"
  | "answered-ask"
  | "resolved-ask"
  | "ask-option-text"
  | "ask-thread-reply"
  | "inbox-ask-row"
  | "inbox-control-name"
  | "approval-ask-question"
  | "inbox-thread-turn"
  | "my-reply-subject"
  | "inbox-thread-title"
  | "issue-decision"
  | "decision-heading-words"
  | "decision-change-quote"
  | "issue-open-question"
  | "item-match-first-line"
  | "spec-requirement-words"
  | "spec-version-summary"
  | "artifact-version-summary"
  | "change-view-diff"
  | "credential-request-reason"
  | "issue-components-reason"
  | "margin-decision-passage"
  | "margin-pinned-conversation"
  | "margin-suggestion-diff"
  | "margin-comment-body"
  | "margin-thread-preview"
  | "anchor-quote"
  | "kept-draft"
  | "document-body"
  | "document-version-body"
  | "document-open-decision"
  | "check-text"
  | "check-negative-control"
  | "check-result-observed"
  | "check-result-negative-control"
  | "check-retraction-reason"
  | "referenced-by-excerpt"
  | "referenced-by-title"
  | "artifacts-reference-name"
  | "person-node-title"
  | "priority-description"
  | "body-reference-cards"
  | "dispatch-reference-card"
  | "reference-card-words"
  | "github-reference-card"
  | "reference-hover-card"
  | "reference-hover-card-ask"
  | "search-ask-status"
  | "search-ask-chosen-options"
  | "search-result"
  | "ref-picker-row-label"
  | "agent-message-issue-option"
  | "issue-page-title"
  | "node-row-words"
  | "node-row-child-title"
  | "node-row-first-line"
  | "node-row-parent-line"
  | "node-row-chip"
  | "outline-crumb"
  | "issue-filed-from"
  | "issue-pin"
  | "issue-page-history-turn"
  | "issue-page-history-handoff-body"
  | "issue-page-history-handoff-data"
  | "issue-page-history-block"
  | "plan-task-line"
  | "spec-decision-line"
  | "document-page-title"
  | "agent-artifact-page-title"
  | "write-label"
  | "delivery-production-jobs"
  | "delivery-population-authors"
  | "delivery-excluded-repos";

/**
 * The seven ways a stored text names a node, each of which a rendered surface reads as R8 says
 * and the reading list's walk checks: an issue key written bare, a `dispatch://` address, a URL of
 * this dashboard, a question block's `#id`, an address whose item id is written short and a
 * mention of a party each read as the node's name, and a link to a node whose words its author
 * wrote reads as those words; each opens its node. Six of them are read from the text and the
 * graph alone. A question id is read against the document the text's own record belongs to, so a
 * surface whose text is no record, or is a record that belongs to no document, cannot carry it.
 */
export const DISPATCH_READING_FORMS = [
  "bare-key",
  "address",
  "dashboard-url",
  "question-id",
  "short-id",
  "party",
  "worded-link",
] as const;

export type DispatchReadingForm = (typeof DISPATCH_READING_FORMS)[number];

/** One row of DISPATCH_READING_SURFACES: one surface a reader reads a record on. */
export interface DispatchReadingSurfaceRow {
  readonly surface: DispatchReadingSurface;
  /** The source files that draw it, from the repository root. A rendered surface is found in
   *  one of them by a mount of the renderer, a card or the plain-text projection whose `surface`
   *  names this row; an excluded one by the comment there naming this row, which is the whole
   *  tie between the reason and the place it excuses. */
  readonly files: readonly string[];
  /** How a test reaches it: the route that draws it and the handle under that route. A route
   *  that names an issue, a project, a document, a session or a person writes that part as
   *  `<ISSUE>`, `<PROJECT>`, `<SLUG>`, `<SESSION>` or `<LOGIN>`. */
  readonly reach: string;
  /** Whether the text it draws is a record of this Dispatch, which the renderer is given as its
   *  `source` so a `#id` in the text names a block of that record's document. A surface whose
   *  text is nobody's record says why in `reason`. */
  readonly record: boolean;
  /** Whether it draws through the one renderer, `MarkdownBody` or `MarkdownPreview`. A surface
   *  that does not says why in `reason`. */
  readonly rendered: boolean;
  /** Why the surface is excluded: why it does not render through the one renderer, or why its
   *  text is no record. Only a row with `record` or `rendered` false carries one. */
  readonly reason?: string;
  /** The forms of `DISPATCH_READING_FORMS` this surface cannot carry, and why. A rendered
   *  surface whose text is no record names the question id here, and so does one whose record
   *  belongs to no document. The row's browser driver reads this, so the text it stores holds
   *  every other form and the surface is held to each of them (`e2e/reading-rows.ts`). */
  readonly cannotCarry?: DispatchReadingUncarried;
  /** The test that proves a row the walk does not drive reads its text as the row says: a row
   *  drawn through the plain-text projection, which reads its text as the one reader does, or a
   *  card a reader of its own draws. Each is a browser driver or a unit test, and a row whose
   *  claim takes more than one test names each of them. Such a row has no driver in the walk,
   *  which drives the rendered rows alone, so this names what holds it; the walk's gate fails
   *  when a file does not exist or holds no test of that title
   *  (`e2e/__tests__/reading-walk.test.ts`), and the source test fails when a row with a mount of
   *  the projection names none (`surface-lists.test.ts`). */
  readonly proof?: DispatchReadingProof | readonly DispatchReadingProof[];
  /** The kinds of record a rendered surface shows that carry fewer forms than the rest of it -
   *  each with its own driver in the walk, which stores the forms that arm carries. A row's own
   *  `cannotCarry` is what its other records cannot carry. */
  readonly arms?: readonly DispatchReadingArm[];
}

/** A test, by the file that holds it, from the repository root, and its own title - the title
 *  its `test` call is given, not the titles of the `describe` blocks around it. */
export interface DispatchReadingProof {
  readonly file: string;
  readonly test: string;
}

/** One kind of record a rendered surface shows that carries fewer forms than the rest of it. */
export interface DispatchReadingArm {
  /** The arm's id: its driver's title opens with the row's id, a `/` and this, and then a
   *  colon (`agents-row-message/direct-message:`). */
  readonly arm: string;
  /** How a test reaches the arm, as the row's own `reach` says it for the row. */
  readonly reach: string;
  /** What the arm's records cannot carry, and why. */
  readonly cannotCarry: DispatchReadingUncarried;
}

/** The forms a surface cannot carry, and what about the surface's text makes that so. */
export interface DispatchReadingUncarried {
  readonly forms: readonly DispatchReadingForm[];
  readonly why: string;
}

/** What a rendered surface whose text is no record cannot carry, which is what each such row's
 *  `reason` says of its own text. */
const NO_QUESTION_ID: DispatchReadingUncarried = {
  forms: ["question-id"],
  why: "The text is no record, so the renderer is given no `source` for it and a `#id` in it names nothing.",
};

/** What a message sent to one session on no issue cannot carry: it is a record, but of no
 *  document, so the question map its `#id` is read against is empty. */
const NO_DOCUMENT: DispatchReadingUncarried = {
  forms: ["question-id"],
  why: "A message sent to one session on no issue belongs to no document whose question a `#id` could name (`refs.ScopeOf`), so a `#id` in it, or in a reply to it, names nothing.",
};

/** How a test reaches an Agents row's message sent to its session alone, and a reply to one. */
const DIRECT_MESSAGE_REACH =
  "/agents, a row's message sent to its session on no issue (`POST /api/v1/agents/{session_id}/messages`), and the session's reply to it";

/** The one test that holds the Decisions panel's projected links to the reader: an open
 *  question's link (and the name its Change is given, which is `decision-heading-words`). */
const DECISIONS_PANEL_PROOF: DispatchReadingProof = {
  file: "packages/dispatch/web/src/features/issue/DecisionsPanel.test.tsx",
  test: "an open question and the Change's name read their text's plain-text projection",
};

/** Why a broadcast's own text is no record: the graph holds the copy each recipient got, never
 *  the broadcast, which is what the three broadcast surfaces say. */
const BROADCAST_REASON =
  "A broadcast is no record of the reference graph - the graph holds the copy each recipient got, never the broadcast - so a `#id` in it declares nothing and names nothing.";

/** Why a title bar and an `<option>` hold the characters they are given: each is an element that
 *  can hold no markup, and what goes in it is one line of text already. */
const NO_MARKUP_REASON =
  "The element holds no markup, and the line it shows is one line of text already - no markdown body to render or strip - so it goes in as the characters it is.";

/** Why the delivery settings lists take neither the renderer nor the picker, which is the same
 *  reason `DISPATCH_WRITING_BOXES` gives their three boxes. */
const SETTINGS_REASON =
  "One setting per line rather than prose: a job name, a login or an owner/repo, which is stored and shown as the characters it is.";

/** Why an issue's reason for touching no component is no record, which is what the two surfaces
 *  that draw it say: the Architecture page's Not architectural list and the issue header's
 *  Components line. */
const NOT_ARCHITECTURAL_REASON =
  "An issue's reason for touching no component is no record of the reference graph: it is none of the four kinds a `TextSource` names (an artifact, an ask, a comment, a message), and the components write stores it as an attribute of the issue's attachment and indexes nothing from it - so a `#id` in it declares nothing and names nothing.";

/** Why a link's words, a `title` and a control's name draw a stored text as plain words: none of
 *  the three can hold markup, so each takes the one reader's plain-text projection of the text
 *  rather than the text as its writer wrote it. */
const PLAIN_WORDS_REASON =
  "The place holds no markup, so it draws the text's plain-text projection (`PlainText`, `usePlainHeadline`): its words with the formatting gone and each reference read as its target's name, never the text's source.";

/** What a place that draws the projection shows before the reader's engine has answered, where
 *  a blank there would leave a link with no words or a control with no name. */
const LOAD_STATE_REASON =
  "Until the reader's engine has answered, the place carries the text as written, which is a load state rather than a second reader.";

/** What a node's one line on the new issue page cannot carry: a row of the outline reads no
 *  question map, and its one line names a reference by the same rule its row does
 *  (`node-row-words`), so the two draw a `#id` alike. */
const NODE_LINE_NO_QUESTION_ID: DispatchReadingUncarried = {
  forms: ["question-id"],
  why: "A node's one line names its references by the rule its row does: a `#id` is read against its document's questions, one read for each document, which no row of the outline makes (`row-references.ts`), so it is drawn as written, in the line as in the row.",
};

/** Why what a check says and what a run of it printed are shown as written, which is what the
 *  five surfaces of the Verification tab that draw them say. */
const COMMAND_OUTPUT_REASON =
  "A check's words and a result's report are commands and what running them printed - a shell line, a log, a diff - where a `*`, a `_` or a `[` is a character of the output, so reading them as markdown would change what they say. They are shown as the characters they are.";

/** Why a one-line field a writer fills in is shown as written, which is what a version's
 *  summary and a credential request's reason say. */
const ONE_LINE_FIELD_REASON =
  "It is a one-line field its writer fills in, which the reference framework leaves out, as it leaves a title: it is stored and shown as the characters its writer gave it.";

/** Why the Proof editor's own drawing of a document is no mount of the renderer, which is what
 *  the live document and a past version of one both say. */
const EDITOR_REASON =
  "The Proof editor paints a document's body itself, from the document it edits, and draws each reference the body holds as a decoration over the stored text (`doc/document-references.tsx`, `@legion/proof-editor`'s `src/reference-decorations.ts`) - the renderer draws a read-only copy and the editor cannot be one. The references are read by the one reader every rendered body is read by (`refs/text-references.ts`), so a bare key, a `#id`, an address and a mention resolve by the same rules there.";

export const DISPATCH_READING_SURFACES: readonly DispatchReadingSurfaceRow[] = [
  // An issue's Activity tab: one event's body, which is an ask's question, its answer, its
  // resolution reason, or the body of a comment or a message. The tab lists each event's line;
  // its body is drawn once the event is pinned, in the margin's Pinned tab.
  {
    files: ["packages/dispatch/web/src/features/activity/EventBody.tsx"],
    reach: "/issues/<ISSUE>/activity, pin an event, then the margin's Pinned tab",
    record: true,
    rendered: true,
    surface: "activity-event-body",
  },
  // An event's one line, in the Activity tab and above its body in the margin's Pinned tab. The
  // line is in no link or button, so a text of the record it quotes is drawn by the renderer.
  {
    files: [
      "packages/dispatch/web/src/features/activity/SentenceParts.tsx",
      "packages/dispatch/web/src/features/activity/activity-model.ts",
      "packages/dispatch/web/src/features/issue/event-description.ts",
    ],
    reach: "/issues/<ISSUE>/activity, an event's line; the same line in the margin's Pinned tab",
    record: true,
    rendered: true,
    surface: "event-line",
  },
  // The Agents page: a reply under a row's exchange, and the targeted message a row shows. A row
  // shows a message sent on an issue and one sent to the session alone, on no issue, and only
  // the first belongs to a document a `#id` could name a question of.
  {
    arms: [{ arm: "direct-message", cannotCarry: NO_DOCUMENT, reach: DIRECT_MESSAGE_REACH }],
    files: ["packages/dispatch/web/src/features/agents/AgentsPage.tsx"],
    reach: "/agents, a row's reply turn to a message sent on an issue",
    record: true,
    rendered: true,
    surface: "agents-row-reply",
  },
  {
    arms: [{ arm: "direct-message", cannotCarry: NO_DOCUMENT, reach: DIRECT_MESSAGE_REACH }],
    files: ["packages/dispatch/web/src/features/agents/AgentsPage.tsx"],
    reach: "/agents, a row's targeted message card for a message sent on an issue",
    record: true,
    rendered: true,
    surface: "agents-row-message",
  },
  // A session's live conversation, which Dispatch stores nothing of and indexes nothing from.
  {
    cannotCarry: NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/agent-view/AgentThread.tsx"],
    reach: "/agents/<SESSION>/live, a turn's text part",
    reason:
      "A session's own turn is no record of the reference graph - it is the live transcript, which Dispatch stores nothing of and indexes nothing from - so a `#id` in it declares nothing and names nothing.",
    record: false,
    rendered: true,
    surface: "agent-turn-text",
  },
  {
    cannotCarry: NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/agent-view/AgentThread.tsx"],
    reach: '/agents/<SESSION>/live, `[data-testid="agent-reasoning"]`',
    reason:
      "The reasoning beside a turn is that same live transcript, stored nowhere and indexed from nowhere.",
    record: false,
    rendered: true,
    surface: "agent-turn-reasoning",
  },
  // The header's line about a `/model` or `/effort` switch, and a message of the viewer's that
  // Dispatch has not made yet. The switch's reply is a message of the session's conversation,
  // sent on no issue.
  {
    cannotCarry: NO_DOCUMENT,
    files: ["packages/dispatch/web/src/features/agent-view/SessionSettings.tsx"],
    reach: "/agents/<SESSION>/live, the header's line after a model or thinking-level switch",
    record: true,
    rendered: true,
    surface: "session-switch-answer",
  },
  {
    cannotCarry: NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/conversation/PendingSendTurn.tsx"],
    reach: '/agents/<SESSION>/live, `[data-testid="agent-send-held"]`',
    reason:
      "A message Dispatch has not made yet is the viewer's own draft rather than a record: the page parses it, as it parses every write the server has not stored, and draws it as the message it becomes, but it belongs to no document yet, so a `#id` in it names nothing until Dispatch makes it and the stored message takes its place.",
    record: false,
    rendered: true,
    surface: "agent-held-send",
  },
  // A broadcast's page: a recipient's reply is a message and a record; the broadcast's own body
  // is not. The reply answers the copy the recipient got, which is a message on no issue.
  {
    cannotCarry: {
      forms: ["question-id"],
      why: "A recipient's reply answers the copy of the broadcast it got, a message on no issue, so it belongs to no document whose question a `#id` could name (`refs.ScopeOf`).",
    },
    files: ["packages/dispatch/web/src/features/agents/BroadcastPage.tsx"],
    reach: "/agents/broadcasts/<ID>, a recipient row's reply",
    record: true,
    rendered: true,
    surface: "broadcast-recipient-reply",
  },
  {
    cannotCarry: NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/agents/BroadcastPage.tsx"],
    reach: "/agents/broadcasts/<ID>, the body under the heading",
    reason: BROADCAST_REASON,
    record: false,
    rendered: true,
    surface: "broadcast-sent-body",
  },
  {
    cannotCarry: NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/agents/BroadcastSends.tsx"],
    reach: "/agents, the Sends strip's row for a send Dispatch refused",
    reason: `${BROADCAST_REASON} A send still queued has not reached the server at all.`,
    record: false,
    rendered: true,
    surface: "broadcast-send-row",
  },
  {
    cannotCarry: NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/agents/BroadcastsPage.tsx"],
    reach: "/agents/broadcasts, a row's one-line body",
    reason: BROADCAST_REASON,
    record: false,
    rendered: true,
    surface: "broadcasts-list-body",
  },
  // The Answers page: the subject a row answers, the options an answer chose, the answer, and
  // the reply that carried it. The options an answer chose are drawn the same way on the new
  // issue page, under the question of an ask that offers no list to mark them in.
  {
    files: ["packages/dispatch/web/src/features/answers/AnswersPage.tsx"],
    reach: "/answers, a row's subject line",
    record: true,
    rendered: true,
    surface: "answers-subject",
  },
  {
    files: ["packages/dispatch/web/src/features/answers/AnswersPage.tsx"],
    reach: "/answers, a row's answer text",
    record: true,
    rendered: true,
    surface: "answers-answer",
  },
  {
    files: [
      "packages/dispatch/web/src/features/answers/AnswersPage.tsx",
      "packages/dispatch/web/src/features/issue-page/NodeRow.tsx",
    ],
    reach: "/answers, a row's chosen options",
    record: true,
    rendered: true,
    surface: "answers-chosen-options",
  },
  {
    files: ["packages/dispatch/web/src/features/answers/AnswersPage.tsx"],
    reach: "/answers, a row's reply body",
    record: true,
    rendered: true,
    surface: "answers-reply",
  },
  // A component's prose, which belongs to the architecture model rather than to Dispatch.
  {
    cannotCarry: NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/architecture/ComponentDetails.tsx"],
    reach: "/projects/<PROJECT>/architecture, a component's Code & definition prose",
    reason:
      "A component's prose is the architecture model's, read from the repository, not a record of the reference graph - so a `#id` in it declares nothing and names nothing.",
    record: false,
    rendered: true,
    surface: "component-prose",
  },
  // An issue's reason for touching no component, on its row of the Not architectural list. The
  // issue header's Components line draws the same reason (`issue-components-reason`).
  {
    cannotCarry: NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/architecture/AttachmentLists.tsx"],
    reach: "/projects/<PROJECT>/architecture?view=none, a Not architectural row's reason",
    reason: NOT_ARCHITECTURAL_REASON,
    record: false,
    rendered: true,
    surface: "not-architectural-reason",
  },
  // An issue's Conversation tab: a reply, a message turn and a targeted message turn.
  {
    files: ["packages/dispatch/web/src/features/conversation/ConversationTab.tsx"],
    reach: "/issues/<ISSUE>/conversation, a reply turn's body",
    record: true,
    rendered: true,
    surface: "conversation-reply",
  },
  {
    files: ["packages/dispatch/web/src/features/conversation/ConversationTab.tsx"],
    reach: "/issues/<ISSUE>/conversation, a message turn's body",
    record: true,
    rendered: true,
    surface: "conversation-message",
  },
  {
    files: ["packages/dispatch/web/src/features/conversation/ConversationTab.tsx"],
    reach: "/issues/<ISSUE>/conversation, a targeted message turn's body",
    record: true,
    rendered: true,
    surface: "conversation-targeted-message",
  },
  // A handoff's card, whose body is the JSON its phase wrote: the words a session wrote in it,
  // and the data beside them.
  {
    cannotCarry: NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/conversation/ConversationTab.tsx"],
    reach: "/issues/<ISSUE>/conversation, a handoff card opened on its body",
    reason:
      "A handoff is no record of the reference graph (`textSourceKind`): the renderer draws the words its session wrote - each string field, each proof entry, a body that is no JSON object - from the reading the server sends for each (`field_trees`), and is given no `source`, so a `#id` in them names nothing.",
    record: false,
    rendered: true,
    surface: "conversation-handoff-body",
  },
  {
    files: ["packages/dispatch/web/src/features/conversation/ConversationTab.tsx"],
    reach: "/issues/<ISSUE>/conversation, a handoff card's field of JSON",
    reason:
      "A handoff field that holds an array, an object, a number, a boolean or null is the data its session wrote, not words: it is shown as the JSON it is.",
    record: false,
    rendered: false,
    surface: "conversation-handoff-data",
  },
  // The line a reply quotes of what it answers, wherever a reply is drawn.
  {
    files: ["packages/dispatch/web/src/features/conversation/ReplyQuote.tsx"],
    reach: "/issues/<ISSUE>/conversation, the quoted line above a reply",
    record: true,
    rendered: true,
    surface: "reply-quote",
  },
  // The new issue page's outline: the words a node's row draws - its own text, and the answer an
  // ask carries under its question - and a child issue's row, which is its title. The Decisions
  // view's rows draw two more texts by the same reader: the body of the Change a person asked of
  // a decision, and the note a person wrote as they declined an ask, which the walk does not
  // reach, so the tests that drive them are named.
  {
    cannotCarry: {
      forms: ["question-id"],
      why: "A row reads no question map: a `#id` is read against its document's questions, one read for each document, which a row of the outline does not make (`row-references.ts`), so it is drawn as written.",
    },
    files: [
      "packages/dispatch/web/src/features/issue-page/RowText.tsx",
      "packages/dispatch/web/src/features/issue-page/decisions-filter/DecisionsRow.tsx",
    ],
    proof: [
      {
        file: "packages/dispatch/web/src/features/issue-page/decisions-filter/DecisionsFilterBody.drawn.test.tsx",
        test: "a Change's body is drawn through the renderer, its marks gone",
      },
      {
        file: "packages/dispatch/web/src/features/issue-page/decisions-filter/DecisionsFilterBody.drawn.test.tsx",
        test: "a declined ask's note is drawn through the renderer, a reference in it read as its target",
      },
      {
        file: "packages/dispatch/web/src/features/issue-page/decisions-filter/DecisionsFilterBody.drawn.test.tsx",
        test: "a declined ask's note is drawn from the reading the asks read sends",
      },
    ],
    reach: "/issues/<ISSUE> with the new issue page on, a comment's row in the outline",
    record: true,
    rendered: true,
    surface: "node-row-words",
  },
  {
    files: ["packages/dispatch/web/src/features/issue-page/NodeRow.tsx"],
    reach: "/issues/<ISSUE> with the new issue page on, a child issue's row",
    reason: `${NO_MARKUP_REASON} A child issue's row is its title, which is that one line, as the title bar's is (\`issue-page-title\`).`,
    record: false,
    rendered: false,
    surface: "node-row-child-title",
  },
  // The new issue page's one line of a node, drawn as plain words: a folded branch's line, the
  // line a filter's row names a node's parent by, a crumb above a zoom, a pin in Details and a
  // task's line in the Plan view, each projected from the node's own words and cut by R1's
  // first-line rule; and a chip, which names a tie's target - in a row of the outline, in the
  // Plan chart's needs and serves, and on a check's "Also under" line in the Verification view.
  {
    cannotCarry: NODE_LINE_NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/issue-page/node-line.tsx"],
    proof: {
      file: "packages/dispatch/web/src/features/issue-page/NodeRow.drawn.test.tsx",
      test: "a folded branch's line reads a reference as its target's name, and as its author's words until it resolves",
    },
    reach: "/issues/<ISSUE> with the new issue page on, a folded branch's one line",
    reason: `A folded branch is one line, its node's first line, cut by R1's first-line rule with its ellipsis and drawn beside the count of what it holds. ${PLAIN_WORDS_REASON}`,
    record: true,
    rendered: false,
    surface: "node-row-first-line",
  },
  {
    cannotCarry: NODE_LINE_NO_QUESTION_ID,
    files: [
      "packages/dispatch/web/src/features/issue-page/node-line.tsx",
      "packages/dispatch/web/src/features/issue-page/decisions-filter/DecisionsRow.tsx",
    ],
    proof: [
      {
        file: "packages/dispatch/web/src/features/issue-page/NodeRow.drawn.test.tsx",
        test: "a parent line is its parent's projected first line, a reference in it read by its target's name",
      },
      {
        file: "packages/dispatch/web/src/features/issue-page/decisions-filter/DecisionsFilterBody.drawn.test.tsx",
        test: "an entry's parent line reads a reference in its parent's words by its target's name",
      },
    ],
    reach:
      "/issues/<ISSUE>/plan with the new issue page on, a plan task's line naming the statement it serves",
    reason: `A filter's row names a parent it draws no row for on one line, the parent's first line cut by R1's first-line rule, in the words of a link to it. ${PLAIN_WORDS_REASON}`,
    record: true,
    rendered: false,
    surface: "node-row-parent-line",
  },
  {
    files: [
      "packages/dispatch/web/src/features/issue-page/NodeRow.tsx",
      "packages/dispatch/web/src/features/issue-page/decisions-filter/DecisionsFilterBody.tsx",
      "packages/dispatch/web/src/features/issue-page/plan-filter/PlanFilterBody.tsx",
      "packages/dispatch/web/src/features/issue-page/verification-filter/VerificationFilterBody.tsx",
    ],
    proof: [
      {
        file: "packages/dispatch/web/src/features/issue-page/NodeRow.drawn.test.tsx",
        test: "a numbered step keeps its number in a chip and in a folded line",
      },
      {
        file: "packages/dispatch/web/src/features/issue-page/decisions-filter/DecisionsFilterBody.drawn.test.tsx",
        test: "each statement on the informs line is named within 32 characters",
      },
      {
        file: "packages/dispatch/web/src/features/issue-page/plan-filter/PlanFilterBody.test.tsx",
        test: "a step's needs and serves chips name each block by its first worded block, cut to a chip's room",
      },
      {
        file: "packages/dispatch/web/src/features/issue-page/verification-filter/VerificationFilterBody.drawn.test.tsx",
        test: "a check under two statements names the other by its words with no mark of markdown",
      },
    ],
    reach:
      "/issues/<ISSUE> with the new issue page on, a plan task's needs chip; /issues/<ISSUE>/plan with it on, a step's needs and serves chips in the chart; /issues/<ISSUE>/verification with it on, a check's \"Also under\" line; /issues/<ISSUE>/decisions with it on, a decision's informs line",
    reason: `A chip is a link to a tie's target, worded with the target's name cut to a chip's room: the chip's address as the projection reads it standing alone, which names its target by the name the page published from its batched read (\`refs/reference-names.tsx\`), itself projected from the target's own words as a title over one record, and a block by the line the store projects beside that name, the first block of its words (\`KnownReferences.lines\`). The Plan chart's chips - the tasks a step needs and the statements it serves - and the Verification view's "Also under" line, which names the other statements a check is drawn under, name each block as a tie's chip does, by that line cut to a chip's room (\`chipName\`), read off the store with no projection of their own: a serves chip as a link to its statement, and a need and an "Also under" name as words alone. The address is no record a \`#id\` could name a question of. ${PLAIN_WORDS_REASON}`,
    record: false,
    rendered: false,
    surface: "node-row-chip",
  },
  {
    cannotCarry: NODE_LINE_NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/issue-page/IssueTree.tsx"],
    proof: {
      file: "packages/dispatch/web/src/features/issue-page/IssueTree.test.tsx",
      test: "a crumb reads a reference in its node's line as its target's name, and as its author's words until it resolves",
    },
    reach:
      "/issues/<ISSUE> with the new issue page on, below 1024 px, the crumb line above a node zoomed into",
    reason: `A crumb above a zoom names a node on one line, as a button: its first line cut by R1's first-line rule with its ellipsis, and its kind's word where its words hold none. ${PLAIN_WORDS_REASON}`,
    record: true,
    rendered: false,
    surface: "outline-crumb",
  },
  {
    cannotCarry: NODE_LINE_NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/issue-page/IssuePins.tsx"],
    proof: {
      file: "packages/dispatch/web/src/features/issue-page/IssueMargin.test.tsx",
      test: "a pin's line reads a reference as its target's name",
    },
    reach: "/issues/<ISSUE> with the new issue page on, Details' Pinned section",
    reason: `A pin names its record on one line, its first line cut by R1's rule. ${PLAIN_WORDS_REASON}`,
    record: true,
    rendered: false,
    surface: "issue-pin",
  },
  {
    cannotCarry: NODE_LINE_NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/issue-page/IssueMargin.tsx"],
    proof: {
      file: "packages/dispatch/web/src/features/issue-page/IssueMargin.test.tsx",
      test: "Filed from's line reads a reference as its target's name",
    },
    reach: "/issues/<ISSUE> with the new issue page on, Details' Filed from row",
    reason: `Details names the item an issue was filed out of on one line, as a link to it: its first line cut by R1's first-line rule, and its kind's word where its words hold none. ${PLAIN_WORDS_REASON}`,
    record: true,
    rendered: false,
    surface: "issue-filed-from",
  },
  {
    files: ["packages/dispatch/web/src/features/issue-page/history-entries.tsx"],
    reach: "/issues/<ISSUE>/activity with the new issue page on, a turn in Details' History",
    record: true,
    rendered: true,
    surface: "issue-page-history-turn",
  },
  {
    cannotCarry: NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/issue-page/HistoryHandoff.tsx"],
    reach:
      "/issues/<ISSUE>/activity with the new issue page on, a handoff's opened fields in Details' History",
    reason:
      "A handoff is no record of the reference graph (`textSourceKind`): the renderer draws the words its session wrote - each string field, each proof entry, a body that is no JSON object - from the reading the server sends for each (`field_trees`), and is given no `source`, so a `#id` in them names nothing.",
    record: false,
    rendered: true,
    surface: "issue-page-history-handoff-body",
  },
  {
    files: ["packages/dispatch/web/src/features/issue-page/HistoryHandoff.tsx"],
    reach:
      "/issues/<ISSUE>/activity with the new issue page on, a handoff's field of JSON in Details' History",
    reason:
      "A handoff field that holds an array, an object, a number, a boolean or null is the data its session wrote, not words: it is shown as the JSON it is.",
    record: false,
    rendered: false,
    surface: "issue-page-history-handoff-data",
  },
  {
    cannotCarry: NODE_LINE_NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/issue-page/history-entries.tsx"],
    proof: {
      file: "packages/dispatch/web/src/features/issue-page/IssueHistory.test.tsx",
      test: "a block's line in the history reads a reference as its target's name",
    },
    reach:
      "/issues/<ISSUE>/activity with the new issue page on, the line of a repaired or refused block in Details' History",
    reason: `A line of the history names a block by the one line of the node that carries it, its first line cut by the page's first-line rule. ${PLAIN_WORDS_REASON}`,
    record: true,
    rendered: false,
    surface: "issue-page-history-block",
  },
  {
    cannotCarry: NODE_LINE_NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/issue-page/plan-filter/PlanFilterBody.tsx"],
    proof: {
      file: "packages/dispatch/web/src/features/issue-page/plan-filter/PlanFilterBody.test.tsx",
      test: "a step's line reads a reference by its target's name, as the task's folded line does",
    },
    reach: "/issues/<ISSUE>/plan with the new issue page on, a step's line in the chart",
    reason: `The Plan view names a task on one line - the line a step's row of the chart labels its bar with, and a task's link in the line of tasks that need each other - its first line cut by R1's first-line rule with its ellipsis, and its kind's word where its words hold none, read as the task's folded line in the outline is (\`node-row-first-line\`). ${PLAIN_WORDS_REASON}`,
    record: true,
    rendered: false,
    surface: "plan-task-line",
  },
  // The Spec view's decision cell and its because line: the quote of a decision and the question an
  // answer answers, each one line of words `specDecidedBy` has named before the view draws it.
  {
    files: ["packages/dispatch/web/src/features/issue-page/spec-filter/SpecFilterBody.tsx"],
    reach: "/issues/<ISSUE>/spec with the new issue page on, a decision cell or because line",
    reason:
      "The Spec view draws what decided a statement as one line of plain text with no markup: a decision's quote, the first sentence of the answer's or the decision's words, and the question an answer answers, each cut to one line by `specDecidedBy` (`spec-filter/spec-decided-by.ts`), which names each reference from the page's published names via `one-line.ts`'s `referenceStandIns` before the view draws the string, rather than mounting `PlainText` or `usePlainHeadline`. It passes no source record: the plain string reads no per-record question map for `#id` references and makes no per-record reads, drawing only from the page's published names.",
    record: false,
    rendered: false,
    surface: "spec-decision-line",
  },
  // The reason a reviewer asked for changes: in a document's Reviews dialog, and on the line
  // under the design gate's steps in a root issue's header.
  {
    files: [
      "packages/dispatch/web/src/features/doc/ApprovalChip.tsx",
      "packages/dispatch/web/src/features/issue/ApprovalSteps.tsx",
    ],
    reach: "/projects/<PROJECT>/documents/<SLUG>, the Reviews dialog's reason",
    record: true,
    rendered: true,
    surface: "approval-review-reason",
  },
  // The ask card, which the Inbox and a document's decision block both draw.
  {
    files: ["packages/dispatch/web/src/features/inbox/AskCard.tsx"],
    reach: "/, an ask card's question",
    record: true,
    rendered: true,
    surface: "ask-card-question",
  },
  {
    files: ["packages/dispatch/web/src/features/inbox/AskCompletionCard.tsx"],
    reach: "/, an ask card's edit history",
    record: true,
    rendered: true,
    surface: "ask-edit-history",
  },
  {
    files: ["packages/dispatch/web/src/features/inbox/AskCompletionCard.tsx"],
    reach: "/, an ask card's earlier answers",
    record: true,
    rendered: true,
    surface: "ask-answer-history",
  },
  // An option an earlier answer chose, drawn from the tree of its own label, which the answer
  // carries: it may name an option the ask has since renamed or removed.
  {
    files: [
      "packages/dispatch/web/src/features/inbox/AskCompletionCard.tsx",
      "packages/dispatch/web/src/features/activity/EventBody.tsx",
    ],
    reach:
      "/, an ask card's earlier answers, an option one chose; /issues/<ISSUE>/activity, a pinned changed answer's `was:` line",
    record: true,
    rendered: true,
    surface: "earlier-chosen-option",
  },
  {
    files: ["packages/dispatch/web/src/features/inbox/AskCompletionCard.tsx"],
    reach: "/, an answered ask's question and answer",
    record: true,
    rendered: true,
    surface: "answered-ask",
  },
  {
    files: ["packages/dispatch/web/src/features/inbox/AskCompletionCard.tsx"],
    reach: "/, a resolved ask's question and resolution reason",
    record: true,
    rendered: true,
    surface: "resolved-ask",
  },
  // An ask's options, on its card and under its question on the new issue page's row.
  {
    files: [
      "packages/dispatch/web/src/features/inbox/AskOptionRow.tsx",
      "packages/dispatch/web/src/features/issue-page/NodeRow.tsx",
    ],
    reach: "/, an ask option's label and description",
    record: true,
    rendered: true,
    surface: "ask-option-text",
  },
  {
    files: ["packages/dispatch/web/src/features/inbox/AskThread.tsx"],
    reach: "/, a reply under an ask",
    record: true,
    rendered: true,
    surface: "ask-thread-reply",
  },
  // The Inbox's list and its reading column.
  {
    files: ["packages/dispatch/web/src/features/inbox/Inbox.tsx"],
    reach: "/, an ask row's one-line question",
    record: true,
    rendered: true,
    surface: "inbox-ask-row",
  },
  {
    files: [
      "packages/dispatch/web/src/features/inbox/useControlName.ts",
      "packages/dispatch/web/src/features/inbox/ask-name.ts",
    ],
    proof: {
      file: "packages/dispatch/web/src/features/inbox/useControlName.test.tsx",
      test: "a #id in an Inbox row's question names the question of its ask in the controls' name",
    },
    reach: "/, the name of an ask row's checkbox, of its Snooze and of the reading column's head",
    reason: `An Inbox row's controls are named in an \`aria-label\`, after the row's owner and the start of its question. ${PLAIN_WORDS_REASON} ${LOAD_STATE_REASON}`,
    record: true,
    rendered: false,
    surface: "inbox-control-name",
  },
  {
    files: ["packages/dispatch/web/src/features/inbox/ReadingPane.tsx"],
    reach: "/, an approval's question in the reading column",
    record: true,
    rendered: true,
    surface: "approval-ask-question",
  },
  {
    files: ["packages/dispatch/web/src/features/inbox/ThreadBody.tsx"],
    reach: "/, a thread turn's body in the reading column",
    record: true,
    rendered: true,
    surface: "inbox-thread-turn",
  },
  {
    files: ["packages/dispatch/web/src/features/inbox/ThreadPane.tsx"],
    reach: "/replies, a thread's subject line",
    record: true,
    rendered: true,
    surface: "my-reply-subject",
  },
  // A thread row's title. The row's checkbox and its Dismiss name the row by the same subject,
  // in an `aria-label` that holds no markup, so they draw its plain-text projection.
  {
    files: [
      "packages/dispatch/web/src/features/inbox/ThreadTitle.tsx",
      "packages/dispatch/web/src/features/inbox/Inbox.tsx",
      "packages/dispatch/web/src/features/inbox/DismissControl.tsx",
    ],
    proof: {
      file: "packages/dispatch/web/src/features/refs/reference-naming.test.tsx",
      test: "a #id in an Inbox thread subject projects as its question where the row names its record",
    },
    reach: "/, a thread row's title",
    record: true,
    rendered: true,
    surface: "inbox-thread-title",
  },
  // The issue's Decisions panel reads each decision's heading and paragraphs as the record they
  // are: an agent's decision a block of the document it is written in, a person's - the answered
  // ask - the ask's own text, whose `#id`s the server reads against the document the ask's block
  // is on. A shaped link names the target block by its id, in the panel's own words: the
  // decisions read sends none of the block's text (`refs.BlockLinks`).
  {
    files: ["packages/dispatch/web/src/features/issue/DecisionsPanel.tsx"],
    reach: "/issues/<ISSUE>/decisions, a decision's heading and paragraphs",
    record: true,
    rendered: true,
    surface: "issue-decision",
  },
  {
    files: ["packages/dispatch/web/src/features/issue/DecisionsPanel.tsx"],
    proof: DECISIONS_PANEL_PROOF,
    reach: "/issues/<ISSUE>/decisions, a link under Open questions",
    reason: `An open question is a link to its ask, worded with the question. ${PLAIN_WORDS_REASON} ${LOAD_STATE_REASON}`,
    record: true,
    rendered: false,
    surface: "issue-open-question",
  },
  // The list an address shows when its short id names more than one item of the issue.
  {
    files: ["packages/dispatch/web/src/features/issue/ItemMatchList.tsx"],
    proof: {
      file: "packages/dispatch/web/src/features/issue/ItemMatchList.test.tsx",
      test: "a match's first line reads its plain-text projection",
    },
    reach: "An issue address whose short id names several items, a match's link",
    reason: `A match is a link to its item, worded with the first line of the item's text, which the server cuts and reads into a tree. ${PLAIN_WORDS_REASON} ${LOAD_STATE_REASON} A document's first line is its file's name, which has no tree: a name, drawn as the characters it is.`,
    record: true,
    rendered: false,
    surface: "item-match-first-line",
  },
  // A decision's heading, where it names a write, a form or a card, or words a link: on the
  // Decisions panel, on a margin card, and in the Keep and the Change of a decision's row on the
  // new issue page. All three read it through one hook (`useDecisionHeadingWords`), and a test
  // drives each of them.
  {
    files: [
      "packages/dispatch/web/src/features/issue/decision-heading.ts",
      "packages/dispatch/web/src/features/issue/DecisionsPanel.tsx",
      "packages/dispatch/web/src/features/margin/DecisionCard.tsx",
      "packages/dispatch/web/src/features/issue-page/DecisionActions.tsx",
    ],
    proof: [
      {
        file: "packages/dispatch/web/src/features/margin/DecisionCard.test.tsx",
        test: "a heading that holds markdown names the card and its link in plain words",
      },
      DECISIONS_PANEL_PROOF,
      {
        file: "packages/dispatch/web/src/features/issue-page/DecisionActions.test.tsx",
        test: "the Change form is named after the heading's plain words",
      },
    ],
    reach:
      "/issues/<ISSUE>/decisions, a waiting decision's Keep and Change; /issues/<ISSUE>/spec, a margin decision card's name and its heading's link",
    reason: `A decision's heading names a write, a form and a card, and words a link to the decision. ${PLAIN_WORDS_REASON} ${LOAD_STATE_REASON}`,
    record: true,
    rendered: false,
    surface: "decision-heading-words",
  },
  // The words a Change of a decision is anchored to, quoted over the box on the new issue page.
  {
    files: ["packages/dispatch/web/src/features/issue-page/DecisionActions.tsx"],
    proof: {
      file: "packages/dispatch/web/src/features/issue-page/DecisionActions.test.tsx",
      test: "the Change quotes its block's plain-text projection and anchors to the document's own words",
    },
    reach:
      "/issues/<ISSUE>/decisions with the new issue page on, a waiting decision's Change, the quote over its box",
    reason: `The Change quotes the decision's first block below its heading, the words the change is anchored to, on one line over the box. ${PLAIN_WORDS_REASON} The write still anchors to the decisions document's own words, which the server matches (\`anchor-quote\`). ${LOAD_STATE_REASON}`,
    record: true,
    rendered: false,
    surface: "decision-change-quote",
  },
  // A requirement's line in the spec's Requirements list.
  {
    files: ["packages/dispatch/web/src/features/issue/SpecRequirements.tsx"],
    reach: "/issues/<ISSUE>/spec, a requirement's line under Requirements",
    reason:
      "A requirement's line is the server's flattened words of a block of the stored spec (`blockWords`, `api/results.go`): its links' addresses and its mentions are dropped and a bare key is left as the characters it is, so the line holds no markdown to render and no reference to read. The spec block itself is drawn by the document's editor (`document-body`).",
    record: false,
    rendered: false,
    surface: "spec-requirement-words",
  },
  // A document version's summary, in the spec's version picker and on an artifact's page.
  {
    files: ["packages/dispatch/web/src/features/issue/SpecToolbar.tsx"],
    reach: "/issues/<ISSUE>/spec, the version picker's options",
    reason: `A version's summary names the version it was given to. ${ONE_LINE_FIELD_REASON}`,
    record: false,
    rendered: false,
    surface: "spec-version-summary",
  },
  {
    files: ["packages/dispatch/web/src/features/artifacts/ArtifactHeader.tsx"],
    reach: "/issues/<ISSUE>/artifacts/<SLUG>, a version's label",
    reason: `A version's summary names the version it was given to. ${ONE_LINE_FIELD_REASON}`,
    record: false,
    rendered: false,
    surface: "artifact-version-summary",
  },
  // The diff between two versions of a document, at the three places that compare them.
  {
    files: [
      "packages/dispatch/web/src/features/doc/ChangeView.tsx",
      "packages/dispatch/web/src/features/artifacts/ArtifactDetails.tsx",
      "packages/dispatch/web/src/features/doc/ProofDocument.tsx",
      "packages/dispatch/web/src/features/inbox/EarlierApproval.tsx",
    ],
    reach: "/projects/<PROJECT>/documents/<SLUG>, Compare between two versions",
    reason:
      "A diff shows the exact characters a change swaps, the removed beside the added, so it is drawn as the characters each version holds rather than rendered (R6): a rendered `**` or `[` would hide the change it is. Each version itself is drawn by the document's editor (`document-body`, `document-version-body`).",
    record: false,
    rendered: false,
    surface: "change-view-diff",
  },
  // The reason an agent gave for a credential it asks for.
  {
    files: ["packages/dispatch/web/src/features/credentials/CredentialRecordFacts.tsx"],
    reach: "/credentials/machine, a machine login request's reason",
    reason: `A credential request's reason is what the asking agent typed for it. ${ONE_LINE_FIELD_REASON}`,
    record: false,
    rendered: false,
    surface: "credential-request-reason",
  },
  // The issue header's Components line, which shows the reason an issue touches no component.
  {
    cannotCarry: NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/issue/IssueComponentsLine.tsx"],
    reach: "/issues/<ISSUE>, the header's Components line",
    reason: NOT_ARCHITECTURAL_REASON,
    record: false,
    rendered: true,
    surface: "issue-components-reason",
  },
  // A document's margin: the decision card and the pinned conversations, which an issue's page
  // shows beside its spec, a suggestion's two sides and a thread's comments. The decision card's
  // text is a passage of its record, as the Decisions panel reads it: an agent's decision's
  // document, which the Decisions read names by artifact id or the decision's own address names,
  // or a person's ask.
  {
    files: ["packages/dispatch/web/src/features/margin/DecisionCard.tsx"],
    reach: "/issues/<ISSUE>/spec, a margin decision card's heading and passage",
    record: true,
    rendered: true,
    surface: "margin-decision-passage",
  },
  {
    files: ["packages/dispatch/web/src/features/margin/PinnedTab.tsx"],
    reach: "/issues/<ISSUE>/conversation, pin a turn, then the margin's Pinned tab",
    record: true,
    rendered: true,
    surface: "margin-pinned-conversation",
  },
  {
    cannotCarry: NO_QUESTION_ID,
    files: ["packages/dispatch/web/src/features/margin/ThreadCard.tsx"],
    reach: "/projects/<PROJECT>/documents/<SLUG>, a suggestion card's `<del>` and `<ins>`",
    reason:
      "A suggestion's two sides are a fragment of the comment rather than the comment: the quote is the document's own characters and the replacement is what accepting would write. A diff shows the exact characters it swaps (`ReferenceHostIs host=\"diff\"`), so neither side is a record a `#id` could declare on.",
    record: false,
    rendered: true,
    surface: "margin-suggestion-diff",
  },
  {
    files: ["packages/dispatch/web/src/features/margin/ThreadCard.tsx"],
    reach: "/projects/<PROJECT>/documents/<SLUG>, a thread card's comment body",
    record: true,
    rendered: true,
    surface: "margin-comment-body",
  },
  {
    files: ["packages/dispatch/web/src/features/margin/ThreadCard.tsx"],
    reach: "/projects/<PROJECT>/documents/<SLUG>, a collapsed thread card's preview",
    record: true,
    rendered: true,
    surface: "margin-thread-preview",
  },
  // The quote a comment or an ask was left on, wherever one is drawn: over a thread card and
  // its preview, over an ask's card, over a thread in the Inbox's reading column, in the
  // composer that is writing on it, in the Decisions panel's Change form, in the Activity tab's
  // line for the comment, and in the words that name it beside a draft a margin compose kept
  // for it (the composer's offer, the margin's list of what its compose kept).
  {
    files: [
      "packages/dispatch/web/src/features/margin/ThreadCard.tsx",
      "packages/dispatch/web/src/features/inbox/AskAnchorLink.tsx",
      "packages/dispatch/web/src/features/inbox/ThreadBody.tsx",
      "packages/dispatch/web/src/features/conversation/MentionComposer.tsx",
      "packages/dispatch/web/src/features/conversation/KeptDraftOffers.tsx",
      "packages/dispatch/web/src/features/margin/CommentsTab.tsx",
      "packages/dispatch/web/src/features/issue/DecisionsPanel.tsx",
      "packages/dispatch/web/src/features/activity/activity-model.ts",
    ],
    reach: "/projects/<PROJECT>/documents/<SLUG>, the quote over a thread card's comment",
    reason:
      "An anchor's quote is the document's own words at the place a comment or an ask was left, not a text anyone wrote for this surface: the server matches and stores it as the document's flattened text, never its markdown (`pmdoc.FindQuote`), so a mention in the document is already the words it reads as there, and nothing in the quote is markdown to render. It is shown as the characters it is. One quote is not those words: a decision the Decisions panel draws from an answer that carried no reading of its text, during a rolling deploy, quotes its first block's markdown, which the server finds only where that block holds no markup, and a Change sent with a quote it cannot find is refused and stores nothing.",
    record: false,
    rendered: false,
    surface: "anchor-quote",
  },
  // A draft the reader wrote and did not send, waiting outside a composer's field: an edit
  // written over a body the comment no longer holds, a draft a margin compose kept for other
  // words, a reply kept for a comment or message no longer listed, and what the margin lists of
  // its compose's kept drafts, each drawn by one offer (`DraftOffer`).
  {
    files: ["packages/dispatch/web/src/features/conversation/KeptDraftOffers.tsx"],
    reach: "/issues/<ISSUE>/conversation, a kept draft beside the docked composer's field",
    reason:
      "A kept draft is the reader's own text as the field holds it, not a record and not yet a message: Dispatch has stored nothing of it, and Use it here puts exactly these characters back in the field, which shows them as the characters they are. It is drawn the same way beside the field, so the reader sees what they will take back - a mention as the link they picked, `[@Planner](dispatch://agent/...)` - and reads the message it becomes only once it is sent.",
    record: false,
    rendered: false,
    surface: "kept-draft",
  },
  // A document's own body, live and at a past version, which the Proof editor draws.
  {
    files: [
      "packages/dispatch/web/src/features/doc/ProofDocument.tsx",
      "packages/dispatch/web/src/features/doc/document-references.tsx",
    ],
    reach: "/projects/<PROJECT>/documents/<SLUG>, the document's body",
    reason: EDITOR_REASON,
    record: true,
    rendered: false,
    surface: "document-body",
  },
  {
    files: ["packages/dispatch/web/src/features/doc/VersionView.tsx"],
    reach: "/projects/<PROJECT>/documents/<SLUG>?v=<N>, a past version's body",
    reason: `${EDITOR_REASON} A past version is drawn by the same editor, read-only.`,
    record: true,
    rendered: false,
    surface: "document-version-body",
  },
  {
    files: ["packages/dispatch/web/src/features/doc/ProofDocument.tsx"],
    proof: {
      file: "packages/dispatch/web/src/features/doc/ProofDocument.decisions.test.tsx",
      test: "the open-decision link reads its question's plain-text projection",
    },
    reach: "/projects/<PROJECT>/documents/<SLUG>, the open decision named above the document",
    reason: `The bar above a document names its open decision in a link and in that link's \`title\`. ${PLAIN_WORDS_REASON}`,
    record: true,
    rendered: false,
    surface: "document-open-decision",
  },
  // The Verification tab: what a check says, and what a result of it reports.
  {
    files: ["packages/dispatch/web/src/features/verification/VerificationTab.tsx"],
    reach: "/issues/<ISSUE>/verification, a check's text",
    reason: COMMAND_OUTPUT_REASON,
    record: true,
    rendered: false,
    surface: "check-text",
  },
  {
    files: ["packages/dispatch/web/src/features/verification/VerificationTab.tsx"],
    reach: "/issues/<ISSUE>/verification, a check's negative control",
    reason: COMMAND_OUTPUT_REASON,
    record: true,
    rendered: false,
    surface: "check-negative-control",
  },
  {
    files: ["packages/dispatch/web/src/features/verification/VerificationTab.tsx"],
    reach: "/issues/<ISSUE>/verification, a result's Observed",
    reason: COMMAND_OUTPUT_REASON,
    record: true,
    rendered: false,
    surface: "check-result-observed",
  },
  {
    files: ["packages/dispatch/web/src/features/verification/VerificationTab.tsx"],
    reach: "/issues/<ISSUE>/verification, a result's Negative control",
    reason: COMMAND_OUTPUT_REASON,
    record: true,
    rendered: false,
    surface: "check-result-negative-control",
  },
  {
    files: [
      "packages/dispatch/web/src/features/verification/VerificationTab.tsx",
      "packages/dispatch/web/src/features/activity/activity-model.ts",
      "packages/dispatch/web/src/features/issue/event-description.ts",
    ],
    reach: "/issues/<ISSUE>/verification, a retracted result's reason; the retraction's event line",
    reason: `${COMMAND_OUTPUT_REASON} A retraction's reason is written beside the result it withdraws, in the same hand, and is shown the same way.`,
    record: true,
    rendered: false,
    surface: "check-retraction-reason",
  },
  // The Referenced by panel: what cites a node, each with the head of its text the graph holds.
  {
    files: ["packages/dispatch/web/src/features/refs/ReferencedBy.tsx"],
    reach: "/issues/<ISSUE>, the Referenced by panel, a citing comment's excerpt",
    record: true,
    rendered: true,
    surface: "referenced-by-excerpt",
  },
  {
    files: ["packages/dispatch/web/src/features/refs/ReferencedBy.tsx"],
    reach: "/issues/<ISSUE>, the Referenced by panel, a citing issue's title",
    reason: `${NO_MARKUP_REASON} A citing issue, document, component or session is listed by its title or name, which is that one line; a citing block, ask, comment or message is listed by the head of its body, which the renderer draws (\`referenced-by-excerpt\`).`,
    record: false,
    rendered: false,
    surface: "referenced-by-title",
  },
  // The Artifacts tab's References and Referenced by rows: each a link to the node at the far end
  // of a link of the issue's references read, worded with the name the read gives that node.
  {
    files: ["packages/dispatch/web/src/features/artifacts/ArtifactsTab.tsx"],
    proof: {
      file: "packages/dispatch/web/src/features/artifacts/ArtifactsTab.test.tsx",
      test: "a references row reads a reference in its name as its target's name",
    },
    reach: "/issues/<ISSUE>/artifacts, a References or Referenced by row",
    reason: `A row of the issue's references read is a link to the node at its far end, worded with the name the read gives that node, as the server cut it. Where that name is the head of a comment's, an ask's or a message's text, whose \`#id\` is read against that record, it is drawn through this projection; a document's name, an issue's title and a component's are drawn as the characters they are. ${PLAIN_WORDS_REASON}`,
    record: true,
    rendered: false,
    surface: "artifacts-reference-name",
  },
  // A person's page: each node they wrote or that reached them, by the first line of its text,
  // which the person nodes read cuts and sends with no tree of it.
  {
    files: ["packages/dispatch/web/src/features/people/PersonPage.tsx"],
    proof: {
      file: "packages/dispatch/web/src/features/people/PersonPage.test.tsx",
      test: "a node's line is drawn through the renderer, its marks drawn and a reference read as its target",
    },
    reach: "/people/<LOGIN>, a node's line",
    record: true,
    rendered: true,
    surface: "person-node-title",
  },
  // A priority's description: drawn whole, on its own page and on the open row of the priorities
  // table, in the documents' editor (`PriorityDescription`), which paints it and draws its
  // references as a document's body is drawn, and its start, in plain words, on a closed row of
  // that table (`priorities-columns.tsx`), through the projection.
  {
    cannotCarry: NO_QUESTION_ID,
    files: [
      "packages/dispatch/web/src/features/priorities/PriorityDescription.tsx",
      "packages/dispatch/web/src/features/priorities/priorities-columns.tsx",
    ],
    proof: [
      {
        file: "packages/dispatch/web/src/features/priorities/PrioritiesPage.test.tsx",
        test: "a closed row runs a description's paragraphs together, a reference read as its target's name",
      },
      {
        file: "packages/dispatch/web/src/features/priorities/PrioritiesPage.test.tsx",
        test: "a closed row draws its description's excerpt from the tree the priorities read sends",
      },
    ],
    reach: "/priorities/<ID>, the Description editor, and /priorities, a closed row's excerpt",
    reason: `A priority's description is no record of the reference graph: a priority is none of the four kinds a \`TextSource\` names (an artifact, an ask, a comment, a message), and the priority write stores the description as an attribute of the priority and indexes nothing from it - so a \`#id\` in it declares nothing and names nothing. Whole, it is drawn in the documents' editor, as a document's body is: ${EDITOR_REASON} A closed row of the priorities table shows its start: ${PLAIN_WORDS_REASON}`,
    record: false,
    rendered: false,
    surface: "priority-description",
  },
  // The cards under a body: the renderer draws them itself, so no surface pairs a card with a
  // body by hand.
  {
    cannotCarry: {
      forms: ["worded-link"],
      why: "A body draws cards only when it is nothing but references (`Unfurl.tsx`'s `referencesOnly`), and a worded link's words are words of the body's own, so a body that holds one draws no card. The worded link is held on every other rendered surface.",
    },
    files: ["packages/dispatch/web/src/features/refs/MarkdownBody.tsx"],
    reach:
      "Any rendered body whose text is nothing but references, `.dispatch-markdown` + the cards after it",
    record: true,
    rendered: true,
    surface: "body-reference-cards",
  },
  // A card's title, and a hover card's below, is cut from its target's own text and is that
  // target's name. R8 is the rule for a stored record drawn on a surface; inside a title an
  // address reads as its short form, never as another node's name (`TITLE_NAMING`,
  // `refs/reference-target.ts`).
  {
    files: ["packages/dispatch/web/src/features/refs/Unfurl.tsx"],
    reach: "A Dispatch reference card under a body, its two rendered lines",
    record: true,
    rendered: true,
    surface: "dispatch-reference-card",
  },
  {
    files: ["packages/dispatch/web/src/features/refs/Unfurl.tsx"],
    reach:
      "A Dispatch reference card under a body, its first line while the target's title loads and for a target that has none",
    reason:
      "A card names its target by its title, and by the reference as the body wrote it while the title loads and where the target has none (a session Dispatch knows with no title of its own). A card is drawn only under a body that is nothing but references (`referenceCards`), so what the body wrote is one address, issue key or `#id`, never a link's words: a name, which holds no markdown, so it is shown as the characters it is - the words written, as R8 reads an unresolved reference.",
    record: true,
    rendered: false,
    surface: "reference-card-words",
  },
  // The one reader this change does not fold: a GitHub issue or pull request, matched and drawn
  // by the card itself. The walk drives it not, so its proofs are the card's own rows: an
  // issue's card draws its title and its body formatted, and a pull request's card matches the
  // URL the body wrote, reads it from GitHub and links to it.
  {
    files: ["packages/dispatch/web/src/features/refs/Unfurl.tsx"],
    proof: [
      {
        file: "packages/dispatch/web/src/features/refs/Unfurl.test.tsx",
        test: "Unfurl renders a GitHub issue's body formatted, on one run of text, with no syntax",
      },
      {
        file: "packages/dispatch/web/src/features/refs/Unfurl.test.tsx",
        test: "Unfurl links a GitHub pull request's card to the URL the body wrote",
      },
    ],
    reach: "A GitHub issue or pull-request card under a body",
    reason:
      "A GitHub issue or pull request is no record of this Dispatch and no `dispatch://` address names one: the card matches the GitHub URL itself and reads the title and body from GitHub's API, which is the one reader this change does not fold into the shared one.",
    record: false,
    rendered: false,
    surface: "github-reference-card",
  },
  // The hover card over a reference, portaled once for the whole app. Its title reads every
  // address in it as its short form, as a card's does (above).
  {
    files: [
      "packages/dispatch/web/src/app.tsx",
      "packages/dispatch/web/src/features/refs/RefPreview.tsx",
    ],
    reach: "Hover a reference link anywhere, `#ref-preview`",
    record: true,
    rendered: true,
    surface: "reference-hover-card",
  },
  {
    files: ["packages/dispatch/web/src/features/refs/RefPreview.tsx"],
    reach: "Hover a reference to an ask, `#ref-preview`",
    record: true,
    rendered: true,
    surface: "reference-hover-card-ask",
  },
  // The search palette, which Ctrl+K opens on every route.
  {
    files: ["packages/dispatch/web/src/features/search/AskHitStatus.tsx"],
    reach: "Ctrl+K, an ask hit's question and answer",
    record: true,
    rendered: true,
    surface: "search-ask-status",
  },
  {
    files: ["packages/dispatch/web/src/features/search/AskHitStatus.tsx"],
    reach: "Ctrl+K, an ask hit's `Chose …`",
    record: true,
    rendered: true,
    surface: "search-ask-chosen-options",
  },
  {
    files: ["packages/dispatch/web/src/features/search/SearchPalette.tsx"],
    reach: "Ctrl+K, a result row's snippet",
    record: true,
    rendered: true,
    surface: "search-result",
  },
  // A row of the reference picker, in every box that carries it.
  {
    files: ["packages/dispatch/web/src/features/refs/RefPickerList.tsx"],
    proof: {
      file: "packages/dispatch/web/src/features/refs/useRefPicker.test.tsx",
      test: "a hit's row reads its snippet as its plain words",
    },
    reach:
      "Any box with the picker: `[[` and two letters, a hit's row; ArrowRight on a hit, a block's row",
    reason: `A picker row is an option the reader takes, worded with a hit's matched words or the head of a block's text. ${PLAIN_WORDS_REASON} ${LOAD_STATE_REASON}`,
    record: true,
    rendered: false,
    surface: "ref-picker-row-label",
  },
  // The five places a record's text is still written into an element that can hold no markup.
  {
    files: ["packages/dispatch/web/src/features/agents/AgentMessageComposer.tsx"],
    reach: "/agents, a row composer's issue `<option>`",
    reason: `${NO_MARKUP_REASON} An \`<option>\` can hold no markup, and an issue's key and title are that one line; the plain-text projection (\`PlainText\`) is for a record's own markdown, which a title is not.`,
    record: false,
    rendered: false,
    surface: "agent-message-issue-option",
  },
  {
    files: ["packages/dispatch/web/src/features/issue/IssuePage.tsx"],
    reach: "/issues/<ISSUE>, the browser's title bar",
    reason: `${NO_MARKUP_REASON} The title bar holds an issue's key and title.`,
    record: false,
    rendered: false,
    surface: "issue-page-title",
  },
  {
    files: ["packages/dispatch/web/src/features/document/DocumentPage.tsx"],
    reach: "/projects/<PROJECT>/documents/<SLUG>, the browser's title bar",
    reason: `${NO_MARKUP_REASON} The title bar holds a document's name.`,
    record: false,
    rendered: false,
    surface: "document-page-title",
  },
  {
    files: ["packages/dispatch/web/src/features/artifacts/AgentArtifactPage.tsx"],
    reach: "/agents/<SESSION>/artifacts/<SLUG>, the browser's title bar",
    reason: `${NO_MARKUP_REASON} The title bar holds an artifact's name.`,
    record: false,
    rendered: false,
    surface: "agent-artifact-page-title",
  },
  {
    files: [
      "packages/dispatch/web/src/features/inbox/question-label.ts",
      "packages/dispatch/web/src/features/inbox/useAskAnswerForm.ts",
      "packages/dispatch/web/src/features/inbox/AskThread.tsx",
      "packages/dispatch/web/src/features/inbox/DeclineControl.tsx",
      "packages/dispatch/web/src/features/issue/CreateIssueDialog.tsx",
    ],
    proof: {
      file: "packages/dispatch/web/src/features/inbox/question-label.test.tsx",
      test: "an answer's label reads the question's plain-text projection",
    },
    reach: "Any route, the Writes indicator's line for a write that is out",
    reason:
      "A write's label is one line the Writes indicator and its refusals read as text, naming which write this is, so it holds no markup. An ask's question is markdown, so a label on an ask draws the question's plain-text projection (`usePlainHeadline`, through `useQuestionLabel`): the words the card draws, formatting gone and each `dispatch://` address read as its target's name, never the question's source. An issue's title is one line of text already, so a new issue's label holds it as the characters it is.",
    record: false,
    rendered: false,
    surface: "write-label",
  },
  // Settings' delivery form. The same three lists `DISPATCH_WRITING_BOXES` excludes from the
  // picker are excluded from the renderer, for the same reason.
  {
    files: ["packages/dispatch/web/src/features/delivery/DeliverySettingsForm.tsx"],
    reach: "/settings, the Production jobs box",
    reason: SETTINGS_REASON,
    record: false,
    rendered: false,
    surface: "delivery-production-jobs",
  },
  {
    files: ["packages/dispatch/web/src/features/delivery/DeliverySettingsForm.tsx"],
    reach: "/settings, the Population authors box",
    reason: SETTINGS_REASON,
    record: false,
    rendered: false,
    surface: "delivery-population-authors",
  },
  {
    files: ["packages/dispatch/web/src/features/delivery/DeliverySettingsForm.tsx"],
    reach: "/settings, the Excluded repositories box",
    reason: SETTINGS_REASON,
    record: false,
    rendered: false,
    surface: "delivery-excluded-repos",
  },
];
