/**
 * The Dispatch API's closed lists, caps and wire constants that no reader outside the schema
 * modules takes: the values the server enforces, the clients refuse on, and `scripts/gen-go.ts`
 * emits into Go, so each is one literal rather than a server string and a client pattern that
 * can drift apart. The ones a browser or `dispatch-tools.ts` reads are in
 * `dispatch-api-constants.ts`, which builds no schema either, and this module takes from it
 * only the types it derives from them.
 */
import type {
  ASK_TURNS,
  DELIVERY_CAPABILITIES,
  IDEMPOTENCY_ANSWER_NOT_KEPT,
  IDEMPOTENCY_KEY_COMMITTED,
  SEARCH_OPERATORS,
} from "./dispatch-api-constants";

export type AskTurn = (typeof ASK_TURNS)[number];

export type DeliveryCapability = (typeof DELIVERY_CAPABILITIES)[number];

export type MessageDeliveryMode = DeliveryCapability;

/** The longest `IDEMPOTENCY_KEY_HEADER` value, in characters. */
export const IDEMPOTENCY_KEY_MAX = 128;

/** A whole `IDEMPOTENCY_KEY_HEADER` value: letters, digits, `.`, `_`, `:` and `-`, from one to
 *  `IDEMPOTENCY_KEY_MAX` characters. Any other is `400 KEY_INVALID` (`IDEMPOTENCY_KEY_INVALID`). */
export const IDEMPOTENCY_KEY_PATTERN = new RegExp(`^[A-Za-z0-9._:-]{1,${IDEMPOTENCY_KEY_MAX}}$`);

/** The code of the `422` for a key already used for a different request (another method,
 *  route, path value or body); that request was not run. */
export const IDEMPOTENCY_KEY_REUSED = "KEY_REUSED";

/** The code of the `400` for an `IDEMPOTENCY_KEY_HEADER` that does not match
 *  `IDEMPOTENCY_KEY_PATTERN`, or is sent twice: named as `KEY_REUSED`, `KEY_IN_FLIGHT` and
 *  `KEY_COMMITTED` are. */
export const IDEMPOTENCY_KEY_INVALID = "KEY_INVALID";

/**
 * The code of the `403` for a write by an access token the secrets broker issued that names a
 * session and a service but no person: the machine holding it has signed nobody in, so Dispatch
 * has nobody to record the write under. Its reads are unaffected. The message names
 * `agent-secrets login`, the one command that makes the write possible, because whoever reads
 * the refusal is whoever must run it.
 */
export const DISPATCH_SIGN_IN_REQUIRED = "SIGN_IN_REQUIRED";

/** The codes of a write Dispatch made and has no answer to give for: `409 KEY_COMMITTED`, or a
 *  replayed 2xx reading `ANSWER_NOT_KEPT`. A client treats either as landed. */
export type IdempotencyLandedCode =
  | typeof IDEMPOTENCY_KEY_COMMITTED
  | typeof IDEMPOTENCY_ANSWER_NOT_KEPT;

/** How long a key's first request holds the key: a repeat within this is
 *  `409 IDEMPOTENCY_KEY_IN_FLIGHT`. One after it, the first one's process presumed dead, is
 *  answered `409 IDEMPOTENCY_KEY_COMMITTED` when that first run's write committed, is answered
 *  from the record on the five routes that keep one (`IDEMPOTENCY_KEY_HEADER`), and otherwise
 *  takes the key over and runs the request again. 60 seconds. */
export const IDEMPOTENCY_LEASE_MS = 60 * 1000;

export type SearchOperator = (typeof SEARCH_OPERATORS)[number];

/** What a `SessionAnswer` is to the session Dispatch delivered it to, one snake_case wire value per
 *  relation, each spelling the phrase a reader prints: a person answered the session's ask,
 *  declined to answer it, replied in its ask's thread, replied in a comment thread it wrote in,
 *  commented on a document it wrote, commented on an issue routed to it, mentioned it, sent it a
 *  message (direct, on an issue, or a broadcast's copy), or replied to its message. A decline is
 *  the item of the new kind `declined`, whose `text` is the person's own note and whose
 *  `dispatch_words` is what Dispatch wrote beside it, so a reader draws the two apart. Mirrored in
 *  Go as the `model.SessionAnswerRelation*` constants, in this order. */
export const SESSION_ANSWER_RELATIONS = [
  "answered_your_ask",
  "declined_your_ask",
  "replied_on_your_ask",
  "replied_in_a_comment_thread",
  "commented_on_a_document",
  "commented_on_an_issue",
  "mentioned_you",
  "sent_you_a_message",
  "replied_to_your_message",
] as const;

export type SessionAnswerRelation = (typeof SESSION_ANSWER_RELATIONS)[number];

/** The reason the document websocket closes with `DOCUMENT_SCHEMA_CLOSE_CODE`: the code reads of
 *  that document answer (`409 DOC_SCHEMA`). Generated into Go as
 *  `contracts.DocumentSchemaCloseReason`. */
export const DOCUMENT_SCHEMA_CLOSE_REASON = "DOC_SCHEMA";

/** The youngest age at which Dispatch ends a live socket: 30 minutes. */
export const LIVE_SOCKET_MIN_AGE_MS = 1_800_000;
/** The age before which Dispatch ends every live socket: 60 minutes. */
export const LIVE_SOCKET_MAX_AGE_MS = 3_600_000;
/** The most subscriptions one live socket holds at once; a subscribe past it is refused with a
 *  `TOO_MANY_SUBSCRIPTIONS` error frame. */
export const LIVE_SOCKET_MAX_SUBSCRIPTIONS = 64;

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
