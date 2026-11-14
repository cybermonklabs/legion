import type { AskThread, IssueAskRow, MyReplyRow, MyReplySession } from "./dispatch-api.generated";

/**
 * What a person and a session each have waiting: the narrowings the Inbox reads its rows
 * through, the closed sets its filters are written in, and what a dismissal carries back. Every
 * body the Inbox's routes answer is generated from the Go route table.
 */

/** One ask of `GET /api/v1/issues/{key}/asks?thread=1`: the issue's ask row plus the thread that
 *  renders the card whole without a fetch per ask. A reader asks for this shape by asking for
 *  the thread, so the thread is always there. */
export interface IssueAskThreadRow extends IssueAskRow {
  /** The card's initial thread; `["ask-thread", id]` remains its targeted live refresh. */
  readonly thread: AskThread;
}

/** A Reply row of the Inbox, narrowed from the row `GET /api/v1/me/replies` answers: an ask's
 *  thread, a comment thread by its root, or an issue message thread by its root, which somebody
 *  else has written in. The route answers one row shape for both kinds and says which it is in
 *  `kind` (`MyReplyRow`, generated), so what this adds is what a row of this kind always carries:
 *  `read_through`, the caller's read mark on the thread (`user_threads.read_through` is not null,
 *  and a thread they have never read is marked at the moment it became theirs), and `version`,
 *  the row's own version, which `user_threads_record` increments on every write somebody else
 *  makes in the thread and which a dismissal of the row is held to (`InboxDismissSeenReply`). */
export interface MyReplyThreadRow extends MyReplyRow {
  readonly kind: "ask" | "comment" | "message";
  readonly read_through: string;
  readonly version: number;
}

/** An Agent row of the Inbox, narrowed the same way: one of the caller's one-off messages to an
 *  agent session, with a reply from that session. It is on no issue and in no document, so it
 *  carries no owner, no priority, no project, no anchor and no read mark - its replies are read
 *  by id, which is why `session`, the session the thread was sent to, and `unread_reply_ids`,
 *  the ids the row drew as unread, are the two a row of this kind always carries. */
export interface MyReplyAgentRow extends MyReplyRow {
  readonly kind: "agent";
  readonly session: MyReplySession;
  readonly unread_reply_ids: string[];
}

/** `row` as the shape its own `kind` names. The route answers both kinds under one schema - the
 *  fields of each are optional on `MyReplyRow`, and `kind` is the open string the Go field
 *  carries - so this is the one place the two are told apart, and a surface that reads a field
 *  only one kind has narrows here rather than widening itself. A `kind` this build does not know
 *  reads as a Reply row, which is what every surface already draws for one. */
export function inboxThreadRow(row: MyReplyRow): MyReplyThreadRow | MyReplyAgentRow {
  return row.kind === "agent" ? (row as MyReplyAgentRow) : (row as MyReplyThreadRow);
}

/** The states `GET /api/v1/me/replies`'s `?state=` reads the caller's threads in: `unread`, the
 *  default and the Inbox count's own read, which answers the rows somebody else has written in
 *  since they read them; `inbox`, every row in their Inbox, read or not, less what they have
 *  dismissed; and `dismissed`, what they dismissed out of it, newest dismissal first. A thread
 *  stays in the Inbox, read or not, until it is dismissed (`InboxDismissRequest`). */
export const INBOX_LIST_STATES = ["unread", "inbox", "dismissed"] as const;
export type InboxListState = (typeof INBOX_LIST_STATES)[number];

/** How many rows one `state=dismissed` page answers, and how many `?cursor=` reads again. */
export const INBOX_DISMISSED_PAGE = 200;

/** The two kinds of thread row the Inbox holds, as `GET /api/v1/me/replies`'s `?kind=` and the
 *  Mark all read body both name them: a Reply row, a thread the reader wrote in where
 *  somebody else has written since, and an Agent row, a one-off message they sent a session
 *  that it answered. */
export const INBOX_THREAD_KINDS = ["reply", "agent"] as const;
export type InboxThreadKind = (typeof INBOX_THREAD_KINDS)[number];

/** A priority the Inbox's Mark all read narrows to: `P0` to `P3` as the Inbox's filter writes
 *  them, or `none` for a thread whose issue carries no priority, a thread on a project document,
 *  which has no issue, and an Agent thread, which has neither. */
export type MarkInboxReadPriority = "P0" | "P1" | "P2" | "P3" | "none";

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

/** One entry of `InboxDismissRequest.seen_replies`, whichever kind of row it is keyed by. The
 *  route reads each entry in the shape its own row's kind takes and refuses it in its own words,
 *  so it holds them unparsed (`inboxDismissRequest.SeenReplies`) and the document types the map's
 *  values as bytes of their own; these two shapes are what each one must be. */
export type InboxDismissSeen = InboxDismissSeenReply | InboxDismissSeenAgent;
