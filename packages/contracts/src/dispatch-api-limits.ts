/**
 * The Dispatch API's closed lists, caps and wire constants: the values the server enforces, the
 * clients refuse on, and `scripts/gen-go.ts` emits into Go, so each is one literal rather than a
 * server string and a client pattern that can drift apart. It imports nothing, so every other
 * module of the contract - and `dispatch-tools.ts`, whose tool schemas take the same caps - can
 * read it.
 */

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
 * The longest issue title, in UTF-16 units, counted after the trim `POST /api/v1/issues` and
 * `PATCH /api/v1/issues/{key}` apply; a longer one is `400 CAP_EXCEEDED`. Production's longest
 * title was 326 characters when this was set. It bounds the duplicate-title check, which compares
 * the new title with every title's lexemes in the project on each creation, and the
 * `POSSIBLE_DUPLICATE` message, which quotes a candidate's whole title. Generated into Go as
 * `contracts.IssueTitleMax`, which the server enforces, and the dashboard's title fields take it as
 * their `maxLength`, so the two agree.
 */
export const ISSUE_TITLE_MAX = 1000;

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

/** Who holds the turn on an open ask after a reply: `human` when the human needs to act,
 *  `agent` when the asking agent still owes the next move (a progress note). */
export const ASK_TURNS = ["human", "agent"] as const;
export type AskTurn = (typeof ASK_TURNS)[number];

/**
 * The closed list of targeted-delivery modes a session can advertise as
 * capabilities on the Envoy listener (`/v1/interests/subscribe`) and that
 * Dispatch can request for a message. Every other spelling of this set —
 * `MessageDeliveryMode`, the delivery event payload schema, the envoy-client
 * targeted-frame schema — derives from this constant.
 */
export const DELIVERY_CAPABILITIES = ["aside", "btw", "steer"] as const;

export type DeliveryCapability = (typeof DELIVERY_CAPABILITIES)[number];

export type MessageDeliveryMode = DeliveryCapability;

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

/** The longest `IDEMPOTENCY_KEY_HEADER` value, in characters. */
export const IDEMPOTENCY_KEY_MAX = 128;

/** A whole `IDEMPOTENCY_KEY_HEADER` value: letters, digits, `.`, `_`, `:` and `-`, from one to
 *  `IDEMPOTENCY_KEY_MAX` characters. Any other is `400 KEY_INVALID` (`IDEMPOTENCY_KEY_INVALID`). */
export const IDEMPOTENCY_KEY_PATTERN = new RegExp(`^[A-Za-z0-9._:-]{1,${IDEMPOTENCY_KEY_MAX}}$`);

/** The code of the `422` for a key already used for a different request (another method,
 *  route, path value or body); that request was not run. */
export const IDEMPOTENCY_KEY_REUSED = "KEY_REUSED";

/** The code of the `409` (with `Retry-After: 1`) for a key whose first request is still running;
 *  nothing was run, and the client asks again under the same key. */
export const IDEMPOTENCY_KEY_IN_FLIGHT = "KEY_IN_FLIGHT";

/** The code of the `409` for a key whose write landed before its answer was lost: Dispatch will
 *  not run it again and has no answer to replay. The write is done; a client treats it as landed
 *  and reads again what it wrote (the event feed carries the change too). */
export const IDEMPOTENCY_KEY_COMMITTED = "KEY_COMMITTED";

/** The code of the `400` for an `IDEMPOTENCY_KEY_HEADER` that does not match
 *  `IDEMPOTENCY_KEY_PATTERN`, or is sent twice: named as `KEY_REUSED`, `KEY_IN_FLIGHT` and
 *  `KEY_COMMITTED` are. */
export const IDEMPOTENCY_KEY_INVALID = "KEY_INVALID";

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
 * The request header a reader of the text routes (`GET /api/v1/artifacts/{id}/text` and its issue
 * and project slug forms) sends, as `Dispatch-Asks: 1`, to say that it reads their `asks` member.
 * Dispatch then serves markdown with none of an ask block's eight server-owned attributes, which
 * the reader can upload back unchanged. A request without it, which is every reader of a release
 * before the header, is served the form kept for one release: each ask block keeps its state, its
 * answerer, its time and a copy's provenance on its opening line, and holds no answer's words.
 * Both forms answer the same token. The `dispatch` command and the page send it on every read of
 * those routes.
 */
export const DISPATCH_ASKS_HEADER = "Dispatch-Asks";

/**
 * The code of the `403` for a write by an access token the secrets broker issued that names a
 * session and a service but no person under the project scope `person`: the machine holding it
 * has signed nobody in, so Dispatch has nobody to record the write under. Its reads are
 * unaffected. The message names `agent-secrets login`, the one command that makes the write
 * possible, because whoever reads the refusal is whoever must run it. A token that names a
 * service and no person under the project scope `named` is no such token: an exchange account
 * entry naming no person minted it, its service is that account, and its writes are recorded
 * under the account.
 */
export const DISPATCH_SIGN_IN_REQUIRED = "SIGN_IN_REQUIRED";

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
 * The code of the `403` for a request by a read-only token that carries a cut (the broker's
 * `as_of`: the past time the token reads at) to a read route other than `GET /api/v1/whoami`.
 * No build serves a read at a past time yet, so Dispatch refuses the request before the route's
 * handler runs, and names the cut, rather than answer it with the present. On a route outside
 * the read class the same token is refused `DISPATCH_READ_ONLY_TOKEN`.
 */
export const DISPATCH_AS_OF_TOKEN_NOT_SERVED = "AS_OF_TOKEN_NOT_SERVED";

/** The codes of a repeat Dispatch gives no copy of its first answer for: `409 KEY_COMMITTED`, a
 *  write that landed, or a replayed 2xx reading `ANSWER_NOT_KEPT`. A client treats a write's as
 *  landed; a call that writes nothing (`IDEMPOTENCY_ANSWER_NOT_KEPT`) asks again. */
export type IdempotencyLandedCode =
  | typeof IDEMPOTENCY_KEY_COMMITTED
  | typeof IDEMPOTENCY_ANSWER_NOT_KEPT;

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

/** How long a key's first request holds the key: a repeat within this is
 *  `409 IDEMPOTENCY_KEY_IN_FLIGHT`. One after it, the first one's process presumed dead, is
 *  answered `409 IDEMPOTENCY_KEY_COMMITTED` when that first run's write committed, is answered
 *  from the record on the five routes that keep one (`IDEMPOTENCY_KEY_HEADER`), and otherwise
 *  takes the key over and runs the request again. 60 seconds. */
export const IDEMPOTENCY_LEASE_MS = 60 * 1000;

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
export type SearchOperator = (typeof SEARCH_OPERATORS)[number];

/** What a `SessionAnswer` is to the session Dispatch delivered it to, one snake_case wire value per
 *  relation, each spelling the phrase a reader prints. A session owns an ask when it asked it or
 *  follows it, and only an owner reads `answered_your_ask`, `declined_your_ask` or
 *  `replied_on_your_ask`. In order: a person answered the session's ask; answered an ask the
 *  session does not own, which reached it for another reason (the issue's route or its claim);
 *  declined to answer the session's ask; declined to answer an ask the session does not own;
 *  replied in the thread of the session's ask; replied in a comment thread it wrote in; commented
 *  on a document it wrote; commented on an issue that reaches it (its route, its claim, or a
 *  credit on its primary document); mentioned it; sent it a message (direct, on an issue, or a
 *  broadcast's copy); or replied to its message. A reply in the thread of an ask the session does
 *  not own carries the relation of the reason that delivered it. A decline is the item of the
 *  kind `declined`, whose `text` is the person's own note; on `declined_your_ask` alone,
 *  `dispatch_words` is what Dispatch wrote beside it for the asking agent, so a reader draws the
 *  two apart. Mirrored in Go as the `model.SessionAnswerRelation*` constants, in this order. */
export const SESSION_ANSWER_RELATIONS = [
  "answered_your_ask",
  "answered_an_ask",
  "declined_your_ask",
  "declined_an_ask",
  "replied_on_your_ask",
  "replied_in_a_comment_thread",
  "commented_on_a_document",
  "commented_on_an_issue",
  "mentioned_you",
  "sent_you_a_message",
  "replied_to_your_message",
] as const;

export type SessionAnswerRelation = (typeof SESSION_ANSWER_RELATIONS)[number];

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

/** The reason the document websocket closes with `DOCUMENT_SCHEMA_CLOSE_CODE`: the code reads of
 *  that document answer (`409 DOC_SCHEMA`). Generated into Go as
 *  `contracts.DocumentSchemaCloseReason`. */
export const DOCUMENT_SCHEMA_CLOSE_REASON = "DOC_SCHEMA";

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

/** The close code Dispatch ends a live socket with once the socket reaches its age, a random point
 *  in [LIVE_SOCKET_MIN_AGE_MS, LIVE_SOCKET_MAX_AGE_MS), or once the Identity-Aware Proxy assertion
 *  its upgrade carried expires (`DISPATCH_IDENTITY=iap`). It is in the private 4000-4999 range,
 *  apart from the 4000 a page closes a silent socket with: the page reopens at once and resumes
 *  every subscription, showing no pill and refreshing nothing. */
export const LIVE_SOCKET_MAX_AGE_CLOSE = 4001;
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
