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
 * before. A new constant of the contract goes here too, rather than beside the schemas, so this
 * module also holds codes no browser reads, which were added after it was split out.
 *
 * The dashboard takes a value of this module through its own subpath,
 * `@legion/contracts/dispatch-api-constants`, never through `@legion/contracts/dispatch-api` or
 * the package index: either would bring the area modules' schemas with the value.
 */

// Types only, but for `outage` of `http-status.ts`, which imports nothing: a type import is
// erased by the compiler, so it builds no edge a bundler would follow back to the schemas.
import type { CredentialPendingRow, MyReplyRow } from "./dispatch-api.generated";
import type { EventType } from "./dispatch-api-events";
import type { MyReplyAgentRow, MyReplyThreadRow } from "./dispatch-api-inbox";
import { outage } from "./http-status";

/** The OAuth scopes a Dispatch credential can hold, mirroring Go's `model.ScopePerson`,
 *  `model.ScopeApprove` and `model.ScopeP0` and the `agent_tokens.scopes` check that stores them.
 *  `person` acts as the credential's own person on every route a person reaches through the
 *  dashboard. `approve` settles what a person settles: a document review, a decision's Keep, the
 *  answer of an approval ask, a machine-login decision, a credential-grant, machine-login or
 *  sign-in revoke, and minting, changing or revoking a personal token. `p0` lets a credential make
 *  an issue's own priority 0 (`POST /api/v1/issues` and `PATCH /api/v1/issues/{key}`). A browser
 *  sign-in holds all three. A personal token holds `p0` only where its maker gave it, and may hold
 *  it alone: no mint gives `p0` by default. A credential holding `approve` holds `person` too.
 *  No broker-issued token, shared token or service-account token holds `p0`, and of the
 *  broker-issued tokens only the approve-only token, which no machine holds, holds `approve`. */
export const DISPATCH_SCOPES = ["person", "approve", "p0"] as const;

/** The scope words a personal token can carry, which the agent token list names and `scopes` in
 *  `GET /api/v1/whoami` names for it: the three of `DISPATCH_SCOPES` and `read`. A token that
 *  holds `read` is read-only (`DISPATCH_READ_ONLY_TOKEN`): a personal token holds it alone or
 *  beside `person`, never beside `approve` or `p0`. No mint gives `p0` by default. `read` is a
 *  mark on the token and not a scope a credential acts under, so no act records it, and
 *  `DISPATCH_SCOPES` and `ActorActSchema` keep `person`, `approve` and `p0` alone.
 *  `GET /api/v1/whoami` also names `legion-status` for a Legion daemon's own
 *  broker token, which holds it alone (`WhoamiResponse`): it is that token's leave to write a
 *  Legion issue's status where Dispatch runs with `DISPATCH_LEGION_STATUS_SCOPE` on, so no act
 *  records it either, and no personal token can hold it. */
export const DISPATCH_TOKEN_SCOPES = ["person", "approve", "p0", "read"] as const;

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
 * Maximum entries in an issue's `advances` input, before duplicate ids are removed: the most
 * priorities one issue states it advances. It counts the issue's own relations alone, never what
 * a child inherits from the issues above it, which is read-side: an issue five deep would
 * otherwise pass the cap without writing anything. Generated into Go as
 * `contracts.MaxIssueAdvances`, which `POST /api/v1/issues` and `PATCH /api/v1/issues/{key}`
 * enforce as `400 ADVANCES_INPUT` naming the limit.
 */
export const MAX_ISSUE_ADVANCES = 20;

/**
 * The most labels one issue holds, and so the most one `labels` or `labels_add` input names,
 * before duplicates, which differ in case alone, are removed. `labels_add` is held to it twice:
 * the input alone, and the issue's labels with the input added. Generated into Go as
 * `contracts.MaxIssueLabels`, which `POST /api/v1/issues`, `PATCH /api/v1/issues/{key}` and the
 * `label` filter of `GET /api/v1/issues` enforce as `400 LABELS_INPUT` naming the limit, and the
 * issue tools' `labels` and `labels_add` take it as their bound.
 */
export const MAX_ISSUE_LABELS = 20;

/**
 * The longest label, in UTF-16 units, counted after the trim the issue routes apply; a blank or
 * longer one is `400 LABELS_INPUT`. Generated into Go as `contracts.IssueLabelMax`, which those
 * routes enforce, and which the label the dashboard gives an issue it files from a person's typed
 * words (`DISPATCH_NEW_ISSUE_LABEL`) is held to at boot; the issue tools' labels take it as each
 * item's bound.
 */
export const ISSUE_LABEL_MAX = 40;

/**
 * The most component ids one `components` input of mode `explicit` names, before duplicates are
 * removed. Generated into Go as `contracts.MaxIssueComponents`, which `POST /api/v1/issues` and
 * `PATCH /api/v1/issues/{key}` enforce as `400 COMPONENTS_INPUT` naming the limit, and the issue
 * tools' `components.ids` take it as its bound.
 */
export const MAX_ISSUE_COMPONENTS = 50;

/**
 * The most bytes of markdown one document may hold, counted in UTF-8 as the server counts a body:
 * what one upload of a markdown document may send, and what a write may grow a stored document's
 * rendering to. An upload past it is `413 CAP_EXCEEDED` with MARKDOWN_DOCUMENT_TOO_LARGE, up to the
 * server's 25 MiB file cap; past that cap the server answers `artifact blob exceeds 25 MB` instead.
 * Generated into Go as `contracts.MarkdownDocumentMaxBytes`, which is the server's
 * `pmdoc.MaxDocumentBytes`. `dispatch artifact` and its dry run refuse a text past it, given
 * inline, in `--content-file`, on stdin or in a Markdown `--path` file, before they ask the round
 * trip or upload, and the call before it reads whether the owner holds the name.
 */
export const MARKDOWN_DOCUMENT_MAX_BYTES = 1 << 20;

/**
 * What the `413 CAP_EXCEEDED` of an upload whose markdown is past MARKDOWN_DOCUMENT_MAX_BYTES says.
 * Generated into Go as `contracts.MarkdownDocumentTooLarge`, which the upload route answers, and
 * `dispatch artifact` prints it after `CAP_EXCEEDED: ` for a text past that bound, however it is
 * given, so its own refusal and the server's are one text for an upload within the server's 25 MiB
 * file cap.
 */
export const MARKDOWN_DOCUMENT_TOO_LARGE = `a markdown document is at most ${MARKDOWN_DOCUMENT_MAX_BYTES / (1 << 20)} MiB; a larger file is stored as a binary artifact under another content type`;

/**
 * The response header that the markdown round trips (`POST /api/v1/markdown/round-trip`, `POST
 * /api/v1/markdown/file-round-trip`) put on their refusal of a text for the text alone, whatever
 * the refusal's status: a mark, with one value, that says the answer is Dispatch's refusal of the
 * text, so a client reads only that the header is there. The routes make the checks an upload
 * makes of the text alone, in the order, status, code and words of that upload, and make a new
 * document's only for a request that asks for them (`new_document`). Generated into Go as
 * `contracts.TextRefusalHeader`. `dispatch artifact` asks for a new document's refusals only where
 * the call makes a new document, so it reads an answer carrying the header as the call's own
 * refusal, and any other failure of the round trip as no answer at all, so a check of the text
 * Dispatch adds to that upload, at any status, reaches the call and its dry run with no change to
 * the client.
 */
export const TEXT_REFUSAL_HEADER = "Dispatch-Text-Refusal";

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
 * The most rows one answer of a post's history holds, newest first: `GET
 * /api/v1/comments/{id}/history` and `GET /api/v1/messages/{id}/history`, whose `?before=` the
 * oldest row's `edited_at` answers the page after. Generated into Go as
 * `contracts.PostHistoryPageLimit`, the page the server reads, and `dispatch read --history`
 * takes a page of exactly this many rows as one that may have older rows, so the two ends agree.
 */
export const POST_HISTORY_PAGE_LIMIT = 200;

/**
 * The longest priority id, in characters: an id is ASCII, so in bytes too. Two readers take it,
 * so they hold one number. `POST /api/v1/priorities` refuses an `id` the caller names that is
 * longer, as `400 PRIORITY_INPUT` naming this bound. The same route cuts the id it derives from
 * the title, when the body names none, to fit it. The cut keeps a slug of at most this many
 * characters whole; it cuts a longer one to its longest start that fits and ends at the end of a
 * word, or to its first this many characters when its first word is longer.
 * `fixtures/priority-ids.json` holds the cases of that rule, and its `description` names the
 * tests that read them. Generated into Go as `contracts.PriorityIDMax`.
 */
export const PRIORITY_ID_MAX = 200;

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

/** Whether a row of `GET /api/v1/credential-requests?approver=me` still waits on somebody's
 *  decision: a pending row, which a broker older than the expired rows sends with no `state` at
 *  all. An expired row - its deadline passed before anybody decided it, so the broker refuses
 *  every decision of it - stays listed until somebody dismisses it, but nothing waits on the
 *  viewer there: no `Needs you` count holds it, on the page or in `dispatch inbox`, and it does
 *  not keep the Inbox from saying that nothing needs them. */
export function awaitsDecision(row: Pick<CredentialPendingRow, "state">): boolean {
  return row.state !== "expired";
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
 * The four states one delivery attempt, of a message or of a mention, is in.
 *
 * `pending` is an attempt Dispatch committed and has not settled: it is before the listener
 * call, or its outcome was never learned. `sent` and `queued` are both the listener having
 * taken the message, and every reader that asks whether it took it counts the two alike - the
 * outbox publishes no second copy, the delivery sweep settles neither as failed, and the
 * session may take either as its own turn. They differ in one thing: a sent message is with a
 * session that is running now, and a queued one waits in the consumer of a session the listener
 * knows and that is not running, which gets it when it runs again, within the
 * `DELIVERY_DUPLICATE_WINDOW_MS` the stream keeps it. `failed` is everything else, and only a
 * failed attempt may be retried: a queued attempt is already stored, so a repeat would store a
 * second copy.
 */
export const DELIVERY_ATTEMPT_STATES = ["pending", "sent", "queued", "failed"] as const;

export type DeliveryAttemptState = (typeof DELIVERY_ATTEMPT_STATES)[number];

/** What a delivery receipt in the event log carries: an attempt that settled, never `pending`,
 *  because the sender that appends the receipt holds the attempt's claim and settles the row in
 *  the statement that appends it. */
export const SETTLED_DELIVERY_STATES = [
  "sent",
  "queued",
  "failed",
] as const satisfies readonly DeliveryAttemptState[];

export type SettledDeliveryState = (typeof SETTLED_DELIVERY_STATES)[number];

/**
 * The most sessions one broadcast sends to (`POST /api/v1/broadcasts`'s `session_ids`, after the
 * duplicates are dropped). A runaway guard, not a product limit: selection is a human ticking
 * boxes, but select-all makes a large send one click. Generated into Go as
 * `contracts.MaxBroadcastRecipients`, which the server enforces, so the dashboard's refusal and
 * the server's are one number.
 */
export const MAX_BROADCAST_RECIPIENTS = 100;

/**
 * The most session ids one `GET /api/v1/agents/known` names (`id`, repeated). A page asks
 * once for every agent reference it holds that the live registry does not list, so the bound is a
 * page's worth with room to spare, and a page holding more asks in batches of this size. Generated
 * into Go as `contracts.KnownSessionsMax`, which the server enforces with `400 INVALID_QUERY`, so
 * the dashboard's batches and the server's refusal are one number.
 */
export const KNOWN_SESSIONS_MAX = 100;

/**
 * The most references one `GET /api/v1/reference-targets` may name, one `ref` each; past it the
 * server answers `400 CAP_EXCEEDED`. It is set above the heaviest set a page was seen to name:
 * 247 distinct references, measured on 2026-11-05 on the issue the new issue page was designed
 * on and its four largest documents. The issue page read its chips with one
 * `GET /api/v1/references` each, 14 on its heaviest tab, measured on 2026-11-05. So the count
 * does not split such a load into a second request, while the client's own budget for the
 * request line can. Generated into Go as `contracts.ReferenceTargetsMax`, which the server
 * enforces, so the server's refusal and the client's batches are one number.
 */
export const REFERENCE_TARGETS_MAX = 256;

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
 * `409 IDEMPOTENCY_KEY_COMMITTED`. `POST /api/v1/issues/{key}/status-change` keeps a recovery
 * point too: a repeat of a finished call answers its `200` from what its steps stored, each issue
 * as its step's write answered it, and a repeat that takes the key of a call that stopped runs
 * only the steps the point does not hold. Where a first answer was a comment or an issue message,
 * eight routes answer its repeat with that post read afresh, under the first answer's status: a
 * comment's edit, resolve, reopen, accept and reject, an issue message's edit, and a session's
 * reply to a comment or a message. Any other stored success is not replayed once it holds a
 * comment or an issue message that is deleted (an object whose own id is the post, such as a
 * related-item hit with its snippet) or a document version a delete renamed: such a repeat is the
 * first answer's status with `IDEMPOTENCY_ANSWER_NOT_KEPT`, and where one of the five routes above
 * answers a repeat of an answer a delete so ended from what it made, that replay keeps none of the
 * first answer's other members, such as its `advice`. A post named only in a text, a citation,
 * holds no part of it and ends nothing, and a stored refusal is replayed as it was. A write
 * without the header is run as it always was, with no replay (the IETF `Idempotency-Key` draft;
 * Google AIP-155).
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

/** The code a replay's body carries when Dispatch gives no copy of the first answer: one larger
 *  than it keeps (1 MiB), or a success that holds a deleted post or a version a delete renamed
 *  (`IDEMPOTENCY_KEY_HEADER`). The replay has the first answer's status and this body instead of
 *  the first's, and its message says what the request did, as the key recorded it: a request that
 *  wrote ran, and its caller reads what it wrote; a request that wrote nothing, such as the
 *  markdown round trip or the related-item check, may be sent again under a new key. */
export const IDEMPOTENCY_ANSWER_NOT_KEPT = "ANSWER_NOT_KEPT";

/**
 * The request header a page puts on every write it makes (`POST`, `PATCH`, `PUT`, `DELETE`): the
 * login of the person who pressed it. Dispatch answers `409 DISPATCH_LOGIN_CHANGED` before
 * anything runs, its key untouched, when the request's caller is another person or no person at
 * all, so a write one person pressed never runs under another's session - another tab signs in
 * in place, and this tab's retry goes out on the new cookie before it learns of it. A caller that
 * resolves to nobody (a lapsed cookie) is answered by the `401` written further in: the keyed
 * layer's for a keyed write, the handler's for any other. A write without the header (the
 * `dispatch` command, an agent) is not checked.
 */
export const DISPATCH_LOGIN_HEADER = "Dispatch-Login";

/** The code of the `409` for a write whose `DISPATCH_LOGIN_HEADER` names a login that is not its
 *  caller's: nothing ran. The page drops every write that login pressed. */
export const DISPATCH_LOGIN_CHANGED = "LOGIN_CHANGED";

/**
 * Whether an attempt Dispatch answered with `status` (and `code`, its error body's) ended without
 * Dispatch's verdict on the key, so a client asks again under the same key: an outage (`outage`: a
 * 5xx, a 408 or a 429, none of which Dispatch stores, `IDEMPOTENCY_KEY_HEADER`), or
 * `409 IDEMPOTENCY_KEY_IN_FLIGHT` (the first request under the key is still running), which is
 * Dispatch's keyed-write layer rather than an outage. Any other answer is Dispatch's verdict, and
 * the same key would be answered with it again. The page and the `dispatch` command both retry by
 * this one rule.
 */
export function retriesUnderKey(status: number, code: string | undefined): boolean {
  return outage(status) || (status === 409 && code === IDEMPOTENCY_KEY_IN_FLIGHT);
}

/**
 * Whether an answer of `status`, `replayed` (it carried `IDEMPOTENCY_REPLAYED_HEADER`) and parsed
 * `body` is a replay whose first answer Dispatch did not keep: a replayed 2xx whose body is
 * `{ code: IDEMPOTENCY_ANSWER_NOT_KEPT }`. Its caller takes no resource from this body: a write
 * that ran reads what it wrote again, and a call that writes nothing asks again under a new key.
 * An answer that is not a replay is the resource itself, whatever it holds.
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

/** `DELIVERY_DUPLICATE_WINDOW_MS` in whole hours, for text that names the window. */
export const DELIVERY_DUPLICATE_WINDOW_HOURS = DELIVERY_DUPLICATE_WINDOW_MS / 3_600_000;

/**
 * The cause a delivery attempt records when the listener never answered its send. The listener
 * publishes before it answers, so a send whose answer missed the window may already have reached
 * the recipient — which is what makes re-sending in another mode a genuine second delivery, and
 * what the dashboard keys that wording on (`POSSIBLY_DELIVERED_CAUSES`). Generated into Go as
 * `contracts.ReceiptTimeoutCause`, so the string the server writes and the string the dashboard
 * recognises are one literal.
 *
 * It is the CAUSE only: the advice that follows from it expires with
 * `DELIVERY_DUPLICATE_WINDOW_MS`, so it is composed at render time and never stored.
 */
export const RECEIPT_TIMEOUT_CAUSE =
  "The listener didn't answer within the send window; the message may already have been delivered.";

/**
 * The error a delete settles each send of its post still pending with: the post's delete ended
 * the attempt, and nothing more - not whether its frame had already reached the session. Each
 * such attempt also gets a failed receipt carrying it. Generated into Go as
 * `contracts.DeletedPostDeliveryError`, so the string the server writes and the string the
 * dashboard reads a delete's receipts and attempts by are one literal.
 */
export const DELETED_POST_DELIVERY_ERROR = "ended by the post's delete";

/**
 * The cause a delivery attempt records when the listener answered its send 503 with the code that
 * says the message may be stored (`maybe_stored`): the listener published it and the message bus
 * never acknowledged the publish, so it may already have reached the recipient, as a receipt
 * timeout may. It is a cause of its own so the attempt's record names what happened - the listener
 * answered, and could not confirm the store - and the dashboard words it as such while giving it
 * the receipt timeout's possible-second-delivery warning (`POSSIBLY_DELIVERED_CAUSES`). Generated
 * into Go as `contracts.StoreUnconfirmedCause`.
 *
 * It is the CAUSE only, as `RECEIPT_TIMEOUT_CAUSE` is.
 */
export const STORE_UNCONFIRMED_CAUSE =
  "The listener sent the message but could not confirm it was stored; the message may already have been delivered.";

/** The causes whose send may already have reached the recipient, so that re-sending it in another
 * mode delivers it a second time: the one list the dashboard keys that warning on. */
export const POSSIBLY_DELIVERED_CAUSES: readonly string[] = [
  RECEIPT_TIMEOUT_CAUSE,
  STORE_UNCONFIRMED_CAUSE,
];

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
 * The code of the `403` for a request by a read-only token to a route outside Dispatch's read
 * class. Every token that holds `read` is read-only: one the secrets broker minted from a read
 * entry or narrowed when its trade asked for `read`, and a personal token minted with `read`.
 * Dispatch serves such a token on the GET routes it classes as reads and refuses it on every
 * other route and on `GET /ws/live`, before the route's handler runs and before its idempotency
 * key is read. So the refused request claims no key, and writes nothing but the record of the
 * token's use: a broker token's spent DPoP proof, or a personal token's last use. No sign-in and
 * no other scope makes the request possible for that token.
 */
export const DISPATCH_READ_ONLY_TOKEN = "READ_ONLY_TOKEN";

/**
 * The code of the `403` for a request by an approve-only token - the token the secrets broker
 * mints for a person's approve grant and spends through Cipher's proxy, of the OAuth scope
 * `approve` alone - to a route Dispatch's approve list (`approveScopeRoutes`) does not name, and
 * for its answer of an ask that is not an approval request. The route ran nothing and nothing was
 * recorded. No sign-in and no other scope makes the request possible for that token.
 */
export const DISPATCH_APPROVE_ONLY_TOKEN = "APPROVE_ONLY_TOKEN";

/**
 * The code of the `403` for a request by a read-only token that carries a cut (the broker's
 * `as_of`: the past time the token reads at) to a read route other than `GET /api/v1/whoami`.
 * No build serves a read at a past time yet, so Dispatch refuses the request before the route's
 * handler runs, and names the cut, rather than answer it with the present. On a route outside
 * the read class the same token is refused `DISPATCH_READ_ONLY_TOKEN`.
 */
export const DISPATCH_AS_OF_TOKEN_NOT_SERVED = "AS_OF_TOKEN_NOT_SERVED";

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
 * The close code the document websocket (`/ws/doc/{room}`) refuses a room with when the tree it
 * holds has an element that this Dispatch process's build cannot read: a newer build wrote it while
 * two builds serve one database during a release. It completes the upgrade and closes with this
 * code, and `DOCUMENT_NEWER_CONTENT_CLOSE_REASON`, before any sync, so a browser editor never
 * receives a tree it would write back without that element. The pinned `@hocuspocus/provider`
 * (2.15) stops reconnecting only on 4401 and 1009 (`HocuspocusProviderWebsocket.ts` `onClose`), so
 * it reconnects on this code with its backoff, and a later attempt can reach a build that reads the
 * document. 425 is HTTP's "Too Early". Generated into Go as
 * `contracts.DocumentNewerContentCloseCode`.
 */
export const DOCUMENT_NEWER_CONTENT_CLOSE_CODE = 4425;

/** The reason the document websocket closes with `DOCUMENT_NEWER_CONTENT_CLOSE_CODE`: the code reads
 *  of that document answer (`409 DOC_NEWER_CONTENT`). Generated into Go as
 *  `contracts.DocumentNewerContentCloseReason`. */
export const DOCUMENT_NEWER_CONTENT_CLOSE_REASON = "DOC_NEWER_CONTENT";

/**
 * The close code the document websocket (`/ws/doc/{room}`) ends an open writable socket with once
 * the live server processes no longer admit the page that opened it: a build whose numbers the
 * admission held against has joined or rolled away, so that page may no longer write
 * (`RunSchemaAdmission`). The close carries this code and
 * `DOCUMENT_ADMISSION_REVOKED_CLOSE_REASON`; what that close waits for, and what a peer too slow
 * to take it reads instead, are in `packages/envoy/cmd/dispatch/README.md`, "Two builds at once",
 * which is the one statement of that rule. The pinned `@hocuspocus/provider` (2.15) stops
 * reconnecting only on 4401, 4403 and 1009 (`HocuspocusProviderWebsocket.ts` `onClose`), so the
 * page reconnects on this code, presents its numbers again and is admitted again: read-only,
 * unless a live process admits it. 412 is HTTP's "Precondition Failed", which the admission the
 * fleet withdrew is. The socket closes with it too once the Identity-Aware Proxy assertion its
 * upgrade carried expires (`DISPATCH_IDENTITY=iap`): the reconnect carries a fresh assertion, which
 * is judged again. Generated into Go as `contracts.DocumentAdmissionRevokedCloseCode`.
 */
export const DOCUMENT_ADMISSION_REVOKED_CLOSE_CODE = 4412;

/** The reason the document websocket closes with `DOCUMENT_ADMISSION_REVOKED_CLOSE_CODE`.
 *  Generated into Go as `contracts.DocumentAdmissionRevokedCloseReason`. */
export const DOCUMENT_ADMISSION_REVOKED_CLOSE_REASON = "DOC_ADMISSION_REVOKED";

/**
 * The code of the `409` Dispatch answers an id written as a prefix two items or more carry: a
 * page address, a `dispatch://` reference or a dashboard address in text, or a route that takes
 * an id in its path (INFRA-869). `candidates` then lists the items it names, at most ten, in id
 * order, for the caller to write more of the id or to choose one. Generated into Go as
 * `contracts.IDAmbiguous`.
 */
export const ID_AMBIGUOUS = "ID_AMBIGUOUS";

/** The close code Dispatch ends a live socket with once the socket reaches its age, a random point
 *  in [LIVE_SOCKET_MIN_AGE_MS, LIVE_SOCKET_MAX_AGE_MS), or once the Identity-Aware Proxy assertion
 *  its upgrade carried expires (`DISPATCH_IDENTITY=iap`). It is in the private 4000-4999 range,
 *  apart from the 4000 a page closes a silent socket with: the page reopens at once and resumes
 *  every subscription, showing no pill and refreshing nothing. */
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

/** How the message of a Dispatch process's `404 NOT_FOUND` starts when the process has no route for
 *  the request ("no route for GET /api/v1/…"). While a release rolls out, two builds of Dispatch
 *  answer, and the older one answers this way for a route only the newer one serves. No handler
 *  ran, so a client may send the request again whatever its method. A `404 NOT_FOUND` for a
 *  resource the server does not know says "not found" instead. */
export const NO_ROUTE_MESSAGE_PREFIX = "no route for ";

/** The waits before each new attempt of a request a Dispatch process answered with no route. Each
 *  wait is jittered by ±25 % (`jitteredDelay`), so the attempts of many clients spread out, and
 *  each attempt may reach another process. */
export const NO_ROUTE_RETRY_DELAYS_MS: readonly number[] = [1_000, 2_000, 4_000];

/** What a client says when every attempt of a request met a Dispatch process with no route for it. */
export const NO_ROUTE_ROLLING_RELEASE_MESSAGE =
  "The Dispatch process that answered does not offer this action yet, because a new release of " +
  "Dispatch is in progress. Try again in one minute.";

/** Waits `ms` milliseconds, or until `signal` aborts. A client takes one so that a test does not
 *  wait. */
export type Sleep = (ms: number, signal?: AbortSignal | null) => Promise<void>;

/** The `Sleep` a client waits with when its caller gives none: a timer. When `signal` aborts,
 *  before or during the wait, the wait rejects at once with the signal's reason, so an aborted
 *  request sends nothing more. */
export const sleep: Sleep = (ms, signal) => {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  if (signal?.aborted) {
    reject(signal.reason);
    return promise;
  }
  const abort = () => {
    clearTimeout(timer);
    reject(signal?.reason);
  };
  const timer = setTimeout(() => {
    signal?.removeEventListener("abort", abort);
    resolve();
  }, ms);
  signal?.addEventListener("abort", abort, { once: true });
  return promise;
};

/** `ms` jittered by ±25 %: a wait of `NO_ROUTE_RETRY_DELAYS_MS`, as a client waits it
 *  (`retryNoRoute`, and the Dispatch write client's own loop). */
export function jitteredDelay(ms: number): number {
  return ms * (0.75 + Math.random() * 0.5);
}

/**
 * Runs `attempt` until a Dispatch process that serves the route answers. An attempt `isNoRoute`
 * recognises is made again after each of `NO_ROUTE_RETRY_DELAYS_MS`, jittered (`jitteredDelay`)
 * and waited with `options.sleep` (a timer, `sleep`, when the caller names none): while a release
 * rolls out, two builds of Dispatch answer, and the next attempt may reach the build that serves
 * the route. No handler ran, so this is safe for every method, and every attempt is the same
 * request. After the last attempt it throws what `options.exhausted` builds, which is the
 * caller's own error for `NO_ROUTE_ROLLING_RELEASE_MESSAGE`. Anything else `attempt` throws is
 * thrown on at once, and an abort of `options.signal` during a wait rejects with the abort's
 * reason, so nothing more is sent.
 */
export async function retryNoRoute<T>(
  attempt: () => Promise<T>,
  isNoRoute: (error: unknown) => boolean,
  options: {
    readonly exhausted: () => Error;
    readonly sleep?: Sleep;
    readonly signal?: AbortSignal | null;
  }
): Promise<T> {
  const wait = options.sleep ?? sleep;
  for (const delay of [...NO_ROUTE_RETRY_DELAYS_MS, undefined]) {
    try {
      return await attempt();
    } catch (error) {
      if (!isNoRoute(error)) throw error;
      if (delay === undefined) break;
    }
    await wait(jitteredDelay(delay), options.signal);
  }
  throw options.exhausted();
}

/** Each member of `EventType` once: a record over the union, so the compiler refuses a type left
 *  out of it and a key that is no type. The union is `dispatch-api-events.ts`'s, beside its
 *  payloads' schemas; this record, the list and `isEventType` below are here, for the reason the
 *  head of this module gives: the dashboard's event feed reads `isEventType` on every frame, and
 *  from beside the union it would bring every area module's schemas into the entry chunk. */
const EVENT_TYPE_MEMBERS: Record<EventType, true> = {
  "architecture.sync_failed": true,
  "architecture.synced": true,
  "artifact.approved": true,
  "artifact.changes_requested": true,
  "artifact.created": true,
  "artifact.human_edited": true,
  "artifact.removed": true,
  "artifact.renamed": true,
  "artifact.version": true,
  "ask.anchor_refreshed": true,
  "ask.answered": true,
  "ask.asked_of": true,
  "ask.edited": true,
  "ask.follower_added": true,
  "ask.follower_removed": true,
  "ask.handed_back": true,
  "ask.opened": true,
  "ask.resolved": true,
  "block.invalid": true,
  "block.repaired": true,
  "child.added": true,
  "child.removed": true,
  "child.status": true,
  "comment.anchor_refreshed": true,
  "comment.answered": true,
  "comment.created": true,
  "comment.deleted": true,
  "comment.delivery": true,
  "comment.edited": true,
  "comment.reopened": true,
  "comment.resolved": true,
  "decision.kept": true,
  "deploy.recorded": true,
  "handoff.recorded": true,
  "issue.claimed": true,
  "issue.closed": true,
  "issue.created": true,
  "issue.deployed": true,
  "issue.pull_request_attribution_changed": true,
  "issue.released": true,
  "issue.updated": true,
  "message.accepted": true,
  "message.answered": true,
  "message.created": true,
  "message.deleted": true,
  "message.delivery": true,
  "message.edited": true,
  "priority.updated": true,
  "project.access_changed": true,
  "project.created": true,
  "project.updated": true,
  "result.recorded": true,
  "result.retracted": true,
  "settings.architecture_source.updated": true,
  "settings.project_default_assignee.updated": true,
  "settings.repo_project.updated": true,
  "subscription.remove_requested": true,
  "subscription.removed": true,
  "suggestion.accepted": true,
  "suggestion.rejected": true,
  "user_agent_state.updated": true,
  "user_state.updated": true,
  "user_thread_state.updated": true,
};

/**
 * Every event type Dispatch writes to its log, sorted: the members of the `DispatchEvent` union,
 * held to it by `EVENT_TYPE_MEMBERS`. `scripts/gen-go.ts` writes it into envoy's
 * `internal/contracts` as `DispatchEventTypes`, so a Go reader that sorts every type, as the
 * keyword fill's inventory test does, learns of a new one from the same list.
 */
export const DISPATCH_EVENT_TYPES: readonly EventType[] = (
  Object.keys(EVENT_TYPE_MEMBERS) as EventType[]
).sort();

/**
 * Whether `value` names an event type of the `DispatchEvent` union, so a reader of a frame can
 * narrow its `type` before it reads the frame as that event. `Object.hasOwn`, not `in`:
 * `EVENT_TYPE_MEMBERS` inherits `Object.prototype`, so `in` would read `constructor` or
 * `toString` as an event type.
 */
export function isEventType(value: string): value is EventType {
  return Object.hasOwn(EVENT_TYPE_MEMBERS, value);
}
