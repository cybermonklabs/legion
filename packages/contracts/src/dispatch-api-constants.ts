/**
 * The constants and plain functions of Dispatch's HTTP and WebSocket contract: the header names,
 * the idempotency codes, the limits, the search operators and the small rules over them.
 *
 * Why they are a module of their own rather than beside the schemas of their area. The area
 * modules `dispatch-api.ts` re-exports build about two hundred zod schemas at their top level,
 * and a bundler may drop none of them: a value is used, so its module is kept, and the schemas
 * are kept with it. A reader who only needs a header's name was therefore given zod as well -
 * 79,622 bytes of it in the dashboard's entry chunk, which every page parses before it draws
 * anything, for a page that validates nothing (dispatch://INFRA-1109, R13). A constant a reader
 * needs must not drag a schema builder into a browser, so the constants live here, an area
 * module that reads one imports it from here, and `dispatch-api.ts` re-exports every one of
 * them: an import of that module, or of the package index, answers exactly what it answered
 * before.
 */

// Types only: a type import is erased by the compiler, so it builds no edge a bundler would
// follow back to the schemas.
import type { MyReplyRow } from "./dispatch-api.generated";
import type { MyReplyAgentRow, MyReplyThreadRow } from "./dispatch-api-inbox";

/** The OAuth scopes a Dispatch credential can hold, mirroring Go's `model.ScopePerson` and
 *  `model.ScopeApprove` and the `agent_tokens.scopes` check that stores them: `person` acts as
 *  the credential's own person on every route a person reaches through the dashboard, and
 *  `approve` settles what a person settles — a document review, the answer of an approval ask, a
 *  machine-login decision or revoke, and minting, changing or revoking a personal token. A
 *  credential holding `approve` holds `person` too. */
export const DISPATCH_SCOPES = ["person", "approve"] as const;

/**
 * How long an issue claim is held for a holder the Envoy registry calls `unresponsive`: a
 * session past the listener's live window but inside its gone horizon has missed heartbeats, not
 * ended, and under box load that is ordinary. Dispatch refuses another agent's claim or release
 * while the holder was last seen no longer ago than this, so a stalled session keeps the issue
 * it is working and two sessions never write the same branch; a person still forces the claim
 * (`force`), which is the escape this grace leaves open.
 *
 * Twenty minutes is four times the listener's live window (`ENVOY_SESSION_LIVE_WINDOW`, 5m) and
 * well inside its gone horizon (`ENVOY_SESSION_GONE_HORIZON`, 1h), so a session that is merely
 * slow keeps its work while a box that really lost one frees it long before the horizon.
 * `scripts/gen-go.ts` emits it as `contracts.IssueClaimUnresponsiveGrace`, so the refusal the
 * server writes and the wait a client predicts are one number.
 */
export const ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS = 20 * 60 * 1000;

/**
 * The longest issue title, in UTF-16 units, counted after the trim `POST /api/v1/issues` and
 * `PATCH /api/v1/issues/{key}` apply; a longer one is `400 CAP_EXCEEDED`. Production's longest
 * title was 326 characters when this was set. It bounds the duplicate-title check, which compares
 * the new title with every title's lexemes in the project on each creation, and the
 * `POSSIBLE_DUPLICATE` message, which quotes a candidate's whole title. Generated into Go as
 * `contracts.IssueTitleMax`, which the server enforces, and the dashboard's title fields take it as
 * their `maxLength`, so the two agree.
 */
export const ISSUE_TITLE_MAX = 1000;

/** Maximum entries in an issue's blocked_by input, before duplicate keys are removed. */
export const MAX_ISSUE_BLOCKERS = 20;

/**
 * The most issues one page of `GET /api/v1/issues` holds: `limit` is 1 to this. Generated into Go
 * as `contracts.MaxIssuePageLimit`, which the server enforces, and the `dispatch_issues` tool's
 * `limit` takes the same bound, so the two ends agree.
 */
export const MAX_ISSUE_PAGE_LIMIT = 250;

/**
 * The page size of `GET /api/v1/issues` when a caller pages with `offset` alone, and of
 * `dispatch_issues` when it names no `limit`. Generated into Go as `contracts.DefaultIssuePageLimit`.
 */
export const DEFAULT_ISSUE_PAGE_LIMIT = 50;

/**
 * How many of an issue's events `dispatch_read` lists, the newest ones, when it names neither
 * `events` nor `all_events`: what `DispatchClient.read` returns by default.
 */
export const DEFAULT_ISSUE_READ_EVENTS = 10;

/**
 * The longest project name, in UTF-16 units, counted after the trim `POST /api/v1/projects`
 * applies; a longer one is `400 CAP_EXCEEDED`. A name shows beside its key in the sidebar's
 * 20rem rail and in every project picker's `KEY · Name` option, any bearer may write one, and no
 * route renames a project, so it is held to about two lines of that rail; production's longest name
 * was 25 characters when this was set. Generated into Go as `contracts.ProjectNameMax`, which the
 * server enforces, and the Settings name field and `dispatch project` take it as their maximum.
 */
export const PROJECT_NAME_MAX = 80;

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

/** Who holds the turn on an open ask after a reply: `human` when the human needs to act,
 *  `agent` when the asking agent still owes the next move (a progress note). */
export const ASK_TURNS = ["human", "agent"] as const;

/** `row` as the shape its own `kind` names. The route answers both kinds under one schema - the
 *  fields of each are optional on `MyReplyRow`, and `kind` is the open string the Go field
 *  carries - so this is the one place the two are told apart, and a surface that reads a field
 *  only one kind has narrows here rather than widening itself. A `kind` this build does not know
 *  reads as a Reply row, which is what every surface already draws for one. */
export function inboxThreadRow(row: MyReplyRow): MyReplyThreadRow | MyReplyAgentRow {
  return row.kind === "agent" ? (row as MyReplyAgentRow) : (row as MyReplyThreadRow);
}

/** How many rows one `state=dismissed` page answers, and how many `?cursor=` reads again. */
export const INBOX_DISMISSED_PAGE = 200;

/** The two kinds of thread row the Inbox holds, as `GET /api/v1/me/replies`'s `?kind=` and the
 *  Mark all read body both name them: a Reply row, a thread the reader wrote in where
 *  somebody else has written since, and an Agent row, a one-off message they sent a session
 *  that it answered. */
export const INBOX_THREAD_KINDS = ["reply", "agent"] as const;

/** A row id `POST /api/v1/me/inbox/dismiss` names a thread by, as the Inbox draws its rows: a Reply
 *  row is `reply:<thread id>` and an Agent row `agent:<root message id>`, each id a uuid. An Ask
 *  row, the third kind the Inbox draws, has no Dismiss at all, and naming `ask:<id>` is
 *  400 INVALID_QUERY; the Reply row of an open ask's thread is an ordinary `reply:` id. A
 *  dismissal's `seen_replies` keys its entries by these same ids. */
export const INBOX_DISMISS_ROW_PREFIXES = { reply: "reply:", agent: "agent:" } as const;

/**
 * The closed list of targeted-delivery modes a session can advertise as
 * capabilities on the Envoy listener (`/v1/interests/subscribe`) and that
 * Dispatch can request for a message. Every other spelling of this set —
 * `MessageDeliveryMode`, the delivery event payload schema, the envoy-client
 * targeted-frame schema — derives from this constant.
 */
export const DELIVERY_CAPABILITIES = ["aside", "btw", "steer"] as const;

/**
 * The most sessions one broadcast sends to (`POST /api/v1/broadcasts`'s `session_ids`, after the
 * duplicates are dropped). A runaway guard, not a product limit: selection is a human ticking
 * boxes, but select-all makes a large send one click. Generated into Go as
 * `contracts.MaxBroadcastRecipients`, which the server enforces, so the dashboard's refusal and
 * the server's are one number.
 */
export const MAX_BROADCAST_RECIPIENTS = 100;

/**
 * The request header that names one write's intent: the same on every retry of that write, and
 * different for every new press. Dispatch accepts it, optionally, on every `POST` and `PATCH`
 * route (`PUT` and `DELETE` are idempotent already): a repeat with the same key and the same
 * request, once the first has finished, is answered with the first's stored status and body and
 * `IDEMPOTENCY_REPLAYED_HEADER`, and runs nothing again; the same key with a different request is
 * `422 IDEMPOTENCY_KEY_REUSED`; the same key while the first is still running (within
 * `IDEMPOTENCY_LEASE_MS`) is `409 IDEMPOTENCY_KEY_IN_FLIGHT` with `Retry-After: 1`, which a client
 * asks again after. A 5xx, 401, 408 or 429 is not stored, so the next attempt runs the request
 * again - unless the first attempt's write committed before its answer was lost, which a repeat
 * is answered `409 IDEMPOTENCY_KEY_COMMITTED` for. Keys belong to the signed-in login that sent
 * them and are kept `IDEMPOTENCY_KEY_TTL_MS`.
 * Five routes answer a repeat from the record they made rather than the stored body: `POST
 * /api/v1/agents/{session_id}/messages`, `POST /api/v1/broadcasts`, `POST
 * /api/v1/issues/{key}/messages`, `POST /api/v1/issues/{key}/comments` and `POST
 * /api/v1/artifacts/{id}/comments` resume any delivery still owed and answer `201` with
 * `IDEMPOTENCY_REPLAYED_HEADER` and the resource read afresh in the first answer's shape (a
 * broadcast with the first answer's `excluded`), so a recipient whose attempt a live sender holds
 * can still read `pending`, and a repeat after a lost answer is that replay, never
 * `409 IDEMPOTENCY_KEY_COMMITTED`. A write without the header is run as it always was, with no
 * replay (the IETF `Idempotency-Key` draft; Google AIP-155).
 */
export const IDEMPOTENCY_KEY_HEADER = "Idempotency-Key";

/** The response header (`?1`, a structured-field boolean) on an answer Dispatch replayed from
 *  the first request under the key rather than ran again. */
export const IDEMPOTENCY_REPLAYED_HEADER = "Idempotency-Replayed";

/** The code of the `409` (with `Retry-After: 1`) for a key whose first request is still running;
 *  nothing was run, and the client asks again under the same key. */
export const IDEMPOTENCY_KEY_IN_FLIGHT = "KEY_IN_FLIGHT";

/** The code of the `409` for a key whose write landed before its answer was lost: Dispatch will
 *  not run it again and has no answer to replay. The write is done; a client treats it as landed
 *  and reads again what it wrote (the event feed carries the change too). */
export const IDEMPOTENCY_KEY_COMMITTED = "KEY_COMMITTED";

/** The code a replay's body carries when the first answer was larger than Dispatch keeps (1 MiB):
 *  the replay has the first answer's status and this body instead of the first's. A 2xx with it
 *  means the write ran; the client reads the resource again rather than this body. */
export const IDEMPOTENCY_ANSWER_NOT_KEPT = "ANSWER_NOT_KEPT";

/**
 * The request header a page puts on every write it makes (`POST`, `PATCH`, `PUT`, `DELETE`): the
 * login of the person who pressed it. Dispatch answers `409 DISPATCH_LOGIN_CHANGED` before
 * anything runs, its key untouched, when the request's caller is another person or no person at
 * all, so a write one person pressed never runs under another's session - another tab signs in
 * in place, and this tab's retry goes out on the new cookie before it learns of it. A caller that
 * resolves to nobody (a lapsed cookie) is answered by the route's own `401`. A write without the
 * header (the `dispatch` command, an agent) is not checked.
 */
export const DISPATCH_LOGIN_HEADER = "Dispatch-Login";

/** The code of the `409` for a write whose `DISPATCH_LOGIN_HEADER` names a login that is not its
 *  caller's: nothing ran. The page drops every write that login pressed. */
export const DISPATCH_LOGIN_CHANGED = "LOGIN_CHANGED";

/**
 * Whether an attempt Dispatch answered with `status` (and `code`, its error body's) ended without
 * Dispatch's verdict on the key, so a client asks again under the same key: a 5xx, a 408 or a 429
 * (none of which Dispatch stores, `IDEMPOTENCY_KEY_HEADER`), or `409 IDEMPOTENCY_KEY_IN_FLIGHT`
 * (the first request under the key is still running). Any other answer is Dispatch's verdict, and
 * the same key would be answered with it again. The page and the `dispatch` command both retry by
 * this one rule.
 */
export function retriesUnderKey(status: number, code: string | undefined): boolean {
  return (
    status >= 500 ||
    status === 408 ||
    status === 429 ||
    (status === 409 && code === IDEMPOTENCY_KEY_IN_FLIGHT)
  );
}

/**
 * Whether an answer of `status`, `replayed` (it carried `IDEMPOTENCY_REPLAYED_HEADER`) and parsed
 * `body` is a replay whose first answer Dispatch did not keep: a replayed 2xx whose body is
 * `{ code: IDEMPOTENCY_ANSWER_NOT_KEPT }`. The write ran; the client reads the resource again
 * rather than taking this body for it. An answer that is not a replay is the resource itself,
 * whatever it holds.
 */
export function answerNotKept(status: number, replayed: boolean, body: unknown): boolean {
  return (
    replayed &&
    status >= 200 &&
    status < 300 &&
    typeof body === "object" &&
    body !== null &&
    Reflect.get(body, "code") === IDEMPOTENCY_ANSWER_NOT_KEPT
  );
}

/** How long Dispatch keeps a key's answer: 24 hours. */
export const IDEMPOTENCY_KEY_TTL_MS = 24 * 60 * 60 * 1000;

/**
 * How long a repeated delivery is recognised, in milliseconds: the window behind the dashboard's
 * promise that a same-mode Retry "won't deliver it twice". This comment is where that promise is
 * stated; every other place that relies on it points here.
 *
 * Dispatch keys a targeted message's send by message and mode (`<message>:<mode>`) and a comment
 * mention's by comment, target and the mode the comment recorded on that mention
 * (`<comment>:<target>:<the mention's mode>`, whatever mode each attempt went in), the same for
 * every attempt, and the listener makes that key, scoped to the recipient
 * (`agent.<session>.<key>`), the envelope's `dedupe_key`. A Retry of a send that already landed -
 * in the same mode, or in any mode at all for a mention - therefore repeats that frame's dedupe
 * key, and only that: the listener mints a new
 * `event_id` for every send. Two things recognise the repeat inside this window, both only for a
 * key that names its event (`dedupeKeyNamesItsEvent` in `envelope.ts`, which every Dispatch key
 * does), and a recipient is protected by whichever of them sits between it and the bus:
 *
 * - The notification stream stores such a frame under a MsgId of its dedupe key and topic, so it
 *   keeps one copy and answers the repeat as a duplicate, which the attempt records as
 *   `duplicate`. Every session is reached from that stored copy — by the relay of its own
 *   consumer, or by the listener's push to its serve port — so the stream's check sits between
 *   the bus and each of them.
 * - Each host hands its agent at most one frame per such key inside this window, through
 *   `createDeliveryDedupe` in `@legion/envoy-client/delivery`, which catches a repeat the
 *   stream's own window no longer holds. A frame the host answered with an error instead (a BTW
 *   whose side turn failed) was never handed over, so the host releases its key and a same-mode
 *   re-send of it reaches the agent.
 *
 * It fails in three places. A host's `createDeliveryDedupe` holds its keys in memory, so a host
 * that keeps nothing else and restarts after the first frame landed has forgotten it, and a Retry
 * reaches that agent a second time; that is ordinary, not rare: `claude --resume` starts a new
 * channel process on the same conversation, and a Sandbox pod resumes its session in a new
 * process. pi-envoy and claude-envoy, from the releases that carry
 * `@legion/envoy-client/delivery-ledger`, also keep a ledger of the frames their session was handed (pi-envoy in the session's
 * transcript, claude-envoy in a file per session id) and drop a repeat it holds after a restart,
 * for 72 hours and up to 20,000 frames; only a crash between the hand-over and the ledger write
 * still lets that Retry through. Older plugins still forget, and the dashboard cannot tell which
 * one a session runs. The stream stores a re-send of an attempt the session answered with an
 * error no more than any other repeat, so a session the listener pushes to never receives it, and
 * its attempt reads as a duplicate. The dashboard therefore offers no same-mode Retry for such an
 * attempt and points at a mode change, which is a new key for a targeted message - a comment
 * mention's mode change repeats the mention's one key, so it delivers nothing once an attempt of
 * that mention reached the session; sending that Retry under a new key is
 * LEGION-431. And a host keeps at most `DELIVERY_DEDUPE_KEY_LIMIT` keys, forgetting the oldest
 * past it, so a re-send of a key it was handed before that many later ones reaches the agent
 * again. The limit is two and a half times what the whole stream stored in
 * one window when it was set, so a host reaches it only once traffic grows past that or a
 * producer floods a topic it follows.
 *
 * Past the window neither remembers the key, and a same-mode retry is a second delivery, so the
 * dashboard makes the promise only inside it, and names the restart
 * (`packages/dispatch/web/src/features/conversation/delivery.ts`). `scripts/gen-go.ts` emits this
 * as `contracts.DeliveryDuplicateWindow`, which `bus/stream.go` uses for both the stream's
 * duplicate window and its retention.
 *
 * It also bounds webhook redelivery dedupe: a GitHub, Slack or Ghost Wispr envelope is stored
 * under a MsgId of its delivery id, and GitHub redelivers deliveries up to three days old, so a
 * window shorter than that lets a GitHub redelivery be stored, and handed to a host, twice.
 *
 * This window bounds a *delivery* retry, not a repeated *write*: a write's `Idempotency-Key`
 * (`IDEMPOTENCY_KEY_HEADER`) is recognised for `IDEMPOTENCY_KEY_TTL_MS`, however late within
 * that the repeat arrives.
 */
export const DELIVERY_DUPLICATE_WINDOW_MS = 72 * 60 * 60 * 1000;

/**
 * The cause a delivery attempt records when the listener never answered its send. The listener
 * publishes before it answers, so this is the one failure that may already have reached the
 * recipient — which is what makes re-sending in another mode a genuine second delivery, and what
 * the dashboard keys that wording on. Generated into Go as `contracts.ReceiptTimeoutCause`, so
 * the string the server writes and the string the dashboard recognises are one literal.
 *
 * It is the CAUSE only: the advice that follows from it expires with
 * `DELIVERY_DUPLICATE_WINDOW_MS`, so it is composed at render time and never stored.
 */
export const RECEIPT_TIMEOUT_CAUSE =
  "The listener didn't answer within the send window; the message may already have been delivered.";

/** The kinds a search result and a write suggestion are one of. The route table carries `kind`
 *  as a plain string, and this is the closed list it means. */
export const SEARCH_RESULT_KINDS = ["issue", "document", "comment", "ask", "message"] as const;

/** `SearchResponse.degraded`'s one value today: meaning search could not run for this request (no
 * embedder configured, the query's own embedding timed out, or the model service answered an
 * error) and search fell back to keyword-only ranking. Generated into Go as
 * `contracts.SearchDegradedEmbedderUnavailable` so the string Dispatch writes and the string
 * `search-answer.ts` compares against cannot drift apart. */
export const SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE = "embedder_unavailable";

/** The operator forms `GET /api/v1/search` reads in the words a person wrote: a `"phrase"` in
 *  quotes, the `OR` between two words, and a `-word` the search leaves out. Each is generated
 *  into Go as `contracts.SearchOperatorPhrase`, `SearchOperatorOr` and `SearchOperatorExclude`,
 *  so the string Dispatch answers and the string a client reads it by cannot drift apart. */
export const SEARCH_OPERATOR_PHRASE = "phrase";

export const SEARCH_OPERATOR_OR = "or";

export const SEARCH_OPERATOR_EXCLUDE = "exclude";

export const SEARCH_OPERATORS = [
  SEARCH_OPERATOR_PHRASE,
  SEARCH_OPERATOR_OR,
  SEARCH_OPERATOR_EXCLUDE,
] as const;

/** The members `GET /api/v1/search` carried before `GET /api/v1/search/results` was added.
 *  `session` is among them because `openapi.go` declares it on every operation of every build
 *  and `actingPerson` reads it on every route, so a request carrying it is one an older build
 *  serves correctly and must not be sent to a path that build refuses. */
export const SEARCH_MEMBERS_EVERY_BUILD_READS = [
  "q",
  "project",
  "limit",
  "offset",
  "session",
] as const;

/** Whether every member a search request carries is one of `SEARCH_MEMBERS_EVERY_BUILD_READS`,
 *  which is the one rule that picks a search's path: a request carrying anything beyond them
 *  goes to `GET /api/v1/search/results`. A Dispatch released before that route answers the
 *  new path `404 no route`, which is a refusal, where it answers `GET /api/v1/search` `200` with
 *  the member dropped, which is unnarrowed rows under the filters a person chose. A key whose
 *  value is `undefined` is not carried, which is how both clients build a request out of
 *  optional filters, and the keys are read from the request itself, so a member added later
 *  routes itself. */
export function everyBuildReadsSearch(request: object): boolean {
  const read: readonly string[] = SEARCH_MEMBERS_EVERY_BUILD_READS;
  return Object.entries(request).every(
    ([member, value]) => value === undefined || read.includes(member)
  );
}

/** The code of the `401` Dispatch itself answers a caller it cannot resolve to a person - no
 *  session cookie, one it cannot read, one whose generation it has revoked, or a membership the
 *  sign-in issuer no longer confirms (`identity.WriteError`). A `401` without it came from
 *  something in front of Dispatch, an authenticating proxy among them, and says nothing about
 *  the session: a sign-out answered that way did not go through. */
export const AUTH_NO_IDENTITY = "NO_IDENTITY";

/**
 * The close code the document websocket (`/ws/doc/{room}`) refuses a room with when the tree it
 * holds is outside the Proof schema: it completes the upgrade and closes with this code, and
 * `DOCUMENT_SCHEMA_CLOSE_REASON`, before any sync, so a browser editor never receives a tree it
 * would normalize and write back. It is in the private 4000-4999 range beside Hocuspocus's own 4401
 * and 4403, and the dashboard reads it as the document's repair state, never as a dropped
 * connection to retry. Generated into Go as `contracts.DocumentSchemaCloseCode`.
 */
export const DOCUMENT_SCHEMA_CLOSE_CODE = 4409;

/**
 * The code of the `409` Dispatch answers an id written as a prefix two items or more carry: a
 * page address, a `dispatch://` reference or a dashboard address in text, or a route that takes
 * an id in its path (INFRA-869). `candidates` then lists the items it names, at most ten, in id
 * order, for the caller to write more of the id or to choose one. Generated into Go as
 * `contracts.IDAmbiguous`.
 */
export const ID_AMBIGUOUS = "ID_AMBIGUOUS";

/** The close code Dispatch ends a live socket with once the socket reaches its age, a random point
 *  in [LIVE_SOCKET_MIN_AGE_MS, LIVE_SOCKET_MAX_AGE_MS). It is in the private 4000-4999 range, apart
 *  from the 4000 a page closes a silent socket with: the page reopens at once and resumes every
 *  subscription, showing no pill and refreshing nothing. */
export const LIVE_SOCKET_MAX_AGE_CLOSE = 4001;

/** How a verified service token's subject reads wherever an actor is shown.
 *
 *  A Kubernetes subject — exactly `system:serviceaccount:<namespace>:<name>` —
 *  renders `<namespace>/<name>`, because the namespace is the half that
 *  disambiguates: every namespace has a `default` service account, so the name
 *  alone names no one once a second namespace authenticates. Every other
 *  subject renders whole. Truncating a subject whose shape is unknown is the
 *  mistake this rule exists to undo, so the fallback shows all of it.
 *
 *  The persisted actor always carries the full subject; this is presentation. */
export function serviceSubjectLabel(subject: string): string {
  const parts = subject.split(":");
  if (
    parts.length === 4 &&
    parts[0] === "system" &&
    parts[1] === "serviceaccount" &&
    parts[2] !== "" &&
    parts[3] !== ""
  ) {
    return `${parts[2]}/${parts[3]}`;
  }
  return subject;
}
