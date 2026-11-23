// packages/envoy-client/src/__tests__/dispatch-write-outcomes.test.ts
//
// Every way a Dispatch write can go, against the rule the client states: once an attempt may have
// landed, only a 2xx whose body was read whole or a final refusal settles the write; every other
// end is `DispatchWriteUnsettledError` carrying the key. Before anything may have landed, a write
// that ends is told the event's own error. A redirect is never followed, and every attempt of the
// write carries one key.
//
// The rule is written here on its own (`oracle`), from what Dispatch and the secrets broker do,
// not read from the client's table, and the client is driven through its public surface
// (`DispatchClient.comment`) with a scripted Dispatch, so the same file runs against a client that
// has no table at all. The oracle says, for each event the client met, whether it must ask again
// or stop, and how it ends where it stops; so a client that stops while it may still ask, or asks
// after an event that ends the write, fails as one that ends wrong does. Two passes:
// - every (state, event) pair: each state reached through each event that reaches it, then every
//   event kind, then an event whose ending says which state the write was in;
// - every sequence of events up to the client's limits (its attempts and its time, the
//   rolling-release schedule, one dead-token re-send), over one event of each kind of step, with
//   the concrete event of each kind rotated along the path, and every ending after each prefix.
// A run whose client asks for an attempt past its script is answered 201, and the pass goes on with
// every event in that place.

import { expect, test } from "bun:test";
import {
  BrokerRefusedError,
  RegistrationPendingError,
  SignInEndedError,
  TradeRetryingError,
  TradeUnansweredError,
} from "@legion/cipher-client/token-errors";
import {
  type CommentWriteResponse,
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_KEY_REUSED,
} from "@legion/contracts";
import type { RequestCredential } from "../bearer";
import {
  DispatchClient,
  DispatchServiceError,
  DispatchWriteLandedError,
  type DispatchWriteRetry,
  DispatchWriteUnsettledError,
} from "../dispatch-http";

const BASE = "http://dispatch.test";
/** Where every redirect points: another origin. */
const ELSEWHERE = "http://elsewhere.test/api/v1/issues/DSP-1/comments";

const actor = { kind: "session" as const, id: "session-1" };
const comment = { body: "Shipped.", actor };
const posted = {
  comment: { id: "comment-1", body: "Shipped." },
} as unknown as CommentWriteResponse;

/** The candidates of a `409 POSSIBLE_DUPLICATE`, as the dispatch command's issue create reads them. */
const duplicates = [
  {
    href: "/issues/DSP-9",
    key: "DSP-9",
    shared_terms: 1,
    snippet: "<mark>Shipped</mark>.",
    status: "todo",
    title: "Shipped.",
  },
];

/**
 * What a step of a write met, by what Dispatch and the broker say it is:
 * - `applied`: a 2xx whose body arrives whole;
 * - `refusal`: a refusal Dispatch or something in its place made, which a repeat does not change;
 * - `committed`: `409 KEY_COMMITTED`, a write Dispatch made whose answer is gone;
 * - `redirect`: a 3xx;
 * - `repeatable`: an answer or failure the keyed protocol says to ask again after under the key
 *   (a 5xx, a 408 or 429, `409 KEY_IN_FLIGHT`, a silence cut, a reset, a refused connection, a 2xx
 *   whose body broke off or is not the JSON it says, a credential whose trade goes on);
 * - `dead-token`: Dispatch's `401 BROKER_TOKEN_NOT_ACTIVE`, made before any handler ran;
 * - `no-route`: an old build's "no route for …", made before any handler ran;
 * - `credential-final`: a credential that cannot present again within the call: for good, or
 *   told only once it has waited out its share of the call's deadline;
 * - `stop-in-flight`: the deadline or the caller's stop while an attempt, its body included, is out;
 * - `stop-wait`: the deadline or the caller's stop between attempts.
 */
type Role =
  | "applied"
  | "refusal"
  | "committed"
  | "redirect"
  | "repeatable"
  | "dead-token"
  | "no-route"
  | "credential-final"
  | "stop-in-flight"
  | "stop-wait";

/** How the oracle says a write ends. */
type Ending =
  | { readonly as: "resolved" }
  | { readonly as: "refusal"; readonly status: number }
  | { readonly as: "landed" }
  | { readonly as: "unsettled" }
  | { readonly as: "credential" }
  | { readonly as: "credential-final"; readonly name: string }
  | { readonly as: "unreachable" }
  | { readonly as: "deadline" }
  | { readonly as: "cancelled" };

interface Kind {
  readonly name: string;
  readonly role: Role;
  /** Where it happens: an attempt's answer or failure, a credential's presentation, or a wait (a
   *  pause, the rolling-release wait, or the presentation that comes first). */
  readonly at: "attempt" | "credential" | "wait";
  /** How the response's `Idempotency-Key` relates to this request's key. */
  readonly keyEcho?: "exact" | "different";
  /** Whether it is a 2xx whose body is empty or not JSON-typed: no answer Dispatch writes to a
   *  keyed write, whatever the process knows of the layer. */
  readonly notJson?: boolean;
  /** Whether it can come after Dispatch committed the write: a 5xx, `KEY_IN_FLIGHT`, a silence
   *  cut, a reset after the request went out, a 2xx whose body did not arrive whole. */
  readonly followsCommit?: boolean;
  /** Whether Dispatch may still be running the write when it comes (a silence cut,
   *  `KEY_IN_FLIGHT`), which lets the client ask for longer. */
  readonly running?: boolean;
  /** The status of the answer it is. */
  readonly status?: number;
  /** Dispatch's error code, where its JSON body is one. A missing code is a page from something
   *  in Dispatch's place. */
  readonly code?: string;
  /** Which server layer wrote this answer: before key claim, a keyed-layer answer, a handler after
   *  the claim, or something other than Dispatch. */
  readonly writer?: "before-claim" | "keyed-layer" | "handler" | "other";
  /** Whether this answer is Dispatch's stored replay (`Idempotency-Replayed: ?1`). */
  readonly replayed?: boolean;
  /** How a write that ends after it with nothing landed is told: its own error. */
  readonly own?: Ending;
  /** What a credential kind's presentation throws. */
  readonly fails?: () => Error;
  readonly respond?: (key: string, init: RequestInit, run: Run) => Response | Promise<Response>;
}

/** One run of the client: its script and what the scripted Dispatch saw. */
interface Run {
  readonly script: readonly Kind[];
  cursor: number;
  readonly consumed: Kind[];
  /** Whether the client asked for an attempt past its script. */
  wantedMore: boolean;
  /** How many times a redirect was followed. */
  followed: number;
  /** The Idempotency-Key of each attempt. */
  readonly keys: (string | undefined)[];
  clock: number;
  readonly controller: AbortController;
  /** A step the script could not place: the harness's own fault, never the client's. */
  misplaced?: string;
}

function json(body: unknown, status: number, key?: string, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      "Content-Type": "application/json",
      ...(key === undefined ? {} : { "Idempotency-Key": key }),
      ...headers,
    },
  });
}

const refused = (
  status: number,
  code: string,
  error: string,
  key?: string,
  headers: Record<string, string> = {}
) => json({ code, error }, status, key, headers);

function page(status: number, statusText: string): Response {
  return new Response(`<html><body><h1>${status} ${statusText}</h1></body></html>`, {
    status,
    statusText,
    headers: { "Content-Type": "text/html" },
  });
}

/** An answer that never comes: the request fails with its signal's reason once that aborts. */
function hang(signal: AbortSignal | null | undefined): Promise<Response> {
  const { promise, reject } = Promise.withResolvers<Response>();
  if (signal?.aborted) reject(signal.reason);
  else signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
  return promise;
}

const timedOut = () => new DOMException("The operation timed out.", "TimeoutError");
const stopped = () => new DOMException("The operation was aborted.", "AbortError");

/**
 * A 201 that carries the key back and whose body starts and then, as the client reads on, meets
 * `then`: the run's deadline or its caller's stop (the body then fails with the request signal's
 * reason, as fetch fails a body its signal ends), or a broken connection.
 */
function partBody(
  key: string,
  init: RequestInit,
  run: Run,
  then: "deadline" | "stop" | "broken"
): Response {
  const whole = JSON.stringify(posted);
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      init.signal?.addEventListener("abort", () => controller.error(init.signal?.reason), {
        once: true,
      });
      controller.enqueue(new TextEncoder().encode(whole.slice(0, 11)));
    },
    pull(controller) {
      if (then === "broken") {
        controller.error(
          Object.assign(new TypeError("The socket connection was closed"), { code: "ECONNRESET" })
        );
      } else {
        run.controller.abort(then === "deadline" ? timedOut() : stopped());
      }
    },
  });
  return new Response(body, {
    status: 201,
    headers: { "Content-Type": "application/json", "Idempotency-Key": key },
  });
}

/** A 3xx to another origin. Asked not to follow (`redirect: "manual"`), fetch hands it on. Left to
 *  follow, it does what Bun does: a 301, 302 or 303 sends the request again as a GET, a 307 or 308
 *  sends the POST again, and either answers as the other origin does. */
function redirect(status: number, init: RequestInit, run: Run): Response {
  if (init.redirect === "manual") {
    return new Response("moved", { status, headers: { Location: ELSEWHERE } });
  }
  run.followed += 1;
  return json(posted, status === 307 || status === 308 ? 201 : 200);
}

const KINDS: readonly Kind[] = [
  {
    name: "201",
    role: "applied",
    at: "attempt",
    keyEcho: "exact",
    respond: (key) => json(posted, 201, key),
  },
  {
    name: "201 un-echoed",
    role: "applied",
    at: "attempt",
    respond: () => json(posted, 201),
  },
  {
    name: "200 portal page",
    role: "applied",
    at: "attempt",
    notJson: true,
    respond: () => page(200, "OK"),
  },
  {
    name: "200 empty",
    role: "applied",
    at: "attempt",
    notJson: true,
    respond: () =>
      new Response("", { status: 200, headers: { "Content-Type": "application/json" } }),
  },
  {
    name: "204",
    role: "applied",
    at: "attempt",
    keyEcho: "exact",
    respond: (key) => new Response(null, { status: 204, headers: { "Idempotency-Key": key } }),
  },
  {
    name: "205",
    role: "applied",
    at: "attempt",
    keyEcho: "exact",
    respond: (key) => new Response(null, { status: 205, headers: { "Idempotency-Key": key } }),
  },
  {
    name: "201 body outlives deadline",
    role: "stop-in-flight",
    at: "attempt",
    respond: (key, init, run) => partBody(key, init, run, "deadline"),
  },
  {
    name: "201 body stopped",
    role: "stop-in-flight",
    at: "attempt",
    respond: (key, init, run) => partBody(key, init, run, "stop"),
  },
  {
    name: "201 body broken",
    role: "repeatable",
    at: "attempt",
    followsCommit: true,
    keyEcho: "exact",
    respond: (key, init, run) => partBody(key, init, run, "broken"),
  },
  {
    name: "201 body malformed",
    role: "repeatable",
    at: "attempt",
    followsCommit: true,
    keyEcho: "exact",
    respond: (key) =>
      new Response(JSON.stringify(posted).slice(0, 11), {
        status: 201,
        headers: { "Content-Type": "application/json", "Idempotency-Key": key },
      }),
  },
  {
    // Every resend of one write carries identical bytes (writeRequestFields), so a key held
    // under another digest proves no attempt of this write claimed it.
    name: "422 KEY_REUSED exact echo",
    role: "refusal",
    at: "attempt",
    status: 422,
    code: IDEMPOTENCY_KEY_REUSED,
    writer: "keyed-layer",
    keyEcho: "exact",
    respond: (key) =>
      refused(422, IDEMPOTENCY_KEY_REUSED, "this key was used for another request", key),
  },
  {
    name: "422 KEY_REUSED with a different key echo",
    role: "refusal",
    at: "attempt",
    status: 422,
    code: IDEMPOTENCY_KEY_REUSED,
    writer: "keyed-layer",
    keyEcho: "different",
    respond: (key) =>
      refused(422, IDEMPOTENCY_KEY_REUSED, "this key was used for another request", `${key}-other`),
  },
  {
    name: "400 KEY_INVALID before claim",
    role: "refusal",
    at: "attempt",
    status: 400,
    code: "KEY_INVALID",
    writer: "before-claim",
    respond: () => refused(400, "KEY_INVALID", "the idempotency key is invalid"),
  },
  {
    name: "400 page",
    role: "refusal",
    at: "attempt",
    status: 400,
    writer: "other",
    respond: () => page(400, "Bad Request"),
  },
  {
    name: "409 KEY_COMMITTED",
    role: "committed",
    at: "attempt",
    status: 409,
    code: IDEMPOTENCY_KEY_COMMITTED,
    writer: "keyed-layer",
    keyEcho: "exact",
    respond: (key) => refused(409, IDEMPOTENCY_KEY_COMMITTED, "this key's write landed", key),
  },
  {
    name: "401 auth",
    role: "refusal",
    at: "attempt",
    status: 401,
    code: "UNAUTHORIZED",
    writer: "before-claim",
    respond: () => refused(401, "UNAUTHORIZED", "a token is required"),
  },
  {
    name: "413 REQUEST_TOO_LARGE before claim",
    role: "refusal",
    at: "attempt",
    status: 413,
    code: "REQUEST_TOO_LARGE",
    writer: "before-claim",
    respond: () => refused(413, "REQUEST_TOO_LARGE", "request body exceeds the limit"),
  },
  {
    name: "400 REQUEST_BODY_UNREADABLE before claim",
    role: "refusal",
    at: "attempt",
    status: 400,
    code: "REQUEST_BODY_UNREADABLE",
    writer: "before-claim",
    respond: () => refused(400, "REQUEST_BODY_UNREADABLE", "could not read the request body"),
  },
  ...[
    "OAUTH_SCOPE_REQUIRED",
    "PROJECT_SCOPE",
    "PROJECT_SCOPE_DEPLOYMENT",
    "READ_ONLY_TOKEN",
    "SESSION_SCOPE",
    "SIGN_IN_REQUIRED",
  ].map(
    (code): Kind => ({
      name: `403 ${code} before claim`,
      role: "refusal",
      at: "attempt",
      status: 403,
      code,
      writer: "before-claim",
      respond: () => refused(403, code, "the outer layer refused this write"),
    })
  ),
  {
    name: "403 handler OAUTH_SCOPE_REQUIRED replay",
    role: "refusal",
    at: "attempt",
    status: 403,
    code: "OAUTH_SCOPE_REQUIRED",
    writer: "handler",
    replayed: true,
    respond: () =>
      refused(403, "OAUTH_SCOPE_REQUIRED", "the handler requires the approve scope", undefined, {
        "Idempotency-Replayed": "?1",
      }),
  },
  {
    name: "403 handler PROJECT_SCOPE replay",
    role: "refusal",
    at: "attempt",
    status: 403,
    code: "PROJECT_SCOPE",
    writer: "handler",
    replayed: true,
    respond: () =>
      refused(403, "PROJECT_SCOPE", "the handler refused another project", undefined, {
        "Idempotency-Replayed": "?1",
      }),
  },
  {
    name: "403 handler PROJECT_SCOPE fresh exact echo",
    role: "refusal",
    at: "attempt",
    status: 403,
    code: "PROJECT_SCOPE",
    writer: "handler",
    keyEcho: "exact",
    respond: (key) => refused(403, "PROJECT_SCOPE", "the handler refused another project", key),
  },
  {
    name: "400 handler CAP_EXCEEDED fresh exact echo for the typed-input fallback reply",
    role: "refusal",
    at: "attempt",
    status: 400,
    code: "CAP_EXCEEDED",
    writer: "handler",
    keyEcho: "exact",
    respond: (key) => refused(400, "CAP_EXCEEDED", "a block is longer than a comment takes", key),
  },
  {
    name: "409 handler POSSIBLE_DUPLICATE fresh exact echo for the issue candidate list",
    role: "refusal",
    at: "attempt",
    status: 409,
    code: "POSSIBLE_DUPLICATE",
    writer: "handler",
    keyEcho: "exact",
    respond: (key) =>
      json(
        {
          code: "POSSIBLE_DUPLICATE",
          error: "possible duplicate of DSP-9",
          candidates: duplicates,
        },
        409,
        key
      ),
  },
  {
    name: "409 handler TARGET_AMBIGUOUS fresh exact echo for docEdit candidates",
    role: "refusal",
    at: "attempt",
    status: 409,
    code: "TARGET_AMBIGUOUS",
    writer: "handler",
    keyEcho: "exact",
    respond: (key) =>
      json(
        {
          code: "TARGET_AMBIGUOUS",
          error: "the target is ambiguous",
          candidates: [{ from: 1, to: 2, context: "same" }],
        },
        409,
        key
      ),
  },
  {
    name: "401 handler fresh exact echo",
    role: "refusal",
    at: "attempt",
    status: 401,
    code: "UNAUTHORIZED",
    writer: "handler",
    keyEcho: "exact",
    respond: (key) => refused(401, "UNAUTHORIZED", "the session lapsed mid-write", key),
  },
  {
    name: "409 handler TARGET_AMBIGUOUS with a different key echo",
    role: "refusal",
    at: "attempt",
    status: 409,
    code: "TARGET_AMBIGUOUS",
    writer: "handler",
    keyEcho: "different",
    respond: (key) =>
      json(
        {
          code: "TARGET_AMBIGUOUS",
          error: "the target is ambiguous",
          candidates: [{ from: 1, to: 2, context: "same" }],
        },
        409,
        `${key}-other`
      ),
  },
  {
    name: "403 page",
    role: "refusal",
    at: "attempt",
    status: 403,
    writer: "other",
    respond: () => page(403, "Forbidden"),
  },
  {
    name: "409 ID_AMBIGUOUS before claim",
    role: "refusal",
    at: "attempt",
    status: 409,
    code: "ID_AMBIGUOUS",
    writer: "before-claim",
    respond: () => refused(409, "ID_AMBIGUOUS", "the path prefix is ambiguous"),
  },
  {
    name: "404 page",
    role: "refusal",
    at: "attempt",
    status: 404,
    writer: "other",
    respond: () => page(404, "Not Found"),
  },
  ...[301, 302, 303, 307, 308].map(
    (status): Kind => ({
      name: `${status} redirect`,
      role: "redirect",
      at: "attempt",
      status,
      respond: (_key, init, run) => redirect(status, init, run),
    })
  ),
  {
    name: "304",
    role: "redirect",
    at: "attempt",
    status: 304,
    respond: () => new Response(null, { status: 304 }),
  },
  {
    name: "503 BROKER_UNAVAILABLE",
    role: "repeatable",
    at: "attempt",
    followsCommit: true,
    respond: () =>
      refused(
        503,
        "BROKER_UNAVAILABLE",
        "the secrets broker cannot be reached; try again in a minute"
      ),
  },
  {
    name: "503 echoed",
    role: "repeatable",
    at: "attempt",
    keyEcho: "exact",
    followsCommit: true,
    respond: (key) => refused(503, "UNAVAILABLE", "the database is restarting", key),
  },
  {
    name: "502 page",
    role: "repeatable",
    at: "attempt",
    followsCommit: true,
    respond: () => page(502, "Bad Gateway"),
  },
  {
    name: "409 KEY_IN_FLIGHT",
    role: "repeatable",
    at: "attempt",
    writer: "keyed-layer",
    keyEcho: "exact",
    followsCommit: true,
    running: true,
    respond: (key) =>
      refused(
        409,
        IDEMPOTENCY_KEY_IN_FLIGHT,
        "an earlier request with this key is still running",
        key
      ),
  },
  {
    name: "silence",
    role: "repeatable",
    at: "attempt",
    followsCommit: true,
    running: true,
    respond: (_key, init) => hang(init.signal),
  },
  {
    name: "reset",
    role: "repeatable",
    at: "attempt",
    followsCommit: true,
    respond: () => {
      throw Object.assign(new TypeError("The socket connection was closed"), {
        code: "ECONNRESET",
      });
    },
  },
  {
    name: "429 echoed",
    role: "repeatable",
    at: "attempt",
    keyEcho: "exact",
    own: { as: "refusal", status: 429 },
    respond: (key) => refused(429, "RATE_LIMITED", "too many requests", key),
  },
  {
    name: "408 page",
    role: "repeatable",
    at: "attempt",
    own: { as: "refusal", status: 408 },
    respond: () => page(408, "Request Timeout"),
  },
  {
    name: "429 un-echoed",
    role: "repeatable",
    at: "attempt",
    own: { as: "refusal", status: 429 },
    respond: () => refused(429, "RATE_LIMITED", "too many requests"),
  },
  {
    name: "refused",
    role: "repeatable",
    at: "attempt",
    own: { as: "unreachable" },
    respond: () => {
      throw Object.assign(new TypeError("Unable to connect"), { code: "ECONNREFUSED" });
    },
  },
  {
    name: "dead token",
    role: "dead-token",
    at: "attempt",
    status: 401,
    respond: () =>
      refused(
        401,
        "BROKER_TOKEN_NOT_ACTIVE",
        "the secrets broker does not hold this token as live"
      ),
  },
  {
    name: "no route",
    role: "no-route",
    at: "attempt",
    status: 404,
    respond: () => refused(404, "NOT_FOUND", "no route for POST /api/v1/issues/DSP-1/comments"),
  },
  {
    name: "deadline in flight",
    role: "stop-in-flight",
    at: "attempt",
    respond: (_key, init, run) => {
      run.controller.abort(timedOut());
      return hang(init.signal);
    },
  },
  {
    name: "stop in flight",
    role: "stop-in-flight",
    at: "attempt",
    respond: (_key, init, run) => {
      run.controller.abort(stopped());
      return hang(init.signal);
    },
  },
  {
    name: "no credential",
    role: "repeatable",
    at: "credential",
    own: { as: "credential" },
    fails: () => new TradeRetryingError("the secrets broker answered 503 temporarily_unavailable"),
  },
  {
    name: "no credential final",
    role: "credential-final",
    at: "credential",
    fails: () => new SignInEndedError("https://issuer.test", "person@example.test"),
  },
  {
    name: "registration pending",
    role: "credential-final",
    at: "credential",
    fails: () =>
      new RegistrationPendingError(
        "ses_pod",
        new BrokerRefusedError(
          400,
          "pod_session_not_registered",
          "pod_session_not_registered: No registration says this pod may write as ses_pod",
          "this pod"
        )
      ),
  },
  {
    name: "trade unanswered",
    role: "credential-final",
    at: "credential",
    fails: () => new TradeUnansweredError("this machine's pool identity"),
  },
  { name: "deadline", role: "stop-wait", at: "wait", own: { as: "deadline" } },
  { name: "stop", role: "stop-wait", at: "wait", own: { as: "cancelled" } },
];

const kind = (name: string): Kind => {
  const found = KINDS.find((one) => one.name === name);
  if (found === undefined) throw new Error(`no kind ${name}`);
  return found;
};

/** Takes the script's next step where it is a wait, ending the run's signal as it says. */
function waitStep(run: Run): unknown {
  const next = run.script[run.cursor];
  if (next?.at !== "wait") return undefined;
  run.cursor += 1;
  run.consumed.push(next);
  const reason = next.name === "deadline" ? timedOut() : stopped();
  run.controller.abort(reason);
  return reason;
}

/**
 * The client's limits as this harness sets them (`drive`'s `writeRetry`): the clock moves only by
 * its pauses, each of 1; no ask starts past 2 on it (`inFlightWithinMs`), so two pauses in all;
 * on the plain pacing, one pause a round (`pausesMs: [1]`, `withinMs: 1`); while Dispatch may be
 * running the write, a pause as long as the clock allows (`inFlightPauseMs`). A round starts with
 * the write and again after each rolling-release wait, which the client makes three times
 * (`NO_ROUTE_RETRY_DELAYS_MS`). A dead token is presented afresh once a call.
 */
const LIMITS = { pauses: 2, routeWaits: 3, deadTokens: 1 } as const;

/** What the oracle says follows one event: the client asks again, or it stops and ends so. */
type Decision = { readonly asks: true } | { readonly asks: false; readonly ending: Ending };

const UNSETTLED: Ending = { as: "unsettled" };

/** The fresh keyed-layer codes that do not prove a handler's answer was stored. */
const KEYED_LAYER_OWN_CODES = new Set([
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_REUSED,
]);

/** Whether `storedStatus` keeps a fresh 4xx handler answer under its request key. */
function storedFourXX(status: number | undefined): boolean {
  return (
    status !== undefined &&
    status >= 400 &&
    status < 500 &&
    status !== 401 &&
    status !== 408 &&
    status !== 429
  );
}

/**
 * Ground truth from Dispatch, rather than the client: route layers outside `keyedWrites` answer
 * before its claim with no key echo (server.go:840-916, request_keys.go:247-312), and
 * `Idempotency-Replayed: ?1` marks a stored replay and a recovery replay alike. Whether a refusal
 * after a possible landing is final follows from which layer wrote it, which each row names, so
 * the oracle never reads the client's classifier:
 * - the keyed layer: `422 KEY_REUSED` under this request's exact key. Every resend of one call
 *   carries identical bytes, so a key held under another digest proves no attempt of this write
 *   claimed it. (`409 KEY_COMMITTED` is landed and `409 KEY_IN_FLIGHT` a retry, by their roles.)
 * - a handler after the claim: its replay, or a fresh answer under this request's exact key whose
 *   4xx `storedStatus` keeps and whose code is none of the keyed layer's own.
 * - before the claim, or something other than Dispatch: never.
 */
function finalAfterLanding(event: Kind): boolean {
  if (event.replayed === true) return true;
  switch (event.writer) {
    case "keyed-layer":
      return event.keyEcho === "exact" && event.code === IDEMPOTENCY_KEY_REUSED;
    case "handler":
      return (
        event.keyEcho === "exact" &&
        storedFourXX(event.status) &&
        !KEYED_LAYER_OWN_CODES.has(event.code ?? "")
      );
    default:
      return false;
  }
}

/**
 * The rule, on its own: what follows each of `events`, read from what Dispatch and the secrets
 * broker say each is (`Kind`), the write's state, the key echoes it has seen and the limits it has
 * spent.
 */
function oracle(events: readonly Kind[]): Decision[] {
  let landed = false;
  // Whether an answer of this call carried back exactly its key: Dispatch's keyed layer answered
  // the call, and it answers every keyed write with the key (`keyedWrites`, request_keys.go).
  let echoed = false;
  let running = false;
  let pauses = 0;
  let roundPauses = 0;
  let routeWaits = 0;
  let deadTokens = 0;
  // Whether the last presentation could not present while its trade goes on, and no attempt went
  // out after it: a wait's stop before a landing then ends with the credential's failure.
  let credentialFailed = false;
  const end = (ending: Ending): Decision => ({ asks: false, ending });
  const endAfter = (own: Ending): Decision => end(landed ? UNSETTLED : own);
  return events.map((event): Decision => {
    const refusal: Ending = { as: "refusal", status: event.status ?? 0 };
    if (event.at === "attempt") credentialFailed = false;
    if (event.at === "credential") credentialFailed = event.role === "repeatable";
    if (event.keyEcho === "exact") echoed = true;
    switch (event.role) {
      case "applied":
        // A 2xx whose body is empty or not JSON is no answer of Dispatch's; nor is one without
        // exactly the key once an answer of the call carried it.
        if (event.notJson || (echoed && event.keyEcho !== "exact")) return end(UNSETTLED);
        return end({ as: "resolved" });
      case "refusal":
        return finalAfterLanding(event) ? end(refusal) : endAfter(refusal);
      case "committed":
        return end({ as: "landed" });
      case "redirect":
        return endAfter(refusal);
      case "credential-final":
        return endAfter({
          as: "credential-final",
          name: (event.fails as () => Error)().constructor.name,
        });
      case "stop-in-flight":
        return end(UNSETTLED);
      case "stop-wait":
        return endAfter(credentialFailed ? { as: "credential" } : (event.own as Ending));
      case "dead-token":
        if (deadTokens < LIMITS.deadTokens) {
          deadTokens += 1;
          return { asks: true };
        }
        return endAfter(refusal);
      case "no-route":
        if (routeWaits < LIMITS.routeWaits) {
          routeWaits += 1;
          roundPauses = 0;
          return { asks: true };
        }
        return endAfter(refusal);
      case "repeatable": {
        if (event.followsCommit) landed = true;
        if (event.running) running = true;
        const allowed = pauses < LIMITS.pauses && (running || roundPauses === 0);
        if (!allowed) return endAfter(event.own ?? UNSETTLED);
        pauses += 1;
        roundPauses += 1;
        return { asks: true };
      }
      default:
        throw new Error(`the oracle has no rule for ${String(event.role satisfies never)}`);
    }
  });
}

/** Drives one write through `script`. */
async function drive(script: readonly Kind[]): Promise<{ run: Run; error?: unknown }> {
  const run: Run = {
    script,
    cursor: 0,
    consumed: [],
    wantedMore: false,
    followed: 0,
    keys: [],
    clock: 0,
    controller: new AbortController(),
  };
  const credential: RequestCredential = {
    scheme: "DPoP",
    headers: async () => {
      const waited = waitStep(run);
      if (waited !== undefined) throw waited;
      const next = run.script[run.cursor];
      if (next?.at === "credential") {
        run.cursor += 1;
        run.consumed.push(next);
        throw (next.fails as () => Error)();
      }
      return { Authorization: "DPoP live" };
    },
    stale: () => {},
    secret: () => "live",
  };
  const fetchImpl = async (_input: RequestInfo | URL, init: RequestInit = {}) => {
    const headers = (init.headers ?? {}) as Record<string, string>;
    const key = headers["Idempotency-Key"];
    run.keys.push(key);
    const next = run.script[run.cursor];
    if (next === undefined) {
      run.wantedMore = true;
      return json(posted, 201, key);
    }
    if (next.at !== "attempt") {
      run.misplaced = `${next.name} reached an attempt`;
      throw new Error(run.misplaced);
    }
    run.cursor += 1;
    run.consumed.push(next);
    return await (next.respond as NonNullable<Kind["respond"]>)(key ?? "", init, run);
  };
  const writeRetry: DispatchWriteRetry = {
    // `LIMITS` reads these: one pause a round on the plain pacing, two in all, the clock moving only
    // by the pauses.
    pausesMs: [1],
    withinMs: 1,
    silenceMs: 1,
    inFlightPauseMs: 1,
    inFlightWithinMs: 2,
    pause: async (ms) => {
      const waited = waitStep(run);
      if (waited !== undefined) throw waited;
      run.clock += ms;
    },
    now: () => run.clock,
  };
  const client = new DispatchClient(BASE, credential, fetchImpl as typeof fetch, {
    writeRetry,
    signal: run.controller.signal,
    sleep: async () => {
      const waited = waitStep(run);
      if (waited !== undefined) throw waited;
    },
  });
  try {
    await client.comment("DSP-1", comment);
    return { run };
  } catch (error) {
    return { run, error };
  }
}

/** Why `error` (or a resolved write, where it is undefined) is not `ending`; undefined where it is. */
function mismatch(
  ending: Ending,
  error: unknown,
  keys: readonly (string | undefined)[]
): string | undefined {
  const got =
    error === undefined
      ? "resolved"
      : `${(error as Error).name ?? typeof error}: ${String((error as Error).message ?? error).slice(0, 140)}`;
  const unsettled = error instanceof DispatchWriteUnsettledError;
  switch (ending.as) {
    case "resolved":
      return error === undefined ? undefined : `want resolved, got ${got}`;
    case "unsettled":
      if (!unsettled) return `want DispatchWriteUnsettledError, got ${got}`;
      return error.key === keys[0]
        ? undefined
        : `the unsettled error carries key ${error.key}, not ${keys[0]}`;
    case "refusal":
      return !unsettled && error instanceof DispatchServiceError && error.status === ending.status
        ? undefined
        : `want a ${ending.status} refusal, got ${got}`;
    case "landed":
      return error instanceof DispatchWriteLandedError
        ? undefined
        : `want DispatchWriteLandedError, got ${got}`;
    case "credential":
      return error instanceof TradeRetryingError
        ? undefined
        : `want the credential's error, got ${got}`;
    case "credential-final":
      return (error as Error | undefined)?.constructor.name === ending.name
        ? undefined
        : `want the credential's ${ending.name}, got ${got}`;
    case "unreachable":
      return !unsettled && String((error as Error)?.message).includes("is unreachable")
        ? undefined
        : `want "unreachable", got ${got}`;
    case "deadline":
      return (error as Error)?.name === "TimeoutError"
        ? undefined
        : `want the deadline's TimeoutError, got ${got}`;
    case "cancelled":
      return (error as Error)?.name === "AbortError"
        ? undefined
        : `want the stop's AbortError, got ${got}`;
  }
}

/**
 * What one run broke of the rule, if anything: a redirect followed; an attempt under another key;
 * an event after one the oracle says ends the write (the client asked again); the client stopping
 * after an event the oracle says it asks again after, while the script had nothing more to give
 * it (it gave up with its limits unspent); or an ending that is not the oracle's.
 */
async function judge(script: readonly Kind[]): Promise<{ run: Run; findings: string[] }> {
  const { run, error } = await drive(script);
  const path = run.consumed.map((one) => one.name).join(" > ");
  const findings: string[] = [];
  if (run.misplaced !== undefined) findings.push(`[${path}] harness: ${run.misplaced}`);
  if (run.followed > 0) findings.push(`[${path}] followed a redirect`);
  if (new Set(run.keys).size > 1) {
    findings.push(`[${path}] the attempts carried ${new Set(run.keys).size} keys`);
  }
  const decisions = oracle(run.consumed);
  const last = decisions.length - 1;
  for (const [index, decision] of decisions.entries()) {
    if (index < last && !decision.asks) {
      findings.push(`[${path}] asked again after ${run.consumed[index]?.name}`);
      return { run, findings };
    }
  }
  const final = decisions[last];
  if (final === undefined) {
    if (!run.wantedMore) findings.push(`[${path}] sent nothing and stopped`);
  } else if (final.asks) {
    if (!run.wantedMore) {
      findings.push(
        `[${path}] stopped after ${run.consumed[last]?.name} with its limits unspent: ${mismatch(UNSETTLED, error, run.keys) ?? "unsettled"}`
      );
    }
  } else if (run.wantedMore) {
    findings.push(`[${path}] asked again after ${run.consumed[last]?.name}`);
  } else {
    const wrong = mismatch(final.ending, error, run.keys);
    if (wrong !== undefined) findings.push(`[${path}] ${wrong}`);
  }
  return { run, findings };
}

/** The events that reach each state: none, and each that keeps nothing landed, for
 *  `nothing-sent`; each that may land, for `may-have-landed`. */
const STATE_PREFIXES: readonly (readonly Kind[])[] = [
  [],
  ...KINDS.filter(
    (one) =>
      (one.role === "repeatable" && one.followsCommit !== true) ||
      one.role === "dead-token" ||
      one.role === "no-route"
  ).map((one) => [one]),
  ...KINDS.filter((one) => one.role === "repeatable" && one.followsCommit === true).map((one) => [
    one,
  ]),
];

/** Endings that say which state the write was in when they came. */
const STATE_PROBES: readonly Kind[] = [kind("401 auth"), kind("302 redirect"), kind("deadline")];

test("every (state, event) pair goes as the rule says", async () => {
  const findings = new Set<string>();
  let runs = 0;
  for (const prefix of STATE_PREFIXES) {
    for (const event of KINDS) {
      for (const probe of [undefined, ...STATE_PROBES]) {
        const script = probe === undefined ? [...prefix, event] : [...prefix, event, probe];
        const { findings: found } = await judge(script);
        runs += 1;
        for (const one of found) findings.add(one);
      }
    }
  }
  expect(runs).toBeGreaterThan(1_500);
  expect([...findings].slice(0, 40)).toEqual([]);
}, 120_000);

test("a replayed handler 403 after a possibly landed answer settles as its refusal", async () => {
  const { run, error } = await drive([
    kind("201 body broken"),
    kind("403 handler OAUTH_SCOPE_REQUIRED replay"),
  ]);

  expect(run.keys).toHaveLength(2);
  expect(error).toEqual(expect.objectContaining({ name: "DispatchServiceError", status: 403 }));
});

test.each([
  kind("403 handler PROJECT_SCOPE fresh exact echo"),
  kind("400 handler CAP_EXCEEDED fresh exact echo for the typed-input fallback reply"),
  kind("409 handler POSSIBLE_DUPLICATE fresh exact echo for the issue candidate list"),
  kind("409 handler TARGET_AMBIGUOUS fresh exact echo for docEdit candidates"),
  kind("422 KEY_REUSED exact echo"),
])("after a gateway page, $name settles as Dispatch's own refusal", async (answer) => {
  const { run, error } = await drive([kind("502 page"), answer]);

  expect(run.keys).toHaveLength(2);
  expect(new Set(run.keys).size).toBe(1);
  expect(error).toBeInstanceOf(DispatchServiceError);
  expect(error).not.toBeInstanceOf(DispatchWriteUnsettledError);
  expect(error).toEqual(
    expect.objectContaining({ code: answer.code, status: answer.status, fromDispatch: true })
  );
});

// The named callers read these fields: the dispatch command's duplicate list and docEdit's
// callers read `candidates`, pi-envoy's typed-input reply falls back on a `fromDispatch` refusal,
// and AskGate's decision record reads `422 KEY_REUSED` from Dispatch as "another writer made it".
test("after a gateway page, the callers' stored refusals keep what their callers read", async () => {
  const duplicate = await drive([
    kind("502 page"),
    kind("409 handler POSSIBLE_DUPLICATE fresh exact echo for the issue candidate list"),
  ]);
  expect((duplicate.error as DispatchServiceError).candidates).toEqual(duplicates);
  const ambiguous = await drive([
    kind("502 page"),
    kind("409 handler TARGET_AMBIGUOUS fresh exact echo for docEdit candidates"),
  ]);
  expect((ambiguous.error as DispatchServiceError).candidates).toEqual([
    { from: 1, to: 2, context: "same" },
  ]);
  const reply = await drive([
    kind("502 page"),
    kind("400 handler CAP_EXCEEDED fresh exact echo for the typed-input fallback reply"),
  ]);
  expect(reply.error).toEqual(
    expect.objectContaining({
      fromDispatch: true,
      message: "a block is longer than a comment takes",
    })
  );
  const reused = await drive([kind("502 page"), kind("422 KEY_REUSED exact echo")]);
  expect(reused.error).toEqual(
    expect.objectContaining({ code: IDEMPOTENCY_KEY_REUSED, status: 422, fromDispatch: true })
  );
});

test.each([
  kind("409 handler TARGET_AMBIGUOUS with a different key echo"),
  kind("422 KEY_REUSED with a different key echo"),
  kind("401 handler fresh exact echo"),
  kind("403 PROJECT_SCOPE before claim"),
  kind("413 REQUEST_TOO_LARGE before claim"),
  kind("400 REQUEST_BODY_UNREADABLE before claim"),
])("after a gateway page, $name leaves the write unsettled", async (answer) => {
  const { run, error } = await drive([kind("502 page"), answer]);

  expect(run.keys).toHaveLength(2);
  expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
  expect((error as DispatchWriteUnsettledError).key).toBe(run.keys[0] as string);
});

/** The events of each kind of step that asks again, rotated along a path so each appears. */
const ASKING_AGAIN: readonly (readonly Kind[])[] = [
  KINDS.filter((one) => one.role === "repeatable" && one.followsCommit === true),
  KINDS.filter(
    (one) => one.role === "repeatable" && one.followsCommit !== true && one.at === "attempt"
  ),
  [kind("no credential")],
  [kind("dead token")],
  [kind("no route")],
];

/** One ending of each role, and each 2xx the keyed layer decides. */
const ENDINGS: readonly Kind[] = [
  kind("201"),
  kind("201 un-echoed"),
  kind("200 portal page"),
  kind("200 empty"),
  kind("204"),
  kind("205"),
  kind("201 body outlives deadline"),
  kind("201 body stopped"),
  kind("422 KEY_REUSED exact echo"),
  kind("409 handler POSSIBLE_DUPLICATE fresh exact echo for the issue candidate list"),
  kind("409 KEY_COMMITTED"),
  kind("401 auth"),
  kind("403 page"),
  kind("308 redirect"),
  kind("304"),
  kind("no credential final"),
  kind("registration pending"),
  kind("trade unanswered"),
  kind("deadline in flight"),
  kind("stop in flight"),
  kind("deadline"),
  kind("stop"),
];

test("every sequence of events up to the client's limits goes as the rule says", async () => {
  const findings = new Set<string>();
  let runs = 0;
  let longest = 0;
  const explore = async (prefix: readonly Kind[]): Promise<void> => {
    const { run, findings: found } = await judge(prefix);
    runs += 1;
    longest = Math.max(longest, run.consumed.length);
    for (const one of found) findings.add(one);
    if (!run.wantedMore || found.length > 0) return;
    for (const [index, choices] of ASKING_AGAIN.entries()) {
      const pick = choices[(prefix.length + index) % choices.length] as Kind;
      await explore([...prefix, pick]);
    }
    for (const ending of ENDINGS) {
      const { findings: endFound } = await judge([...prefix, ending]);
      runs += 1;
      for (const one of endFound) findings.add(one);
    }
  };
  await explore([]);
  expect(runs).toBeGreaterThan(1_000);
  expect(longest).toBeGreaterThan(5);
  expect([...findings].slice(0, 40)).toEqual([]);
}, 600_000);
