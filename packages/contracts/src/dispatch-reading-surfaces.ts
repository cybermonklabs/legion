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
 */

/** Where a reader reads a stored record: one id per surface, whatever route draws it. */
export type DispatchReadingSurface =
  | "activity-event-body"
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
  | "answers-reply"
  | "component-prose"
  | "conversation-reply"
  | "conversation-message"
  | "conversation-targeted-message"
  | "reply-quote"
  | "approval-review-reason"
  | "ask-card-question"
  | "ask-edit-history"
  | "ask-answer-history"
  | "answered-ask"
  | "resolved-ask"
  | "ask-option-text"
  | "ask-thread-reply"
  | "inbox-ask-row"
  | "approval-ask-question"
  | "inbox-thread-turn"
  | "my-reply-subject"
  | "inbox-thread-title"
  | "issue-decision"
  | "margin-decision-passage"
  | "margin-pinned-conversation"
  | "margin-suggestion-diff"
  | "margin-comment-body"
  | "margin-thread-preview"
  | "body-reference-cards"
  | "dispatch-reference-card"
  | "github-reference-card"
  | "reference-hover-card"
  | "reference-hover-card-ask"
  | "search-ask-status"
  | "search-result"
  | "agent-message-issue-option"
  | "issue-page-title"
  | "document-page-title"
  | "agent-artifact-page-title"
  | "write-label"
  | "delivery-production-jobs"
  | "delivery-population-authors"
  | "delivery-excluded-repos";

/** One row of DISPATCH_READING_SURFACES: one surface a reader reads a record on. */
export interface DispatchReadingSurfaceRow {
  readonly surface: DispatchReadingSurface;
  /** The source files that draw it, from the repository root. A rendered surface is found in
   *  one of them by its renderer mount; an excluded one by the comment there naming this row,
   *  which is the whole tie between the reason and the place it excuses. */
  readonly files: readonly string[];
  /** How a test reaches it: the route that draws it and the handle under that route. A route
   *  that names an issue, a project, a document or a session writes that part as `<ISSUE>`,
   *  `<PROJECT>`, `<SLUG>` or `<SESSION>`. */
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
}

/** Why a broadcast's own text is no record: the graph holds the copy each recipient got, never
 *  the broadcast, which is what the three broadcast surfaces say. */
const BROADCAST_REASON =
  "A broadcast is no record of the reference graph - the graph holds the copy each recipient got, never the broadcast - so a `#id` in it declares nothing and names nothing.";

/** Why a title bar, an `<option>` and a write's label hold the characters they are given: each
 *  is an element that can hold no markup, and what goes in it is one line of text already. */
const NO_MARKUP_REASON =
  "The element holds no markup, and the line it shows is one line of text already - no markdown body to render or strip - so it goes in as the characters it is.";

/** Why the delivery settings lists take neither the renderer nor the picker, which is the same
 *  reason `DISPATCH_WRITING_BOXES` gives their three boxes. */
const SETTINGS_REASON =
  "One setting per line rather than prose: a job name, a login or an owner/repo, which is stored and shown as the characters it is.";

export const DISPATCH_READING_SURFACES: readonly DispatchReadingSurfaceRow[] = [
  // An issue's Activity tab: one event's body, which is an ask's question, its answer, its
  // resolution reason, or the body of a comment or a message.
  {
    files: ["packages/dispatch/web/src/features/activity/EventBody.tsx"],
    reach: "/issues/<ISSUE>/activity, an event's `.dispatch-markdown`",
    record: true,
    rendered: true,
    surface: "activity-event-body",
  },
  // The Agents page: a reply under a row's exchange, and the targeted message a row shows.
  {
    files: ["packages/dispatch/web/src/features/agents/AgentsPage.tsx"],
    reach: "/agents, a row's reply turn",
    record: true,
    rendered: true,
    surface: "agents-row-reply",
  },
  {
    files: ["packages/dispatch/web/src/features/agents/AgentsPage.tsx"],
    reach: "/agents, a row's targeted message card",
    record: true,
    rendered: true,
    surface: "agents-row-message",
  },
  // A session's live conversation, which Dispatch stores nothing of and indexes nothing from.
  {
    files: ["packages/dispatch/web/src/features/agent-view/AgentThread.tsx"],
    reach: "/agents/<SESSION>/live, a turn's text part",
    reason:
      "A session's own turn is no record of the reference graph - it is the live transcript, which Dispatch stores nothing of and indexes nothing from - so a `#id` in it declares nothing and names nothing.",
    record: false,
    rendered: true,
    surface: "agent-turn-text",
  },
  {
    files: ["packages/dispatch/web/src/features/agent-view/AgentThread.tsx"],
    reach: '/agents/<SESSION>/live, `[data-testid="agent-reasoning"]`',
    reason:
      "The reasoning beside a turn is that same live transcript, stored nowhere and indexed from nowhere.",
    record: false,
    rendered: true,
    surface: "agent-turn-reasoning",
  },
  // A broadcast's page: a recipient's reply is a message of its issue and a record; the
  // broadcast's own body is not.
  {
    files: ["packages/dispatch/web/src/features/agents/BroadcastPage.tsx"],
    reach: "/agents/broadcasts/<ID>, a recipient row's reply",
    record: true,
    rendered: true,
    surface: "broadcast-recipient-reply",
  },
  {
    files: ["packages/dispatch/web/src/features/agents/BroadcastPage.tsx"],
    reach: "/agents/broadcasts/<ID>, the body under the heading",
    reason: BROADCAST_REASON,
    record: false,
    rendered: true,
    surface: "broadcast-sent-body",
  },
  {
    files: ["packages/dispatch/web/src/features/agents/BroadcastSends.tsx"],
    reach: "/agents, the Writes indicator's broadcast send row",
    reason: `${BROADCAST_REASON} A send still queued has not reached the server at all.`,
    record: false,
    rendered: true,
    surface: "broadcast-send-row",
  },
  {
    files: ["packages/dispatch/web/src/features/agents/BroadcastsPage.tsx"],
    reach: "/agents/broadcasts, a row's one-line body",
    reason: BROADCAST_REASON,
    record: false,
    rendered: true,
    surface: "broadcasts-list-body",
  },
  // The Answers page: the subject a row answers, the answer, and the reply that carried it.
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
    files: ["packages/dispatch/web/src/features/answers/AnswersPage.tsx"],
    reach: "/answers, a row's reply body",
    record: true,
    rendered: true,
    surface: "answers-reply",
  },
  // A component's prose, which belongs to the architecture model rather than to Dispatch.
  {
    files: ["packages/dispatch/web/src/features/architecture/ComponentDetails.tsx"],
    reach: "/projects/<PROJECT>/architecture, a component's Code & definition prose",
    reason:
      "A component's prose is the architecture model's, read from the repository, not a record of the reference graph - so a `#id` in it declares nothing and names nothing.",
    record: false,
    rendered: true,
    surface: "component-prose",
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
  // The line a reply quotes of what it answers, wherever a reply is drawn.
  {
    files: ["packages/dispatch/web/src/features/conversation/ReplyQuote.tsx"],
    reach: "/issues/<ISSUE>/conversation, the quoted line above a reply",
    record: true,
    rendered: true,
    surface: "reply-quote",
  },
  // A document's reviews dialog: the reason a reviewer asked for changes.
  {
    files: ["packages/dispatch/web/src/features/doc/ApprovalChip.tsx"],
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
  {
    files: ["packages/dispatch/web/src/features/inbox/AskOptionRow.tsx"],
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
  {
    files: ["packages/dispatch/web/src/features/inbox/ThreadTitle.tsx"],
    reach: "/, a thread row's title",
    record: true,
    rendered: true,
    surface: "inbox-thread-title",
  },
  // The issue's Decisions panel, which names the decisions document by its artifact id.
  {
    files: ["packages/dispatch/web/src/features/issue/DecisionsPanel.tsx"],
    reach: "/issues/<ISSUE>/decisions, a decision's paragraphs",
    record: true,
    rendered: true,
    surface: "issue-decision",
  },
  // A document's margin: the decision card, the pinned conversations, a suggestion's two sides
  // and a thread's comments.
  {
    files: ["packages/dispatch/web/src/features/margin/DecisionCard.tsx"],
    reach: "/projects/<PROJECT>/documents/<SLUG>, a margin decision card's passage",
    reason:
      "This is a passage of the issue's `decisions.md`, and the decisions view names that document by its address alone (`IssueDecisions.document`) - never by the artifact id a `TextSource` is - so no record reaches this surface. The Decisions panel draws the same passages with theirs (`issue-decision`).",
    record: false,
    rendered: true,
    surface: "margin-decision-passage",
  },
  {
    files: ["packages/dispatch/web/src/features/margin/PinnedTab.tsx"],
    reach: "/projects/<PROJECT>/documents/<SLUG>, the margin's Pinned tab",
    record: true,
    rendered: true,
    surface: "margin-pinned-conversation",
  },
  {
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
  // The cards under a body: the renderer draws them itself, so no surface pairs a card with a
  // body by hand.
  {
    files: ["packages/dispatch/web/src/features/refs/MarkdownBody.tsx"],
    reach:
      "Any rendered body whose text is nothing but references, `.dispatch-markdown` + the cards after it",
    record: true,
    rendered: true,
    surface: "body-reference-cards",
  },
  {
    files: ["packages/dispatch/web/src/features/refs/Unfurl.tsx"],
    reach: "A Dispatch reference card under a body, its two rendered lines",
    record: true,
    rendered: true,
    surface: "dispatch-reference-card",
  },
  // The one reader this change does not fold: a GitHub issue or pull request, matched and drawn
  // by the card itself.
  {
    files: ["packages/dispatch/web/src/features/refs/Unfurl.tsx"],
    reach: "A GitHub issue or pull-request card under a body",
    reason:
      "A GitHub issue or pull request is no record of this Dispatch and no `dispatch://` address names one: the card matches the GitHub URL itself and reads the title and body from GitHub's API, which is the one reader this change does not fold into the shared one.",
    record: false,
    rendered: false,
    surface: "github-reference-card",
  },
  // The hover card over a reference, portaled once for the whole app.
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
    files: ["packages/dispatch/web/src/features/search/SearchPalette.tsx"],
    reach: "Ctrl+K, a result row's snippet",
    record: true,
    rendered: true,
    surface: "search-result",
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
      "packages/dispatch/web/src/features/inbox/useAskAnswerForm.ts",
      "packages/dispatch/web/src/features/inbox/AskThread.tsx",
      "packages/dispatch/web/src/features/issue/CreateIssueDialog.tsx",
    ],
    reach: "Any route, the Writes indicator's line for a write that is out",
    reason: `${NO_MARKUP_REASON} A write's label is one line the Writes indicator and its refusals read as text, and it names which write this is, so it stays the words the reader was shown - never a title drawn over them.`,
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
