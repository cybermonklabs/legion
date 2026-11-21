// packages/cipher-client/src/token-client.ts
//
// This repository's TypeScript client of the secrets broker's RFC 8693 token exchange: a machine
// trades the platform identity it already holds for one of the broker's access tokens, which
// Dispatch and the Envoy listener read back at the broker's introspection route. The Go client of
// `packages/envoy/internal/cipherclient` is its other half, and the two are held to one wire by
// the corpus in that package's `testdata/wire.json`, which both suites replay.
//
// What the exchange is: the caller presents the subject token its platform already gives it (a
// `TokenSource`), proves a key of its own with a DPoP proof (RFC 9449), and is answered an access
// token bound to that key. The answer carries no refresh token at all — the grant issues none —
// so a renewal is a fresh subject token traded again, which is why a source is read at every
// trade rather than once.
//
// What it refuses to do: hand out a token it could not renew. `token()` answers the token in hand
// while that token is live and the broker is merely unreachable, busy (its 429), refusing this
// moment's proof (`invalid_dpop_proof`, `use_dpop_nonce`), refusing a person Dispatch holds until
// they sign in again (`membership_held`), or holding no registration for this pod's session yet
// (`pod_session_not_registered:`), or the source of the identity could not be read this once.
// With nothing live in hand, a caller that fails fast (the default) is told the
// retry loop's last failure at once, and one that waits for ready, or for a missing registration,
// waits until a token lands or its own signal ends; it is then told what the last trade came to,
// or that the trade still out has not answered (`TradeUnansweredError`). It throws once the token
// in hand has run out, once the client is closed (`TokenClientClosedError`), or once the identity
// has been refused in a way no retry can cure (`isFinalRefusal`): by the broker, or by the issuer
// that no longer honours a person's sign-in. So a caller that writes only with what `token()`
// gave it never writes with no identity, nor with one that has been ended.

import { holdSecret, holdUrlUserinfo, scrub } from "@legion/contracts/held-secrets";
import { errorClass, isHttpUrl, urlForText } from "@legion/contracts/log-text";
import { withoutTrailingSlashes } from "@legion/contracts/trailing-slash";
import {
  allowInsecureRequests,
  Configuration,
  type CustomFetchOptions,
  customFetch,
  genericGrantRequest,
  None,
  ResponseBodyError,
  type TokenEndpointResponse,
  WWWAuthenticateChallengeError,
} from "openid-client";
import type { DpopKey } from "./dpop";
import { messageOf } from "./error-text";
import type { RefusalContext } from "./refusal-text";
import type { SubjectToken, TokenSource } from "./source";
import {
  BrokerRefusedError,
  BrokerTradeFailedError,
  isFinalRefusal,
  LockHeldError,
  RegistrationPendingError,
  TokenClientClosedError,
  TradeRetryingError,
  TradeUnansweredError,
} from "./token-errors";
import {
  ACTOR_TOKEN_TYPE_MACHINE,
  EXCHANGE_CLIENT_ID,
  FORM_LAUNCHER_CREDENTIAL,
  FORM_LAUNCHER_CREDENTIAL_DECLINED,
  FORM_PROJECTS,
  FORM_SESSION,
  GRANT_TYPE_TOKEN_EXCHANGE,
  SUBJECT_TOKEN_TYPE_GOOGLE,
  TOKEN_PATH,
  TOKEN_TYPE_DPOP,
} from "./wire";

/**
 * How long before an access token's expiry the client trades again, so a renewal lands with the
 * token in hand still good and nothing of the caller's restarts. The broker mints a machine's
 * token for an hour or less.
 */
export const DEFAULT_RENEW_BEFORE_MS = 5 * 60_000;

/** One of the broker's access tokens as this client holds it. The five members here are the whole
 *  of what a client reads out of an answer, as `testdata/wire.json`'s `expect.token` names them. */
export interface BrokerToken {
  /** The token itself, presented as `Authorization: DPoP <token>` beside a proof of the key it is
   *  bound to. */
  readonly accessToken: string;
  /** The answer's `token_type`, `DPoP` for every token this grant mints. */
  readonly tokenType: string;
  /** When the token lapses, from the answer's `expires_in`. */
  readonly expiresAt: Date;
  /** The launcher credential the trade minted for this machine or sign-in, under which sessions
   *  of this machine enrol; empty where the answer carries none. */
  readonly credentialId: string;
  /** When that credential lapses, null where the answer carries none. */
  readonly credentialExpiresAt: Date | null;
}

/** What names the broker's URL in a text where it does not parse (`urlForText`); the constructor
 *  refuses such a URL, and `brokerTokenClient` names AGENT_SECRETS_URL in its refusal. A device
 *  login names it the same way (`device-login.ts`). */
export const BROKER_URL_SETTING = "the broker URL";

/** The first wait before trading again after a trade the broker did not answer, answered with a
 *  5xx, its 429 or a DPoP refusal, or that failed before it reached the broker, doubling to
 *  {@link RETRY_MAX_WAIT_MS} until a trade lands: the Go client's `defaultRetryWait` and
 *  `maxRetryWait` (packages/envoy/internal/cipherclient/brokerclient.go). */
export const RETRY_FIRST_WAIT_MS = 15_000;
export const RETRY_MAX_WAIT_MS = 5 * 60_000;

/** The first wait before trading again on a pod session the broker holds no registration for
 *  yet, doubling to {@link PENDING_MAX_WAIT_MS}. Shorter than the other schedule: a registration
 *  usually lands within seconds, and no caller cuts a wait short, so the cap is the longest wait
 *  between a pending refusal and the next trade. That trade has its own {@link TRADE_ATTEMPT_MS},
 *  and a failure of another class that comes between moves the loop to the other schedule. */
export const PENDING_FIRST_WAIT_MS = 2_000;
export const PENDING_MAX_WAIT_MS = 30_000;

/** How often the loop says again that the broker holds no registration for its session, after
 *  saying so at the first such refusal: the Go client's `unregisteredLogEvery`. */
const UNREGISTERED_LOG_EVERY_MS = 30 * 60_000;

/** The most one trade may take, from the subject token's read to the broker's answer, before it
 *  is a trade the broker did not answer: the Go client's `callTimeout`
 *  (packages/envoy/internal/cipherclient/environment.go). */
export const TRADE_ATTEMPT_MS = 30_000;

/** Waits `ms`, or less when `signal` aborts first: the default of
 *  {@link BrokerTokenClientOptions.sleep}, of the device login's poll and of its lock's wait.
 *  `holdsProcess` says whether its timer holds the process open. The retry loop's does not: the
 *  wait runs behind the calls that need it, and a `dispatch` command that is done exits rather
 *  than waiting out a retry nobody awaits. A wait its caller awaits to go on (a poll of a code, a
 *  lock another process holds) does, or the process would exit printing nothing of why. */
export function sleep(ms: number, signal?: AbortSignal, holdsProcess = false): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  if (signal?.aborted) {
    resolve();
    return promise;
  }
  const done = (): void => {
    clearTimeout(timer);
    signal?.removeEventListener("abort", done);
    resolve();
  };
  const timer = setTimeout(done, ms);
  if (!holdsProcess) timer.unref();
  signal?.addEventListener("abort", done, { once: true });
  return promise;
}

/**
 * A failed trade of the retry loop that a later trade may cure, by its class, with the text that
 * says what it was (`#failure`): `pending`, a pod session the broker holds no registration for
 * yet; `busy`, the broker's 429, a DPoP refusal or a person Dispatch holds (`membership_held`,
 * which the person's next Dispatch sign-in ends); `unanswered`, no answer within the attempt's
 * time, no connection, a 5xx, or an answer that is not this grant's; `unsent`, a trade that
 * failed before it reached the broker, its identity's source unreadable this once.
 */
type Retried =
  | { readonly kind: "pending"; readonly said: string; readonly refusal: BrokerRefusedError }
  | { readonly kind: "busy" | "unanswered" | "unsent"; readonly said: string };

/** One caller waiting on a client's retry loop: how to tell it the loop's answer, and whether it
 *  waits through the loop's retries or is told the first failure ({@link TokenCallOptions}). */
interface Waiter {
  readonly waitForReady: boolean;
  readonly resolve: (token: BrokerToken) => void;
  readonly reject: (error: unknown) => void;
}

/** How a caller of {@link BrokerTokenClient.token} waits. */
export interface TokenCallOptions {
  /**
   * Whether the caller waits through the retry loop's retries, as gRPC's wait-for-ready call
   * does, until a token lands, a refusal no trade cures ends the loop, or its signal ends. A
   * caller that does not (the default, gRPC's fail-fast call) is told at once, with
   * {@link TradeRetryingError}, when the loop's last trade failed in a way it trades again on and
   * no live token is in hand. Either kind waits within its signal for a pod session the broker
   * holds no registration for yet.
   */
  readonly waitForReady?: boolean | undefined;
}

/** One client: the broker it trades at, the identity it trades, and the key it proves. */
export interface BrokerTokenClientOptions {
  /** The broker's base URL, with or without a trailing slash. */
  readonly brokerUrl: string;
  /** The DPoP key this client proves — one this process made, or a person's box key — whose
   *  public half alone reaches the broker. */
  readonly key: DpopKey;
  /** The identity being traded. */
  readonly subject: TokenSource;
  /** The machine a person signed in on, presented as the actor token beside their own Google
   *  identity token. Absent for every trade a machine makes for itself. */
  readonly actor?: TokenSource | undefined;
  /** The session name the minted token carries, which an entry that fixes one refuses to be
   *  told and an entry that fixes none requires. */
  readonly session?: string | undefined;
  /**
   * The projects the minted token names ({@link FORM_PROJECTS}), sorted and each once, as
   * `tradeProjects` reads them from `AGENT_SECRETS_PROJECTS` (./environment.ts). Only a trade
   * that acts as its person passes them — a person's sign-in — and the broker names only projects
   * that person may see. A machine trading for itself passes none: its entry decides its project,
   * and the broker refuses one it is told. Empty or absent sends no parameter at all.
   */
  readonly projects?: readonly string[] | undefined;
  /**
   * Whether this trade wants the launcher credential its identity holds minted or moved onto
   * the key it proved ({@link FORM_LAUNCHER_CREDENTIAL}); true by default, as an absent
   * parameter is on the wire.
   *
   * A machine's own client passes `false` (`brokerTokenClient`, ./environment.ts): the
   * TypeScript clients want a Dispatch access token and nothing else, and none of them reads
   * `credentialId`. Each trade that asks writes a `launcher_credentials` row keyed by the
   * machine and the key it proved and a `launcher_credential.token_login` audit row, and this
   * client makes a key per process and per session, so asking would put a row on an operator's
   * machine-logins list for every `dispatch` call, every hook run and every session of a
   * long-lived process — rows nothing removes.
   *
   * A person's client leaves it unset, because the broker refuses a person's trade that declines
   * with `400 invalid_request` (`personDeclinesNothing`,
   * `packages/envoy/internal/cipher/oauth/exchange.go`): a sign-in holds its credential, and the
   * revoke of the sign-in ends every token minted under it. So each person's trade writes one
   * `launcher_credential.token_login` audit row, which the broker writes for every one of them.
   */
  readonly launcherCredential?: boolean | undefined;
  /** Overrides {@link DEFAULT_RENEW_BEFORE_MS}. Only a test shortens it. */
  readonly renewBeforeMs?: number | undefined;
  /** `Date.now` unless a test replaces it. */
  readonly now?: (() => number) | undefined;
  /** How long one trade may take, the subject token's read and the broker's answer together,
   *  before it is a trade the broker did not answer: {@link TRADE_ATTEMPT_MS} unless a test
   *  replaces it. */
  readonly attemptMs?: number | undefined;
  /** Waits `ms` before the retry loop's next trade, returning early when `signal` aborts (the
   *  client's {@link BrokerTokenClient.close}); a timer unless a test replaces it. */
  readonly sleep?: ((ms: number, signal?: AbortSignal) => Promise<void>) | undefined;
  /** A number in [0, 1) for each wait's jitter; `Math.random` unless a test replaces it. */
  readonly random?: (() => number) | undefined;
  /** Where the retry loop says, at each wait, what its last trade came to and when it trades
   *  again; `console.warn` of the line scrubbed of every held secret
   *  (`@legion/contracts/held-secrets`) unless a caller replaces it. */
  readonly log?: ((line: string) => void) | undefined;
  /** The fetch every call goes through; the global one by default. The DPoP proof is signed
   *  around it, so a caller passing its own keeps whatever that one does. */
  readonly fetch?: typeof fetch | undefined;
}

/**
 * The HTTP status behind `error`, or 0 where it carries none: an OAuth error answer and a
 * `WWW-Authenticate` challenge carry the status of the answer they were read from (their `cause`
 * is the body and the challenge), and a status `openid-client` reports as an unconforming answer
 * is left in the response it hangs off the error's `cause` chain. The person's identity source
 * reads its issuer's errors with it too (`person-identity.ts`).
 */
export function statusOf(error: unknown): number {
  let cause: unknown = error;
  for (let depth = 0; depth < 4; depth += 1) {
    if (cause instanceof Response) return cause.status;
    if (cause instanceof ResponseBodyError || cause instanceof WWWAuthenticateChallengeError) {
      return cause.status;
    }
    if (typeof cause !== "object" || cause === null || !("cause" in cause)) return 0;
    cause = cause.cause;
  }
  return 0;
}

/**
 * What a failed request to the broker's `endpoint` is, the one way every client of this package
 * words it: a trade, and a device login's code request, poll and refresh. An OAuth error the
 * broker answered with a 4xx is its refusal of `refused` (`BrokerRefusedError`), which shows the
 * code only where it is one the clients know and the description only where it is a known
 * refusal; anything else (no answer, a 5xx, a 4xx with no OAuth error, an answer that is not the
 * grant's) is a failure of `doing` it (`BrokerTradeFailedError`), named by the status where there
 * is one and by the error's class, never by its message, which can repeat the URL it was handed,
 * and with no `cause`: an answer the library refused hangs off its error (oauth4webapi's `cause`
 * is the body it read), and that answer can hold a minted token, which no log may carry.
 * `context` is what the request sent that a known refusal may name.
 */
export function brokerRefusal(
  doing: string,
  refused: string,
  endpoint: string,
  error: unknown,
  context: RefusalContext = {}
): BrokerRefusedError | BrokerTradeFailedError {
  if (error instanceof ResponseBodyError && error.status >= 400 && error.status < 500) {
    // The library checks only that `error` is a string; a description of any other type is a
    // peer's value the client reads as none.
    const description: unknown = error.error_description;
    return new BrokerRefusedError(
      error.status,
      error.error,
      typeof description === "string" ? description : "",
      refused,
      context
    );
  }
  const status = statusOf(error);
  const said = status === 0 ? errorClass(error) : `HTTP ${status}: ${errorClass(error)}`;
  return new BrokerTradeFailedError(
    `${doing} ${refused} at ${urlForText(endpoint, BROKER_URL_SETTING)}: ${said}`
  );
}

/**
 * The request `openid-client` asked for in `options`, sent with `headers`: its method, its body,
 * its redirect rule, and its own signal beside the caller's `signal`, either of which aborts it.
 * The broker's exchange and the person's identity source (`person-identity.ts`) both send what
 * the library asks through it.
 */
export function requestInit(
  options: CustomFetchOptions,
  signal: AbortSignal | undefined,
  headers: Headers | Record<string, string>
): RequestInit {
  const signals = [options.signal, signal].filter((one) => one !== undefined);
  return {
    method: options.method,
    headers,
    // openid-client's own body union is wider than one `fetch` typing or another admits; the
    // bytes are the form it encoded, and every runtime this package runs on sends them.
    body: options.body as RequestInit["body"],
    redirect: options.redirect,
    ...(signals.length === 0 ? {} : { signal: AbortSignal.any(signals) }),
  };
}

/** The most recent `DPoP-Nonce` a server sent one client, which every later proof to it carries
 *  (RFC 9449 Section 8). */
export interface DpopNonce {
  value: string;
}

/**
 * One request `openid-client` asked for in `options`, sent through `fetch` with a DPoP proof of
 * `key` naming the request's method and URL and the nonce `nonce` holds, and sent once more when
 * the server answers `use_dpop_nonce` with a nonce to carry. A nonce any answer sends is kept in
 * `nonce` for the next proof. The broker's exchange and a device login's poll and refresh
 * (`device-login.ts`) send through it, so every request to the token endpoint is proved alike.
 */
export async function sendWithProof(
  proved: { readonly key: DpopKey; readonly fetch: typeof fetch; readonly nonce: DpopNonce },
  url: string,
  options: CustomFetchOptions,
  signal: AbortSignal | undefined
): Promise<Response> {
  const send = async (): Promise<Response> => {
    const headers = new Headers(options.headers);
    headers.set(
      "DPoP",
      await proved.key.proof({ method: options.method, url, nonce: proved.nonce.value })
    );
    return await proved.fetch(url, requestInit(options, signal, headers));
  };
  let answer = await send();
  const given = answer.headers.get("DPoP-Nonce") ?? "";
  if (given !== "") proved.nonce.value = given;
  if (answer.status === 400 && given !== "") {
    const body: unknown = await answer
      .clone()
      .json()
      .catch(() => null);
    const asked =
      typeof body === "object" &&
      body !== null &&
      "error" in body &&
      body.error === "use_dpop_nonce";
    if (asked) answer = await send();
  }
  return answer;
}

/** Trades this client's identity for the broker's access tokens and keeps one live. */
export class BrokerTokenClient {
  readonly #brokerUrl: string;
  readonly #key: DpopKey;
  readonly #subject: TokenSource;
  readonly #actor: TokenSource | undefined;
  readonly #session: string;
  readonly #projects: readonly string[];
  readonly #launcherCredential: boolean;
  readonly #renewBeforeMs: number;
  readonly #now: () => number;
  readonly #attemptMs: number;
  readonly #sleep: (ms: number, signal?: AbortSignal) => Promise<void>;
  readonly #log: (line: string) => void;
  readonly #fetch: typeof fetch;
  readonly #random: () => number;
  /** The most recent `DPoP-Nonce` the broker sent, carried in every later proof. */
  readonly #nonce: DpopNonce = { value: "" };
  #held: BrokerToken | null = null;
  /** The retry loop under way (`#run`), so the callers that need a token make one trade per slot
   *  of its schedule however many they are. It runs on no caller's signal: each caller stops
   *  waiting for it on its own (`#joined`). */
  #loop: Promise<BrokerToken> | null = null;
  /** The callers waiting on that loop, each told its answer once. A caller whose signal ends
   *  leaves this set, so the loop, which can trade for the session's life, holds nothing of a
   *  call that has its answer. */
  readonly #waiters = new Set<Waiter>();
  /** The loop's last failed trade a later trade may cure, since a token last landed; null where
   *  none has failed. */
  #last: Retried | null = null;
  /** When the first of those failures came, and how many trades have failed since. */
  #since: number | null = null;
  #failed = 0;
  /** When the loop last said the broker holds no registration for its session, since a token
   *  last landed; null where it has not said so. */
  #pendingSaid: number | null = null;
  /** The refusal that ended the last loop, answered again with no trade until `#nextAt`. */
  #refused: unknown = null;
  /** The earliest time the loop may trade again, and the next wait of each schedule. */
  #nextAt = 0;
  #pendingWait = PENDING_FIRST_WAIT_MS;
  #retryWait = RETRY_FIRST_WAIT_MS;
  /** Ended by {@link close}, which stops the retry loop. */
  readonly #closed = new AbortController();

  constructor(options: BrokerTokenClientOptions) {
    const brokerUrl = withoutTrailingSlashes(options.brokerUrl.trim());
    if (brokerUrl === "") throw new Error("broker client: no broker URL");
    // Not quoted: a value that is no URL may be a token pasted where the URL goes.
    if (!isHttpUrl(brokerUrl))
      throw new Error("broker client: the broker URL is not an http or https URL");
    // Its user info held: Bun sends it as `Authorization: Basic` on the token request, which
    // carries no header of its own (`holdUrlUserinfo`).
    this.#brokerUrl = holdUrlUserinfo(brokerUrl);
    this.#key = options.key;
    this.#subject = options.subject;
    this.#actor = options.actor;
    this.#session = options.session ?? "";
    this.#projects = [...(options.projects ?? [])];
    this.#launcherCredential = options.launcherCredential ?? true;
    this.#renewBeforeMs = options.renewBeforeMs ?? DEFAULT_RENEW_BEFORE_MS;
    this.#now = options.now ?? Date.now;
    this.#attemptMs = options.attemptMs ?? TRADE_ATTEMPT_MS;
    this.#sleep = options.sleep ?? sleep;
    this.#log = options.log ?? ((line) => console.warn(scrub(line)));
    this.#fetch = options.fetch ?? fetch;
    this.#random = options.random ?? Math.random;
  }

  /** The key this client proves, whose public half the broker records. */
  get key(): DpopKey {
    return this.#key;
  }

  /** Names the identity traded, without naming a token. */
  describe(): string {
    return this.#subject.describe();
  }

  /**
   * A live access token: the one in hand while it is good for longer than the renewal window, and
   * otherwise the answer of this client's one retry loop (`#run`), which trades until a token
   * lands or a refusal no trade cures ends it. A caller never starts a second loop and never cuts
   * the loop's wait short, so however many callers ask — a NATS connection's every reconnect,
   * each Dispatch request — the broker sees one trade per slot of the loop's schedule.
   *
   * - Inside the renewal window, a caller waits for the loop's trade; once a trade of the loop has
   *   failed, a caller takes the token in hand at once while it is live, since a client that
   *   already holds an identity need not stop for a broker that is merely down or busy, and the
   *   loop trades again on its schedule.
   * - With no live token in hand, a fail-fast caller (the default) is told the loop's last
   *   failure at once where it was one the loop trades again on, other than a pod session not
   *   registered yet; a wait-for-ready caller waits on ({@link TokenCallOptions}).
   * - A refusal no trade cures ({@link isFinalRefusal}) ends the loop: it drops the token in hand
   *   and answers every caller with the refusal, and a caller arriving before the schedule's next
   *   slot is answered the same, with no trade.
   * - A caller whose `signal` ends first takes the token in hand where it is still live, and is
   *   otherwise told {@link RegistrationPendingError} where the last trade found the pod's session
   *   not registered yet, {@link TradeRetryingError} where it failed otherwise,
   *   {@link TradeUnansweredError} where no trade has ended yet but one is out, and the signal's
   *   own reason where no trade is out at all (`#stopped`). The loop goes on. A caller that passes
   *   no signal waits until a token lands, a refusal ends the loop or, failing fast, a trade
   *   fails.
   * - A closed client ({@link close}) answers a caller with nothing live in hand
   *   {@link TokenClientClosedError}, and trades no more.
   */
  async token(signal?: AbortSignal, options: TokenCallOptions = {}): Promise<BrokerToken> {
    const waitForReady = options.waitForReady === true;
    const now = this.#now();
    const held = this.#held;
    if (held !== null && now < held.expiresAt.getTime() - this.#renewBeforeMs) return held;
    if (held !== null && now < held.expiresAt.getTime() && this.#last !== null) return held;
    // A closed client trades no more, and says so to a caller with nothing live in hand.
    const closed = this.#closed.signal;
    if (closed.aborted) throw closed.reason;
    const last = this.#last;
    // Only while a loop is trading again: a last failure no loop is retrying is no retry under way.
    if (!waitForReady && this.#loop !== null && last !== null && last.kind !== "pending") {
      throw new TradeRetryingError(last.said);
    }
    if (this.#loop === null) {
      if (this.#refused !== null && now < this.#nextAt) throw this.#refused;
      this.#refused = null;
      this.#start();
    }
    return await this.#joined(signal, waitForReady);
  }

  #tell(answer: (waiter: Waiter) => void, which: (waiter: Waiter) => boolean = () => true): void {
    const waiters = [...this.#waiters].filter(which);
    for (const waiter of waiters) this.#waiters.delete(waiter);
    for (const waiter of waiters) answer(waiter);
  }

  /** What a caller whose wait ended before the loop's answer is told: the loop's last failure,
   *  as {@link token} says; where no trade has ended yet, that the trade still out has not been
   *  answered (`TradeUnansweredError`), the caller's deadline being no answer about the trade;
   *  or `reason` where no trade is out at all. */
  #stopped(reason: unknown): unknown {
    const last = this.#last;
    if (last === null)
      return this.#loop === null ? reason : new TradeUnansweredError(this.describe());
    return last.kind === "pending"
      ? new RegistrationPendingError(this.#session, last.refusal)
      : new TradeRetryingError(last.said);
  }

  /**
   * The loop's answer, or, when the caller's `signal` ends first, the token in hand where it is
   * still live (a caller inside the renewal window need not wait out a broker that is slow to
   * answer), else what `#stopped` says. The loop goes on either way; the caller leaves the
   * waiting set, so nothing of it stays behind.
   */
  #joined(signal: AbortSignal | undefined, waitForReady: boolean): Promise<BrokerToken> {
    const { promise, resolve, reject } = Promise.withResolvers<BrokerToken>();
    const stop = (): void => {
      this.#waiters.delete(waiter);
      const held = this.#held;
      if (held !== null && this.#now() < held.expiresAt.getTime()) resolve(held);
      else reject(this.#stopped(signal?.reason));
    };
    const waiter: Waiter = {
      waitForReady,
      resolve: (token) => {
        signal?.removeEventListener("abort", stop);
        resolve(token);
      },
      reject: (error) => {
        signal?.removeEventListener("abort", stop);
        reject(error);
      },
    };
    if (signal?.aborted) {
      stop();
      return promise;
    }
    this.#waiters.add(waiter);
    signal?.addEventListener("abort", stop, { once: true });
    return promise;
  }

  /** Stops the retry loop, for a caller done with this client: a caller still waiting on it, and
   *  any later caller with no live token in hand, is told the client is closed, and nothing is
   *  traded again. */
  close(): void {
    this.#closed.abort(new TokenClientClosedError());
  }

  /**
   * Starts the retry loop, and owns how it ends: whether `#run` lands a token, throws a refusal
   * no trade cures, or fails some other way, the loop is cleared and then every caller waiting
   * on it is told, once. A caller that joins after `#run` has ended and before this hears of it
   * is in the waiting set, so it is told too; one answered here that asks again starts the next
   * loop.
   */
  #start(): void {
    const loop = this.#run();
    this.#loop = loop;
    loop.then(
      (token) => {
        if (this.#loop === loop) this.#loop = null;
        this.#tell((waiter) => waiter.resolve(token));
      },
      (error: unknown) => {
        if (this.#loop === loop) this.#loop = null;
        this.#tell((waiter) => waiter.reject(error));
      }
    );
  }

  /**
   * Forgets the token in hand, so the next {@link token} trades afresh: for a caller a server has
   * told that the token it presented is no longer live, such as one the broker ended at a revoke
   * before it lapsed. Where the caller names that token (`accessToken`), a token in hand that is
   * another, one this client traded since, is kept: it is not the one the server refused, and
   * dropping it would cost a trade for nothing. A loop already under way is left alone, since the
   * token it brings back is a fresh one.
   */
  drop(accessToken?: string): void {
    if (accessToken !== undefined && this.#held?.accessToken !== accessToken) return;
    this.#held = null;
  }

  /** A wait of `ms` with equal jitter: between half of it and all of it, so clients that failed
   *  together do not trade again together. */
  #jittered(ms: number): number {
    return Math.round(ms / 2 + this.#random() * (ms / 2));
  }

  /**
   * The retry loop: capped exponential backoff with jitter around a per-trade timeout, the shape
   * of gRPC's connection backoff (grpc/grpc, doc/connection-backoff.md), whose callers wait for
   * it within their own deadlines (gRPC's wait-for-ready), and of "Exponential Backoff And
   * Jitter" (Marc Brooker, AWS Architecture Blog, 2015). Each trade ends at
   * {@link TRADE_ATTEMPT_MS} where the broker has not answered by then, as a trade it did not
   * answer. What a trade comes to (`#failure`):
   *
   * - a token: every caller is answered with it, and both schedules start over;
   * - a refusal no trade cures: the loop ends, answering every caller with it;
   * - a pod session the broker holds no registration for yet: traded again on its own schedule,
   *   {@link PENDING_FIRST_WAIT_MS} doubling to {@link PENDING_MAX_WAIT_MS};
   * - no answer, no connection, a 5xx, the broker's 429 or a DPoP refusal, an answer that is not
   *   this grant's, or a trade that could not be made: traded again on the Go client's schedule,
   *   {@link RETRY_FIRST_WAIT_MS} doubling to {@link RETRY_MAX_WAIT_MS}.
   *
   * It says at each wait what the trade came to, naming the session, and when it trades again.
   * Only a token landing starts a schedule over: no caller arriving, and no trade failing
   * differently. The next trade's time is kept on the client, so a loop started after a refusal
   * ended the last one trades no sooner than the schedule allows.
   */
  async #run(): Promise<BrokerToken> {
    const closed = this.#closed.signal;
    while (true) {
      const delay = this.#nextAt - this.#now();
      if (closed.aborted) throw closed.reason;
      if (delay > 0) await this.#sleep(delay, closed);
      if (closed.aborted) throw closed.reason;
      const attempt = AbortSignal.timeout(this.#attemptMs);
      let token: BrokerToken;
      try {
        token = await this.#trade(AbortSignal.any([closed, attempt]));
      } catch (error) {
        if (closed.aborted) throw closed.reason;
        if (this.#retried(error, attempt.aborted)) continue;
        throw error;
      }
      if (this.#pendingSaid !== null && this.#since !== null) {
        this.#log(
          `the secrets broker answered this pod's session ${this.#session} once it was registered, ${Math.round((this.#now() - this.#since) / 1000)} s and ${this.#failed} failed trades after the first`
        );
      }
      this.#held = token;
      this.#last = null;
      this.#since = null;
      this.#failed = 0;
      this.#pendingSaid = null;
      this.#nextAt = 0;
      this.#pendingWait = PENDING_FIRST_WAIT_MS;
      this.#retryWait = RETRY_FIRST_WAIT_MS;
      return token;
    }
  }

  /**
   * Reads a trade that failed, `timedOut` where its own time ran out, and says whether the loop
   * trades again. A refusal no trade cures does not: the token in hand is dropped, and the next
   * loop waits out the longer schedule's slot, answering a caller before it with the refusal.
   * Any other failure is traded again at the next slot of its schedule, with a line saying so: a
   * live token in hand answers every waiting caller; otherwise a failure other than a pending
   * registration answers a fail-fast caller `TradeRetryingError` at once, and a wait-for-ready
   * caller waits on. Both kinds wait out a pending registration within their own signals.
   */
  #retried(error: unknown, timedOut: boolean): boolean {
    const now = this.#now();
    const failure = this.#failure(error, timedOut);
    if (failure === null) {
      if (this.#pendingSaid !== null) {
        this.#log(
          `the wait for this pod's session ${this.#session} to be registered ended in a refusal no trade cures: ${scrub(messageOf(error))}`
        );
      }
      this.#held = null;
      this.#last = null;
      this.#since = null;
      this.#pendingSaid = null;
      this.#refused = error;
      this.#nextAt = now + this.#jittered(this.#retryWait);
      this.#retryWait = Math.min(this.#retryWait * 2, RETRY_MAX_WAIT_MS);
      return false;
    }
    let wait: number;
    if (failure.kind === "pending") {
      wait = this.#jittered(this.#pendingWait);
      this.#pendingWait = Math.min(this.#pendingWait * 2, PENDING_MAX_WAIT_MS);
    } else {
      wait = this.#jittered(this.#retryWait);
      this.#retryWait = Math.min(this.#retryWait * 2, RETRY_MAX_WAIT_MS);
    }
    this.#nextAt = now + wait;
    this.#last = failure;
    this.#since ??= now;
    this.#failed += 1;
    const held = this.#held;
    if (held !== null && now < held.expiresAt.getTime()) {
      this.#tell((waiter) => waiter.resolve(held));
    } else {
      this.#held = null;
      // A fail-fast caller is told this failure now; a wait-for-ready caller waits on. Each kind
      // waits out a pending registration within its own signal.
      if (failure.kind !== "pending") {
        const told = new TradeRetryingError(failure.said);
        this.#tell(
          (waiter) => waiter.reject(told),
          (waiter) => !waiter.waitForReady
        );
      }
    }
    // A pending registration is said once, and again every 30 minutes while it lasts, as the Go
    // client says it (`unregisteredLogEvery`); every other failure at each wait.
    if (failure.kind === "pending") {
      if (this.#pendingSaid !== null && now - this.#pendingSaid < UNREGISTERED_LOG_EVERY_MS) {
        return true;
      }
      this.#pendingSaid = now;
    }
    this.#log(
      `${failure.said}; trading again in ${Math.ceil(wait / 1000)} s (${this.#failed} failed, the first ${Math.round((now - this.#since) / 1000)} s ago)`
    );
    return true;
  }

  /**
   * What a failed trade came to, as a class the loop retries ({@link Retried}), or null for a
   * refusal no trade cures ({@link isFinalRefusal}). Each class's text names the identity traded
   * and the session and nothing the broker wrote: a refusal prints as it prints
   * (`BrokerRefusedError.said`), a trade that got no answer as broker-client words it
   * (`BrokerTradeFailedError`), a trade whose time ran out while its source waited for a lock
   * (`LockHeldError`) as a trade never sent, and a trade its identity's source could not make in
   * that source's own words, scrubbed: the words a caller of `token` was always handed.
   */
  #failure(error: unknown, timedOut: boolean): Retried | null {
    if (isFinalRefusal(error)) return null;
    if (error instanceof BrokerRefusedError) {
      if (error.podSessionUnregistered) {
        return {
          kind: "pending",
          refusal: error,
          said: `the secrets broker has no registration for this pod's session ${this.#session} yet${error.said === "" ? "" : ` (${error.said})`}`,
        };
      }
      return { kind: "busy", said: error.message };
    }
    if (error instanceof LockHeldError) {
      return {
        kind: "unsent",
        said: `the trade of ${this.describe()} could not be made: ${scrub(error.message)}`,
      };
    }
    if (timedOut) {
      return {
        kind: "unanswered",
        said: `the secrets broker did not answer the trade of ${this.describe()} within ${this.#attemptMs / 1000} s`,
      };
    }
    if (error instanceof BrokerTradeFailedError) return { kind: "unanswered", said: error.message };
    return {
      kind: "unsent",
      said: `the trade of ${this.describe()} could not be made: ${scrub(messageOf(error))}`,
    };
  }

  /**
   * One RFC 8693 exchange of this moment's subject token: through the source's own `spend`
   * where it has one, so a token another process can end stays good until the exchange settles
   * (a device login's), and otherwise of the token the source reads (`#exchange`).
   */
  async #trade(signal: AbortSignal | undefined): Promise<BrokerToken> {
    const source = this.#subject;
    if (source.spend !== undefined) {
      return await source.spend((subject) => this.#exchange(subject, signal), signal);
    }
    return await this.#exchange(await source.read(signal), signal);
  }

  /**
   * One RFC 8693 exchange: `subject`, the actor token where a person is signing in, the session
   * name, whether this trade declines the launcher credential its identity holds, and a DPoP
   * proof of this client's key.
   *
   * `openid-client` makes the request and reads the answer — the grant type, the client id, the
   * form encoding, the endpoint and the shape of what came back — and the proof is signed by the
   * fetch it is given, so one signer serves the token endpoint and every later request this
   * process makes with the token it got.
   */
  async #exchange(subject: SubjectToken, signal: AbortSignal | undefined): Promise<BrokerToken> {
    // Held, with the actor's and the minted token, so every line this process prints is scrubbed
    // of them (`@legion/contracts/held-secrets`): a broker at the wrong URL can echo them. Each is
    // held until an hour past its lapse (`heldUntil`), so a session that trades all its life holds
    // the tokens of its last hours, not every one it ever traded.
    holdSecret(subject.token, heldUntil(subject.token));
    const form: Record<string, string> = {
      subject_token: subject.token,
      subject_token_type: subject.tokenType,
    };
    if (this.#session !== "") form[FORM_SESSION] = this.#session;
    // Sent only when there is a list: a trade naming none reads exactly as one written before the
    // parameter existed.
    if (this.#projects.length > 0) form[FORM_PROJECTS] = this.#projects.join(" ");
    if (!this.#launcherCredential) {
      // Sent only to decline: an absent parameter asks, so a broker that has never heard of this
      // parameter answers a client that does ask exactly as it always has.
      form[FORM_LAUNCHER_CREDENTIAL] = FORM_LAUNCHER_CREDENTIAL_DECLINED;
    }
    if (this.#actor !== undefined) {
      let actor: SubjectToken;
      try {
        actor = await this.#actor.read(signal);
      } catch (error) {
        throw new Error(`read the machine this person signed in on: ${messageOf(error)}`, {
          cause: error,
        });
      }
      form["actor_token"] = holdSecret(actor.token, heldUntil(actor.token));
      // The same token under the type that says what it proves here: not who is asking, but which
      // box the person is asking from.
      form["actor_token_type"] =
        actor.tokenType === SUBJECT_TOKEN_TYPE_GOOGLE ? ACTOR_TOKEN_TYPE_MACHINE : actor.tokenType;
    }
    const endpoint = this.#brokerUrl + TOKEN_PATH;
    const raw: { body: unknown } = { body: undefined };
    const config = new Configuration(
      { issuer: this.#brokerUrl, token_endpoint: endpoint },
      EXCHANGE_CLIENT_ID,
      {},
      None()
    );
    // The broker is wherever the deployment put it, which in a cluster and on a dev stack is an
    // http origin; the client posts where it is told, as the Go client does.
    allowInsecureRequests(config);
    config[customFetch] = (url, options) => this.#send(url, options, signal, raw);
    let answered: TokenEndpointResponse;
    try {
      answered = await genericGrantRequest(config, GRANT_TYPE_TOKEN_EXCHANGE, form);
    } catch (error) {
      throw brokerRefusal("trade", this.#subject.describe(), endpoint, error, {
        ...(this.#subject.person === undefined ? {} : { person: this.#subject.person }),
        ...(this.#session === "" ? {} : { session: this.#session }),
      });
    }
    // Named as every text of a trade names the broker (`urlForText`).
    return this.#read(urlForText(endpoint, BROKER_URL_SETTING), answered, raw.body);
  }

  /**
   * One request with a proof of this client's key ({@link sendWithProof}). The answer's own body
   * is kept in `raw`, since `openid-client` lowercases `token_type` (RFC 6749 makes it
   * case-insensitive) while the corpus names the spelling the broker wrote.
   */
  async #send(
    url: string,
    options: CustomFetchOptions,
    signal: AbortSignal | undefined,
    raw: { body: unknown }
  ): Promise<Response> {
    const answer = await sendWithProof(
      { key: this.#key, fetch: this.#fetch, nonce: this.#nonce },
      url,
      options,
      signal
    );
    raw.body = await answer
      .clone()
      .json()
      .catch(() => undefined);
    return answer;
  }

  /**
   * The answer as a token, refusing one that is not this grant's: a token type that is not DPoP,
   * no usable expiry, or a refresh token — the exchange issues none, and a client that kept one
   * would hold a credential this contract says is never minted.
   *
   * Every member but one is read from what `openid-client` validated. `token_type` is read from
   * `raw`, the answer's own body: the library lowercases it, RFC 6749 Section 7.1 making it
   * case-insensitive, while the corpus names the spelling the broker wrote and holds both clients
   * to it.
   */
  #read(endpoint: string, answered: TokenEndpointResponse, raw: unknown): BrokerToken {
    const written =
      typeof raw === "object" && raw !== null && "token_type" in raw ? raw.token_type : undefined;
    const tokenType = typeof written === "string" ? written : "";
    // No value the broker wrote is quoted in these refusals: a peer at AGENT_SECRETS_URL can put
    // there the subject token it was sent (`KNOWN_BROKER_REFUSALS`).
    if (tokenType.toLowerCase() !== TOKEN_TYPE_DPOP.toLowerCase()) {
      throw new BrokerTradeFailedError(
        `the broker at ${endpoint} answered a token_type that is not DPoP, and this grant mints DPoP-bound tokens alone`
      );
    }
    if (answered.refresh_token !== undefined && answered.refresh_token !== "") {
      throw new BrokerTradeFailedError(
        `the broker at ${endpoint} answered a refresh token, which this grant never issues; a renewal trades a fresh identity token instead`
      );
    }
    const seconds = answered.expires_in ?? 0;
    if (!(seconds > 0)) {
      throw new BrokerTradeFailedError(
        `the broker at ${endpoint} answered no usable expires_in (a positive number of seconds)`
      );
    }
    const expiresAtText =
      typeof answered["credential_expires_at"] === "string"
        ? answered["credential_expires_at"]
        : "";
    let credentialExpiresAt: Date | null = null;
    if (expiresAtText !== "") {
      credentialExpiresAt = new Date(expiresAtText);
      if (Number.isNaN(credentialExpiresAt.getTime())) {
        throw new BrokerTradeFailedError(
          `the broker at ${endpoint} answered a credential_expires_at that is no RFC 3339 time`
        );
      }
    }
    return {
      // Lapsing on the real clock, which the held-secret registry forgets by.
      accessToken: holdSecret(answered.access_token, Date.now() + seconds * 1000),
      tokenType,
      expiresAt: new Date(this.#now() + seconds * 1000),
      credentialId: typeof answered["credential_id"] === "string" ? answered["credential_id"] : "",
      credentialExpiresAt,
    };
  }
}

/**
 * When `token` lapses, as {@link holdSecret} holds it, on the real clock the registry forgets by:
 * a JWT's `exp`, or now for a token whose lapse cannot be read, which is then held for the hour
 * past the trade that sent it, the time a broker that echoes it answers within.
 */
function heldUntil(token: string): number {
  const now = Date.now();
  const payload = token.split(".")[1];
  if (payload === undefined) return now;
  try {
    const { exp } = JSON.parse(Buffer.from(payload, "base64url").toString("utf8")) as {
      exp?: unknown;
    };
    return typeof exp === "number" && Number.isFinite(exp) ? Math.max(exp * 1000, now) : now;
  } catch {
    return now;
  }
}
