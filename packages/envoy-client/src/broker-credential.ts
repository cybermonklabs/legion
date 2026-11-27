// packages/envoy-client/src/broker-credential.ts
//
// The credential a machine makes out of the identities it already holds: the secrets broker's
// RFC 8693 exchange turns them into an access token bound to a key the token's holder proves, and
// Dispatch and the Envoy listener read that token back at the broker's introspection route.
//
// Which identity is traded is the factory's to decide (`brokerTokenClient`,
// `@legion/cipher-client/environment`). Where a person ran `agent-secrets login` on this machine,
// it is that person: their Google identity token is the subject, this machine's identity is the
// actor, and the sign-in's box key signs every proof, so the token names the person. Where nobody
// did, it is the machine itself — a pod's projected service-account token, a Compute Engine
// machine's Google identity token, or one minted through a workload identity pool — bound to a
// key this process made. A sign-in that cannot be traded is an error, never the machine's token
// in its place.
//
// It is the last source a section consults. Every bearer a deployment writes today —
// `DISPATCH_TOKEN`, `DISPATCH_TOKEN_FILE`, `ENVOY_TOKEN`, `ENVOY_TOKEN_FILE`, an envoy.json's own
// token — is resolved first and unchanged, so a session that has a token by any of those routes
// behaves exactly as it did before this module existed.

import type { DpopKey } from "@legion/cipher-client/dpop";
import {
  BROKER_URL_VAR,
  BROKER_VARIABLES,
  type BrokerEnvironment,
  brokerTokenClient,
  IDENTITY_VAR,
  machineIdentity,
  type ReadTrade,
} from "@legion/cipher-client/environment";
import { chooseSignIn, type SignInRecord } from "@legion/cipher-client/signins";
import type { BrokerToken, BrokerTokenClient } from "@legion/cipher-client/token-client";
import { isFinalRefusal } from "@legion/cipher-client/token-errors";
import type { RequestCredential } from "./bearer";
import { LoggableError } from "./errors";

/**
 * The session a trade writes under, which the broker records the minted token against and the
 * listener checks a subscribe, an unsubscribe, a role claim and a send's own `source_session`
 * against. A caller that holds one name passes it; a caller whose session changes over the life
 * of its process — Claude Code mints a new id on `/clear`, and one Oh My Pi process switches
 * between sessions — passes a function, which is read at every trade rather than fixed when the
 * configuration resolved. A caller that acts as no session at all passes nothing, and the factory
 * names the trade: the machine's own under whatever `AGENT_SECRETS_SESSION` names, which is how a
 * daemon names its own (`tradeSession`, `@legion/cipher-client/environment`, the one rule both
 * clients read), and a person's under their sign-in's own name (`signInSessionName`), never that
 * variable's. Such a caller is never answered the client of one that names the very session
 * `AGENT_SECRETS_SESSION` names, nor the reverse (`CLIENTS`): a person's trade names the two apart.
 */
export type BrokerSessionName = string | (() => string);

/** `naming` as one name, trimmed; "" when it names none. A NATS credential reads it to tell the
 *  session a token was traded for from the one the host is on now. */
export function sessionName(naming: BrokerSessionName | undefined): string {
  if (naming === undefined) return "";
  return (typeof naming === "string" ? naming : naming()).trim();
}

/**
 * Whether this machine can trade for one of the broker's access tokens — a broker to trade at
 * and an identity this client can prove — and, where it cannot, the phrase that says which of
 * the two is missing, as a refusal ends with. One pass answers both, so no caller decides the
 * question a second time to say why. An identity named with its own variables missing throws
 * here, naming them, rather than reading as a machine that has none.
 */
export type BrokerRoute =
  | { readonly traded: true }
  | { readonly traded: false; readonly gap: string };

export function brokerRouteConfigured(env: BrokerEnvironment): BrokerRoute {
  if ((env[BROKER_URL_VAR] ?? "").trim() === "") {
    return { traded: false, gap: `${BROKER_URL_VAR} names no secrets broker to trade it at` };
  }
  if (machineIdentity(env) === undefined) {
    return { traded: false, gap: `${IDENTITY_VAR} names no identity this machine proves` };
  }
  return { traded: true };
}

/** A client the factory made, and the sign-in it trades as {@link signInName} names one, or ""
 *  where it trades this machine's own identity. */
interface Made {
  readonly client: BrokerTokenClient | undefined;
  readonly signIn: string;
}

/** One caller's client under one set of {@link BROKER_VARIABLES}, and what it was refused. */
interface Held {
  /**
   * The factory's answer, under way or given. A factory that failed is not kept, so the next
   * call asks it again: a store the person corrected — one of two sign-ins removed, say — is read
   * with no restart.
   */
  making: Promise<Made> | undefined;
  /** What `making` answered, once it has. */
  made: Made | undefined;
  /**
   * The sign-ins this entry's clients were refused for good ({@link isFinalRefusal}), by
   * {@link signInName}, each with the error it was refused with, as Go's
   * `SignInAuthority.refused` remembers them
   * (`packages/envoy/internal/cipherclient/signinauthority.go`).
   * Once it holds one, every call reads the store again (`chooseSignIn`): a sign-in it names is
   * refused again at once, with no request to the issuer or the broker, and a sign-in it does not
   * — a new `agent-secrets login` made a new box key — gets a client of its own. Go then enrols
   * under the machine; this client never does, so a revoked sign-in stops its sessions rather than
   * turning them into the machine.
   */
  readonly refused: Map<string, unknown>;
}

/**
 * The clients this process has made, by the session their callers act as ("" for a caller that
 * acts as none) and, under it, by the value of every variable the factory reads and the read
 * trade the caller asks for ({@link keyOf}). That key is what the factory is handed, so the name a
 * trade asks under is one per entry: a caller acting as no session and one acting as the session
 * `AGENT_SECRETS_SESSION` names are two entries, since a person's trade asks under the sign-in's
 * own name for the first and under that session for the second. One token shared between them
 * would name one of them wrongly, and the listener refuses every write of a session whose token
 * names another (`sessionNotBound`, `packages/envoy/cmd/listener/brokerauth.go`).
 *
 * One process holds one client per broker, identity and caller's session, and keeps one token
 * live under it, which both Dispatch and the listener of that session present: two clients would
 * be two credentials for one session, and the broker records the key the token is bound to. The
 * session is part of the identity, not of the process: a token names one session, and the
 * listener answers 403 to a session presenting another's, so two sessions of one process hold two
 * clients. An entry is one client and one token, made only for a caller this process actually
 * traded for, and dropped when the host leaves that session (`forgetBrokerSession`).
 *
 * The one exception is a Dispatch credential narrowed to reading (`DISPATCH_TOKEN_BROKER_READ`,
 * `dispatch-config.ts`): its read trade and its cut are part of its key, so it is a client of its
 * own beside the session's shared one. The listener and NATS refuse a read-only token, and a
 * session's inbound messages must keep working, so they keep the shared client, which asks for no
 * scope.
 *
 * The key is the caller's session rather than the name its trade asks under because a person's
 * name is their sign-in's, which only the store says, and the store is read when the client is
 * made. That has one cost, where nobody signed in: a process whose `AGENT_SECRETS_SESSION` names
 * S, and which has callers both acting as no session and acting as S, holds two clients that both
 * trade the machine under S — two key pairs, two trades and two tokens renewing for one name.
 */
const CLIENTS = new Map<string, Map<string, Held>>();

/** Closes every client `entries` made, so none goes on waiting out a missing registration. */
function closeAll(entries: Map<string, Held> | undefined): void {
  for (const held of entries?.values() ?? []) held.made?.client?.close();
}

/** Drops what this process made, so a test drives a broker of its own from nothing. */
export function forgetBrokerClients(): void {
  for (const entries of CLIENTS.values()) closeAll(entries);
  CLIENTS.clear();
}

/**
 * Drops what this process traded for callers acting as `session`: their client, the key pair it
 * made, the token renewing under it, the trade it may still be retrying while that session's
 * registration is missing, and the sign-ins it was refused. A caller acting as no session is kept
 * apart (`CLIENTS`), so it keeps its own, whatever `AGENT_SECRETS_SESSION` names.
 *
 * For the host that has left that session, once it has made its last call for it — Oh My Pi's
 * pass once the role of the session it left is handed back, Claude Code's handoff once the old
 * id is deregistered. One long-lived process binds many session ids over its life (`/clear`, a
 * session switch), and without this each one it ever traded under keeps a key pair and a token
 * that trades again at every renewal, for the life of the process. A session that comes back
 * trades afresh, as one this process never traded for does.
 */
export function forgetBrokerSession(session: string): void {
  const key = session.trim();
  closeAll(CLIENTS.get(key));
  CLIENTS.delete(key);
}

/** A sign-in as Go's `SignInAuthority` names one: the person, and the box key's thumbprint. */
function signInName(record: Pick<SignInRecord, "person" | "thumbprint">): string {
  return `${record.person}\u0000${record.thumbprint}`;
}

/** Where {@link CLIENTS} keeps a client: by the session its callers act as, then by the
 *  variables the factory reads and the read trade the caller asks for. */
interface ClientKey {
  /** The session the caller acts as, trimmed as `headers` and `stale` resolve it; "" for a
   *  caller that acts as none. */
  readonly session: string;
  /** The value of every one of {@link BROKER_VARIABLES} in the caller's environment, and the
   *  read trade it asks for where it asks for one, as one string. */
  readonly variables: string;
}

/**
 * The key of the client for a caller of `env` acting as the session `own`: `own` itself, and the
 * value of every variable the factory reads ({@link BROKER_VARIABLES}) — the broker, this
 * machine's identity and what each kind of identity reads, `AGENT_SECRETS_SESSION`, where a
 * person's sign-in is kept, which person a session acts for and the sign-in client that renews
 * theirs (`chooseSignIn`, `signInFlow`) — with the read trade `read` and its cut, where the
 * caller asks for one. Two environments that differ in any of them are two clients, so neither is
 * answered the client the other described, and a caller that asks for no read trade keys exactly
 * as it did before there was one.
 */
function keyOf(env: BrokerEnvironment, own: string, read?: ReadTrade | undefined): ClientKey {
  const values: unknown[] = BROKER_VARIABLES.map((name) => env[name]);
  if (read !== undefined) values.push({ read: true, asOf: read.asOf ?? "" });
  return { session: own.trim(), variables: JSON.stringify(values) };
}

/** The entry of `CLIENTS` at `key`, made empty where there is none yet. */
function entryFor(key: ClientKey): Held {
  let sessions = CLIENTS.get(key.session);
  if (sessions === undefined) {
    sessions = new Map();
    CLIENTS.set(key.session, sessions);
  }
  let entry = sessions.get(key.variables);
  if (entry === undefined) {
    entry = { making: undefined, made: undefined, refused: new Map() };
    sessions.set(key.variables, entry);
  }
  return entry;
}

/** The factory's client for `own`, asking for the read trade `read` where it is given, and the
 *  sign-in it trades. */
async function make(
  env: BrokerEnvironment,
  own: string,
  read: ReadTrade | undefined
): Promise<Made> {
  const made = await brokerTokenClient(env, own, read);
  const signIn = made?.signIn;
  return { client: made?.client, signIn: signIn === undefined ? "" : signInName(signIn) };
}

/**
 * The client `env` describes for a caller acting as the session `own`, asking for the read trade
 * `read` where it is given, made once per process for each key ({@link keyOf}), with its entry.
 * The factory is handed the session the entry is kept under and the environment whose variables
 * key it, and decides from those and the sign-in the store holds the name the trade asks under:
 * the caller's own session where it acts as one, else the machine's under `AGENT_SECRETS_SESSION`
 * and a person's under their sign-in's own name, never under that variable. So every caller one
 * entry answers is one the factory names alike.
 */
async function clientFor(
  env: BrokerEnvironment,
  own: string,
  read: ReadTrade | undefined
): Promise<{ readonly entry: Held; readonly made: Made }> {
  const key = keyOf(env, own, read);
  const entry = entryFor(key);
  if (entry.refused.size > 0) {
    const record = await chooseSignIn(env);
    const chosen = record === undefined ? "" : signInName(record);
    if (entry.refused.has(chosen)) throw entry.refused.get(chosen);
    if (entry.made !== undefined && entry.made.signIn !== chosen) {
      // Closed, so its retry loop trades no more for a sign-in this machine no longer chooses.
      entry.made.client?.close();
      entry.making = undefined;
      entry.made = undefined;
    }
  }
  let making = entry.making;
  if (making === undefined) {
    const started = make(env, key.session, read);
    making = started;
    entry.making = started;
    started.then(
      (made) => {
        if (entry.making === started) entry.made = made;
      },
      () => {
        if (entry.making === started) entry.making = undefined;
      }
    );
  }
  return { entry, made: await making };
}

/** A live token of the broker's for a caller acting as `session`, and the key it is bound to. */
export interface TradedToken {
  readonly token: BrokerToken;
  readonly key: DpopKey;
  readonly session: string;
  /** The identity the token presents as: the client's entry ({@link keyOf}: the session and the
   *  variables the factory read) and the sign-in it trades, or "" for the machine's own. Dispatch
   *  keys a write by person or service, never by session, so two identities can be one login
   *  there; the identity is stricter on purpose, since this client cannot tell whether another
   *  entry trades as the same person. */
  readonly identity: string;
}

/**
 * `promise`, or `signal`'s reason the moment it ends: a caller's wait for the one client
 * {@link clientFor} makes for a session, which reads the identity's source and makes its key.
 */
function untilAborted<T>(promise: Promise<T>, signal: AbortSignal | undefined): Promise<T> {
  if (signal === undefined) return promise;
  if (signal.aborted) return Promise.reject(signal.reason);
  return new Promise<T>((resolve, reject) => {
    const stop = () => reject(signal.reason);
    signal.addEventListener("abort", stop, { once: true });
    promise.then(
      (value) => {
        signal.removeEventListener("abort", stop);
        resolve(value);
      },
      (error: unknown) => {
        signal.removeEventListener("abort", stop);
        reject(error);
      }
    );
  });
}

/** What one {@link tradedToken} call asks for: the `signal` that ends its wait, whether it waits
 *  for ready (`waitForReady`, false unless set), and the read trade `read`, where it asks for one. */
interface TradeOptions {
  readonly signal?: AbortSignal | undefined;
  readonly waitForReady?: boolean | undefined;
  readonly read?: ReadTrade | undefined;
}

/**
 * The token this process holds, or trades now, for a caller of `env` acting as the session
 * `own`, and the key the broker bound it to, out of the one client {@link clientFor} keeps for
 * that session and the read trade `read`, where the caller asks for one. A caller whose `signal`
 * ends stops waiting, for the client's making as for its trade. A caller fails fast unless
 * `waitForReady` says otherwise (`TokenCallOptions`): it is told the client's last failure at
 * once where its retry loop trades again on it, and a pod's missing registration is waited out
 * within `signal` either way (`BrokerTokenClient.token`, `RegistrationPendingError`). A person's
 * sign-in refused for good is remembered against that sign-in (`Held.refused`), so later calls
 * fail with the same words.
 */
async function tradedToken(
  env: BrokerEnvironment,
  own: string,
  { signal, waitForReady = false, read }: TradeOptions
): Promise<TradedToken> {
  const { entry, made } = await untilAborted(clientFor(env, own, read), signal);
  const { client } = made;
  if (client === undefined) {
    throw new LoggableError(
      "AGENT_SECRETS_URL and AGENT_SECRETS_MACHINE_IDENTITY no longer name a broker and an identity to trade"
    );
  }
  const { session, variables } = keyOf(env, own, read);
  try {
    return {
      token: await client.token(signal, { waitForReady }),
      key: client.key,
      session: own,
      identity: JSON.stringify([session, variables, made.signIn]),
    };
  } catch (error) {
    if (made.signIn !== "" && isFinalRefusal(error)) entry.refused.set(made.signIn, error);
    throw error;
  }
}

/**
 * The token source of a NATS connection acting as `session` (`nats-credential.ts`): each call
 * answers the token {@link brokerCredential} presents for the same session, and the key it is
 * bound to, since both are this process's one client for that session (`CLIENTS`). A session
 * name read from a function is read at each call, so a process that moved to another session
 * takes that session's token. A call fails fast, or waits for ready where `waitForReady` says
 * so, as `tradedToken` says.
 */
export function brokerTokenFor(
  env: BrokerEnvironment,
  session: BrokerSessionName | undefined
): (signal?: AbortSignal, waitForReady?: boolean) => Promise<TradedToken> {
  // The shared client, which asks for no scope: the NATS auth service refuses a read-only token.
  return (signal, waitForReady) => tradedToken(env, sessionName(session), { signal, waitForReady });
}

/** Has the client at `key` drop `token`, or the token it holds where `token` is undefined. Looked
 *  up, never made: a session this process never traded for is given no entry. */
function dropHeld(key: ClientKey, token: string | undefined): void {
  CLIENTS.get(key.session)?.get(key.variables)?.made?.client?.drop(token);
}

/**
 * Has the client that traded `traded` for a NATS connection of `env`'s drop that token alone, as
 * `brokerCredential`'s `stale` drops a token Dispatch said is not live: the NATS server refused it
 * though it has not lapsed (`keepNatsCredential`). The client's next answer is a fresh trade, so
 * the session's HTTP calls stop presenting it too; a fresher token the client holds is kept.
 */
export function dropTradedToken(env: BrokerEnvironment, traded: TradedToken): void {
  dropHeld(keyOf(env, traded.session), traded.token.accessToken);
}

/**
 * This machine's broker credential for `session`: `Authorization: DPoP <access token>` and a
 * proof of the token's key naming the request's method, its target URI and the token it is
 * presented beside (`ath`), so a copy of the token taken off the wire buys nothing. Where a
 * person signed in on this machine the token is theirs and the key is their sign-in's box key;
 * otherwise the token is the machine's own and the key is this process's.
 *
 * The token names `session` (`BrokerSessionName`), which is what a fleet entry that fixes no
 * session requires and what the listener checks each call against; a caller that names none
 * trades under the name the factory gives it instead (`BrokerSessionName`). The name is read at
 * every trade, so a process that moves from one session to another trades afresh under the new
 * name rather than presenting the old one.
 *
 * A call that names a session of its own (`headers`' third argument) is answered that session's
 * credential rather than this one: the listener holds a write that acts as a session to the
 * session its token names, and a process that has moved on still has calls to make for the
 * session it left — the hand-back of a role that session holds, its deregistration — which under
 * the credential of the session running now are answered 403. Each such session is one client of
 * its own and one trade of its own (`CLIENTS`), so the session this process left answers out of
 * the token it already holds.
 *
 * With `read`, the credential is narrowed to reading: its trade asks for the scope read and the
 * cut `read` names (`brokerTokenClient`), out of a client of its own ({@link CLIENTS}), so the
 * session's shared client, which the listener and NATS present, still asks for no scope. Only
 * Dispatch's credential passes it (`dispatchSettings`). A person's read token is bound to a key
 * its client made, never the box key, and every proof beside it is signed with that key.
 *
 * The trade happens at the first request that needs it and at every renewal after, both inside
 * `headers`, so nothing of the caller's restarts when a token lapses. Dispatch and the listener
 * mint no DPoP nonce (`packages/envoy/internal/ciphertoken/brokertoken.go`, `withoutNonces`), so
 * a request to either carries the proof it was signed with and no nonce round trip; the broker's
 * own token endpoint, which may ask for one, is where that retry lives
 * (`@legion/cipher-client/token-client`).
 *
 * A server that says the token presented for a session is not live (`stale`, as Dispatch's
 * `401 BROKER_TOKEN_NOT_ACTIVE` says of a token a revoke ended before it lapsed) has the client
 * that presented it drop that token alone, so the next `headers` presents the token the client
 * holds now, trading where it holds none: the broker answers a new token, or refuses in its own
 * words, which after a revoke name `agent-secrets login`. A person's
 * sign-in refused for good is remembered against that sign-in (`Held.refused`), so later calls
 * fail with the same words and ask nothing of the issuer or the broker until a new sign-in is on
 * the machine.
 */
export function brokerCredential(
  env: BrokerEnvironment,
  session?: BrokerSessionName | undefined,
  read?: ReadTrade | undefined
): RequestCredential {
  let presented = "";
  // What each presentation was made as, by the headers object it answered: the login
  // (`identity`), and the client's key and the token, which `stale` drops that one from.
  const presentedAs = new WeakMap<
    object,
    { readonly identity: string; readonly key: ClientKey; readonly token: string }
  >();
  /** The session whose credential answers a call acting for `acting`, which `headers` and
   *  `stale` read alike. */
  const own = (acting: string | undefined): string => (acting ?? "").trim() || sessionName(session);
  return {
    scheme: "DPoP",
    async headers(
      method: string,
      url: string,
      acting?: string,
      signal?: AbortSignal
    ): Promise<Readonly<Record<string, string>>> {
      const traded = await tradedToken(env, own(acting), { signal, read });
      const { token, key, identity } = traded;
      presented = token.accessToken;
      const headers = {
        Authorization: `DPoP ${token.accessToken}`,
        DPoP: await key.proof({ method, url, accessToken: token.accessToken }),
      };
      presentedAs.set(headers, {
        identity,
        key: keyOf(env, traded.session, read),
        token: token.accessToken,
      });
      return headers;
    },
    identity: (headers) => presentedAs.get(headers)?.identity,
    stale(acting?: string, headers?: Readonly<Record<string, string>>): void {
      // The presentation's own client, and its token alone, where the caller names it: the host
      // may have moved to another session since, and the client may hold a fresher token.
      // Otherwise the session `acting` resolves to now.
      const told = headers === undefined ? undefined : presentedAs.get(headers);
      dropHeld(told?.key ?? keyOf(env, own(acting), read), told?.token);
    },
    secret: () => presented,
  };
}
