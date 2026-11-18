// packages/broker-client/src/token-client.ts
//
// This repository's TypeScript client of the secrets broker's RFC 8693 token exchange: a machine
// trades the platform identity it already holds for one of the broker's access tokens, which
// Dispatch and the Envoy listener read back at the broker's introspection route. The Go client of
// `packages/envoy/internal/brokerclient` is its other half, and the two are held to one wire by
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
// moment's proof (`invalid_dpop_proof`, `use_dpop_nonce`) or holding no registration for this
// pod's session yet (`pod_session_not_registered:`), or the source of the identity could not be
// read this once. With nothing live in hand it waits out a missing registration, trading again
// until the registration lands or its caller stops waiting. It throws once the token in hand has
// run out, or once the identity has been refused in a way no retry can cure (`isFinalRefusal`): by
// the broker, or by the issuer that no longer honours a person's sign-in. So a caller that writes
// only with what `token()` gave it never writes with no identity, nor with one that has been
// ended.

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
} from "openid-client";
import type { DpopKey } from "./dpop";
import { messageOf } from "./error-text";
import { SIGNIN_COMMAND } from "./signins";
import type { SubjectToken, TokenSource } from "./source";
import {
  ACTOR_TOKEN_TYPE_MACHINE,
  EXCHANGE_CLIENT_ID,
  FORM_LAUNCHER_CREDENTIAL,
  FORM_LAUNCHER_CREDENTIAL_DECLINED,
  FORM_PROJECTS,
  FORM_SESSION,
  GRANT_TYPE_TOKEN_EXCHANGE,
  POD_SESSION_NOT_REGISTERED,
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

/** The broker's rate limit: the first of the five refusals a later trade may be answered
 *  differently. */
const TOO_MANY_REQUESTS = 429;

/** What the broker answers a proof whose `iat` is outside its 60 s skew window. */
const INVALID_DPOP_PROOF = "invalid_dpop_proof";

/** What the broker answers a proof it wants a nonce in (RFC 9449 Section 8), which `#send`
 *  already retries once with the nonce the answer carried. */
const USE_DPOP_NONCE = "use_dpop_nonce";

/** The code of the broker's refusal of a pod session it holds no registration for yet, whose
 *  description begins {@link POD_SESSION_NOT_REGISTERED} and a colon. */
const INVALID_GRANT = "invalid_grant";

const MEMBERSHIP_HELD = "membership_held";

/** The first wait before trading again on a missing registration, doubling to
 *  {@link UNREGISTERED_MAX_WAIT_MS}: the Go client's `defaultRetryWait` and `maxRetryWait`. */
const UNREGISTERED_FIRST_WAIT_MS = 15_000;
const UNREGISTERED_MAX_WAIT_MS = 5 * 60_000;

/** How often a client says again that the broker holds no registration for its session, after
 *  saying so at the first such refusal (the Go client's `unregisteredLogEvery`). */
const UNREGISTERED_LOG_EVERY_MS = 30 * 60_000;

/** Waits `ms`, or less when `signal` aborts first: the default of
 *  {@link BrokerTokenClientOptions.sleep}. Its timer holds no process open: the wait runs behind
 *  the calls that need it, and a `dispatch` command that is done exits rather than waiting out a
 *  retry nobody awaits. */
function sleep(ms: number, signal?: AbortSignal): Promise<void> {
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
  timer.unref();
  signal?.addEventListener("abort", done, { once: true });
  return promise;
}

/**
 * A trade the broker refused: a plain error naming what was traded and what the broker said.
 * Almost every refusal is one no retry can cure — an identity no entry lists, a sign-in a person
 * revoked, a token of the wrong audience — and a renewal stops on it rather than asking again.
 * The five exceptions are the broker's 429, its two DPoP refusals, a pod session it holds no
 * registration for yet, and a person Dispatch holds (`permanent`).
 */
export class BrokerRefusedError extends Error {
  /** `refused` names the identity traded, as its source describes it: this machine's, or a
   *  person's sign-in on it. */
  constructor(
    readonly status: number,
    readonly code: string,
    readonly description: string,
    refused: string
  ) {
    super(
      description === ""
        ? `the secrets broker refused ${refused}: HTTP ${status} ${code}`
        : `the secrets broker refused ${refused}: HTTP ${status} ${code}: ${description}`
    );
    this.name = "BrokerRefusedError";
  }

  /**
   * Whether this refusal is one no retry can cure, which every refusal is but five. The Go
   * client reads it the same way, out of the same five cases (`RefusedError.Permanent`,
   * packages/envoy/internal/brokerclient/brokerclient.go), and
   * `packages/envoy/internal/brokerclient/testdata/wire.json` holds the two to that one reading.
   *
   * The broker's 429: its token endpoint spends a bucket per source address, and a bucket
   * refills. A client that dropped the token in hand on a 429 would write with no identity
   * through a burst — its own, or another process's on the same machine — while the token it
   * already held was still good.
   *
   * `invalid_dpop_proof`, which is what the broker answers a proof whose `iat` is outside its
   * 60 s skew window. A process started before NTP has corrected the host's clock would
   * otherwise drop a live token and fail every Dispatch and listener call until something
   * restarted it; the next trade after the correction succeeds.
   *
   * `use_dpop_nonce`, which `#send` already retries once with the nonce the answer carried. One
   * that reaches here is an answer that carried no nonce, or a nonce the broker then refused in
   * turn — a server state the next trade can find changed.
   *
   * A pod session the broker holds no registration for yet ({@link podSessionUnregistered}): a
   * pod's trade can reach the broker before its daemon's or its session watch's registration
   * does, and a registrar can be down for minutes, so the trade that follows the registration
   * succeeds.
   *
   * `membership_held`: Dispatch holds the person this identity names, because Google refused
   * their Dispatch sign-in, or no refresh has confirmed them for 24 hours. A hold revokes nothing,
   * and the person's next Dispatch sign-in, or the next refresh that confirms them, ends it, so
   * the identity this client trades is served again then with no new credential. A client that
   * dropped it would need a restart after every hold.
   */
  get permanent(): boolean {
    return (
      this.status !== TOO_MANY_REQUESTS &&
      this.code !== INVALID_DPOP_PROOF &&
      this.code !== USE_DPOP_NONCE &&
      this.code !== MEMBERSHIP_HELD &&
      !this.podSessionUnregistered
    );
  }

  /** Whether this is the broker's refusal of a pod whose session no registration names yet:
   *  `invalid_grant`, its description beginning {@link POD_SESSION_NOT_REGISTERED} and a colon.
   *  The same words under another code, or anywhere but at the start, are some other refusal. */
  get podSessionUnregistered(): boolean {
    return (
      this.code === INVALID_GRANT && this.description.startsWith(`${POD_SESSION_NOT_REGISTERED}:`)
    );
  }
}

/**
 * A call that stopped waiting, its signal ended by its own deadline, while the broker held no
 * registration for this pod's session yet ({@link BrokerRefusedError.podSessionUnregistered}).
 * The client goes on trading for that session in the background, so it is not final
 * ({@link isFinalRefusal}): a later call takes the token once the registration lands.
 */
export class RegistrationPendingError extends Error {
  constructor(
    readonly session: string,
    refusal: BrokerRefusedError
  ) {
    super(
      `the secrets broker has no registration for this pod's session ${session} yet, and the trade is being retried in the background until it lands (${refusal.description})`,
      { cause: refusal }
    );
    this.name = "RegistrationPendingError";
  }
}

/** One caller waiting on a client's shared trade: how to tell it the trade's answer. */
interface Waiter {
  readonly resolve: (token: BrokerToken) => void;
  readonly reject: (error: unknown) => void;
}

/**
 * A person's sign-in that the issuer no longer honours: its refresh token was answered
 * `invalid_grant`, because the person revoked the grant at Google or it expired. It mirrors the
 * Go client's `ErrSignInEnded` (`packages/envoy/internal/brokerclient/device.go`), and like a
 * broker's permanent refusal it is final ({@link isFinalRefusal}): no retry and no other caller
 * cures it, only a new {@link SIGNIN_COMMAND}.
 */
export class SignInEndedError extends Error {
  constructor(
    readonly issuer: string,
    readonly person: string,
    options?: ErrorOptions
  ) {
    super(
      `${issuer} no longer honours ${person}'s sign-in on this machine; run: ${SIGNIN_COMMAND}`,
      options
    );
    this.name = "SignInEndedError";
  }
}

/**
 * Whether `error` ends the identity a client trades for good: a broker's refusal no retry can
 * cure (`BrokerRefusedError.permanent`), or a sign-in the issuer has ended
 * ({@link SignInEndedError}). A client drops the token in hand on one rather than answer it
 * until it lapses, and a caller that remembers refusals remembers these alone: remembering the
 * broker's 429 or a clock-skewed proof would stop a client a later trade would have answered.
 */
export function isFinalRefusal(error: unknown): boolean {
  return (
    (error instanceof BrokerRefusedError && error.permanent) || error instanceof SignInEndedError
  );
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
   * `packages/envoy/internal/broker/oauth/exchange.go`): a sign-in holds its credential, and the
   * revoke of the sign-in ends every token minted under it. So each person's trade writes one
   * `launcher_credential.token_login` audit row, which the broker writes for every one of them.
   */
  readonly launcherCredential?: boolean | undefined;
  /** Overrides {@link DEFAULT_RENEW_BEFORE_MS}. Only a test shortens it. */
  readonly renewBeforeMs?: number | undefined;
  /** `Date.now` unless a test replaces it. */
  readonly now?: (() => number) | undefined;
  /** Waits `ms` before the next trade on a missing registration, returning early when `signal`
   *  aborts; a timer unless a test replaces it. */
  readonly sleep?: ((ms: number, signal?: AbortSignal) => Promise<void>) | undefined;
  /** Where the client says the broker holds no registration for its session yet;
   *  `console.warn` unless a caller replaces it. */
  readonly log?: ((line: string) => void) | undefined;
  /** The fetch every call goes through; the global one by default. The DPoP proof is signed
   *  around it, so a caller passing its own keeps whatever that one does. */
  readonly fetch?: typeof fetch | undefined;
}

/**
 * The HTTP status behind `error`, or 0 where it carries none: a status `openid-client` reports
 * as an unconforming answer is left in the response it hangs off the error's `cause` chain. The
 * person's identity source reads its issuer's errors with it too (`person-identity.ts`).
 */
export function statusOf(error: unknown): number {
  let cause: unknown = error;
  for (let depth = 0; depth < 4; depth += 1) {
    if (cause instanceof Response) return cause.status;
    if (typeof cause !== "object" || cause === null || !("cause" in cause)) return 0;
    cause = cause.cause;
  }
  return 0;
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
  readonly #sleep: (ms: number, signal?: AbortSignal) => Promise<void>;
  readonly #log: (line: string) => void;
  readonly #fetch: typeof fetch;
  /** The most recent `DPoP-Nonce` the broker sent, carried in every later proof. */
  #nonce = "";
  #held: BrokerToken | null = null;
  /** The trade under way, so two callers that need a token at once make one trade. It runs on
   *  no caller's signal: each caller stops waiting for it on its own (`#joined`). */
  #trading: Promise<BrokerToken> | null = null;
  /** The callers waiting on that trade, told its answer by its one completion subscription. A
   *  caller whose signal ends leaves this set, so the trade, which can stay pending for the
   *  session's life, holds nothing of a call that has its answer. */
  readonly #waiters = new Set<Waiter>();
  /** The missing-registration refusal the trade under way is waiting out, or null. */
  #pending: BrokerRefusedError | null = null;
  /** Cuts that wait short, so a call arriving during it trades at once; null outside it. */
  #wake: (() => void) | null = null;
  /** Ended by {@link close}, which stops a trade waiting out a missing registration. */
  readonly #closed = new AbortController();

  constructor(options: BrokerTokenClientOptions) {
    const brokerUrl = withoutTrailingSlashes(options.brokerUrl.trim());
    if (brokerUrl === "") throw new Error("broker client: no broker URL");
    this.#brokerUrl = brokerUrl;
    this.#key = options.key;
    this.#subject = options.subject;
    this.#actor = options.actor;
    this.#session = options.session ?? "";
    this.#projects = [...(options.projects ?? [])];
    this.#launcherCredential = options.launcherCredential ?? true;
    this.#renewBeforeMs = options.renewBeforeMs ?? DEFAULT_RENEW_BEFORE_MS;
    this.#now = options.now ?? Date.now;
    this.#sleep = options.sleep ?? sleep;
    this.#log = options.log ?? console.warn;
    this.#fetch = options.fetch ?? fetch;
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
   * otherwise a fresh trade. A trade that failed in a way a later trade may not repeat — the
   * broker did not answer, answered with one of its five refusals a later trade may be answered
   * differently (`BrokerRefusedError.permanent`), or the identity's source could not be read this
   * once — answers the token in hand while it is still live, since a client that already holds
   * an identity need not stop for a broker or an issuer that is merely down, busy, refusing this
   * moment's proof or waiting on a registration. With nothing live in hand, a pod session the
   * broker holds no registration for yet is waited out in the background (`#renew`), and a call
   * arriving during that wait has the client trade at once. A caller whose `signal` ends first
   * stops waiting with {@link RegistrationPendingError}, naming the session, while the trade goes
   * on. A final refusal ({@link isFinalRefusal}), and any other failure with nothing live left,
   * drop the token in hand and throw, so no caller writes with no identity or with one the broker
   * or the issuer has ended.
   *
   * A person whom Dispatch holds is restored by their next Dispatch sign-in, or the next refresh
   * that confirms them, with no new credential, so the token source is kept and traded again:
   * like a 429 or a DPoP refusal, the refusal answers the token in hand while it is live, and
   * with nothing live in hand it is thrown and the next call trades afresh.
   */
  async token(signal?: AbortSignal): Promise<BrokerToken> {
    const held = this.#held;
    if (held !== null && this.#now() < held.expiresAt.getTime() - this.#renewBeforeMs) return held;
    if (this.#trading === null) {
      const started = this.#renew();
      this.#trading = started;
      started.then(
        (token) => this.#settled(started, (waiter) => waiter.resolve(token)),
        (error: unknown) => this.#settled(started, (waiter) => waiter.reject(error))
      );
    } else {
      this.#wake?.();
    }
    return await this.#joined(signal);
  }

  /** Ends `started`, the trade that has just settled, and tells every caller waiting on it. */
  #settled(started: Promise<BrokerToken>, tell: (waiter: Waiter) => void): void {
    if (this.#trading === started) this.#trading = null;
    const waiters = [...this.#waiters];
    this.#waiters.clear();
    for (const waiter of waiters) tell(waiter);
  }

  /**
   * The answer of the trade under way, or what this caller is told when its `signal` ends first:
   * that the session's registration is still pending, where the trade is waiting one out, and
   * otherwise the signal's own reason. The trade itself goes on either way; the caller leaves
   * the waiting set, so nothing of it stays behind.
   */
  #joined(signal: AbortSignal | undefined): Promise<BrokerToken> {
    const { promise, resolve, reject } = Promise.withResolvers<BrokerToken>();
    const stop = (): void => {
      this.#waiters.delete(waiter);
      const pending = this.#pending;
      reject(
        pending === null ? signal?.reason : new RegistrationPendingError(this.#session, pending)
      );
    };
    const waiter: Waiter = {
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

  /** Stops a trade waiting out a missing registration, for a caller done with this client. */
  close(): void {
    this.#closed.abort();
  }

  /**
   * Forgets the token in hand, so the next {@link token} trades afresh: for a caller a server has
   * told that the token it presented is no longer live, such as one the broker ended at a revoke
   * before it lapsed. A trade already under way is left alone, since the token it brings back is
   * a fresh one.
   */
  drop(): void {
    this.#held = null;
  }

  /**
   * Trades until a token lands, with what the client does about each failure. A refusal of a pod
   * session the broker holds no registration for yet, with nothing live in hand, is the one it
   * trades again on: a pod's trade can reach the broker before its registration does. It waits
   * {@link UNREGISTERED_FIRST_WAIT_MS}, doubling to {@link UNREGISTERED_MAX_WAIT_MS}, until a
   * trade lands or the client is closed, and a call arriving during a wait cuts it short. It says
   * so once, naming the session, and again every {@link UNREGISTERED_LOG_EVERY_MS} while the
   * refusal lasts.
   */
  async #renew(): Promise<BrokerToken> {
    const closed = this.#closed.signal;
    let wait = UNREGISTERED_FIRST_WAIT_MS;
    // When the first refusal came, and when the client last said so; null until one has.
    let since: number | null = null;
    let logged: number | null = null;
    try {
      while (true) {
        try {
          const token = await this.#trade(closed);
          this.#held = token;
          return token;
        } catch (error) {
          const held = this.#held;
          if (!isFinalRefusal(error) && held !== null && this.#now() < held.expiresAt.getTime()) {
            return held;
          }
          this.#held = null;
          const unregistered = error instanceof BrokerRefusedError && error.podSessionUnregistered;
          if (!unregistered || closed.aborted) throw error;
          this.#pending = error;
          const now = this.#now();
          since ??= now;
          if (logged === null || now - logged >= UNREGISTERED_LOG_EVERY_MS) {
            logged = now;
            this.#log(
              `the secrets broker has no registration for this pod's session ${this.#session} yet (${Math.round((now - since) / 1000)} s so far); trading ${this.describe()} again until it lands`
            );
          }
          const woken = new AbortController();
          this.#wake = () => woken.abort();
          try {
            await this.#sleep(wait, AbortSignal.any([closed, woken.signal]));
          } finally {
            this.#wake = null;
          }
          if (closed.aborted) throw error;
          wait = Math.min(wait * 2, UNREGISTERED_MAX_WAIT_MS);
        }
      }
    } finally {
      this.#pending = null;
    }
  }

  /**
   * One RFC 8693 exchange: the subject token this moment's source answers, the actor token where a
   * person is signing in, the session name, whether this trade declines the launcher credential
   * its identity holds, and a DPoP proof of this client's key.
   *
   * `openid-client` makes the request and reads the answer — the grant type, the client id, the
   * form encoding, the endpoint and the shape of what came back — and the proof is signed by the
   * fetch it is given, so one signer serves the token endpoint and every later request this
   * process makes with the token it got.
   */
  async #trade(signal: AbortSignal | undefined): Promise<BrokerToken> {
    const subject = await this.#subject.read(signal);
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
      form["actor_token"] = actor.token;
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
      throw this.#refusal(endpoint, error);
    }
    return this.#read(endpoint, answered, raw.body);
  }

  /**
   * What a failed trade is. An OAuth error the broker answered with a 4xx is a refusal of the
   * identity, which no retry can cure; everything else — a broker that did not answer, a 5xx, an
   * answer that is not this grant's — is an error a renewal may ask about again. A status the
   * library reports without naming it is named here, since "the broker answered HTTP 503" is what
   * tells a reader to look at the broker rather than at this process.
   */
  #refusal(endpoint: string, error: unknown): Error {
    if (error instanceof ResponseBodyError && error.status >= 400 && error.status < 500) {
      return new BrokerRefusedError(
        error.status,
        error.error,
        error.error_description ?? "",
        this.#subject.describe()
      );
    }
    const status = statusOf(error);
    const message = messageOf(error);
    const said = status === 0 ? message : `HTTP ${status}: ${message}`;
    return new Error(`trade ${this.#subject.describe()} at ${endpoint}: ${said}`, {
      cause: error,
    });
  }

  /**
   * One request through the caller's fetch with a DPoP proof of this client's key, retried once
   * when the broker answers `use_dpop_nonce` with a nonce to carry (RFC 9449 Section 8). The
   * answer's own body is kept in `raw`, since `openid-client` lowercases `token_type` (RFC 6749
   * makes it case-insensitive) while the corpus names the spelling the broker wrote.
   */
  async #send(
    url: string,
    options: CustomFetchOptions,
    signal: AbortSignal | undefined,
    raw: { body: unknown }
  ): Promise<Response> {
    const send = async (): Promise<Response> => {
      const headers = new Headers(options.headers);
      headers.set(
        "DPoP",
        await this.#key.proof({ method: options.method, url, nonce: this.#nonce })
      );
      return await this.#fetch(url, requestInit(options, signal, headers));
    };
    let answer = await send();
    const given = answer.headers.get("DPoP-Nonce") ?? "";
    if (given !== "") this.#nonce = given;
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
    if (tokenType.toLowerCase() !== TOKEN_TYPE_DPOP.toLowerCase()) {
      throw new Error(
        `the broker at ${endpoint} answered token_type ${JSON.stringify(tokenType)}, and this grant mints DPoP-bound tokens alone`
      );
    }
    if (answered.refresh_token !== undefined && answered.refresh_token !== "") {
      throw new Error(
        `the broker at ${endpoint} answered a refresh token, which this grant never issues; a renewal trades a fresh identity token instead`
      );
    }
    const seconds = answered.expires_in ?? 0;
    if (!(seconds > 0)) {
      throw new Error(
        `the broker at ${endpoint} answered no usable expires_in (${JSON.stringify(answered.expires_in)})`
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
        throw new Error(
          `the broker at ${endpoint} answered a credential_expires_at that is no RFC 3339 time (${JSON.stringify(expiresAtText)})`
        );
      }
    }
    return {
      accessToken: answered.access_token,
      tokenType,
      expiresAt: new Date(this.#now() + seconds * 1000),
      credentialId: typeof answered["credential_id"] === "string" ? answered["credential_id"] : "",
      credentialExpiresAt,
    };
  }
}
