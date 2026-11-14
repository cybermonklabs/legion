// packages/envoy-client/src/broker-credential.ts
//
// The credential a machine makes out of the identities it already holds: the secrets broker's
// RFC 8693 exchange turns them into an access token bound to a key the token's holder proves, and
// Dispatch and the Envoy listener read that token back at the broker's introspection route.
//
// Which identity is traded is the factory's to decide (`brokerTokenClient`,
// `@legion/broker-client/environment`). Where a person ran `agent-secrets login` on this machine,
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

import {
  BROKER_URL_VAR,
  type BrokerEnvironment,
  brokerTokenClient,
  IDENTITY_VAR,
  MACHINE_AUDIENCE_VAR,
  MACHINE_TOKEN_FILE_VAR,
  machineIdentity,
  POOL_ACCOUNT_VAR,
  POOL_AUDIENCE_VAR,
  signInTradedBy,
  tradeSession,
} from "@legion/broker-client/environment";
import {
  chooseSignIn,
  PERSON_VAR,
  SIGNIN_CLIENT_ID_VAR,
  SIGNIN_CLIENT_SECRET_FILE_VAR,
  SIGNIN_DIR_VAR,
  SIGNIN_ISSUER_VAR,
  type SignInRecord,
} from "@legion/broker-client/signins";
import {
  type BrokerToken,
  type BrokerTokenClient,
  isFinalRefusal,
} from "@legion/broker-client/token-client";
import type { RequestCredential } from "./bearer";

/**
 * The session a trade writes under, which the broker records the minted token against and the
 * listener checks a subscribe, an unsubscribe, a role claim and a send's own `source_session`
 * against. A caller that holds one name passes it; a caller whose session changes over the life
 * of its process — Claude Code mints a new id on `/clear`, and one Oh My Pi process switches
 * between sessions — passes a function, which is read at every trade rather than fixed when the
 * configuration resolved. A caller that acts as no session at all passes nothing, and the factory
 * names the trade: the machine's own under whatever `AGENT_SECRETS_SESSION` names, which is how a
 * daemon names its own (`tradeSession`, `@legion/broker-client/environment`, the one rule both
 * clients read), and a person's under their sign-in's own name (`signInSessionName`), never that
 * variable's.
 */
export type BrokerSessionName = string | (() => string);

/** `naming` as one name, trimmed; "" when it names none. */
function sessionName(naming: BrokerSessionName | undefined): string {
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

/**
 * The variables that describe a client beside the session it trades under: the broker, this
 * machine's identity and what each kind of identity reads, and where a person's sign-in is kept,
 * which person a session acts for and the sign-in client that renews theirs (`chooseSignIn`,
 * `signInFlow`). Two environments that differ in any of them are two clients, so neither is
 * answered the client the other described.
 */
const CLIENT_VARIABLES = [
  BROKER_URL_VAR,
  IDENTITY_VAR,
  MACHINE_TOKEN_FILE_VAR,
  MACHINE_AUDIENCE_VAR,
  POOL_AUDIENCE_VAR,
  POOL_ACCOUNT_VAR,
  SIGNIN_DIR_VAR,
  "XDG_STATE_HOME",
  "HOME",
  PERSON_VAR,
  SIGNIN_ISSUER_VAR,
  SIGNIN_CLIENT_ID_VAR,
  SIGNIN_CLIENT_SECRET_FILE_VAR,
] as const;

/** A client the factory made, and the sign-in it trades as {@link signInName} names one, or ""
 *  where it trades this machine's own identity. */
interface Made {
  readonly client: BrokerTokenClient | undefined;
  readonly signIn: string;
}

/** One session's client under one set of {@link CLIENT_VARIABLES}, and what it was refused. */
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
   * (`packages/envoy/internal/brokerclient/signinauthority.go`).
   * Once it holds one, every call reads the store again (`chooseSignIn`): a sign-in it names is
   * refused again at once, with no request to the issuer or the broker, and a sign-in it does not
   * — a new `agent-secrets login` made a new box key — gets a client of its own. Go then enrols
   * under the machine; this client never does, so a revoked sign-in stops its sessions rather than
   * turning them into the machine.
   */
  readonly refused: Map<string, unknown>;
}

/**
 * The clients this process has made, by the session they trade under and, under it, by the
 * variables that describe them. One process holds one client per broker, identity and session,
 * and keeps one token live under it, which both Dispatch and the listener of that session
 * present: two clients would be two credentials for one session, and the broker records the key
 * the token is bound to. The session is part of the identity, not of the process: a token names
 * one session, and the listener answers 403 to a session presenting another's, so two sessions of
 * one process hold two clients. An entry is one client and one token, made only for a session
 * this process actually traded for, and dropped when the host leaves that session
 * (`forgetBrokerSession`).
 */
const CLIENTS = new Map<string, Map<string, Held>>();

/** Drops what this process made, so a test drives a broker of its own from nothing. */
export function forgetBrokerClients(): void {
  CLIENTS.clear();
}

/**
 * Drops what this process traded for `session`: its client, the key pair it made, the token
 * renewing under it, and the sign-ins it was refused.
 *
 * For the host that has left that session, once it has made its last call for it — Oh My Pi's
 * pass once the role of the session it left is handed back, Claude Code's handoff once the old
 * id is deregistered. One long-lived process binds many session ids over its life (`/clear`, a
 * session switch), and without this each one it ever traded under keeps a key pair and a token
 * that trades again at every renewal, for the life of the process. A session that comes back
 * trades afresh, as one this process never traded for does.
 */
export function forgetBrokerSession(session: string): void {
  CLIENTS.delete(session.trim());
}

/** A sign-in as Go's `SignInAuthority` names one: the person, and the box key's thumbprint. */
function signInName(record: Pick<SignInRecord, "person" | "thumbprint">): string {
  return `${record.person}\u0000${record.thumbprint}`;
}

/**
 * The entry of `CLIENTS` for a caller of `env` acting as the session `own`, made empty where
 * there is none yet. It is held under the name {@link tradeSession} gives the caller rather than
 * under `own`: a caller that acts as no session and one that acts as the session
 * `AGENT_SECRETS_SESSION` names are one client, and two environments naming two sessions are two.
 */
function entryFor(env: BrokerEnvironment, own: string): Held {
  const session = tradeSession(env, own);
  const variables = JSON.stringify(CLIENT_VARIABLES.map((name) => env[name]));
  const sessions = CLIENTS.get(session) ?? new Map<string, Held>();
  CLIENTS.set(session, sessions);
  let entry = sessions.get(variables);
  if (entry === undefined) {
    entry = { making: undefined, made: undefined, refused: new Map() };
    sessions.set(variables, entry);
  }
  return entry;
}

/** The factory's client for `own`, and the sign-in it trades. */
async function make(env: BrokerEnvironment, own: string): Promise<Made> {
  const client = await brokerTokenClient(env, own);
  const signIn = client === undefined ? undefined : signInTradedBy(client);
  return { client, signIn: signIn === undefined ? "" : signInName(signIn) };
}

/**
 * The client `env` describes for a caller acting as the session `own`, made once per process
 * (`entryFor`), with its entry. The factory is handed `own` as the caller gave it, since the
 * factory decides the name the trade asks under: a person's sign-in asks under the sign-in's own
 * name where the caller acts as no session, and never under `AGENT_SECRETS_SESSION`, which the
 * machine trades under.
 */
async function clientFor(
  env: BrokerEnvironment,
  own: string
): Promise<{ readonly entry: Held; readonly made: Made }> {
  const entry = entryFor(env, own);
  if (entry.refused.size > 0) {
    const record = await chooseSignIn(env);
    const chosen = record === undefined ? "" : signInName(record);
    if (entry.refused.has(chosen)) throw entry.refused.get(chosen);
    if (entry.made !== undefined && entry.made.signIn !== chosen) {
      entry.making = undefined;
      entry.made = undefined;
    }
  }
  let making = entry.making;
  if (making === undefined) {
    const started = make(env, own);
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
 * The trade happens at the first request that needs it and at every renewal after, both inside
 * `headers`, so nothing of the caller's restarts when a token lapses. Dispatch and the listener
 * mint no DPoP nonce (`packages/envoy/internal/brokertoken/brokertoken.go`, `withoutNonces`), so
 * a request to either carries the proof it was signed with and no nonce round trip; the broker's
 * own token endpoint, which may ask for one, is where that retry lives
 * (`@legion/broker-client/token-client`).
 *
 * A server that says the token presented for a session is not live (`stale`, as Dispatch's
 * `401 BROKER_TOKEN_NOT_ACTIVE` says of a token a revoke ended before it lapsed) has that
 * session's client drop it, so the next `headers` trades afresh: the broker answers a new token,
 * or refuses in its own words, which after a revoke name `agent-secrets login`. A person's
 * sign-in refused for good is remembered against that sign-in (`Held.refused`), so later calls
 * fail with the same words and ask nothing of the issuer or the broker until a new sign-in is on
 * the machine.
 */
export function brokerCredential(
  env: BrokerEnvironment,
  session?: BrokerSessionName | undefined
): RequestCredential {
  let presented = "";
  /** The session whose credential answers a call acting for `acting`, which `headers` and
   *  `stale` read alike. */
  const own = (acting: string | undefined): string => (acting ?? "").trim() || sessionName(session);
  return {
    scheme: "DPoP",
    async headers(
      method: string,
      url: string,
      acting?: string
    ): Promise<Readonly<Record<string, string>>> {
      const { entry, made } = await clientFor(env, own(acting));
      const { client } = made;
      if (client === undefined) {
        throw new Error(
          "AGENT_SECRETS_URL and AGENT_SECRETS_MACHINE_IDENTITY no longer name a broker and an identity to trade"
        );
      }
      let token: BrokerToken;
      try {
        token = await client.token();
      } catch (error) {
        if (made.signIn !== "" && isFinalRefusal(error)) entry.refused.set(made.signIn, error);
        throw error;
      }
      presented = token.accessToken;
      return {
        Authorization: `DPoP ${token.accessToken}`,
        DPoP: await client.key.proof({ method, url, accessToken: token.accessToken }),
      };
    },
    stale(acting?: string): void {
      entryFor(env, own(acting)).made?.client?.drop();
    },
    secret: () => presented,
  };
}
