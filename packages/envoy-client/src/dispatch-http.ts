import { TradeRetryingError } from "@legion/cipher-client/token-errors";
import type {
  AcceptedMessageDelivery,
  Actor,
  Advised,
  Agent,
  ApprovalRequestResponse,
  ArchitectureSource,
  Artifact,
  ArtifactBlock,
  ArtifactReferences,
  ArtifactReview,
  ArtifactText,
  ArtifactUploadResponse,
  ArtifactVersionText,
  AskRead,
  AskWithTrees,
  CheckResultDryRun,
  CheckResultRecorded,
  CheckResultRetraction,
  CheckResultRetractionInput,
  ClaimedIssue,
  Comment,
  CommentRead,
  CommentWriteResponse,
  CreateAgentMessageInput,
  CreateArtifactInput,
  CreateAskInput,
  CreateCommentInput,
  CreatedProject,
  CreateIssueInput,
  CreateMessageInput,
  CreateProjectInput,
  CreateVersionInput,
  CredentialPendingResponse,
  DeployInput,
  DeployResponse,
  DeploysResponse,
  DispatchApiMethod,
  DispatchEvent,
  DispatchLimits,
  DispatchServiceErrorShape,
  DocumentAdvised,
  EditArtifactInput,
  EditArtifactResponse,
  EditAskInput,
  FileRoundTripInput,
  GraphEdgeKind,
  GraphReferences,
  Handoff,
  HandoffSummary,
  InboxRow,
  Issue,
  IssueCheckResults,
  IssueCoverage,
  IssueDetails,
  IssuePriority,
  IssueRead,
  IssueReferencesAnswer,
  IssueRouteStatus,
  IssueSteps,
  IssueSummary,
  IssueSummaryPage,
  ItemBlocks,
  MarkdownRoundTripInput,
  MarkdownRoundTripResponse,
  Message,
  MessageRead,
  MessageReplyInput,
  MessageReplyResult,
  MyRepliesResponse,
  OpenAsksResponse,
  PostHistoryRead,
  Priorities,
  Project,
  ProjectPlans,
  RecordCheckResultInput,
  RelatedCheckInput,
  ResolveAskInput,
  SearchResponse,
  SearchResultKind,
  SessionAnswersResponse,
  SpecCheckResult,
  SpecCheckResultInput,
  SpecCheckResults,
  StatusChangeAnswer,
  StatusChangeInput,
  Suggestions,
  UpdateIssueInput,
  Version,
  WhoamiResponse,
} from "@legion/contracts";
import {
  answerNotKept,
  DEFAULT_ISSUE_READ_EVENTS,
  everyBuildReadsSearch,
  IDEMPOTENCY_ANSWER_NOT_KEPT,
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_HEADER,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_KEY_REUSED,
  IDEMPOTENCY_REPLAYED_HEADER,
  type IdempotencyLandedCode,
  jitteredDelay,
  NO_ROUTE_MESSAGE_PREFIX,
  NO_ROUTE_RETRY_DELAYS_MS,
  NO_ROUTE_ROLLING_RELEASE_MESSAGE,
  retriesUnderKey,
  retryNoRoute,
  type Sleep,
  sleep,
  TEXT_REFUSAL_HEADER,
} from "@legion/contracts";
import { holdUrlUserinfo } from "@legion/contracts/held-secrets";
import { mayHaveReachedServer, outage, retryAfterMs } from "@legion/contracts/http-status";
import { errorClass, isCleanUrl, requestForText, urlForText } from "@legion/contracts/log-text";
import { withoutTrailingSlashes } from "@legion/contracts/trailing-slash";
import type { ZodType } from "zod";
import { type ApproveRoute, credentialDeadline, type RequestCredential } from "./bearer";
import { rowStores, WRITE_ROWS } from "./dispatch-write-rows";
import { LoggableError, reachedNothing } from "./errors";
import { bodyForText, excerpt, redirectOrigin, SentTokens } from "./excerpt";
import { pathNamesNothingPattern } from "./go-space";
import { type WriteEvent, type WriteState, writeStep } from "./write-outcome";

/** What names the Dispatch URL in a text where the URL does not parse (`urlForText`,
 *  `requestForText`). The configuration refuses such a URL before a client is made
 *  (`dispatch-config.ts`). */
const DISPATCH_URL_SETTING = "the configured Dispatch URL";

export type {
  CredentialPendingResponse,
  CredentialPendingRow,
  CredentialSession,
  DispatchServiceErrorShape,
} from "@legion/contracts";

/** The stored bytes of one version of an uploaded file, and the MIME type Dispatch serves them as. */
export interface ArtifactFileVersion {
  readonly mime: string;
  readonly bytes: Uint8Array;
}

/**
 * What `readRoute` and `sendRoute` answer: `payload`, Dispatch's own JSON (a 2xx typed JSON whose
 * body parses and that carries no `Content-Disposition: attachment`), or, for every other 2xx,
 * `body`, the bytes that came, with `type`, the answer's Content-Type where it named one. A file
 * version answers its bytes under its own MIME type, a document version read with
 * `?download=true` its Markdown, both as an attachment, which is stored content whatever its type
 * (JSON included) and is never parsed; a bodiless 204 or 205 answers no bytes.
 */
export type RouteAnswer =
  | { readonly payload: unknown }
  | { readonly body: Uint8Array; readonly type: string | undefined };

/** The members of Dispatch's refusal beside its code and its message that `DispatchServiceError`
 *  keeps: one list, which the refusal reader (`DispatchRefusal`) and the error's own details
 *  both name, so a member one of them carries cannot be dropped by the other. */
type RefusalMember = "candidates" | "changed" | "current" | "hint" | "issues" | "mismatches";

type RefusalMembers = { readonly [Member in RefusalMember]: DispatchServiceErrorShape[Member] };

export class DispatchServiceError extends Error {
  override readonly name = "DispatchServiceError";
  /**
   * Whether Dispatch itself wrote this refusal, so its status and code say what Dispatch decided;
   * false for an answer something in Dispatch's place wrote (`DispatchGatewayError`).
   */
  readonly fromDispatch: boolean = true;
  readonly candidates: DispatchServiceErrorShape["candidates"] | undefined;
  readonly current: DispatchServiceErrorShape["current"] | undefined;
  readonly mismatches: DispatchServiceErrorShape["mismatches"] | undefined;
  /** A `409 PARENT_STATUS_CONFLICT`'s issues: each one the write would leave out of line. */
  readonly issues: DispatchServiceErrorShape["issues"] | undefined;
  /** A status change's refusal: the entry of each step that wrote before it. */
  readonly changed: DispatchServiceErrorShape["changed"] | undefined;
  /**
   * Whether Dispatch marked the answer as its refusal of a text for the text alone
   * (`TEXT_REFUSAL_HEADER`), whatever its status: the refusals of the two markdown round trips,
   * inline and of a file, which are an upload's refusals of the same text. False for any other
   * answer.
   */
  readonly textRefusal: boolean;
  /** Dispatch's `hint`, where to look next, which only its answer to a path no route serves
   *  carries (`DispatchServiceErrorShape["hint"]`); `messageFor` prints it after the message. */
  readonly hint: string | undefined;

  constructor(
    readonly code: string,
    readonly status: number,
    message: string,
    /** The members of Dispatch's refusal beside its message that a caller reads off the error:
     *  its answer's JSON, or another refusal whose members it keeps. */
    details: { readonly [Member in RefusalMember]?: RefusalMembers[Member] | undefined } & {
      readonly textRefusal?: boolean | undefined;
    } = {},
    /** The wait the answer's `Retry-After` names, and null where it names none. */
    readonly retryAfterMs: number | null = null
  ) {
    super(message);
    this.candidates = details.candidates;
    this.current = details.current;
    this.mismatches = details.mismatches;
    this.issues = details.issues;
    this.changed = details.changed;
    this.textRefusal = details.textRefusal ?? false;
    this.hint = details.hint;
  }
}

/**
 * An answer that is not Dispatch's JSON, so whatever answers in its place (a proxy, a load
 * balancer, an auth gateway, a captive portal) wrote it: a non-2xx, or a 2xx that a read, a PUT or
 * a DELETE of a route this client reads as JSON could not read as JSON (`#response`). A keyed
 * write's 2xx is the write table's (`answerEvent`), and `readRoute` and `sendRoute`, which send a
 * route their caller wrote, give a 2xx's body that is not Dispatch's JSON back as it came
 * (`#route`). `answer` says what came back and `advice` what asking again can do, judged from the
 * method and the status alone; the message joins them. A caller that knows more about what its
 * own request did gives `answer` advice of its own instead. `code` is `HTTP_<status>`, as for any
 * refusal that names no code.
 */
export class DispatchGatewayError extends DispatchServiceError {
  override readonly fromDispatch = false;
  /** Whether asking again can clear it: a 5xx, a timeout (408), a rate limit (429), or a 2xx
   *  something in Dispatch's place wrote (a portal or a proxy in the way, which a later request
   *  can get past). */
  readonly transient: boolean;
  /**
   * Whether the request may have reached Dispatch before the gateway answered, so a write may
   * have taken effect (`mayHaveReachedServer`: after a 5xx or a 408).
   */
  readonly mayHaveReachedDispatch: boolean;

  constructor(
    status: number,
    /**
     * `<METHOD> <url> answered <status>[ <reason>] with <body>, which looks like a proxy or
     * gateway page rather than Dispatch's own answer`, the URL as `requestForText` names it.
     */
    readonly answer: string,
    /** What asking again can do, as a clause that follows "so", without a final stop. */
    readonly advice: string,
    message = `${answer}, so ${advice}.`
  ) {
    super(`HTTP_${status}`, status, message);
    this.transient = gatewayClears(status);
    this.mayHaveReachedDispatch = mayHaveReachedServer(status);
  }
}

/** The two headers Cipher's proxy writes on an answer of its own (`proxy/record.go`, `ownAnswer`):
 *  its code, and whether the request may have left the proxy (`false`, `unknown`, or `true` on a
 *  service's answer it carried, which has no code). Every write reads them (`proxyOwnAnswer`): a
 *  person's review goes through the proxy (`reviewArtifact`), and a CI job's result write goes
 *  through it as the secrets broker's airlock (`airlockCredential`), named by the job's
 *  `HTTPS_PROXY`, which this client does not see. */
const PROXY_CODE_HEADER = "Cipher-Proxy-Code";
const PROXY_FORWARDED_HEADER = "Cipher-Proxy-Forwarded";

/**
 * A write Cipher's proxy answered on its own (`Cipher-Proxy-Code`): a person's review sent through
 * the proxy (`reviewArtifact`) refused or failed there. `code` and `status` are the proxy's. It is
 * what the write throws only while no attempt may have reached Dispatch: then nothing did, and a
 * new `dispatch review` trades a new grant. Once an attempt may have reached Dispatch, the write
 * ends as `DispatchWriteUnsettledError` with this as its `last`, whatever the proxy's last answer
 * says (`#proxyRefusal`). The advice is never the Dispatch URL's.
 */
export class DispatchProxyRefusalError extends DispatchServiceError {
  override readonly fromDispatch = false;
}

/** A `Cipher-Proxy-Code` as the proxy writes one (`proxy.go`, `record.go`): an upper-case word of
 *  at most 64 characters. A CONNECT answer comes over the plain-HTTP hop to the proxy, so a code of
 *  any other form is not taken into an error's code or text. */
const PROXY_CODE_FORM = /^[A-Z][A-Z0-9_]{0,63}$/;

/** An answer Cipher's proxy wrote itself (`proxyOwnAnswer`): its `Cipher-Proxy-Code`, or
 *  `HTTP_<status>` where it names none, and whether the request may have left the proxy first. */
interface ProxyOwnAnswer {
  readonly code: string;
  readonly left: boolean;
}

/**
 * `response` as an answer Cipher's proxy wrote itself inside the tunnel, or undefined where it is
 * none: one that says `Cipher-Proxy-Forwarded: false` was written before any byte of the request
 * left the proxy, so the request reached no service (`left: false`); one that says `unknown`, or
 * names a `Cipher-Proxy-Code` without saying `false`, was written after the request may have
 * reached Dispatch (`left: true`). A service's answer the proxy carried says `true` and names no
 * code, and Dispatch writes neither header, so both are read as Dispatch's. The one reading of
 * these headers, for every write; a CONNECT the proxy refused is `AirlockTunnelRefusedError`.
 */
function proxyOwnAnswer(response: Response): ProxyOwnAnswer | undefined {
  const forwarded = response.headers.get(PROXY_FORWARDED_HEADER)?.trim().toLowerCase();
  const named = response.headers.get(PROXY_CODE_HEADER)?.trim() ?? "";
  if (forwarded !== "false" && forwarded !== "unknown" && named === "") return undefined;
  return { code: named === "" ? `HTTP_${response.status}` : named, left: forwarded !== "false" };
}

/**
 * The table's event (write-outcome.ts) for an answer Cipher's proxy wrote itself with `status`,
 * inside the tunnel (`proxyOwnAnswer`) or to the CONNECT (`AirlockTunnelRefusedError`, always
 * before anything left it). Once the request may have left the proxy (`left`), the write may have
 * landed and is asked again under its key (`proxy-unsettled`). Before, nothing reached Dispatch:
 * an outage (`outage`: a 408, a 429 or a 5xx) is the proxy failing (a grant it could not read
 * this moment, a dial to Dispatch that failed, a wait it gave up), asked again under the key as a
 * connection that never opened is (`proxy-failed`), and any other status is its refusal, which
 * the same placeholder meets again (`proxy-refused`).
 */
function proxyEvent(status: number, left: boolean): WriteEvent {
  if (left) return "proxy-unsettled";
  return outage(status) ? "proxy-failed" : "proxy-refused";
}

/** The code of `DispatchCredentialEchoError`, in Dispatch's form: no status of its own, since any
 *  2xx can carry such a body. */
export const CREDENTIAL_ECHOED = "CREDENTIAL_ECHOED";

/** A client refusal of a 2xx that holds a credential this command presented. `whoami` returns
 *  identity metadata, and `POST /api/v1/me/agent-tokens` returns a newly minted token. Neither
 *  echoes the caller's credential. Quote nothing from the request or body in this refusal. */
export class DispatchCredentialEchoError extends DispatchServiceError {
  override readonly fromDispatch = false;

  constructor(status: number) {
    super(
      CREDENTIAL_ECHOED,
      status,
      `The answer had HTTP status ${status} (2xx), so a write may have been made. The body held a credential this command presented. It is not printed. Something in Dispatch's place appears to have echoed the request. Check whether any write took effect before sending it again.`
    );
  }
}

/**
 * A write that ended, once an attempt may have landed (write-outcome.ts), without a 2xx Dispatch
 * wrote, read whole (not an empty, untyped or non-JSON one; a bodiless 204 or 205 is one; not one
 * that names no key back or another request's, `keyNamedBack`), its `409 KEY_COMMITTED` or a
 * final refusal, so it may or may not have taken effect.
 * A final refusal names exactly this request's key back and is an answer marked
 * `Idempotency-Replayed: ?1` (including a recovery replay), `422 KEY_REUSED`, or a fresh answer
 * with a 4xx Dispatch stores (not 401, 408 or 429) and no keyed-layer code (`answerEvent`).
 * Everything else — a 5xx, a 401, a 408 or 429, `409 KEY_IN_FLIGHT`, a broken body, a 2xx that is
 * no answer of Dispatch's, a page, a 3xx, a refusal before the key claim, an answer naming another
 * key or none, a credential failure, a silence cut, the deadline or the caller's stop — ends here.
 * This is deliberately no `DispatchServiceError`, which a caller reads as a refusal Dispatch
 * decided. `last` (also the `cause`) is what ended it. `key` is the `Idempotency-Key` every
 * attempt of the write carried, each attempt that may have landed as one identity: a resend under
 * it, as that identity, is answered from what Dispatch stored, never written twice. The message
 * always says Dispatch finishes an attempt it received, and that a resend is a new command's,
 * under a new key, so the write is to be read back first.
 */
export class DispatchWriteUnsettledError extends Error {
  override readonly name = "DispatchWriteUnsettledError";

  constructor(
    readonly last: Error,
    readonly attempts: number,
    /** The `Idempotency-Key` every attempt of the write carried. */
    readonly key: string,
    /**
     * What the write got, as the message's first clause, where `last` is no refusal that says it:
     * `<METHOD> <url>` (the URL as `requestForText` names it) and how it ended. `last`'s own
     * message otherwise.
     */
    unanswered?: string
  ) {
    let answer: string;
    // A 5xx or a 408 can itself come after Dispatch committed the write (`mayHaveReachedServer`),
    // and so can a refusal Cipher's proxy wrote itself (`DispatchProxyRefusalError`), whose own
    // text names the attempt that may have left the proxy; any other refusal says the landing was
    // an earlier attempt's.
    let why =
      (last instanceof DispatchServiceError && mayHaveReachedServer(last.status)) ||
      last instanceof DispatchProxyRefusalError
        ? "an attempt of the write may have reached Dispatch"
        : "an earlier attempt of the write may have reached Dispatch";
    if (last instanceof DispatchGatewayError) {
      answer = last.answer;
      // A 2xx that is not Dispatch's answer to the write (`#writePayload`) came from whatever
      // answers in its place, which may have passed the write on before it answered.
      if (last.status < 300) why = "the write may have reached Dispatch behind what answered it";
    } else if (last instanceof DispatchServiceError && last.code === IDEMPOTENCY_KEY_IN_FLIGHT) {
      // Dispatch's own text tells the caller to ask again under the same key, which the client
      // already did and a person or agent reading this cannot: the code alone names the answer.
      answer = last.code;
      why = "Dispatch was still running an earlier attempt of the write";
    } else if (last instanceof DispatchServiceError) {
      answer = `${last.code}: ${last.message}`;
    } else {
      answer = unanswered ?? last.message;
      why = "an attempt of the write may have reached Dispatch";
    }
    const tried = attempts === 1 ? "its only attempt" : `the last of ${attempts} attempts`;
    super(
      `${answer} (${tried}), and ${why}, so it may or may not have taken effect. Dispatch ` +
        "finishes an attempt it received whatever this command does, so read the write back " +
        "before you send it again: a new command sends it under a new key.",
      { cause: last }
    );
  }
}

/**
 * A write Dispatch made whose answer is gone, so the caller cannot be shown what it made: a repeat
 * under its key answered `409 IDEMPOTENCY_KEY_COMMITTED` (the write landed before its answer was
 * lost, and Dispatch will not run it again), a replayed 2xx reading
 * `IDEMPOTENCY_ANSWER_NOT_KEPT` (Dispatch gives no copy of its first answer: one larger than the
 * 1 MiB a key keeps, or a success that holds a comment or a message that was deleted, or a
 * document version name a delete replaced, which would give back text the delete withdrew), or a
 * 2xx that names the call's key back and whose body gives no row (`DISPATCH_ANSWER_LOST`,
 * `#writePayload`). The write took effect, so this is no refusal and deliberately no
 * `DispatchServiceError`; it is never asked again. Its message is the one line a session reads:
 * what was made, why its answer is gone, and to read it back. A call that writes nothing
 * (`rowStores`) made nothing, so its answer that is not its row is no landing: its replayed
 * `IDEMPOTENCY_ANSWER_NOT_KEPT` is a `DispatchAnswerNotKeptError`, and its other such answers,
 * its `409 KEY_COMMITTED` naming its key back among them, a `DispatchAnswerMissingError`.
 */
export class DispatchWriteLandedError extends Error {
  override readonly name = "DispatchWriteLandedError";

  constructor(
    readonly code: DispatchLandedCode,
    /** The status Dispatch answered with: the 409, or the 2xx whose answer is gone. */
    readonly status: number,
    method: string,
    /** The request's URL as a text names it (`requestForText`). */
    shownUrl: string,
    options?: ErrorOptions
  ) {
    const lost =
      code === IDEMPOTENCY_ANSWER_NOT_KEPT
        ? "Dispatch gives no copy of its answer, which was too large to keep or holds a comment " +
          "or a message that was deleted, or a document version name a delete replaced"
        : "its answer was lost on the way back";
    const request = `${method} ${shownUrl}`;
    super(
      `${request} was made, but ${lost} (${code}), so this command cannot print what it ` +
        "made: read it back with `dispatch read`, or with the matching list for something it created.",
      options
    );
  }
}

/**
 * A call that writes nothing (`markdownRoundTrip`, `fileRoundTrip`, `related`, `dryRunResult`)
 * whose repeat under its key Dispatch answered with a replayed 2xx reading
 * `IDEMPOTENCY_ANSWER_NOT_KEPT`: Dispatch kept no copy of the answer, and the call made nothing, so
 * there is nothing to read back and no landing. The client asks such a call once more under a new
 * key before it throws this (`#writingNothing`); this is what reaches the caller when that ask is
 * answered the same way. It is no refusal, so it is no `DispatchServiceError`, and no
 * `DispatchWriteLandedError`, which says a write was made. Its message is the one line a session
 * reads: the call wrote nothing, Dispatch kept no copy of its answer, and to run the command
 * again, which asks under a new key.
 */
export class DispatchAnswerNotKeptError extends Error {
  override readonly name = "DispatchAnswerNotKeptError";
  readonly code = IDEMPOTENCY_ANSWER_NOT_KEPT;

  constructor(
    /** The first answer's own 2xx, which the replay carries. */
    readonly status: number,
    method: string,
    /** The request's URL as a text names it (`requestForText`). */
    shownUrl: string
  ) {
    super(
      `${method} ${shownUrl} wrote nothing, and Dispatch kept no copy of its answer ` +
        `(${IDEMPOTENCY_ANSWER_NOT_KEPT}), so this command has no answer to print: run it again, ` +
        "which asks under a new key."
    );
  }
}

/** `value`, a parsed body, as Dispatch's error shape: its members where it is an object, and none
 *  where it is not, each read unchecked. A body is Dispatch's own refusal where its `error` is a
 *  string (`refusalIn`). */
function asErrorShape(value: unknown): DispatchServiceErrorShape {
  return typeof value === "object" && value !== null ? (value as DispatchServiceErrorShape) : {};
}

/**
 * Whether an answer's `Content-Type` names a JSON media type: `application/json`, or one ending in
 * `+json`, its parameters left out and letter case aside, as Dispatch's own rule reads it
 * (`compressibleAnswer`, packages/envoy/internal/dispatch/api/response_compression.go).
 */
function isJson(response: Response): boolean {
  const [type = ""] = (response.headers.get("content-type") ?? "").split(";");
  const mediaType = type.trim().toLowerCase();
  return (
    mediaType === "application/json" ||
    (mediaType.length > "+json".length && mediaType.endsWith("+json"))
  );
}

/**
 * Whether an answer's `Content-Disposition` names the disposition type `attachment`, with or
 * without parameters and letter case aside, as RFC 6266 reads it. Dispatch sets it on every
 * stored file version and on a document version read with `?download=true`
 * (`attachmentDisposition`, packages/envoy/internal/dispatch/api/artifact_files.go): stored
 * content, whatever its Content-Type says.
 */
function isAttachment(response: Response): boolean {
  const [type = ""] = (response.headers.get("content-disposition") ?? "").split(";");
  return type.trim().toLowerCase() === "attachment";
}

/** Check decoded JSON keys and strings as well: an escape in the source can hide a credential
 *  the CLI would print after parsing. Use a stack so deeply nested JSON cannot overflow a
 *  recursive check. */
function jsonHoldsCredential(payload: unknown, credentials: ReadonlySet<string>): boolean {
  const pending: unknown[] = [payload];
  while (pending.length > 0) {
    const value = pending.pop();
    if (typeof value === "string") {
      for (const credential of credentials) {
        if (value.includes(credential)) return true;
      }
    } else if (value !== null && typeof value === "object") {
      const members = value as Record<string, unknown>;
      for (const key in members) {
        if (!Object.hasOwn(members, key)) continue;
        for (const credential of credentials) {
          if (key.includes(credential)) return true;
        }
        pending.push(members[key]);
      }
    }
  }
  return false;
}

/** Whether `error` is a Dispatch process's answer that it has no route for the request (`404
 *  NOT_FOUND`, `NO_ROUTE_MESSAGE_PREFIX`), as a process older than a route answers it; a `404
 *  NOT_FOUND` for a resource the server does not know says "not found" instead. */
export function isNoRoute(error: unknown): boolean {
  return (
    error instanceof DispatchServiceError &&
    error.status === 404 &&
    error.code === "NOT_FOUND" &&
    error.message.startsWith(NO_ROUTE_MESSAGE_PREFIX)
  );
}

/**
 * A request every attempt of which a Dispatch process answered with no route for it (`404
 * NOT_FOUND`, "no route for …"). While a release rolls out, two builds of Dispatch answer, and
 * the older one answers this way for a route only the newer one serves. A server older than a
 * route answers it this way too, so a caller with an older route to fall back on catches this
 * error and asks that route.
 */
export class DispatchNoRouteError extends DispatchServiceError {
  constructor() {
    super("NOT_FOUND", 404, NO_ROUTE_ROLLING_RELEASE_MESSAGE);
  }
}

/**
 * A request that got no answer from Dispatch at the transport, named by the Dispatch URL as
 * `urlForText` names it and the failure's class (`DispatchClient`'s `#fetch`). `reachedNothing`
 * says the failure sent nothing to any server (a refused connection, a name that did not resolve):
 * a write attempt that failed so cannot have landed, where one that failed otherwise (a reset after
 * Dispatch read the request) may have, since Dispatch runs a keyed write it received to its end.
 * `failure` is the failure's class alone (`errorClass`), for a text that names another path to
 * Dispatch (a write through Cipher's proxy, `#proxyUnreachable`), and `code` is that class where it
 * is a code alone, such as `ECONNRESET`, which a caller names an unknown outcome by (a CI job's
 * `dispatch result`'s `write_outcome_unknown`), and undefined where it is not.
 */
export class DispatchUnreachableError extends Error {
  readonly code: string | undefined;

  constructor(
    message: string,
    readonly reachedNothing: boolean,
    readonly failure: string
  ) {
    super(message);
    this.code = /^[A-Z][A-Z0-9_]*$/.test(failure) ? failure : undefined;
  }
}

/**
 * What `reviewArtifact` throws once the Dispatch at `baseUrl` answered no route for `route` (the
 * method and path it sent the review to) each time it was sent. The code and status are that
 * answer's, so a caller reads it as the refusal it is. The words are not
 * `DispatchNoRouteError`'s: a build without the versioned review route may simply be older than
 * that route rather than half of a release in progress, and no other review route is asked, so
 * the message says why. Dispatch's no-route answer carries only a code, an error and a hint
 * (`staticHandler`, packages/envoy/internal/dispatch/routes/router.go) and names no build, so it
 * points at `/healthz`, whose `commit` member names the commit of the build that answers it when
 * that build was stamped with one and is null otherwise (`buildCommit` and `healthzHandler`,
 * packages/envoy/cmd/dispatch/main.go), rather than guessing.
 */
function reviewNoRouteRefusal(
  baseUrl: string,
  route: string,
  version: number
): DispatchServiceError {
  const shown = urlForText(baseUrl, DISPATCH_URL_SETTING);
  // Its health route, by the URL where that prints, and by its path on that server otherwise.
  const health = isCleanUrl(baseUrl) ? `${baseUrl}/healthz` : "/healthz on that server";
  return new DispatchServiceError(
    "NOT_FOUND",
    404,
    `Dispatch at ${shown} answered no route for ${route} each time the review was sent, and an ` +
      "answer of no route records no review. This build is older than that route. No other review " +
      "route was asked: the review route an older build serves takes no version, and it " +
      `would record the review at a version that build chooses, which may not be version ${version}. ` +
      `The no-route answer does not name the build; GET ${health} names the commit of the ` +
      "build that answers it, when that build was stamped with one."
  );
}

/**
 * What `dryRunResult` throws once the Dispatch at `baseUrl` answered no route for `route` each time
 * it was sent: a build older than a result's dry run. The code and status are that answer's. The
 * record's own route is never asked in its place, since a build that drops a field or a query it
 * does not know would record the result, so the message says that nothing was recorded.
 */
function resultDryRunNoRouteRefusal(baseUrl: string, route: string): DispatchServiceError {
  return new DispatchServiceError(
    "NOT_FOUND",
    404,
    `Dispatch at ${urlForText(baseUrl, DISPATCH_URL_SETTING)} answered no route for ${route} each ` +
      "time the dry run was sent: this build is older than a result's dry run, so it cannot show " +
      "the pull-request body line before the record. Nothing was recorded."
  );
}

/**
 * What `changeIssueStatus` throws once the Dispatch at `baseUrl` answered no route for `route` each
 * time the write was sent: a build older than the status change route. The code and status are
 * that answer's. The PATCH is never sent in its place, since a PATCH takes no option and would
 * write the status without what the option asks for, so the message says that nothing changed.
 */
function statusChangeNoRouteRefusal(baseUrl: string, route: string): DispatchServiceError {
  return new DispatchServiceError(
    "NOT_FOUND",
    404,
    `Dispatch at ${urlForText(baseUrl, DISPATCH_URL_SETTING)} answered no route for ${route} each ` +
      "time the status change was sent. This build is older than that route, so it takes no " +
      "include_children, unlink_children or include_parent, and nothing changed. No PATCH was " +
      "sent in its place, since a PATCH would write the status without the option."
  );
}

export interface ListIssuesOptions {
  readonly project?: string;
  readonly status?: string;
  readonly parent?: string;
  readonly label?: string;
  /** Each value repeats as `priority=`; `"none"` matches an issue with no priority. */
  readonly priority?: readonly (IssuePriority | "none")[];
  readonly updated_since?: string;
  /** Only open issues whose route is in this state. */
  readonly route_status?: IssueRouteStatus;
}

/** The page `listIssuePage` asks for: `limit` issues from position `offset` of the listing. */
export interface IssuePageRequest {
  readonly limit: number;
  readonly offset: number;
}

/** What the `dispatch search` command narrows by. It carries none of the five members the search
 *  bar sends (`number_project`, `status`, `author`, `updated_since`, `keywords_only`): those
 *  belong to the bar's own feature, and the command cannot narrow by them today. `search` reads
 *  the request's keys rather than this shape, so the day one is added here it is routed to
 *  `GET /api/v1/search/results` with nobody remembering to. */
export interface SearchOptions {
  readonly project?: string;
  readonly limit?: number;
  readonly offset?: number;
  /** The kinds to rank, count and page (`SEARCH_KINDS`): `#url` writes one `kind=` per entry,
   *  which is how the route reads it. With none named the search reads every kind it indexes. */
  readonly kind?: readonly SearchResultKind[];
}

/** One node's edges: `to` reads what points at it, `from` what it points to; exactly one. */
export type GraphReferencesQuery = ({ readonly to: string } | { readonly from: string }) & {
  readonly kind?: readonly GraphEdgeKind[];
  /** events.id; keeps mentions introduced after it and excludes structural edges. */
  readonly since?: number;
};

/** Which of an issue's events `read` returns, oldest first: the newest `events`, or every one. */
export interface IssueReadOptions {
  /** `DEFAULT_ISSUE_READ_EVENTS` when absent or undefined. */
  readonly events?: number | "all" | undefined;
}

/** The most events one `GET /api/v1/issues/{key}/events` page holds: Dispatch refuses a larger
 *  `limit` (`parseEventLimit` in packages/envoy/internal/dispatch/api/events.go). */
const ISSUE_EVENTS_PAGE_MAX = 200;

/** A client's own deadline, counted from when it was made, where its maker names none. */
export const DISPATCH_TOOL_DEADLINE_MS = 60_000;

function requestSignal(signal: AbortSignal | undefined, deadlineMs: number): AbortSignal {
  const deadline = AbortSignal.timeout(deadlineMs);
  return signal === undefined ? deadline : AbortSignal.any([signal, deadline]);
}

/**
 * How a write asks Dispatch again under its key while its attempts get no definitive answer:
 * `pausesMs` is the wait before each retry in turn, so the write makes at most one attempt more than
 * it has entries, and no retry starts once `withinMs` has passed since the first attempt began. An
 * answer's `Retry-After` longer than the scheduled pause is waited instead. An attempt that goes
 * `silenceMs` without progress before its answer begins - no read of its body in that time - is
 * cut off and asked again like one that failed in transport, so a write Dispatch is slow to finish
 * is asked again under its key (and answered `409 KEY_IN_FLIGHT`, then replayed) rather than
 * waited on for the call's whole deadline. A body of `STREAMED_BODY_MIN` or more is read a chunk at
 * a time as the connection's buffers take it, and they hold megabytes, so an upload on a slow line
 * can be cut off while it is still going out: when its next read, or the rest of its bytes and the
 * answer after its last read, take longer than `silenceMs` (README, "Dispatch"). Once an attempt of
 * a body under `STREAMED_BODY_MIN` was cut off for silence or answered `409 KEY_IN_FLIGHT`, Dispatch
 * may still be running the write, and its keyed layer finishes that run whatever the client does,
 * so the write keeps asking until it gets the run's stored answer:
 * every `inFlightPauseMs` at most, unless a shorter pause of the schedule's or a longer
 * `Retry-After` applies, with no retry starting once `inFlightWithinMs` has passed. An upload keeps
 * the schedule alone, since each repeat sends its whole body again. No retry of any write starts
 * more than `inFlightWithinMs` after the client was made, since the client's deadline counts from
 * there. `pause` and `now` are the timer and the monotonic clock, which a test replaces.
 */
export interface DispatchWriteRetry {
  readonly pausesMs: readonly number[];
  readonly withinMs: number;
  readonly silenceMs: number;
  readonly inFlightPauseMs: number;
  readonly inFlightWithinMs: number;
  /** Waits `ms`, or rejects with the signal's reason once it aborts. */
  readonly pause?: (ms: number, signal: AbortSignal) => Promise<void>;
  readonly now?: () => number;
}

/**
 * At most five attempts, 1, 2, 4 and 8 s apart, none starting more than 30 s after the first, so a
 * write whose every attempt fails at once settles in about 15 s and a `Retry-After` can stretch that
 * to 30 s. A write Dispatch may still be running is asked every 3 s at most (longer only when a
 * `Retry-After` asks) until 55 s, so an attempt cut off for silence at 20 s still has 35 s in which
 * Dispatch can finish it, two silent attempts and the ask that collects the answer fit, and the
 * last ask has 5 s before the client's 60 s deadline.
 *
 * Measured from the transcript store over 3,612 `dispatch` write commands that succeeded between
 * 2026-10-31 21:00Z and 2026-11-02 07:30Z, Dispatch's busiest days so far: p50 1.8 s, p90 10.5 s,
 * p95 19.6 s, p99 45.5 s, p99.9 62 s, each command timed whole, process start included, so an
 * upper bound on the write itself. 20 s of silence is that p95, and two silent attempts and the
 * ask that collects the answer still fit in 55 s (20 + 1 + 20 + 2 = 43 s). Cutting off a healthy
 * write costs nothing: Dispatch's keyed layer goes on running it and replays its answer.
 */
export const DISPATCH_WRITE_RETRY: DispatchWriteRetry = {
  pausesMs: [1_000, 2_000, 4_000, 8_000],
  withinMs: 30_000,
  silenceMs: 20_000,
  inFlightPauseMs: 3_000,
  inFlightWithinMs: 55_000,
};

function pauseFor(ms: number, signal: AbortSignal): Promise<void> {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  if (signal.aborted) {
    reject(signal.reason);
    return promise;
  }
  const stop = (): void => {
    clearTimeout(timer);
    reject(signal.reason);
  };
  const timer = setTimeout(() => {
    signal.removeEventListener("abort", stop);
    resolve();
  }, ms);
  signal.addEventListener("abort", stop, { once: true });
  return promise;
}

/** Whether `response` names back the key its request sent, which says the answer came through
 *  Dispatch's keyed layer. An empty key names no request, so no answer names it back: an answer
 *  that names no key is not this call's for a caller that gave an empty key of its own. */
function keyEchoed(response: Response, key: string): boolean {
  return key !== "" && keyNamedBack(response) === key;
}

/** A request body encoded once, so every attempt of a write sends the bytes the first one did. */
interface EncodedBody {
  readonly content: string | Uint8Array<ArrayBuffer>;
  readonly type: string;
}

/** A request's answer and the credential headers sent to get it, including any stale attempt. */
interface SentRequest {
  readonly answer: Response;
  readonly presented: Readonly<Record<string, string>>;
  readonly stalePresentation?: Readonly<Record<string, string>>;
}

/**
 * The size from which an attempt's body goes out as a stream read a chunk at a time
 * (`STREAMED_BODY_CHUNK`), so each read of a chunk counts as the attempt's progress.
 * A smaller body leaves in one write to the connection, so there is no progress of it to see.
 */
const STREAMED_BODY_MIN = 1 << 20;
const STREAMED_BODY_CHUNK = 64 << 10;

/**
 * What one attempt sends for `body`: its bytes as they are when they are smaller than
 * `STREAMED_BODY_MIN`, or else a stream of them that calls `progress` each time fetch reads a chunk
 * (fetch reads a stream body only as the connection's buffers take it), with the `Content-Length`
 * they add up to, so the request on the wire is the one the bytes make.
 */
function attemptBody(
  body: EncodedBody,
  headers: Record<string, string>,
  progress: () => void
): Pick<RequestInit, "body" | "headers"> {
  const bytes =
    typeof body.content === "string" ? new TextEncoder().encode(body.content) : body.content;
  if (bytes.byteLength < STREAMED_BODY_MIN) return { body: body.content, headers };
  let sent = 0;
  const stream = new ReadableStream<Uint8Array>({
    pull(controller) {
      progress();
      if (sent >= bytes.byteLength) {
        controller.close();
        return;
      }
      const end = Math.min(sent + STREAMED_BODY_CHUNK, bytes.byteLength);
      controller.enqueue(bytes.subarray(sent, end));
      sent = end;
    },
  });
  return { body: stream, headers: { ...headers, "Content-Length": String(bytes.byteLength) } };
}

/** An attempt cut off after `DispatchWriteRetry.silenceMs` without progress before its answer. */
class WriteAttemptSilent extends Error {
  override readonly name = "WriteAttemptSilent";

  constructor(silenceMs: number) {
    super(`made no progress for ${silenceMs / 1_000} s, so it was cut off`);
  }
}

/** What a write's attempts have shown, apart from its table state (`WriteState`): what the
 *  pacing and the unsettled message read. */
interface WriteProgress {
  /** How many attempts went out: a credential that could not present one sent nothing. */
  sent: number;
  /** Whether Dispatch may still be running an attempt: one was cut off for silence (an upload's
   *  excepted, which keeps the schedule) or answered 409 KEY_IN_FLIGHT. The pacing asks again
   *  sooner and for longer while it is. */
  running: boolean;
}

/** A POST or PATCH call: its one `Idempotency-Key`, its row (whether it stores anything is the
 *  row's, `rowStores`), the request as a text names it (`requestForText`), and what its attempts
 *  have shown. */
interface WriteCall {
  readonly key: string;
  readonly row: WriteRow;
  readonly request: string;
  readonly progress: WriteProgress;
}

/** How a write goes through Cipher's proxy (`#throughProxy`): the credential every attempt
 *  presents in the client's own credential's place, which answers `Authorization: DPoP
 *  <placeholder>` and has no identity and no `stale`, and the transport members of each request —
 *  the proxy, its user info the placeholder, and the one authority this request trusts. */
interface ProxyTransport {
  readonly credential: RequestCredential;
  readonly init: { readonly proxy: string; readonly tls: { readonly ca: string } };
}

/** `url`'s scheme and host alone, for a text that names it: never its user info, which for the
 *  proxy is the placeholder. */
function originOf(url: string): string {
  const parsed = new URL(url);
  return `${parsed.protocol}//${parsed.host}`;
}

/** One attempt's answer to a write, its body read whole inside the attempt (`writeAnswer`). */
interface WriteAnswer {
  /** The answer's status and headers: its body is read, and is `text`. */
  readonly response: Response;
  /** Whether its `Content-Type` says JSON and the body read whole is not JSON: an answer cut
   *  short on the way. */
  readonly malformed: boolean;
  /** Whether it is a status that carries a body and its body is empty or its `Content-Type` does
   *  not say JSON. Every answer Dispatch writes to a keyed write is JSON or a bodiless 204 or 205
   *  (`WriteJSON`, packages/envoy/internal/dispatch/api/server.go), so such a 2xx is no answer of its own. */
  readonly notJson: boolean;
  /** The body as read: "" for a status that carries none. */
  readonly text: string;
  /** The body parsed, where it is typed JSON and parses (`parsedJson`). */
  readonly json: ParsedJson;
  /** Dispatch's own refusal's code and text (its error JSON, with a string `error`), where the
   *  answer is one. */
  readonly code: string | undefined;
  readonly error: string | undefined;
  /** Whether Dispatch replayed the stored answer or a recovery answer under this key. */
  readonly replayed: boolean;
  /** Whether it names exactly the request's key back (`keyEchoed`), as every answer of Dispatch's
   *  keyed layer to a keyed write does. */
  readonly echoed: boolean;
  /** What it is as an answer Cipher's proxy wrote itself (`proxyOwnAnswer`), where it is one. */
  readonly proxy: ProxyOwnAnswer | undefined;
}

/** A body parsed as JSON, or undefined where it is empty, not typed JSON, or not JSON. */
type ParsedJson = { readonly value: unknown } | undefined;

/** `text`, the body of `response`, parsed where it is typed JSON (`isJson`) and not empty. */
function parsedJson(response: Response, text: string): ParsedJson {
  if (text === "" || !isJson(response)) return undefined;
  try {
    return { value: JSON.parse(text) };
  } catch {
    return undefined;
  }
}

/** Dispatch's own refusal, as an answer carries it: its code, where it names one, its text, and
 *  the members some refusals carry beside them, which `DispatchServiceError` keeps. */
interface DispatchRefusal extends RefusalMembers {
  readonly code: string | undefined;
  readonly error: string;
}

/**
 * Dispatch's own refusal in `json`, an answer's body parsed (`parsedJson`), or undefined where the
 * answer is none: its body is typed JSON (`isJson`) that parses to an object whose `error` is a
 * string, as Dispatch writes every refusal, and its `code` counts only where it is a string. The
 * rule `writeAnswer` and `#payloadOf` read a refusal by; `tokenNotLive` reads a 401's code alone,
 * whatever its `error`.
 */
function refusalIn(json: ParsedJson): DispatchRefusal | undefined {
  const shape = asErrorShape(json?.value);
  if (typeof shape.error !== "string") return undefined;
  return {
    code: typeof shape.code === "string" ? shape.code : undefined,
    error: shape.error,
    candidates: shape.candidates,
    changed: shape.changed,
    current: shape.current,
    hint: typeof shape.hint === "string" ? shape.hint : undefined,
    issues: shape.issues,
    mismatches: shape.mismatches,
  };
}

/**
 * One attempt of a write as the loop reads it (`#write`): the table's event, and what its ending,
 * its resend and the loop's own state are read from. A wait the deadline or the caller's stop
 * ended is one too, with only its event and its stop.
 */
interface WriteAttempt {
  readonly event: WriteEvent;
  /** The answer, its body read whole, where one came. */
  readonly answer?: WriteAnswer;
  /** What ended the attempt where no whole answer came: the credential's failure, another
   *  identity, the transport's, the stop, a body that broke off. */
  readonly failure?: unknown;
  /** The headers the credential answered with, where it answered. */
  readonly presented?: Readonly<Record<string, string>>;
  /** The credential's own failure, where its presentation failed for a reason other than the
   *  call's stop. */
  readonly credentialFailure?: unknown;
  /** The identity the attempt went out as (`RequestCredential.identity`) and its headers, where
   *  it went out: a dead-token answer names those headers to the credential (`stale`). */
  readonly sent?: {
    readonly as: string | undefined;
    readonly with: Readonly<Record<string, string>>;
  };
  /** The answer's `Retry-After`, where headers came even when its body did not. */
  readonly heard?: { readonly retryAfterMs: number };
}

/** The statuses whose response carries no body, which `new Response` refuses one for. */
const NULL_BODY_STATUSES: ReadonlySet<number> = new Set([101, 204, 205, 304]);

/**
 * One attempt's answer to a write, `echoed` saying whether it carried back the request's key
 * (`keyEchoed`) and `proxy` whether Cipher's proxy wrote it itself (`proxyOwnAnswer`), its body
 * read whole here, inside the attempt, so a body the deadline, the caller's stop or a broken
 * connection ends is the attempt's own event (write-outcome.ts), never a failure after the write
 * has settled: a 2xx settles only once its body is read. The body is read and parsed once, here,
 * and the payload is read from that (`#writePayload`), so whoever reads it reads no connection. A
 * status that carries no body (a 204, a 304) is not read; a redirect's body is read as any
 * other's, and its error prints it as one redacted line (`#gatewayError`). A body that fails to
 * read throws the read's failure.
 */
async function writeAnswer(response: Response, echoed: boolean): Promise<WriteAnswer> {
  if (NULL_BODY_STATUSES.has(response.status)) {
    return {
      response,
      malformed: false,
      notJson: false,
      text: "",
      json: undefined,
      code: undefined,
      error: undefined,
      replayed: response.headers.get(IDEMPOTENCY_REPLAYED_HEADER)?.trim() === "?1",
      echoed,
      proxy: proxyOwnAnswer(response),
    };
  }
  const text = await response.text();
  const typed = isJson(response);
  const json = parsedJson(response, text);
  const refusal = response.ok ? undefined : refusalIn(json);
  return {
    response,
    malformed: text !== "" && typed && json === undefined,
    notJson: text === "" || !typed,
    text,
    json,
    code: refusal?.code,
    error: refusal?.error,
    replayed: response.headers.get(IDEMPOTENCY_REPLAYED_HEADER)?.trim() === "?1",
    echoed,
    proxy: proxyOwnAnswer(response),
  };
}

/** Whether `storedStatus` (the Envoy module's `internal/dispatch/api/request_keys.go:1473-1475`)
 *  keeps this answer under the key, which this mirrors: a 4xx that is neither a 401 nor an outage
 *  (`outage`: a 408 or a 429). An outage does reach it: `answerEvent` hands no text refusal to
 *  `retriesUnderKey`, whatever its status, and the keyed layer answers a text refusal with the
 *  key, as it answers every handler's answer to a keyed write. */
function isStoredKeyStatus(status: number): boolean {
  return status >= 400 && status < 500 && status !== 401 && !outage(status);
}

/**
 * The table's event for an answer (`WriteEvent`, write-outcome.ts).
 * - An answer Cipher's proxy wrote itself (`answer.proxy`, `proxyOwnAnswer`) is read by its headers
 *   alone, whatever its status or body (`proxyEvent`): `proxy-refused` for a 4xx and
 *   `proxy-failed` for a 5xx it wrote before any byte left it (`Cipher-Proxy-Forwarded: false`),
 *   and `proxy-unsettled` for one it wrote after the write may have reached Dispatch. A service's
 *   answer it carried names no code, and is read as Dispatch's below.
 * - A 2xx is `applied` once its body was read whole, except where it is no answer of Dispatch's
 *   (`foreign-2xx`): its body is empty or not JSON-typed where its status carries one (`notJson`),
 *   a captive portal's 200 page for one; or it names no key back, or another request's, though
 *   Dispatch's keyed layer names the request's key on every answer it gives a keyed write
 *   (`keyedWrites`, request_keys.go), whatever it holds. A 2xx whose body is not the JSON it says
 *   it is was cut short on the way (`body-malformed`). Dispatch answers a 2xx only after the write
 *   committed, so an applied answer's row is then read from it (`#writePayload`), and a body that
 *   gives none is a landing whose answer was lost.
 * - Two refusals are read by their codes whatever came back with them, since Dispatch makes both
 *   before any handler or its keyed layer runs: its `401 BROKER_TOKEN_NOT_ACTIVE`
 *   (`brokerTokenActor`), a dead token to present afresh where `canTellDeadToken` (the credential
 *   can be told and has not been told yet in this call), else a refusal nothing judged for the
 *   key; and an old build's `404 NOT_FOUND` "no route for …".
 * - An echoed 401 is no outcome of the key: the keyed layer releases the key on it, and it can
 *   come after the handler committed (`storedStatus`, request_keys.go), so the write may have
 *   landed (`uncommitted-401`).
 * - Dispatch's 503 for a token it could not check yet (`tokenNotCheckedAnswer`) asks again later,
 *   as a 5xx, a 408 or a 429 does (`retriesUnderKey`), and `409 KEY_IN_FLIGHT` says an earlier
 *   attempt still runs. A refusal Dispatch marks as its refusal of a text for the text alone
 *   (`TEXT_REFUSAL_HEADER`) is its verdict on that text whatever its status, a 5xx among them:
 *   asking again gets the same answer, so it is never asked again.
 * After those, a non-2xx is final (`judged`) only where it names exactly this request's key back,
 * as every answer of the keyed layer does, and:
 * - it carries `Idempotency-Replayed: ?1`, a stored replay or a recovery replay;
 * - it is `409 KEY_COMMITTED`, a write that landed (`#payloadOf` throws it as landed);
 * - it is `422 KEY_REUSED`: every attempt of a call carries the call's one key (`#writeCall`) and
 *   the bytes encoded once for the call, so a key held under another digest
 *   (`writeRequestFields`, request_keys.go) proves no attempt of this write claimed it;
 * - it is a fresh answer with a 4xx `storedStatus` keeps and none of the keyed layer's own codes:
 *   a handler ran under the claim and its answer is the key's.
 * Every other answer is `unjudged`. A layer before the claim and a build without the keyed layer
 * name no key back. Each rule holds only once every replica runs `b1c42d1d7d`: before it, an
 * attempt could land unkeyed. The key decides only how a final answer reads; every retry goes
 * regardless.
 *
 * An answer Cipher's proxy wrote itself, which `proxyOwnAnswer` reads once for every write
 * (`Cipher-Proxy-Code`), is read before anything else. One it wrote before any byte left it
 * (`Cipher-Proxy-Forwarded: false`) is `proxy-failed` for an outage (`outage`: a 408, a 429 or a
 * 5xx), which is asked again under the key, and `proxy-refused` for any other status, which the
 * same placeholder would get again. One it wrote after the write may have reached Dispatch
 * (`unknown`, or no such header) is `proxy-unsettled`. A service's answer it carried has no code,
 * and is read as Dispatch's.
 */
function answerEvent(answer: WriteAnswer, canTellDeadToken: boolean): WriteEvent {
  const { status } = answer.response;
  if (answer.proxy !== undefined) return proxyEvent(status, answer.proxy.left);
  if (status >= 200 && status < 300) {
    if (answer.notJson) return "foreign-2xx";
    if (answer.malformed) return "body-malformed";
    return answer.echoed ? "applied" : "foreign-2xx";
  }
  if (status >= 300 && status < 400) return "redirect";
  if (status === 401 && answer.code === BROKER_TOKEN_NOT_ACTIVE) {
    return canTellDeadToken ? "dead-token" : "unjudged";
  }
  if (
    status === 404 &&
    answer.code === "NOT_FOUND" &&
    answer.error?.startsWith(NO_ROUTE_MESSAGE_PREFIX) === true
  ) {
    return "no-route";
  }
  if (status === 401 && answer.echoed) return "uncommitted-401";
  if (tokenNotCheckedAnswer(status, answer.code)) return "try-later";
  const textRefusal =
    answer.error !== undefined && answer.response.headers.has(TEXT_REFUSAL_HEADER);
  if (!textRefusal && retriesUnderKey(status, answer.code)) {
    if (status === 409) return "in-flight";
    return mayHaveReachedServer(status) ? "may-have-run" : "try-later";
  }
  // Every verdict of the keyed layer names the request's key back: an answer that names none, or
  // another request's, is no verdict of it, whatever its headers or code say.
  if (!answer.echoed) return "unjudged";
  if (answer.replayed || answer.code === IDEMPOTENCY_KEY_COMMITTED) return "judged";
  if (!isStoredKeyStatus(status)) return "unjudged";
  if (answer.code === IDEMPOTENCY_KEY_REUSED) return "judged";
  // The fresh-handler clause: no keyed-layer code is a handler's stored answer. `KEY_COMMITTED`
  // and `KEY_REUSED` were read by their own clauses above.
  return answer.code === IDEMPOTENCY_KEY_IN_FLIGHT ? "unjudged" : "judged";
}

/**
 * The table's event for a credential that could not present an attempt: `no-credential` for the
 * fail-fast broker credential's `TradeRetryingError`, which it throws at once after a failed trade
 * while the trade goes on (`BrokerTokenClient.token`), so a later presentation within the call can
 * succeed; `no-credential-final` for any other. That includes a trade still unanswered
 * (`TradeUnansweredError`) and a pod's registration still pending (`RegistrationPendingError`),
 * which the token client throws only once the presentation's signal has ended: the credential's
 * share of the call's deadline (`credentialDeadline`), so no later presentation fits in the call.
 */
function credentialEvent(error: unknown): WriteEvent {
  return error instanceof TradeRetryingError ? "no-credential" : "no-credential-final";
}

/**
 * The table's event for a write a deadline or a stop ended, with an attempt in flight or not:
 * read from `limit` (the call's signal and the client's) where it ended, else from `error`'s own
 * name, since a fetch, a pause or a credential that times out or is aborted on its own signal
 * fails as `TimeoutError` or `AbortError`. A credential that waited its share of the deadline
 * (`credentialDeadline`) is the deadline too. Undefined for any other failure.
 */
function stoppedEvent(
  limit: AbortSignal,
  inFlight: boolean,
  error: unknown
): WriteEvent | undefined {
  const reason: unknown = limit.aborted ? limit.reason : error;
  const name = reason instanceof Error || reason instanceof DOMException ? reason.name : "";
  if (name === "TimeoutError") return inFlight ? "deadline-in-flight" : "deadline";
  if (limit.aborted || name === "AbortError") return inFlight ? "cancelled-in-flight" : "cancelled";
  return undefined;
}

/**
 * Dispatch's refusal of a broker token the secrets broker does not hold as live: unknown to it,
 * lapsed, or ended by a revoke before it lapsed (`brokerTokenActor`,
 * packages/envoy/internal/dispatch/api/broker_token.go).
 */
const BROKER_TOKEN_NOT_ACTIVE = "BROKER_TOKEN_NOT_ACTIVE";

/**
 * Whether `response` is Dispatch's `401 BROKER_TOKEN_NOT_ACTIVE`, read by its JSON's code alone,
 * whatever its `error`. Its JSON is read from a clone, so the body stays whole for whoever reads
 * the answer. Exported for `dispatch proxy`, which reads a `401` by the rule this client reads it
 * by.
 */
export async function tokenNotLive(response: Response): Promise<boolean> {
  if (response.status !== 401 || !isJson(response)) return false;
  const body: unknown = await response
    .clone()
    .json()
    .catch(() => undefined);
  return asErrorShape(body).code === BROKER_TOKEN_NOT_ACTIVE;
}

/**
 * Why `answer` is not the page `listIssuePage` asked for, and whether asking again can change
 * that; undefined when it is one. A JSON answer that is not a page is Dispatch's own and comes back
 * the same. A 2xx that is not JSON never reaches here: `#response` refuses it as something in
 * Dispatch's place, which a retry can get past.
 */
function whyNotAPage(answer: unknown): string | undefined {
  let what: string;
  if (Array.isArray(answer)) {
    what =
      `a bare array of ${answer.length} entries, the unpaged listing, which means that Dispatch ` +
      "is older than sjawhar/legion#1612 or that change has regressed";
  } else if (answer === null || typeof answer !== "object") {
    what = answer === null ? "null" : `a ${typeof answer}`;
  } else {
    const fields = answer as Partial<Record<keyof IssueSummaryPage, unknown>>;
    const lacking = [
      ...(Array.isArray(fields.issues) ? [] : ["an issues array"]),
      ...(["total", "limit", "offset"] as const)
        .filter((name) => typeof fields[name] !== "number")
        .map((name) => `a numeric ${name}`),
    ];
    if (lacking.length === 0) return undefined;
    what = `an object without ${lacking.join(" or ")}`;
  }
  return (
    `${what}. Retrying will not help: the same request gets the same answer until that ` +
    "Dispatch is upgraded or fixed."
  );
}

/**
 * Why `answer` is not the bare array `listAllIssues` asked for. A page here is the telling case:
 * it means Dispatch paged a request that carried neither `limit` nor `offset`, so the caller did
 * not get the exact set it needs. A 2xx that is not JSON never reaches here (`#response`).
 */
function whyNotAnArray(answer: unknown): string {
  if (answer === null) return "null";
  if (typeof answer !== "object") return `a ${typeof answer}`;
  if ("issues" in answer && Array.isArray(answer.issues)) {
    return "a page, so this Dispatch pages a request that carries no limit and no offset";
  }
  return "an object that is not the listing";
}

/**
 * What a body that is not Dispatch's JSON is. Dispatch writes every answer as JSON, its errors and
 * its `/api` 404 included, so anything else in its place came from a proxy or gateway in the way.
 */
const GATEWAY_PAGE = "which looks like a proxy or gateway page rather than Dispatch's own answer";

/**
 * What asking again can do once a gateway answered `method` with `status`. A status that cannot
 * clear answers the same request the same way. One that can (`gatewayClears`) may clear for a
 * read, and for a 2xx, which reaches here only for a read, a PUT or a DELETE, none of which writes
 * twice: a keyed write's 2xx is the write table's (`#endWrite` reads it, never `#payloadOf`), and
 * `#route` gives every 2xx's body that is not Dispatch's JSON back as it came, or refuses one that
 * holds a credential it presented (`DispatchCredentialEchoError`). A write answered so may already
 * have taken effect only when the request may have reached Dispatch (`mayHaveReachedServer`: after
 * a gateway's 5xx or 408, not its 429), where a blind retry could apply it twice; otherwise it did
 * not, and a retry may succeed as for a read.
 */
function gatewayAdvice(method: string, status: number): string {
  if (!gatewayClears(status)) {
    return "a retry gets the same answer until the Dispatch URL, or whatever answers in its place, is fixed";
  }
  if (method === "GET" || (status >= 200 && status < 300)) return "a retry may succeed";
  return mayHaveReachedServer(status)
    ? "the write may or may not have reached Dispatch: check whether it took effect before retrying it"
    : "the write did not reach Dispatch, and a retry may succeed";
}

/** Whether a gateway's answer with `status` can clear on asking again: an outage (`outage`: a
 *  gateway's 5xx, a timeout or a rate limit), which can on its own, or a 2xx something in
 *  Dispatch's place wrote, which a later request can get past once whatever is in the way is. Any
 *  other status answers the same request the same way. */
function gatewayClears(status: number): boolean {
  return outage(status) || (status >= 200 && status < 300);
}

/** The methods of a read, and of a write sent once with no key (`PUT`, `DELETE`). */
type ReadMethod = "GET" | "PUT" | "DELETE";

/** The methods of a write under a key (`WriteCall`), whose 2xx is read into its row. */
type WriteMethod = "POST" | "PATCH";

/** The members a write's callers read of its 2xx, in the forms its route answers
 *  (`dispatch-write-rows.ts`). A 2xx that names the call's key back and is not this row is a
 *  landing (`#writePayload`). */
type WriteRow = ZodType;

/** A write's options. Each admits `undefined` as well as absence, since a caller passes on an
 *  option it may not have, and a package compiled with `exactOptionalPropertyTypes`
 *  (`packages/claude-envoy`) tells the two apart. */
interface WriteOptions {
  /** The write's query, such as `follow_up`. */
  readonly query?: object | undefined;
  /** The caller's own `Idempotency-Key`, where it has one intent across calls and processes. */
  readonly key?: string | undefined;
  /** Cipher's proxy the write goes through (`#throughProxy`), for a person's review alone. */
  readonly via?: ProxyTransport | undefined;
}

/**
 * The client's code of a write whose 2xx named its key back and whose body, read whole, gives no
 * row its callers read (`#writePayload`): Dispatch answers a 2xx only after the write committed,
 * so the write landed, and its answer is what was lost. It is also the code of a
 * `DispatchAnswerMissingError`, a 2xx of a route that stores nothing, which made nothing.
 */
export const DISPATCH_ANSWER_LOST = "ANSWER_LOST";

/** The codes of a landed write whose answer is gone: Dispatch's two (`IdempotencyLandedCode`),
 *  and the client's own `DISPATCH_ANSWER_LOST`. */
export type DispatchLandedCode = IdempotencyLandedCode | typeof DISPATCH_ANSWER_LOST;

/**
 * An answer of a POST that stores nothing (`rowStores`: Dispatch's parse of a text, the
 * related-item check, a result's dry run) that is not this call's answer: a 2xx with no JSON, an
 * empty body, JSON that is not its row, or an answer naming another request's key back, and a
 * `409 IDEMPOTENCY_KEY_COMMITTED`; a replay whose first answer Dispatch did not keep is a
 * `DispatchAnswerNotKeptError` instead (`#writingNothing`). A bodiless 204
 * or 205 the write table settled on is its answer, "" (`#writePayload`). The call made nothing,
 * so this is no landing and leaves no outcome unknown, and asking again is safe. `code` is
 * `DISPATCH_ANSWER_LOST`.
 */
export class DispatchAnswerMissingError extends Error {
  override readonly name = "DispatchAnswerMissingError";
  readonly code = DISPATCH_ANSWER_LOST;

  constructor(
    /** The 2xx that came without the answer. */
    readonly status: number,
    method: string,
    url: string,
    options?: ErrorOptions
  ) {
    super(
      `${method} ${url} answered ${status} without the answer it asks for ` +
        `(${DISPATCH_ANSWER_LOST}); it stores nothing, so nothing was made, and asking again is safe.`,
      options
    );
  }
}

/**
 * A call whose path holds a value that names nothing (`pathNamesNothingPattern`: Go's white space
 * alone, or only dots and slashes), refused before any request (`DispatchClient.#url`): sent,
 * `POST /api/v1/comments//resolve` names no comment, and
 * Dispatch's router answers it with a redirect to another path, and `GET /api/v1/agents/../answers`
 * asks another route. The message names the place of that value in the route.
 */
export class DispatchEmptyPathError extends Error {
  override readonly name = "DispatchEmptyPathError";

  constructor(path: readonly string[]) {
    const route = path
      .map((segment) =>
        segment === ""
          ? "{empty}"
          : pathNamesNothingPattern.test(segment)
            ? `{${JSON.stringify(segment)}}`
            : segment
      )
      .join("/");
    super(
      `/${route} holds a value that names nothing where the call names what it is about (empty, only white space, or only dots and slashes, which name no step of a path), so nothing was sent: a variable that gives it may be empty or unset`
    );
  }
}

/** How Bun words a proxy's refusal of a CONNECT: `Proxy refused to open a tunnel: <status>
 *  <reason>`, the status and the reason phrase the proxy answered with. */
const PROXY_TUNNEL_REFUSED = /: (\d{3}) ?(.*)$/;

/**
 * The secrets broker's airlock proxy, Cipher's proxy, refused to open the write's tunnel, so
 * nothing was sent to Dispatch: Bun's `ERR_PROXY_TUNNEL`, whose message carries the status line,
 * since a refusal at the CONNECT reaches the caller as its status and headers and no body. `status`
 * is that status, 0 where the message names none, `reason` its reason phrase, and `proxyCode` the
 * answer's `Cipher-Proxy-Code` where it is of the proxy's form (`PROXY_CODE_FORM`). The write reads
 * it as every answer the proxy wrote before anything left it (`proxyEvent`): a 5xx, a grant it could
 * not read among them, is a connection that never opened and is asked again within the write's
 * bound; every other status ends the write at once, a 407 among them, which names a grant that
 * ended or a proxy password that names none. A review sent through the proxy (`via`) ends with
 * `DispatchProxyRefusalError` in its place (`#endWrite`).
 */
export class AirlockTunnelRefusedError extends Error {
  override readonly name = "AirlockTunnelRefusedError";
  readonly code = "AIRLOCK_TUNNEL_REFUSED";

  constructor(
    readonly status: number,
    readonly reason: string,
    readonly proxyCode: string | undefined,
    options?: ErrorOptions
  ) {
    const said = status === 0 ? reason : `${status}${reason === "" ? "" : ` ${reason}`}`;
    super(
      `the airlock proxy refused the connection (${said}); nothing was sent to Dispatch`,
      options
    );
  }
}

/** `error` as the airlock's refusal of a tunnel where it is Bun's `ERR_PROXY_TUNNEL`, or undefined.
 *  Bun keeps the CONNECT answer's headers on the error, where the proxy names its code. */
function tunnelRefusal(error: unknown): AirlockTunnelRefusedError | undefined {
  if (!(error instanceof Error) || Reflect.get(error, "code") !== "ERR_PROXY_TUNNEL") {
    return undefined;
  }
  const headers: unknown = Reflect.get(error, "headers");
  const named = headers instanceof Headers ? (headers.get(PROXY_CODE_HEADER)?.trim() ?? "") : "";
  const proxyCode = PROXY_CODE_FORM.test(named) ? named : undefined;
  const said = PROXY_TUNNEL_REFUSED.exec(error.message);
  return said === null
    ? new AirlockTunnelRefusedError(0, error.message, proxyCode, { cause: error })
    : new AirlockTunnelRefusedError(Number(said[1]), said[2]?.trim() ?? "", proxyCode, {
        cause: error,
      });
}

/**
 * An answer the airlock proxy gave itself inside a connection that was open
 * (`Cipher-Proxy-Forwarded: false`, `proxyOwnAnswer`) to a write that did not go through it as a
 * review (`via`): a refusal of the request, a grant it could not read, or a dial to Dispatch that
 * failed, each before any byte of the request left it, so the request reached no service and
 * stored nothing. `code` is its `Cipher-Proxy-Code`, or `HTTP_<status>` where it names none, and
 * `body` its text as the client's excerpt quotes it (`#airlockAnswer`). A 5xx is asked again
 * within the write's bound before the write ends with it (`proxyEvent`). It is no
 * `DispatchServiceError`, which a caller reads as a refusal Dispatch decided, and no rule reads it
 * as a write whose outcome is unknown (`writeOutcomeUnknown`). The proxy's answer after the write
 * may have left it is `AirlockOutcomeUnknownError`.
 */
export class AirlockRefusedError extends Error {
  override readonly name = "AirlockRefusedError";

  constructor(
    readonly status: number,
    readonly code: string,
    readonly body: string
  ) {
    super(`the airlock proxy answered ${status} ${code} itself, and sent nothing on: ${body}`);
  }
}

/**
 * An answer the airlock proxy gave itself after the write may have reached Dispatch
 * (`Cipher-Proxy-Forwarded: unknown`, `proxyOwnAnswer`): its connection to Dispatch broke after the
 * write was sent, or the write's grant ended while Dispatch held the call. Dispatch may have stored
 * it, so the write asks again under its key (`proxy-unsettled`), and ends unsettled
 * (`DispatchWriteUnsettledError`) with this as its `last` when nothing settles it. `code` and
 * `body` are as `AirlockRefusedError`'s.
 */
class AirlockOutcomeUnknownError extends Error {
  override readonly name = "AirlockOutcomeUnknownError";

  constructor(
    readonly status: number,
    readonly code: string,
    readonly body: string
  ) {
    super(
      `the airlock proxy answered ${status} ${code} itself after the write may have reached Dispatch: ${body}`
    );
  }
}

/**
 * Dispatch's two answers of its check of a broker token that judged nothing, before any handler
 * ran (`brokerTokenActor`, packages/envoy/internal/dispatch/api/broker_token.go): `429
 * BROKER_TOKEN_RATE_LIMITED`, the address's bucket of checks is spent and nothing was asked of
 * the broker, and `503 BROKER_UNAVAILABLE`, the broker answered nothing. A request answered so did
 * nothing, so a write answered so stored nothing, though one of them is a 5xx.
 */
const TOKEN_NOT_CHECKED: Readonly<Partial<Record<string, number>>> = {
  BROKER_TOKEN_RATE_LIMITED: 429,
  BROKER_UNAVAILABLE: 503,
};

/** Whether status and code are one of {@link TOKEN_NOT_CHECKED}'s answers. */
function tokenNotCheckedAnswer(status: number, code: string | undefined): boolean {
  return code !== undefined && TOKEN_NOT_CHECKED[code] === status;
}

/** Whether `error` is Dispatch's answer that it judged no token, so the request did nothing
 *  ({@link TOKEN_NOT_CHECKED}). */
export function tokenNotChecked(error: unknown): error is DispatchServiceError {
  return (
    error instanceof DispatchServiceError &&
    error.fromDispatch &&
    tokenNotCheckedAnswer(error.status, error.code)
  );
}

/**
 * Whether a write that failed with `error` may have stored what it sent while no answer says
 * whether it did: a `DispatchWriteUnsettledError`; a transport failure after an attempt that may
 * have reached Dispatch; a gateway's answer that may have come after the request reached Dispatch
 * (`mayHaveReachedDispatch`); or Dispatch's own 5xx, which can come after it committed, but for
 * its answer that it judged no token. A refusal Dispatch decided, a gateway's answer that never
 * reached it, a refusal Cipher's proxy wrote before anything of the write left it
 * (`DispatchProxyRefusalError`, which a write throws only while no attempt may have reached
 * Dispatch, its 5xx among them), and a failure before any request of the write went out (its
 * credential, a refused connection) stored nothing.
 */
export function writeOutcomeUnknown(error: unknown): boolean {
  if (error instanceof DispatchWriteUnsettledError) return true;
  if (error instanceof DispatchGatewayError) return error.mayHaveReachedDispatch;
  if (error instanceof DispatchProxyRefusalError) return false;
  if (error instanceof DispatchServiceError) return error.status >= 500 && !tokenNotChecked(error);
  return false;
}

/**
 * The key `response` names back (`Idempotency-Key`), or "" for none, bare: a key sent in double
 * quotes comes back without them, so both forms are read. Dispatch's keyed layer names the
 * request's key back on every answer it gives a keyed write, which is what makes an answer that
 * names none, or another request's, no answer of this call's (`#writePayload`).
 */
function keyNamedBack(response: Response): string {
  return (response.headers.get(IDEMPOTENCY_KEY_HEADER)?.trim() ?? "").replace(/^"(.*)"$/, "$1");
}

/** What a `DispatchClient` is built with beside its URL, its credential and its fetch. */
export interface DispatchClientOptions {
  readonly signal?: AbortSignal | undefined;
  /** The client's own deadline, counted from now: `DISPATCH_TOOL_DEADLINE_MS` when absent. A
   *  maker with a budget of its own names it here rather than as a timed `signal`, so the
   *  credential is given that budget less the request's reserve (`#credentialSignal`). */
  readonly deadlineMs?: number | undefined;
  readonly writeRetry?: DispatchWriteRetry | undefined;
  readonly sleep?: Sleep | undefined;
  /** Whether a request a Dispatch process answered with no route for it is asked again
   *  (`#noRoute`), true by default. False runs each request once and throws that answer as it
   *  came (a `DispatchServiceError` `isNoRoute` recognises), for a caller that has an answer
   *  of its own to a missing route and wants it at once, rather than after the waits of
   *  `NO_ROUTE_RETRY_DELAYS_MS`. */
  readonly retryNoRoute?: boolean | undefined;
}

/**
 * JSON HTTP client for Dispatch's native-tool API. Every read, PUT or DELETE goes through `#send`,
 * where a broker token Dispatch says is no longer live is traded afresh and the request sent once
 * more. Every write (POST or PATCH) goes through `#sendWrite`, where it carries an
 * `Idempotency-Key` of its own and asks again under it as `writeRetry` paces it; a person's review
 * through Cipher's proxy is such a write too, presenting the approve placeholder in the
 * credential's place on every attempt (`reviewArtifact`, `#throughProxy`).
 *
 * `credential` is what every request presents: a bearer (`bearerCredential`, src/bearer.ts), or
 * what the secrets broker trades this machine's identity for, or the identity of a person signed
 * in on it, presented under the `DPoP` scheme with a fresh proof of the token's key on every
 * request. A resolved Dispatch holds exactly one of them (`ActiveDispatchConfig.credential`), so
 * no caller chooses.
 */
export class DispatchClient {
  readonly #baseUrl: string;
  readonly #resolvedIssues = new Map<string, Promise<string>>();
  readonly #signal: AbortSignal;
  readonly #credential: RequestCredential;
  /** The tokens this client presented lately, which every excerpt of an answer redacts beside the
   *  credential's secret now (`SentTokens`). */
  readonly #sent = new SentTokens();
  readonly #sleep: Sleep;
  readonly #retryNoRoute: boolean;
  readonly writeRetry: DispatchWriteRetry;
  /**
   * When this client was made, on the write clock (`#now`): its deadline counts from here, so
   * no write asks again later than `writeRetry.inFlightWithinMs` after it, however late in the
   * client's life the write began.
   */
  readonly #madeAt: number;
  /** When the client's own deadline ends, on the process's monotonic clock, which the deadline's
   *  timer keeps too: the time a credential is given to present is read off it
   *  (`#credentialSignal`). */
  readonly #deadlineAt: number;
  /**
   * `fetchImpl`, with a transport failure named by the Dispatch URL as `urlForText` names it and
   * the failure's class (`errorClass`), never the runtime's message: that message repeats the host
   * it dialed, and a URL that is not clean dials part of its credential
   * (`https://user:p@ssword` dials `ssword`). Every request of this client goes through it, so the
   * `dispatch` command, Claude Code's session hook, the people's-answers line and pi-envoy's
   * notices all read the one text.
   */
  readonly #fetch: typeof fetch;

  constructor(
    baseUrl: string,
    credential: RequestCredential,
    readonly fetchImpl: typeof fetch = fetch,
    options: DispatchClientOptions = {}
  ) {
    this.#credential = credential;
    // Its user info held, as the Envoy client's is (`holdUrlUserinfo`).
    this.#baseUrl = holdUrlUserinfo(withoutTrailingSlashes(baseUrl));
    const shown = urlForText(this.#baseUrl, DISPATCH_URL_SETTING);
    this.#fetch = Object.assign(
      async (input: Parameters<typeof fetch>[0], init?: RequestInit): Promise<Response> => {
        try {
          // No request follows a redirect. Dispatch's API routes answer none, so a 3xx is something
          // in its place: followed, a 301, 302 or 303 turns a write's POST into a GET whose 2xx
          // reads as the write's answer, and a 307 or 308 sends the POST again to another origin
          // without its credential. A 3xx is handed on as an answer, and the write's table reads it
          // (`redirect`, write-outcome.ts).
          return await fetchImpl(input, { ...init, redirect: "manual" });
        } catch (error) {
          // Bun's and undici's fetch reject a request that reached no server with a TypeError; a
          // deadline or a caller's stop is a DOMException, which passes on unchanged. The error
          // keeps whether the failure reached nothing at all (`reachedNothing`), which the write
          // loop reads before the runtime's error is dropped.
          const tunnel = tunnelRefusal(error);
          if (tunnel !== undefined) throw tunnel;
          if (error instanceof TypeError) {
            throw new DispatchUnreachableError(
              `Dispatch at ${shown} is unreachable: ${errorClass(error)}. If the Dispatch URL changed, restart this agent process so it picks up the new configuration.`,
              reachedNothing(error),
              errorClass(error)
            );
          }
          throw error;
        }
      },
      { preconnect: fetchImpl.preconnect }
    );
    const deadlineMs = options.deadlineMs ?? DISPATCH_TOOL_DEADLINE_MS;
    this.#signal = requestSignal(options.signal, deadlineMs);
    this.#deadlineAt = performance.now() + deadlineMs;
    this.writeRetry = options.writeRetry ?? DISPATCH_WRITE_RETRY;
    this.#sleep = options.sleep ?? sleep;
    this.#retryNoRoute = options.retryNoRoute ?? true;
    this.#madeAt = this.#now();
  }

  /** The secret this client presents, which an excerpt of someone else's answer redacts; "" while
   *  a broker credential has yet to trade for one. */
  get token(): string {
    return this.#credential.secret();
  }

  /**
   * Creates an issue. `created` is whether this call made one (Dispatch's 201); false (its 200)
   * means Dispatch already holds the issue `external` names and answered with that issue.
   */
  async issue(input: CreateIssueInput): Promise<Advised<Issue> & { created: boolean }> {
    const answer = await this.#writeAnswer<Advised<Issue>>(
      "POST",
      ["api", "v1", "issues"],
      WRITE_ROWS.issue,
      input
    );
    return { ...answer.payload, created: answer.status === 201 };
  }

  /** Resolves an existing native key or external reference without creating an issue. */
  async resolveIssue(issueReference: string): Promise<string> {
    return this.#resolveIssue(issueReference);
  }

  /**
   * One page of `GET /api/v1/issues?limit=&offset=`, used as Dispatch served it: `IssueSummaryPage`
   * (sjawhar/legion#1612). Any other answer is refused, naming the request sent, what arrived and
   * whether a retry can help; a bare array answered to this paged request means a Dispatch older
   * than that change or a regression of it.
   */
  async listIssuePage(
    options: ListIssuesOptions,
    page: IssuePageRequest
  ): Promise<IssueSummaryPage> {
    const path = ["api", "v1", "issues"];
    const query = { ...options, limit: page.limit, offset: page.offset };
    const answer: unknown = await this.#json("GET", path, undefined, query);
    const refusal = whyNotAPage(answer);
    if (refusal === undefined) return answer as IssueSummaryPage;
    const url = requestForText(this.#baseUrl, this.#url(path, query), DISPATCH_URL_SETTING);
    throw new Error(
      `GET ${url} asked for a page ({issues, total, limit, offset}) and got ${refusal}`
    );
  }

  /**
   * Every issue the filters match, in one read: `GET /api/v1/issues` with neither `limit` nor
   * `offset`, which Dispatch answers as a bare array rather than a page (`parseIssuePage` returns
   * no page when the query carries neither). A caller whose own writes move issues out of what it
   * filtered on cannot walk offsets safely, because a row that shifts across a page boundary is
   * served twice or never; this read is the exact set at one moment. Any answer that is not an
   * array is refused, naming what arrived: a page here means a Dispatch that still pages a request
   * carrying no paging parameters.
   */
  async listAllIssues(options: ListIssuesOptions): Promise<IssueSummary[]> {
    const path = ["api", "v1", "issues"];
    const query = { ...options };
    const answer: unknown = await this.#json("GET", path, undefined, query);
    if (Array.isArray(answer)) return answer as IssueSummary[];
    const url = requestForText(this.#baseUrl, this.#url(path, query), DISPATCH_URL_SETTING);
    throw new Error(`GET ${url} asked for the unpaged listing and got ${whyNotAnArray(answer)}.`);
  }

  /** `GET /api/v1/issues/{key}/artifacts`: every document and file the issue holds, each with
   *  its versions, which is what `dispatch artifact` reads to refuse a name the issue already
   *  holds. */
  async listIssueArtifacts(issue: string): Promise<Artifact[]> {
    return this.#json("GET", ["api", "v1", "issues", await this.#resolveIssue(issue), "artifacts"]);
  }

  /** `POST /api/v1/markdown/round-trip`: the markdown Dispatch would store for this text. The
   *  route writes nothing: it checks the text as an upload checks it, parses it and renders it
   *  again, as that upload's write does, so a caller compares the answer with the text it holds
   *  and refuses a text the store would change. A text every upload refuses for the text alone is
   *  refused here in the upload's own words. With `newDocument`, the request asks for the
   *  refusals only a new document's seed makes too (`new_document`), and the answer is a new
   *  document's; without it, which is the body every Dispatch with the route takes, Dispatch
   *  answers as the most permissive replacement takes the text: with the rendering a replacement
   *  over a document already holding what the text holds stores, or with a refusal every
   *  replacement makes. */
  async markdownRoundTrip(markdown: string, newDocument = false): Promise<string> {
    return this.#roundTrip(
      ["api", "v1", "markdown", "round-trip"],
      WRITE_ROWS.markdownRoundTrip,
      { markdown },
      newDocument
    );
  }

  /** `POST /api/v1/markdown/file-round-trip`: `markdownRoundTrip` of a file's bytes, sent as
   *  base64 in the body's `file`, with `newDocument` as `markdownRoundTrip` takes it. Dispatch
   *  checks them as bytes, as it checks an upload's file part, so a file holding a byte that is
   *  not UTF-8 is refused as its upload is. A Dispatch without this route answers 404 or 405. */
  async fileRoundTrip(bytes: Uint8Array, newDocument = false): Promise<string> {
    const file = Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength).toString("base64");
    return this.#roundTrip(
      ["api", "v1", "markdown", "file-round-trip"],
      WRITE_ROWS.fileRoundTrip,
      { file },
      newDocument
    );
  }

  /** The round trip at `path` of `body`, read into `row`, which stores nothing (`rowStores`),
   *  asking for a new document's refusals too where `newDocument` (`new_document`). The member is
   *  left out otherwise, so the body is the one a Dispatch before the member takes. An answer
   *  Dispatch kept no copy of is asked for again (`#writingNothing`). */
  async #roundTrip(
    path: readonly string[],
    row: WriteRow,
    body: MarkdownRoundTripInput | FileRoundTripInput,
    newDocument: boolean
  ): Promise<string> {
    const answer: MarkdownRoundTripResponse = await this.#writingNothing(
      path,
      row,
      newDocument ? { ...body, new_document: true } : body
    );
    return answer.markdown;
  }

  async listProjectArtifacts(project: string, unlinked = false): Promise<Artifact[]> {
    return this.#json(
      "GET",
      ["api", "v1", "projects", project, "artifacts"],
      undefined,
      unlinked ? { unlinked: "true" } : undefined
    );
  }

  /**
   * `POST /api/v1/projects/{key}/artifacts`: an upload the project owns. `key` is the caller's own
   * `Idempotency-Key` for the upload, where the caller has one intent across calls and processes —
   * AskGate's decision document, created once per session however many processes reach for it — and
   * replaces the key this call would mint for itself. It must match `IDEMPOTENCY_KEY_PATTERN`.
   */
  async projectArtifact(
    project: string,
    input: CreateArtifactInput,
    key?: string
  ): Promise<DocumentAdvised<ArtifactUploadResponse>> {
    return this.#upload(
      ["api", "v1", "projects", project, "artifacts"],
      input,
      WRITE_ROWS.projectArtifact,
      key
    );
  }

  async getProjectArtifact(project: string, slug: string): Promise<Artifact> {
    return this.#json("GET", ["api", "v1", "projects", project, "artifacts", slug]);
  }

  /** `POST /api/v1/agents/{session_id}/artifacts`: an upload owned by an agent's conversation on
   *  the Agents page, addressed `dispatch://agent/<session id>/artifact/<slug>`. That owner takes
   *  files and pictures only; Dispatch refuses a Markdown document there. */
  async agentArtifact(
    sessionId: string,
    input: CreateArtifactInput
  ): Promise<Advised<ArtifactUploadResponse>> {
    return this.#upload(
      ["api", "v1", "agents", sessionId, "artifacts"],
      input,
      WRITE_ROWS.agentArtifact
    );
  }

  async getAgentArtifact(sessionId: string, slug: string): Promise<Artifact> {
    return this.#json("GET", ["api", "v1", "agents", sessionId, "artifacts", slug]);
  }

  async getIssueArtifact(issue: string, slug: string): Promise<Artifact> {
    return this.#json("GET", [
      "api",
      "v1",
      "issues",
      await this.#resolveIssue(issue),
      "artifacts",
      slug,
    ]);
  }

  /** `GET /api/v1/search`, or `GET /api/v1/search/results` for a request carrying a member a
   *  Dispatch released before that route would drop (`everyBuildReadsSearch`), which such a
   *  build refuses rather than answering unnarrowed. */
  async search(query: string, options: SearchOptions = {}): Promise<SearchResponse> {
    const request = { q: query, ...options };
    const path = everyBuildReadsSearch(request)
      ? ["api", "v1", "search"]
      : ["api", "v1", "search", "results"];
    return this.#json("GET", path, undefined, request);
  }

  /** `POST /api/v1/projects/{key}/related`: the related-item check over a title and a text,
   *  writing nothing. A blank title and text answer `outcome: "not_made"` with the reason. An
   *  answer Dispatch kept no copy of is asked for again (`#writingNothing`). */
  async related(project: string, input: RelatedCheckInput): Promise<Suggestions> {
    return this.#writingNothing(
      ["api", "v1", "projects", project, "related"],
      WRITE_ROWS.related,
      input
    );
  }

  async getIssue(issue: string): Promise<IssueDetails> {
    return this.#json("GET", ["api", "v1", "issues", await this.#resolveIssue(issue)]);
  }

  /** `PATCH /api/v1/issues/{key}`: the server replaces every field the body names. A call that
   *  writes `components` reads them back, so its answer is held to the row that requires them. */
  async updateIssue(issue: string, input: UpdateIssueInput): Promise<Advised<Issue>> {
    return this.#write(
      "PATCH",
      ["api", "v1", "issues", await this.#resolveIssue(issue)],
      input.components === undefined ? WRITE_ROWS.updateIssue : WRITE_ROWS.updateIssueComponents,
      input
    );
  }

  /**
   * `POST /api/v1/issues/{key}/status-change`: writes the issue's status and, with an option, what
   * happens to the open issues under it or to the issues above it, so each parent's status stays
   * in line with its children's. The answer is the issue and one `changed` entry for each issue
   * whose status or parent the call changed, in the order the steps ran. A refusal carries
   * `changed` too: the entries of the steps that wrote before it (`DispatchServiceError`).
   *
   * A build without the route answers no route each time the write is sent (`#write` asks again
   * as for every write), and this then throws `statusChangeNoRouteRefusal`'s refusal. No PATCH is
   * sent in its place, since a PATCH would write the status without the option.
   */
  async changeIssueStatus(issue: string, input: StatusChangeInput): Promise<StatusChangeAnswer> {
    const path = ["api", "v1", "issues", await this.#resolveIssue(issue), "status-change"];
    try {
      return await this.#write("POST", path, WRITE_ROWS.changeIssueStatus, input);
    } catch (error) {
      throw this.#refusalOfNoRoute(error, path, (route) =>
        statusChangeNoRouteRefusal(this.#baseUrl, route)
      );
    }
  }

  /** `POST /api/v1/issues/{key}/claim`: this session takes the issue. 409 ISSUE_CLAIMED when a
   *  running session, or any human, holds it; 409 CLAIM_CONTENDED when the holder changed twice
   *  while the request ran, so nothing was applied. The answer's `legion_managed_status` says
   *  whether the Legion daemon owns this issue's status — true when the issue carries the
   *  `legion` label in any case or sits under one of its own project that does. On such an
   *  issue, where Dispatch runs with `DISPATCH_LEGION_STATUS_SCOPE` on, it refuses a status
   *  write from a credential that holds neither the `person` nor the `legion-status` scope
   *  (403 `LEGION_STATUS_SCOPE_REQUIRED`), but for `done`, `backlog`, `icebox` or `triage` from
   *  an architect of an issue above with its own broker token. Otherwise the daemon undoes a
   *  status anyone else writes. */
  async claimIssue(issue: string, input: { readonly actor?: Actor } = {}): Promise<ClaimedIssue> {
    return this.#write(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "claim"],
      WRITE_ROWS.claimIssue,
      input
    );
  }

  /** `DELETE /api/v1/issues/{key}/claim`: give up the claim, or clear one whose session is
   *  gone. The status does not move. It answers the same two 409s, has no force, and carries
   *  `legion_managed_status` as `claimIssue` does. */
  async releaseIssueClaim(
    issue: string,
    input: { readonly actor?: Actor } = {}
  ): Promise<ClaimedIssue> {
    return this.#json(
      "DELETE",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "claim"],
      input
    );
  }

  async getIssueEvents(
    issue: string,
    after = 0,
    limit = ISSUE_EVENTS_PAGE_MAX
  ): Promise<DispatchEvent[]> {
    return this.#json(
      "GET",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "events"],
      undefined,
      { after, limit }
    );
  }

  /** The issue and its newest `options.events` events (every one for `"all"`), oldest first, up
   *  to the issue's `last_seq` as read. An issue's events are numbered 1 to `last_seq`, so the
   *  newest N are the N after `last_seq - N`, read a page of at most `ISSUE_EVENTS_PAGE_MAX` at a
   *  time, and an event written after the issue was read is left to the next read. */
  async read(issueReference: string, options: IssueReadOptions = {}): Promise<IssueRead> {
    const issueKey = await this.#resolveIssue(issueReference);
    const issue = await this.getIssue(issueKey);
    const requested = options.events;
    const count =
      requested === undefined
        ? DEFAULT_ISSUE_READ_EVENTS
        : requested === "all"
          ? issue.last_seq
          : Math.min(requested, issue.last_seq);
    const events: DispatchEvent[] = [];
    let after = Math.max(0, issue.last_seq - count);
    while (events.length < count) {
      const limit = Math.min(ISSUE_EVENTS_PAGE_MAX, count - events.length);
      const page = await this.getIssueEvents(issueKey, after, limit);
      events.push(...page);
      const last = page.at(-1);
      // A short page is the end of the issue's events; a full one ends the read once it holds
      // `count`. Only a page the read goes past must have moved past `after`.
      if (page.length < limit || last === undefined || events.length >= count) break;
      if (last.seq <= after) {
        throw new Error(
          `GET /api/v1/issues/${issueKey}/events?after=${after} answered no event after seq ${after}`
        );
      }
      after = last.seq;
    }
    return { issue, events };
  }

  async ask(issue: string, input: CreateAskInput): Promise<Advised<AskWithTrees>> {
    return this.#write(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "asks"],
      WRITE_ROWS.ask,
      input
    );
  }

  async openAsks(sessionID: string, since?: string): Promise<OpenAsksResponse> {
    return this.#json("GET", ["api", "v1", "asks", "open"], undefined, {
      author_session: sessionID,
      ...(since === undefined ? {} : { since }),
    });
  }

  /** Every open ask on a project's issues and documents (issue- or document-owned), oldest first. */
  async openAsksForProject(project: string): Promise<OpenAsksResponse> {
    return this.#json("GET", ["api", "v1", "asks", "open"], undefined, { project });
  }

  /** `GET /api/v1/priorities`: every live priority in rank order with the work under it, and the
   *  two faults; `project`, repeated, counts only those projects' work. */
  async priorities(projects: readonly string[] = []): Promise<Priorities> {
    return this.#json("GET", ["api", "v1", "priorities"], undefined, { project: projects });
  }

  /** `GET /api/v1/agents/{session_id}/answers`: what people wrote that Dispatch delivered to the
   *  session since `since` (72 hours ago when unset), newest first. With `maxChars` the server
   *  stops after the item whose text takes the running sum of text lengths past it, so the body
   *  stays about that size plus one item. `signal` aborts the request beside the client's own
   *  deadline, so a caller with a shorter deadline of its own does not leave it running. A server
   *  older than the route answers 404. */
  async sessionAnswers(
    sessionID: string,
    options: {
      readonly since?: string;
      readonly maxChars?: number;
      readonly signal?: AbortSignal;
    } = {}
  ): Promise<SessionAnswersResponse> {
    return this.#json(
      "GET",
      ["api", "v1", "agents", sessionID, "answers"],
      undefined,
      {
        ...(options.since === undefined ? {} : { since: options.since }),
        ...(options.maxChars === undefined ? {} : { max_chars: options.maxChars }),
      },
      options.signal
    );
  }

  /** `GET /api/v1/agents`: the Envoy listener's live sessions, as of Dispatch's copy of its list
   *  (at most 5 s old), for naming a session by the title it is running under rather than the
   *  one it stamped on an old write. */
  async listAgents(): Promise<Agent[]> {
    return this.#json("GET", ["api", "v1", "agents"]);
  }

  async whoami(): Promise<WhoamiResponse> {
    return this.#json("GET", ["api", "v1", "whoami"]);
  }

  /** `GET /api/v1/limits`: the bounds this server holds a write to, each read from the constant
   *  that enforces it. The client keeps no copy of them: it prints what the server answers. */
  async limits(): Promise<DispatchLimits> {
    return this.#json("GET", ["api", "v1", "limits"]);
  }

  /** `GET <path>`, for `dispatch api`: one read of a route no method here names. `path` begins
   *  `/api/v1` and may carry a query, and the contract's rule for it (`dispatch_api`) refuses a
   *  scheme, a host and a dot segment, so it resolves under this client's server as every other
   *  route does (`#url`). Answers Dispatch's JSON, or any other 2xx's body as the bytes that
   *  came, an attachment's whatever its type (`RouteAnswer`, `#route`), and refuses a 2xx whose
   *  body holds a credential the request presented (`DispatchCredentialEchoError`).
   *
   *  It sends the GET through one `#send` (once, and once more only after Dispatch answers that
   *  the token presented is not live) and throws a refusal as `#payloadOf` throws it, an answer
   *  of no route (`404 NOT_FOUND`, "no route for …") included: unlike every other request this
   *  client makes, it skips `#noRoute`, so it neither sends that request again nor turns the
   *  answer into `DispatchNoRouteError`. The caller wrote this path, and a caller-written path
   *  Dispatch has no route for is far more often a wrong path than half of a release in progress,
   *  so Dispatch's own refusal, which names the route, is printed as it came. */
  async readRoute(path: string): Promise<RouteAnswer> {
    return this.#route("GET", path);
  }

  /** `<method> <path>`, for `dispatch api --method`: one write of a route no method here names,
   *  `path` resolved as `readRoute` resolves it, with `body`, where given, sent as written as
   *  the request's JSON body. Like `readRoute` it goes through one `#send`, skipping `#noRoute`,
   *  and answers as `readRoute` does: Dispatch's JSON, any other 2xx's body as it came, the
   *  refusal of a 2xx whose body holds a credential it presented, or Dispatch's refusal thrown.
   *  It is no write of `#write`'s: it goes once, and once more only after Dispatch answers that
   *  the token it presented is not live (Dispatch authenticated nothing, so nothing ran), with no
   *  redirect followed and no `Idempotency-Key`, so a caller that repeats it decides that itself. */
  async sendRoute(method: DispatchApiMethod, path: string, body?: string): Promise<RouteAnswer> {
    return this.#route(method, path, body);
  }

  /** `GET /api/v1/inbox?assignee=<who>`: the open asks one person's Inbox lists, in the server's
   *  own order (the owning issue's priority, then whose turn it is, then the latest activity),
   *  which no reader re-sorts. `me` is the caller, and keeps the rows waiting on them — the asks
   *  handed to them, and the asks on the issues assigned to them that nobody has handed on —
   *  which is what the Inbox page shows by default. The route reads the caller's own `person`
   *  scope and refuses a credential without it with 403 OAUTH_SCOPE_REQUIRED. */
  async inbox(assignee: string): Promise<InboxRow[]> {
    return this.#json("GET", ["api", "v1", "inbox"], undefined, { assignee });
  }

  /** `GET /api/v1/me/replies?kind=reply,agent`: every unread thread the caller's Inbox lists —
   *  a thread they wrote in that somebody else has written in since, and a one-off message they
   *  sent an agent session that it answered — newest reply first, in the server's own order,
   *  which no reader re-sorts. No `limit` is asked for, so the answer holds every row. The route
   *  reads the caller's own `person` scope and refuses a credential without it with 403
   *  OAUTH_SCOPE_REQUIRED, as the Inbox does. */
  async myReplies(): Promise<MyRepliesResponse> {
    return this.#json("GET", ["api", "v1", "me", "replies"], undefined, { kind: "reply,agent" });
  }

  /** `GET /api/v1/credential-requests?approver=me`: the credential requests the caller may
   *  decide, then the expired ones nobody has dismissed (each row's `state`, `pending` or
   *  `expired`; none from an older broker, which is pending), relayed from the secrets broker, or
   *  null from a Dispatch that has no broker configured, which is an ordinary deployment rather
   *  than a failure. `me` is the only approver the route takes. It needs the same `person` scope
   *  the Inbox does. */
  async credentialPending(): Promise<CredentialPendingResponse | null> {
    return this.#json("GET", ["api", "v1", "credential-requests"], undefined, { approver: "me" });
  }

  /** `POST /api/v1/projects`: create a project. It answers the project with the default assignee
   *  the create stored, which the project list leaves out. A key that already exists is 409
   *  PROJECT_EXISTS, and a key that is not a project key or an empty name 400 INVALID_PROJECT. */
  async createProject(input: CreateProjectInput): Promise<CreatedProject> {
    return this.#write("POST", ["api", "v1", "projects"], WRITE_ROWS.createProject, input);
  }

  /** `GET /api/v1/projects`: every project, ordered by key. */
  async listProjects(): Promise<Project[]> {
    return this.#json("GET", ["api", "v1", "projects"]);
  }

  /** `PUT /api/v1/projects/{key}/proven-sessions`: turn the project's proven-sessions setting on.
   *  A bearer names its session in `actor`. It answers the project as `listProjects` lists it. A
   *  token scoped to another project turns it on, as the shared token does. A token scoped to one
   *  project is 403 PROJECT_SCOPE for a project that does not exist and for a private project
   *  that it does not name. */
  async turnOnProvenSessions(
    project: string,
    input: { readonly actor?: Actor } = {}
  ): Promise<Project> {
    return this.#json("PUT", ["api", "v1", "projects", project, "proven-sessions"], input);
  }

  /** `DELETE /api/v1/projects/{key}/proven-sessions`: turn the project's proven-sessions setting
   *  off. The route needs the person scope, so a credential without it is 403
   *  OAUTH_SCOPE_REQUIRED, a token scoped to another project included, and its writer is the
   *  person the credential acts for. It answers the project as `listProjects` lists it. A token
   *  scoped to one project is 403 PROJECT_SCOPE for a project that does not exist and for a
   *  private project that it does not name. */
  async turnOffProvenSessions(project: string): Promise<Project> {
    return this.#json("DELETE", ["api", "v1", "projects", project, "proven-sessions"]);
  }

  /** `GET /api/v1/projects/{key}/architecture-sources`: the project's architecture sources, one
   *  per repository, in the order the project added them; `[]` when it has none.
   *
   *  The plugin and the server ship separately, so a rollout mixes versions (#1522). A server
   *  older than this route has no route for it (`404 NOT_FOUND`, "no route for …"). Once every
   *  attempt met no route (`DispatchNoRouteError`), the client asks its
   *  `GET /api/v1/projects/{key}/architecture-source` instead: `200 null` (or a pre-#1522
   *  `404 SOURCE_NOT_FOUND`) is `[]`, and a row is that one source. Any other answer throws, so a
   *  project the server does not know (`404 NOT_FOUND`, "not found") is still an error. */
  async listArchitectureSources(project: string): Promise<ArchitectureSource[]> {
    try {
      return await this.#json("GET", ["api", "v1", "projects", project, "architecture-sources"]);
    } catch (error) {
      if (!(error instanceof DispatchNoRouteError)) throw error;
    }
    try {
      const source: ArchitectureSource | null = await this.#json("GET", [
        "api",
        "v1",
        "projects",
        project,
        "architecture-source",
      ]);
      return source === null ? [] : [source];
    } catch (error) {
      if (
        error instanceof DispatchServiceError &&
        error.status === 404 &&
        error.code === "SOURCE_NOT_FOUND"
      ) {
        return [];
      }
      throw error;
    }
  }

  /** `POST /api/v1/projects/{key}/architecture-sources/sync`: import every source of the
   *  project now. Every source is attempted, and each row's `last_error` says whether its
   *  import replaced its components or its previous ones stay up. 404 SOURCE_NOT_FOUND when the
   *  project has no source.
   *
   *  The plugin and the server ship separately, so a rollout mixes versions (#1522). A server
   *  older than this route has no route for it (`404 NOT_FOUND`, "no route for …"). Once every
   *  attempt met no route (`DispatchNoRouteError`), the client asks its
   *  `POST /api/v1/projects/{key}/architecture-source/sync` instead, which imports that server's
   *  one source, and returns the row as a one-element array. Any other answer of either call
   *  throws as is, so that server's `404 SOURCE_NOT_FOUND` for a project with no source is still
   *  an error. */
  async syncArchitectureSources(project: string): Promise<ArchitectureSource[]> {
    try {
      return await this.#write(
        "POST",
        ["api", "v1", "projects", project, "architecture-sources", "sync"],
        WRITE_ROWS.syncArchitectureSources
      );
    } catch (error) {
      if (!(error instanceof DispatchNoRouteError)) throw error;
    }
    const source: ArchitectureSource = await this.#write(
      "POST",
      ["api", "v1", "projects", project, "architecture-source", "sync"],
      WRITE_ROWS.syncArchitectureSource
    );
    return [source];
  }

  /** `POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync`: import the one
   *  source `repo` (`owner/name`) now; the row's `last_error` says whether the import replaced
   *  its components or its previous ones stay up. 404 SOURCE_NOT_FOUND when the project has no
   *  such source. */
  async syncArchitectureSource(project: string, repo: string): Promise<ArchitectureSource> {
    const [owner, name, ...rest] = repo.split("/");
    if (!owner || !name || rest.length > 0) {
      throw new Error(`repository must be owner/name, got ${JSON.stringify(repo)}`);
    }
    return this.#write(
      "POST",
      ["api", "v1", "projects", project, "architecture-sources", owner, name, "sync"],
      WRITE_ROWS.syncArchitectureSource
    );
  }

  async resolveAsk(id: string, input: ResolveAskInput): Promise<AskWithTrees> {
    return this.#write("POST", ["api", "v1", "asks", id, "resolve"], WRITE_ROWS.resolveAsk, input);
  }

  /**
   * Requests approval of a document at its latest settled version, its question the document, the
   * version and `summary`. A document holds one open request: this opens it, rewords it with a new
   * summary, or hands it back to the human when a new version or a thread reply left it waiting on
   * the agent. `recorded` is whether this call did any of that (Dispatch's 201); false (its 200)
   * means the open request already stood as asked, waiting on the human, or the version is already
   * approved, when `ask` is null and `approval` carries the standing approval.
   */
  async requestApproval(
    artifactID: string,
    input: { actor: Actor; summary: string; include_verification?: boolean }
  ): Promise<ApprovalRequestResponse & { recorded: boolean }> {
    const answer = await this.#writeAnswer<ApprovalRequestResponse>(
      "POST",
      ["api", "v1", "artifacts", artifactID, "approval-requests"],
      WRITE_ROWS.requestApproval,
      input
    );
    return { ...answer.payload, recorded: answer.status === 201 };
  }

  /**
   * Records the plan review's approval of a plan document at `version`. A plan is approved by its
   * review, never by a person: `requestApproval` on a plan answers 409 `PLAN_APPROVED_BY_REVIEW`,
   * and this answers 409 `NOT_PLAN_DOCUMENT` on any document whose role is not `plan`.
   */
  async planApprove(
    artifactID: string,
    input: { actor: Actor; version: number }
  ): Promise<ArtifactReview> {
    return this.#write(
      "POST",
      ["api", "v1", "artifacts", artifactID, "plan-reviews"],
      WRITE_ROWS.planApprove,
      input
    );
  }

  /**
   * Records the review of the person the credential names on one version of a document:
   * `approved`, or `changes_requested` with its `reason`. It sends
   * `POST /api/v1/artifacts/{id}/versions/{number}/reviews` with `{state, reason?}` and nothing
   * else (`createArtifactVersionReview` in packages/envoy/internal/dispatch/api/reviews.go); the
   * body names no actor, since the route records the person the credential names. `version` is
   * the version the caller read, sent as the path's `{number}` rather than as a member of the
   * body: a Dispatch route ignores a member it does not declare (`decodeJSON`;
   * packages/envoy/cmd/dispatch/README.md, "Two builds at once"), so a build without this route
   * answers that it has no route for it rather than recording the review at a version it picks
   * itself.
   *
   * Dispatch refuses a credential that names no person holding `approve` (403
   * `OAUTH_SCOPE_REQUIRED` or `SIGN_IN_REQUIRED`), and a `version` other than the one the review
   * would record (409 `REVIEW_VERSION_MOVED`); those refusals are thrown as Dispatch wrote them.
   * With an approval request open the review answers it and records the verdict on every
   * document the request named, but the answer is this document's review alone; its `ask_id`
   * names the request it answered.
   *
   * A build without the route answers no route each time the review is sent (`#noRoute`), and
   * this then throws `reviewNoRouteRefusal`'s refusal. No other review route is asked, since the
   * review route such a build serves takes no version.
   */
  async reviewArtifact(
    artifactID: string,
    version: number,
    input: { readonly state: "approved" | "changes_requested"; readonly reason?: string }
  ): Promise<ArtifactReview> {
    const path = ["api", "v1", "artifacts", artifactID, "versions", String(version), "reviews"];
    // Built from the two members the route takes, so nothing else a caller's object carries can
    // reach the wire; JSON leaves out a reason that is undefined.
    const body = { state: input.state, reason: input.reason };
    try {
      // A person's sign-in on this machine approves only through Cipher's proxy; the review is
      // the same keyed write either way, its 2xx read into the same row.
      const route = await this.#credential.approveRoute?.(this.#credentialSignal(this.#signal));
      const via = route === undefined ? undefined : this.#throughProxy(route, this.#url(path));
      return await this.#write("POST", path, WRITE_ROWS.reviewArtifact, body, { via });
    } catch (error) {
      throw this.#refusalOfNoRoute(error, path, (route) =>
        reviewNoRouteRefusal(this.#baseUrl, route, version)
      );
    }
  }

  /**
   * How a write to `url` goes through Cipher's proxy on `route`, the approve route of a person's
   * sign-in (`RequestCredential.approveRoute`): the proxy's URL with the placeholder as its
   * password, the route's authority as the one certificate the proxy is trusted by, and a
   * credential that presents `Authorization: DPoP <placeholder>` in place of the client's own,
   * which the proxy swaps for the approve token it holds and signs a proof for with its own key.
   * No proof of any key of this process goes with it: the token is bound to the broker pod's key,
   * and the proxy replaces any proof a request carries. That credential has no identity, since
   * every attempt presents the one placeholder, and no `stale`, since a dead token is the proxy's
   * to answer. Every attempt of the write (`#sendWrite`) presents the same, under the write's one
   * `Idempotency-Key`, which the proxy passes on, and notes it among the tokens this client sent,
   * as it notes every presentation, so an excerpt of any answer redacts it. The proxy reaches the
   * hosts the route names, on https and port 443 alone, so a Dispatch URL of any other host,
   * scheme or port is refused here and nothing is sent.
   */
  #throughProxy(route: ApproveRoute, url: string): ProxyTransport {
    const target = new URL(url);
    if (
      target.protocol !== "https:" ||
      target.port !== "" ||
      !route.hosts.includes(target.hostname)
    ) {
      throw new LoggableError(
        `a person's review goes through Cipher's proxy, which reaches https://${route.hosts.join(" and https://")} alone, and the Dispatch URL is ${urlForText(this.#baseUrl, DISPATCH_URL_SETTING)}, so no review was sent`
      );
    }
    const proxy = new URL(route.proxyUrl);
    proxy.username = "agent";
    proxy.password = route.placeholder;
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: () => ({ Authorization: `DPoP ${route.placeholder}` }),
      secret: () => route.placeholder,
    };
    // Bun's fetch takes the proxy and the authority per request (`proxy`, `tls.ca`), so this one
    // request trusts the proxy's authority and nothing else of this process does.
    return { credential, init: { proxy: proxy.toString(), tls: { ca: route.caCertificate } } };
  }

  /**
   * Records one result of one check of an issue's Verification document, and answers it beside
   * the pull-request body line it implies. `issue` is the issue the result is recorded on; the
   * check may be one of an issue above it, and the result is recorded against that issue.
   */
  async recordResult(issue: string, input: RecordCheckResultInput): Promise<CheckResultRecorded> {
    return this.#write(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "results"],
      WRITE_ROWS.recordResult,
      input
    );
  }

  /**
   * Records a CI job's result of one check of an issue's Verification document whose ci_check
   * names the job, under the job's results token, and answers it beside the pull-request body
   * line it implies. The body is built from the four members the route takes from a results
   * token, so nothing else a caller's object carries reaches the wire: Dispatch takes who
   * recorded the result, the commit's branch and the job from the credential and from GitHub.
   * JSON leaves out an `observed` that is undefined.
   */
  async recordCIJobResult(
    issue: string,
    input: Pick<RecordCheckResultInput, "check" | "commit" | "verdict" | "observed">
  ): Promise<CheckResultRecorded> {
    const { check, commit, verdict, observed } = input;
    const body = { check, commit, verdict, observed };
    return this.#write(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "results"],
      WRITE_ROWS.recordCIJobResult,
      body
    );
  }

  /**
   * The dry run of `recordResult`: Dispatch refuses the body as the record refuses it before it
   * writes, and answers the pull-request body line the record would answer, recording nothing.
   * An answer Dispatch kept no copy of is asked for again (`#writingNothing`).
   */
  async dryRunResult(issue: string, input: RecordCheckResultInput): Promise<CheckResultDryRun> {
    const path = ["api", "v1", "issues", await this.#resolveIssue(issue), "results", "dry-run"];
    try {
      return await this.#writingNothing<CheckResultDryRun>(path, WRITE_ROWS.dryRunResult, input);
    } catch (error) {
      throw this.#refusalOfNoRoute(error, path, (route) =>
        resultDryRunNoRouteRefusal(this.#baseUrl, route)
      );
    }
  }

  /** Each check's complete results, compared with each open head or the final head of the last
   *  pull request to merge. A branch result must hold its work's merge. The newest such result
   *  decides, unless a pass and a fail both count at that commit. A live check reads its
   *  program's current deploy. `at` names one 40-hex commit and leaves today's deploy out. */
  async issueResults(issue: string, at?: string): Promise<IssueCheckResults> {
    return this.#json(
      "GET",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "results"],
      undefined,
      at === undefined ? undefined : { at }
    );
  }

  /** Record a build using the credential's own writer, never an actor selected in the body. */
  async recordDeploy(input: DeployInput): Promise<DeployResponse> {
    return this.#write("POST", ["api", "v1", "deploys"], WRITE_ROWS.recordDeploy, input);
  }

  /** Each program's current deploy, including its source and the GitHub attempt when known. */
  async deploys(repo: string): Promise<DeploysResponse> {
    return this.#json("GET", ["api", "v1", "deploys"], undefined, { repo });
  }

  /**
   * Records that one stored result is wrong, with the reason, and answers the retraction. The
   * result itself is neither changed nor deleted: a reader of the results sees the result and
   * its retraction together. The session that recorded the result, a session holding the
   * Dispatch claim of the issue it was recorded on, and a caller whose credential carries a
   * person may each retract it, a role's result or a CI job's alike.
   */
  async retractResult(
    resultID: string,
    input: CheckResultRetractionInput
  ): Promise<CheckResultRetraction> {
    return this.#write(
      "POST",
      ["api", "v1", "results", resultID, "retract"],
      WRITE_ROWS.retractResult,
      input
    );
  }

  async editAsk(id: string, input: EditAskInput): Promise<AskWithTrees> {
    return this.#write("PATCH", ["api", "v1", "asks", id], WRITE_ROWS.editAsk, input);
  }

  async comment(issue: string, input: CreateCommentInput): Promise<Advised<CommentWriteResponse>> {
    return this.#write(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "comments"],
      WRITE_ROWS.comment,
      input
    );
  }

  /** Every ask on an issue (all states unless narrowed). */
  async listIssueAsks(issue: string, state?: "all" | "open" | "answered"): Promise<AskWithTrees[]> {
    return this.#json(
      "GET",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "asks"],
      undefined,
      state === undefined ? undefined : { state }
    );
  }

  async getArtifactAsks(id: string, state?: "all" | "open" | "answered"): Promise<AskWithTrees[]> {
    return this.#json(
      "GET",
      ["api", "v1", "artifacts", id, "asks"],
      undefined,
      state === undefined ? undefined : { state }
    );
  }

  async artifactAsk(id: string, input: CreateAskInput): Promise<Advised<AskWithTrees>> {
    return this.#write(
      "POST",
      ["api", "v1", "artifacts", id, "asks"],
      WRITE_ROWS.artifactAsk,
      input
    );
  }

  async suggest(
    issue: string,
    input: Omit<CreateCommentInput, "body" | "suggestion"> & {
      readonly body?: string;
      readonly replace_with: string;
    }
  ): Promise<Advised<Comment>> {
    const { replace_with, ...comment } = input;
    return this.#write(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "comments"],
      WRITE_ROWS.suggest,
      {
        ...comment,
        suggestion: { replace_with },
      }
    );
  }

  async getArtifactComments(id: string): Promise<Comment[]> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "comments"]);
  }

  async artifactComment(
    id: string,
    input: CreateCommentInput
  ): Promise<Advised<CommentWriteResponse>> {
    return this.#write(
      "POST",
      ["api", "v1", "artifacts", id, "comments"],
      WRITE_ROWS.artifactComment,
      input
    );
  }

  async artifactSuggest(
    id: string,
    input: Omit<CreateCommentInput, "body" | "suggestion"> & {
      readonly body?: string;
      readonly replace_with: string;
    }
  ): Promise<Advised<Comment>> {
    const { replace_with, ...comment } = input;
    return this.#write(
      "POST",
      ["api", "v1", "artifacts", id, "comments"],
      WRITE_ROWS.artifactSuggest,
      {
        ...comment,
        suggestion: { replace_with },
      }
    );
  }

  async message(issue: string, input: CreateMessageInput): Promise<Advised<Message>> {
    return this.#write(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "messages"],
      WRITE_ROWS.message,
      input
    );
  }

  /**
   * `POST /api/v1/agents/{session_id}/messages`: one message into a session's own conversation,
   * which belongs to no issue. The route is the person's, so the credential must hold the
   * `person` scope. It answers once the message's first delivery attempt has settled, with that
   * attempt on the message.
   */
  async agentMessage(sessionId: string, input: CreateAgentMessageInput): Promise<Message> {
    return this.#write(
      "POST",
      ["api", "v1", "agents", sessionId, "messages"],
      WRITE_ROWS.agentMessage,
      input
    );
  }

  /**
   * `POST /api/v1/messages/{id}/reply`: the targeted session's reply to the delivery it
   * received. It takes no issue, so it is the route that answers a human's direct message -
   * a conversation Dispatch keeps without one - and the server settles the delivery attempt
   * with the reply it inserts. `followUp` asks to post more once the attempt is answered
   * (`?follow_up=true`); without it an answered attempt hands back its stored reply and posts
   * nothing. The result's `duplicate` says the route posted nothing.
   */
  async messageReply(
    id: string,
    input: MessageReplyInput,
    options: { readonly followUp?: boolean } = {}
  ): Promise<MessageReplyResult> {
    return this.#write(
      "POST",
      ["api", "v1", "messages", id, "reply"],
      WRITE_ROWS.messageReply,
      input,
      { query: options.followUp === true ? { follow_up: "true" } : undefined }
    );
  }

  /**
   * `POST /api/v1/{messages|comments}/{id}/reply` as the session a targeted delivery reached: its
   * reply to that delivery attempt, or why it could not honour it (`postDeliveryReply`, whose
   * body names the session as `actor` and the `attempt`). `followUp` is `messageReply`'s own
   * option: the attempt is settled by the first reply Dispatch takes, so a second answer to it -
   * the output a waiting reply promised - is posted only when it asks (`?follow_up=true`).
   */
  async deliveryReply(
    resource: "message" | "comment",
    id: string,
    input: Readonly<Record<string, unknown>>,
    options: { readonly followUp?: boolean } = {}
  ): Promise<void> {
    const collection = resource === "message" ? "messages" : "comments";
    await this.#write(
      "POST",
      ["api", "v1", collection, id, "reply"],
      WRITE_ROWS.deliveryReply,
      input,
      { query: options.followUp === true ? { follow_up: "true" } : undefined }
    );
  }

  async getMessage(issue: string, id: string): Promise<MessageRead> {
    return this.#json("GET", [
      "api",
      "v1",
      "issues",
      await this.#resolveIssue(issue),
      "messages",
      id,
    ]);
  }

  /** `GET /api/v1/messages/{id}`: the conversation a message belongs to, by any message id in
   *  it - the root and every reply. It takes no issue, so it reads a human's direct message to
   *  a session and the replies to it, which belong to none. It reads as `session`; which
   *  threads a session may read is the route's rule (`GET /api/v1` describes it). A caller with
   *  no session of its own names none, as `messageHistory` does: a thread on an issue follows
   *  the issue's rule, and a direct one is then refused (400 SESSION_REQUIRED). */
  async getMessageThread(id: string, session?: string): Promise<MessageRead> {
    return this.#json(
      "GET",
      ["api", "v1", "messages", id],
      undefined,
      session === undefined ? undefined : { session }
    );
  }

  /** `PATCH /api/v1/messages/{id}`: the author replaces the body of its own issue message whole.
   *  Dispatch keeps the body it replaces, which `messageHistory` reads. */
  async editMessage(
    id: string,
    input: { readonly body: string; readonly actor: Actor }
  ): Promise<Message> {
    return this.#write("PATCH", ["api", "v1", "messages", id], WRITE_ROWS.editMessage, input);
  }

  /** `DELETE /api/v1/messages/{id}`: the author withdraws its own issue message. The row and its
   *  place in the thread stay, and the answer carries `deleted_at`, `deleted_by` and no body. */
  async deleteMessage(id: string, actor: Actor): Promise<Message> {
    return this.#json("DELETE", ["api", "v1", "messages", id], { actor });
  }

  /** `GET /api/v1/messages/{id}/history`: each entry an edit or a delete of the message wrote,
   *  newest first: with the body an edit replaced while the message stands, and with the deleted
   *  mark and no body once it is deleted. It is read under
   *  `GET /api/v1/messages/{id}`'s rule, so the session goes in the query as `messageBlocks`
   *  sends it. */
  async messageHistory(id: string, session?: string): Promise<PostHistoryRead> {
    return this.#json(
      "GET",
      ["api", "v1", "messages", id, "history"],
      undefined,
      session === undefined ? undefined : { session }
    );
  }

  /** `GET /api/v1/issues/{key}/messages`: the issue's message threads, each root with its
   *  replies. It is read to resolve a `dispatch://KEY/message/<id>` reference whose id is a
   *  prefix; a reference carrying a full id needs no listing. */
  async issueMessages(issue: string): Promise<MessageRead[]> {
    return this.#json("GET", ["api", "v1", "issues", await this.#resolveIssue(issue), "messages"]);
  }

  /** `POST /api/v1/messages/{id}/deliveries/{attempt}/accept`: this session records that it took
   *  that attempt of the message as its user's own turn, and gets back the attempt with the
   *  message's stored body, which is what it injects. Dispatch allows one acceptance per message,
   *  of a person's own direct Send or Aside to this session (no issue, no broadcast), of its
   *  latest attempt, which a person asked for within the last minute and which did not fail; any
   *  refusal throws a `DispatchServiceError` naming the check. */
  async acceptMessageDelivery(
    id: string,
    attempt: number,
    input: { readonly actor: Actor }
  ): Promise<AcceptedMessageDelivery> {
    return this.#write(
      "POST",
      ["api", "v1", "messages", id, "deliveries", String(attempt), "accept"],
      WRITE_ROWS.acceptMessageDelivery,
      input
    );
  }

  async artifact(
    issue: string,
    input: CreateArtifactInput
  ): Promise<
    Advised<ArtifactUploadResponse> & {
      readonly artifact: Artifact & { readonly issue_key: string };
    }
  > {
    return this.#upload(
      ["api", "v1", "issues", await this.#resolveIssue(issue), "artifacts"],
      input,
      WRITE_ROWS.artifact
    );
  }

  async getArtifact(id: string): Promise<Artifact> {
    return this.#json("GET", ["api", "v1", "artifacts", id]);
  }

  async docRead(id: string): Promise<ArtifactText>;
  async docRead(id: string, version: number): Promise<ArtifactVersionText>;
  async docRead(id: string, version?: number): Promise<ArtifactText | ArtifactVersionText>;
  async docRead(id: string, version?: number): Promise<ArtifactText | ArtifactVersionText> {
    return version === undefined
      ? this.#json("GET", ["api", "v1", "artifacts", id, "text"])
      : this.#json("GET", ["api", "v1", "artifacts", id, "versions", String(version)]);
  }

  /** `GET /api/v1/artifacts/{id}/text?blocks=true`: the live document's markdown, its token, its
   *  version and its blocks in one answer, every part of it rendered from the one tree, so a write
   *  by another session between two reads cannot leave the blocks indexing markdown they were not
   *  computed over. A Dispatch older than the parameter answers the text without `blocks`. */
  async docReadWithBlocks(id: string): Promise<ArtifactText> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "text"], undefined, { blocks: true });
  }

  /** One version of an uploaded file as Dispatch stores it: `GET /artifacts/{id}/versions/{n}`
   * answers a file with its bytes under its own MIME type, never a JSON envelope. */
  async fileVersion(id: string, version: number): Promise<ArtifactFileVersion> {
    const url = this.#url(["api", "v1", "artifacts", id, "versions", String(version)]);
    return this.#noRoute(async () => {
      const { answer: response } = await this.#send("GET", url, undefined, "*/*");
      if (!response.ok) return this.#response<never>("GET", url, response);
      return {
        mime: response.headers.get("Content-Type") ?? "application/octet-stream",
        bytes: new Uint8Array(await response.arrayBuffer()),
      };
    });
  }

  async artifactBlocks(id: string): Promise<ArtifactBlock[]> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "blocks"]);
  }

  /** `GET /api/v1/issues/{key}/artifacts/{slug}/blocks`: the blocks of one issue document named
   *  by its slug (`spec` for the issue's spec), with no read of the issue to resolve its id. */
  async issueArtifactBlocks(issue: string, slug: string): Promise<ArtifactBlock[]> {
    return this.#json("GET", ["api", "v1", "issues", issue, "artifacts", slug, "blocks"]);
  }

  /** `GET /api/v1/projects/{key}/artifacts/{slug}/blocks`: the same for a project document. */
  async projectArtifactBlocks(project: string, slug: string): Promise<ArtifactBlock[]> {
    return this.#json("GET", ["api", "v1", "projects", project, "artifacts", slug, "blocks"]);
  }

  /** `GET /api/v1/issues/{key}/coverage`: what each requirement of the issue's spec lacks, with
   *  one `gaps` line per gap. Nothing is ever refused for a gap. */
  async issueCoverage(key: string): Promise<IssueCoverage> {
    return this.#json("GET", ["api", "v1", "issues", await this.#resolveIssue(key), "coverage"]);
  }

  /** `GET /api/v1/comments/{id}/blocks`: the comment's current blocks with their ids, text,
   *  references and UTF-16 offsets into its stored body, and the comment's revision. */
  async commentBlocks(id: string): Promise<ItemBlocks> {
    return this.#json("GET", ["api", "v1", "comments", id, "blocks"]);
  }

  /** `GET /api/v1/messages/{id}/blocks`: the message's current blocks, read under
   *  `GET /api/v1/messages/{id}`'s rule, so a direct message is read as `session`, a session in
   *  its conversation (`getMessageThread`). */
  async messageBlocks(id: string, session?: string): Promise<ItemBlocks> {
    return this.#json(
      "GET",
      ["api", "v1", "messages", id, "blocks"],
      undefined,
      session === undefined ? undefined : { session }
    );
  }

  /**
   * `POST /api/v1/artifacts/{id}/edits`: an edit of a document's text. `key` is the caller's own
   * `Idempotency-Key` for the edit, where the caller has one intent across calls and processes —
   * AskGate's one append per decision, which a resumed session must not write twice — and replaces
   * the key this call would mint for itself. It must match `IDEMPOTENCY_KEY_PATTERN`.
   */
  async docEdit(
    id: string,
    input: EditArtifactInput,
    key?: string
  ): Promise<Advised<EditArtifactResponse>> {
    return this.#write("POST", ["api", "v1", "artifacts", id, "edits"], WRITE_ROWS.docEdit, input, {
      key,
    });
  }

  async nameArtifactVersion(id: string, input: CreateVersionInput): Promise<Version> {
    return this.#write(
      "POST",
      ["api", "v1", "artifacts", id, "versions"],
      WRITE_ROWS.nameArtifactVersion,
      input
    );
  }

  async getArtifactEvents(id: string, after = 0, limit = 200): Promise<DispatchEvent[]> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "events"], undefined, { after, limit });
  }

  async getArtifactReferences(id: string): Promise<ArtifactReferences> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "references"]);
  }

  async getAsk(id: string): Promise<AskRead> {
    return this.#json("GET", ["api", "v1", "asks", id]);
  }

  /** A bearer follows only its own session: the body names it and the path repeats it. */
  async followAsk(id: string, sessionId: string, actor: Actor): Promise<void> {
    await this.#json("PUT", ["api", "v1", "asks", id, "followers", sessionId], { actor });
  }

  async unfollowAsk(id: string, sessionId: string, actor: Actor): Promise<void> {
    await this.#json("DELETE", ["api", "v1", "asks", id, "followers", sessionId], { actor });
  }

  async getComment(id: string): Promise<CommentRead> {
    return this.#json("GET", ["api", "v1", "comments", id]);
  }

  /** Resolves a comment thread as `actor`; the server takes no reason and any actor may resolve. */
  async resolveComment(id: string, actor: Actor): Promise<Comment> {
    return this.#write(
      "POST",
      ["api", "v1", "comments", id, "resolve"],
      WRITE_ROWS.resolveComment,
      {
        actor,
      }
    );
  }

  /** `PATCH /api/v1/comments/{id}`: the author replaces the comment's body whole. A Dispatch of
   *  this release keeps the body it replaces, which `commentHistory` reads. On an older build a
   *  person's own credential can edit that person's own comment here too, and that build keeps
   *  nothing of the body it replaces (`throughPostCorrections`). */
  async editComment(
    id: string,
    input: { readonly body: string; readonly actor: Actor }
  ): Promise<Comment> {
    return this.#write("PATCH", ["api", "v1", "comments", id], WRITE_ROWS.editComment, input);
  }

  /** `DELETE /api/v1/comments/{id}`: the author withdraws its own comment. The row and its place
   *  in the thread stay, and the answer carries `deleted_at`, `deleted_by` and no body. */
  async deleteComment(id: string, actor: Actor): Promise<Comment> {
    return this.#json("DELETE", ["api", "v1", "comments", id], { actor });
  }

  /** `GET /api/v1/comments/{id}/history`: each entry an edit or a delete of the comment wrote,
   *  newest first: with the body an edit replaced while the comment stands, and with the deleted
   *  mark and no body once it is deleted. Any authenticated caller
   *  reads it, as it reads `getComment`. */
  async commentHistory(id: string): Promise<PostHistoryRead> {
    return this.#json("GET", ["api", "v1", "comments", id, "history"]);
  }

  async getComments(issue: string): Promise<Comment[]> {
    return this.#json("GET", ["api", "v1", "issues", await this.#resolveIssue(issue), "comments"]);
  }

  /** `POST /api/v1/issues/{key}/handoffs/{phase}`: records `body`, the handoff's exact text, as
   *  the next version of the issue's phase, written by `actor`, the session it names. The text
   *  travels as one JSON string, so the server stores the very characters it was given. */
  async writeHandoff(
    issue: string,
    phase: string,
    input: { readonly actor: Actor; readonly body: string }
  ): Promise<HandoffSummary> {
    return this.#write(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "handoffs", phase],
      WRITE_ROWS.writeHandoff,
      input
    );
  }

  /** `GET /api/v1/issues/{key}/handoffs`: every handoff of the issue, newest first, without bodies. */
  async listHandoffs(issue: string): Promise<HandoffSummary[]> {
    return this.#json("GET", ["api", "v1", "issues", await this.#resolveIssue(issue), "handoffs"]);
  }

  /** `GET /api/v1/issues/{key}/handoffs/{phase}/latest`: the phase's newest handoff with its body;
   *  404 HANDOFF_NOT_FOUND when the phase has none. */
  async latestHandoff(issue: string, phase: string): Promise<Handoff> {
    return this.#json("GET", [
      "api",
      "v1",
      "issues",
      await this.#resolveIssue(issue),
      "handoffs",
      phase,
      "latest",
    ]);
  }

  /** `GET /api/v1/handoffs/{id}`: one handoff by id, with its body. */
  async handoff(id: string): Promise<Handoff> {
    return this.#json("GET", ["api", "v1", "handoffs", id]);
  }

  /** `GET /api/v1/issues/{key}/steps`: every run of each step of the issue, task by task in the
   *  order each task first ran, each with the attempt the read computes. An issue no step ran on
   *  answers with no task; a key Dispatch does not hold is `404 NOT_FOUND`, as the issue read. */
  async listSteps(issue: string): Promise<IssueSteps> {
    return this.#json("GET", ["api", "v1", "issues", await this.#resolveIssue(issue), "steps"]);
  }

  /** `GET /api/v1/projects/{key}/plans`: each finished issue of the project, newest close first,
   *  with its plan, each retro's rating, its time, its count of failed steps, and the experiment
   *  and the variant the step records of its newest run name, null where they name none. `since`
   *  and `until` bound the close to `[since, until)`, and a bound left out is not sent. */
  async listProjectPlans(
    project: string,
    window: { readonly since?: string | undefined; readonly until?: string | undefined } = {}
  ): Promise<ProjectPlans> {
    return this.#json("GET", ["api", "v1", "projects", project, "plans"], undefined, window);
  }

  async getIssueReferences(issue: string): Promise<IssueReferencesAnswer> {
    return this.#json(
      "GET",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "references"],
      undefined,
      { versions: "latest" }
    );
  }

  async getReferences(query: GraphReferencesQuery): Promise<GraphReferences> {
    return this.#json("GET", ["api", "v1", "references"], undefined, {
      ...("to" in query ? { to: query.to } : { from: query.from }),
      ...(query.kind === undefined || query.kind.length === 0
        ? {}
        : { kind: query.kind.join(",") }),
      ...(query.since === undefined ? {} : { since: query.since }),
    });
  }

  /**
   * A write's answer, read whole (`writeAnswer`), read by one rule before any caller can use it.
   * Dispatch's keyed layer names the call's key back on every answer it gives a keyed write
   * (`keyedWrites`, packages/envoy/internal/dispatch/api/request_keys.go, held by
   * `TestEveryKeyedAnswerNamesItsKey`), on every `POST` and `PATCH` route of its table, and
   * Dispatch answers a write's 2xx only after the write committed. So:
   * - a 2xx the table settles on (`applied`: it names this call's key back, and its body, read
   *   whole, is JSON or a status that carries none) is Dispatch's own: its row
   *   (`dispatch-write-rows.ts`, the members the method's callers read, in the forms the route
   *   answers) is the call's answer, and a body that gives no row — JSON that is not the row, or
   *   none where the row needs one — is a landing whose answer was lost (`DISPATCH_ANSWER_LOST`),
   *   as is a replay whose first answer Dispatch did not keep (`answerNotKept`);
   * - every other 2xx — one that names another request's key back, or names none (a proxy's page,
   *   a server that is not Dispatch, a proxy or a cache that answers one write with another's, or
   *   one that drops the header), or whose body is empty or not JSON-typed — is no answer this
   *   client can take as Dispatch's to this call (`foreign-2xx`), whatever it holds, and the write
   *   may have reached Dispatch behind what answered it: the table ends it with its outcome
   *   unknown (`#endWrite`). A body cut short or broken off on the way is asked again under the
   *   key (`body-malformed`, `body-broken`). No member of such an answer is compared with what the
   *   call sent: an equal value tells this call's write from no other.
   * A route that stores nothing (`rowStores`) made nothing either way, so a 2xx of it that is not
   * its answer, or its `409 IDEMPOTENCY_KEY_COMMITTED`, is a `DispatchAnswerMissingError`; its
   * bodiless 204 or 205 the table settled on is its answer, "", as `#payloadOf` reads one, while
   * on a route that stores, such an answer gives no row and is a landing whose answer was lost.
   * The row is the answer as it came, so a caller reads every member Dispatch sent, the ones the
   * check names among them. Any other refusal is read as `#payloadOf` reads it.
   */
  #writePayload<T>(method: string, url: string, answer: WriteAnswer, write: WriteCall): T {
    const stores = rowStores(write.row);
    if (!answer.response.ok) {
      if (!stores && answer.echoed && answer.code === IDEMPOTENCY_KEY_COMMITTED)
        throw this.#missingWriteAnswer(method, url, answer.response, write, answer.text);
      return this.#payloadOf<T>(method, url, answer);
    }
    if (NULL_BODY_STATUSES.has(answer.response.status) && !stores)
      return this.#payloadOf<T>(method, url, answer);
    const value = answer.json?.value;
    const replayed = answer.response.headers.get(IDEMPOTENCY_REPLAYED_HEADER)?.trim() === "?1";
    if (answerNotKept(answer.response.status, replayed, value)) {
      throw this.#missingWriteAnswer(
        method,
        url,
        answer.response,
        write,
        answer.text,
        undefined,
        IDEMPOTENCY_ANSWER_NOT_KEPT
      );
    }
    const parsed = write.row.safeParse(value);
    if (!parsed.success)
      throw this.#missingWriteAnswer(
        method,
        url,
        answer.response,
        write,
        answer.text,
        parsed.error
      );
    return value as T;
  }

  /**
   * What a write's answer that is not its row is, by the one rule of a landing: a route that
   * stores nothing (`rowStores`) made nothing, so its replay whose first answer Dispatch did not
   * keep, naming the call's key back, is a `DispatchAnswerNotKeptError` (`code`
   * `IDEMPOTENCY_ANSWER_NOT_KEPT`), which `#writingNothing` asks once more under a new key, and any
   * other such answer is missing, a `DispatchAnswerMissingError`; an answer that names the call's
   * key back is Dispatch's word that
   * the write committed under it, a landing whose answer is gone (`code`). Any other (a 2xx that
   * names no key back or another request's, a `409 IDEMPOTENCY_KEY_COMMITTED` that names none) is
   * no verdict of Dispatch's on this write, which may have reached Dispatch behind what answered
   * it, so its outcome is unknown: a `DispatchWriteUnsettledError` whose last answer is that
   * answer as a `DispatchGatewayError`, naming `text`, the body that came, as `bodyForText` names
   * it.
   */
  #missingWriteAnswer(
    method: string,
    url: string,
    response: Response,
    write: WriteCall,
    text: string,
    cause?: unknown,
    code: DispatchLandedCode = DISPATCH_ANSWER_LOST
  ): Error {
    const shown = requestForText(this.#baseUrl, url, DISPATCH_URL_SETTING);
    if (!rowStores(write.row))
      return code === IDEMPOTENCY_ANSWER_NOT_KEPT && keyEchoed(response, write.key)
        ? new DispatchAnswerNotKeptError(response.status, method, shown)
        : new DispatchAnswerMissingError(response.status, method, shown, { cause });
    if (keyEchoed(response, write.key))
      return new DispatchWriteLandedError(code, response.status, method, shown, { cause });
    const names =
      keyNamedBack(response) === "" ? "naming no key back" : "naming another request's key back";
    return new DispatchWriteUnsettledError(
      this.#gatewayError(method, url, response, text, "the write's answer", {
        advice: "the write may have reached Dispatch behind what answered it",
        also: `${names}, `,
      }),
      write.progress.sent,
      write.key
    );
  }

  /**
   * A write under a key (`POST` or `PATCH`, `WriteCall`), with the status Dispatch answered, for a
   * route whose status says what it did: its body sent as JSON, and its 2xx read into `row`, the
   * members the method's callers read (`#writePayload`). The row is required, so a keyed write
   * that names none does not compile; a read, a PUT or a DELETE goes through `#json`, which takes
   * none. A POST whose row stores nothing (`rowStores`: a conversion, the related-item check, a
   * result's dry run) is sent as every write is, under one key with its retries, and never read as
   * a write that may have landed. `options.key` is the caller's own `Idempotency-Key`, where it has
   * one intent across calls; `options.via` sends the write through Cipher's proxy (`#throughProxy`).
   */
  async #writeAnswer<T>(
    method: WriteMethod,
    path: readonly string[],
    row: WriteRow,
    body?: unknown,
    options: WriteOptions = {}
  ): Promise<{ readonly status: number; readonly payload: T }> {
    const url = this.#url(path, options.query);
    const write = this.#writeCall(method, url, row, options.key);
    const answer = await this.#sendWrite(
      method,
      url,
      body === undefined ? undefined : { content: JSON.stringify(body), type: "application/json" },
      "application/json",
      undefined,
      write,
      options.via
    );
    return {
      status: answer.response.status,
      payload: this.#writePayload<T>(method, url, answer, write),
    };
  }

  /** `#writeAnswer`'s payload alone. */
  async #write<T>(
    method: WriteMethod,
    path: readonly string[],
    row: WriteRow,
    body?: unknown,
    options: WriteOptions = {}
  ): Promise<T> {
    return (await this.#writeAnswer<T>(method, path, row, body, options)).payload;
  }

  /** Every result CI jobs stored for checks of `project`'s spec files at `commit`, of `check`
   *  alone when it is given, newest first in Dispatch's record order. */
  async specCheckResults(
    project: string,
    commit: string,
    check?: string
  ): Promise<SpecCheckResults> {
    return this.#json("GET", ["api", "v1", "projects", project, "results"], undefined, {
      commit,
      check,
    });
  }

  /**
   * Records one CI job's result of one check of `project`'s spec files, and answers the stored
   * result. The body is built from the four members the route takes, so nothing else a caller's
   * object carries reaches the wire: Dispatch takes who wrote the result from the credential.
   * JSON leaves out an `observed` that is undefined.
   */
  async recordSpecCheckResult(
    project: string,
    input: SpecCheckResultInput
  ): Promise<SpecCheckResult> {
    const { commit, check, verdict, observed } = input;
    const body = { commit, check, verdict, observed };
    return this.#write(
      "POST",
      ["api", "v1", "projects", project, "results"],
      WRITE_ROWS.recordSpecCheckResult,
      body
    );
  }

  async #resolveIssue(issueReference: string): Promise<string> {
    if (!issueReference.includes("#")) return issueReference;
    let resolved = this.#resolvedIssues.get(issueReference);
    if (!resolved) {
      resolved = this.#json<{ key: string }>("GET", ["api", "v1", "issues", "resolve"], undefined, {
        ref: issueReference,
      }).then((resolution) => resolution.key);
      this.#resolvedIssues.set(issueReference, resolved);
    }
    try {
      return await resolved;
    } catch (error) {
      this.#resolvedIssues.delete(issueReference);
      throw error;
    }
  }
  /** A read, or a write sent once with no key (`PUT`, `DELETE`): its payload. It takes no row and
   *  no key: a keyed write goes through `#writeAnswer`. `signal` aborts it beside the client's own
   *  signal (`#send`). */
  async #json<T>(
    method: ReadMethod,
    path: readonly string[],
    body?: unknown,
    query?: object,
    signal?: AbortSignal
  ): Promise<T> {
    const url = this.#url(path, query);
    const encoded: EncodedBody | undefined =
      body === undefined ? undefined : { content: JSON.stringify(body), type: "application/json" };
    return this.#noRoute(async () => {
      const { answer: response } = await this.#send(method, url, encoded, undefined, signal);
      return this.#response<T>(method, url, response);
    }, signal);
  }

  /**
   * A read's answer, with an answer of no route for the request (`404 NOT_FOUND`, "no route for
   * …") sent again, as the same request, after each of `NO_ROUTE_RETRY_DELAYS_MS`: while a release
   * rolls out, two builds of Dispatch answer, and the next attempt may reach the build that serves
   * the route. After the last attempt it throws `DispatchNoRouteError`. An abort of the client's
   * signal, or of the call's own `signal`, during a wait rejects at once with the abort's reason
   * and sends nothing more. A write asks no route again in its own loop (`#sendWrite`).
   *
   * A client built with `retryNoRoute: false` runs `attempt` once instead and throws its answer
   * of no route as it came (the `DispatchServiceError` `isNoRoute` recognises), so a caller with
   * an answer of its own to a missing route — a back-off of its own — reaches it at once.
   */
  async #noRoute<T>(attempt: () => Promise<T>, signal?: AbortSignal): Promise<T> {
    if (!this.#retryNoRoute) return await attempt();
    return await retryNoRoute(attempt, isNoRoute, {
      exhausted: () => new DispatchNoRouteError(),
      signal: signal === undefined ? this.#signal : AbortSignal.any([this.#signal, signal]),
      sleep: this.#sleep,
    });
  }

  /** What the catch of a `POST` of `path` throws: the refusal `refuse` writes for that route where
   *  `error` is an answer of no route each time it was sent (a build older than the route), else
   *  `error` as it came. It returns rather than throws, so the throw stays at the catch. */
  #refusalOfNoRoute(
    error: unknown,
    path: readonly string[],
    refuse: (route: string) => DispatchServiceError
  ): unknown {
    if (!(error instanceof DispatchNoRouteError) && !isNoRoute(error)) return error;
    return refuse(`POST /${path.map((segment) => encodeURIComponent(segment)).join("/")}`);
  }

  /**
   * A write's one key and what its attempts have shown, made once per call, so every attempt of
   * the call goes under one `Idempotency-Key`: the caller's own, where it has one intent across
   * calls and processes, else one minted for this call alone.
   */
  #writeCall(method: WriteMethod, url: string, row: WriteRow, key: string | undefined): WriteCall {
    return {
      key: key ?? crypto.randomUUID(),
      request: `${method} ${requestForText(this.#baseUrl, url, DISPATCH_URL_SETTING)}`,
      row,
      progress: { sent: 0, running: false },
    };
  }

  /** An upload to an owner's artifact route: a file as multipart, and everything else as JSON -
   *  inline content, and a name alone, which makes an empty document. A base the upload names
   *  travels in the JSON body as `precondition`, and in a multipart form as the field
   *  `precondition` holding that same JSON object's text, which is how the route reads it. */
  async #upload<T>(
    path: readonly string[],
    input: CreateArtifactInput,
    row: WriteRow,
    key?: string
  ): Promise<T> {
    if (input.file === undefined) {
      return this.#write("POST", path, row, input, { key });
    }
    const form = new FormData();
    form.set("name", input.name);
    if (input.summary !== undefined) form.set("summary", input.summary);
    if (input.precondition !== undefined)
      form.set("precondition", JSON.stringify(input.precondition));
    if (input.actor !== undefined) form.set("actor", JSON.stringify(input.actor));
    form.set("file", input.file, input.name);
    return this.#form(path, form, row, key);
  }

  /**
   * A multipart write, a `POST` under a key read into `row` as every write is (`#writeAnswer`).
   * The form is encoded here, once, before the first attempt: every encoding of a `FormData` draws
   * a new boundary, and a retry whose bytes differ from its first attempt's is a different request
   * to Dispatch, which refuses a key reused for one (`422 KEY_REUSED`), and every attempt of the
   * call carries its one key.
   */
  async #form<T>(
    path: readonly string[],
    form: FormData,
    row: WriteRow,
    key: string | undefined
  ): Promise<T> {
    const url = this.#url(path);
    const encoded = new Response(form);
    const type = encoded.headers.get("Content-Type") ?? "multipart/form-data";
    const body: EncodedBody = { content: new Uint8Array(await encoded.arrayBuffer()), type };
    const write = this.#writeCall("POST", url, row, key);
    const answer = await this.#sendWrite("POST", url, body, "application/json", undefined, write);
    return this.#writePayload<T>("POST", url, answer, write);
  }

  /**
   * A `POST` of JSON to a route that writes nothing (`markdownRoundTrip`, `fileRoundTrip`, `related`,
   * `dryRunResult`), `row` its row of the write table, which stores nothing (`rowStores`). It is
   * sent as a write is, under a key of its own, so an attempt with no definitive answer is asked
   * again under that key (`#writeAnswer`). Such a repeat can come back as a replayed 2xx reading
   * `IDEMPOTENCY_ANSWER_NOT_KEPT`: an earlier attempt ran, and Dispatch kept no copy of its
   * answer. `#writePayload` reads that replay of a route that stores nothing as a
   * `DispatchAnswerNotKeptError`, no landing, since this request wrote nothing: it is asked once
   * more under a new key, which `#writeCall` mints, and which runs it afresh and can double
   * nothing. That ask's answer is the call's, and a second such replay is thrown as it came
   * rather than asked again, so the call is asked under at most two keys. Every other error, a
   * `DispatchAnswerMissingError` among them, is thrown as it came.
   */
  async #writingNothing<T>(path: readonly string[], row: WriteRow, body: unknown): Promise<T> {
    for (let keys = 1; ; keys++) {
      try {
        return await this.#write<T>("POST", path, row, body);
      } catch (error) {
        if (!(error instanceof DispatchAnswerNotKeptError) || keys === 2) throw error;
      }
    }
  }

  /** Send the caller's route once through `#send`, without `#noRoute`. Read non-2xx answers as
   *  other commands do. For a 2xx, check its bytes for echoed credentials before returning
   *  anything. An attachment is stored content, never JSON to parse. Without that disposition,
   *  decode and parse typed JSON where possible. Everything else returns the original bytes,
   *  never decoded. */
  async #route(
    method: "GET" | DispatchApiMethod,
    path: string,
    body?: string
  ): Promise<RouteAnswer> {
    const url = new URL(path.slice(1), `${this.#baseUrl}/`).toString();
    const encoded = body === undefined ? undefined : { content: body, type: "application/json" };
    const {
      answer: response,
      presented,
      stalePresentation,
    } = await this.#send(method, url, encoded);
    if (!response.ok) return { payload: await this.#response(method, url, response) };

    const bytes = new Uint8Array(await response.arrayBuffer());
    const credentials = new Set(this.#sent.with(this.token));
    // SentTokens keeps token values, not complete headers or request-specific proofs.
    for (const headers of [presented, stalePresentation]) {
      if (headers === undefined) continue;
      for (const [name, value] of Object.entries(headers)) {
        const header = name.toLowerCase();
        if (header !== "authorization" && header !== "dpop") continue;
        const credential = value.trim();
        if (credential !== "") credentials.add(credential);
      }
    }
    // Every credential is ASCII, so it is in the decoded text only where it is in the bytes.
    const raw = Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    for (const credential of credentials) {
      if (raw.includes(credential)) throw new DispatchCredentialEchoError(response.status);
    }
    if (!isAttachment(response) && isJson(response)) {
      const text = new TextDecoder().decode(bytes);
      const json = parsedJson(response, text);
      if (json !== undefined) {
        if (text.includes("\\") && jsonHoldsCredential(json.value, credentials)) {
          throw new DispatchCredentialEchoError(response.status);
        }
        return { payload: this.#payloadOf(method, url, { response, text, json }) };
      }
    }
    return { body: bytes, type: response.headers.get("Content-Type") ?? undefined };
  }

  /**
   * A read, a PUT, a DELETE or `sendRoute`'s write: it presents the credential and sends the
   * request once (`#sendOnce`), and once more where Dispatch answers that the token presented is
   * not live (`tokenNotLive`, its `401 BROKER_TOKEN_NOT_ACTIVE`) and the credential can be told so
   * (`RequestCredential.stale`): the broker ended that token before it lapsed, as a person's
   * revoke of their sign-in does. The credential is told which presentation was dead, drops that
   * token alone, and presents the token it holds now, trading at the broker where it holds none;
   * the request goes again. Dispatch authenticates a request before any handler
   * runs (`brokerTokenActor`, packages/envoy/internal/dispatch/api/broker_token.go), so that 401
   * says it did nothing. A second such answer is returned as any other refusal is. A trade the
   * broker refuses throws out of the second presentation, so the call fails in the broker's own
   * words, which name `agent-secrets login` after a revoke. It returns the answer and the headers
   * of each presentation (`SentRequest`). Every other write (POST or PATCH) goes through
   * `#write` instead.
   */
  async #send(
    method: string,
    url: string,
    body?: EncodedBody,
    accept = "application/json",
    signal?: AbortSignal
  ): Promise<SentRequest> {
    const sent = await this.#sendOnce(method, url, body, accept, signal);
    if (this.#credential.stale === undefined || !(await tokenNotLive(sent.answer))) {
      return sent;
    }
    // The presentation the dead token went out in, so the credential drops that token alone.
    this.#credential.stale(undefined, sent.presented);
    return {
      ...(await this.#sendOnce(method, url, body, accept, signal)),
      stalePresentation: sent.presented,
    };
  }

  /** One request of `#send`'s (a read, a PUT, a DELETE or `sendRoute`'s write), presenting the
   *  credential for it, and the headers it presented; `signal` aborts it beside the client's own
   *  signal. */
  async #sendOnce(
    method: string,
    url: string,
    body: EncodedBody | undefined,
    accept: string,
    signal: AbortSignal | undefined
  ): Promise<SentRequest> {
    // The call's own deadline and the client's, which the credential is given too, less the
    // request's reserve (`#credentialSignal`): a broker credential waiting on a pod's missing
    // registration stops at it, naming the session, and one whose renewal goes unanswered answers
    // with the live token in hand while the request can still be sent with it.
    const limit = signal === undefined ? this.#signal : AbortSignal.any([this.#signal, signal]);
    const presented = await this.#credential.headers(
      method,
      url,
      undefined,
      this.#credentialSignal(limit)
    );
    this.#sent.note(presented);
    const answer = await this.#fetch(url, {
      method,
      headers: {
        Accept: accept,
        ...(body === undefined ? {} : { "Content-Type": body.type }),
        ...presented,
      },
      signal: limit,
      ...(body === undefined ? {} : { body: body.content }),
    });
    return { answer, presented };
  }

  /**
   * A write (POST or PATCH): every attempt under the call's one key (`WriteCall`), with its
   * outcome read from the transition table (`writeStep`, write-outcome.ts) and nothing else. Each
   * presentation, each attempt with its answer's body, each wait the call is stopped in, and the
   * end of the asking is one `WriteEvent`: the answer's (`answerEvent`), the credential's
   * (`credentialEvent`), the failure's (a silence cut, a transport failure that went out or reached
   * nothing, a body that broke off), the stop's (`stoppedEvent`), or `exhausted`. The table names
   * the write's state after it and what follows:
   * - `settle`: the answer is the call's (its row, the refusal Dispatch decided, or a write
   *   Dispatch made whose answer is gone), handed back read whole, for `#writePayload`;
   * - `retry`: asked again on `writeRetry`'s pacing (or the last answer's `Retry-After`, whether or
   *   not its body came whole), while attempts and time are left. With none left, the next event
   *   is `exhausted`;
   * - `retry-at-once`: the credential is told its token is not live (once per call), presents
   *   afresh, and the write goes again;
   * - `retry-route`: the write goes again after each of `NO_ROUTE_RETRY_DELAYS_MS`, jittered, as
   *   the rolling release asks (none where the client was built with `retryNoRoute: false`); with
   *   none left, the next event is `exhausted`;
   * - `end`: `#endWrite`.
   * A deadline or a stop that ends a pause or a rolling-release wait is that wait's event, read by
   * the table as every other; where the deadline ends a pause before anything of the write may
   * have landed, the write stands at its last answer, which says why nothing was stored.
   *
   * Every Dispatch that carries `b1c42d1d7d` runs the keyed layer on every POST and PATCH route, so
   * a repeat under the key is answered from the key rather than run again: a write asks again after
   * any answer that is no definitive one, a transport failure and a silence cut alike, and asks
   * nothing else first. A failing answer that names the request's key back (`keyEchoed`), one of
   * Dispatch's own 5xx, 429 or `409 KEY_IN_FLIGHT`, says the layer judged it.
   *
   * Every attempt presents the credential afresh (a resource server that keeps a replay store
   * refuses a proof it has already seen) and sends the same encoded body. Once an attempt may have
   * landed, every later one presents as the same identity, or is not sent. A write through Cipher's
   * proxy (`via`, `#throughProxy`) presents the placeholder's credential in the client's own
   * credential's place on every attempt instead, over the proxy's transport: the proxy signs each
   * request's proof itself, and that credential has no identity, so the write cannot turn into
   * another identity between attempts, and no `stale`, so no attempt tells a credential its token
   * is not live. Every write reads an answer Cipher's proxy wrote itself by its headers, and a
   * CONNECT the proxy refused as one it wrote before anything left it (`proxyEvent`:
   * `proxy-refused`, `proxy-failed`, `proxy-unsettled`), whether the write names the proxy (`via`)
   * or a CI job's `HTTPS_PROXY` does; one through `via` ends with the proxy's refusal
   * (`#proxyRefusal`). An upload (a body of `STREAMED_BODY_MIN` or more, sent as a stream) keeps
   * the schedule alone: it never enters the in-flight asking. No ask starts later than
   * `writeRetry.inFlightWithinMs` after the client was made.
   */
  async #sendWrite(
    method: string,
    url: string,
    body: EncodedBody | undefined,
    accept: string,
    signal: AbortSignal | undefined,
    write: WriteCall,
    via?: ProxyTransport
  ): Promise<WriteAnswer> {
    const limit = signal === undefined ? this.#signal : AbortSignal.any([this.#signal, signal]);
    const { key, progress } = write;
    const stores = rowStores(write.row);
    // What every attempt presents: the client's credential, or the placeholder's in its place for
    // a write through Cipher's proxy (`ProxyTransport.credential`).
    const credential = via?.credential ?? this.#credential;
    const headers: Record<string, string> = { Accept: accept, [IDEMPOTENCY_KEY_HEADER]: key };
    if (body !== undefined) headers["Content-Type"] = body.type;
    const { pausesMs, withinMs, silenceMs, inFlightPauseMs, inFlightWithinMs } = this.writeRetry;
    const pause = this.writeRetry.pause ?? pauseFor;
    const bodyBytes =
      body === undefined
        ? 0
        : typeof body.content === "string"
          ? Buffer.byteLength(body.content)
          : body.content.byteLength;
    const upload = bodyBytes >= STREAMED_BODY_MIN;
    const lastAsk = this.#madeAt + inFlightWithinMs;
    const routeWaits = this.#retryNoRoute ? [...NO_ROUTE_RETRY_DELAYS_MS] : [];
    let state: WriteState = "nothing-sent";
    let deadTokenTold = false;
    // The write's pacing round: a no-route retry starts a new one, as a new build may answer it.
    let started = this.#now();
    let paced = 1;
    // The latest failure of the credential's own, never the call's stop (`limit.reason`), since a
    // presentation last answered. Before anything may have landed, it is why nothing was sent, so
    // a deadline or a stop that ends the wait for the next presentation ends the write with it,
    // not with the wait's own error.
    let credentialFailure: unknown;
    // The identity the write's attempts went out as (`RequestCredential.identity`). Dispatch
    // keys a write per login, the person or the service and never the session (`requestKeyLogin`,
    // request_keys.go), so a repeat as another person or as the machine is a new key there and can
    // run the write a second time. The pin is stricter than that key on purpose: it holds the
    // session and the variables the broker client was made from beside the sign-in, since this
    // client cannot tell whether another session's client trades as the same person. So once an
    // attempt may have landed, a presentation as another identity (a sign-out, a sign-in, a
    // session switch) is never sent. Before that, a repeat reaches no handler, and the write goes
    // as the identity it presents now.
    let login: string | undefined;
    // One presentation and attempt, its answer's body read inside it. It changes none of the
    // loop's state: what it saw comes back in its result, which `next` reads.
    const attempt = async (): Promise<WriteAttempt> => {
      let presented: Readonly<Record<string, string>> | undefined;
      try {
        // A call already stopped or out of time sends nothing more.
        limit.throwIfAborted();
        presented = await credential.headers(method, url, undefined, this.#credentialSignal(limit));
        this.#sent.note(presented);
        // A credential that answered after the call's signal ended still sends nothing.
        limit.throwIfAborted();
      } catch (error) {
        return {
          event: stoppedEvent(limit, false, error) ?? credentialEvent(error),
          failure: error,
          ...(presented === undefined ? {} : { presented }),
          ...(error === limit.reason ? {} : { credentialFailure: error }),
        };
      }
      const identity = credential.identity?.(presented);
      if (state === "may-have-landed" && identity !== login) {
        return {
          event: "no-credential-final",
          failure: new Error(
            `${write.request} would present as another identity than the attempt before it (a sign-out, a sign-in or a session switch came between them), which Dispatch may key apart from the write, so it is not sent again`
          ),
          presented,
        };
      }
      const sent = { as: identity, with: presented };
      progress.sent += 1;
      let response: Response;
      try {
        response = await this.#attempt(
          url,
          method,
          { ...headers, ...presented },
          body,
          silenceMs,
          limit,
          via?.init
        );
      } catch (error) {
        return {
          event:
            stoppedEvent(limit, true, error) ??
            // A CONNECT the proxy refused opened no tunnel: nothing of the write left it.
            (error instanceof AirlockTunnelRefusedError
              ? proxyEvent(error.status, false)
              : error instanceof WriteAttemptSilent
                ? "silence"
                : error instanceof DispatchUnreachableError && error.reachedNothing
                  ? "unreached"
                  : "reset"),
          failure: error,
          presented,
          sent,
        };
      }
      const echoed = keyEchoed(response, key);
      const heard = { retryAfterMs: retryAfterMs(response.headers) };
      let answer: WriteAnswer;
      try {
        answer = await writeAnswer(response, echoed);
      } catch (error) {
        // The answer's headers came and its body did not, whole: the request reached whatever
        // answered, and whether the write ran was the body's to say. A deadline or a stop that
        // ended the read is that stop; any other failure, the body breaking off.
        const stop = stoppedEvent(limit, true, error);
        return {
          event: stop ?? "body-broken",
          failure:
            stop === undefined
              ? new Error(
                  `${write.request} lost the body of its ${response.status} answer (${errorClass(error)})`
                )
              : error,
          presented,
          sent,
          heard,
        };
      }
      return {
        event: answerEvent(answer, credential.stale !== undefined && !deadTokenTold),
        answer,
        presented,
        sent,
        heard,
      };
    };
    // The next attempt, and the loop's own state it moves: a presentation that answered clears
    // the credential's last failure and one that failed sets it; an attempt that went out pins the
    // identity it went as.
    const next = async (): Promise<WriteAttempt> => {
      const result = await attempt();
      if (result.presented !== undefined) credentialFailure = undefined;
      if (result.credentialFailure !== undefined) credentialFailure = result.credentialFailure;
      if (result.sent !== undefined) login = result.sent.as;
      return result;
    };
    // A pause or a rolling-release wait the deadline or the caller's stop ended: nothing was in
    // flight, and the stop is what the table reads next. Every such event ends the write.
    const waitStopped = (error: unknown): WriteAttempt => ({
      ...(state === "nothing-sent" && error instanceof Error && error.name === "TimeoutError"
        ? last
        : {}),
      event: stoppedEvent(limit, false, error) ?? "cancelled",
      failure: error,
    });
    // The last attempt (or the wait that stopped), and the event the table reads: that attempt's,
    // or `exhausted` where no ask is left after it.
    let last = await next();
    let event = last.event;
    for (;;) {
      const step = writeStep(state, event, stores);
      state = step.state;
      if ((event === "silence" || event === "in-flight") && !upload) progress.running = true;
      switch (step.action) {
        case "settle":
          // The table settles only on an answer read whole (`applied`, `judged`).
          if (last.answer === undefined) {
            throw new Error(`${write.request}: the write table settled on ${event} with no answer`);
          }
          return last.answer;
        case "end":
          throw await this.#endWrite(
            method,
            url,
            state,
            event === "exhausted" ? last.event : event,
            last.answer,
            state === "nothing-sent" &&
              credentialFailure !== undefined &&
              (event === "deadline" || event === "cancelled")
              ? credentialFailure
              : last.failure,
            write,
            event === "exhausted" && last.event === "no-route" && this.#retryNoRoute,
            via
          );
        case "retry-at-once":
          deadTokenTold = true;
          credential.stale?.(undefined, last.sent?.with);
          last = await next();
          event = last.event;
          continue;
        case "retry-route": {
          const delay = routeWaits.shift();
          if (delay === undefined) {
            event = "exhausted";
            continue;
          }
          try {
            await this.#sleep(jitteredDelay(delay), limit);
          } catch (error) {
            last = waitStopped(error);
            event = last.event;
            continue;
          }
          started = this.#now();
          paced = 1;
          last = await next();
          event = last.event;
          continue;
        }
        case "retry": {
          const scheduled = progress.running
            ? Math.min(pausesMs[paced - 1] ?? inFlightPauseMs, inFlightPauseMs)
            : pausesMs[paced - 1];
          const wait = Math.max(scheduled ?? 0, last.heard?.retryAfterMs ?? 0);
          const latest = Math.min(
            started + (progress.running ? inFlightWithinMs : withinMs),
            lastAsk
          );
          const again = scheduled !== undefined && this.#now() + wait <= latest;
          if (!again) {
            event = "exhausted";
            continue;
          }
          try {
            await pause(wait, limit);
          } catch (error) {
            last = waitStopped(error);
            event = last.event;
            continue;
          }
          if (this.#now() > latest) {
            event = "exhausted";
            continue;
          }
          paced += 1;
          last = await next();
          event = last.event;
          continue;
        }
      }
    }
  }

  /**
   * The signal a credential presents within: the call's `limit`, and the time left on the
   * client's own deadline less the request's reserve (`credentialDeadline`). For the 60 s
   * default the credential waits 59 s and the request has 1 s. A caller's own `signal` ends
   * `limit` where it ends first, and its length is the caller's, so no reserve is taken from it.
   */
  #credentialSignal(limit: AbortSignal): AbortSignal {
    return credentialDeadline(limit, Math.max(1, this.#deadlineAt - performance.now()));
  }

  /** The write clock: `writeRetry.now`, or the process's monotonic one. */
  #now(): number {
    return this.writeRetry.now?.() ?? performance.now();
  }

  /**
   * One attempt of a write, under `limit` (the call's signal and the client's) and a silence bound
   * of its own: once `silenceMs` pass before its answer begins with no read of its body in that
   * time, it is cut off and fails as `WriteAttemptSilent`. The bound ends with the answer's
   * headers; its body is read under `limit` alone. `transport` is Cipher's proxy and its
   * authority, for a write sent through it (`ProxyTransport`). A CONNECT the proxy refused reaches
   * the write as `AirlockTunnelRefusedError` (`#fetch`, `tunnelRefusal`), as it does for a write
   * whose proxy the environment names.
   */
  async #attempt(
    url: string,
    method: string,
    headers: Record<string, string>,
    body: EncodedBody | undefined,
    silenceMs: number,
    limit: AbortSignal,
    transport?: ProxyTransport["init"]
  ): Promise<Response> {
    const cut = new AbortController();
    const silent = (): void => cut.abort(new WriteAttemptSilent(silenceMs));
    let timer = setTimeout(silent, silenceMs);
    let answered = false;
    // A stream body read after the answer came (an early refusal) starts no timer of its own.
    const progress = (): void => {
      if (answered) return;
      clearTimeout(timer);
      timer = setTimeout(silent, silenceMs);
    };
    try {
      return await this.#fetch(url, {
        method,
        signal: AbortSignal.any([limit, cut.signal]),
        ...transport,
        ...(body === undefined ? { headers } : attemptBody(body, headers, progress)),
      });
    } catch (error) {
      if (cut.signal.aborted && !limit.aborted) throw cut.signal.reason;
      // The proxy checks the grant as the tunnel opens, so its usual refusal - a grant not live, a
      // store it could not read - answers the CONNECT, and Bun's fetch rejects that answer rather
      // than returning it: `#fetch` throws it as `AirlockTunnelRefusedError`, which the write reads.
      throw error;
    } finally {
      answered = true;
      clearTimeout(timer);
    }
  }

  /**
   * What a write throws when the table ends it (`end`, write-outcome.ts), in `state`, after
   * `event` (the last attempt's, where the asking was exhausted) with its `answer` or `failure`.
   * Its own reading first: a refusal as `#response` reads it (an old build's no route, with the
   * rolling-release schedule spent (`routeSpent`), as `DispatchNoRouteError`); a 2xx the table
   * would not settle on, for what it is: one that names no key back or another request's, one of
   * a route that stores nothing, or a `409 IDEMPOTENCY_KEY_COMMITTED` that names no key back, by
   * the one rule of a landing (`#missingWriteAnswer`: no answer of Dispatch's to this call), and
   * one that names the key back with a body that is empty, not JSON-typed, or not the JSON it says;
   * or the failure. An answer Cipher's proxy wrote itself (`WriteAnswer.proxy`) is the proxy's,
   * never Dispatch's or a gateway's: for a write that names no proxy (a CI job's, through the
   * airlock its `HTTPS_PROXY` names) an `AirlockRefusedError` or `AirlockOutcomeUnknownError`
   * (`#airlockAnswer`), and a CONNECT the proxy refused stays its `AirlockTunnelRefusedError`. A
   * write Dispatch made whose answer is gone (`DispatchWriteLandedError`) is definite in either
   * state. In `nothing-sent` that reading is what the write throws. In `may-have-landed` the write
   * is `DispatchWriteUnsettledError` carrying its key, with that reading as `last`, and, for a
   * write that ended with no answer of Dispatch's, what ended it. On a write sent through Cipher's
   * proxy (`via`), every text is made here, from `state` and `event`: an answer the proxy wrote
   * itself, and its refusal of the CONNECT, is `DispatchProxyRefusalError` (`#proxyRefusal`); a
   * write that got no answer names the proxy (`#proxyUnreachable`); and only a write no attempt of
   * which may have reached Dispatch is told it can be sent again.
   */
  async #endWrite(
    method: string,
    url: string,
    state: WriteState,
    event: WriteEvent,
    answer: WriteAnswer | undefined,
    failure: unknown,
    write: WriteCall,
    routeSpent: boolean,
    via: ProxyTransport | undefined
  ): Promise<unknown> {
    const { request, progress } = write;
    if (
      answer?.response.status === 409 &&
      answer.code === IDEMPOTENCY_KEY_COMMITTED &&
      !answer.echoed
    ) {
      return this.#missingWriteAnswer(method, url, answer.response, write, answer.text);
    }
    let own: unknown = failure;
    if (answer?.proxy !== undefined) {
      own =
        via === undefined
          ? this.#airlockAnswer(answer, answer.proxy)
          : this.#proxyRefusal(
              request,
              via,
              answer.response.status,
              answer.proxy.code,
              answer.text,
              state,
              event
            );
    } else if (via !== undefined && failure instanceof AirlockTunnelRefusedError) {
      // Bun keeps no body of a CONNECT's answer, so the proxy's code and status are all it said.
      own = this.#proxyRefusal(
        request,
        via,
        failure.status,
        failure.proxyCode ?? failure.code,
        "",
        state,
        event
      );
    } else if (via !== undefined && failure instanceof DispatchUnreachableError) {
      own = this.#proxyUnreachable(via, failure, state);
    } else if (answer?.response.ok === true) {
      if (!answer.echoed || !rowStores(write.row)) {
        return this.#missingWriteAnswer(method, url, answer.response, write, answer.text);
      }
      const { status } = answer.response;
      own = new Error(
        answer.notJson
          ? `${request} answered ${status} with ${bodyForText(answer.text, this.#sent.with(this.token), "JSON")}, though every Dispatch write answers JSON, so something in Dispatch's place answered`
          : `${request} answered ${status} with a body that is not the JSON its Content-Type says`
      );
    } else if (answer !== undefined) {
      own =
        event === "no-route" && routeSpent
          ? new DispatchNoRouteError()
          : this.#payloadOrError(method, url, answer);
    }
    if (own instanceof DispatchWriteLandedError || state === "nothing-sent") return own;
    const last = own instanceof Error ? own : new Error(String(own));
    let unanswered: string | undefined;
    // An answer the proxy wrote itself is no answer of Dispatch's: its text names the request too.
    if (answer === undefined || answer.proxy !== undefined) {
      switch (event) {
        case "silence":
          unanswered = `${request} ${last.message}`;
          break;
        case "body-broken":
          unanswered = last.message;
          break;
        case "no-credential":
        case "no-credential-final":
          unanswered = `${request} could not present its credential for another attempt (${last.message})`;
          break;
        case "deadline":
        case "deadline-in-flight":
          unanswered = `${request} had no definitive answer when its deadline ended it`;
          break;
        case "cancelled":
        case "cancelled-in-flight":
          unanswered = `${request} was stopped by its caller with no definitive answer`;
          break;
        default:
          unanswered = `${request} failed on its last attempt (${last.message})`;
      }
    }
    return new DispatchWriteUnsettledError(last, progress.sent, write.key, unanswered);
  }

  /**
   * A write through Cipher's proxy on `via` that got no answer, `failure`: it failed at the proxy,
   * which is the secrets broker's, or past it, so its text names the proxy by its origin, never a
   * restart of this process, which no setting of this process would mend. In `nothing-sent` no
   * attempt reached Dispatch, so the review can be sent again; once one may have, this is the
   * unsettled write's `last`, whose own advice is to read the write back first.
   */
  #proxyUnreachable(
    via: ProxyTransport,
    failure: DispatchUnreachableError,
    state: WriteState
  ): DispatchUnreachableError {
    const again =
      state === "nothing-sent" ? " The review can be sent again once the broker answers." : "";
    return new DispatchUnreachableError(
      `Dispatch at ${urlForText(this.#baseUrl, DISPATCH_URL_SETTING)}, through Cipher's proxy at ${originOf(via.init.proxy)}, is unreachable: ${failure.failure}. The proxy is the secrets broker's.${again}`,
      failure.reachedNothing,
      failure.failure
    );
  }

  /**
   * The refusal Cipher's proxy wrote itself to a write sent through it on `via`, inside the tunnel
   * or to its CONNECT: its `status` and `code` (its `Cipher-Proxy-Code`), the proxy by its origin
   * (never the placeholder in its user info), and its `text` redacted as every excerpt is. Its
   * words follow `state` and `event` (`proxyEvent`'s reading of this answer): while no attempt may
   * have reached Dispatch (`nothing-sent`), the proxy refused (`proxy-refused`) or failed
   * (`proxy-failed`) before any byte left it, so nothing reached Dispatch, with the proxy's advice
   * that a new `dispatch review` trades a new grant (once the broker answers, for a failure). Once
   * an attempt may have reached Dispatch, the text names the attempt that may have - this one
   * (`proxy-unsettled`) or an earlier one - and gives no advice: `#endWrite` ends the write
   * unsettled with this as its `last`, whose own advice is to read the write back first.
   */
  #proxyRefusal(
    request: string,
    via: ProxyTransport,
    status: number,
    code: string,
    text: string,
    state: WriteState,
    event: WriteEvent
  ): DispatchProxyRefusalError {
    const said = excerpt(text, this.#sent.with(this.token));
    const by = `${request} was answered ${status} ${code} by Cipher's proxy at ${originOf(via.init.proxy)}${said === "" ? "" : ` (${JSON.stringify(said)})`}`;
    const message =
      state !== "nothing-sent"
        ? event === "proxy-unsettled"
          ? `${by}, after this attempt may have reached Dispatch`
          : `${by} before this attempt left the proxy, after an earlier attempt of the review may have reached Dispatch`
        : event === "proxy-failed"
          ? `${by} before any of it left the proxy, so nothing reached Dispatch. The proxy is the secrets broker's, and a new \`dispatch review\` trades a new grant once the broker answers.`
          : `${by} before any of it left the proxy, so nothing reached Dispatch, and a new \`dispatch review\` trades a new grant.`;
    return new DispatchProxyRefusalError(code, status, message);
  }

  /**
   * The error of an answer Cipher's proxy wrote itself (`proxy`, `proxyOwnAnswer`) to a write that
   * names no proxy, a CI job's result through the airlock its `HTTPS_PROXY` names: before anything
   * left the proxy, `AirlockRefusedError`; after the write may have reached Dispatch,
   * `AirlockOutcomeUnknownError`. Its body is the proxy's words as every text of a proxy's own
   * answer is quoted (`#proxyRefusal` too): the client's bounded excerpt, one line of at most the
   * excerpt's length, read from the body's first `EXCERPT_SCAN_LIMIT` characters, each piece of a
   * token this client presented redacted.
   */
  #airlockAnswer(answer: WriteAnswer, proxy: ProxyOwnAnswer): Error {
    const body = excerpt(answer.text, this.#sent.with(this.token));
    const { status } = answer.response;
    return proxy.left
      ? new AirlockOutcomeUnknownError(status, proxy.code, body)
      : new AirlockRefusedError(status, proxy.code, body);
  }

  #url(path: readonly string[], query?: object): string {
    if (path.some((segment) => pathNamesNothingPattern.test(segment))) {
      throw new DispatchEmptyPathError(path);
    }
    const url = new URL(
      `${path.map((segment) => encodeURIComponent(segment)).join("/")}`,
      `${this.#baseUrl}/`
    );
    if (query) {
      for (const [name, value] of Object.entries(query)) {
        // A boolean is sent as `true` or `false`, the two words every route that reads one
        // takes (`parseQueryBool` in the server). It must be sent rather than dropped: a
        // search's path is picked from the members the request carries
        // (`everyBuildReadsSearch`), so a dropped `keywords_only: true` would ask for
        // `/api/v1/search/results` and then arrive with the flag gone, and the server would
        // answer by meaning the search asked it to skip.
        if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") {
          url.searchParams.set(name, String(value));
        } else if (Array.isArray(value)) {
          for (const item of value) url.searchParams.append(name, String(item));
        }
      }
    }
    return url.toString();
  }

  /**
   * The answer's payload, or the refusal it carries: `#payloadOf` of its body, read here. Dispatch's
   * own refusal is JSON with a string `error` (`writeError` in the server), and it renders as that
   * text with its `code`. Any other non-2xx body — a gateway's HTML page, an empty body, JSON of
   * another shape — did not come from Dispatch, so it is a `DispatchGatewayError`
   * (`#gatewayError`). So is a 2xx whose body is empty, not typed JSON or not JSON, but for a
   * bodiless 204 or 205: every answer Dispatch gives a route this client reads as JSON is JSON
   * (`WriteJSON`), so such a 2xx is something in its place, a captive portal's 200 page for one,
   * and never the route's answer (the acceptance of #4140 at be17859eca: a portal's 200 text page
   * read as `open-asks`' answer told a session that nothing waited on it). A replayed refusal is
   * the refusal Dispatch first gave, read as any other. A write Dispatch made whose answer is
   * gone — `409 IDEMPOTENCY_KEY_COMMITTED`, or a replayed 2xx reading `IDEMPOTENCY_ANSWER_NOT_KEPT`
   * (`answerNotKept`) — is a `DispatchWriteLandedError`, neither a refusal nor a payload. A call
   * that writes nothing made nothing, so `#writingNothing` reads that replayed 2xx as no landing
   * and asks again.
   */
  async #response<T>(method: string, url: string, response: Response): Promise<T> {
    const text = await response.text();
    return this.#payloadOf<T>(method, url, { response, text, json: parsedJson(response, text) });
  }

  /** `#response`'s reading of an answer whose body was read already: `text`, and `json` its parse
   *  where it is typed JSON and parses. */
  #payloadOf<T>(
    method: string,
    url: string,
    answer: { readonly response: Response; readonly text: string; readonly json: ParsedJson }
  ): T {
    const { response, text, json } = answer;
    const payload: unknown = json === undefined ? text : json.value;
    if (!response.ok) {
      const refusal = refusalIn(json);
      if (refusal !== undefined) {
        if (response.status === 409 && refusal.code === IDEMPOTENCY_KEY_COMMITTED) {
          throw new DispatchWriteLandedError(
            IDEMPOTENCY_KEY_COMMITTED,
            response.status,
            method,
            requestForText(this.#baseUrl, url, DISPATCH_URL_SETTING)
          );
        }
        throw new DispatchServiceError(
          refusal.code ?? `HTTP_${response.status}`,
          response.status,
          refusal.error,
          {
            ...refusal,
            textRefusal: response.headers.has(TEXT_REFUSAL_HEADER),
          },
          retryAfterMs(response.headers) || null
        );
      }
      throw this.#gatewayError(method, url, response, text, "Dispatch's error JSON");
    }
    if (json === undefined && !NULL_BODY_STATUSES.has(response.status)) {
      throw this.#gatewayError(method, url, response, text, "JSON");
    }
    const replayed = response.headers.get(IDEMPOTENCY_REPLAYED_HEADER)?.trim() === "?1";
    if (answerNotKept(response.status, replayed, payload)) {
      throw new DispatchWriteLandedError(
        IDEMPOTENCY_ANSWER_NOT_KEPT,
        response.status,
        method,
        requestForText(this.#baseUrl, url, DISPATCH_URL_SETTING)
      );
    }
    return payload as T;
  }

  /** What `#payloadOf` throws for `answer`, or its payload where it throws nothing. */
  #payloadOrError(method: string, url: string, answer: WriteAnswer): unknown {
    try {
      return this.#payloadOf(method, url, answer);
    } catch (error) {
      return error;
    }
  }

  /**
   * The error for an answer something in Dispatch's place wrote (`#response`, or a write's 2xx
   * that is not its answer, `#missingWriteAnswer`): the request (method, URL and query; the URL's
   * user and password left out, and the token travels in a header, never the URL), the status and
   * reason phrase, the origin a redirect pointed at, a one-line plain-text excerpt of `text`
   * (`bodyForText`: a body that is not `notWhat`, an empty body, or the body's size where none of
   * what the excerpt reads is text, with every token this client presented redacted), what else
   * the caller says of it (`also`, before the page clause), and what asking again can do:
   * `advice`, or what it can do for that method and status.
   */
  #gatewayError(
    method: string,
    url: string,
    response: Response,
    text: string,
    notWhat: string,
    caller: { readonly advice?: string; readonly also?: string } = {}
  ): DispatchGatewayError {
    const tokens = this.#sent.with(this.token);
    const body = bodyForText(text, tokens, notWhat);
    const reasonPhrase = excerpt(response.statusText, tokens);
    const reason = reasonPhrase === "" ? "" : ` ${reasonPhrase}`;
    // A redirect is never followed (`#fetch`), so its error names where it pointed: the origin
    // alone, which is what tells a Dispatch URL to correct from one that something answers for.
    const target = redirectOrigin(response, url, tokens);
    const redirected =
      target === undefined ? "" : ` (a redirect to ${target}, which this client does not follow)`;
    const request = `${method} ${requestForText(this.#baseUrl, url, DISPATCH_URL_SETTING)}`;
    return new DispatchGatewayError(
      response.status,
      `${request} answered ${response.status}${reason}${redirected} with ${body}, ${caller.also ?? ""}${GATEWAY_PAGE}`,
      caller.advice ?? gatewayAdvice(method, response.status)
    );
  }
}
