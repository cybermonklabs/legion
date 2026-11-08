import type {
  Ask,
  AskAnswerWithTrees,
  AskEditWithTrees,
  AskFollower,
  AskKind,
  AskLastReply,
  AskReach,
  AskWithTrees,
  InboxDocument,
  IssueAskRow,
  OpenAskOwner,
} from "./dispatch-api-asks";
import type { CommentWithTrees } from "./dispatch-api-comments";
import type { Actor, Anchor, AskUrgency, DisplayTree } from "./dispatch-api-core";
import type { ArtifactApprovalWithTrees } from "./dispatch-api-documents";
import type { Issue, IssuePriority } from "./dispatch-api-issues";
import type { AskTurn, SessionAnswerRelation } from "./dispatch-api-limits";

/**
 * What a person and a session each have waiting: the Inbox's open asks, the threads of an ask,
 * the answers a person wrote, the replies they are owed, and what Dispatch delivered to one
 * session.
 */

/** An ask's thread as a list row carries it: everything a card needs before its first targeted
 *  thread refresh. */
export interface AskThread extends AskReach {
  readonly replies: CommentWithTrees[];
  /** Every rewording of the question, oldest first; empty when never edited. */
  readonly edits: AskEditWithTrees[];
  /** Every answer, oldest first: empty on an Inbox row, which is an open ask, and the answers of
   *  an answered ask on `GET /api/v1/issues/{key}/asks?thread=1`. */
  readonly answers: AskAnswerWithTrees[];
  /** The sessions this ask's answer and replies reach directly, oldest first. */
  readonly followers: AskFollower[];
}

/** An open ask as returned by the human Inbox. */
export interface InboxRow extends AskWithTrees {
  /** The owning issue's coarse priority, or null for an unset or unassigned issue. */
  readonly priority: IssuePriority | null;
  /** When the viewer's own snooze on this row ends, or null when they have not snoozed it.
   *  The server surfaces it and never filters on it: the row is listed either way, and the
   *  Inbox folds it into `Later` only while this moment is still ahead. */
  readonly snoozed_until: string | null;
  /** The card's initial thread; `["ask-thread", id]` remains its targeted live refresh. */
  readonly thread: AskThread;
}

/** One ask of `GET /api/v1/issues/{key}/asks?thread=1`: the row above, plus the thread that
 *  renders the card whole without a fetch per ask. A reader asks for this shape by asking for
 *  the thread, so the thread is always there. */
export interface IssueAskThreadRow extends IssueAskRow {
  /** The card's initial thread; `["ask-thread", id]` remains its targeted live refresh. */
  readonly thread: AskThread;
}

export interface AskRead extends AskReach {
  readonly ask: AskWithTrees;
  readonly replies: CommentWithTrees[];
  /** Every rewording of the question, oldest first; empty when never edited. */
  readonly edits: AskEditWithTrees[];
  /** Every answer, oldest first; a restored document block does not repeat its answer here. */
  readonly answers: AskAnswerWithTrees[];
  /** The sessions this ask's answer and replies reach directly, oldest first. */
  readonly followers: AskFollower[];
}

export interface AskFollowersRead {
  readonly followers: AskFollower[];
}

export interface CommentRead {
  readonly comment: CommentWithTrees;
  readonly replies: CommentWithTrees[];
}

/** What `POST /api/v1/artifacts/{id}/approval-requests` answers: the document's open request (null
 *  when its latest version is already approved), the version it names, and the document's approval
 *  as the call left it. */
export interface ApprovalRequestResponse {
  readonly ask: Ask | null;
  readonly artifact_id: string;
  readonly version: number;
  readonly approval: ArtifactApprovalWithTrees;
  /** One line per requirement of the issue's spec with no check, or the single line `the spec
   *  marks no requirements` for a spec that marks none, as `IssueCoverage.gaps` writes them; no
   *  line at all for a spec every requirement of which has a check, empty for a document that is
   *  not a spec, and never a missing plan task, since the plan comes after the approval. Absent
   *  from a Dispatch older than requirement coverage. */
  readonly coverage?: readonly string[];
  /** True when the spec's coverage could not be read, so the empty `coverage` is no answer about
   *  it. The request is recorded either way: a coverage read refuses nothing. Absent otherwise. */
  readonly coverage_unread?: boolean;
}

export interface OpenAsk {
  readonly id: string;
  /** Relative Dispatch deep link to the ask itself: `/issues/KEY/asks/ID` for an issue ask (the
   *  item route, which selects an anchored ask on its document and focuses an unanchored one's
   *  Conversation turn), `/projects/PROJECT/documents/SLUG?ask=ID` for a document ask. */
  readonly ref: string;
  readonly question: string;
  readonly kind: AskKind;
  readonly urgency: AskUrgency;
  readonly created_at: string;
  readonly age_seconds: number;
  readonly priority: IssuePriority | null;
  readonly owner: OpenAskOwner;
  /** True when any human has replied, even if the agent spoke last. */
  readonly human_replied: boolean;
  readonly last_reply: AskLastReply | null;
  /** Whose reply would move the currently open ask forward. */
  readonly waiting_on: AskTurn;
}

export interface OpenAsksResponse {
  readonly session_id: string;
  readonly as_of: string;
  /** An ask was opened by this session at or after the supplied server baseline. */
  readonly opened_since: boolean;
  readonly count: number;
  readonly waiting_on_human: number;
  readonly waiting_on_agent: number;
  readonly asks: OpenAsk[];
}

/** One answer or ask reply written by the caller on `GET /api/v1/me/answers`. */
export interface MyAnswerRow {
  readonly kind: "answer" | "reply";
  readonly at: string;
  readonly ask_id: string;
  /** Relative Dispatch deep link to the ask itself, as on `OpenAsk.ref`. */
  readonly ref: string;
  readonly question: string;
  /** The display tree of `question`, absent unless the request sent `Dispatch-Trees: 1`. */
  readonly question_tree?: DisplayTree;
  readonly ask_kind: AskKind;
  readonly ask_state: Ask["state"];
  readonly edited_at: string | null;
  readonly owner: OpenAskOwner;
  readonly answer?: AskAnswerWithTrees;
  /** True when an answer row's answer is still the ask's current answer. */
  readonly current: boolean;
  readonly reply?: {
    readonly id: string;
    readonly body: string;
    /** The display tree of `body`, absent unless the request sent `Dispatch-Trees: 1`. */
    readonly body_tree?: DisplayTree;
  };
}

export interface MyAnswersResponse {
  readonly rows: MyAnswerRow[];
  readonly total: number;
  readonly limit: number;
  readonly offset: number;
}

/** The newest write someone else made in a thread on `GET /api/v1/me/replies`: a reply (a comment
 *  or a message), or the ask's answer, whose `id` is the ask's, whose `author` is the person who
 *  answered and whose `first_line` is the chosen options and the answer's text. */
export interface MyReplyReply {
  readonly id: string;
  readonly kind: "reply" | "answer";
  readonly author: Actor;
  /** The reply's or the answer's first non-empty line, at most 160 characters. */
  readonly first_line: string;
  readonly created_at: string;
}

/** What every thread the caller is reading in the Inbox carries on `GET /api/v1/me/replies`,
 *  whichever of the two row shapes below it is. */
interface MyReplyRowBase {
  /** The ask's id, the comment or message thread's root id, or an Agent row's root message id:
   *  what `PUT /api/v1/me/threads/{id}/read` marks. */
  readonly thread_id: string;
  /** Relative Dispatch deep link to the thread's root, never to the reply; the session's live
   *  view, `/agents/<session id>/live`, for an Agent row. */
  readonly ref: string;
  /** The ask's question, or the root's first line, at most 160 characters. */
  readonly subject: string;
  readonly reply: MyReplyReply;
  /** Whether the row still holds a write the caller has not read: always true under
   *  `state=unread`, either way under `state=inbox` and `state=dismissed`. */
  readonly unread: boolean;
  /** How many writes in the thread the caller has not read: for a Reply row the comments,
   *  messages and answers somebody else wrote after `read_through`, for an Agent row the session
   *  replies in `unread_reply_ids`. */
  readonly unread_count: number;
  /** When the caller dismissed the thread out of their Inbox, absent on a thread that is in it.
   *  It is the order `state=dismissed` answers in and the cursor it resumes from. */
  readonly dismissed_at?: string;
}

/** A Reply row: an ask's thread, a comment thread by its root, or an issue message thread by its
 *  root, which somebody else has written in. */
export interface MyReplyThreadRow extends MyReplyRowBase {
  readonly kind: "ask" | "comment" | "message";
  readonly issue?: Pick<Issue, "key" | "title">;
  readonly document?: InboxDocument;
  /** The owning issue's coarse priority, absent on a row with no issue or none set. */
  readonly priority?: IssuePriority;
  /** The thread owner's project key, absent where the owner carries none. */
  readonly project?: string;
  /** The caller's read mark on the thread (`user_threads.read_through`), which every Reply row
   *  carries: the column is not null, and a thread the caller has never read is marked at the
   *  moment it became theirs, so a write after the mark is one they have not read. */
  readonly read_through: string;
  /** The document mark the thread's root is anchored to, when it has one. */
  readonly anchor?: Anchor;
  /** The row's own version (`user_threads.version`), which `user_threads_record` increments on
   *  every write somebody else makes in the thread, and which a dismissal of the row is held to:
   *  `InboxDismissRequest.seen_replies` carries it back and the route writes the row's mark only
   *  while the row still holds it, so a press can bury no write this page never drew. An Agent
   *  row has no `user_threads` row and so no version of its own - the field means nothing on one,
   *  and what a dismissal of one carries is `InboxDismissSeenAgent` instead. */
  readonly version: number;
}

/** An Agent row: one of the caller's one-off messages to an agent session, with a reply from
 *  that session. It is on no issue and in no document, so it carries no owner, no priority, no
 *  project, no anchor and no read mark - its replies are read by id. */
export interface MyReplyAgentRow extends MyReplyRowBase {
  readonly kind: "agent";
  /** The agent session the thread was sent to: its id and the name Dispatch knows it by (its
   *  newest stamped session title, or the id when it has stamped none). */
  readonly session: { readonly id: string; readonly name: string };
  /** The unread session replies, which the Inbox marks read by id; empty on a read row. */
  readonly unread_reply_ids: string[];
}

/** One thread the caller is reading in the Inbox on `GET /api/v1/me/replies`: a Reply row - an
 *  ask's thread, a comment thread by its root, or an issue message thread by its root, which
 *  somebody else has written in - or an Agent row, one of the caller's one-off messages to an
 *  agent session with a reply from that session. The JSON names which it is in `kind`, and each
 *  shape's own fields are always sent for it. */
export type MyReplyRow = MyReplyThreadRow | MyReplyAgentRow;

/** `GET /api/v1/me/replies`'s answer in one state. `unread` and `inbox` answer every matching row
 *  when the caller names no `limit`, which is what the Inbox reads; `dismissed` answers
 *  `INBOX_DISMISSED_PAGE` rows at a time and names where the next page resumes in `next_cursor`. */
export interface MyRepliesResponse {
  readonly rows: MyReplyRow[];
  /** How many rows the state holds in all - except under `state=dismissed`, where it is how many
   *  this page answers: that view reads a page at a time from `next_cursor`, which is what says
   *  whether another follows, and counting every thread a person has ever dismissed is the most
   *  expensive read the Inbox has. */
  readonly total: number;
  /** The page size the caller named, or 0 when they named none and the answer holds every row. */
  readonly limit: number;
  readonly offset: number;
  /** The database time of the one snapshot every row was read in, which a mark-all passes back as
   *  its read mark. */
  readonly as_of: string;
  /** Where the next `state=dismissed` page resumes, as its `?cursor=`: sent by a page that filled,
   *  absent on the last one. Following it may answer no row, when the rows it names have been
   *  restored since. */
  readonly next_cursor?: string;
}

/** The states `GET /api/v1/me/replies`'s `?state=` reads the caller's threads in: `unread`, the
 *  default and the Inbox count's own read, which answers the rows somebody else has written in
 *  since they read them; `inbox`, every row in their Inbox, read or not, less what they have
 *  dismissed; and `dismissed`, what they dismissed out of it, newest dismissal first. A thread
 *  stays in the Inbox, read or not, until it is dismissed (`InboxDismissRequest` below). */
export const INBOX_LIST_STATES = ["unread", "inbox", "dismissed"] as const;
export type InboxListState = (typeof INBOX_LIST_STATES)[number];

/** How many rows one `state=dismissed` page answers, and how many `?cursor=` reads again. */
export const INBOX_DISMISSED_PAGE = 200;

/** The two kinds of thread row the Inbox holds, as `GET /api/v1/me/replies`'s `?kind=` and the
 *  Mark all read body below both name them: a Reply row, a thread the reader wrote in where
 *  somebody else has written since, and an Agent row, a one-off message they sent a session
 *  that it answered. */
export const INBOX_THREAD_KINDS = ["reply", "agent"] as const;
export type InboxThreadKind = (typeof INBOX_THREAD_KINDS)[number];

/** A priority the Inbox's Mark all read narrows to: `P0` to `P3` as the Inbox's filter writes
 *  them, or `none` for a thread whose issue carries no priority, a thread on a project document,
 *  which has no issue, and an Agent thread, which has neither. */
export type MarkInboxReadPriority = "P0" | "P1" | "P2" | "P3" | "none";

/** The body of `POST /api/v1/me/inbox/read`, the Inbox's Mark all read: it marks exactly the
 *  threads `GET /api/v1/me/replies?state=unread` answers under the same narrowing whose newest
 *  reply is at or before `read_through`, which is the list's own `as_of`. Each absent list means
 *  every value, so a body of `read_through` alone marks the whole unread Inbox. A thread written
 *  in since `read_through` stays unread, and a thread already read past it keeps its later mark.
 *
 *  An Agent thread carries no issue, document, project, priority or urgency, so `issue`,
 *  `artifact`, and a `priority` or `project` list without `none`, each leave it out; a Reply
 *  thread carries no urgency either, so an `urgency` list without `none` marks nothing at all. */
export interface MarkInboxReadRequest {
  /** RFC3339; stored as no later than now. Missing, unparsable or more than a minute ahead of
   *  the server is 400 INVALID_STATE. */
  readonly read_through: string;
  /** Both kinds by default, unlike the list route's default of `reply` alone. */
  readonly kind?: InboxThreadKind[];
  readonly priority?: MarkInboxReadPriority[];
  readonly urgency?: (AskUrgency | "none")[];
  /** Project keys, and `none` for a thread with no project. */
  readonly project?: string[];
  /** One issue's threads. With `artifact` it is 400 INVALID_QUERY. */
  readonly issue?: string;
  /** One project document's threads, by document id. With `issue`, or when it is not a document
   *  id, it is 400 INVALID_QUERY. */
  readonly artifact?: string;
}

/** `POST /api/v1/me/inbox/read`'s answer: how many threads it marked - the Reply threads whose
 *  mark moved plus the Agent threads whose replies it read. A request that moves nothing answers
 *  0 and announces nothing. */
export interface MarkInboxReadResponse {
  readonly marked: number;
}

/** A row id `POST /api/v1/me/inbox/dismiss` names a thread by, as the Inbox draws its rows: a Reply
 *  row is `reply:<thread id>` and an Agent row `agent:<root message id>`, each id a uuid. An Ask
 *  row, the third kind the Inbox draws, has no Dismiss at all, and naming `ask:<id>` is
 *  400 INVALID_QUERY; the Reply row of an open ask's thread is an ordinary `reply:` id. A
 *  dismissal's `seen_replies` keys its entries by these same ids. */
export const INBOX_DISMISS_ROW_PREFIXES = { reply: "reply:", agent: "agent:" } as const;

/** What a dismissal carries for one Reply row: the row's own version as
 *  `GET /api/v1/me/replies` answered it (`MyReplyThreadRow.version`), and nothing else - a
 *  `reply:` row is one `user_threads` row, and that row's version is the whole of what the page
 *  drew of it. */
export interface InboxDismissSeenReply {
  readonly version: number;
}

/** What a dismissal carries for one Agent row, which has no `user_threads` row and so no version:
 *  the two things the page drew of its thread, as `GET /api/v1/me/replies` answered them. */
export interface InboxDismissSeenAgent {
  /** The newest session reply the row showed, which is that row's `reply.id`. */
  readonly newest_reply: string;
  /** The ids the row drew as unread, which is that row's `unread_reply_ids`; empty on a read
   *  row. */
  readonly unread_replies: string[];
}

/** One entry of `InboxDismissRequest.seen_replies`, whichever kind of row it is keyed by. */
export type InboxDismissSeen = InboxDismissSeenReply | InboxDismissSeenAgent;

/** The body of `POST /api/v1/me/inbox/dismiss`, which takes a thread out of the Inbox and puts it
 *  back. A thread stays in the Inbox, read or not, until it is dismissed, as an email stays until
 *  it is archived: reading it and `POST /api/v1/me/inbox/read` leave it there.
 *
 *  A request names either `threads`, the row ids to change, or the list's own narrowing with
 *  `dismiss_through`, never both (400 INVALID_QUERY). Under the narrowing it changes exactly the
 *  threads `GET /api/v1/me/replies` answers under it whose newest reply, or whose dismissal, is at
 *  or before that mark, which is the list's own `as_of`, as Mark all read reads `read_through`.
 *
 *  A dismissal that names `threads` carries `seen_replies` with them, what the page drew in each
 *  of those rows. A row's version moves on every write somebody else makes in the thread, and the
 *  server writes that row's mark only while the row still holds the version the page drew, so a
 *  press can bury no write the page never drew. Why no comparison of times can stand in for that
 *  check is written once, on the increment in
 *  `20261105093501_user_threads_record_dismissed.up.sql`.
 *
 *  Dismissing marks a thread read as well, so a dismissed thread never counts as unread. A later
 *  write by somebody else brings a dismissed thread back unread; the caller's own reply does not,
 *  so a reply sent from the Dismissed view leaves the thread there. */
export interface InboxDismissRequest {
  /** The rows to change, as `INBOX_DISMISS_ROW_PREFIXES` spells them. With any narrowing field,
   *  `dismiss_through` included, it is 400 INVALID_QUERY. */
  readonly threads?: string[];
  /** What the page drew in each row `threads` names, keyed by that same row id: a `reply:` row's
   *  entry is `InboxDismissSeenReply`, an `agent:` row's `InboxDismissSeenAgent`, both as
   *  `GET /api/v1/me/replies` answered them. The route holds each row to its entry before it
   *  writes the row's mark.
   *
   *  Refusals, each 400 INVALID_QUERY: a row `threads` names that this leaves out, which is an
   *  old client; a key naming no row `threads` names; a `reply:` entry with no `version`, or
   *  carrying an Agent field; an `agent:` entry missing either of its two fields, or carrying
   *  `version`; a value of the wrong type, or an id that is not a uuid; this field on a restore,
   *  which draws the rows back rather than burying anything; and this field with the filtered
   *  form, where the server draws the rows itself. */
  readonly seen_replies?: Readonly<Record<string, InboxDismissSeen>>;
  /** RFC3339; stored as no later than now. Required when the request names the narrowing rather
   *  than `threads`: missing, unparsable or more than a minute ahead of the server is
   *  400 INVALID_STATE. */
  readonly dismiss_through?: string;
  /** True clears the marks instead of writing them, so the threads are in the Inbox again, drawn
   *  as they were read. */
  readonly restore?: boolean;
  /** Both kinds by default, as Mark all read reads them. */
  readonly kind?: InboxThreadKind[];
  readonly priority?: MarkInboxReadPriority[];
  readonly urgency?: (AskUrgency | "none")[];
  /** Project keys, and `none` for a thread with no project. */
  readonly project?: string[];
  /** One issue's threads. With `artifact` it is 400 INVALID_QUERY. */
  readonly issue?: string;
  /** One project document's threads, by document id. With `issue`, or when it is not a document
   *  id, it is 400 INVALID_QUERY. */
  readonly artifact?: string;
}

/** `POST /api/v1/me/inbox/dismiss`'s answer: how many threads it changed - the Reply threads whose
 *  mark it wrote or cleared plus the Agent threads whose dismissal row it wrote or deleted. A
 *  request that changes nothing answers 0 and announces nothing. */
export interface InboxDismissResponse {
  readonly dismissed: number;
}

/** One thing a person wrote that Dispatch delivered to a session, on
 *  `GET /api/v1/agents/{session_id}/answers`: an ask's current answer, a comment at its current
 *  body, or a message. The list holds each `(kind, id)` once, whichever ways it reached the
 *  session. Mirrors `model.SessionAnswer`. */
export interface SessionAnswer {
  /** `answer` is an ask's current answer, `comment` a comment (a reply in an ask's thread
   *  included), `message` a message. */
  readonly kind: "answer" | "comment" | "message";
  /** What the item is to the session. */
  readonly relation: SessionAnswerRelation;
  /** The ask's id for an answer, else the comment's or the message's id. */
  readonly id: string;
  /** When the person wrote it (RFC 3339, UTC): the current answer's time, or the comment's or the
   *  message's creation. The window and the order both read it. */
  readonly at: string;
  /** When a comment's body was last edited; absent on a comment never edited, and on an answer or
   *  a message. */
  readonly edited_at?: string;
  /** The person who wrote it: the list carries nothing an agent or the system wrote. */
  readonly author: Extract<Actor, { readonly kind: "user" }>;
  /** The person's words in full, never cut: the answer's text (`""` when the answer chose options
   *  only), the comment's current body, or the message's body. */
  readonly text: string;
  /** An answer's chosen option labels, in the order the person chose them; absent when it chose
   *  none, and on a comment or a message. */
  readonly selected?: string[];
  /** `true` on an answer when the ask has more than one answer, so this one replaced an earlier
   *  one; absent otherwise. */
  readonly replaced?: true;
  /** The ask the item answers or replies in, with the session's whole question (a reader cuts
   *  it): present on an answer and on a reply in an ask's thread, absent otherwise. */
  readonly ask?: { readonly id: string; readonly question: string };
  /** The issue (key and title) or the project document holding the item; null for a direct
   *  message, which is on no issue. */
  readonly owner: OpenAskOwner | null;
  /** The item's `dispatch://` address (`dispatch://KEY/ask/ID`, `dispatch://KEY/comment/ID`,
   *  `dispatch://KEY/message/ID`, or `dispatch://PROJECT/artifact/SLUG/ask/ID` and `/comment/ID`
   *  on a project document); null for a direct message, which has no address. */
  readonly ref: string | null;
  /** Relative Dispatch deep link to the item, as `DISPATCH_HREF_REFERENCES` lists the shapes, or
   *  `/agents/SESSION/live` for a direct message. */
  readonly href: string;
}

/** `GET /api/v1/agents/{session_id}/answers`: what people wrote to one session since `since`,
 *  newest first. Mirrors `model.SessionAnswersResponse`. */
export interface SessionAnswersResponse {
  readonly session_id: string;
  /** The window's start the server applied (RFC 3339, UTC): the request's `since`, or 72 hours
   *  before the read when it named none. */
  readonly since: string;
  /** How many items the window holds, `max_chars` notwithstanding. */
  readonly total: number;
  /** Newest first by `at`, then by `id`. With `max_chars`, the list stops after the item whose
   *  `text` takes the running sum of text lengths past it, so it can be shorter than `total`. */
  readonly items: SessionAnswer[];
}
