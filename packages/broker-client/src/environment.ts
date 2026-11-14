// packages/broker-client/src/environment.ts
//
// The variables every program holding one of these identities reads, so a deployment writes one
// set of values and the Go client (`packages/envoy/internal/brokerclient/environment.go`) and this
// one are told the same things the same way.

import { importDpopKey, newDpopKey } from "./dpop";
import { metadataIdentity } from "./metadata-identity";
import { personIdentity } from "./person-identity";
import { poolIdentity } from "./pool-identity";
import { projectedToken } from "./projected-token";
import {
  chooseSignIn,
  PERSON_TRADE_VAR,
  PERSON_VAR,
  SIGNIN_CLIENT_ID_VAR,
  SIGNIN_CLIENT_SECRET_FILE_VAR,
  SIGNIN_DIR_VAR,
  SIGNIN_ISSUER_VAR,
  type SignInRecord,
  signInFlow,
  signInSessionName,
} from "./signins";
import type { TokenSource } from "./source";
import { BrokerTokenClient } from "./token-client";

/** Names the secrets broker, as it does for every other form of `agent-secrets`. */
export const BROKER_URL_VAR = "AGENT_SECRETS_URL";
/** Says which identity this machine proves. Unset, this machine has none and nothing trades,
 *  which is every deployment that has not turned the exchange on. */
export const IDENTITY_VAR = "AGENT_SECRETS_MACHINE_IDENTITY";
/** The projected Kubernetes service-account token file, which {@link IDENTITY_KUBERNETES} reads
 *  and {@link IDENTITY_POOL} federates. */
export const MACHINE_TOKEN_FILE_VAR = "AGENT_SECRETS_MACHINE_TOKEN_FILE";
/** The audience of this machine's Google identity token: the metadata server mints it for that
 *  audience, and a pool mint names it as the minted token's own. */
export const MACHINE_AUDIENCE_VAR = "AGENT_SECRETS_MACHINE_AUDIENCE";
/** The session a trade writes under for a caller that acts as no session of its own, which a
 *  daemon, a command and a helper are ({@link tradeSession}). */
export const SESSION_VAR = "AGENT_SECRETS_SESSION";
/** The workload identity pool provider an external credential is traded at, the full
 *  `//iam.googleapis.com/projects/…` resource name (Go's `PoolAudienceVar`). */
export const POOL_AUDIENCE_VAR = "AGENT_SECRETS_POOL_AUDIENCE";
/** The managed identity `generateIdToken` is called on (Go's `PoolAccountVar`). */
export const POOL_ACCOUNT_VAR = "AGENT_SECRETS_POOL_ACCOUNT";

/** A pod's projected service-account token. */
export const IDENTITY_KUBERNETES = "kubernetes";
/** A Compute Engine machine's own Google identity token. */
export const IDENTITY_METADATA = "metadata";
/** A Google identity token minted through a workload identity pool, from the projected token
 *  {@link MACHINE_TOKEN_FILE_VAR} names (`pool-identity.ts`): a machine with no Google service
 *  account of its own. */
export const IDENTITY_POOL = "pool";

/**
 * The variables this package reads, by name: this module's, and those `signins.ts` reads to find
 * a person's sign-in on this machine and the client that renews it, whose names that module
 * holds (`SIGNIN_DIR_VAR`, `PERSON_VAR`, `SIGNIN_ISSUER_VAR`, `SIGNIN_CLIENT_ID_VAR` and
 * `SIGNIN_CLIENT_SECRET_FILE_VAR`), `PERSON_TRADE_VAR`, which turns a person's trade off for a
 * process whose own credentials are a service's, and `XDG_STATE_HOME` and `HOME`, which its
 * directory is read from when `SIGNIN_DIR_VAR` names none. A caller that keeps what this package
 * made by the environment that made it reads the names here, so a variable added to the package
 * reaches its key.
 */
export const BROKER_VARIABLES = [
  BROKER_URL_VAR,
  IDENTITY_VAR,
  MACHINE_TOKEN_FILE_VAR,
  MACHINE_AUDIENCE_VAR,
  SESSION_VAR,
  POOL_AUDIENCE_VAR,
  POOL_ACCOUNT_VAR,
  SIGNIN_DIR_VAR,
  "XDG_STATE_HOME",
  "HOME",
  PERSON_VAR,
  PERSON_TRADE_VAR,
  SIGNIN_ISSUER_VAR,
  SIGNIN_CLIENT_ID_VAR,
  SIGNIN_CLIENT_SECRET_FILE_VAR,
] as const;

/** {@link BROKER_VARIABLES}, each an optional string. */
type BrokerVariables = {
  readonly [Name in (typeof BROKER_VARIABLES)[number]]?: string | undefined;
};

/** An environment this module reads those variables out of, over whatever else it holds: a
 *  process's own, or a caller's record of one. */
export type BrokerEnvironment = BrokerVariables & Readonly<Record<string, string | undefined>>;

/** `name`'s value with its whitespace trimmed, or "" when it is unset or blank. */
function value(env: BrokerEnvironment, name: keyof BrokerVariables): string {
  return (env[name] ?? "").trim();
}

/** A variable a named identity cannot do without. */
function required(env: BrokerEnvironment, name: keyof BrokerVariables, kind: string): string {
  const named = value(env, name);
  if (named === "") throw new Error(`${name} is required by ${IDENTITY_VAR}=${kind}`);
  return named;
}

/**
 * The identity {@link IDENTITY_VAR} names, or undefined when it names none, which is a machine
 * that trades nothing and is no error. A named identity whose own variables are missing throws
 * naming them, since a deployment that asked for one and gave it nothing has a mistake in it.
 * Each kind reads the variables Go's `MachineIdentity` reads for it
 * (`packages/envoy/internal/brokerclient/environment.go`), in the same order.
 */
export function machineIdentity(env: BrokerEnvironment): TokenSource | undefined {
  const kind = value(env, IDENTITY_VAR);
  switch (kind) {
    case "":
      return undefined;
    case IDENTITY_KUBERNETES:
      return projectedToken(required(env, MACHINE_TOKEN_FILE_VAR, kind));
    case IDENTITY_METADATA:
      return metadataIdentity(required(env, MACHINE_AUDIENCE_VAR, kind));
    case IDENTITY_POOL: {
      const subject = required(env, MACHINE_TOKEN_FILE_VAR, kind);
      const tokenAudience = required(env, MACHINE_AUDIENCE_VAR, kind);
      const audience = required(env, POOL_AUDIENCE_VAR, kind);
      const serviceAccount = required(env, POOL_ACCOUNT_VAR, kind);
      return poolIdentity({
        subject: projectedToken(subject),
        audience,
        serviceAccount,
        tokenAudience,
      });
    }
    default:
      throw new Error(
        `${IDENTITY_VAR}="${kind}" names no identity this machine can prove; it takes ${IDENTITY_KUBERNETES}, ${IDENTITY_METADATA} or ${IDENTITY_POOL}`
      );
  }
}

/**
 * The session a trade writes under, which the broker records the minted token against: `own`,
 * the session the caller is acting as, where it acts as one, and otherwise `AGENT_SECRETS_SESSION`.
 * The Go client reads the same rule out of the same two places
 * (`packages/envoy/internal/brokerclient/environment.go`, `TradeSession`).
 *
 * The caller's own session comes first because a token names one session and the Envoy listener
 * holds every write that acts as a session to the session its token names
 * (`packages/envoy/cmd/listener/brokerauth.go`, `sessionNotBound`): a token minted under some
 * other name is one its own holder cannot spend, so a deployment's variable winning here would
 * turn a refused trade, which says which name the entry fixes, into a 403 on every subscribe,
 * unsubscribe, role claim and send. `AGENT_SECRETS_SESSION` is therefore the name for a caller
 * that acts as no session of its own — a daemon, a command, the helper — and the only name such
 * a caller has.
 */
export function tradeSession(env: BrokerEnvironment, own = ""): string {
  const acting = own.trim();
  return acting !== "" ? acting : value(env, SESSION_VAR);
}

/** A client {@link brokerTokenClient} made, and the sign-in it trades. */
export interface MadeBrokerClient {
  readonly client: BrokerTokenClient;
  /**
   * The sign-in the client trades, as its person and its box key's thumbprint, where it trades a
   * person's; undefined where it trades this machine's own identity. The pair names a sign-in as
   * the broker records one (the person and the thumbprint of the key the proof is signed with),
   * so a caller that remembers a sign-in refused for good keys the refusal by it, as Go's
   * `SignInAuthority.refused` does (`packages/envoy/internal/brokerclient/signinauthority.go`):
   * only a new `agent-secrets login`, which makes a box key of its own, cures that refusal.
   */
  readonly signIn: Pick<SignInRecord, "person" | "thumbprint"> | undefined;
}

/**
 * The client this environment describes for a caller acting as the session `own`, with the
 * sign-in it trades, or undefined where it names no broker or no identity — a machine that trades
 * nothing, which every deployment that has not turned the exchange on is.
 *
 * Where a person has signed in on this machine ({@link chooseSignIn}), the client trades that
 * person: their Google identity token, renewed at their issuer through the sign-in client the
 * deployment configures, is the subject (`person-identity.ts`); this machine's identity is the
 * actor; and the proof is signed with the sign-in's box key, because the broker binds the sign-in
 * to that key's thumbprint and a trade with any other key would be a new sign-in, which a revoke
 * of this one does not reach. It trades under `own`, else under {@link signInSessionName}, and
 * never under `AGENT_SECRETS_SESSION`: the machine trades under that name, and the broker holds a
 * name for one identity, so it would refuse the person. Go's own person trades ask under
 * `SessionFor` alone; this client asks under the caller's own session where it has one, since
 * the Envoy listener holds a session's writes to the session its token names
 * (`packages/envoy/cmd/listener/brokerauth.go`, `sessionNotBound`). It leaves
 * `launcherCredential` unset, because the broker refuses a person's trade that declines
 * (`personDeclinesNothing`, `packages/envoy/internal/broker/oauth/exchange.go`). Every error
 * while it chooses the sign-in, reads it or builds this client is thrown: nothing here answers
 * the machine's own client while a sign-in record is on the machine, so a session never writes
 * as the machine where a person signed in to act. The answer names the sign-in the client trades
 * ({@link MadeBrokerClient}'s `signIn`).
 *
 * The one exception is a process that turns the person trade off ({@link PERSON_TRADE_VAR} set to
 * `off`): it trades the machine as when nobody signed in, even where a sign-in record is on the
 * machine, because its own credentials are a service's — the controller `legion controller start`
 * launches, whose operator's sign-in sits under the HOME it keeps.
 *
 * With no sign-in, it is the machine's own client. The key is this process's own and is made
 * here, once per client, so only its public half ever leaves the process, and the session is
 * {@link tradeSession}'s. It declines the launcher credential its identity holds
 * (`launcherCredential`): every caller of this factory wants a Dispatch access token and nothing
 * else, and none of them reads `credentialId`. A key is this process's and this session's, so
 * asking would write a `launcher_credentials` row and an audit row for every `dispatch` call,
 * every Claude Code hook run, every `envoy-send` and every session of a long-lived Oh My Pi or
 * Claude Code process, each of them on the operator's machine-logins list for its hour and
 * removed by nothing. The machine's own launcher credential is `agent-secrets`' and the
 * helper's, which ask for it.
 */
export async function brokerTokenClient(
  env: BrokerEnvironment,
  own = ""
): Promise<MadeBrokerClient | undefined> {
  const brokerUrl = value(env, BROKER_URL_VAR);
  const machine = machineIdentity(env);
  if (brokerUrl === "" || machine === undefined) return undefined;
  const record = await chooseSignIn(env);
  if (record === undefined) {
    const client = new BrokerTokenClient({
      brokerUrl,
      key: await newDpopKey(),
      subject: machine,
      session: tradeSession(env, own),
      launcherCredential: false,
    });
    return { client, signIn: undefined };
  }
  const flow = await signInFlow(env);
  if (flow === undefined) {
    throw new Error(
      `${record.person} is signed in on this machine, and this machine configures no sign-in client to renew that sign-in with: set ${SIGNIN_CLIENT_ID_VAR} and ${SIGNIN_CLIENT_SECRET_FILE_VAR}`
    );
  }
  const client = new BrokerTokenClient({
    brokerUrl,
    key: await importDpopKey(record.box_key, `${record.person}'s box key`),
    subject: personIdentity(record, flow),
    actor: machine,
    session: own.trim() || signInSessionName(record),
  });
  return { client, signIn: { person: record.person, thumbprint: record.thumbprint } };
}
