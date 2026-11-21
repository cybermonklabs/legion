// packages/envoy-client/src/nats-credential.ts
//
// The credential a session's NATS connection presents. It is one of three: the nkey user a seed
// names, as every session presented before the hub ran an auth service; the session's broker
// token, which the auth service of a hub that runs one admits as that session alone
// (`packages/envoy/internal/cipher/natscallout`); or none. Which one is the deployment's to say
// (`envoySettings`): a machine that can trade for a broker token says nothing about the hub, so
// the broker token is presented only where `NATS_AUTH` or `envoy.natsAuth` says `broker`.
//
// A broker token goes on the CONNECT as its token, with a proof of the key it is bound to over the
// nonce the server sent that one connection (`DpopKey.connectionProof`) as its signature, which
// is the claim set the Go client signs (`packages/envoy/natscred`, `Session`). The auth service
// answers with a NATS user that expires with the token, so the connection moves to the next token
// before then (`keepNatsCredential`).

import type { BrokerEnvironment } from "@legion/cipher-client/environment";
import { SIGNIN_COMMAND } from "@legion/cipher-client/signins";
import { DEFAULT_RENEW_BEFORE_MS } from "@legion/cipher-client/token-client";
import {
  BrokerRefusedError,
  BrokerTradeFailedError,
  RegistrationPendingError,
  SignInEndedError,
  TokenClientClosedError,
  TradeRetryingError,
  TradeUnansweredError,
} from "@legion/cipher-client/token-errors";
import { scrub } from "@legion/contracts/held-secrets";
// Types alone: `envoySettings` imports this module, and every bundle of it — the `dispatch`
// command's, each hook's — would otherwise carry nats.js whole.
import type { Auth, ErrorCode, Events, NatsConnection, Status } from "nats";
import {
  type BrokerSessionName,
  brokerTokenFor,
  dropTradedToken,
  sessionName,
  type TradedToken,
} from "./broker-credential";
import type { NatsSeed } from "./envoy-settings";
import { LoggableError, natsFailureReason } from "./errors";

/** The variable that names which credential a session's NATS connection presents: `broker` or
 *  `none`. `envoy.natsAuth` names it in the file, and the variable overrides that field. */
export const NATS_AUTH_VAR = "NATS_AUTH";

/** The credential a session's NATS connection presents. */
export type NatsCredential =
  | { readonly kind: "seed"; readonly seed: NatsSeed }
  | { readonly kind: "broker"; readonly broker: BrokerNatsCredential }
  | { readonly kind: "none" };

/**
 * A session's broker token as its NATS connection presents it. One per process and session name:
 * where the session's listener and Dispatch calls present a broker token too, it is this one
 * (`brokerTokenFor`, `brokerCredential`), so a session holds one token, not two; a bearer those
 * calls are given instead (`ENVOY_TOKEN`, a file's `token`) is theirs alone.
 */
export interface BrokerNatsCredential {
  /**
   * Takes the token to present, trading for one where the token in hand is not live: a connection
   * is dialled only once this has answered, since the CONNECT is signed at once. It is one wait on
   * the token client's retry loop (`BrokerTokenClient.token`), in one of three modes
   * ({@link ReadyOptions}): for a caller's `life`, waiting for ready until a token lands, a refusal
   * no trade cures ends the loop, or `life` ends; for a startup's bound (`waitForReady`), waiting
   * for ready for `tradeDeadlineMs`, through every failure the loop trades again on, and then
   * failing, naming the last one; and failing fast, the default: a failure the loop trades again
   * on is told at once, naming it, and a pod session not registered yet is waited out for
   * `tradeDeadlineMs`. Either way the loop goes on, and the token client logs as its schedule
   * says.
   */
  ready(options?: ReadyOptions): Promise<void>;
  /**
   * The CONNECT's credential for the server's `nonce`: the token in hand as `auth_token`, and as
   * `sig` the proof of its key over that nonce, base64url-encoded, as the auth service reads a
   * signature. One snapshot of the token per CONNECT, so the proof always names the token sent
   * beside it. It presents nothing — and the server refuses the connection — where it holds no
   * live token, and then starts a trade, so the next reconnect presents a live token where the
   * broker answers one; that case never throws, since nats.js closes a connection for good when
   * its authenticator throws. A server that sent no nonce is the one case that does throw
   * ({@link noAuthService}): nats-server sends one to every client where an auth service runs,
   * so such a hub has nothing that checks a broker token, and a CONNECT with no credential would
   * be admitted there while the session believed it presented its own.
   */
  authenticator(nonce?: string): Auth;
  /** When the token in hand lapses; the epoch before any is taken. */
  expiresAt(): Date;
  /** Takes the session's token again: a renewed one inside the renewal window, and the token of
   *  the session the name now reads, after a switch, even one made while a renewal was in flight.
   *  It fails fast (`TokenCallOptions`) unless `waitForReady`: where the token client's last trade
   *  failed in a way its retry loop trades again on and nothing live is in hand, it is told so at
   *  once. It waits no longer than `signal`, and a switch to another session ends the waits for
   *  the one before. */
  renew(signal?: AbortSignal, waitForReady?: boolean): Promise<void>;
  /** Whether the last CONNECT presented a token other than the one in hand, so the connection
   *  should reconnect to present it. */
  needsReconnect(): boolean;
  /** A number that moves each time a token traded is not the one held: a renewal's, a session
   *  switch's, or the one after a drop (`refused`). It names no token. */
  tokenGeneration(): number;
  /** The session the token in hand was traded for; undefined where none is held. */
  tokenSession(): string | undefined;
  /** Reads a NATS server's refusal by nats.js's code, on the credential's one count of refusals of
   *  the live token in hand, which its keeper and every dial that opens its connection share: what
   *  it did, or undefined where it counts for nothing. A token's second refusal drops it; the
   *  caller has another traded. */
  refused(code: string): BrokerTokenRefusal | undefined;
  /** A connection presenting the token was admitted: by the keeper at each reconnect, and by
   *  `connectNats` at a dial that began at `dialedAt` (`Date.now()`). The refusal count starts
   *  over, and the redial handler reads it (`admissions`). */
  admitted(dialedAt?: number): void;
  /** The connections admitted so far (`admitted`), and when the dial that made the last one began,
   *  where its maker knew. */
  admissions(): NatsAdmissions;
}

/** A broker credential's connections' admissions (`BrokerNatsCredential.admissions`). */
export interface NatsAdmissions {
  readonly count: number;
  readonly dialedAt: number | undefined;
}

/** How {@link BrokerNatsCredential.ready} waits. */
export interface ReadyOptions {
  /** Waits for ready for as long as this signal lives, and rethrows its reason when it ends: a
   *  caller whose whole life is the wait, such as Claude Code's channel server, which Claude Code
   *  does not restart. */
  readonly life?: AbortSignal | undefined;
  /** Waits for ready for `tradeDeadlineMs`, a startup's bound inside its pass, and then fails
   *  naming the trade's last failure. */
  readonly waitForReady?: boolean | undefined;
  /** The most the wait may take, where that is less than `tradeDeadlineMs`: a startup whose pass
   *  deadline is shorter than the default leaves its pass the time the rest of it needs. */
  readonly boundMs?: number | undefined;
}

/** Why a CONNECT presenting the broker token fails at a hub that sent no nonce: it runs no auth
 *  service (a hub that does sends every client one), and the remedy, which undoes the setting
 *  `source` names: `NATS_AUTH`, or a file's `envoy.natsAuth in <file>`. */
export function noAuthService(source: string): string {
  const remedy =
    source === NATS_AUTH_VAR ? `unset ${NATS_AUTH_VAR}` : "take natsAuth out of that file";
  return `${source} is broker, but the NATS server sent no nonce to sign: it runs no auth service, so ${remedy} for this hub`;
}

/**
 * A broker token for a NATS connection whose process names no session: nothing is traded, since
 * a trade under no session is answered the machine's token, under `AGENT_SECRETS_SESSION`, or the
 * signed-in person's, under their sign-in's own name, neither of which is the session the
 * connection delivers to. `envoySettings` turns inbound off for it (`EnvoyInbound.noSessionNamed`),
 * and a wait for the token rejects with it.
 */
export class NoSessionNamedError extends LoggableError {
  constructor(source: string) {
    super(`${source} is broker, but this process names no session for its NATS connection's token`);
    this.name = "NoSessionNamedError";
  }
}

/**
 * How long {@link BrokerNatsCredential.ready} waits for a trade: nats.js's own dial timeout, so a
 * secrets broker that never answers fails a start as a hub that never answers does, named,
 * rather than holding it in silence. A startup that waits for ready (`ReadyOptions.waitForReady`)
 * waits this long at most: in a pass of 30 s, the floor of the pass's deadline (`ENVOY_PASS_DEADLINE_MS`;
 * by default it is the heartbeat, 90 s), that leaves 10 s for the rest of the pass (the dial,
 * its subscribes and the registration), so the start fails naming the trade's cause before the
 * pass is abandoned, and a start whose pass deadline is shorter bounds the wait by it
 * (`ReadyOptions.boundMs`). It is longer than the retry loop's longest first wait (15 s,
 * `RETRY_FIRST_WAIT_MS`, with its jitter at its longest), so a first trade that fails at once is
 * traded again inside it; whether that trade answers in time is the broker's.
 */
export const TRADE_DEADLINE_MS = 20_000;

/** Why `ready` failed: the broker has not answered the trade the setting `source` chose. */
function tradeUnanswered(source: string, deadlineMs: number): string {
  return `${source} is broker, but the secrets broker did not answer the trade for the session's token within ${deadlineMs / 1000} s`;
}

/**
 * Why a trade for the session's token failed, as a log may print it. A refusal is the broker's
 * HTTP status, OAuth code and what it said where that is one of its known refusals, else "(its
 * text is withheld)" (`BrokerRefusedError.said`: a peer at AGENT_SECRETS_URL can put the subject
 * token it was sent in its text); a trade the token client's retry loop is still trading again is
 * broker-client's own line for its last failure, naming the session where its registration is
 * pending (`RegistrationPendingError`, `TradeRetryingError`); a trade that got no answer, or an
 * answer that is not the grant's, is broker-client's own text, which names the broker as
 * `urlForText` does and the failure by its class (`BrokerTradeFailedError`); a closed client is
 * its own words (`TokenClientClosedError`); an ended sign-in is its remedy; and any other failure
 * goes by `natsFailureReason`'s allowlist, which prints this package's own reasons
 * (`LoggableError`) whole, since any other text is the runtime's or the transport's, which quotes
 * what it was handed.
 */
export function tradeFailureReason(error: unknown): string {
  if (error instanceof BrokerRefusedError) {
    const code = error.shownCode === "" ? "" : ` ${error.shownCode}`;
    const said = error.said === "" ? "" : `: ${scrub(error.said)}`;
    return `the secrets broker refused the identity traded: HTTP ${error.status}${code}${said}`;
  }
  if (error instanceof RegistrationPendingError) return scrub(error.message);
  if (error instanceof TradeRetryingError) return scrub(error.message);
  if (error instanceof TradeUnansweredError) return scrub(error.message);
  if (error instanceof SessionMovedError) return error.message;
  if (error instanceof BrokerTradeFailedError) return scrub(error.message);
  if (error instanceof TokenClientClosedError) return error.message;
  if (error instanceof SignInEndedError)
    return `the issuer no longer honours the sign-in on this machine; run: ${SIGNIN_COMMAND}`;
  return natsFailureReason(error);
}

/** Whether `error` says the caller's deadline ended with no trade answered: an
 *  `AbortSignal.timeout`'s reason, or the token client's own words for it
 *  (`TradeUnansweredError`). */
function isDeadline(error: unknown): boolean {
  return (
    (error instanceof DOMException && error.name === "TimeoutError") ||
    error instanceof TradeUnansweredError
  );
}

/**
 * A wait for one session's token that a switch to another session ended: the host has moved on,
 * and the renewal for the session it is on now is the one that counts. The trade itself goes on
 * in the token client, for that session's next caller.
 */
export class SessionMovedError extends LoggableError {
  constructor(from: string, to: string) {
    super(`the host moved from the session ${from} to ${to} while ${from}'s token was traded`);
    this.name = "SessionMovedError";
  }
}

/**
 * The broker token of `env`'s machine, or of a person signed in on it, for the session `session`
 * names, as a NATS connection presents it, chosen by the setting `source` names (`NATS_AUTH` by
 * default, or a file's `envoy.natsAuth in <file>`), which a refusal at a hub with no auth service
 * names. Nothing is traded until {@link BrokerNatsCredential.ready} or
 * {@link BrokerNatsCredential.renew}.
 *
 * Every wait for a trade ends at its caller's deadline. The token client's one retry loop per
 * session trades until a token lands or a refusal no trade cures ends it
 * (`BrokerTokenClient.token`), and each caller here waits on that loop within its own signal, so
 * no caller adds a trade, and none waits behind a trade for another session: a renewal for a
 * session other than the last renewal's is a switch, and it ends every wait for the session
 * before ({@link SessionMovedError}). A token is held only where it was traded for the session
 * the name reads when it lands; a trade that lands for the session before a switch ends its
 * renewal with the same error.
 */
export function brokerNatsCredential(
  env: BrokerEnvironment,
  session: BrokerSessionName | undefined,
  source: string = NATS_AUTH_VAR,
  tradeDeadlineMs: number = TRADE_DEADLINE_MS
): BrokerNatsCredential {
  const traded = brokerTokenFor(env, session);
  let held: TradedToken | undefined;
  let generation = 0;
  let presented: string | undefined;
  // The session the last renewal was for, and what ends every wait for it at a switch.
  let waitingFor: string | undefined;
  let switched = new AbortController();
  // The authenticator's own renewal while it runs: one at a time, so the CONNECTs nats.js makes
  // for a refused connection (`brokerRedialDelays`) trade no more often than one renewal's deadline
  // allows, and leave the token client's back-off on a missing registration as it is.
  let connecting: Promise<void> | undefined;
  const live = (): boolean => held !== undefined && held.token.expiresAt.getTime() > Date.now();
  /** One wait on the token client's retry loop for the session the name reads now, failing fast
   *  unless `waitForReady` (`TokenCallOptions`). */
  const take = (signal: AbortSignal | undefined, waitForReady: boolean): Promise<void> => {
    const name = sessionName(session);
    if (name === "") return Promise.reject(new NoSessionNamedError(source));
    if (waitingFor !== undefined && name !== waitingFor) {
      switched.abort(new SessionMovedError(waitingFor, name));
      switched = new AbortController();
    }
    waitingFor = name;
    const moved = switched.signal;
    const limit = signal === undefined ? moved : AbortSignal.any([signal, moved]);
    return traded(limit, waitForReady).then(
      (taken) => {
        const now = sessionName(session);
        if (taken.session !== now) throw new SessionMovedError(taken.session, now);
        if (held?.token.accessToken !== taken.token.accessToken) generation += 1;
        held = taken;
      },
      (error: unknown) => {
        // A switch ended this wait: the token client's words for it name the session before,
        // which is not what the caller needs to know.
        if (moved.aborted && signal?.aborted !== true) throw moved.reason;
        throw error;
      }
    );
  };
  const renew = (signal?: AbortSignal, waitForReady = false): Promise<void> =>
    take(signal, waitForReady);
  /** Why a wait in `ready` ended, as this package's own line naming the setting `source`. */
  const notReady = (error: unknown, deadlineMs: number): unknown => {
    if (error instanceof NoSessionNamedError) return error;
    if (isDeadline(error)) return new LoggableError(tradeUnanswered(source, deadlineMs));
    if (error instanceof RegistrationPendingError) {
      return new LoggableError(
        `${source} is broker, and the session's token waits on its registration: ${tradeFailureReason(error)}`
      );
    }
    if (error instanceof TradeRetryingError) {
      return new LoggableError(
        `${source} is broker, and the trade for the session's token is still being retried: ${tradeFailureReason(error)}`
      );
    }
    return new LoggableError(
      `${source} is broker, but the trade for the session's token failed: ${tradeFailureReason(error)}`
    );
  };
  const expiresAt = (): Date => held?.token.expiresAt ?? new Date(0);
  const needsReconnect = (): boolean =>
    presented !== undefined && held !== undefined && presented !== held.token.accessToken;
  const drop = (): void => {
    const dropped = held;
    held = undefined;
    if (dropped !== undefined) dropTradedToken(env, dropped);
  };
  const tokenGeneration = (): number => generation;
  const tokenSession = (): string | undefined => held?.session;
  const refusals = brokerTokenRefusals({
    expiresAt,
    needsReconnect,
    drop,
    tokenGeneration,
    tokenSession,
  });
  let admissions: NatsAdmissions = { count: 0, dialedAt: undefined };
  return {
    // One wait on the token client's retry loop, which trades on past it for the keeper and the
    // next `ready`, in the mode the caller asks for (`ReadyOptions`).
    ready: async (options: ReadyOptions = {}) => {
      if (live()) return;
      const { life } = options;
      const boundMs = Math.max(0, Math.min(tradeDeadlineMs, options.boundMs ?? tradeDeadlineMs));
      try {
        await (life === undefined
          ? take(AbortSignal.timeout(boundMs), options.waitForReady === true)
          : take(life, true));
      } catch (error) {
        if (life?.aborted === true) throw life.reason;
        throw notReady(error, boundMs);
      }
    },
    renew,
    authenticator(nonce?: string): Auth {
      if (nonce === undefined || nonce === "") throw new LoggableError(noAuthService(source));
      const snapshot = held;
      if (snapshot === undefined || !live()) {
        // Nothing live to present: the server refuses this CONNECT, and the trade started here
        // lands a token for the next one, within `ready`'s deadline. Its failure is the keeper's
        // to report.
        if (connecting === undefined) {
          connecting = renew(AbortSignal.timeout(tradeDeadlineMs))
            .catch(() => {})
            .finally(() => {
              connecting = undefined;
            });
        }
        return undefined;
      }
      const accessToken = snapshot.token.accessToken;
      let sig: string;
      try {
        // base64url, as nats.go sends a signature and the auth service reads one.
        const proof = snapshot.key.connectionProof({ accessToken, nonce });
        sig = Buffer.from(proof, "utf8").toString("base64url");
      } catch {
        return undefined;
      }
      presented = accessToken;
      return { auth_token: accessToken, sig };
    },
    expiresAt,
    needsReconnect,
    tokenGeneration,
    tokenSession,
    refused: refusals.refused,
    admitted: (dialedAt?: number) => {
      admissions = { count: admissions.count + 1, dialedAt };
      refusals.reset();
    },
    admissions: () => admissions,
  };
}

/** The least the keeper waits between two renewals, where the token in hand lives long enough for
 *  it: the wait is never longer than half the token's remaining life. */
const RENEW_FLOOR_MS = 1_000;
/** The keeper's retry after a renewal that landed no new token: doubling from the first to the
 *  last. */
const RETRY_FIRST_MS = 2_000;
const RETRY_LAST_MS = 60_000;
/** How often the keeper repeats a warning while its cause lasts. */
const WARN_AGAIN_MS = 30 * 60_000;
/** How many refusals of the live token in hand the credential's count waits for before it drops
 *  that token and has another traded: one refusal can be the auth service's own outage, which a
 *  new token does not cure, and nats.js redials within 20 s (`brokerRedialDelays`). */
const REFUSALS_BEFORE_DROP = 2;
/** The codes nats.js gives a status, or a dial's error, when the server refused the credential a
 *  CONNECT presented (`ErrorCode.AuthorizationViolation`, `ErrorCode.AuthenticationExpired`),
 *  written out as `RECONNECTED` is. */
const REFUSED_CODES: ReadonlySet<string> = new Set<`${ErrorCode}`>([
  "AUTHORIZATION_VIOLATION",
  "AUTHENTICATION_EXPIRED",
]);
/** nats.js's status of an error the server sent (`Events.Error`), written out as `RECONNECTED`
 *  is. */
const ERRORED: `${Events.Error}` = "error";
/** nats.js's status of a connection that is back (`Events.Reconnect`), written out so this module
 *  imports nats.js's types alone; the type holds it to the enum's own value. */
const RECONNECTED: `${Events.Reconnect}` = "reconnect";

/** What one refusal of the session's broker token did (`BrokerNatsCredential.refused`). */
export interface BrokerTokenRefusal {
  /** This session's token was dropped and another traded once already since a connection was
   *  last admitted, so this refusal drops none. */
  readonly droppedBefore: boolean;
  /** This refusal was the token's second, and dropped it: another is to be traded. */
  readonly dropped: boolean;
}

/** What a refusal count (`brokerTokenRefusals`) reads of the token in hand, and how it drops it:
 *  `brokerNatsCredential` hands it its own closure's functions, and a test's stand-in its own. */
export interface RefusalCountedToken {
  /** When the token in hand lapses; the epoch before any is taken. */
  expiresAt(): Date;
  /** Whether the connection has yet to present the token in hand. */
  needsReconnect(): boolean;
  /** Drops the token in hand, which the NATS server refused though it is live (a revoke), here
   *  and in the token client that traded it, so the next renewal, or the next CONNECT's own
   *  trade, takes another rather than be answered that one again. */
  drop(): void;
  /** The token in hand's generation (`BrokerNatsCredential.tokenGeneration`). */
  tokenGeneration(): number;
  /** The session the token in hand was traded for; undefined where none is held. */
  tokenSession(): string | undefined;
}

/**
 * A new count of a NATS server's refusals of a broker credential's live token in hand. A
 * credential builds its own once and owns it (`BrokerNatsCredential.refused`), so its keeper
 * (`keepNatsCredential`) and every dial that opens its connection (`connectNats`, pass after pass)
 * share it; a stand-in credential in a test builds its own the same way. `refused(code)` reads
 * one refusal's nats.js code, and answers undefined where it is no refusal of the credential
 * (`AUTHORIZATION_VIOLATION`, `AUTHENTICATION_EXPIRED`) or none of the live token in hand: a
 * refusal of a token that lapsed, or of one the connection is about to move off, is the renewal's
 * to cure. Refusals are counted per token (`tokenGeneration`), so a refusal of one token and a
 * refusal of the next are no two refusals of either. At a token's second refusal it drops that
 * token (`token.drop`: here and in the token client that traded it), so a revoked token is not
 * presented again until it lapses; the caller has another traded. It drops one token per session
 * until `reset`, which the credential calls when a connection is admitted
 * (`BrokerNatsCredential.admitted`): an auth service that is away refuses every token whatever it
 * is, and costs each session one trade for the outage, however many dials and passes it refuses,
 * never one every second refusal.
 */
export function brokerTokenRefusals(token: RefusalCountedToken): {
  readonly refused: (code: string) => BrokerTokenRefusal | undefined;
  readonly reset: () => void;
} {
  let refusals = 0;
  let counting = token.tokenGeneration();
  const droppedFor = new Set<string>();
  return {
    refused: (code) => {
      const live = token.expiresAt().getTime() > Date.now();
      if (!REFUSED_CODES.has(code) || !live || token.needsReconnect()) return undefined;
      const generation = token.tokenGeneration();
      if (generation !== counting) {
        counting = generation;
        refusals = 0;
      }
      refusals += 1;
      const session = token.tokenSession();
      const droppedBefore = session !== undefined && droppedFor.has(session);
      if (refusals !== REFUSALS_BEFORE_DROP || session === undefined || droppedBefore) {
        return { droppedBefore, dropped: false };
      }
      droppedFor.add(session);
      token.drop();
      return { droppedBefore, dropped: true };
    },
    reset: () => {
      refusals = 0;
      droppedFor.clear();
    },
  };
}

function warnedEvery(warn: (text: string) => void): {
  readonly warn: (text: string) => void;
  readonly reset: () => void;
} {
  let warnedAt: number | undefined;
  return {
    warn: (text) => {
      const now = Date.now();
      if (warnedAt !== undefined && now - warnedAt < WARN_AGAIN_MS) return;
      warnedAt = now;
      warn(text);
    },
    reset: () => {
      warnedAt = undefined;
    },
  };
}

/** How long after `now` the keeper renews a token that lapses at `expiry`: the token client's
 *  window ahead of it (`DEFAULT_RENEW_BEFORE_MS`, inside which it trades), or halfway there for
 *  a token that lives less than twice that window; at least `RENEW_FLOOR_MS`, unless that is
 *  past half the token's remaining life (the broker allows one-second tokens), and at once for a
 *  token already lapsed. */
function renewDelay(expiry: number, now: number): number {
  const remaining = expiry - now;
  if (remaining <= 0) return 0;
  const ahead = remaining - Math.min(DEFAULT_RENEW_BEFORE_MS, remaining / 2);
  return Math.max(Math.min(RENEW_FLOOR_MS, remaining / 2), ahead);
}

/**
 * Holds `connection` to a token the auth service still admits: ahead of each token's expiry it
 * renews `credential` and, when that lands a new token, reconnects, so the CONNECT presents the
 * new token before the server drops the user the old one was admitted as. A renewal that fails, or
 * lands no new token (the token client keeps the token in hand through a transient exchange
 * failure), is retried at 2 s doubling to a minute, and `warn` is told once, naming why, and again
 * every 30 minutes while it lasts; the token is never named. The connection keeps reconnecting on
 * its own meanwhile (`ignoreAuthErrorAbort`), and the authenticator starts a trade of its own at
 * each CONNECT it cannot sign, so the first live token admits it again.
 *
 * It reads the connection's status too. A server that refuses the live token in hand
 * (`AUTHORIZATION_VIOLATION`, `AUTHENTICATION_EXPIRED`) is told to `warn` at once, naming the
 * code, and again every 30 minutes until the connection is back: the auth service is away, or the
 * token was revoked. At a token's second refusal the keeper drops that token, here and in the
 * token client, and trades for another at once, so a revoked token is not presented again until
 * it lapses. Refusals are counted per token (`tokenGeneration`), and a drop is made once per
 * session until the connection is back (`tokenSession`): after a switch, the new session's
 * revoked token is dropped in its turn, and an auth service that is away, which refuses every
 * token whatever it is while nats.js redials (`brokerRedialDelays`), costs each session one trade
 * for the outage, never one every second refusal. A refusal of a token that lapsed, or of one the
 * connection is about to move off, is the renewal's to cure.
 *
 * Returns the function that stops it, its status reader included; the connection closing stops
 * it too.
 */
export function keepNatsCredential(
  connection: Pick<NatsConnection, "reconnect" | "isClosed" | "closed" | "status">,
  credential: BrokerNatsCredential,
  warn: (text: string) => void
): () => void {
  let stopped = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let retries = 0;
  // A renewal in flight, and whether a drop asked for another the moment it ends.
  let renewing = false;
  let renewAgain = false;
  const renewal = warnedEvery((why) =>
    warn(
      `the NATS connection's broker token ${why}; it is tried again, and the connection reconnects once a token is live`
    )
  );
  const refusal = warnedEvery(warn);
  // nats.js's status() is a QueuedIterator, which has a stop() its declared type leaves out.
  const statuses: AsyncIterable<Status> & { stop?: () => void } = connection.status();
  const stop = () => {
    stopped = true;
    clearTimeout(timer);
    timer = undefined;
    statuses.stop?.();
  };
  const schedule = (delay: number) => {
    if (stopped || connection.isClosed()) return;
    clearTimeout(timer);
    timer = setTimeout(() => void tick(), delay);
    timer.unref?.();
  };
  const retry = () => {
    retries += 1;
    schedule(Math.min(RETRY_FIRST_MS * 2 ** (retries - 1), RETRY_LAST_MS));
  };
  const renew = async () => {
    const before = credential.expiresAt().getTime();
    try {
      // Bounded as `ready` is: the token client waits out a missing registration for as long as
      // its caller does, and the keeper's next tick is its own retry.
      await credential.renew(AbortSignal.timeout(TRADE_DEADLINE_MS));
      if (stopped) return;
      if (credential.needsReconnect()) await connection.reconnect();
    } catch (error) {
      // The host moved to another session meanwhile: that session's renewal is the one that
      // counts, so the keeper renews again at once, for it, and has nothing to warn of.
      if (error instanceof SessionMovedError) {
        schedule(0);
        return;
      }
      renewal.warn(`could not be renewed (${tradeFailureReason(error)})`);
      retry();
      return;
    }
    if (credential.expiresAt().getTime() === before) {
      // The token client answered the token in hand: its trade failed in a way it keeps the
      // token through (a broker that is away, a busy one), so try again before it lapses.
      renewal.warn(
        `renewal landed no new token, so the one in hand lapses at ${new Date(before).toISOString()}`
      );
      retry();
      return;
    }
    retries = 0;
    renewal.reset();
    schedule(renewDelay(credential.expiresAt().getTime(), Date.now()));
  };
  const tick = async () => {
    timer = undefined;
    if (stopped || connection.isClosed()) return;
    renewing = true;
    try {
      await renew();
    } finally {
      renewing = false;
    }
    if (renewAgain) {
      renewAgain = false;
      schedule(0);
    }
  };
  const renewNow = () => {
    if (renewing) renewAgain = true;
    else schedule(0);
  };
  void (async () => {
    for await (const status of statuses) {
      if (stopped) return;
      if (status.type === RECONNECTED) {
        credential.admitted();
        refusal.reset();
        continue;
      }
      if (status.type !== ERRORED || typeof status.data !== "string") continue;
      const counted = credential.refused(status.data);
      if (counted === undefined) continue;
      refusal.warn(
        `the NATS server refused the connection's live broker token (${status.data}): the auth service is away, or the token was revoked; the connection keeps redialling, and ${
          counted.droppedBefore
            ? "this session's token was dropped and another traded once already, so none is dropped again until the connection is back"
            : "at the second refusal the token is dropped and another traded"
        }`
      );
      if (counted.dropped) renewNow();
    }
  })().catch((error: unknown) => {
    if (!stopped) warn(`the NATS connection's status reader failed: ${natsFailureReason(error)}`);
  });
  connection.closed().then(stop, stop);
  schedule(renewDelay(credential.expiresAt().getTime(), Date.now()));
  return stop;
}

/** How long a session switch waits for its connection to come back as the new session, once its
 *  token is in hand, at most. */
const FOLLOW_RECONNECT_MS = 8_000;

/** The most a session switch's follow takes, its default bound: the trade's `TRADE_DEADLINE_MS`,
 *  then `FOLLOW_RECONNECT_MS` for the connection to come back: in a pass of 30 s, the floor of
 *  the pass's deadline (`ENVOY_PASS_DEADLINE_MS`), 2 s are left. */
export const FOLLOW_DEADLINE_MS = TRADE_DEADLINE_MS + FOLLOW_RECONNECT_MS;

function seconds(ms: number): number {
  return Math.round(ms / 100) / 10;
}

/**
 * Moves `connection` to the token of the session `credential`'s name reads now — the session a
 * host switched to — and returns once the connection has come back admitted with it. A caller
 * subscribes the new session's subjects only after this returns: until then the connection is
 * still the user the old session's token was admitted as, the auth service granted that user none
 * of the new session's subjects, and the server refuses a subscribe to one, which nats.js answers
 * by closing the subscription. Nothing happens where the token in hand is the one the connection
 * presented. A reconnect counts only once the CONNECT it made presented the token in hand, so one
 * that began before the move does not end it; and since nats.js's `reconnect()` does nothing while
 * the connection is redialling, a redial that comes back with the old token is followed by another
 * reconnect, which presents the new one. The new session's trade waits for ready
 * (`TokenCallOptions`), since a switch has nothing to do until it holds that token, through every
 * failure the token client's retry loop trades again on: a first trade's 503 is ridden out.
 *
 * The whole follow takes at most `boundMs`, `FOLLOW_DEADLINE_MS` by default, so a caller inside a
 * pass whose deadline is shorter bounds it by that pass: the trade waits all of it but the
 * connection's share, `FOLLOW_RECONNECT_MS` or half the bound where that is less (at the default,
 * `TRADE_DEADLINE_MS`), and the connection has what is left, at most `FOLLOW_RECONNECT_MS`.
 * Throws, naming the trade's last failure, where the token does not come within the trade's
 * share, where the connection does not come back within what is left, or where the connection
 * closes first. The status reader it opens is stopped on every way out: nats.js keeps a reader
 * registered until it is stopped, whatever becomes of the connection.
 */
export async function followNatsCredential(
  connection: Pick<NatsConnection, "reconnect" | "status" | "closed">,
  credential: BrokerNatsCredential,
  boundMs: number = FOLLOW_DEADLINE_MS
): Promise<void> {
  const started = Date.now();
  const bound = Math.max(0, Math.min(FOLLOW_DEADLINE_MS, boundMs));
  const tradeMs = bound - Math.min(FOLLOW_RECONNECT_MS, bound / 2);
  try {
    await credential.renew(AbortSignal.timeout(tradeMs), true);
  } catch (error) {
    throw new LoggableError(
      isDeadline(error)
        ? `the NATS connection could not take the new session's token within ${seconds(tradeMs)} s`
        : `the NATS connection could not take the new session's token: ${tradeFailureReason(error)}`
    );
  }
  if (!credential.needsReconnect()) return;
  const reconnectMs = Math.max(0, Math.min(FOLLOW_RECONNECT_MS, bound - (Date.now() - started)));
  const deadline = AbortSignal.timeout(reconnectMs);
  // nats.js's status() is a QueuedIterator, which has a stop() its declared type leaves out.
  const statuses: AsyncIterable<Status> & { stop?: () => void } = connection.status();
  const back = (async () => {
    for await (const status of statuses) {
      if (status.type !== RECONNECTED) continue;
      if (!credential.needsReconnect()) return;
      // Back as the old session: a redial that was in flight when the move asked for one.
      connection.reconnect().catch(() => {});
    }
    throw new LoggableError(
      "the NATS connection's status ended before it came back as the new session"
    );
  })();
  const closed = connection.closed().then(() => {
    throw new LoggableError("the NATS connection closed before it came back as the new session");
  });
  const late = Promise.withResolvers<never>();
  const expire = () =>
    late.reject(
      new LoggableError(
        `the NATS connection did not come back as the new session within ${seconds(reconnectMs)} s`
      )
    );
  if (deadline.aborted) expire();
  else deadline.addEventListener("abort", expire, { once: true });
  try {
    await connection.reconnect();
    await Promise.race([back, closed, late.promise]);
  } finally {
    deadline.removeEventListener("abort", expire);
    statuses.stop?.();
    back.catch(() => {});
    closed.catch(() => {});
    late.promise.catch(() => {});
  }
}
