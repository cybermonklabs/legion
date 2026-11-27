// packages/cipher-client/src/token-errors.ts
//
// The errors a trade for the broker's access token ends in, and the one reading of them every
// caller shares (`isFinalRefusal`): a refusal of the broker's, a pod session not registered yet, a
// retry still under way, a trade that got no answer, and a person's sign-in the issuer has ended.
// The token client (`token-client.ts`) throws them; the person-identity source, envoy-client's
// broker and NATS credentials, and every test read them without owning the retry loop.

import { scrub } from "@legion/contracts/held-secrets";
import { outage } from "@legion/contracts/http-status";
import {
  isKnownBrokerRefusal,
  KNOWN_OAUTH_ERROR_CODES,
  REFUSAL_TEXT_WITHHELD,
  type RefusalContext,
  shownOAuthCode,
} from "./refusal-text";
import { SIGNIN_COMMAND } from "./signins";
import { POD_SESSION_NOT_REGISTERED } from "./wire";

/** What the broker answers a proof whose `iat` is outside its 60 s skew window. */
const INVALID_DPOP_PROOF = "invalid_dpop_proof";

/** What the broker answers a proof it wants a nonce in (RFC 9449 Section 8), which `#send`
 *  already retries once with the nonce the answer carried. */
const USE_DPOP_NONCE = "use_dpop_nonce";

/** The code of the broker's refusal of a pod session it holds no registration for yet, whose
 *  description begins {@link POD_SESSION_NOT_REGISTERED} and a colon. */
const INVALID_GRANT = "invalid_grant";

/** What the broker answers a trade naming a person Dispatch holds (`oauthwire.MembershipHeld`). */
const MEMBERSHIP_HELD = "membership_held";

/** How the broker's OAuth library ends its `invalid_request` to a subject token it no longer
 *  honours: one revoked, or one whose record is already removed. */
const TOKEN_NOT_VALID = "Token is not valid or has expired.";

/** The hint that ends the broker's `invalid_grant` to a trade of a machine login's access token
 *  once that login has ended (`oauth/exchange.go`); the broker's library writes its own sentence
 *  before it (`oauth/testdata/refusal-descriptions.json`). */
const MACHINE_LOGIN_ENDED = "The machine login the subject token belongs to has ended.";

/**
 * A trade the broker refused: a plain error naming what was traded and what the broker said.
 * Almost every refusal is one no retry can cure — an identity no entry lists, a sign-in a person
 * revoked, a token of the wrong audience — and a renewal stops on it rather than asking again.
 * The five exceptions are an outage (the broker's 429, or a 408), its two DPoP refusals, a pod
 * session it holds no registration for yet, and a person Dispatch holds (`permanent`).
 */
export class BrokerRefusedError extends Error {
  /** What the broker said, as a text may print it: its description where that is a known
   *  refusal ({@link isKnownBrokerRefusal}), "" where it sent none, and {@link REFUSAL_TEXT_WITHHELD}
   *  otherwise. The description the broker sent is kept nowhere. */
  readonly said: string;
  /** The OAuth code where it is one the clients handle ({@link KNOWN_OAUTH_ERROR_CODES}), else "";
   *  what decides a retry ({@link BrokerRefusedError.permanent}). */
  readonly code: string;
  /** The code as a text shows it ({@link shownOAuthCode}). */
  readonly shownCode: string;
  /** Whether this is the broker's refusal of a pod whose session no registration names yet:
   *  `invalid_grant`, its description beginning {@link POD_SESSION_NOT_REGISTERED} and a colon.
   *  The same words under another code, or anywhere but at the start, are some other refusal.
   *  Read from the description as the broker sent it, which is then kept nowhere. */
  readonly podSessionUnregistered: boolean;
  /** Whether this is the broker's refusal of a subject token it no longer honours, and of nothing
   *  else: `400 invalid_request` ending "Token is not valid or has expired." (a token revoked, or
   *  already removed), or `400 invalid_grant` ending "The machine login the subject token belongs
   *  to has ended.", each after the library's own sentence. A token past its expiry whose record
   *  still stands is `401 invalid_token`, which is not one: the refresh margin before the expiry
   *  exists to avoid it. A refusal of the trade's session or projects uses the same codes and is
   *  not one either. Read from the description as the broker sent it, which is then kept nowhere. */
  readonly subjectNotHonoured: boolean;

  /** `refused` names the identity traded, as its source describes it: this machine's, or a
   *  person's sign-in on it; `context` is what the trade sent that a known refusal may name. */
  constructor(
    readonly status: number,
    code: string,
    description: string,
    refused: string,
    context: RefusalContext = {}
  ) {
    const shownCode = shownOAuthCode(code);
    const said =
      description === ""
        ? ""
        : isKnownBrokerRefusal(description, context)
          ? // Scrubbed here, not only where a caller prints it: a host prints the error itself
            // (a `console.warn(text, error)`, an uncaught error), and the policy's own refusal
            // can carry the subject token it was sent.
            scrub(description)
          : REFUSAL_TEXT_WITHHELD;
    super(
      `the secrets broker refused ${refused}: HTTP ${status}${shownCode === "" ? "" : ` ${shownCode}`}${said === "" ? "" : `: ${said}`}`
    );
    this.name = "BrokerRefusedError";
    this.code = KNOWN_OAUTH_ERROR_CODES.has(code) ? code : "";
    this.shownCode = shownCode;
    this.said = said;
    this.podSessionUnregistered =
      code === INVALID_GRANT && description.startsWith(`${POD_SESSION_NOT_REGISTERED}:`);
    this.subjectNotHonoured =
      status === 400 &&
      ((code === "invalid_request" && description.endsWith(TOKEN_NOT_VALID)) ||
        (code === INVALID_GRANT && description.endsWith(MACHINE_LOGIN_ENDED)));
  }

  /**
   * Whether this refusal is one no retry can cure, which every refusal is but five. The Go
   * client reads it the same way, out of the same five cases (`RefusedError.Permanent`,
   * packages/envoy/internal/cipherclient/brokerclient.go), and
   * `packages/envoy/internal/cipherclient/testdata/wire.json` holds the two to that one reading.
   *
   * An outage ({@link outage}, Go's `oauthrefusal.Outage`): the broker's 429, and a 408, the
   * broker or a proxy before it that did not take the request, whatever OAuth error either body
   * names (`brokerRefusal` makes a refusal only of a 4xx, so no 5xx is one). The broker's token
   * endpoint spends a bucket per source address, and a bucket refills. A client that dropped the
   * token in hand on one would write with no identity through a burst — its own, or another
   * process's on the same machine — or over one request a proxy did not pass on, while the token
   * it already held was still good.
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
   * their Dispatch sign-in, or no refresh has confirmed them for 24 hours, or, behind
   * Identity-Aware Proxy, the Workspace Directory no longer confirms their account. A hold revokes
   * nothing, and the person's next Dispatch sign-in, the next refresh that confirms them, or the
   * Directory's next active answer ends it, so the identity this client trades is served again then
   * with no new credential. A client that dropped it would need a restart after every hold.
   */
  get permanent(): boolean {
    return (
      !outage(this.status) &&
      this.code !== INVALID_DPOP_PROOF &&
      this.code !== USE_DPOP_NONCE &&
      this.code !== MEMBERSHIP_HELD &&
      !this.podSessionUnregistered
    );
  }
}

/**
 * A call that stopped waiting, its signal ended by its own deadline, while the broker held no
 * registration for this pod's session yet ({@link BrokerRefusedError.podSessionUnregistered}).
 * The client goes on trading for that session in the background, so it is not final
 * ({@link isFinalRefusal}): a later call takes the token once the registration lands. Its message
 * carries the refusal as the refusal prints it (`said`), never the text the broker sent.
 */
export class RegistrationPendingError extends Error {
  constructor(
    readonly session: string,
    refusal: BrokerRefusedError
  ) {
    super(
      `the secrets broker has no registration for this pod's session ${session} yet, and the trade is being retried in the background until it lands${refusal.said === "" ? "" : ` (${refusal.said})`}`,
      { cause: refusal }
    );
    this.name = "RegistrationPendingError";
  }
}

/**
 * A trade failure a later trade may cure, told to a caller with no live token in hand while the
 * client's retry loop trades again: at once to a caller that fails fast, and to one that waits for
 * ready once its own signal ends. A pod session not registered yet is
 * {@link RegistrationPendingError} instead. The failure is no answer within the trade's own time
 * (`TRADE_ATTEMPT_MS`), no connection, a 5xx, a 408 or a 429, a DPoP refusal, a 4xx naming no
 * OAuth error, or a trade that could not be made.
 * The loop goes on, so it is not final ({@link isFinalRefusal}). Its message is the loop's own
 * line for that failure (`Retried.said`), which names nothing a peer wrote.
 */
export class TradeRetryingError extends Error {
  constructor(said: string) {
    super(`${said}, and the trade is being retried in the background`);
    this.name = "TradeRetryingError";
  }
}

/**
 * A call that stopped waiting, its signal ended, while the client's first trade since its last
 * token was still out, so no trade had ended yet to say what it came to: a broker that does not
 * answer, named as that rather than as the signal's bare "The operation timed out.". The trade
 * goes on, so it is not final ({@link isFinalRefusal}). Its message names the identity traded and
 * nothing a peer wrote.
 */
export class TradeUnansweredError extends Error {
  constructor(identity: string) {
    super(
      `the secrets broker has not yet answered the trade of ${identity}, and the trade goes on in the background`
    );
    this.name = "TradeUnansweredError";
  }
}

/**
 * A trade, or a step of a device login, that ended other than in a refusal: the broker gave no
 * answer, a 5xx, an answer that is not this grant's, or one this machine will not use (a field
 * holding a control character, a device code no poll could reach). Its message names the broker as
 * `urlForText` names it and what failed: the identity traded or the login, and the failure by its
 * class (`errorClass`); the field of the answer this machine will not use; or the numbers in it
 * that were checked finite, printed as digits. It holds nothing else a peer wrote, so a caller
 * prints it whole, and it keeps no cause, since the library's error can hold the answer it read.
 * The class says nothing of whether asking again cures it: a trade's loop tries again
 * (`BrokerTokenClient`), and {@link DEVICE_LOGIN_COMMAND} prints it and stops; a device code no
 * poll could reach says in its message that asking again meets the same answer.
 */
export class BrokerTradeFailedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "BrokerTradeFailedError";
  }
}

/**
 * A call to a client that was closed (`BrokerTokenClient.close`), whose retry loop trades no
 * more: a sign-in this machine no longer chooses, a session the process left. Its message is the
 * client's own and names nothing a peer wrote, so a caller prints it whole.
 */
export class TokenClientClosedError extends Error {
  constructor() {
    super("the secrets broker client was closed, and trades no more");
    this.name = "TokenClientClosedError";
  }
}

/**
 * A person's sign-in that the issuer no longer honours: its refresh token was answered 400 or 401
 * `invalid_grant`, because the person revoked the grant at Google or it expired. It mirrors the
 * Go client's `ErrSignInEnded` (`packages/envoy/internal/cipherclient/device.go`), and like a
 * broker's permanent refusal it is final ({@link isFinalRefusal}): no retry and no other caller
 * cures it, only a new {@link SIGNIN_COMMAND}. `issuer` is the issuer as a text names it
 * (`urlForText`), and it keeps no cause: the library's error holds the issuer's answer.
 */
export class SignInEndedError extends Error {
  constructor(
    readonly issuer: string,
    readonly person: string
  ) {
    super(
      `${issuer} no longer honours ${person}'s sign-in on this machine; run: ${SIGNIN_COMMAND}`
    );
    this.name = "SignInEndedError";
  }
}

/** The command that logs a machine in at the broker's device grant, which every refusal a new
 *  device login cures names (`device-login.ts`). */
export const DEVICE_LOGIN_COMMAND = "dispatch login --device";

/**
 * A device login the broker no longer honours: its refresh was answered 400 or 401
 * `invalid_grant` (`endsGrant`, `oauth-refusal.ts`), because its credential was revoked or ran
 * out, or its refresh token was presented again after its rotation's grace, which revokes the
 * login (`oauthstore.Store.RevokeRefreshToken`). Like a sign-in the issuer ended it is final
 * ({@link isFinalRefusal}): only a new {@link DEVICE_LOGIN_COMMAND} cures it. `login` names the
 * login as its source describes it, and it keeps no cause: the library's error holds the broker's
 * answer.
 */
export class DeviceLoginEndedError extends Error {
  constructor(login: string) {
    super(
      `the secrets broker no longer honours ${login}: it was revoked or ran out; run: ${DEVICE_LOGIN_COMMAND}`
    );
    this.name = "DeviceLoginEndedError";
  }
}

/**
 * A trade whose time ran out while its identity's source still waited for a lock that another
 * process of this machine holds (a device login's, `device-login.ts`): no request of the trade
 * reached the broker, so its text names the lock, never the broker. `lock` names the lock, and
 * `cause` is the signal's reason. A later trade cures it, once the holder lets go or its lock is
 * stale and taken.
 */
export class LockHeldError extends Error {
  constructor(lock: string, options?: ErrorOptions) {
    super(
      `another process of this machine held ${lock} for all of this trade's time, so nothing was sent to the secrets broker`,
      options
    );
    this.name = "LockHeldError";
  }
}

/**
 * Whether `error` ends the identity a client trades for good: a broker's refusal no retry can
 * cure (`BrokerRefusedError.permanent`), a sign-in the issuer has ended
 * ({@link SignInEndedError}), or a device login the broker has ended
 * ({@link DeviceLoginEndedError}). A client drops the token in hand on one rather than answer it
 * until it lapses, and a caller that remembers refusals remembers these alone: remembering an
 * outage (the broker's 429, or a 408) or a clock-skewed proof would stop a client a later trade
 * would have answered.
 */
export function isFinalRefusal(error: unknown): boolean {
  return (
    (error instanceof BrokerRefusedError && error.permanent) ||
    error instanceof SignInEndedError ||
    error instanceof DeviceLoginEndedError
  );
}
