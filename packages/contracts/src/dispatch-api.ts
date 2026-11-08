// The Dispatch API's wire shapes are generated from the Go route table through
// `schemas/dispatch-openapi.json`: `dispatch-api.generated.ts` is the one definition of every
// HTTP request and response body of that table, and this module re-exports it whole.
//
// Hand-written here is only what the route table does not hold:
//   - the Dispatch event payloads and the event envelope, which travel the message bus;
//   - the `GET /ws/live` socket's frames, a route the table does not carry, held against Go by
//     `fixtures/live/` and the server's `delivery_live_diff_test.go`;
//   - the closed string enums the OpenAPI document widens to `string`, with the `const` arrays
//     they are read from and the limits and header names `scripts/gen-go.ts` renders into Go;
//   - the helpers and client-side composites no route answers (`Advised`, `IssueRead`,
//     `ArtifactVersionContent`, `CreateArtifactInput`, …).
import { z } from "zod";
import type { AgentStreamResponder } from "./agent-stream";
import type {
  Actor,
  ArchitectureSource,
  Artifact,
  ArtifactVersionText,
  Ask,
  AskAnswer,
  AskEditPrevious,
  AskResolution,
  AskThread,
  Comment,
  DeliveryMeasuresResponse,
  DeliveryPipelineResponse,
  DeliveryTimelineWithMeasures,
  DocumentWriteAdvice,
  DuplicateCandidate,
  EditPreconditionCurrent,
  EditPreconditionMismatch,
  Issue,
  IssueAskRow,
  IssueClaim,
  IssueCollaboratorPerson,
  IssueDetails,
  Message,
  MyReplyRow,
  MyReplySession,
  Project,
  ProjectDefaultAssignee,
  RepoProject,
  SearchResponse,
  TargetCandidate,
  UserIssueState,
  Version,
  WhoamiAgent,
  WhoamiUser,
  WriteAdvice,
} from "./dispatch-api.generated";
import type { ASK_URGENCIES, DeliveryCapability, MessageDeliveryMode } from "./dispatch-constants";
import { DELIVERY_CAPABILITIES } from "./dispatch-constants";
import type { ISSUE_COMPONENTS_MODES, ISSUE_ROUTE_STATUSES } from "./dispatch-tools";
import type { GATED_STEPS } from "./legion-roles";

export * from "./dispatch-api.generated";

export type AskUrgency = (typeof ASK_URGENCIES)[number];

/** The OAuth scopes a Dispatch credential can hold, mirroring Go's `model.ScopePerson` and
 *  `model.ScopeApprove` and the `agent_tokens.scopes` check that stores them: `person` acts as
 *  the credential's own person on every route a person reaches through the dashboard, and
 *  `approve` settles what a person settles — a document review, the answer of an approval ask, a
 *  machine-login decision or revoke, and minting, changing or revoking a personal token. A
 *  credential holding `approve` holds `person` too. */
export const DISPATCH_SCOPES = ["person", "approve"] as const;

export type DispatchScope = (typeof DISPATCH_SCOPES)[number];

export type IssuePriority = 0 | 1 | 2 | 3;

export type IssueComponentsMode = (typeof ISSUE_COMPONENTS_MODES)[number];

/** Response-only; never on an event payload. The advice an issue's write carries, which reports
 *  the issue's status, the session's writes since the last human event and its open asks. */
export type Advised<T> = T & { readonly advice?: WriteAdvice };

/** Response-only. The advice a **project** document's write carries: a project document stands
 *  under no issue, so there is no issue status, no writes-since-human count and no issue-scoped
 *  open-ask list to report, and the server sends the document's block report alone
 *  (`withRawAdvice`, the server's `documentWriteAdvice`). `Advised` would promise fields that
 *  write never sends. */
export type DocumentAdvised<T> = T & { readonly advice?: DocumentWriteAdvice };

/**
 * The state of the session holding an issue's claim, as an issue read reports it beside the
 * claim: `live`, `unresponsive` (past the live window, inside the gone horizon - stalled, not
 * gone, and still the holder), `gone` (past the horizon, or no registration at all), or
 * `unknown` when the Envoy listener gave the read no list to judge it by.
 */
export type IssueClaimHolderState = "live" | "unresponsive" | "gone" | "unknown";

/**
 * Whether an issue's route reaches a running session right now: `live` when a live session
 * holds the role or the routed session is live, `no_holder` when nobody live holds the role (it
 * is unclaimed or its holder's session lapsed) or the session is not live, `unknown` when the
 * Envoy listener gave no list to judge it by (never read as live, nor as `no_holder`).
 */
export type IssueRouteStatus = (typeof ISSUE_ROUTE_STATUSES)[number];

/**
 * The longest `TmuxPlace.layout` either side handles: the cap the session's own client applies
 * before it sends a place (`packages/envoy-client/src/tmux-place.ts`, which drops a longer
 * layout and reports the place without it) and the cap a reader applies before it parses one
 * (`packages/dispatch/web/src/features/machines/tmux-layout.ts`). A layout grows with every
 * split, so a pathological window would make every heartbeat carry kilobytes a drawing can do
 * without; and because the listener stores whatever a session registers and Dispatch answers it
 * unchanged, a reader that trusted the sender's cap would parse a string no tmux wrote. One
 * number here, read by both, so neither side can drift from the other.
 */
export const TMUX_LAYOUT_MAX = 2_000;

/** The part an issue's document plays beside its spec. `decisions` is absent from a Dispatch
 *  older than the design gate's steps. */
export type DocumentRole = "verification" | "plan" | "decisions";

/** A design-gate step a Legion deployment gates, as Dispatch stores it: `decisions` names the
 *  root issue's decisions document, `criteria` its Verification document, `spec` its primary
 *  document. */
export type GatedStep = (typeof GATED_STEPS)[number];

/** Why someone is on `GET /api/v1/issues/{key}/collaborators`: the assignee, whoever claimed the
 *  issue, whoever posted on it or one of its documents, and whoever answered one of its asks. The
 *  set is the handler's own (`issue_collaborators.go`'s `collaboratorReasons`), so the document
 *  carries it as a closed union on each `reasons` array rather than as a schema of its own; this
 *  projects one element of that array, which is the name a consumer imports. */
export type CollaboratorReason = NonNullable<IssueCollaboratorPerson["reasons"]>[number];

/** Who gave a review: `person` for one a person gave, `plan_review` for a plan review's approval
 *  of a plan version. A review Dispatch carried forward to a later version keeps the kind of the
 *  review it carried, so a plan version restored to its approved text is still `plan_review`.
 *  Absent from a Dispatch older than review kinds, which wrote people's reviews alone. */
export type ArtifactReviewKind = "person" | "plan_review";

/** Who holds the turn on an open ask after a reply: `human` when the human needs to act,
 *  `agent` when the asking agent still owes the next move (a progress note). */
export const ASK_TURNS = ["human", "agent"] as const;

export type AskTurn = (typeof ASK_TURNS)[number];

/** One ask of `GET /api/v1/issues/{key}/asks?thread=1`: the row above, plus the thread that
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

/** What a `SessionAnswer` is to the session Dispatch delivered it to, one snake_case wire value per
 *  relation, each spelling the phrase a reader prints: a person answered the session's ask,
 *  replied in its ask's thread, replied in a comment thread it wrote in, commented on a document it
 *  wrote, commented on an issue routed to it, mentioned it, sent it a message (direct, on an issue,
 *  or a broadcast's copy), or replied to its message. Mirrored in Go as the
 *  `model.SessionAnswerRelation*` constants. */
export const SESSION_ANSWER_RELATIONS = [
  "answered_your_ask",
  "replied_on_your_ask",
  "replied_in_a_comment_thread",
  "commented_on_a_document",
  "commented_on_an_issue",
  "mentioned_you",
  "sent_you_a_message",
  "replied_to_your_message",
] as const;

export type SessionAnswerRelation = (typeof SESSION_ANSWER_RELATIONS)[number];

/** The reference-change pair every event that reports a graph write carries, mirroring the
 *  server's embedded `model.ReferenceChangesPayload`: one shape, so a new payload family
 *  extends it rather than re-typing the fields. */
export interface ReferenceChangesPayload {
  /** The nodes whose inbound reference edges this write moved, named so a reader carrying
   *  their batched backlink counts refreshes exactly those rows. Absent when the write moved
   *  no counted node, and on every event recorded before the field existed. */
  readonly references_changed?: ChangedReference[];
  /** The write moved more counted nodes of some kind than an event carries (`refs.CountedChangeLimit`
   *  of each kind): a reader that does not read `references_changed_truncated_kinds` refreshes
   *  every list that holds a count. */
  readonly references_changed_truncated?: boolean;
  /** The kinds whose targets the write cut from `references_changed`, set with
   *  `references_changed_truncated`; the targets of every other kind are still named. A reader
   *  refreshes every read of each kind listed, and only those. Absent from an event written
   *  before the field existed, whose truncation said nothing of its kinds. */
  readonly references_changed_truncated_kinds?: ChangedReference["kind"][];
}

/** Whose Inbox a write can have moved, mirroring the server's `ThreadLogins` payload field:
 *  sorted, `[]` when the write moves nobody's. On a comment, an ask or an issue message it is
 *  every login with a Replies row (the server's `user_threads`) on the write's thread once the
 *  write is in; on a direct (issue-less) message, which keeps no Replies row, it is the thread
 *  root author's login, the one Inbox that conversation's Agent row sits in. Each payload family
 *  names the event types that carry it; it is absent from every other type and from every event
 *  written before the field existed, which a reader takes as "anyone's Inbox may have moved". */
export interface ThreadLoginsPayload {
  readonly thread_logins?: string[];
}

/** What of an item an event carries, on the `comment.*`, `suggestion.*` and `message.*` payloads.
 *
 *  `body_truncated` is that `body` is the first part of the item's body and not all of it. A
 *  comment, comment edit, message or broadcast body is bounded block by block
 *  (`DISPATCH_BLOCK_UNITS` on each block, `DISPATCH_ITEM_BLOCKS` on how many it holds), so one
 *  body can be far longer than the `DISPATCH_EVENT_BODY_MAX` UTF-16 units an event carries, and
 *  the rest is one read away: `GET /api/v1/comments/{id}` or `GET /api/v1/messages/{id}`. Envoy's
 *  webhook normalizer writes a field of the same name, a string (`"true"`), on the payload of a
 *  GitHub review whose body it cut; that is another payload and another field, and a reader must
 *  not take one for the other.
 *
 *  `payload_omitted` is that the message bus refused this event whole, so Dispatch's outbox
 *  published a stand-in in its place: the payload holds the original's top-level fields that are
 *  short enough to send and nothing else, so `body` and every other long field is absent. The
 *  item itself answers the rest. */
export interface CarriedBodyPayload {
  readonly body_truncated?: boolean;
  readonly payload_omitted?: boolean;
}

/** What an `issue.created`, `issue.updated` or `issue.closed` write changed, read in its own
 *  transaction. Each field is absent from every other `issue.*` type and from an event written
 *  before the fields existed, which a reader takes as "anything may have changed". */
export interface IssueEventChanges {
  /** Each field whose value the write moved, of `title`, `status`, `closed_at` (for a close or a
   *  reopen), `rank`, `priority`, `assignee`, `parent`, `labels`, `route`, `repo`, `components`,
   *  `blocked_by` and `external_links`; for `issue.created`, every field the new issue holds a
   *  value in. */
  readonly changed?: string[];
  /** The issue's parent chain after the write, parent first up to the root; `[]` for a root. */
  readonly ancestors?: string[];
  /** Present only when the status or the assignee moved: for a close or a reopen every login
   *  with a Replies row on one of the issue's open asks, for an assignee move the assignee the
   *  issue was taken from and the one it was given to, for the two together the sorted union of
   *  both, and for any other status move `[]`. */
  readonly thread_logins?: string[];
  /** On an `issue.created` whose create went past the near-copy refusal with `force`: `past`
   *  names the issues that refusal named, and is the empty list when the check did not answer.
   *  A forced create the refusal would not have stopped carries no `forced` at all. */
  readonly forced?: { readonly past: readonly string[] };
}

/** Every `issue.*` event payload is the issue row, flat, plus what the write moved in the
 *  reference graph: a new issue's spec text is indexed in the creating transaction. */
export type IssueEventPayload = Issue & ReferenceChangesPayload & IssueEventChanges;

/** Every `ask.*` event payload is the ask row, flat, plus what the write moved in the reference
 *  graph. `restored` is true on the `ask.opened` or `ask.answered` settlement appends when an edit
 *  restores a decision block it retracted: the ask returns to the state it had, and no one answered
 *  anything new. A restore the release before Dispatch kept the mark writes - every one already in
 *  the event log, and one written during a rollout or an image rollback - carries no mark, so a
 *  missing `restored` does not prove an event is a new answer. `thread_logins` (the ask's own
 *  thread) is carried by `ask.opened`, `ask.answered` and `ask.resolved`. */
export type AskEventPayload = Ask &
  ReferenceChangesPayload &
  ThreadLoginsPayload & { readonly restored?: boolean };

/** An `ask.answered` payload. `previous_answer` is present when the answer replaced an earlier
 *  one; the asker and every follower receive it as they received the first. */
export type AskAnsweredEventPayload = AskEventPayload & {
  readonly previous_answer?: AskAnswer;
};

export type AskEditEventPayload = AskEventPayload & {
  readonly previous: AskEditPrevious;
  readonly edited_by: Actor;
  /** A version move of an approval request that follows an earlier move since the request was
   *  opened or last handed back: before this move, its `requested_version` was already below the
   *  version it named. The payload's `approval.version` is the version after the move, so the
   *  first move satisfies `requested_version < version` too and is not quiet. A quiet move changes
   *  only the version the request names, so its event, like a human's unnamed `artifact.version`,
   *  carries `notify: false` and reaches no follower. Absent on every other edit, the first move
   *  after an opening or a hand-back included, even when a thread reply left the request waiting
   *  on its agent. */
  readonly quiet?: true;
};

/** The durable result of one mention-delivery attempt in the issue event log. */
export interface CommentDeliveryEventPayload {
  readonly comment_id: string;
  /** The ask whose thread holds the comment, so a consumer keyed on the ask sees a receipt
   *  that changes the thread it renders; null when the comment belongs to no ask, and absent
   *  on a receipt a server older than the field wrote. */
  readonly ask_id?: string | null;
  readonly target: string;
  readonly attempt: number;
  readonly delivery: DeliveryCapability;
  readonly session_id: string | null;
  /** A receipt is only ever appended with a settled outcome: the sender that records it holds
   *  the attempt's claim, and both reply handlers settle the row in the statement that appends
   *  theirs. The attempt row itself reads `pending` between its commit and that outcome. */
  readonly state: "sent" | "failed";
  /** The stream already held this message: an earlier attempt landed, so this one is recognised as
   *  a repeat and not handed to the mentioned session again (`DELIVERY_DUPLICATE_WINDOW_MS`).
   *  Absent means false. */
  readonly duplicate?: boolean;
  readonly error?: string;
  readonly reply_id: string | null;
}

/**
 * The payload of every `comment.*` event. It is the comment row as it stood when the event was
 * written, and events are retained verbatim, so the row's later additions are optional here:
 * `mentions` and `deliveries` were added on 2026-09-18 (#1188), and every comment event recorded
 * before that day carries neither. A read row (`Comment`) always has both. `thread_logins` (the
 * comment's ask, else its root) is carried by `comment.created` and `comment.answered`.
 */
export interface CommentEventPayload
  extends Omit<Comment, "mentions" | "deliveries">,
    CarriedBodyPayload,
    ReferenceChangesPayload,
    ThreadLoginsPayload {
  readonly mentions?: Comment["mentions"];
  readonly deliveries?: Comment["deliveries"];
  readonly artifact_name: string;
  /** The document owner for an artifact comment, including direct author routes. */
  readonly project_key?: string;
  readonly artifact_slug?: string;
  /** The question text of the ask this comment replies to (Comment.ask_id); empty otherwise. */
  readonly ask_question?: string;
  /** The state of that ask when the comment was posted. A human reply while it is still `open`
   *  is a request for clarification: the asker answers in the thread or rewords the question. */
  readonly ask_state?: Ask["state"];
  /** The open ask's `waiting_on` once this comment is its newest reply, so a stream consumer can
   *  move the ask between "waiting on you" and "waiting on the agent" without re-reading it.
   *  Absent when the comment does not reply to an open ask. */
  readonly ask_waiting_on?: AskTurn;
}

/**
 * A node whose inbound reference edges one write moved, addressed the way the reads that show
 * it are keyed: an issue by key, an ask by id together with the issue or document whose ask list
 * holds its row, and a document or file (`artifact`) by its uuid (`id`, the node id its
 * `GET /api/v1/references` read reports) and its `ref`, the dispatch:// address its Referenced by
 * panel is opened on (`dispatch://CORE/artifact/runbook`, `dispatch://CORE-1/spec`,
 * `dispatch://CORE-1/artifact/notes-md`). An `artifact` entry carries neither `issue_key` nor
 * `artifact_id`; `ref` is absent for one whose owner names no address. A block of a document
 * (`block`) is named by its node id (`<document ref_key>#<block id>`, as its read reports it), its
 * `ref` (`dispatch://CORE-1/spec#b-intro`) and its document's uuid as `artifact_id`, whose panel
 * lists the edges at its blocks; a write names one whose inbound edges or whose text it moved. A
 * citation of a comment or message changes no row read by name and the graph query covers it. An
 * event written before `artifact` or `block` entries existed names none.
 */
export interface ChangedReference {
  readonly kind: "issue" | "ask" | "artifact" | "block";
  readonly id: string;
  readonly issue_key?: string | null;
  readonly artifact_id?: string | null;
  readonly ref?: string;
}

/** `thread_logins` is carried by `message.created` and `message.answered`: on an issue message
 *  the logins with a Replies row on its thread root (up `in_reply_to`), and on a direct
 *  (issue-less) conversation's message, which keeps no Replies row, the thread root author's
 *  login, the one Inbox that conversation's Agent row sits in (`[]` when no person opened it). */
export interface MessageEventPayload
  extends Message,
    CarriedBodyPayload,
    ReferenceChangesPayload,
    ThreadLoginsPayload {
  /** First 160 characters of the reply target's body, or of the block it names (`reply_block`);
   *  empty otherwise. */
  readonly reply_body?: string;
  /** The target of a reply's thread root: the conversation the reply lands in, for a consumer
   *  that groups a thread under its root. A session replying to a message aimed at that
   *  session inherits no target of its own, so its `target` names no conversation. Absent on a
   *  root message, whose `target` already names one, and under an untargeted thread. It is the
   *  root's `target` column verbatim, so it is whatever a target may be - `session:<id>` or
   *  the `role:<role>` route the thread was aimed at - and never resolved to a session. */
  readonly thread_target?: string;
}

export interface MessageDeliveryEventPayload {
  readonly message_id: string;
  readonly attempt: number;
  readonly delivery: MessageDeliveryMode;
  readonly session_id: string;
  readonly target?: string;
  readonly title: string;
  readonly state: "sent" | "failed";
  /** The stream already held this message: an earlier attempt landed, so this one is recognised as
   *  a repeat and not handed to the recipient again (`DELIVERY_DUPLICATE_WINDOW_MS`). Absent means
   *  false. */
  readonly duplicate?: boolean;
  readonly error?: string;
}

/** `message.accepted`: the session an attempt went to took the message as its user's own turn.
 *  It is never a second `message.delivery` receipt; the send still appends its own. `target` is
 *  the message's own, `session:<recipient>` for a direct message. */
export interface MessageAcceptedEventPayload {
  readonly message_id: string;
  readonly attempt: number;
  readonly session_id: string;
  readonly accepted_as: "user_turn";
  readonly target: string;
}

/** The kinds a search result and a write suggestion are one of. The route table carries `kind`
 *  as a plain string, and this is the closed list it means. */
export const SEARCH_RESULT_KINDS = ["issue", "document", "comment", "ask", "message"] as const;

export type SearchResultKind = (typeof SEARCH_RESULT_KINDS)[number];

/** The four paging fields a Dispatch that supports search paging always answers together:
 *  `total` is every match of every kind, and `reachable` is how many of `total` the pages can
 *  return — each kind lists only its best `SEARCH_KIND_DEPTH` (`dispatch-tools.ts`), so an
 *  offset at or past it returns no results. */
export type SearchResultsPage = Readonly<
  Pick<SearchResponse, "total" | "reachable" | "limit" | "offset">
>;

/** `SearchResultsPage` with every field retyped to `V`, always optional: the shape of "none of
 * these fields" for a sentinel `V` (`undefined` for the wire type below, `never` for
 * `fake-search-response.ts`'s stricter test-only override, which forbids supplying a concrete
 * value without forbidding an explicit `undefined` — TypeScript widens every optional property to
 * admit one regardless of its declared type unless the whole project turns on
 * `exactOptionalPropertyTypes`). */
export type SearchResultsPageAbsentAs<V> = { readonly [K in keyof SearchResultsPage]?: V };

/** `SearchResponse.degraded`'s one value today: meaning search could not run for this request (no
 * embedder configured, the query's own embedding timed out, or the model service answered an
 * error) and search fell back to keyword-only ranking. Generated into Go as
 * `contracts.SearchDegradedEmbedderUnavailable` so the string Dispatch writes and the string
 * `search-answer.ts` compares against cannot drift apart. */
export const SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE = "embedder_unavailable";

// GraphEdgeKind is an edge's kind: one of the structural kinds, or the name of a typed block's
// `block[]` attribute (BlockAttributeSchema), whose pointers are edges of that kind. The block
// schema decides those names (Go's pmdoc.LinkKinds), so any other string is admitted beside the
// structural ones, which keep their completion; none of them is ever an attribute's name.
export type GraphEdgeKind =
  | "mentions"
  | "child_of"
  | "blocked_by"
  | "attached_to"
  | "anchored_to"
  | "owned_by"
  | "replies_to"
  | "answers"
  | "followed_by"
  | "part_of"
  | "depends_on"
  | "affects"
  | (string & {});

// DecisionKeptEventPayload is the payload of `decision.kept`, which a person's Keep appends on
// the decision's issue: the document, the decision's heading block and the version its text stood
// at. It wakes nobody - a Keep records that a person agreed, which asks its agent for nothing.
export interface DecisionKeptEventPayload {
  readonly artifact_id: string;
  readonly block_id: string;
  readonly changed_version: number;
}

export interface ArtifactCreatedEventPayload extends ReferenceChangesPayload {
  readonly artifact: Artifact;
}

export interface ArtifactVersionEventPayload extends ReferenceChangesPayload {
  readonly artifact_id: string;
  readonly name: string;
  readonly version: Version;
  readonly diff?: string;
}

/** `artifact.human_edited`: a person's browser edits of a document, sent once the document has
 *  been quiet for 30 seconds after the last of them. `from_version` is the version before the
 *  quiet window's first version by a person, `to_version` the window's last one; `editors` names
 *  each person who wrote one, and the event's actor is the one who edited last. `diff` is the
 *  unified diff of the two versions' stored markdown, cut at its last line end within 4,096 bytes,
 *  `diff_truncated` then true; it is empty when a version is no longer stored. The event reaches
 *  each session the document credits and the issue's route, never a person. */
export interface ArtifactHumanEditedEventPayload {
  readonly artifact_id: string;
  readonly name: string;
  readonly from_version: number;
  readonly to_version: number;
  readonly editors: readonly string[];
  readonly diff: string;
  readonly diff_truncated: boolean;
}

/** `artifact.approved` and `artifact.changes_requested`: a human review of a document,
 *  pinned to `version`; `reason` is required for changes requested; `ask_id` names the
 *  approval ask the review answered, null when given from the document header with no
 *  approval ask open at that version. A third provenance is the carry: Dispatch records an
 *  `artifact.approved` in a human's name at the version his answer to a decision block writes,
 *  when that answer is the whole change since the version he approved, and at a version whose
 *  text is byte for byte the approved version's with no decision block in it decided since; its
 *  `reason` names the carry and the version it follows, and its `ask_id` is null unless a request
 *  was open at the carry, which it answers (see `ArtifactReview`). */
export interface ArtifactReviewEventPayload {
  readonly artifact_id: string;
  readonly name: string;
  readonly version: number;
  /** Who gave the review; absent from a Dispatch older than review kinds. */
  readonly kind?: ArtifactReviewKind;
  readonly actor: Actor;
  readonly reason: string | null;
  readonly ask_id: string | null;
  readonly content_sha256?: string;
  readonly content_sha256_source?: "insert" | "backfill";
}

/** Whether a recorded result still speaks for the work as it stands: `current` at the commit the
 *  work stands at and the words the result proved, `older` at an earlier commit or against words
 *  that have changed since, and `unknown` when the check's issue resolves to no repository or
 *  Dispatch's GitHub App cannot read it. */
export type CheckResultCurrency = "current" | "older" | "unknown";

/** When a check runs: before the merge, after it, or against the default branch. The closed
 *  enum `pmdoc/schema/blocks.json` declares under the `when` attribute's `choices`. */
export type CheckWhen = "before_merge" | "after_merge" | "default_branch";

/** `result.recorded`: one result recorded against a check, appended on the **check's** issue and
 *  naming in `issue` the issue it was recorded on. */
export interface CheckResultEventPayload {
  readonly result_id: string;
  readonly issue: string;
  readonly check_issue: string;
  readonly check_artifact_id: string;
  readonly check_block_id: string;
  readonly check_version: number;
  readonly when: CheckWhen;
  readonly verdict: "pass" | "fail";
  readonly role: string;
  readonly commit_sha: string;
}

/** A server-owned typed-block attribute was restored from its indexed row. */
export interface BlockRepairedEventPayload {
  readonly block_id: string;
  readonly version: number;
  readonly disturbed_by: Actor;
}

/** A browser-authored ask block is malformed and awaits repair. */
export interface BlockInvalidEventPayload {
  readonly block_id: string;
  readonly version: number;
  readonly reason: string;
  readonly disturbed_by: Actor;
}

export interface ChildStatusEventPayload {
  readonly child_key: string;
  readonly from: string;
  readonly to: string;
}

export interface ChildEventPayload {
  readonly child_key: string;
}

// SubscriptionRemovedEventPayload is the wire payload of subscription.removed:
// a human unsubscribed session_id from topics on this issue or document, and
// this record is both the audit trail and the direct notice routed to that
// session's own agent topic regardless of the issue's route.
export interface SubscriptionRemovedEventPayload {
  readonly session_id: string;
  readonly by: Actor;
  readonly topics: readonly string[];
  /** True only while Dispatch is retrying the listener-side removal. */
  readonly pending?: boolean;
  /** The remove-requested event this completion settles. */
  readonly request_event_id?: number;
}

// AskFollowerEventPayload is the wire payload of ask.follower_added and
// ask.follower_removed: which session joined or left which ask, and who did it. The
// removed event also reaches the removed session's own topic directly.
export interface AskFollowerEventPayload {
  readonly ask_id: string;
  readonly session_id: string;
  readonly by: Actor;
}

export interface RepoProjectUpdatedEventPayload {
  readonly mapping: RepoProject;
  readonly deleted: boolean;
}

/** `settings.project_default_assignee.updated`: a human set or cleared a project's default
 *  assignee. The payload is the stored row, so a clear carries `assignee: null`. */
export type ProjectDefaultAssigneeUpdatedEventPayload = ProjectDefaultAssignee;

export interface ArchitectureSourceUpdatedEventPayload {
  readonly source: ArchitectureSource;
  readonly deleted: boolean;
}

/** `architecture.synced`: the importer replaced one source's part of the project's model.
 *  `repo` and `branch` name that source; events retained from before projects had several
 *  sources lack them. */
export interface ArchitectureSyncedEventPayload {
  readonly repo?: string;
  readonly branch?: string;
  readonly commit: string;
  readonly components: number;
}

/** `architecture.sync_failed`: one source's import was rejected or failed; that source's
 *  previous components stay up and its row carries the same text in `last_error`. `repo` and
 *  `branch` name that source; events retained from before projects had several sources lack
 *  them. */
export interface ArchitectureSyncFailedEventPayload {
  readonly repo?: string;
  readonly branch?: string;
  readonly error: string;
}

export interface UserStateUpdatedEventPayload {
  readonly login: string;
  /** The viewer's row for one issue as the write left it. Absent from a snooze's event, which
   *  every signed-in feed carries and so names no issue of the writer's. */
  readonly state?: UserIssueState;
  /** Which of `pinned`, `last_read_seq`, `dismissed` and `snoozed` the write moved (`[]` when it
   *  moved none), so another tab refreshes its pinned list for a pin, its whole Inbox for a snooze
   *  or an un-snooze (`PUT`/`DELETE /api/v1/me/asks/{id}/snooze`, whose event carries only `login`
   *  and `["snoozed"]`), and neither for a read mark. Absent from an event written before the
   *  field existed, which a reader takes as "any of pinned, last_read_seq and dismissed may have
   *  moved". */
  readonly changed?: string[];
}

/** `user_agent_state.updated`: one viewer's Clear or read mark on one session's conversation
 *  changed. It carries no state; the viewer's other tabs and devices refetch theirs. */
export interface UserAgentStateUpdatedEventPayload {
  readonly login: string;
  readonly session_id: string;
}

/** `user_thread_state.updated`: one viewer's read of the Inbox's threads moved. It carries no
 *  state; the viewer's other tabs and devices refetch theirs. `thread_id` names the thread when
 *  one thread was marked (`PUT /api/v1/me/threads/{id}/read`); a mark-all
 *  (`POST /api/v1/me/inbox/read`) names no thread, since one event stands for every thread it
 *  marked in that project. */
export interface UserThreadStateUpdatedEventPayload {
  readonly login: string;
  readonly thread_id?: string;
}

/**
 * `issue.claimed` and `issue.released`: who is working the issue now, whose claim this one
 * replaced or cleared, and why. `previous_claim` is present on a takeover (the holder's
 * session was no longer live), a release, and a close; the previous claimant's own agent
 * topic receives the event so it learns it no longer holds the work.
 */
export interface IssueClaimEventPayload {
  readonly key: string;
  readonly status: string;
  /** The issue's `repo` and `resolved_repo`, as on every issue event. */
  readonly repo: string | null;
  readonly resolved_repo: string | null;
  readonly claim: IssueClaim | null;
  readonly previous_claim?: IssueClaim;
  readonly reason: "claimed" | "takeover" | "forced" | "released" | "closed";
}

/**
 * `issue.released` always says whose claim it cleared: the server appends the event only with a
 * claim in hand, whether the holder released it, a human did, or a close did. So readers of a
 * release never have to handle a missing previous claim, and must not pretend to.
 */
export type IssueReleasedEventPayload = IssueClaimEventPayload & {
  readonly previous_claim: IssueClaim;
};

/**
 * `issue.deployed`: the delivery timeline's word that the issue's pull request reached production,
 * written once per pull request the moment the timeline first knows the production run that
 * shipped it (by intake or by the reconcile), with the system actor `delivery-timeline`, and
 * published on the issue's subject (`notifications.dispatch.issue.<KEY>.issue.deployed`) as every
 * issue event is. `repo` and `number` name the pull request, `run_id` the production run, and
 * `completed_at` when that run's production job completed, in UTC. The Legion daemon no longer
 * reads it: nothing a deployment publishes wakes a production check. Strict: the Go writer
 * (`packages/envoy/internal/dispatch/delivery/deployed.go`) emits exactly these four fields.
 */
export const IssueDeployedEventPayloadSchema = z.strictObject({
  repo: z.string(),
  number: z.number().int().positive(),
  run_id: z.number().int().positive(),
  completed_at: z.iso.datetime(),
});

export type IssueDeployedEventPayload = z.infer<typeof IssueDeployedEventPayloadSchema>;

/**
 * `handoff.recorded`: a session recorded version `version` of the issue's `phase` handoff, the
 * record `handoff_id`. It never carries the body; a reader fetches the record by id. Appended in
 * the write's own transaction and published on the issue's subject as every issue event is.
 * Strict: the Go writer (`HandoffRecordedEventPayload` in
 * `packages/envoy/internal/dispatch/model`) emits exactly these three fields.
 */
export const HandoffRecordedPayloadSchema = z.strictObject({
  handoff_id: z.string(),
  phase: z.string(),
  version: z.number().int().positive(),
});

export type HandoffRecordedPayload = z.infer<typeof HandoffRecordedPayloadSchema>;

/**
 * The subject Dispatch announces its server fleet's build on, directly on the bus rather than
 * through the outbox (it is no issue's, document's or project's event), as an Envoy envelope with
 * `source` `dispatch` whose payload is `DispatchServerStartedPayload`. One envelope each time the
 * commit every live Dispatch server process serves changes: a rolling update is announced once its
 * last process of the build before has gone, a restart on the announced build is not announced, and
 * a rollback to an earlier build is announced again. Its event id, source event id and dedupe key
 * are `dispatch-server-started-<commit>-<seq>`, `seq` being the announcement's own number. Mirrored
 * by `startup.Subject` in `packages/envoy/internal/dispatch/startup`. The Legion daemon's intake
 * matches no subject to it: an envelope on it is no fact.
 */
export const DISPATCH_SERVER_STARTED_SUBJECT = "notifications.dispatch.server.started";

/**
 * The payload of a `server.started` envelope: the build commit every live Dispatch server process
 * serves (a fleet any of whose processes carries no stamped commit is never announced) and when
 * the fleet was found serving it, by the database's clock, in UTC. The field is named `started_at`
 * because the Go writer carries that name. The Legion daemon reads none of
 * it: nothing a Dispatch build announces wakes a production check. Strict: the Go writer emits
 * exactly these two fields.
 */
export const DispatchServerStartedPayloadSchema = z.strictObject({
  commit: z.string().min(1),
  started_at: z.iso.datetime(),
});

export type DispatchServerStartedPayload = z.infer<typeof DispatchServerStartedPayloadSchema>;

interface DispatchEventBase {
  readonly id: number;
  readonly issue_key: string | null;
  readonly artifact_id?: string | null;
  readonly project?: string;
  readonly seq: number;
  readonly actor: Actor;
  /** Whether the event wakes the issue's route, claimant and authors (`events.Notify`), so an
   *  agent's own event is not a wake. A reader following an owner topic on the bus skips on the
   *  published copy's `quiet` instead (`PublishedDispatchEvent`). */
  readonly notify: boolean;
  readonly created_at: string;
}

export type DispatchEvent =
  | (DispatchEventBase & { readonly type: "project.created"; readonly payload: Project })
  | (DispatchEventBase & { readonly type: "project.updated"; readonly payload: Project })
  | (DispatchEventBase & {
      readonly type: "settings.repo_project.updated";
      readonly payload: RepoProjectUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "settings.project_default_assignee.updated";
      readonly payload: ProjectDefaultAssigneeUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "settings.architecture_source.updated";
      readonly payload: ArchitectureSourceUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "architecture.synced";
      readonly payload: ArchitectureSyncedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "architecture.sync_failed";
      readonly payload: ArchitectureSyncFailedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "user_state.updated";
      readonly payload: UserStateUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "user_agent_state.updated";
      readonly payload: UserAgentStateUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "user_thread_state.updated";
      readonly payload: UserThreadStateUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "decision.kept";
      readonly payload: DecisionKeptEventPayload;
    })
  | (DispatchEventBase & { readonly type: "issue.created"; readonly payload: IssueEventPayload })
  | (DispatchEventBase & { readonly type: "issue.updated"; readonly payload: IssueEventPayload })
  | (DispatchEventBase & { readonly type: "issue.closed"; readonly payload: IssueEventPayload })
  | (DispatchEventBase & {
      readonly type: "issue.claimed";
      readonly payload: IssueClaimEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "issue.released";
      readonly payload: IssueReleasedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "issue.deployed";
      readonly payload: IssueDeployedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "handoff.recorded";
      readonly payload: HandoffRecordedPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.created";
      readonly payload: ArtifactCreatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.version";
      readonly payload: ArtifactVersionEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.human_edited";
      readonly payload: ArtifactHumanEditedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.approved";
      readonly payload: ArtifactReviewEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.changes_requested";
      readonly payload: ArtifactReviewEventPayload;
    })
  | (DispatchEventBase & { readonly type: "ask.opened"; readonly payload: AskEventPayload })
  | (DispatchEventBase & {
      readonly type: "ask.anchor_refreshed";
      readonly payload: AskEventPayload;
    })
  /** Every rewording: a PATCH edit, a block ask's new text, and an approval request's move to a new
   *  version or new summary. Each one stamps the ask's `edited_at`. */
  | (DispatchEventBase & {
      readonly type: "ask.edited";
      readonly payload: AskEditEventPayload;
    })
  /** An agent handed its approval request back to the human: `approval.requested_version` is the
   *  version it handed back. It rewords nothing and leaves `edited_at` as it was; a hand-back
   *  with a new summary is an `ask.edited` followed by this event. */
  | (DispatchEventBase & {
      readonly type: "ask.handed_back";
      readonly payload: AskEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "ask.answered";
      readonly payload: AskAnsweredEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "ask.resolved";
      readonly payload: AskEventPayload & {
        readonly state: "resolved";
        readonly resolution: AskResolution;
      };
    })
  | (DispatchEventBase & {
      readonly type: "block.repaired";
      readonly payload: BlockRepairedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "block.invalid";
      readonly payload: BlockInvalidEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "result.recorded";
      readonly payload: CheckResultEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.created";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.anchor_refreshed";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.delivery";
      readonly payload: CommentDeliveryEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.answered";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.resolved";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.reopened";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.edited";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "suggestion.accepted";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "suggestion.rejected";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "message.created";
      readonly payload: MessageEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "message.delivery";
      readonly payload: MessageDeliveryEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "message.accepted";
      readonly payload: MessageAcceptedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "message.answered";
      readonly payload: MessageEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "child.status";
      readonly payload: ChildStatusEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "child.added";
      readonly payload: ChildEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "child.removed";
      readonly payload: ChildEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "subscription.remove_requested";
      readonly payload: SubscriptionRemovedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "subscription.removed";
      readonly payload: SubscriptionRemovedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "ask.follower_added";
      readonly payload: AskFollowerEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "ask.follower_removed";
      readonly payload: AskFollowerEventPayload;
    });

export type EventType = DispatchEvent["type"];

/** `GET /auth/whoami`'s answer for a person signed in to the dashboard. */
export interface AuthenticatedUser {
  readonly kind: "user";
  readonly login: string;
  /** The decimal id at or below which every event is final (committed or rolled back for good)
   *  when Dispatch answered. Every read the page makes after this answer holds every event at or
   *  below it, so the event feed resumes from it on its first
   *  subscription and replays exactly what committed since. Always answered: a server that cannot
   *  answer the head answers a 503 rather than a whoami without one (`EVENT_HEAD_UNAVAILABLE` while
   *  no event id is proven final yet, which ends on its own; `EVENT_HEAD_READ_FAILED` when its
   *  database does not answer), and a
   *  server older than the field serves no `/ws/live` either, so no page of this release ever
   *  reads one of its answers. */
  readonly event_head: string;
}

/** GET /api/v1/whoami: a person by lowercase email (`login`), or an agent with its personal
 *  token's owner (a lowercase email) — null under the shared token or a verified service token —
 *  and that service token's subject, null unless one authenticated the request. `session` is the
 *  session the credential itself proved, which only an access token the secrets broker issued
 *  does, and `project` the one project key that credential lets the caller write, null where it
 *  names none. Both are absent from a Dispatch older than broker-issued tokens, which a client
 *  reads as an older build rather than as null.
 *
 *  `scopes` is the OAuth scopes the caller's own credential holds, which decide the routes it may
 *  call: both of them for a person at a browser, the set its row names for a personal token, and
 *  `[]` for the shared token and a verified service token, which act as nobody. The kind stays
 *  `agent` for a personal token holding `person`, because the kind names the credential and not
 *  the act; the person it acts for is `owner`. */
export type WhoamiResponse = WhoamiUser | WhoamiAgent;

interface CreateArtifactOptions {
  readonly name: string;
  readonly summary?: string;
  readonly actor?: Actor;
}

export type CreateArtifactInput = CreateArtifactOptions &
  ({ readonly file: Blob } | { readonly content: string });

/** What `DispatchClient.read` composes from `GET /api/v1/issues/{key}` and that issue's events
 *  page: no route answers it. The events are `DispatchEvent`, whose payload this package types
 *  per event type; the route table's own `Event` leaves `payload` as `unknown`. */
export interface IssueRead {
  readonly issue: IssueDetails;
  readonly events: DispatchEvent[];
}

export interface ArtifactVersionBlob {
  readonly blob: Blob;
  readonly mime: string;
  readonly version: number;
}

export type ArtifactVersionContent = ArtifactVersionText | ArtifactVersionBlob;

/**
 * The code of the `409` Dispatch answers an id written as a prefix two items or more carry: a
 * page address, a `dispatch://` reference or a dashboard address in text, or a route that takes
 * an id in its path (INFRA-869). `candidates` then lists the items it names, at most ten, in id
 * order, for the caller to write more of the id or to choose one. Generated into Go as
 * `contracts.IDAmbiguous`.
 */
export const ID_AMBIGUOUS = "ID_AMBIGUOUS";

/**
 * One item an ambiguous id prefix names (`ID_AMBIGUOUS`). `id` is the whole id the prefix
 * shortens - the item's uuid, or the block id a document block is unique under - and `ref` its
 * whole `dispatch://` reference, empty for an item that has no address (a direct message, a
 * document of an agent's conversation). At most one of `issue_key` and `document` names its
 * owner: the issue that owns it, or the reference of the project document it is written on.
 * `first_line` is the head of what tells two candidates apart, absent for a direct message a
 * caller may not read.
 */
export interface IDCandidate {
  readonly kind: "ask" | "comment" | "message" | "artifact" | "block";
  readonly id: string;
  readonly ref: string;
  readonly issue_key?: string;
  readonly document?: string;
  readonly first_line?: string;
}

export interface DispatchServiceErrorShape {
  readonly error?: string;
  readonly code?: string;
  readonly candidates?: TargetCandidate[] | DuplicateCandidate[] | IDCandidate[];
  readonly current?: EditPreconditionCurrent;
  readonly mismatches?: EditPreconditionMismatch[];
}

/**
 * Tolerantly validates the Dispatch event framing delivered by Envoy. Payload
 * content is parsed per event type so malformed optional display fields do not
 * discard a valid event header.
 */
export const DispatchEventSchema = z.object({
  issue_key: z.string().nullable(),
  artifact_id: z.string().nullish(),
  project: z.string().optional(),
  type: z.string(),
  actor: z.object({ kind: z.string(), id: z.string().optional() }).passthrough(),
  payload: z.object({}).passthrough(),
});

export type InboundDispatchEvent = z.infer<typeof DispatchEventSchema>;

/**
 * One event as Dispatch's outbox publishes it to NATS: the event this API serves, plus `quiet`.
 *
 * `quiet` is whether the event carries nothing for a reader following its owner topic
 * (`events.Quiet` in envoy): a delivery receipt, an anchor refresh, settings or per-viewer
 * bookkeeping, a targeted message its session already had, an ask edit that only moved a version,
 * or a version nobody named. That is what such a reader skips on, where `notify` answers only
 * whether the event wakes the issue's route, claimant and authors - false for every event an agent
 * wrote, so a reader skipping on it drops the activity a session subscribed to the issue asked
 * for. It is optional because a frame published before Dispatch stated it carries none, and such a
 * frame is judged by `notify` instead. The API's own event shape carries no `quiet`: the outbox
 * adds it to the copy it publishes.
 */
export type PublishedDispatchEvent = DispatchEvent & { readonly quiet?: boolean };

export const IssueEventPayloadSchema = z.object({
  title: z.string().optional(),
  labels: z.array(z.string()).optional(),
  status: z.string().optional(),
  priority: z.number().int().min(0).max(3).nullable().optional(),
  rank: z.string().optional(),
  route: z.string().nullish(),
  changed: z.array(z.string()).optional(),
  ancestors: z.array(z.string()).optional(),
  thread_logins: z.array(z.string()).optional(),
  /** On an `issue.created` whose create went past the near-copy refusal with `force`: `past`
   *  names the issues that refusal named, and is the empty list when the check did not answer.
   *  A forced create the refusal would not have stopped carries no `forced` at all. */
  forced: z.object({ past: z.array(z.string()) }).optional(),
});

export const ArtifactCreatedEventPayloadSchema = z.object({
  artifact: z
    .object({ id: z.string().optional(), slug: z.string().optional(), name: z.string().optional() })
    .optional(),
});

export const ArtifactVersionEventPayloadSchema = z.object({
  artifact_id: z.string().optional(),
  name: z.string().optional(),
  version: z.object({ number: z.number().optional(), summary: z.string().nullish() }).optional(),
  diff: z.string().optional(),
});

export const ArtifactHumanEditedEventPayloadSchema = z.object({
  artifact_id: z.string().optional(),
  name: z.string().optional(),
  from_version: z.number().int().optional(),
  to_version: z.number().int().optional(),
  editors: z.array(z.string()).optional(),
  diff: z.string().optional(),
  diff_truncated: z.boolean().optional(),
});

export const ArtifactReviewEventPayloadSchema = z.object({
  artifact_id: z.string().optional(),
  name: z.string().optional(),
  version: z.number().int().optional(),
  actor: z.object({ kind: z.string(), id: z.string() }).passthrough().optional(),
  reason: z.string().nullish(),
  ask_id: z.string().nullish(),
  content_sha256: z.string().optional(),
  content_sha256_source: z.enum(["insert", "backfill"]).optional(),
});

const askEventPayloadFields = {
  id: z.string().optional(),
  opened_event_id: z.number().int().positive(),
  // Inbound only: JetStream retains ask.* envelopes for 72 h, so a renderer may still see a
  // kind the server no longer writes (the removed `action`). It is optional because an envelope
  // retained from before the field existed carries none, and a plain string because model.Ask
  // declares one, which is what the route's own answer carries as the generated `Ask.kind`.
  kind: z.string().optional(),
  question: z.string().optional(),
  // An option and the answer carry the part id of the block each is stored as, and the payload the
  // ask's part revision, as every other read of the ask carries them; a payload the ask has moved
  // past carries no id, and one of an ask whose parts are not indexed yet no revision either.
  options: z
    .array(z.object({ id: z.string().optional(), label: z.string().optional() }))
    .optional(),
  revision: z.number().int().positive().optional(),
  answer: z
    .object({
      id: z.string().optional(),
      selected: z.array(z.string()).nullish(),
      text: z.string().nullish(),
    })
    .nullish(),
  previous_answer: z
    .object({
      user: z.string().optional(),
      selected: z.array(z.string()).nullish(),
      text: z.string().nullish(),
      at: z.string().optional(),
    })
    .nullish(),
  restored: z.boolean().optional(),
  anchor: z
    .object({
      quote: z.string().optional(),
      mark_id: z.string().optional(),
      block_id: z.string().nullable().optional(),
    })
    .passthrough()
    .nullish(),
  anchor_artifact: z
    .object({
      project: z.string().optional(),
      slug: z.string().optional(),
      name: z.string().optional(),
      primary: z.boolean().optional(),
    })
    .optional(),
  resolution: z
    .object({
      kind: z.enum(["retracted", "resolved", "withdrawn"]).optional(),
      reason: z.string().optional(),
      actor: z
        .object({ kind: z.string().optional(), id: z.string().optional() })
        .passthrough()
        .optional(),
      at: z.string().optional(),
    })
    .nullish(),
  thread_logins: z.array(z.string()).optional(),
};

export const AskEventPayloadSchema = z.intersection(
  z.object(askEventPayloadFields),
  z.object({
    previous: z.never().optional(),
    edited_by: z.never().optional(),
  })
);

export const AskEditedEventPayloadSchema = z.object({
  ...askEventPayloadFields,
  multiple: z.boolean(),
  urgency: z.string(),
  edited_at: z.string().nullish(),
  previous: z.object({
    question: z.string(),
    options: z.array(z.object({ label: z.string().optional() })),
    multiple: z.boolean(),
    urgency: z.string(),
  }),
  edited_by: z.object({ kind: z.string(), id: z.string() }).passthrough(),
});

const CommentMentionSchema = z.object({
  target: z.string(),
  delivery: z.enum(DELIVERY_CAPABILITIES),
  session_id: z.string().nullable(),
});

const CommentDeliverySchema = z.object({
  comment_id: z.string(),
  target: z.string(),
  attempt: z.number().int().positive(),
  delivery: z.enum(DELIVERY_CAPABILITIES),
  session_id: z.string().nullable(),
  envelope_id: z.string().nullable(),
  // A stripping object: without this line the flag is dropped from every agent-bound comment
  // frame, since this schema is nested inside the passthrough payload schemas.
  duplicate: z.boolean().optional(),
  state: z.enum(["pending", "sent", "failed"]),
  error: z.string().nullable(),
  resolve_error: z.string().nullable(),
  reply_id: z.string().nullable(),
  created_at: z.string(),
});

/** The block a reply answers, as a comment or message event payload carries it (`ReplyBlock`).
 *  An event recorded before the field, or by a Dispatch older than it, carries none. */
export const ReplyBlockSchema = z.object({
  node: z.string(),
  ref: z.string().optional(),
  block_id: z.string(),
  text: z.string(),
  removed: z.boolean(),
});

export const CommentEventPayloadSchema = z
  .object({
    id: z.string().optional(),
    artifact_name: z.string().optional(),
    project_key: z.string().optional(),
    artifact_slug: z.string().optional(),
    body: z.string().optional(),
    // `CarriedBodyPayload` states what these two mean and who answers the rest.
    body_truncated: z.boolean().optional(),
    payload_omitted: z.boolean().optional(),
    reply_to: z.string().nullish(),
    ask_id: z.string().nullish(),
    ask_question: z.string().optional(),
    ask_state: z.enum(["open", "answered", "resolved"]).optional(),
    ask_waiting_on: z.enum(ASK_TURNS).optional(),
    turn: z.enum(ASK_TURNS).nullish(),
    anchor: z
      .object({
        artifact_id: z.string().optional(),
        block_id: z.string().nullable().optional(),
        mark_id: z.string().optional(),
        orphaned: z.boolean().optional(),
        quote: z.string().optional(),
        version: z.number().int().optional(),
      })
      .nullish(),
    suggestion: z.object({ replace_with: z.string().optional() }).nullish(),
    author: z.object({ kind: z.string(), id: z.string() }).optional(),
    created_at: z.string().optional(),
    mentions: z.array(CommentMentionSchema).optional(),
    deliveries: z.array(CommentDeliverySchema).optional(),
    reply_block: ReplyBlockSchema.nullish(),
    thread_logins: z.array(z.string()).optional(),
  })
  // Bus frames are untrusted and this schema declares only the fields Dispatch
  // clients are known to read today, but the wire payload always carries every
  // field the server model has. .passthrough() keeps a field this schema hasn't
  // caught up to riding along instead of silently vanishing when a consumer that
  // reads it is added later, as ask_waiting_on and turn would vanish from a
  // comment.created reply with no test to catch it.
  .passthrough();

export const MessageEventPayloadSchema = z.object({
  id: z.string().optional(),
  body: z.string().optional(),
  // `CarriedBodyPayload` states what these two mean and who answers the rest.
  body_truncated: z.boolean().optional(),
  payload_omitted: z.boolean().optional(),
  target: z.string().nullish(),
  in_reply_to: z.string().nullish(),
  reply_body: z.string().optional(),
  thread_target: z.string().optional(),
  author: z.object({ kind: z.string(), id: z.string() }).optional(),
  reply_block: ReplyBlockSchema.nullish(),
  thread_logins: z.array(z.string()).optional(),
});

export const DispatchTargetedResourceIDSchema = z.uuid();

export const DispatchTargetedMessagePayloadSchema = MessageEventPayloadSchema.extend({
  id: DispatchTargetedResourceIDSchema,
  issue_key: z.string().nullable(),
  author: z.object({ kind: z.string(), id: z.string() }),
  body: z.string(),
  target: z.string(),
  in_reply_to: z.string().nullable(),
  // The broadcast a frame's message is one recipient's copy of, as the frame claims it: a frame is
  // untrusted, so neither its value nor its absence proves anything.
  broadcast_id: z.string().nullish(),
  deliveries: z.array(z.unknown()),
  created_at: z.string(),
});

export const DispatchTargetedCommentPayloadSchema = CommentEventPayloadSchema.extend({
  id: DispatchTargetedResourceIDSchema,
  issue_key: z.string().nullable(),
  artifact_id: z.string().nullable(),
  author: z.object({ kind: z.string(), id: z.string() }),
  body: z.string(),
  reply_to: z.string().nullable(),
  ask_id: z.string().nullable(),
  mentions: z.array(CommentMentionSchema),
  deliveries: z.array(CommentDeliverySchema),
  created_at: z.string(),
  artifact_name: z.string(),
});

export const DispatchTargetedMessageDeliverySchema = z.object({
  attempt: z.number().int().positive(),
  mode: z.enum(DELIVERY_CAPABILITIES),
});

export const DispatchTargetedCommentDeliverySchema = DispatchTargetedMessageDeliverySchema.extend({
  comment_id: DispatchTargetedResourceIDSchema,
  target: z.string(),
});

export const DispatchTargetedDeliverySchema = z.union([
  DispatchTargetedCommentDeliverySchema,
  DispatchTargetedMessageDeliverySchema,
]);

export const MessageDeliveryEventPayloadSchema = z.object({
  message_id: z.string().optional(),
  attempt: z.number().int().positive().optional(),
  delivery: z.enum(DELIVERY_CAPABILITIES).optional(),
  session_id: z.string().optional(),
  target: z.string().optional(),
  title: z.string().optional(),
  state: z.enum(["sent", "failed"]).optional(),
  // A stripping object: without this line the flag is dropped from every agent-bound
  // message.delivery frame.
  duplicate: z.boolean().optional(),
  error: z.string().optional(),
});

export const ChildStatusEventPayloadSchema = z.object({
  child_key: z.string().optional(),
  from: z.string().optional(),
  to: z.string().optional(),
});

export const SubscriptionRemovedEventPayloadSchema = z.object({
  session_id: z.string().optional(),
  by: z.object({ kind: z.string(), id: z.string().optional() }).passthrough().optional(),
  topics: z.array(z.string()).optional(),
  pending: z.boolean().optional(),
  request_event_id: z.number().int().positive().optional(),
});

export const AskFollowerEventPayloadSchema = z.object({
  ask_id: z.string().optional(),
  session_id: z.string().optional(),
  by: z.object({ kind: z.string(), id: z.string().optional() }).passthrough().optional(),
});

/** The timeline's facets, as the query parameters that select them and the `facet_counts` keys
 *  that count them. */
export type DeliveryFacet =
  | "repo"
  | "parent_agent"
  | "session"
  | "issue"
  | "priority"
  | "component"
  | "author"
  | "rework"
  | "deployed";

/** The facets the page colours merges by, as the `color_counts` keys that count them. */
export type DeliveryColorFacet = "repo" | "author" | "priority" | "component" | "parent_agent";

/** A window by preset, whose end stays at the present, or by the exact `from`/`to` of a custom range. */
export type DeliveryLiveWindow =
  | { readonly preset: 7 | 14 | 28 | 84 }
  | { readonly from: string; readonly to: string };

/** One view a page shows. `query` is the query string the page would send to the matching GET route
 *  without `from`/`to` (search and every facet), so the live answer and the GET answer are one. */
export type DeliveryLiveView =
  | { readonly kind: "timeline"; readonly window: DeliveryLiveWindow; readonly query: string }
  | { readonly kind: "measures"; readonly window: DeliveryLiveWindow; readonly query: string }
  | { readonly kind: "pipeline"; readonly at?: string };

/** The timeline's lists sent item by item, by id. */
export type DeliveryLiveList = "prs" | "runs" | "session_groups";

/** The timeline's maps sent entry by entry, by key. */
export type DeliveryLiveMap =
  | "issue_titles"
  | "session_titles"
  | "session_pr_counts"
  | "components";

/** The timeline's two-level count maps, sent count by count: `{facet: {value: count}}`. */
export type DeliveryLiveCounts = "facet_counts" | "color_counts";

/** What a page holds, as revisions (16 hex digits each) in the frame's own sections. */
export interface DeliveryLiveRevs {
  readonly items: Readonly<Partial<Record<DeliveryLiveList, Readonly<Record<string, string>>>>>;
  readonly maps: Readonly<Partial<Record<DeliveryLiveMap, Readonly<Record<string, string>>>>>;
  readonly counts: Readonly<
    Partial<Record<DeliveryLiveCounts, Readonly<Record<string, Readonly<Record<string, string>>>>>>
  >;
  /** Every field sent whole, by its name. */
  readonly fields: Readonly<Record<string, string>>;
}

/** A delta's revision changes, in DeliveryLiveRevs' shape; null for a key that left. */
export interface DeliveryLiveRevChanges {
  readonly items?: Readonly<
    Partial<Record<DeliveryLiveList, Readonly<Record<string, string | null>>>>
  >;
  readonly maps?: Readonly<
    Partial<Record<DeliveryLiveMap, Readonly<Record<string, string | null>>>>
  >;
  readonly counts?: Readonly<
    Partial<
      Record<DeliveryLiveCounts, Readonly<Record<string, Readonly<Record<string, string | null>>>>>
    >
  >;
  readonly fields?: Readonly<Record<string, string>>;
}

/** A delivery view's resume: the page's last applied change and what it holds; Dispatch answers
 *  the delta. */
export interface DeliveryLiveResume {
  readonly seq: string;
  readonly revs: DeliveryLiveRevs;
}

/** The Dispatch event feed, event by event in id order. */
export interface EventsLiveView {
  readonly kind: "events";
}

/** One session's agent stream: whether it is responding, its replay, and each frame it publishes. */
export interface AgentLiveView {
  readonly kind: "agent";
  /** The session's id, a UUID. */
  readonly session_id: string;
}

/** One view a live socket subscription shows. */
export type LiveView = DeliveryLiveView | EventsLiveView | AgentLiveView;

/** An events view's resume: the decimal id of the last event the page applied. */
export interface EventsLiveResume {
  readonly since: string;
}

/** A subscribe frame for a view of type `V`, whose resume is an `R`. */
interface LiveSubscribeTo<V extends LiveView, R> {
  readonly type: "subscribe";
  /** The page's own id for this subscription, echoed on every frame for it. */
  readonly sub: string;
  readonly view: V;
  /** A reconnecting page's resume point, of its view's kind. */
  readonly resume?: R;
}

/** Starts the subscription `sub` names. Its resume is its view's kind's: a `DeliveryLiveResume`
 *  for a delivery view, an `EventsLiveResume` for the events view, and none for an agent view,
 *  which starts from the session's replay. Dispatch refuses any other pairing with an
 *  `INVALID_FRAME` error frame, so the type admits none. */
export type LiveSubscribe =
  | LiveSubscribeTo<DeliveryLiveView, DeliveryLiveResume>
  | LiveSubscribeTo<EventsLiveView, EventsLiveResume>
  | LiveSubscribeTo<AgentLiveView, never>;

export interface LiveUnsubscribe {
  readonly type: "unsubscribe";
  readonly sub: string;
}

export type LiveClientFrame = LiveSubscribe | LiveUnsubscribe;

export type DeliveryLiveBody =
  | DeliveryTimelineWithMeasures
  | DeliveryMeasuresResponse
  | DeliveryPipelineResponse;

export interface DeliveryLiveSnapshot {
  readonly type: "snapshot";
  readonly sub: string;
  /** The change log position this answer reflects; a decimal string, since it is a bigint. */
  readonly seq: string;
  /** When the newest change it reflects was recorded; null when the log is empty. */
  readonly changed_at: string | null;
  /** Exactly the matching GET route's body for the resolved window. */
  readonly body: DeliveryLiveBody;
  readonly revs: DeliveryLiveRevs;
}

/** One list's changes: its items that are new or changed, whole, and the ids of those that left. */
export interface DeliveryLiveItemChanges<L extends DeliveryLiveList> {
  readonly upsert: NonNullable<DeliveryTimelineWithMeasures[L]>;
  readonly remove: readonly string[];
}

export interface DeliveryLiveDelta {
  readonly type: "delta";
  readonly sub: string;
  readonly seq: string;
  readonly changed_at: string | null;
  /** Fields replaced whole. `window` is here whenever it moved, and never alone. */
  readonly set: Readonly<Record<string, unknown>>;
  readonly items: { readonly [L in DeliveryLiveList]?: DeliveryLiveItemChanges<L> };
  /** A key's new value, or null when it left. */
  readonly maps: {
    readonly [M in DeliveryLiveMap]?: Readonly<
      Record<string, NonNullable<DeliveryTimelineWithMeasures[M]>[string] | null>
    >;
  };
  /** A count's new value, or null when it left. */
  readonly counts: Readonly<
    Partial<
      Record<DeliveryLiveCounts, Readonly<Record<string, Readonly<Record<string, number | null>>>>>
    >
  >;
  readonly revs: DeliveryLiveRevChanges;
}

/** Sent to every subscriber when the freshness row's verdict changes, and to a subscription right
 *  after its first answer, so a page that was not subscribed at a change still learns it; never
 *  otherwise. */
export interface DeliveryLiveFreshness {
  readonly type: "freshness";
  readonly freshness: DeliveryTimelineWithMeasures["freshness"];
  /** The sources past their threshold: "prs", "runs", "ci", "events". */
  readonly stale: readonly string[];
}

/** Sent once at the start of every events subscription, before any `event`: the id it starts
 *  after, which is the page's `since` clamped to the feed's head, or the head when the page sent
 *  none. A page holding no cursor adopts it, so a page that has applied no event still resumes
 *  without a gap. */
export interface LiveEventsStart {
  readonly type: "events_start";
  readonly sub: string;
  /** A decimal event id. */
  readonly since: string;
  /** True when the page resumed from an id at or below the feed's head: the subscription replays
   *  every event after it before it goes live, so the page missed nothing and owes no refresh for
   *  the gap. False with no resume, and for a resume above the head (an id of a log since
   *  replaced), which start at the head and replay nothing the page missed. */
  readonly replay_complete: boolean;
}

/** One event of the feed. */
export interface LiveEventFrame {
  readonly type: "event";
  readonly sub: string;
  /** The event's decimal id, which an `EventsLiveResume` names as `since`. */
  readonly id: string;
  readonly event: DispatchEvent;
}

interface LiveAgentEvent<E extends string, D> {
  readonly type: "agent";
  readonly sub: string;
  readonly event: E;
  readonly data: D;
}

/** One message of a session's agent stream; `event` names what `data` is. The responder verdict
 *  is Dispatch's own. A replay's or a frame's data is the session's own JSON, which Dispatch
 *  relays and never parses, so it is `unknown` here: meant to be an `AgentStreamReplay` or an
 *  `AgentStreamFrame`, it is whatever the session published, and a page checks it before it
 *  renders it. */
export type LiveAgentFrame =
  | LiveAgentEvent<"responder", AgentStreamResponder>
  | LiveAgentEvent<"replay", unknown>
  | LiveAgentEvent<"frame", unknown>;

/** Dispatch ended this one subscription: it fell behind (`behind`), or the relay dropped the
 *  viewer (`dropped`). The page subscribes it again at once. */
export interface LiveEnded {
  readonly type: "ended";
  readonly sub: string;
  readonly reason: "behind" | "dropped";
}

/** A refusal: of the subscription `sub` names, or, without `sub`, of a frame Dispatch could not
 *  read. */
export interface LiveError {
  readonly type: "error";
  readonly sub?: string;
  readonly code: string;
  readonly message: string;
}

/** Sent every heartbeat interval beside the socket's own ping, carrying nothing: a browser never
 *  shows a ping to the page, so this is how the page tells a quiet socket from a dead one. */
export interface LiveHeartbeat {
  readonly type: "heartbeat";
}

export type LiveServerFrame =
  | DeliveryLiveSnapshot
  | DeliveryLiveDelta
  | DeliveryLiveFreshness
  | LiveEventsStart
  | LiveEventFrame
  | LiveAgentFrame
  | LiveEnded
  | LiveError
  | LiveHeartbeat;
